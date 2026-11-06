package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore borrows a standalone Redis client. The caller owns its lifecycle.
// Alert hashes and the history index have no TTL. This is not a distributed
// executor or Redis Cluster backend; WATCH covers all keys in one database transaction.
type RedisStore struct {
	client                    *redis.Client
	incidents, active, outbox string
	history, meta             string
	initMu                    sync.Mutex
	ready                     bool
}

// NewRedisStore creates no connections, workers, or keys. prefix is an explicit
// deployment namespace; the fixed alerts:v1 suffix isolates monitoring keys.
func NewRedisStore(client *redis.Client, prefix string) (*RedisStore, error) {
	if client == nil || strings.TrimSpace(prefix) == "" {
		return nil, ErrInvalid
	}
	base := strings.TrimRight(prefix, ":") + ":alerts:v1:"
	return &RedisStore{client: client, incidents: base + "incidents", active: base + "active", outbox: base + "outbox", history: base + "history", meta: base + "meta"}, nil
}

// Check migrates legacy alert records in resumable batches and marks Schema v2
// ready only after validation. Call before handing the borrowed client to workers.
func (s *RedisStore) Check(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if err := s.migrate(ctx); err != nil {
		return err
	}
	s.ready = true
	return nil
}
func (s *RedisStore) ensure(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if s.ready {
		return ctx.Err()
	}
	if err := s.migrate(ctx); err != nil {
		return err
	}
	s.ready = true
	return nil
}
func (s *RedisStore) checkTypes(ctx context.Context, client redis.Cmdable) error {
	for _, key := range []string{s.incidents, s.active, s.outbox, s.meta, s.history} {
		kind, err := client.Type(ctx, key).Result()
		if err != nil {
			return err
		}
		want := "hash"
		if key == s.history {
			want = "zset"
		}
		if kind != "none" && kind != want {
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
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	return redisIncident(ctx, s.client, s.incidents, id)
}

// Active uses WATCH even for reads so the index and record form one snapshot.
func (s *RedisStore) Active(ctx context.Context, endpoint string) (result *Incident, err error) {
	if err = s.ensure(ctx); err != nil {
		return nil, err
	}
	err = s.watchReady(ctx, func(tx *redis.Tx) error {
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
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			err := s.checkTypes(ctx, tx)
			if err == nil {
				err = fn(tx)
			}
			if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) {
				// WATCH does not make separate reads a snapshot. A concurrent EXEC
				// can invalidate conditions before fn reaches its own EXEC. Validate
				// these logical errors with a read-only transaction before returning
				// them; genuine conflicts remain errors, torn reads retry instead.
				if _, checkErr := tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Ping(ctx); return nil }); checkErr != nil {
					return checkErr
				}
			}
			return err
		}, s.incidents, s.active, s.outbox, s.history, s.meta)
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return ErrConflict
}

func (s *RedisStore) watchReady(ctx context.Context, fn func(*redis.Tx) error) error {
	return s.watch(ctx, func(tx *redis.Tx) error {
		fields, err := tx.HMGet(ctx, s.meta, "format", "schema_version", "state").Result()
		if err != nil {
			return err
		}
		if fields[0] != alertFormat || fields[1] != "2" || fields[2] != "ready" {
			return ErrInvalid
		}
		return fn(tx)
	})
}

// Apply uses watched conditions and one EXEC for the incident, history and tasks.
func (s *RedisStore) Apply(ctx context.Context, change Transition) (result Incident, err error) {
	if err = s.ensure(ctx); err != nil {
		return result, err
	}
	err = s.watchReady(ctx, func(tx *redis.Tx) error {
		i, err := redisIncident(ctx, tx, s.incidents, change.Incident.ID)
		if err != nil {
			return err
		}
		activeID, err := tx.HGet(ctx, s.active, change.Incident.EndpointID).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		var tasks []Task
		result, tasks, err = prepare(i, activeID, change)
		if err != nil {
			return err
		}
		if tasks == nil {
			return redisReadCommit(ctx, tx)
		}
		tasks, err = transitionTasks(result, tasks, func(id string) (*Task, error) { return redisTask(ctx, tx, s.outbox, id) })
		if err != nil {
			return err
		}
		return s.writeIncident(ctx, tx, result, tasks)
	})
	return
}

