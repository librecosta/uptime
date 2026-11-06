package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testTime = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testBbolt(t *testing.T) *BboltStore {
	t.Helper()
	s, err := OpenBbolt(filepath.Join(t.TempDir(), "alerts.db"), time.Second)
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	return s
}

func testIncident(id, endpoint string) Incident {
	return Incident{ID: id, EndpointID: endpoint, FirstFailureAt: testTime, ConfirmedAt: testTime.Add(2 * time.Second), ChannelIDs: []string{"a", "b"}}
}

func TestMachine(t *testing.T) {
	t.Parallel()
	for threshold := 1; threshold <= 10; threshold++ {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			m := machine{}
			for n := 1; n <= threshold; n++ {
				var event EventType
				m, event = advance(m, Observation{CheckedAt: testTime.Add(time.Duration(n) * time.Second)}, threshold, 1)
				if n < threshold && (m.state != Pending || event != "") {
					t.Fatal("premature DOWN")
				}
				if n == threshold && (m.state != Firing || event != Down) {
					t.Fatal("missing DOWN")
				}
			}
			m, event := advance(m, Observation{CheckedAt: testTime.Add(20 * time.Second)}, threshold, 1)
			if m.state != Firing || event != "" {
				t.Fatal("repeated failure produced transition")
			}
			m, event = advance(m, Observation{CheckedAt: testTime.Add(21 * time.Second), Gap: true, Success: true}, threshold, 1)
			if m.state != Firing || event != "" {
				t.Fatal("gap closed an incident")
			}
			m, event = advance(m, Observation{CheckedAt: testTime.Add(22 * time.Second), Success: true}, threshold, 1)
			if m.state != Healthy || event != Recovered {
				t.Fatal("missing recovery")
			}
			_, event = advance(m, Observation{CheckedAt: testTime.Add(23 * time.Second), Success: true}, threshold, 1)
			if event != "" {
				t.Fatal("duplicate recovery")
			}
		})
	}
	m, _ := advance(machine{}, Observation{CheckedAt: testTime}, 3, 1)
	for _, o := range []Observation{{CheckedAt: testTime.Add(time.Second), Success: true}, {CheckedAt: testTime.Add(time.Second), Gap: true}} {
		next, event := advance(m, o, 3, 1)
		if next.state != Healthy || next.failures != 0 || event != "" {
			t.Fatal("pending not reset")
		}
		next, event = advance(next, Observation{CheckedAt: testTime.Add(2 * time.Second)}, 3, 1)
		if next.failures != 1 || event != "" {
			t.Fatal("reset sequence reused failures")
		}
	}
	unchanged, event := advance(m, Observation{CheckedAt: testTime.Add(-time.Second), Success: true}, 3, 1)
	if unchanged != m || event != "" {
		t.Fatal("out-of-order event consumed")
	}
	for range 2 {
		m, _ = advance(m, Observation{CheckedAt: testTime}, 3, 1)
	}
	if m.state != Firing {
		t.Fatal("equal completion times lost actual probes")
	}
	m, event = advance(m, Observation{CheckedAt: testTime, Success: true}, 3, 1)
	if m.state != Healthy || event != Recovered {
		t.Fatal("equal-time real success did not recover")
	}
}

func TestEngineRestartAndChannels(t *testing.T) {
	t.Parallel()
	s := testBbolt(t)
	now := testTime
	newEngine := func() *Engine { e, err := NewEngine(s, func() time.Time { return now }); must(t, err); return e }
	e := newEngine()
	observe := func(success, gap bool, channels []string) State {
		t.Helper()
		now = now.Add(time.Second)
		state, err := e.Observe(context.Background(), Observation{EndpointID: "api", Success: success, Gap: gap}, Policy{ChannelIDs: channels})
		must(t, err)
		return state
	}
	if observe(false, false, nil) != Pending || observe(false, false, nil) != Pending {
		t.Fatal("default threshold")
	}
	e = newEngine() // PENDING is lost.
	if observe(false, false, nil) != Pending {
		t.Fatal("PENDING survived restart")
	}
	if observe(false, true, nil) != Healthy {
		t.Fatal("gap did not reset pending")
	}
	observe(false, false, nil)
	observe(false, false, nil)
	if observe(false, false, []string{"b", "a", "a"}) != Firing {
		t.Fatal("missing firing")
	}
	active, err := s.Active(context.Background(), "api")
	must(t, err)
	if active == nil || !slices.Equal(active.ChannelIDs, []string{"a", "b"}) {
		t.Fatal("snapshot not saved")
	}
	e = newEngine() // FIRING is restored, including its channels.
	if observe(false, true, []string{"new"}) != Firing || observe(false, false, []string{"new"}) != Firing {
		t.Fatal("FIRING did not survive restart/gap")
	}
	if observe(true, false, []string{"new"}) != Healthy {
		t.Fatal("missing recovered")
	}
	observe(true, false, nil)
	tasks, err := s.Outbox(context.Background())
	must(t, err)
	if len(tasks) != 4 {
		t.Fatalf("tasks=%d, want 2 DOWN + 2 RECOVERED", len(tasks))
	}
	for _, task := range tasks {
		if task.ChannelID == "new" || task.Status != "PENDING" || task.IncidentID != active.ID {
			t.Fatal("snapshot or outbox changed")
		}
	}
	for _, threshold := range []int{-1, 11} {
		_, err := e.Observe(context.Background(), Observation{EndpointID: "api"}, Policy{FailureThreshold: threshold})
		if !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid threshold accepted")
		}
	}
}

type faultyStore struct {
	Store
	before, after, read, active bool
	ids                         []string
}

var errInjected = errors.New("injected storage failure")

func (s *faultyStore) Apply(ctx context.Context, tr Transition) (Incident, error) {
	s.ids = append(s.ids, tr.Incident.ID)
	if s.before {
		return Incident{}, errInjected
	}
	i, err := s.Store.Apply(ctx, tr)
	if err == nil && s.after {
		return Incident{}, errInjected
	}
	return i, err
}
func (s *faultyStore) Get(ctx context.Context, id string) (*Incident, error) {
	if s.read {
		return nil, errInjected
	}
	return s.Store.Get(ctx, id)
}
func (s *faultyStore) Active(ctx context.Context, id string) (*Incident, error) {
	if s.active {
		return nil, errInjected
	}
	return s.Store.Active(ctx, id)
}

func TestEngineCommitReconciliation(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			s := &faultyStore{Store: testBbolt(t), before: !committed, after: committed, read: true}
			e, err := NewEngine(s, nil)
			must(t, err)
			ctx := context.Background()
			o := Observation{EndpointID: "api", CheckedAt: testTime}
			policy := Policy{FailureThreshold: 1, ChannelIDs: []string{"a", "b"}}
			_, err = e.Observe(ctx, o, policy)
			if !errors.Is(err, errInjected) {
				t.Fatal("lost uncertain write error")
			}
			tasks, err := s.Store.Outbox(ctx)
			must(t, err)
			want := 0
			if committed {
				want = 2
			}
			if len(tasks) != want {
				t.Fatal("partial DOWN")
			}
			s.before, s.after, s.read = false, false, false
			state, err := e.Observe(ctx, o, policy)
			must(t, err)
			if state != Firing || len(s.ids) != 2 || s.ids[0] != s.ids[1] {
				t.Fatal("reconciliation changed transition identity")
			}
			// A lost recovery response is verified by reading the committed record.
			s.after = true
			o.CheckedAt = testTime.Add(time.Second)
			o.Success = true
			state, err = e.Observe(ctx, o, Policy{ChannelIDs: []string{"other"}})
			must(t, err)
			if state != Healthy {
				t.Fatal("uncertain recovery not reconciled")
			}
			tasks, err = s.Outbox(ctx)
			must(t, err)
			if len(tasks) != 4 {
				t.Fatal("duplicate/lost tasks")
			}
		})
	}
}

