package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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
				m, event = advance(m, Observation{CheckedAt: testTime.Add(time.Duration(n) * time.Second)}, threshold)
				if n < threshold && (m.state != Pending || event != "") {
					t.Fatal("premature DOWN")
				}
				if n == threshold && (m.state != Firing || event != Down) {
					t.Fatal("missing DOWN")
				}
			}
			m, event := advance(m, Observation{CheckedAt: testTime.Add(20 * time.Second)}, threshold)
			if m.state != Firing || event != "" {
				t.Fatal("repeated failure produced transition")
			}
			m, event = advance(m, Observation{CheckedAt: testTime.Add(21 * time.Second), Gap: true, Success: true}, threshold)
			if m.state != Firing || event != "" {
				t.Fatal("gap closed an incident")
			}
			m, event = advance(m, Observation{CheckedAt: testTime.Add(22 * time.Second), Success: true}, threshold)
			if m.state != Healthy || event != Recovered {
				t.Fatal("missing recovery")
			}
			_, event = advance(m, Observation{CheckedAt: testTime.Add(23 * time.Second), Success: true}, threshold)
			if event != "" {
				t.Fatal("duplicate recovery")
			}
		})
	}
	m, _ := advance(machine{}, Observation{CheckedAt: testTime}, 3)
	for _, o := range []Observation{{CheckedAt: testTime.Add(time.Second), Success: true}, {CheckedAt: testTime.Add(time.Second), Gap: true}} {
		next, event := advance(m, o, 3)
		if next.state != Healthy || next.failures != 0 || event != "" {
			t.Fatal("pending not reset")
		}
		next, event = advance(next, Observation{CheckedAt: testTime.Add(2 * time.Second)}, 3)
		if next.failures != 1 || event != "" {
			t.Fatal("reset sequence reused failures")
		}
	}
	unchanged, event := advance(m, Observation{CheckedAt: testTime.Add(-time.Second), Success: true}, 3)
	if unchanged != m || event != "" {
		t.Fatal("out-of-order event consumed")
	}
	for range 2 {
		m, _ = advance(m, Observation{CheckedAt: testTime}, 3)
	}
	if m.state != Firing {
		t.Fatal("equal completion times lost actual probes")
	}
	m, event = advance(m, Observation{CheckedAt: testTime, Success: true}, 3)
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