// ApplyReminder atomically advances a conditional sequence and its channel tasks.
func (s *RedisStore) ApplyReminder(ctx context.Context, c ReminderChange) (result Incident, err error) {
	if err = s.ensure(ctx); err != nil {
		return result, err
	}
	err = s.watchReady(ctx, func(tx *redis.Tx) error {
		i, err := redisIncident(ctx, tx, s.incidents, c.IncidentID)
		if err != nil {
			return err
		}
		if i == nil {
			return ErrNotFound
		}
		activeID, err := tx.HGet(ctx, s.active, i.EndpointID).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		read := func(id string) (*Task, error) { return redisTask(ctx, tx, s.outbox, id) }
		var tasks []Task
		result, tasks, err = prepareReminderChange(i, activeID, c, read)
		if err != nil {
			return err
		}
		if tasks == nil {
			return redisReadCommit(ctx, tx)
		}
		tasks, err = transitionTasks(result, tasks, read)
		if err != nil {
			return err
		}
		return s.writeIncident(ctx, tx, result, tasks)
	})
	return
}
func redisReadCommit(ctx context.Context, tx *redis.Tx) error {
	_, err := tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Ping(ctx); return nil })
	return err
}
func taskPayload(tasks []Task) (map[string]any, error) {
	payload := make(map[string]any, len(tasks))
	for _, t := range tasks {
		data, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		payload[t.EventID] = data
	}
	return payload, nil
}
func (s *RedisStore) writeIncident(ctx context.Context, tx *redis.Tx, i Incident, tasks []Task) error {
	key, err := historyKey(i)
	if err != nil {
		return err
	}
	existing, err := tx.HExists(ctx, s.incidents, i.ID).Result()
	if err != nil {
		return err
	}
	score, err := tx.ZScore(ctx, s.history, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	if existing && (err != nil || score != 0) || !existing && err == nil {
		return ErrInvalid
	}
	data, err := json.Marshal(i)
	if err != nil {
		return err
	}
	payload, err := taskPayload(tasks)
	if err != nil {
		return err
	}
	_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, s.incidents, i.ID, data)
		p.ZAdd(ctx, s.history, redis.Z{Score: 0, Member: key})
		if i.RecoveredAt.IsZero() {
			p.HSet(ctx, s.active, i.EndpointID, i.ID)
		} else {
			p.HDel(ctx, s.active, i.EndpointID)
		}
		if len(payload) > 0 {
			p.HSet(ctx, s.outbox, payload)
		}
		return nil
	})
	return err
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
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
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
	if err := s.ensure(ctx); err != nil {
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
		current, _, err := s.ReadDelivery(ctx, task.EventID, q.Now)
		if err != nil {
			return TaskPage{}, err
		}
		if current != nil {
			page.Tasks = append(page.Tasks, *current)
		}
	}
	if end < uint64(len(values)/2) {
		page.NextCursor = strconv.FormatUint(cursor, 10) + ":" + strconv.FormatUint(end, 10)
	} else if next != 0 {
		page.NextCursor = strconv.FormatUint(next, 10) + ":0"
	}
	return page, nil
}

// ReadDelivery rechecks a candidate, incident and dependencies under WATCH.
func (s *RedisStore) ReadDelivery(ctx context.Context, id string, now time.Time) (task *Task, i *Incident, err error) {
	if err = s.ensure(ctx); err != nil {
		return
	}
	err = s.watchReady(ctx, func(tx *redis.Tx) error {
		task, i = nil, nil
		t, e := redisTask(ctx, tx, s.outbox, id)
		if e != nil {
			return e
		}
		if t == nil {
			return ErrNotFound
		}
		incident, e := redisIncident(ctx, tx, s.incidents, t.IncidentID)
		if e != nil {
			return e
		}
		if incident == nil {
			return ErrInvalid
		}
		down, previous, e := deliveryDependencies(*t, *incident, func(id string) (*Task, error) { return redisTask(ctx, tx, s.outbox, id) })
		if e != nil {
			return e
		}
		ready, e := DeliveryReady(*t, *incident, down, previous, now)
		if e != nil {
			return e
		}
		if ready {
			task, i = t, incident
		}
		return redisReadCommit(ctx, tx)
	})
	return
}