func TestEngineReadFailureAndCancellation(t *testing.T) {
	t.Parallel()
	s := &faultyStore{Store: testBbolt(t)}
	e, err := NewEngine(s, nil)
	must(t, err)
	o := Observation{EndpointID: "api", CheckedAt: testTime}
	_, err = e.Observe(context.Background(), o, Policy{})
	must(t, err)
	s.active = true
	o.CheckedAt = o.CheckedAt.Add(time.Second)
	o.Success = true
	_, err = e.Observe(context.Background(), o, Policy{})
	if !errors.Is(err, errInjected) {
		t.Fatal(err)
	}
	s.active = false
	o.Success = false
	o.CheckedAt = o.CheckedAt.Add(time.Second)
	state, err := e.Observe(context.Background(), o, Policy{})
	must(t, err)
	if state != Pending || e.states["api"].failures != 1 {
		t.Fatal("read failure retained unconfirmed sequence")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = e.Observe(ctx, o, Policy{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	tasks, err := s.Outbox(context.Background())
	must(t, err)
	if len(tasks) != 0 {
		t.Fatal("operational failure emitted alerts")
	}
}

// exerciseStore runs identical transition/atomicity rules against real backends.
func exerciseStore(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	i := testIncident("stable-id", "api")
	_, err := s.Apply(ctx, Transition{Down, i})
	must(t, err)
	_, err = s.Apply(ctx, Transition{Down, i})
	must(t, err)
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	_, err = s.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	_, err = s.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	old := i
	old.RecoveredAt = time.Time{}
	_, err = s.Apply(ctx, Transition{Down, old})
	must(t, err) // Never reopen an old incident.
	active, err := s.Active(ctx, "api")
	must(t, err)
	if active != nil {
		t.Fatal("reopened incident")
	}
	tasks, err := s.Outbox(ctx)
	must(t, err)
	if len(tasks) != 4 {
		t.Fatal("incorrect outbox deduplication")
	}
	for _, task := range tasks {
		if task.Status != "PENDING" {
			t.Fatal("task was sent")
		}
	}
	// Competing IDs for one endpoint must yield exactly one active incident.
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for n := range 12 {
		wg.Go(func() {
			_, err := s.Apply(ctx, Transition{Down, testIncident(fmt.Sprintf("race-%d", n), "race")})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("active incident winners=%d", wins)
	}
	active, err = s.Active(ctx, "race")
	must(t, err)
	if active == nil {
		t.Fatal("missing active incident")
	}
	active.RecoveredAt = active.ConfirmedAt.Add(time.Second)
	for range 12 {
		wg.Go(func() {
			_, err := s.Apply(ctx, Transition{Recovered, *active})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	tasks, err = s.Outbox(ctx)
	must(t, err)
	if len(tasks) != 8 {
		t.Fatalf("concurrent tasks=%d", len(tasks))
	}
}

func TestBboltTransitions(t *testing.T) { t.Parallel(); exerciseStore(t, testBbolt(t)) }

func TestEngineConcurrent(t *testing.T) {
	t.Parallel()
	s := testBbolt(t)
	e, err := NewEngine(s, nil)
	must(t, err)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := e.Observe(context.Background(), Observation{EndpointID: "api", CheckedAt: testTime}, Policy{FailureThreshold: 1, ChannelIDs: []string{"a"}})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	tasks, err := s.Outbox(context.Background())
	must(t, err)
	if len(tasks) != 1 {
		t.Fatal("concurrent observations duplicated DOWN")
	}
}

func TestRetryTime(t *testing.T) {
	want := []time.Duration{10, 20, 40, 80, 160, 320, 640, 1280, 1800}
	for n, seconds := range want {
		next, err := RetryTime(n+1, testTime, time.Time{})
		must(t, err)
		if !next.Equal(testTime.Add(seconds * time.Second)) {
			t.Fatalf("attempt %d: %v", n+1, next)
		}
	}
	next, err := RetryTime(MaxAttempts, testTime, testTime.Add(time.Hour))
	must(t, err)
	if !next.IsZero() {
		t.Fatal("exhausted retry scheduled again")
	}
	for _, hint := range []time.Time{testTime.Add(time.Hour), testTime.Add(time.Second)} {
		next, err := RetryTime(1, testTime, hint)
		must(t, err)
		if next != maxTime(testTime.Add(10*time.Second), hint) {
			t.Fatal("adapter retry bound lost")
		}
	}
	for _, n := range []int{-1, 0, 11, 1000000} {
		if _, err := RetryTime(n, testTime, time.Time{}); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid count accepted")
		}
	}
	for _, at := range []time.Time{{}, time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)} {
		if _, err := RetryTime(1, at, time.Time{}); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid retry time accepted")
		}
	}
	if _, err := RetryTime(1, testTime, testTime.Add(-time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatal("past retry hint accepted")
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

type deliveryTestStore interface {
	Store
	DeliveryStore
}

func deliveryOutcome(task Task, status TaskStatus, at time.Time) Outcome {
	o := Outcome{EventID: task.EventID, AttemptID: fmt.Sprintf("attempt-%d", task.Attempts+1), ExpectedAttempts: task.Attempts, AttemptedAt: at, Status: status}
	if status != TaskSent {
		o.ErrorCode = ErrorNetwork
	}
	return o
}

func TestRecoveryStability(t *testing.T) {
	for _, threshold := range []int{0, 1, 3, 10} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			store := testBbolt(t)
			engine, err := NewEngine(store, nil)
			must(t, err)
			policy := Policy{FailureThreshold: 1, RecoverySuccesses: threshold, ChannelIDs: []string{"a"}}
			now := testTime
			observe := func(success, gap bool) State {
				now = now.Add(time.Second)
				state, err := engine.Observe(t.Context(), Observation{EndpointID: "api", CheckedAt: now, Success: success, Gap: gap}, policy)
				must(t, err)
				return state
			}
			if observe(false, false) != Firing {
				t.Fatal("failure not confirmed")
			}
			want := max(1, threshold)
			for n := 1; n < want; n++ {
				if observe(true, false) != Firing {
					t.Fatal("recovered before stable successes")
				}
			}
			// A failure, a gap (even with an old time), and restart each discard
			// an unconfirmed recovery; none closes the durable incident.
			for _, reset := range []string{"failure", "gap", "restart"} {
				switch reset {
				case "failure":
					observe(false, false)
				case "gap":
					_, err := engine.Observe(t.Context(), Observation{EndpointID: "api", CheckedAt: testTime, Gap: true}, policy)
					must(t, err)
				case "restart":
					engine, err = NewEngine(store, nil)
					must(t, err)
				}
				for n := 1; n < want; n++ {
					if observe(true, false) != Firing {
						t.Fatal("recovery count survived reset")
					}
				}
			}
			if observe(true, false) != Healthy || observe(true, false) != Healthy {
				t.Fatal("stable recovery/deduplication failed")
			}
			tasks, err := store.Outbox(t.Context())
			must(t, err)
			if len(tasks) != 2 {
				t.Fatal("duplicate incident or recovery")
			}
		})
	}
	for _, v := range []int{-1, 11} {
		e, err := NewEngine(testBbolt(t), nil)
		must(t, err)
		if _, err = e.Observe(t.Context(), Observation{EndpointID: "api", CheckedAt: testTime}, Policy{RecoverySuccesses: v}); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid recovery threshold")
		}
	}
}

func reminderFixture() Incident {
	i := testIncident("reminder", "api")
	i.ReminderPolicy = &ReminderPolicy{Interval: time.Hour, MaxReminders: 3}
	i.NextReminderAt = i.ConfirmedAt.Add(time.Hour)
	return i
}

func TestReminderDecisionsAndIdentity(t *testing.T) {
	i := reminderFixture()
	obs := Observation{EndpointID: i.EndpointID, CheckedAt: i.NextReminderAt, StatusCode: 500}
	for _, o := range []Observation{{EndpointID: i.EndpointID, CheckedAt: obs.CheckedAt, Success: true}, {EndpointID: i.EndpointID, CheckedAt: obs.CheckedAt, Gap: true}, {EndpointID: i.EndpointID, CheckedAt: obs.CheckedAt.Add(-time.Nanosecond)}} {
		got, tasks, err := PrepareReminder(i, o)
		must(t, err)
		if got.ReminderCount != 0 || tasks != nil {
			t.Fatal("non-failure/early reminder generated")
		}
	}
	// Restart/downtime does not catch up an arbitrary number of intervals.
	obs.CheckedAt = obs.CheckedAt.Add(48 * time.Hour)
	for sequence := 1; sequence <= 3; sequence++ {
		next, tasks, err := PrepareReminder(i, obs)
		must(t, err)
		if len(tasks) != len(i.ChannelIDs) || next.ReminderCount != sequence {
			t.Fatal("reminder count/channel snapshot")
		}
		for n, task := range tasks {
			sum := sha256.Sum256([]byte(i.ID + "\x00REMINDER\x00" + strconv.Itoa(sequence) + "\x00" + i.ChannelIDs[n]))
			if task.EventID != hex.EncodeToString(sum[:]) || task.ReminderSequence != sequence || !task.OccurredAt.Equal(obs.CheckedAt) || validateTask(task) != nil {
				t.Fatal("reminder identity/time")
			}
		}
		if sequence < 3 && !next.NextReminderAt.Equal(obs.CheckedAt.Add(time.Hour)) {
			t.Fatal("interval did not start at actual generation")
		}
		if sequence == 3 && !next.NextReminderAt.IsZero() {
			t.Fatal("limit retained a schedule")
		}
		_, again, err := PrepareReminder(next, obs)
		must(t, err)
		if again != nil {
			t.Fatal("repeated observation generated again")
		}
		i = next
		obs.CheckedAt = obs.CheckedAt.Add(2 * time.Hour)
	}
	_, tasks, err := PrepareReminder(i, obs)
	must(t, err)
	if tasks != nil {
		t.Fatal("count limit exceeded")
	}
	legacy := testIncident("legacy", "old")
	data, err := json.Marshal(legacy)
	must(t, err)
	decoded, err := decodeIncident(data)
	must(t, err)
	_, tasks, err = PrepareReminder(*decoded, Observation{EndpointID: legacy.EndpointID, CheckedAt: obs.CheckedAt})
	must(t, err)
	if tasks != nil {
		t.Fatal("legacy incident got reminders")
	}
	for _, sequence := range []int{0, -1} {
		if _, err := ReminderEventID("id", sequence, "a"); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid sequence accepted")
		}
	}
	// Old identities do not acquire a sequence component.
	sum := sha256.Sum256([]byte("id\x00DOWN\x00a"))
	if eventID("id", Down, "a") != hex.EncodeToString(sum[:]) {
		t.Fatal("DOWN identity changed")
	}
}

func TestReminderDependenciesAndCancellation(t *testing.T) {
	i := reminderFixture()
	now := i.NextReminderAt
	i, first, err := PrepareReminder(i, Observation{EndpointID: i.EndpointID, CheckedAt: now})
	must(t, err)
	down := Task{EventID: eventID(i.ID, Down, "a"), IncidentID: i.ID, ChannelID: "a", Type: Down, OccurredAt: i.ConfirmedAt, Status: TaskPending}
	ready, err := DeliveryReady(first[0], i, &down, nil, now)
	must(t, err)
	if ready {
		t.Fatal("reminder overtook DOWN")
	}
	failed, _, err := prepareOutcome(down, nil, deliveryOutcome(down, TaskFailed, now))
	must(t, err)
	ready, err = DeliveryReady(first[0], i, &failed, nil, now)
	must(t, err)
	if ready {
		t.Fatal("failed DOWN permitted reminder")
	}
	down, _, err = prepareOutcome(down, nil, deliveryOutcome(down, TaskSent, now))
	must(t, err)
	retry := deliveryOutcome(first[0], TaskPending, now)
	previous, _, err := PrepareDeliveryOutcome(first[0], i, &down, nil, retry)
	must(t, err)
	// A receipt is task-local, even if corrupt input copied another task's digest.
	wrong := retry
	wrong.EventID = "another-task"
	_, digest, err := normalizeOutcome(wrong)
	must(t, err)
	forged := previous
	forged.AttemptReceipts = []AttemptReceipt{{AttemptID: wrong.AttemptID, Digest: digest}}
	if _, _, err := PrepareDeliveryOutcome(forged, i, &down, nil, wrong); !errors.Is(err, ErrConflict) {
		t.Fatal("cross-task receipt accepted")
	}
	i, second, err := PrepareReminder(i, Observation{EndpointID: i.EndpointID, CheckedAt: i.NextReminderAt})
	must(t, err)
	ready, err = DeliveryReady(second[0], i, &down, &previous, i.NextReminderAt)
	must(t, err)
	if ready {
		t.Fatal("pending predecessor did not block")
	}
	if _, err := DeliveryReady(second[0], i, &down, nil, i.NextReminderAt); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing predecessor allowed")
	}
	previous, _, err = PrepareDeliveryOutcome(previous, i, &down, nil, deliveryOutcome(previous, TaskFailed, previous.NextAttemptAt))
	must(t, err)
	ready, err = DeliveryReady(second[0], i, &down, &previous, i.NextReminderAt)
	must(t, err)
	if !ready {
		t.Fatal("failed predecessor blocked next reminder")
	}
	// Preserve a real pending attempt when recovery cancels the next reminder.
	secondRetry := deliveryOutcome(second[0], TaskPending, second[0].OccurredAt)
	pending, _, err := PrepareDeliveryOutcome(second[0], i, &down, &previous, secondRetry)
	must(t, err)
	i.RecoveredAt = pending.LastAttemptAt.Add(time.Minute)
	i.NextReminderAt = time.Time{}
	canceled, err := CancelReminder(pending, i)
	must(t, err)
	if canceled.Status != TaskSkipped || canceled.Attempts != 1 || canceled.LastAttemptID != pending.LastAttemptID || !reflect.DeepEqual(canceled.AttemptReceipts, pending.AttemptReceipts) || !canceled.LastAttemptAt.Equal(pending.LastAttemptAt) || !canceled.FinishedAt.Equal(i.RecoveredAt) {
		t.Fatal("cancellation invented/lost attempts")
	}
	if matched, err := canceled.HasOutcome(secondRetry); err != nil || !matched {
		t.Fatal("historical receipt lost after cancellation")
	}
	late := deliveryOutcome(pending, TaskSent, i.RecoveredAt.Add(time.Second))
	if reconciled, err := canceled.CanceledByRecovery(i, late); err != nil || !reconciled {
		t.Fatal("late completed send not reconciled")
	}
	if _, _, err := PrepareDeliveryOutcome(canceled, i, &down, &previous, late); !errors.Is(err, ErrConflict) {
		t.Fatal("late send overwrote cancellation")
	}
	recovery := Task{EventID: eventID(i.ID, Recovered, "a"), IncidentID: i.ID, ChannelID: "a", Type: Recovered, OccurredAt: i.RecoveredAt, Status: TaskPending}
	ready, err = DeliveryReady(recovery, i, &down, &pending, i.RecoveredAt)
	must(t, err)
	if ready {
		t.Fatal("recovery overtook pending reminder")
	}
	ready, err = DeliveryReady(recovery, i, &down, &canceled, i.RecoveredAt)
	must(t, err)
	if !ready {
		t.Fatal("canceled reminder blocked recovery")
	}
}

func TestReminderValidation(t *testing.T) {
	i := reminderFixture()
	for _, mutate := range []func(*Incident){func(i *Incident) { i.ReminderPolicy = &ReminderPolicy{Interval: 0, MaxReminders: 3} }, func(i *Incident) { i.ReminderCount = -1 }, func(i *Incident) { i.ReminderCount = 4 }, func(i *Incident) { i.NextReminderAt = time.Time{} }, func(i *Incident) { i.NextReminderAt = i.ConfirmedAt }, func(i *Incident) { i.ReminderPolicy = nil }} {
		bad := i
		mutate(&bad)
		if validateIncident(bad) == nil {
			t.Fatal("invalid reminder snapshot accepted")
		}
	}
	closed := i
	closed.RecoveredAt = i.NextReminderAt
	closed.NextReminderAt = time.Time{}
	_, tasks, err := PrepareReminder(closed, Observation{EndpointID: i.EndpointID, CheckedAt: i.NextReminderAt.Add(time.Hour)})
	must(t, err)
	if tasks != nil {
		t.Fatal("closed incident generated reminder")
	}
	late := i
	late.NextReminderAt = time.Date(9999, 12, 31, 23, 30, 0, 0, time.UTC)
	if _, _, err := PrepareReminder(late, Observation{EndpointID: i.EndpointID, CheckedAt: late.NextReminderAt}); !errors.Is(err, ErrInvalid) {
		t.Fatal("overflowing next time accepted")
	}
	for _, o := range []Observation{{EndpointID: "other", CheckedAt: i.NextReminderAt}, {EndpointID: i.EndpointID}, {EndpointID: i.EndpointID, CheckedAt: i.NextReminderAt, StatusCode: -1}} {
		if _, _, err := PrepareReminder(i, o); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid observation accepted")
		}
	}
	i, tasks, err = PrepareReminder(i, Observation{EndpointID: i.EndpointID, CheckedAt: i.NextReminderAt})
	must(t, err)
	canceled := tasks[0]
	i.RecoveredAt = canceled.OccurredAt.Add(time.Minute)
	i.NextReminderAt = time.Time{}
	canceled, err = CancelReminder(canceled, i)
	must(t, err)
	if canceled.Attempts != 0 || len(canceled.AttemptReceipts) != 0 {
		t.Fatal("unattempted cancellation counted as a send")
	}
	o := deliveryOutcome(tasks[0], TaskSent, i.RecoveredAt.Add(time.Second))
	active := i
	active.RecoveredAt = time.Time{}
	active.NextReminderAt = i.ConfirmedAt.Add(2 * time.Hour)
	if ok, err := canceled.CanceledByRecovery(active, o); err == nil || ok {
		t.Fatal("unconfirmed recovery released fence")
	}
	o.ExpectedAttempts = 1
	if ok, err := canceled.CanceledByRecovery(i, o); err != nil || ok {
		t.Fatal("changed attempt count released fence")
	}
	bad := canceled
	bad.Type = Down
	bad.ReminderSequence = 0
	bad.EventID = eventID(bad.IncidentID, Down, bad.ChannelID)
	if validateTask(bad) == nil {
		t.Fatal("non-reminder accepted recovery cancellation code")
	}
	o = deliveryOutcome(tasks[0], TaskSkipped, i.RecoveredAt)
	o.ErrorCode = ErrorIncidentRecovered
	if _, _, err := normalizeOutcome(o); !errors.Is(err, ErrInvalid) {
		t.Fatal("cancellation reason accepted as a send")
	}
}

func readTask(t *testing.T, s DeliveryStore, i Incident, kind EventType, channel string) Task {
	t.Helper()
	task, err := s.GetTask(context.Background(), eventID(i.ID, kind, channel))
	must(t, err)
	if task == nil {
		t.Fatal("missing task")
	}
	return *task
}

func allDue(t *testing.T, s DeliveryStore, now time.Time, limit int) []Task {
	t.Helper()
	q := DueQuery{Now: now, Limit: limit}
	var tasks []Task
	for range 1000 {
		page, err := s.QueryDue(context.Background(), q)
		must(t, err)
		if len(page.Tasks) > limit {
			t.Fatal("unbounded page")
		}
		tasks = append(tasks, page.Tasks...)
		if page.NextCursor == "" {
			return tasks
		}
		if page.NextCursor == q.Cursor {
			t.Fatal("cursor did not advance")
		}
		q.Cursor = page.NextCursor
	}
	t.Fatal("pagination did not terminate")
	return nil
}

func exerciseDelivery(t *testing.T, s deliveryTestStore) {
	t.Helper()
	ctx := context.Background()
	i := testIncident("delivery", "api")
	_, err := s.Apply(ctx, Transition{Down, i})
	must(t, err)
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	_, err = s.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	if len(allDue(t, s, i.ConfirmedAt.Add(-time.Nanosecond), 1)) != 0 {
		t.Fatal("future tasks delivered")
	}
	due := allDue(t, s, i.RecoveredAt, 1)
	if len(due) != 2 || due[0].Type != Down || due[1].Type != Down {
		t.Fatal("recovery did not wait for DOWN")
	}
	down := readTask(t, s, i, Down, "a")
	if down.Attempts != 0 || !down.NextAttemptAt.IsZero() {
		t.Fatal("initial task defaults")
	}
	retry := deliveryOutcome(down, TaskPending, i.ConfirmedAt)
	retry.RetryAt = i.ConfirmedAt.Add(time.Hour)
	delayed, err := s.RecordOutcome(ctx, retry)
	must(t, err)
	if delayed.Attempts != 1 || delayed.NextAttemptAt != retry.RetryAt || !delayed.FinishedAt.IsZero() {
		t.Fatal("retry metadata")
	}
	_, err = s.RecordOutcome(ctx, retry)
	must(t, err)
	conflict := retry
	conflict.ErrorCode = ErrorTimeout
	if _, err := s.RecordOutcome(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting replay accepted")
	}
	conflict = retry
	conflict.AttemptID = "different"
	if _, err := s.RecordOutcome(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatal("stale expected count accepted")
	}
	tooEarly := deliveryOutcome(delayed, TaskSent, delayed.NextAttemptAt.Add(-time.Nanosecond))
	if _, err := s.RecordOutcome(ctx, tooEarly); !errors.Is(err, ErrConflict) {
		t.Fatal("attempt recorded before due time")
	}
	reusedID := deliveryOutcome(delayed, TaskSent, delayed.NextAttemptAt)
	reusedID.AttemptID = retry.AttemptID
	if _, err := s.RecordOutcome(ctx, reusedID); !errors.Is(err, ErrConflict) {
		t.Fatal("attempt ID reused at a new count")
	}
	recovery := readTask(t, s, i, Recovered, "a")
	if _, err := s.RecordOutcome(ctx, deliveryOutcome(recovery, TaskSent, retry.RetryAt)); !errors.Is(err, ErrConflict) {
		t.Fatal("blocked recovery updated")
	}
	if len(allDue(t, s, retry.RetryAt.Add(-time.Nanosecond), 1)) != 1 {
		t.Fatal("retry was due early")
	}
	sent, err := s.RecordOutcome(ctx, deliveryOutcome(delayed, TaskSent, delayed.NextAttemptAt))
	must(t, err)
	if sent.Status != TaskSent || sent.Attempts != 2 || !sent.NextAttemptAt.IsZero() || sent.FinishedAt != delayed.NextAttemptAt || sent.LastErrorCode != "" {
		t.Fatal("SENT metadata")
	}
	old, err := s.RecordOutcome(ctx, retry)
	must(t, err)
	if old.Status != TaskSent || old.Attempts != 2 {
		t.Fatal("old duplicate rewound task")
	}
	zoneReplay := retry
	zoneReplay.AttemptedAt = zoneReplay.AttemptedAt.In(time.FixedZone("offset", 3600))
	_, err = s.RecordOutcome(ctx, zoneReplay)
	must(t, err)
	matched, err := sent.HasOutcome(retry)
	must(t, err)
	if !matched {
		t.Fatal("read-back did not recognize older outcome")
	}
	matched, err = sent.HasOutcome(conflict)
	must(t, err)
	if matched {
		t.Fatal("read-back accepted conflicting result")
	}
	if _, err := s.RecordOutcome(ctx, deliveryOutcome(sent, TaskPending, sent.FinishedAt)); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal task reopened")
	}
	due = allDue(t, s, sent.FinishedAt, 1)
	if len(due) != 2 {
		t.Fatal("sent DOWN did not release its recovery")
	}
	_, err = s.RecordOutcome(ctx, deliveryOutcome(recovery, TaskSent, sent.FinishedAt))
	must(t, err)
	downB := readTask(t, s, i, Down, "b")
	_, err = s.RecordOutcome(ctx, deliveryOutcome(downB, TaskFailed, sent.FinishedAt))
	must(t, err)
	skipped := readTask(t, s, i, Recovered, "b")
	if skipped.Status != TaskSkipped || skipped.Attempts != 0 || skipped.LastAttemptID != "" || skipped.LastErrorCode != ErrorDependencyFailed || skipped.FinishedAt != sent.FinishedAt {
		t.Fatal("dependent recovery not skipped atomically")
	}
	if len(allDue(t, s, sent.FinishedAt.Add(time.Hour), 1)) != 0 {
		t.Fatal("terminal tasks returned")
	}
	if _, err := s.RecordOutcome(ctx, deliveryOutcome(skipped, TaskSent, skipped.FinishedAt)); !errors.Is(err, ErrConflict) {
		t.Fatal("SKIPPED reopened")
	}
	// Fail DOWN before creating RECOVERED, including an explicitly skipped DOWN.
	for _, status := range []TaskStatus{TaskFailed, TaskSkipped} {
		later := testIncident(string(status), string(status))
		_, err = s.Apply(ctx, Transition{Down, later})
		must(t, err)
		task := readTask(t, s, later, Down, "a")
		_, err = s.RecordOutcome(ctx, deliveryOutcome(task, status, later.ConfirmedAt))
		must(t, err)
		later.RecoveredAt = later.ConfirmedAt.Add(time.Minute)
		_, err = s.Apply(ctx, Transition{Recovered, later})
		must(t, err)
		if task := readTask(t, s, later, Recovered, "a"); task.Status != TaskSkipped || task.FinishedAt != later.RecoveredAt {
			t.Fatal("late recovery did not inherit terminal dependency")
		}
		if task := readTask(t, s, later, Recovered, "b"); task.Status != TaskPending {
			t.Fatal("skip crossed channels")
		}
	}
	// Exhaust ten attempts and skip an already-created recovery in the same write.
	exhausted := testIncident("exhausted", "exhausted")
	exhausted.ChannelIDs = []string{"a"}
	_, err = s.Apply(ctx, Transition{Down, exhausted})
	must(t, err)
	exhausted.RecoveredAt = exhausted.ConfirmedAt.Add(time.Second)
	_, err = s.Apply(ctx, Transition{Recovered, exhausted})
	must(t, err)
	task := readTask(t, s, exhausted, Down, "a")
	for n := 1; n <= MaxAttempts; n++ {
		task, err = s.RecordOutcome(ctx, deliveryOutcome(task, TaskPending, dueAt(task)))
		must(t, err)
		if task.Attempts != n || (task.Status == TaskFailed) != (n == MaxAttempts) {
			t.Fatal("retry exhaustion")
		}
	}
	if !task.NextAttemptAt.IsZero() || readTask(t, s, exhausted, Recovered, "a").Status != TaskSkipped {
		t.Fatal("exhaustion did not skip recovery")
	}
	if _, err := s.RecordOutcome(ctx, deliveryOutcome(task, TaskSent, task.FinishedAt)); !errors.Is(err, ErrInvalid) {
		t.Fatal("eleventh attempt accepted")
	}
	missing, err := s.GetTask(ctx, "missing")
	must(t, err)
	if missing != nil {
		t.Fatal("invented task")
	}
	unknown := retry
	unknown.EventID = eventID("missing", Down, "a")
	if _, err := s.RecordOutcome(ctx, unknown); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown outcome accepted")
	}
	for _, code := range []ErrorCode{"https://PRIVATE", "token=PRIVATE", "raw error\nPRIVATE"} {
		invalid := retry
		invalid.ErrorCode = code
		if _, err := s.RecordOutcome(ctx, invalid); !errors.Is(err, ErrInvalid) {
			t.Fatal("unsafe error code accepted")
		}
	}
	for _, q := range []DueQuery{{Now: testTime, Limit: 0}, {Now: testTime, Limit: MaxDuePageSize + 1}, {Limit: 1}, {Now: testTime, Limit: 1, Cursor: "invalid"}} {
		if _, err := s.QueryDue(ctx, q); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid page query accepted")
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetTask(ctx, down.EventID); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled task read accepted")
	}
	if _, err := s.QueryDue(ctx, DueQuery{Now: testTime, Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled page accepted")
	}
	if _, err := s.RecordOutcome(ctx, retry); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled outcome accepted")
	}
}

