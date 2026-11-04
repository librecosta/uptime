package alerts

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
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

func TestV1RefusesReminderPersistence(t *testing.T) {
	s := testBbolt(t)
	i := reminderFixture()
	if _, err := s.Apply(t.Context(), Transition{Type: Down, Incident: i}); !errors.Is(err, ErrInvalid) {
		t.Fatal("v1 accepted reminder snapshot")
	}
	i, tasks, err := PrepareReminder(i, Observation{EndpointID: i.EndpointID, CheckedAt: i.NextReminderAt})
	must(t, err)
	if _, err := s.Apply(t.Context(), Transition{Type: Reminder, Incident: i}); !errors.Is(err, ErrInvalid) {
		t.Fatal("v1 accepted reminder transition")
	}
	box, err := s.Outbox(t.Context())
	must(t, err)
	if len(box) != 0 {
		t.Fatal("rejected reminder mutated outbox")
	}
	data, err := json.Marshal(tasks[0])
	must(t, err)
	if _, err := decodeTask(data); !errors.Is(err, ErrInvalid) {
		t.Fatal("v1 decoded future reminder task")
	}
	data, err = json.Marshal(i)
	must(t, err)
	if _, err := decodeIncident(data); !errors.Is(err, ErrInvalid) {
		t.Fatal("v1 decoded future incident snapshot")
	}
	legacy := testIncident("legacy", "legacy")
	data, err = json.Marshal(legacy)
	must(t, err)
	if bytes.Contains(data, []byte("reminder")) {
		t.Fatal("legacy JSON gained reminder fields")
	}
	_, err = s.Apply(t.Context(), Transition{Type: Down, Incident: legacy})
	must(t, err)
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

func TestBboltDeliveryLegacyAndReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "alerts.db")
	s, err := OpenBbolt(path, time.Second)
	must(t, err)
	defer func() { must(t, s.Close()) }()
	i := testIncident("legacy-delivery", "api")
	_, err = s.Apply(ctx, Transition{Down, i})
	must(t, err)
	legacy := readTask(t, s, i, Down, "a")
	// Frozen B1 EventID for legacy-delivery, DOWN, channel a.
	if legacy.EventID != "ab129dc11610dff77fcb47c6020e4c0705357ffab3ad29c3af90087c8e8d27df" {
		t.Fatal("legacy EventID identity changed")
	}
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("outbox")).Put([]byte(legacy.EventID), legacyTaskJSON(t, legacy))
	}))
	must(t, s.Close())
	s, err = OpenBbolt(path, time.Second)
	must(t, err)
	old := readTask(t, s, i, Down, "a")
	if old.Status != TaskPending || old.Attempts != 0 || len(old.AttemptReceipts) != 0 {
		t.Fatal("legacy delivery defaults changed")
	}
	if len(allDue(t, s, i.ConfirmedAt, 1)) != 2 {
		t.Fatal("legacy tasks not initially due")
	}
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	_, err = s.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	first := deliveryOutcome(old, TaskPending, i.ConfirmedAt)
	pending, err := s.RecordOutcome(ctx, first)
	must(t, err)
	must(t, s.Close())
	if _, err := s.GetTask(ctx, old.EventID); err == nil {
		t.Fatal("closed task query accepted")
	}
	if _, err := s.QueryDue(ctx, DueQuery{Now: testTime, Limit: 1}); err == nil {
		t.Fatal("closed due query accepted")
	}
	if _, err := s.RecordOutcome(ctx, first); err == nil {
		t.Fatal("closed task update accepted")
	}
	s, err = OpenBbolt(path, time.Second)
	must(t, err)
	stored := readTask(t, s, i, Down, "a")
	matched, err := stored.HasOutcome(first)
	must(t, err)
	if !matched || stored.NextAttemptAt != pending.NextAttemptAt {
		t.Fatal("delivery state lost after reopen")
	}
	second := deliveryOutcome(stored, TaskSent, stored.NextAttemptAt)
	_, err = s.RecordOutcome(ctx, second)
	must(t, err)
	_, err = s.RecordOutcome(ctx, first)
	must(t, err)
	incident, err := s.Get(ctx, i.ID)
	must(t, err)
	if incident == nil || !sameIncident(*incident, i) || incident.RecoveredAt != i.RecoveredAt {
		t.Fatal("delivery updates damaged legacy incident")
	}
	must(t, s.db.View(checkAlertSchema)) // Same identity, buckets, and Schema v1.
}

func TestBboltDeliveryAtomicRollback(t *testing.T) {
	t.Parallel()
	s := testBbolt(t)
	ctx := context.Background()
	i := testIncident("outcome-rollback", "api")
	_, err := s.Apply(ctx, Transition{Down, i})
	must(t, err)
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	_, err = s.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	down := readTask(t, s, i, Down, "a")
	recovery := readTask(t, s, i, Recovered, "a")
	must(t, s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("outbox")).Put([]byte(recovery.EventID), []byte("corrupt SECRET data"))
	}))
	if _, err := s.RecordOutcome(ctx, deliveryOutcome(down, TaskFailed, i.RecoveredAt)); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid dependency did not abort outcome")
	}
	unchanged := readTask(t, s, i, Down, "a")
	if unchanged.Status != TaskPending || unchanged.Attempts != 0 {
		t.Fatal("partial failed outcome")
	}
	data, err := json.Marshal(recovery)
	must(t, err)
	must(t, s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("outbox")).Put([]byte(recovery.EventID), data) }))
	_, err = s.RecordOutcome(ctx, deliveryOutcome(down, TaskFailed, i.RecoveredAt))
	must(t, err)
	// A corrupt DOWN also aborts incident recovery and creation of its tasks.
	other := testIncident("recovery-rollback", "other")
	_, err = s.Apply(ctx, Transition{Down, other})
	must(t, err)
	id := eventID(other.ID, Down, "a")
	must(t, s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("outbox")).Put([]byte(id), []byte("corrupt")) }))
	other.RecoveredAt = other.ConfirmedAt.Add(time.Second)
	if _, err := s.Apply(ctx, Transition{Recovered, other}); !errors.Is(err, ErrInvalid) {
		t.Fatal("corrupt DOWN accepted")
	}
	incident, err := s.Get(ctx, other.ID)
	must(t, err)
	if !incident.RecoveredAt.IsZero() {
		t.Fatal("partial incident recovery")
	}
	if task, err := s.GetTask(ctx, eventID(other.ID, Recovered, "b")); err != nil || task != nil {
		t.Fatal("partial recovery tasks")
	}
}
