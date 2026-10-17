package bbolt

import (
	"context"
	"fmt"
	"math"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	bolt "go.etcd.io/bbolt"
)

// WriteHeartbeat atomically records a unique service/day/slot and advances
// existing metadata. It does not register services or process instances.
// Repeated writes to the same slot never increment the successful-slot count.
func (s *Store) WriteHeartbeat(ctx context.Context, h uptimestorage.Heartbeat) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.ServiceID == "" {
		return fmt.Errorf("heartbeat: ServiceID is required")
	}
	if err := validDay(h.Day); err != nil {
		return err
	}
	if h.Slot < 0 {
		return fmt.Errorf("heartbeat: slot must not be negative")
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		servicePath := fmt.Sprintf("services/%q", h.ServiceID)
		service, err := child(tx.Bucket([]byte("services")), []byte(h.ServiceID), servicePath, false)
		if err != nil {
			return err
		}
		if service != nil {
			if _, err := readService(service, h.ServiceID); err != nil {
				return err
			}
			if err := advanceTime(service, "last_seen_at", servicePath, h.SeenAt); err != nil {
				return err
			}
		}
		instancePath := fmt.Sprintf("instances/%d", h.InstanceID)
		instance, err := child(tx.Bucket([]byte("instances")), encodeInt64(h.InstanceID), instancePath, false)
		if err != nil {
			return err
		}
		if instance != nil {
			if _, err := readInstance(instance, encodeInt64(h.InstanceID)); err != nil {
				return err
			}
			if err := advanceTime(instance, "last_seen_at", instancePath, h.SeenAt); err != nil {
				return err
			}
		}
		path := fmt.Sprintf("samples/%q", h.ServiceID)
		serviceDays, err := child(tx.Bucket([]byte("samples")), []byte(h.ServiceID), path, true)
		if err != nil {
			return err
		}
		path += "/" + h.Day
		day, err := child(serviceDays, []byte(h.Day), path, false)
		if err != nil {
			return err
		}
		if day == nil {
			day, err = child(serviceDays, []byte(h.Day), path, true)
			if err != nil {
				return err
			}
			if err := day.Put([]byte("up_slots"), encodeInt64(0)); err != nil {
				return err
			}
			if _, err := day.CreateBucket([]byte("slots")); err != nil {
				return err
			}
		}
		count, err := readSampleCount(day, path)
		if err != nil {
			return err
		}
		slots := day.Bucket([]byte("slots"))
		key := encodeInt64(h.Slot)
		if slots.Bucket(key) != nil {
			return fmt.Errorf("%s/slots/%d: invalid slot marker", path, h.Slot)
		}
		if marker := slots.Get(key); marker != nil {
			if len(marker) != 1 || marker[0] != 1 {
				return fmt.Errorf("%s/slots/%d: invalid slot marker", path, h.Slot)
			}
			return nil
		}
		if count == math.MaxInt {
			return fmt.Errorf("%s/up_slots: count overflow", path)
		}
		if err := slots.Put(key, []byte{1}); err != nil {
			return err
		}
		return day.Put([]byte("up_slots"), encodeInt64(int64(count+1)))
	})
}

// readSampleCount checks the required counter and bucket without scanning slots.
// Hot paths trust this strictly decoded count; maintenance validates all slots.
func readSampleCount(day *bolt.Bucket, path string) (int, error) {
	count, err := readCount(day, "up_slots", path)
	if err != nil {
		return 0, err
	}
	slots, err := child(day, []byte("slots"), path+"/slots", false)
	if err != nil {
		return 0, err
	}
	if slots == nil {
		return 0, fmt.Errorf("%s/slots: missing required bucket", path)
	}
	return count, nil
}

// validateSampleDay checks every slot against the persisted counter. Maintenance
// rejects inconsistent history without repairing it.
func validateSampleDay(ctx context.Context, day *bolt.Bucket, path string) (int, error) {
	count, err := readSampleCount(day, path)
	if err != nil {
		return 0, err
	}
	slots := day.Bucket([]byte("slots"))
	actual := 0
	err = slots.ForEach(func(k, v []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		slot, err := decodeInt64(k)
		if err != nil {
			return fmt.Errorf("%s/slots key: %w", path, err)
		}
		if slot < 0 {
			return fmt.Errorf("%s/slots: negative slot key", path)
		}
		if len(v) != 1 || v[0] != 1 {
			return fmt.Errorf("%s/slots/%d: invalid slot marker", path, slot)
		}
		if actual == math.MaxInt {
			return fmt.Errorf("%s/slots: count overflow", path)
		}
		actual++
		return nil
	})
	if err != nil {
		return 0, err
	}
	if actual != count {
		return 0, fmt.Errorf("%s/up_slots: count %d does not match %d slot keys", path, count, actual)
	}
	return count, nil
}