func TestBboltDelivery(t *testing.T) { t.Parallel(); exerciseDelivery(t, testBbolt(t)) }

func exerciseDeliveryConcurrency(t *testing.T, s deliveryTestStore) {
	t.Helper()
	ctx := context.Background()
	i := testIncident("concurrent-delivery", "api")
	i.ChannelIDs = []string{"a"}
	_, err := s.Apply(ctx, Transition{Down, i})
	must(t, err)
	task := readTask(t, s, i, Down, "a")
	var wg sync.WaitGroup
	winners := make(chan Outcome, 12)
	for n := range 12 {
		wg.Go(func() {
			o := deliveryOutcome(task, TaskFailed, i.ConfirmedAt)
			o.AttemptID = fmt.Sprintf("racer-%d", n)
			_, err := s.RecordOutcome(ctx, o)
			if err == nil {
				winners <- o
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		})
	}
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	wg.Go(func() {
		_, err := s.Apply(ctx, Transition{Recovered, i})
		if err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatal("conditional update had multiple winners")
	}
	winner := <-winners
	for range 12 {
		wg.Go(func() {
			_, err := s.RecordOutcome(ctx, winner)
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if readTask(t, s, i, Down, "a").Attempts != 1 || readTask(t, s, i, Recovered, "a").Status != TaskSkipped {
		t.Fatal("concurrent recovery escaped skip or duplicated attempt")
	}
}

func TestBboltDeliveryConcurrency(t *testing.T) {
	t.Parallel()
	exerciseDeliveryConcurrency(t, testBbolt(t))
}

// Deliberately omit every C1 field instead of marshaling the current Task type.
func legacyTaskJSON(t *testing.T, task Task) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"event_id": task.EventID, "incident_id": task.IncidentID, "channel_id": task.ChannelID, "type": task.Type, "occurred_at": task.OccurredAt, "status": "PENDING"})
	must(t, err)
	return data
}

type uncertainDelivery struct {
	DeliveryStore
	before, after, read bool
}

func (s *uncertainDelivery) RecordOutcome(ctx context.Context, o Outcome) (Task, error) {
	if s.before {
		return Task{}, errInjected
	}
	task, err := s.DeliveryStore.RecordOutcome(ctx, o)
	if err == nil && s.after {
		return Task{}, errInjected
	}
	return task, err
}

func (s *uncertainDelivery) GetTask(ctx context.Context, id string) (*Task, error) {
	if s.read {
		return nil, errInjected
	}
	return s.DeliveryStore.GetTask(ctx, id)
}

func TestDeliveryReconciliation(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			store := testBbolt(t)
			i := testIncident("uncertain-outcome", "api")
			_, err := store.Apply(context.Background(), Transition{Down, i})
			must(t, err)
			task := readTask(t, store, i, Down, "a")
			o := deliveryOutcome(task, TaskPending, i.ConfirmedAt)
			s := &uncertainDelivery{DeliveryStore: store, before: !committed, after: committed, read: true}
			if _, err := s.RecordOutcome(context.Background(), o); !errors.Is(err, errInjected) {
				t.Fatal("lost write failure")
			}
			if _, err := s.GetTask(context.Background(), task.EventID); !errors.Is(err, errInjected) {
				t.Fatal("unavailable read misreported")
			}
			s.read = false
			observed, err := s.GetTask(context.Background(), task.EventID)
			must(t, err)
			found, err := observed.HasOutcome(o)
			must(t, err)
			if found != committed {
				t.Fatal("uncertain outcome misclassified")
			}
			s.before, s.after = false, false
			retried, err := s.RecordOutcome(context.Background(), o)
			must(t, err)
			if retried.Attempts != 1 {
				t.Fatal("reconciliation duplicated attempt")
			}
		})
	}
}

