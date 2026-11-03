package alerts

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestBboltReopenLockAndClose(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "alerts.db")
	s, err := OpenBbolt(path, time.Second)
	must(t, err)
	_, err = s.Apply(context.Background(), Transition{Down, testIncident("persisted", "api")})
	must(t, err)
	if other, err := OpenBbolt(path, 20*time.Millisecond); err == nil {
		_ = other.Close()
		t.Fatal("exclusive lock bypassed")
	}
	must(t, s.Close())
	must(t, s.Close())
	if _, err = s.Active(context.Background(), "api"); err == nil {
		t.Fatal("closed database accepted query")
	}
	s, err = OpenBbolt(path, time.Second)
	must(t, err)
	defer s.Close()
	e, err := NewEngine(s, nil)
	must(t, err)
	state, err := e.Observe(context.Background(), Observation{EndpointID: "api", CheckedAt: testTime.Add(3 * time.Second), Success: true}, Policy{})
	must(t, err)
	if state != Healthy {
		t.Fatal("persistent firing not restored")
	}
	tasks, err := s.Outbox(context.Background())
	must(t, err)
	if len(tasks) != 4 {
		t.Fatal("recovery after reopen lost channels")
	}
}

func TestBboltRefusesForeignSchema(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"empty", "garbage", "foreign", "missing", "old", "future", "bad-version"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "alerts.db")
			switch kind {
			case "empty":
				must(t, os.WriteFile(path, nil, 0600))
			case "garbage":
				must(t, os.WriteFile(path, []byte("not a database"), 0600))
			default:
				s, err := OpenBbolt(path, time.Second)
				must(t, err)
				must(t, s.db.Update(func(tx *bolt.Tx) error {
					meta := tx.Bucket([]byte("meta"))
					switch kind {
					case "foreign":
						return meta.Put([]byte("format"), []byte("deepfurry-uptime-bbolt"))
					case "missing":
						return tx.DeleteBucket([]byte("outbox"))
					case "old":
						return meta.Put([]byte("schema_version"), binary.BigEndian.AppendUint64(nil, 0))
					case "future":
						return meta.Put([]byte("schema_version"), binary.BigEndian.AppendUint64(nil, 2))
					default:
						return meta.Put([]byte("schema_version"), []byte{1})
					}
				}))
				must(t, s.Close())
			}
			before, err := os.ReadFile(path)
			must(t, err)
			if s, err := OpenBbolt(path, time.Second); err == nil {
				_ = s.Close()
				t.Fatal("adopted invalid database")
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid database was changed")
			}
		})
	}
}

func TestBboltAtomicRollback(t *testing.T) {
	t.Parallel()
	s := testBbolt(t)
	ctx := context.Background()
	for _, kind := range []EventType{Down, Recovered} {
		i := testIncident(string(kind), string(kind))
		if kind == Recovered {
			_, err := s.Apply(ctx, Transition{Down, i})
			must(t, err)
			i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
		}
		var existing *Incident
		active := ""
		if kind == Recovered {
			copy := i
			copy.RecoveredAt = time.Time{}
			existing = &copy
			active = i.ID
		}
		_, tasks, err := prepare(existing, active, Transition{kind, i})
		must(t, err)
		// Force a failure after the incident/index writes have been queued.
		must(t, s.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("outbox")).Put([]byte(tasks[0].EventID), []byte("collision"))
		}))
		if _, err = s.Apply(ctx, Transition{kind, i}); err == nil {
			t.Fatal("missing transaction failure")
		}
		stored, err := s.Get(ctx, i.ID)
		must(t, err)
		if kind == Down && stored != nil {
			t.Fatal("partial incident created")
		}
		if kind == Recovered && (stored == nil || !stored.RecoveredAt.IsZero()) {
			t.Fatal("partial incident recovery")
		}
		must(t, s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("outbox")).Delete([]byte(tasks[0].EventID)) }))
		_, err = s.Apply(ctx, Transition{kind, i})
		must(t, err)
	}
	tasks, err := s.Outbox(ctx)
	must(t, err)
	if len(tasks) != 6 {
		t.Fatal("partial or duplicate outbox")
	}
}
