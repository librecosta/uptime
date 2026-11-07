package alerts

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const alertFormat = "librecosta-uptime-alerts"

var alertBuckets = []string{"meta", "incidents", "active", "outbox", "incidents_by_time"}

// BboltStore owns a separate, exclusively locked alert database. Close it after
// stopping callers. It never opens or migrates the monitoring history database.
type BboltStore struct {
	db        *bolt.DB
	closeOnce sync.Once
	closeErr  error
}

// OpenBbolt initializes only a file created by this call. Existing empty,
// foreign, damaged, or unsupported databases are refused without repair.
// Recognized v1 files upgrade transactionally before callers can access v2.
// Callers explicitly choose a path; the opt-in runtime uses a sibling alerts.db.
func OpenBbolt(path string, timeout time.Duration) (*BboltStore, error) {
	if path == "" || timeout < 0 {
		return nil, ErrInvalid
	}
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return nil, err
	}
	var created os.FileInfo
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: timeout, NoFreelistSync: true,
		OpenFile: func(path string, flag int, mode os.FileMode) (*os.File, error) {
			f, err := os.OpenFile(path, flag|os.O_EXCL, mode)
			fresh := err == nil
			if errors.Is(err, os.ErrExist) {
				f, err = os.OpenFile(path, flag&^os.O_CREATE, mode)
			}
			if err != nil {
				return nil, err
			}
			info, err := f.Stat()
			if err != nil {
				_ = f.Close()
				return nil, err
			}
			if fresh {
				created = info
			}
			if !info.Mode().IsRegular() || (!fresh && info.Size() == 0) {
				_ = f.Close()
				return nil, ErrInvalid
			}
			return f, nil
		},
	})
	if err == nil {
		if created != nil {
			db.NoFreelistSync = false
			err = db.Update(func(tx *bolt.Tx) error {
				if k, _ := tx.Cursor().First(); k != nil {
					return ErrInvalid
				}
				for _, name := range alertBuckets {
					if _, err := tx.CreateBucket([]byte(name)); err != nil {
						return err
					}
				}
				meta := tx.Bucket([]byte("meta"))
				if err := meta.Put([]byte("format"), []byte(alertFormat)); err != nil {
					return err
				}
				return meta.Put([]byte("schema_version"), binary.BigEndian.AppendUint64(nil, 2))
			})
		}
		if err == nil {
			var version uint64
			err = db.View(func(tx *bolt.Tx) error {
				var err error
				version, err = alertSchemaVersion(tx)
				return err
			})
			if err == nil && version == 1 {
				err = db.Update(migrateAlertBbolt)
			}
			if err == nil {
				err = db.View(checkAlertSchema)
			}
		}
		if err == nil {
			db.NoFreelistSync = false
			return &BboltStore{db: db}, nil
		}
		_ = db.Close()
	}
	if created != nil {
		if current, statErr := os.Stat(path); statErr == nil && os.SameFile(created, current) {
			_ = os.Remove(path)
		}
	}
	return nil, err
}

func alertSchemaVersion(tx *bolt.Tx) (uint64, error) {
	for _, name := range alertBuckets[:4] {
		if tx.Bucket([]byte(name)) == nil {
			return 0, ErrInvalid
		}
	}
	meta := tx.Bucket([]byte("meta"))
	v := meta.Get([]byte("schema_version"))
	if string(meta.Get([]byte("format"))) != alertFormat || len(v) != 8 {
		return 0, ErrInvalid
	}
	version := binary.BigEndian.Uint64(v)
	if version != 1 && version != 2 {
		return 0, ErrInvalid
	}
	return version, nil
}
func checkAlertSchema(tx *bolt.Tx) error {
	version, err := alertSchemaVersion(tx)
	if err != nil || version != 2 || tx.Bucket([]byte("incidents_by_time")) == nil {
		return ErrInvalid
	}
	return nil
}