// Both real backends run the same durable reminder and dependency contract.
func exerciseReminders(t *testing.T, s deliveryTestStore) {
	t.Helper()
	ctx := t.Context()
	i := reminderFixture()
	i.ID = "durable-reminders"
	i.EndpointID = i.ID
	i.ReminderPolicy = &ReminderPolicy{Interval: time.Minute, MaxReminders: 3}
	i.NextReminderAt = i.ConfirmedAt.Add(time.Minute)
	_, err := s.Apply(ctx, Transition{Down, i})
	must(t, err)
	change := ReminderChange{i.ID, 0, i.NextReminderAt.Add(time.Hour)}
	i, err = s.ApplyReminder(ctx, change)
	must(t, err)
	if i.ReminderCount != 1 || !i.NextReminderAt.Equal(change.CheckedAt.Add(time.Minute)) {
		t.Fatal("catch-up generated multiple events or kept old deadline")
	}
	_, err = s.ApplyReminder(ctx, change)
	must(t, err)
	changed := change
	changed.CheckedAt = changed.CheckedAt.Add(time.Second)
	if _, err = s.ApplyReminder(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting occurrence accepted")
	}
	id1, _ := ReminderEventID(i.ID, 1, "a")
	if task, _, err := s.ReadDelivery(ctx, id1, change.CheckedAt); err != nil || task != nil {
		t.Fatal("reminder sent before DOWN")
	}
	down := readTask(t, s, i, Down, "a")
	_, err = s.RecordOutcome(ctx, deliveryOutcome(down, TaskSent, i.ConfirmedAt))
	must(t, err)
	other := readTask(t, s, i, Down, "b")
	_, err = s.RecordOutcome(ctx, deliveryOutcome(other, TaskFailed, i.ConfirmedAt))
	must(t, err)
	skipped, err := s.GetTask(ctx, func() string { id, _ := ReminderEventID(i.ID, 1, "b"); return id }())
	must(t, err)
	if skipped.Status != TaskSkipped || skipped.Attempts != 0 {
		t.Fatal("DOWN failure did not atomically skip reminder")
	}
	first, err := s.GetTask(ctx, id1)
	must(t, err)
	prior := deliveryOutcome(*first, TaskPending, first.OccurredAt)
	pending, err := s.RecordOutcome(ctx, prior)
	must(t, err)
	i, err = s.ApplyReminder(ctx, ReminderChange{i.ID, 1, i.NextReminderAt})
	must(t, err)
	id2, _ := ReminderEventID(i.ID, 2, "a")
	if task, _, err := s.ReadDelivery(ctx, id2, i.NextReminderAt); err != nil || task != nil {
		t.Fatal("second reminder bypassed first retry")
	}
	failed := deliveryOutcome(pending, TaskFailed, pending.NextAttemptAt)
	_, err = s.RecordOutcome(ctx, failed)
	must(t, err)
	second, _, err := s.ReadDelivery(ctx, id2, i.NextReminderAt)
	must(t, err)
	if second == nil || second.ReminderSequence != 2 || second.Attempts != 0 {
		t.Fatal("failed predecessor blocked next sequence")
	}
	old := deliveryOutcome(*second, TaskPending, second.OccurredAt)
	pending, err = s.RecordOutcome(ctx, old)
	must(t, err)
	i, err = s.ApplyReminder(ctx, ReminderChange{i.ID, 2, i.NextReminderAt})
	must(t, err)
	if i.ReminderCount != 3 || !i.NextReminderAt.IsZero() {
		t.Fatal("limit did not exhaust")
	}
	if _, err = s.ApplyReminder(ctx, ReminderChange{i.ID, 3, change.CheckedAt.Add(24 * time.Hour)}); !errors.Is(err, ErrConflict) {
		t.Fatal("limit exceeded")
	}
	i.RecoveredAt = change.CheckedAt.Add(3 * time.Minute)
	i.NextReminderAt = time.Time{}
	_, err = s.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	canceled, err := s.GetTask(ctx, id2)
	must(t, err)
	matched, err := canceled.HasOutcome(old)
	must(t, err)
	if canceled.Status != TaskSkipped || canceled.LastErrorCode != ErrorIncidentRecovered || canceled.Attempts != 1 || !matched {
		t.Fatal("recovery destroyed retry history")
	}
	late := deliveryOutcome(pending, TaskSent, i.RecoveredAt.Add(time.Second))
	if _, err = s.RecordOutcome(ctx, late); !errors.Is(err, ErrConflict) {
		t.Fatal("late outcome overwrote cancellation")
	}
	ok, err := canceled.CanceledByRecovery(i, late)
	must(t, err)
	if !ok {
		t.Fatal("cancellation not reconcilable")
	}
	recovery := readTask(t, s, i, Recovered, "a")
	ready, _, err := s.ReadDelivery(ctx, recovery.EventID, i.RecoveredAt)
	must(t, err)
	if ready == nil {
		t.Fatal("canceled reminders blocked recovery")
	}
	if readTask(t, s, i, Recovered, "b").Status != TaskSkipped {
		t.Fatal("late recovery ignored failed DOWN")
	}
	_, err = s.ApplyReminder(ctx, change)
	must(t, err) // Replay remains safe after recovery.
}
func TestBboltDurableReminders(t *testing.T) { exerciseReminders(t, testBbolt(t)) }

