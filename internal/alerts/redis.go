package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisStore borrows a standalone Redis client. The caller owns its lifecycle.
// Three alert-only hashes have no TTL. This is not a distributed executor or
// Redis Cluster backend; WATCH covers all keys in one database transaction.
type RedisStore struct {
	client                    *redis.Client
	incidents, active, outbox string
}

// NewRedisStore creates no connections, workers, or keys. prefix is an explicit
// deployment namespace; the fixed alerts:v1 suffix isolates monitoring keys.
func NewRedisStore(client *redis.Client, prefix string) (*RedisStore, error) {
	if client == nil || strings.TrimSpace(prefix) == "" {
		return nil, ErrInvalid
	}
	base := strings.TrimRight(prefix, ":") + ":alerts:v1:"
	return &RedisStore{client: client, incidents: base + "incidents", active: base + "active", outbox: base + "outbox"}, nil
}

// Check validates connectivity and namespace types before runtime handoff. It
// creates no keys; Apply still checks types under WATCH before every transition.
func (s *RedisStore) Check(ctx context.Context) error {
	for _, key := range []string{s.incidents, s.active, s.outbox} {
		kind, err := s.client.Type(ctx, key).Result()
		if err != nil {
			return err
		}
		if kind != "none" && kind != "hash" {
			return ErrInvalid
		}
	}
	return nil
}

func redisIncident(ctx context.Context, client redis.Cmdable, key, id string) (*Incident, error) {
	data, err := client.HGet(ctx, key, id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	i, err := decodeIncident(data)
	if err == nil && i != nil && i.ID != id {
		return nil, ErrInvalid
	}
	return i, err
}

// Get reads the durable incident for reconciliation after an uncertain EXEC result.
func (s *RedisStore) Get(ctx context.Context, id string) (*Incident, error) {
	return redisIncident(ctx, s.client, s.incidents, id)
}

// Active uses WATCH even for reads so the index and record form one snapshot.
func (s *RedisStore) Active(ctx context.Context, endpoint string) (result *Incident, err error) {
	err = s.watch(ctx, func(tx *redis.Tx) error {
		result = nil
		id, err := tx.HGet(ctx, s.active, endpoint).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		if err == nil {
			result, err = redisIncident(ctx, tx, s.incidents, id)
			if err != nil {
				return err
			}
			if result == nil || result.EndpointID != endpoint || !result.RecoveredAt.IsZero() {
				return ErrInvalid
			}
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Ping(ctx); return nil })
		return err
	})
	return result, err
}

func (s *RedisStore) watch(ctx context.Context, fn func(*redis.Tx) error) error {
	for range 32 {
		err := s.client.Watch(ctx, fn, s.incidents, s.active, s.outbox)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return ErrConflict
}

// Apply uses watched conditions and MULTI/EXEC for the incident, index, and tasks.
// Only transaction conflicts are retried; transport errors leave the result unknown.
func (s *RedisStore) Apply(ctx context.Context, change Transition) (result Incident, err error) {
	err = s.watch(ctx, func(tx *redis.Tx) error {
		existing, err := redisIncident(ctx, tx, s.incidents, change.Incident.ID)
		if err != nil {
			return err
		}
		activeID, err := tx.HGet(ctx, s.active, change.Incident.EndpointID).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		var tasks []Task
		result, tasks, err = prepare(existing, activeID, change)
		if err != nil || tasks == nil {
			return err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		// Redis EXEC does not roll back command type errors. Check every key's
		// type while watched, before queuing writes, to avoid partial mutations.
		for _, key := range []string{s.incidents, s.active, s.outbox} {
			kind, err := tx.Type(ctx, key).Result()
			if err != nil {
				return err
			}
			if kind != "none" && kind != "hash" {
				return ErrInvalid
			}
		}
		payload := make(map[string]any, len(tasks))
		for _, task := range tasks {
			found, err := tx.HExists(ctx, s.outbox, task.EventID).Result()
			if err != nil {
				return err
			}
			if found {
				return ErrConflict
			}
			data, err := json.Marshal(task)
			if err != nil {
				return err
			}
			payload[task.EventID] = data
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.HSet(ctx, s.incidents, result.ID, data)
			if change.Type == Down {
				p.HSet(ctx, s.active, result.EndpointID, result.ID)
			} else {
				p.HDel(ctx, s.active, result.EndpointID)
			}
			if len(payload) > 0 {
				p.HSet(ctx, s.outbox, payload)
			}
			return nil
		})
		return err
	})
	return result, err
}

// Outbox reads the task hash atomically and sorts by EventID, without delivery work.
func (s *RedisStore) Outbox(ctx context.Context) ([]Task, error) {
	values, err := s.client.HGetAll(ctx, s.outbox).Result()
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(values))
	for id, value := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task, err := decodeTask([]byte(value))
		if err != nil {
			return nil, err
		}
		if task.EventID != id {
			return nil, ErrInvalid
		}
		tasks = append(tasks, task)
	}
	slices.SortFunc(tasks, func(a, b Task) int { return strings.Compare(a.EventID, b.EventID) })
	return tasks, nil
}