// A failed validation rolls back both the index and version in this one Update.
func migrateAlertBbolt(tx *bolt.Tx) error {
	if tx.Bucket([]byte("incidents_by_time")) != nil {
		return ErrInvalid
	}
	index, err := tx.CreateBucket([]byte("incidents_by_time"))
	if err != nil {
		return err
	}
	incidents, active, outbox := tx.Bucket([]byte("incidents")), tx.Bucket([]byte("active")), tx.Bucket([]byte("outbox"))
	read := func(id string) (*Task, error) { return boltTask(outbox, id) }
	if err := incidents.ForEach(func(k, v []byte) error {
		i, err := decodeIncident(v)
		if err != nil || i == nil || i.ID != string(k) || validateLegacyIncident(*i) != nil {
			return ErrInvalid
		}
		if err := validateLegacyLinks(*i, string(active.Get([]byte(i.EndpointID))), read); err != nil {
			return err
		}
		key, err := historyKey(*i)
		if err != nil {
			return err
		}
		return index.Put([]byte(key), k)
	}); err != nil {
		return err
	}
	if err := active.ForEach(func(k, v []byte) error {
		i, err := decodeIncident(incidents.Get(v))
		if err != nil || i == nil || i.ID != string(v) || i.EndpointID != string(k) || !i.RecoveredAt.IsZero() {
			return ErrInvalid
		}
		return nil
	}); err != nil {
		return err
	}
	if err := outbox.ForEach(func(k, v []byte) error {
		t, err := decodeTask(v)
		if err != nil || t.EventID != string(k) || t.Type == Reminder {
			return ErrInvalid
		}
		i, err := decodeIncident(incidents.Get([]byte(t.IncidentID)))
		if err != nil {
			return err
		}
		return validateStoredTask(t, i)
	}); err != nil {
		return err
	}
	return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), binary.BigEndian.AppendUint64(nil, 2))
}

func (s *BboltStore) transaction(ctx context.Context, write bool, fn func(*bolt.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	run := func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkAlertSchema(tx); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return ctx.Err()
	}
	if write {
		return s.db.Update(run)
	}
	return s.db.View(run)
}

// Close releases the exclusive lock once; subsequent operations return errors.
func (s *BboltStore) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

// Get reads an incident by stable ID for commit reconciliation, including closed ones.
func (s *BboltStore) Get(ctx context.Context, id string) (result *Incident, err error) {
	err = s.transaction(ctx, false, func(tx *bolt.Tx) error {
		result, err = decodeIncident(tx.Bucket([]byte("incidents")).Get([]byte(id)))
		if err == nil && result != nil && result.ID != id {
			return ErrInvalid
		}
		return err
	})
	return result, err
}

// Active reads the active index and incident in one read transaction.
func (s *BboltStore) Active(ctx context.Context, endpoint string) (result *Incident, err error) {
	err = s.transaction(ctx, false, func(tx *bolt.Tx) error {
		id := tx.Bucket([]byte("active")).Get([]byte(endpoint))
		if id == nil {
			return nil
		}
		result, err = decodeIncident(tx.Bucket([]byte("incidents")).Get(id))
		if err != nil {
			return err
		}
		if result == nil || result.ID != string(id) || result.EndpointID != endpoint || !result.RecoveredAt.IsZero() {
			return ErrInvalid
		}
		return nil
	})
	return result, err
}

// Apply commits an incident, history index, and derived tasks atomically.
func (s *BboltStore) Apply(ctx context.Context, change Transition) (result Incident, err error) {
	err = s.transaction(ctx, true, func(tx *bolt.Tx) error {
		incidents, active, outbox := tx.Bucket([]byte("incidents")), tx.Bucket([]byte("active")), tx.Bucket([]byte("outbox"))
		existing, err := decodeIncident(incidents.Get([]byte(change.Incident.ID)))
		if err != nil {
			return err
		}
		var tasks []Task
		result, tasks, err = prepare(existing, string(active.Get([]byte(change.Incident.EndpointID))), change)
		if err != nil || tasks == nil {
			return err
		}
		tasks, err = transitionTasks(result, tasks, func(id string) (*Task, error) { return boltTask(outbox, id) })
		if err != nil {
			return err
		}
		if err := boltPutIncident(tx, result); err != nil {
			return err
		}
		if change.Type == Down {
			err = active.Put([]byte(result.EndpointID), []byte(result.ID))
		} else {
			err = active.Delete([]byte(result.EndpointID))
		}
		if err != nil {
			return err
		}
		return boltPutTasks(outbox, tasks)
	})
	return result, err
}

