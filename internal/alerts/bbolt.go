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

var alertBuckets = []string{"meta", "incidents", "active", "outbox"}

// BboltStore owns a separate, exclusively locked alert database. Close it after
// stopping callers. It never opens or migrates the monitoring history database.
type BboltStore struct {
	db        *bolt.DB
	closeOnce sync.Once
	closeErr  error
}

// OpenBbolt initializes only a file created by this call. Existing empty,
// foreign, damaged, or unsupported databases are refused without repair.
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
				return meta.Put([]byte("schema_version"), binary.BigEndian.AppendUint64(nil, 1))
			})
		}
		if err == nil {
			err = db.View(checkAlertSchema)
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

func checkAlertSchema(tx *bolt.Tx) error {
	for _, name := range alertBuckets {
		if tx.Bucket([]byte(name)) == nil {
			return ErrInvalid
		}
	}
	meta := tx.Bucket([]byte("meta"))
	version := meta.Get([]byte("schema_version"))
	if string(meta.Get([]byte("format"))) != alertFormat || len(version) != 8 || binary.BigEndian.Uint64(version) != 1 {
		return ErrInvalid
	}
	return nil
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

// Apply commits an incident, active index, and all channel tasks in one transaction.
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
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := incidents.Put([]byte(result.ID), data); err != nil {
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
		for _, task := range tasks {
			if task.Type == Recovered {
				down, err := boltTask(outbox, eventID(task.IncidentID, Down, task.ChannelID))
				if err != nil {
					return err
				}
				task, err = skipRecovery(task, down)
				if err != nil {
					return err
				}
			}
			data, err := json.Marshal(task)
			if err != nil {
				return err
			}
			if outbox.Get([]byte(task.EventID)) != nil {
				return ErrConflict
			}
			if err := outbox.Put([]byte(task.EventID), data); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
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
			var down *Task
			if task.Type == Recovered && task.Status == TaskPending {
				down, err = boltTask(bucket, eventID(task.IncidentID, Down, task.ChannelID))
				if err != nil {
					return err
				}
				if down == nil {
					return ErrInvalid
				}
			}
			if deliverable(task, down, q.Now) {
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

// RecordOutcome atomically persists a conditional attempt and any dependent
// recovery skip. Errors may leave commit status unknown; GetTask permits read-back.
func (s *BboltStore) RecordOutcome(ctx context.Context, o Outcome) (result Task, err error) {
	if _, _, err := normalizeOutcome(o); err != nil {
		return Task{}, err
	}
	err = s.transaction(ctx, true, func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("outbox"))
		task, err := boltTask(bucket, o.EventID)
		if err != nil {
			return err
		}
		if task == nil {
			return ErrNotFound
		}
		var down *Task
		if task.Type == Recovered {
			down, err = boltTask(bucket, eventID(task.IncidentID, Down, task.ChannelID))
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
		if result.Type == Down && (result.Status == TaskFailed || result.Status == TaskSkipped) {
			recovery, err := boltTask(bucket, eventID(result.IncidentID, Recovered, result.ChannelID))
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
				if err := bucket.Put([]byte(skipped.EventID), data); err != nil {
					return err
				}
			}
		}
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(result.EventID), data)
	})
	return result, err
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