func exerciseReminderRace(t *testing.T, s deliveryTestStore) {
	t.Helper()
	ctx := t.Context()
	for n := range 8 {
		i := reminderFixture()
		i.ID = fmt.Sprintf("reminder-race-%d", n)
		i.EndpointID = i.ID
		i.ChannelIDs = []string{"a"}
		_, err := s.Apply(ctx, Transition{Down, i})
		must(t, err)
		down := readTask(t, s, i, Down, "a")
		_, err = s.RecordOutcome(ctx, deliveryOutcome(down, TaskSent, i.ConfirmedAt))
		must(t, err)
		c := ReminderChange{i.ID, 0, i.NextReminderAt}
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				_, err := s.ApplyReminder(ctx, c)
				if err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		stored, err := s.Get(ctx, i.ID)
		must(t, err)
		i = *stored
		id, _ := ReminderEventID(i.ID, 1, "a")
		task, err := s.GetTask(ctx, id)
		must(t, err)
		i.RecoveredAt = c.CheckedAt.Add(time.Second)
		i.NextReminderAt = time.Time{}
		o := deliveryOutcome(*task, TaskSent, i.RecoveredAt)
		wg.Go(func() {
			_, err := s.Apply(ctx, Transition{Recovered, i})
			if err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			_, err := s.RecordOutcome(ctx, o)
			if err != nil && !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		})
		wg.Wait()
		final, err := s.GetTask(ctx, id)
		must(t, err)
		if final.Status != TaskSent && final.Status != TaskSkipped {
			t.Fatal("illegal race result")
		}
		if final.Status == TaskSkipped && final.Attempts != 0 {
			t.Fatal("cancellation invented attempt")
		}
	}
}
func TestBboltReminderRace(t *testing.T) { exerciseReminderRace(t, testBbolt(t)) }

