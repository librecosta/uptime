package bbolt

import (
	"context"
	"fmt"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	bolt "go.etcd.io/bbolt"
)

// Cleanup applies exclusive raw/daily retention bounds and expires instance
// metadata whose last activity (LastSeenAt, falling back to StartedAt) is older
// than 24 hours. Unknown activity is retained. Raw samples are processed before
// daily records are deleted: only a finalized row permits deleting its raw samples.
// No service metadata is removed and no worker is started.
func (s *Store) Cleanup(ctx context.Context, options uptimestorage.CleanupOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := optionalDay(options.SamplesBeforeDay); err != nil {
		return err
	}
	if err := optionalDay(options.DailyBeforeDay); err != nil {
		return err
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		if options.SamplesBeforeDay != "" {
			err := pruneHistory(ctx, tx, "samples", options.SamplesBeforeDay, func(id, day string, b *bolt.Bucket) (bool, error) {
				if _, err := validateSampleDay(ctx, b, fmt.Sprintf("samples/%q/%s", id, day)); err != nil {
					return false, err
				}
				daily, err := historyDay(tx, "daily", id, day, false)
				if err != nil || daily == nil {
					return false, err
				}
				row, err := readDaily(daily, id, day)
				return row.Finalized, err
			})
			if err != nil {
				return err
			}
		}
		if options.DailyBeforeDay != "" {
			err := pruneHistory(ctx, tx, "daily", options.DailyBeforeDay, func(id, day string, b *bolt.Bucket) (bool, error) {
				_, err := readDaily(b, id, day)
				return true, err
			})
			if err != nil {
				return err
			}
		}
		cutoff := s.now().Add(-24 * time.Hour)
		instances := tx.Bucket([]byte("instances"))
		var expired [][]byte
		err := eachBucket(ctx, instances, "instances", func(k []byte, b *bolt.Bucket) error {
			instance, err := readInstance(b, k)
			if err != nil {
				return err
			}
			activity := instance.LastSeenAt
			if activity.IsZero() {
				activity = instance.StartedAt
			}
			if !activity.IsZero() && activity.Before(cutoff) {
				expired = append(expired, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, key := range expired {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := instances.DeleteBucket(key); err != nil {
				return err
			}
		}
		return nil
	})
}

// pruneHistory collects eligible dates before deleting buckets so mutation cannot
// invalidate the day cursor. Validation and deletion share the caller's transaction.
func pruneHistory(ctx context.Context, tx *bolt.Tx, root, before string, eligible func(string, string, *bolt.Bucket) (bool, error)) error {
	return eachBucket(ctx, tx.Bucket([]byte(root)), root, func(k []byte, days *bolt.Bucket) error {
		id := string(k)
		var expired []string
		err := scanDays(ctx, days, fmt.Sprintf("%s/%q", root, id), "", before, func(day string, b *bolt.Bucket) error {
			if day >= before {
				return nil
			}
			remove, err := eligible(id, day, b)
			if err != nil {
				return err
			}
			if remove {
				expired = append(expired, day)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, day := range expired {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := days.DeleteBucket([]byte(day)); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveService atomically deletes the service, its raw/daily history, and all
// associated instances. An unknown ID is a no-op. Active/detached policy belongs
// to the caller, not this backend.
func (s *Store) RemoveService(ctx context.Context, serviceID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if serviceID == "" {
		return fmt.Errorf("remove service: ID is required")
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		for _, name := range []string{"services", "samples", "daily"} {
			parent := tx.Bucket([]byte(name))
			b, err := child(parent, []byte(serviceID), fmt.Sprintf("%s/%q", name, serviceID), false)
			if err != nil {
				return err
			}
			if b != nil {
				if err := parent.DeleteBucket([]byte(serviceID)); err != nil {
					return err
				}
			}
		}
		instances := tx.Bucket([]byte("instances"))
		var remove [][]byte
		err := eachBucket(ctx, instances, "instances", func(k []byte, b *bolt.Bucket) error {
			instance, err := readInstance(b, k)
			if err != nil {
				return err
			}
			if instance.ServiceID == serviceID {
				remove = append(remove, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, key := range remove {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := instances.DeleteBucket(key); err != nil {
				return err
			}
		}
		return nil
	})
}
