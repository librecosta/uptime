package bbolt

import (
	"context"
	"fmt"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	bolt "go.etcd.io/bbolt"
)

// UpsertService refreshes display metadata while preserving meaningful CreatedAt
// and monotonically advancing LastSeenAt. IDs are opaque, non-empty strings.
// If creation time is not yet set, incoming LastSeenAt supplies the fallback;
// both timestamps zero leave it unset for a later upsert.
func (s *Store) UpsertService(ctx context.Context, service uptimestorage.Service) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if service.ID == "" {
		return fmt.Errorf("upsert service: ID is required")
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		path := fmt.Sprintf("services/%q", service.ID)
		b, err := child(tx.Bucket([]byte("services")), []byte(service.ID), path, false)
		if err != nil {
			return err
		}
		var previous uptimestorage.Service
		if b != nil {
			previous, err = readService(b, service.ID)
			if err != nil {
				return err
			}
		} else {
			b, err = child(tx.Bucket([]byte("services")), []byte(service.ID), path, true)
			if err != nil {
				return err
			}
		}
		candidate := service.CreatedAt
		if candidate.IsZero() {
			candidate = service.LastSeenAt
		}
		if previous.CreatedAt.IsZero() && !candidate.IsZero() {
			if err := putTime(b, "created_at", path, candidate); err != nil {
				return err
			}
		}
		if err := advanceTime(b, "last_seen_at", path, service.LastSeenAt); err != nil {
			return err
		}
		for _, f := range []struct {
			name  string
			value []byte
		}{
			{"name", []byte(service.Name)}, {"description", []byte(service.Description)},
			{"sample_interval", encodeInt64(int64(service.SampleInterval))},
		} {
			if err := b.Put([]byte(f.name), f.value); err != nil {
				return fmt.Errorf("%s/%s: %w", path, f.name, err)
			}
		}
		return nil
	})
}

func readService(b *bolt.Bucket, id string) (uptimestorage.Service, error) {
	service := uptimestorage.Service{ID: id}
	path := fmt.Sprintf("services/%q", id)
	var err error
	if service.Name, err = readString(b, "name", path); err != nil {
		return service, err
	}
	if service.Name == "" {
		service.Name = id
	}
	if service.Description, err = readString(b, "description", path); err != nil {
		return service, err
	}
	if service.CreatedAt, err = readTime(b, "created_at", path); err != nil {
		return service, err
	}
	if service.LastSeenAt, err = readTime(b, "last_seen_at", path); err != nil {
		return service, err
	}
	v, err := field(b, "sample_interval", path, true)
	if err != nil {
		return service, err
	}
	n, err := decodeInt64(v)
	if err != nil {
		return service, fmt.Errorf("%s/sample_interval: %w", path, err)
	}
	service.SampleInterval = time.Duration(n)
	return service, nil
}

// ListServices returns all persisted metadata. Ordering is unspecified.
func (s *Store) ListServices(ctx context.Context) ([]uptimestorage.Service, error) {
	var services []uptimestorage.Service
	err := s.view(ctx, func(tx *bolt.Tx) error {
		var err error
		services, err = listServices(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return services, nil
}

func listServices(ctx context.Context, tx *bolt.Tx) ([]uptimestorage.Service, error) {
	var services []uptimestorage.Service
	err := eachBucket(ctx, tx.Bucket([]byte("services")), "services", func(k []byte, b *bolt.Bucket) error {
		service, err := readService(b, string(k))
		if err != nil {
			return err
		}
		services = append(services, service)
		return nil
	})
	return services, err
}

// UpsertInstance refreshes the instance association and display metadata while
// preserving meaningful StartedAt and monotonically advancing LastSeenAt.
// An unset start time falls back to incoming LastSeenAt, as in UpsertService.
func (s *Store) UpsertInstance(ctx context.Context, instance uptimestorage.Instance) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if instance.ID == 0 {
		return fmt.Errorf("upsert instance: ID must not be zero")
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		path := fmt.Sprintf("instances/%d", instance.ID)
		b, err := child(tx.Bucket([]byte("instances")), encodeInt64(instance.ID), path, false)
		if err != nil {
			return err
		}
		var previous uptimestorage.Instance
		if b != nil {
			previous, err = readInstance(b, encodeInt64(instance.ID))
			if err != nil {
				return err
			}
		} else {
			b, err = child(tx.Bucket([]byte("instances")), encodeInt64(instance.ID), path, true)
			if err != nil {
				return err
			}
		}
		candidate := instance.StartedAt
		if candidate.IsZero() {
			candidate = instance.LastSeenAt
		}
		if previous.StartedAt.IsZero() && !candidate.IsZero() {
			if err := putTime(b, "started_at", path, candidate); err != nil {
				return err
			}
		}
		if err := advanceTime(b, "last_seen_at", path, instance.LastSeenAt); err != nil {
			return err
		}
		for _, f := range []struct {
			name  string
			value []byte
		}{
			{"service_id", []byte(instance.ServiceID)}, {"hostname", []byte(instance.Hostname)}, {"pid", encodeInt64(int64(instance.PID))},
		} {
			if err := b.Put([]byte(f.name), f.value); err != nil {
				return fmt.Errorf("%s/%s: %w", path, f.name, err)
			}
		}
		return nil
	})
}

func readInstance(b *bolt.Bucket, key []byte) (uptimestorage.Instance, error) {
	var instance uptimestorage.Instance
	id, err := decodeInt64(key)
	if err != nil {
		return instance, fmt.Errorf("instances key: %w", err)
	}
	if id == 0 {
		return instance, fmt.Errorf("instances key: zero ID")
	}
	instance.ID = id
	path := fmt.Sprintf("instances/%d", id)
	if instance.ServiceID, err = readString(b, "service_id", path); err != nil {
		return instance, err
	}
	if instance.Hostname, err = readString(b, "hostname", path); err != nil {
		return instance, err
	}
	if instance.PID, err = readInt(b, "pid", path, false); err != nil {
		return instance, err
	}
	if instance.StartedAt, err = readTime(b, "started_at", path); err != nil {
		return instance, err
	}
	if instance.LastSeenAt, err = readTime(b, "last_seen_at", path); err != nil {
		return instance, err
	}
	return instance, nil
}