func exerciseHistory(t *testing.T, s interface {
	Store
	HistoryStore
}) {
	t.Helper()
	ctx := t.Context()
	empty, err := s.ListIncidents(ctx, HistoryQuery{})
	must(t, err)
	if len(empty.Incidents) != 0 || empty.NextCursor != "" {
		t.Fatal("nonempty initial history")
	}
	var want []string
	for n := range 25 {
		id := fmt.Sprintf("history-%02d", n)
		i := testIncident(id, id)
		i.FirstFailureAt = testTime
		i.ConfirmedAt = testTime.Add(time.Duration(n/3) * time.Second)
		_, err := s.Apply(ctx, Transition{Down, i})
		must(t, err)
		want = append([]string{id}, want...)
	}
	var got []string
	cursor := ""
	pages := 0
	for {
		page, err := s.ListIncidents(ctx, HistoryQuery{Limit: 2, Cursor: cursor})
		must(t, err)
		for _, i := range page.Incidents {
			got = append(got, i.ID)
		}
		pages++
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if pages > 20 {
			t.Fatal("history cursor loop")
		}
	}
	defaultPage, err := s.ListIncidents(ctx, HistoryQuery{})
	must(t, err)
	if len(defaultPage.Incidents) != 20 || defaultPage.NextCursor == "" {
		t.Fatal("default history limit")
	}
	maxPage, err := s.ListIncidents(ctx, HistoryQuery{Limit: 50})
	must(t, err)
	if len(maxPage.Incidents) != 25 || maxPage.NextCursor != "" {
		t.Fatal("maximum history page")
	}
	if !slices.Equal(got, want) || pages != 13 {
		t.Fatalf("history paging: got %v want %v", got, want)
	}
	key, err := historyKey(Incident{ID: want[len(want)-1], ConfirmedAt: testTime})
	must(t, err)
	page, err := s.ListIncidents(ctx, HistoryQuery{Cursor: historyCursor(key)})
	must(t, err)
	if len(page.Incidents) != 0 || page.NextCursor != "" {
		t.Fatal("nonempty end page")
	}
	for _, q := range []HistoryQuery{{Limit: -1}, {Limit: 51}, {Cursor: "invalid"}, {Cursor: strings.Repeat("x", 1025)}} {
		if _, err := s.ListIncidents(ctx, q); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid history cursor accepted")
		}
	}
}
func TestBboltHistory(t *testing.T) { exerciseHistory(t, testBbolt(t)) }

