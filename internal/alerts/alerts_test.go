package alerts

import (
	"context"
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
