package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
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
			if task.Type == Recovered {
				down, err := redisTask(ctx, tx, s.outbox, eventID(task.IncidentID, Down, task.ChannelID))
				if err != nil {
					return err
				}
				task, err = skipRecovery(task, down)
				if err != nil {
					return err
				}
			}
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

var _ DeliveryStore = (*RedisStore)(nil)

func redisTask(ctx context.Context, client redis.Cmdable, key, id string) (*Task, error) {
	data, err := client.HGet(ctx, key, id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	task, err := decodeTask(data)
	if err != nil {
		return nil, err
	}
	if task.EventID != id {
		return nil, ErrInvalid
	}
	return &task, nil
}

// GetTask reads current delivery state and replay receipts by EventID.
func (s *RedisStore) GetTask(ctx context.Context, id string) (*Task, error) {
	return redisTask(ctx, s.client, s.outbox, id)
}

// QueryDue scans one HSCAN batch, evaluating at most Limit records. Redis COUNT
// is a hint (compact hashes can return more); an offset in the opaque cursor
// resumes oversized batches without dropping their remaining entries. This is
// a weakly consistent scan, not a reservation or cross-page snapshot.
func (s *RedisStore) QueryDue(ctx context.Context, q DueQuery) (TaskPage, error) {
	if err := validateQuery(q); err != nil {
		return TaskPage{}, err
	}
	var cursor, offset uint64
	if q.Cursor != "" {
		parts := strings.Split(q.Cursor, ":")
		if len(parts) != 2 || len(q.Cursor) > 41 {
			return TaskPage{}, ErrInvalid
		}
		var err error
		cursor, err = strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			return TaskPage{}, ErrInvalid
		}
		offset, err = strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return TaskPage{}, ErrInvalid
		}
	}
	values, next, err := s.client.HScan(ctx, s.outbox, cursor, "*", int64(q.Limit)).Result()
	if err != nil {
		return TaskPage{}, err
	}
	if len(values)%2 != 0 {
		return TaskPage{}, ErrInvalid
	}
	var page TaskPage
	// A concurrent encoding change/rehash may shrink a resumed batch. Advance
	// to its next Redis cursor; later passes revisit any displaced records.
	offset = min(offset, uint64(len(values)/2))
	end := min(uint64(len(values)/2), offset+uint64(q.Limit))
	if end < offset {
		return TaskPage{}, ErrInvalid
	}
	for n := offset; n < end; n++ {
		if err := ctx.Err(); err != nil {
			return TaskPage{}, err
		}
		task, err := decodeTask([]byte(values[2*n+1]))
		if err != nil {
			return TaskPage{}, err
		}
		if task.EventID != values[2*n] {
			return TaskPage{}, ErrInvalid
		}
		var down *Task
		if task.Type == Recovered && task.Status == TaskPending {
			down, err = s.GetTask(ctx, eventID(task.IncidentID, Down, task.ChannelID))
			if err != nil {
				return TaskPage{}, err
			}
			if down == nil {
				return TaskPage{}, ErrInvalid
			}
		}
		if deliverable(task, down, q.Now) {
			page.Tasks = append(page.Tasks, task)
		}
	}
	if end < uint64(len(values)/2) {
		page.NextCursor = strconv.FormatUint(cursor, 10) + ":" + strconv.FormatUint(end, 10)
	} else if next != 0 {
		page.NextCursor = strconv.FormatUint(next, 10) + ":0"
	}
	return page, nil
}

// RecordOutcome uses WATCH and a single HSET for the task and dependent recovery.
// Conditional conflicts retry; network errors return unchanged for reconciliation.
func (s *RedisStore) RecordOutcome(ctx context.Context, o Outcome) (result Task, err error) {
	if _, _, err := normalizeOutcome(o); err != nil {
		return Task{}, err
	}
	err = s.watch(ctx, func(tx *redis.Tx) error {
		task, err := redisTask(ctx, tx, s.outbox, o.EventID)
		if err != nil {
			return err
		}
		if task == nil {
			return ErrNotFound
		}
		var down *Task
		if task.Type == Recovered {
			down, err = redisTask(ctx, tx, s.outbox, eventID(task.IncidentID, Down, task.ChannelID))
			if err != nil {
				return err
			}
			if down == nil {
				return ErrInvalid
			}
		}
		var changed bool
		result, changed, err = prepareOutcome(*task, down, o)
		if err != nil || !changed {
			return err
		}
		payload := make(map[string]any, 2)
		if result.Type == Down && (result.Status == TaskFailed || result.Status == TaskSkipped) {
			recovery, err := redisTask(ctx, tx, s.outbox, eventID(result.IncidentID, Recovered, result.ChannelID))
			if err != nil {
				return err
			}
			if recovery != nil {
				skipped, err := skipRecovery(*recovery, &result)
				if err != nil {
					return err
				}
				data, err := json.Marshal(skipped)
				if err != nil {
					return err
				}
				payload[skipped.EventID] = data
			}
		}
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		payload[result.EventID] = data
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.HSet(ctx, s.outbox, payload); return nil })
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
