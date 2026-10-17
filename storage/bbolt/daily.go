package bbolt

import (
	"context"
	"fmt"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	bolt "go.etcd.io/bbolt"
)

func readDaily(b *bolt.Bucket, serviceID, day string) (uptimestorage.DailyStatus, error) {
	row := uptimestorage.DailyStatus{ServiceID: serviceID, Day: day}
	path := fmt.Sprintf("daily/%q/%s", serviceID, day)
	var err error
	if row.UpSlots, err = readCount(b, "up_slots", path); err != nil {
		return row, err
	}
	if row.ExpectedSlots, err = readCount(b, "expected_slots", path); err != nil {
		return row, err
	}
	v, err := field(b, "finalized", path, true)
	if err != nil {
		return row, err
	}
	row.Finalized, err = decodeBool(v)
	if err != nil {
		return row, fmt.Errorf("%s/finalized: %w", path, err)
	}
	return row, nil
}

func historyDay(tx *bolt.Tx, root, serviceID, day string, create bool) (*bolt.Bucket, error) {
	path := fmt.Sprintf("%s/%q", root, serviceID)
	service, err := child(tx.Bucket([]byte(root)), []byte(serviceID), path, create)
	if err != nil || service == nil {
		return nil, err
	}
	return child(service, []byte(day), path+"/"+day, create)
}

func scanDays(ctx context.Context, b *bolt.Bucket, path, from, to string, fn func(string, *bolt.Bucket) error) error {
	c := b.Cursor()
	k, v := c.First()
	if from != "" {
		k, v = c.Seek([]byte(from))
	}
	for ; k != nil; k, v = c.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		day := string(k)
		if err := validDay(day); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if to != "" && day > to {
			break
		}
		if v != nil {
			return fmt.Errorf("%s/%s: expected day bucket, found value", path, day)
		}
		if err := fn(day, b.Bucket(k)); err != nil {
			return err
		}
	}
	return nil
}