func exerciseReminderEngine(t *testing.T, s deliveryTestStore) {
	t.Helper()
	ctx := t.Context()
	e, err := NewEngine(s, nil)
	must(t, err)
	policy := Policy{FailureThreshold: 1, RecoverySuccesses: 2, ChannelIDs: []string{"a"}, ReminderPolicy: &ReminderPolicy{Interval: time.Minute, MaxReminders: 2}}
	at := testTime
	_, err = e.Observe(ctx, Observation{EndpointID: "engine", CheckedAt: at}, policy)
	must(t, err)
	// Changing live config cannot replace the frozen policy.
	policy.ReminderPolicy = nil
	_, err = e.Observe(ctx, Observation{EndpointID: "engine", CheckedAt: at.Add(time.Minute), Gap: true}, policy)
	must(t, err)
	_, err = e.Observe(ctx, Observation{EndpointID: "engine", CheckedAt: at.Add(2 * time.Minute), Success: true}, policy)
	must(t, err)
	i, err := s.Active(ctx, "engine")
	must(t, err)
	if i.ReminderCount != 0 {
		t.Fatal("success or gap issued reminder")
	}
	// Restart resets the unconfirmed recovery count but preserves the deadline.
	e, err = NewEngine(s, nil)
	must(t, err)
	_, err = e.Observe(ctx, Observation{EndpointID: "engine", CheckedAt: at.Add(time.Hour)}, policy)
	must(t, err)
	i, err = s.Active(ctx, "engine")
	must(t, err)
	if i.ReminderCount != 1 || !i.NextReminderAt.Equal(at.Add(time.Hour+time.Minute)) {
		t.Fatal("restart reminder schedule")
	}
	_, err = e.Observe(ctx, Observation{EndpointID: "engine", CheckedAt: i.NextReminderAt}, policy)
	must(t, err)
	i, err = s.Active(ctx, "engine")
	must(t, err)
	if i.ReminderCount != 2 || !i.NextReminderAt.IsZero() {
		t.Fatal("restart limit")
	}
	legacy := testIncident("legacy-firing", "legacy-firing")
	_, err = s.Apply(ctx, Transition{Down, legacy})
	must(t, err)
	policy.ReminderPolicy = &ReminderPolicy{Interval: time.Second, MaxReminders: 1}
	_, err = e.Observe(ctx, Observation{EndpointID: legacy.EndpointID, CheckedAt: at.Add(24 * time.Hour)}, policy)
	must(t, err)
	old, err := s.Active(ctx, legacy.EndpointID)
	must(t, err)
	if old.ReminderPolicy != nil || old.ReminderCount != 0 {
		t.Fatal("legacy incident acquired policy")
	}
}
func TestBboltReminderEngine(t *testing.T) { exerciseReminderEngine(t, testBbolt(t)) }