// ApplyReminder atomically advances the frozen counter and creates its tasks.
func (s *BboltStore) ApplyReminder(ctx context.Context, c ReminderChange) (result Incident, err error) {
	err = s.transaction(ctx, true, func(tx *bolt.Tx) error {
		i, err := decodeIncident(tx.Bucket([]byte("incidents")).Get([]byte(c.IncidentID)))
		if err != nil {
			return err
		}
		if i == nil {
			return ErrNotFound
		}
		outbox := tx.Bucket([]byte("outbox"))
		read := func(id string) (*Task, error) { return boltTask(outbox, id) }
		var tasks []Task
		result, tasks, err = prepareReminderChange(i, string(tx.Bucket([]byte("active")).Get([]byte(i.EndpointID))), c, read)
		if err != nil || tasks == nil {
			return err
		}
		tasks, err = transitionTasks(result, tasks, read)
		if err != nil {
			return err
		}
		if err := boltPutIncident(tx, result); err != nil {
			return err
		}
		return boltPutTasks(outbox, tasks)
	})
	return result, err
}

func boltPutIncident(tx *bolt.Tx, i Incident) error {
	key, err := historyKey(i)
	if err != nil {
		return err
	}
	// Runtime writes must not silently repair a damaged or orphaned index.
	existing := tx.Bucket([]byte("incidents")).Get([]byte(i.ID)) != nil
	indexed := tx.Bucket([]byte("incidents_by_time")).Get([]byte(key))
	if existing && string(indexed) != i.ID || !existing && indexed != nil {
		return ErrInvalid
	}
	data, err := json.Marshal(i)
	if err != nil {
		return err
	}
	if err := tx.Bucket([]byte("incidents")).Put([]byte(i.ID), data); err != nil {
		return err
	}
	return tx.Bucket([]byte("incidents_by_time")).Put([]byte(key), []byte(i.ID))
}
func boltPutTasks(b *bolt.Bucket, tasks []Task) error {
	for _, t := range tasks {
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if err = b.Put([]byte(t.EventID), data); err != nil {
			return err
		}
	}
	return nil
}

var _ DeliveryStore = (*BboltStore)(nil)

func boltTask(bucket *bolt.Bucket, id string) (*Task, error) {
	data := bucket.Get([]byte(id))
	if data == nil {
		return nil, nil
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
func (s *BboltStore) GetTask(ctx context.Context, id string) (result *Task, err error) {
	err = s.transaction(ctx, false, func(tx *bolt.Tx) error {
		result, err = boltTask(tx.Bucket([]byte("outbox")), id)
		return err
	})
	return result, err
}

// QueryDue evaluates at most Limit records in key order. Its cursor names the
// last scanned key, including tasks that were not due or dependency-ready.
func (s *BboltStore) QueryDue(ctx context.Context, q DueQuery) (TaskPage, error) {
	if err := validateQuery(q); err != nil {
		return TaskPage{}, err
	}
	if q.Cursor != "" {
		if b, err := hex.DecodeString(q.Cursor); err != nil || len(b) != 32 {
			return TaskPage{}, ErrInvalid
		}
	}
	var page TaskPage
	err := s.transaction(ctx, false, func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("outbox"))
		cursor := bucket.Cursor()
		key, value := cursor.First()
		if q.Cursor != "" {
			key, value = cursor.Seek([]byte(q.Cursor))
			if string(key) == q.Cursor {
				key, value = cursor.Next()
			}
		}
		for n := 0; key != nil && n < q.Limit; n++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			task, err := decodeTask(value)
			if err != nil {
				return err
			}
			if task.EventID != string(key) {
				return ErrInvalid
			}
			ready, _, err := boltReadDelivery(tx, task, q.Now)
			if err != nil {
				return err
			}
			if ready {
				page.Tasks = append(page.Tasks, task)
			}
			page.NextCursor = string(key)
			key, value = cursor.Next()
		}
		if key == nil {
			page.NextCursor = ""
		}
		return nil
	})
	if err != nil {
		return TaskPage{}, err
	}
	return page, nil
}