// RollupDaily finalizes sample days before BeforeDay. The optional callback is
// invoked synchronously outside all transactions and is never retained. A short
// write transaction rechecks finalized state and reads the latest slot count.
// Finalized rows are immutable, even if later heartbeats add raw samples.
func (s *Store) RollupDaily(ctx context.Context, options uptimestorage.RollupOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := optionalDay(options.BeforeDay); err != nil {
		return err
	}
	type candidate struct {
		service uptimestorage.Service
		day     string
	}
	var candidates []candidate
	err := s.view(ctx, func(tx *bolt.Tx) error {
		if options.BeforeDay == "" {
			return nil
		}
		services, err := listServices(ctx, tx)
		if err != nil {
			return err
		}
		for _, service := range services {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := fmt.Sprintf("samples/%q", service.ID)
			days, err := child(tx.Bucket([]byte("samples")), []byte(service.ID), path, false)
			if err != nil {
				return err
			}
			if days == nil {
				continue
			}
			err = scanDays(ctx, days, path, "", options.BeforeDay, func(day string, samples *bolt.Bucket) error {
				if day >= options.BeforeDay {
					return nil
				}
				if _, err := validateSampleDay(ctx, samples, path+"/"+day); err != nil {
					return err
				}
				daily, err := historyDay(tx, "daily", service.ID, day, false)
				if err != nil {
					return err
				}
				if daily != nil {
					row, err := readDaily(daily, service.ID, day)
					if err != nil {
						return err
					}
					if row.Finalized {
						return nil
					}
				}
				candidates = append(candidates, candidate{service, day})
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		expected := 0
		// Caller code may query or write this Store; holding a transaction here
		// could deadlock it or keep the database locked during slow work.
		if options.ExpectedSlots != nil {
			expected = options.ExpectedSlots(c.service, c.day)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if expected < 0 {
			return fmt.Errorf("daily/%q/%s: negative expected slot count", c.service.ID, c.day)
		}
		// Candidates can go stale while the callback runs. Recheck existence and
		// finality to avoid recreating removed history or overwriting a rollup.
		err = s.update(ctx, func(tx *bolt.Tx) error {
			service, err := child(tx.Bucket([]byte("services")), []byte(c.service.ID), "services/"+c.service.ID, false)
			if err != nil || service == nil {
				return err
			}
			if _, err := readService(service, c.service.ID); err != nil {
				return err
			}
			daily, err := historyDay(tx, "daily", c.service.ID, c.day, false)
			if err != nil {
				return err
			}
			if daily != nil {
				row, err := readDaily(daily, c.service.ID, c.day)
				if err != nil {
					return err
				}
				if row.Finalized {
					return nil
				}
			}
			samples, err := historyDay(tx, "samples", c.service.ID, c.day, false)
			if err != nil || samples == nil {
				return err
			}
			up, err := validateSampleDay(ctx, samples, fmt.Sprintf("samples/%q/%s", c.service.ID, c.day))
			if err != nil {
				return err
			}
			if daily == nil {
				daily, err = historyDay(tx, "daily", c.service.ID, c.day, true)
				if err != nil {
					return err
				}
			}
			if err := daily.Put([]byte("up_slots"), encodeInt64(int64(up))); err != nil {
				return err
			}
			if err := daily.Put([]byte("expected_slots"), encodeInt64(int64(expected))); err != nil {
				return err
			}
			return daily.Put([]byte("finalized"), encodeBool(true))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func selectedIDs(ctx context.Context, tx *bolt.Tx, ids []string) ([]string, error) {
	if ids == nil {
		services, err := listServices(ctx, tx)
		if err != nil {
			return nil, err
		}
		for _, service := range services {
			ids = append(ids, service.ID)
		}
	}
	selected := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !seen[id] {
			selected = append(selected, id)
			seen[id] = true
		}
	}
	return selected, nil
}

// QueryDaily returns matching history with inclusive bounds. Nil ServiceIDs
// selects registered services; an explicit empty selection returns no rows.
// Empty bounds are unbounded. Result ordering is unspecified.
func (s *Store) QueryDaily(ctx context.Context, options uptimestorage.QueryDailyOptions) ([]uptimestorage.DailyStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := optionalDay(options.FromDay); err != nil {
		return nil, err
	}
	if err := optionalDay(options.ToDay); err != nil {
		return nil, err
	}
	var rows []uptimestorage.DailyStatus
	err := s.view(ctx, func(tx *bolt.Tx) error {
		ids, err := selectedIDs(ctx, tx, options.ServiceIDs)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := fmt.Sprintf("daily/%q", id)
			days, err := child(tx.Bucket([]byte("daily")), []byte(id), path, false)
			if err != nil {
				return err
			}
			if days == nil {
				continue
			}
			err = scanDays(ctx, days, path, options.FromDay, options.ToDay, func(day string, b *bolt.Bucket) error {
				row, err := readDaily(b, id, day)
				if err != nil {
					return err
				}
				rows = append(rows, row)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// QueryTodaySamples returns nonzero raw summaries for one day. An empty Day
// returns no rows. Service selection has the same semantics as QueryDaily;
// result ordering is unspecified.
func (s *Store) QueryTodaySamples(ctx context.Context, options uptimestorage.QueryTodaySamplesOptions) ([]uptimestorage.TodaySampleStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := optionalDay(options.Day); err != nil {
		return nil, err
	}
	var rows []uptimestorage.TodaySampleStatus
	err := s.view(ctx, func(tx *bolt.Tx) error {
		if options.Day == "" {
			return nil
		}
		ids, err := selectedIDs(ctx, tx, options.ServiceIDs)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			day, err := historyDay(tx, "samples", id, options.Day, false)
			if err != nil {
				return err
			}
			if day == nil {
				continue
			}
			up, err := readSampleCount(day, fmt.Sprintf("samples/%q/%s", id, options.Day))
			if err != nil {
				return err
			}
			if up != 0 {
				rows = append(rows, uptimestorage.TodaySampleStatus{ServiceID: id, Day: options.Day, UpSlots: up})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