// RecordOutcome atomically rechecks dependencies and commits all resulting skips.
func (s *RedisStore) RecordOutcome(ctx context.Context, o Outcome) (result Task, err error) {
	if _, _, err := normalizeOutcome(o); err != nil {
		return Task{}, err
	}
	if err = s.ensure(ctx); err != nil {
		return result, err
	}
	err = s.watchReady(ctx, func(tx *redis.Tx) error {
		read := func(id string) (*Task, error) { return redisTask(ctx, tx, s.outbox, id) }
		t, err := read(o.EventID)
		if err != nil {
			return err
		}
		if t == nil {
			return ErrNotFound
		}
		i, err := redisIncident(ctx, tx, s.incidents, t.IncidentID)
		if err != nil {
			return err
		}
		if i == nil {
			return ErrInvalid
		}
		var tasks []Task
		result, tasks, err = outcomeTasks(*t, *i, o, read)
		if err != nil {
			return err
		}
		if tasks == nil {
			return redisReadCommit(ctx, tx)
		}
		payload, err := taskPayload(tasks)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.HSet(ctx, s.outbox, payload); return nil })
		return err
	})
	return
}

// ListIncidents uses score-zero reverse lexicographic keyset pagination. Check
// must have completed Schema v2 initialization; this query never migrates data.
func (s *RedisStore) ListIncidents(ctx context.Context, q HistoryQuery) (page IncidentPage, err error) {
	limit, anchor, err := historyQuery(q)
	if err != nil {
		return page, err
	}
	err = s.watchReady(ctx, func(tx *redis.Tx) error {
		page = IncidentPage{}
		maxKey := "+"
		if anchor != "" {
			maxKey = "(" + anchor
		}
		keys, e := tx.ZRevRangeByLex(ctx, s.history, &redis.ZRangeBy{Max: maxKey, Min: "-", Offset: 0, Count: int64(limit + 1)}).Result()
		if e != nil {
			return e
		}
		count, e := tx.ZCard(ctx, s.history).Result()
		if e != nil {
			return e
		}
		rows, e := tx.HLen(ctx, s.incidents).Result()
		if e != nil {
			return e
		}
		if count != rows {
			return ErrInvalid
		}
		lastKey := ""
		for _, key := range keys {
			score, e := tx.ZScore(ctx, s.history, key).Result()
			if e != nil {
				return e
			}
			if score != 0 {
				return ErrInvalid
			}
			id, e := historyID(key)
			if e != nil {
				return e
			}
			i, e := redisIncident(ctx, tx, s.incidents, id)
			if e != nil {
				return e
			}
			if indexedIncident(key, i) != nil {
				return ErrInvalid
			}
			if len(page.Incidents) == limit {
				page.NextCursor = historyCursor(lastKey)
				break
			}
			page.Incidents = append(page.Incidents, *i)
			lastKey = key
		}
		return redisReadCommit(ctx, tx)
	})
	if err != nil {
		return IncidentPage{}, err
	}
	return
}

// ReadIncident validates its watched read set with EXEC, retrying concurrent
// incident/outcome changes. It never initializes schema or scans the task hash.
func (s *RedisStore) ReadIncident(ctx context.Context, q DetailQuery) (page IncidentDetail, err error) {
	if err = q.Validate(); err != nil {
		return page, err
	}
	err = s.watchReady(ctx, func(tx *redis.Tx) error {
		page = IncidentDetail{}
		i, e := redisIncident(ctx, tx, s.incidents, q.IncidentID)
		if e != nil {
			return e
		}
		page, e = readIncidentDetail(i, q, func(id string) (*Task, error) { return redisTask(ctx, tx, s.outbox, id) })
		if e != nil {
			return e
		}
		return redisReadCommit(ctx, tx)
	})
	if err != nil {
		return IncidentDetail{}, err
	}
	return
}

// Outbox reads the task hash atomically and sorts by EventID, without delivery work.
func (s *RedisStore) Outbox(ctx context.Context) ([]Task, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
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