// ReadDelivery rechecks a candidate and its dependency chain in one read snapshot.
func (s *BboltStore) ReadDelivery(ctx context.Context, id string, now time.Time) (task *Task, i *Incident, err error) {
	err = s.transaction(ctx, false, func(tx *bolt.Tx) error {
		t, e := boltTask(tx.Bucket([]byte("outbox")), id)
		if e != nil {
			return e
		}
		if t == nil {
			return ErrNotFound
		}
		ready, incident, e := boltReadDelivery(tx, *t, now)
		if e == nil && ready {
			task, i = t, incident
		}
		return e
	})
	return
}
func boltReadDelivery(tx *bolt.Tx, t Task, now time.Time) (bool, *Incident, error) {
	i, err := decodeIncident(tx.Bucket([]byte("incidents")).Get([]byte(t.IncidentID)))
	if err != nil {
		return false, nil, err
	}
	if i == nil {
		return false, nil, ErrInvalid
	}
	down, prev, err := deliveryDependencies(t, *i, func(id string) (*Task, error) { return boltTask(tx.Bucket([]byte("outbox")), id) })
	if err != nil {
		return false, nil, err
	}
	ready, err := DeliveryReady(t, *i, down, prev, now)
	return ready, i, err
}

// RecordOutcome conditionally records a send and all resulting dependency skips.
func (s *BboltStore) RecordOutcome(ctx context.Context, o Outcome) (result Task, err error) {
	if _, _, err := normalizeOutcome(o); err != nil {
		return Task{}, err
	}
	err = s.transaction(ctx, true, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("outbox"))
		read := func(id string) (*Task, error) { return boltTask(b, id) }
		t, err := read(o.EventID)
		if err != nil {
			return err
		}
		if t == nil {
			return ErrNotFound
		}
		i, err := decodeIncident(tx.Bucket([]byte("incidents")).Get([]byte(t.IncidentID)))
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
		return boltPutTasks(b, tasks)
	})
	return
}

// ListIncidents uses the time index directly, with an exclusive reverse cursor.
func (s *BboltStore) ListIncidents(ctx context.Context, q HistoryQuery) (page IncidentPage, err error) {
	limit, anchor, err := historyQuery(q)
	if err != nil {
		return page, err
	}
	err = s.transaction(ctx, false, func(tx *bolt.Tx) error {
		index := tx.Bucket([]byte("incidents_by_time"))
		incidents := tx.Bucket([]byte("incidents"))
		lastKey := ""
		c := index.Cursor()
		k, v := c.Last()
		if anchor != "" {
			k, v = c.Seek([]byte(anchor))
			if k == nil {
				k, v = c.Last()
			} else {
				k, v = c.Prev()
			}
		}
		for k != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			i, e := decodeIncident(incidents.Get(v))
			if e != nil {
				return e
			}
			if e = indexedIncident(string(k), i); e != nil || i.ID != string(v) {
				return ErrInvalid
			}
			if len(page.Incidents) == limit {
				page.NextCursor = historyCursor(lastKey)
				break
			}
			page.Incidents = append(page.Incidents, *i)
			lastKey = string(k)
			k, v = c.Prev()
		}
		return nil
	})
	if err != nil {
		return IncidentPage{}, err
	}
	return
}

// ReadIncident reads the incident and derived task page in one bbolt snapshot.
func (s *BboltStore) ReadIncident(ctx context.Context, q DetailQuery) (page IncidentDetail, err error) {
	if err = q.Validate(); err != nil {
		return page, err
	}
	err = s.transaction(ctx, false, func(tx *bolt.Tx) error {
		i, e := decodeIncident(tx.Bucket([]byte("incidents")).Get([]byte(q.IncidentID)))
		if e != nil {
			return e
		}
		page, e = readIncidentDetail(i, q, func(id string) (*Task, error) { return boltTask(tx.Bucket([]byte("outbox")), id) })
		return e
	})
	if err != nil {
		return IncidentDetail{}, err
	}
	return
}

// Outbox returns all tasks ordered by EventID; it never claims or delivers them.
func (s *BboltStore) Outbox(ctx context.Context) (tasks []Task, err error) {
	err = s.transaction(ctx, false, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("outbox")).ForEach(func(key, value []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			task, err := decodeTask(value)
			if err != nil {
				return err
			}
			if task.EventID != string(key) {
				return ErrInvalid
			}
			tasks = append(tasks, task)
			return nil
		})
	})
	return tasks, err
}