type faultyReminderStore struct {
	deliveryTestStore
	before, after, read bool
	changes             []ReminderChange
}

func (s *faultyReminderStore) ApplyReminder(ctx context.Context, c ReminderChange) (Incident, error) {
	s.changes = append(s.changes, c)
	if s.before {
		return Incident{}, errInjected
	}
	i, err := s.deliveryTestStore.ApplyReminder(ctx, c)
	if err == nil && s.after {
		return Incident{}, errInjected
	}
	return i, err
}
func (s *faultyReminderStore) Get(ctx context.Context, id string) (*Incident, error) {
	if s.read {
		return nil, errInjected
	}
	return s.deliveryTestStore.Get(ctx, id)
}
func TestReminderUncertainReconciliation(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			base := testBbolt(t)
			i := reminderFixture()
			_, err := base.Apply(t.Context(), Transition{Down, i})
			must(t, err)
			store := &faultyReminderStore{deliveryTestStore: base, before: !committed, after: committed, read: true}
			e, err := NewEngine(store, nil)
			must(t, err)
			o := Observation{EndpointID: i.EndpointID, CheckedAt: i.NextReminderAt}
			if _, err = e.Observe(t.Context(), o, Policy{}); !errors.Is(err, errInjected) {
				t.Fatal("unknown result hidden")
			}
			actual, err := base.Get(t.Context(), i.ID)
			must(t, err)
			if (actual.ReminderCount == 1) != committed {
				t.Fatal("fault injection did not exercise requested commit")
			}
			store.before, store.after, store.read = false, false, false
			o.CheckedAt = o.CheckedAt.Add(time.Second)
			_, err = e.Observe(t.Context(), o, Policy{})
			must(t, err)
			actual, err = base.Get(t.Context(), i.ID)
			must(t, err)
			if actual.ReminderCount != 1 || len(store.changes) != 2 || store.changes[0] != store.changes[1] {
				t.Fatal("reconciliation changed count/time identity")
			}
			tasks, err := base.Outbox(t.Context())
			must(t, err)
			if len(tasks) != 4 {
				t.Fatal("uncertain reminder duplicated or omitted tasks")
			}
		})
	}
}

func exerciseIncidentDetails(t *testing.T, s interface {
	deliveryTestStore
	HistoryStore
}) {
	t.Helper()
	exerciseReminders(t, s) // Real persisted retries, failed predecessor and recovery cancellation.
	ctx := t.Context()
	cursor := ""
	seen := map[string]bool{}
	states := map[TaskStatus]bool{}
	for pages := 0; ; pages++ {
		page, err := s.ReadIncident(ctx, DetailQuery{IncidentID: "durable-reminders", Cursor: cursor, Limit: 3})
		must(t, err)
		if page.Incident.RecoveredAt.IsZero() || page.Incident.ReminderCount != 3 || len(page.Tasks) > 3 {
			t.Fatal("inconsistent detail snapshot")
		}
		for _, task := range page.Tasks {
			if seen[task.EventID] {
				t.Fatal("duplicate detail task")
			}
			seen[task.EventID] = true
			states[task.Status] = true
			if task.Type == Reminder && task.ChannelID == "a" && task.ReminderSequence == 2 && (task.Attempts != 1 || task.Status != TaskSkipped || len(task.AttemptReceipts) != 1) {
				t.Fatal("detail lost canceled retry history")
			}
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
		if pages > 5 {
			t.Fatal("detail cursor loop")
		}
	}
	if len(seen) != 10 || len(states) != 4 {
		t.Fatal("missing history event or delivery state")
	}
	for _, channels := range [][]string{nil, {"a", "b"}} {
		i := testIncident(fmt.Sprintf("legacy-%d", len(channels)), fmt.Sprintf("legacy-endpoint-%d", len(channels)))
		i.ChannelIDs = channels
		_, err := s.Apply(ctx, Transition{Down, i})
		must(t, err)
		page, err := s.ReadIncident(ctx, DetailQuery{IncidentID: i.ID})
		must(t, err)
		if len(page.Tasks) != len(channels) || page.Incident.ReminderPolicy != nil || !page.Incident.RecoveredAt.IsZero() {
			t.Fatal("legacy/no-channel history changed")
		}
	}
	many := testIncident("many-channels", "many-channels")
	many.ChannelIDs = nil
	for n := range 61 {
		many.ChannelIDs = append(many.ChannelIDs, fmt.Sprintf("channel-%03d", n))
	}
	_, err := s.Apply(ctx, Transition{Down, many})
	must(t, err)
	page, err := s.ReadIncident(ctx, DetailQuery{IncidentID: many.ID})
	must(t, err)
	if len(page.Tasks) != 20 || page.NextCursor == "" {
		t.Fatal("unbounded detail default")
	}
	count := 0
	_, err = readIncidentDetail(&many, DetailQuery{IncidentID: many.ID, Limit: 50}, func(id string) (*Task, error) { count++; return s.GetTask(ctx, id) })
	must(t, err)
	if count != 50 {
		t.Fatal("detail exceeded its read bound")
	}
	for _, q := range []DetailQuery{{IncidentID: "../invalid"}, {IncidentID: many.ID, Limit: 51}, {IncidentID: many.ID, Cursor: "invalid"}, {IncidentID: "other", Cursor: page.NextCursor}, {IncidentID: many.ID, Cursor: strings.Repeat("x", 1025)}} {
		if _, err := s.ReadIncident(ctx, q); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid detail query accepted")
		}
	}
	if _, err := s.ReadIncident(ctx, DetailQuery{IncidentID: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing incident accepted")
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ReadIncident(ctx, DetailQuery{IncidentID: many.ID}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled detail read")
	}
}
func TestBboltIncidentDetails(t *testing.T) { exerciseIncidentDetails(t, testBbolt(t)) }

func exerciseDetailRecoveryRace(t *testing.T, s interface {
	Store
	HistoryStore
}) {
	i := testIncident("detail-race", "detail-race")
	_, err := s.Apply(t.Context(), Transition{Down, i})
	must(t, err)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 20 {
			page, err := s.ReadIncident(t.Context(), DetailQuery{IncidentID: i.ID})
			if err != nil {
				t.Error(err)
				return
			}
			want := 2
			if !page.Incident.RecoveredAt.IsZero() {
				want = 4
			}
			if len(page.Tasks) != want {
				t.Error("torn incident/task read")
			}
		}
	})
	i.RecoveredAt = i.ConfirmedAt.Add(time.Minute)
	_, err = s.Apply(t.Context(), Transition{Recovered, i})
	must(t, err)
	wg.Wait()
}
func TestBboltIncidentDetailRace(t *testing.T) { exerciseDetailRecoveryRace(t, testBbolt(t)) }
