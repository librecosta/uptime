// Package alerts implements an observation-driven alert core, independent of
// probing, configuration loading, and notification delivery.
package alerts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"
)

// State describes availability confirmed by actual observations.
type State string

const (
	Healthy State = "HEALTHY"
	Pending State = "PENDING"
	Firing  State = "FIRING"
)

// EventType identifies a durable incident transition, not a delivery attempt.
type EventType string

const (
	Down      EventType = "DOWN"
	Recovered EventType = "RECOVERED"
)

var (
	ErrConflict = errors.New("alerts: conflicting incident transition")
	ErrNotFound = errors.New("alerts: incident not found")
	ErrInvalid  = errors.New("alerts: invalid data or schema")
)

// Observation contains only probe facts. Gap is a standalone loss-of-events
// signal: its Success/StatusCode fields are ignored, and it never closes an incident.
type Observation struct {
	EndpointID string
	CheckedAt  time.Time
	Success    bool
	StatusCode int
	Gap        bool
}

// Policy is supplied explicitly by the caller, without channel credentials.
type Policy struct {
	FailureThreshold int // Zero selects three; otherwise 1 through 10.
	ChannelIDs       []string
}

// Incident stores a stable random identity and immutable channel ID snapshot.
// A zero RecoveredAt denotes an active incident. No channel credentials are stored.
type Incident struct {
	ID             string    `json:"id"`
	EndpointID     string    `json:"endpoint_id"`
	FirstFailureAt time.Time `json:"first_failure_at"`
	ConfirmedAt    time.Time `json:"confirmed_at"`
	RecoveredAt    time.Time `json:"recovered_at"`
	ChannelIDs     []string  `json:"channel_ids"`
}

// Task is a simulated outbox entry. B1 only creates PENDING tasks.
type Task struct {
	EventID    string    `json:"event_id"`
	IncidentID string    `json:"incident_id"`
	ChannelID  string    `json:"channel_id"`
	Type       EventType `json:"type"`
	OccurredAt time.Time `json:"occurred_at"`
	Status     string    `json:"status"`
}

// Transition atomically persists Incident and its derived per-channel tasks.
// Retrying the same incident ID is safe, even after an uncertain commit response.
type Transition struct {
	Type     EventType
	Incident Incident
}

// Store serializes conditional transitions with their outbox tasks. Returned
// records belong to the caller. Resource ownership is specific to each backend.
type Store interface {
	Active(context.Context, string) (*Incident, error)
	Get(context.Context, string) (*Incident, error)
	Apply(context.Context, Transition) (Incident, error)
	Outbox(context.Context) ([]Task, error)
}

// machine is pure in-memory state. Only confirmed incidents survive a restart.
type machine struct {
	state       State
	failures    int
	first, last time.Time
}

func advance(m machine, o Observation, threshold int) (machine, EventType) {
	if m.state == "" {
		m.state = Healthy
	}
	if o.Gap {
		if o.CheckedAt.After(m.last) {
			m.last = o.CheckedAt
		}
		if m.state != Firing {
			m.state, m.failures, m.first = Healthy, 0, time.Time{}
		}
		return m, ""
	}
	// Equal timestamps can describe distinct probes (including a fixed test clock).
	// Completion time is not an observation identity or replay-deduplication key.
	if !m.last.IsZero() && o.CheckedAt.Before(m.last) {
		return m, ""
	}
	m.last = o.CheckedAt
	if o.Success {
		kind := EventType("")
		if m.state == Firing {
			kind = Recovered
		}
		m.state, m.failures, m.first = Healthy, 0, time.Time{}
		return m, kind
	}
	if m.state == Firing {
		return m, ""
	}
	if m.failures == 0 {
		m.first = o.CheckedAt
	}
	m.failures++
	m.state = Pending
	if m.failures >= threshold {
		m.state = Firing
		return m, Down
	}
	return m, ""
}

type pendingTransition struct {
	change Transition
	next   machine
}

// Engine serializes observations and persistence for a single alert executor.
// It owns no workers and never closes its Store. A nil clock uses time.Now.
// Zero observation timestamps use the clock; supplied completion times are preserved.
type Engine struct {
	mu      sync.Mutex
	store   Store
	now     func() time.Time
	states  map[string]machine
	pending map[string]pendingTransition
}

// NewEngine creates an idle core. Persistent state is loaded on observation;
// constructing an engine neither opens a store nor performs monitoring work.
func NewEngine(store Store, now func() time.Time) (*Engine, error) {
	if store == nil {
		return nil, ErrInvalid
	}
	if now == nil {
		now = time.Now
	}
	return &Engine{store: store, now: now, states: map[string]machine{}, pending: map[string]pendingTransition{}}, nil
}

// Observe consumes a real probe or gap signal. Storage errors never count as
// probe failures. A failed transition remains available for synchronous reconciliation
// on the next call; no background retries or fabricated observations are generated.
func (e *Engine) Observe(ctx context.Context, o Observation, policy Policy) (State, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	threshold := policy.FailureThreshold
	if threshold == 0 {
		threshold = 3
	}
	channels, err := normalizeChannels(policy.ChannelIDs)
	if err != nil || threshold < 1 || threshold > 10 || o.EndpointID == "" || o.StatusCode < 0 || o.StatusCode > 599 {
		return "", ErrInvalid
	}
	if o.CheckedAt.IsZero() {
		o.CheckedAt = e.now()
	}
	if o.CheckedAt.IsZero() {
		return "", ErrInvalid
	}
	o.CheckedAt = o.CheckedAt.UTC()
	if p, ok := e.pending[o.EndpointID]; ok {
		if _, err := e.commit(ctx, p.change); err != nil {
			return "", err
		}
		e.states[o.EndpointID] = p.next
		delete(e.pending, o.EndpointID)
	}
	active, err := e.store.Active(ctx, o.EndpointID)
	if err != nil {
		// This observation could not be consumed. Break an unconfirmed sequence
		// rather than count failures across an unobserved success.
		delete(e.states, o.EndpointID)
		return "", err
	}
	m := e.states[o.EndpointID]
	if active != nil {
		m.state, m.first = Firing, active.FirstFailureAt
		if m.last.Before(active.ConfirmedAt) {
			m.last = active.ConfirmedAt
		}
	} else if m.state == Firing {
		m = machine{state: Healthy, last: m.last}
	}
	next, kind := advance(m, o, threshold)
	if kind == "" {
		e.states[o.EndpointID] = next
		return next.state, nil
	}
	var incident Incident
	if kind == Down {
		incident = Incident{ID: rand.Text(), EndpointID: o.EndpointID, FirstFailureAt: next.first, ConfirmedAt: o.CheckedAt, ChannelIDs: channels}
	} else {
		incident = *active
		incident.RecoveredAt = o.CheckedAt
	}
	change := Transition{Type: kind, Incident: incident}
	e.pending[o.EndpointID] = pendingTransition{change: change, next: next}
	if _, err := e.commit(ctx, change); err != nil {
		return "", err
	}
	delete(e.pending, o.EndpointID)
	e.states[o.EndpointID] = next
	return next.state, nil
}

func (e *Engine) commit(ctx context.Context, change Transition) (Incident, error) {
	stored, err := e.store.Apply(ctx, change)
	if err == nil {
		return stored, nil
	}
	// An I/O error may follow a committed transaction. Verify durable facts;
	// never infer a commit or rollback merely from the transport error.
	observed, readErr := e.store.Get(ctx, change.Incident.ID)
	if readErr == nil && observed != nil && sameIncident(*observed, change.Incident) &&
		(change.Type == Down || observed.RecoveredAt.Equal(change.Incident.RecoveredAt)) {
		return *observed, nil
	}
	return Incident{}, err
}

var channelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func normalizeChannels(ids []string) ([]string, error) {
	result := slices.Clone(ids)
	for _, id := range result {
		if !channelPattern.MatchString(id) {
			return nil, ErrInvalid
		}
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func validateIncident(i Incident) error {
	ids, err := normalizeChannels(i.ChannelIDs)
	if err != nil || !slices.Equal(ids, i.ChannelIDs) || i.ID == "" || i.EndpointID == "" || i.FirstFailureAt.IsZero() || i.ConfirmedAt.IsZero() || i.ConfirmedAt.Before(i.FirstFailureAt) || (!i.RecoveredAt.IsZero() && i.RecoveredAt.Before(i.ConfirmedAt)) {
		return ErrInvalid
	}
	return nil
}

func sameIncident(a, b Incident) bool {
	return a.ID == b.ID && a.EndpointID == b.EndpointID && a.FirstFailureAt.Equal(b.FirstFailureAt) && a.ConfirmedAt.Equal(b.ConfirmedAt) && slices.Equal(a.ChannelIDs, b.ChannelIDs)
}

// prepare is shared validation, not shared I/O: backends must call it within
// their atomic conditional write boundary, before changing any persistent data.
func prepare(existing *Incident, activeID string, change Transition) (Incident, []Task, error) {
	i := change.Incident
	if err := validateIncident(i); err != nil {
		return Incident{}, nil, err
	}
	switch change.Type {
	case Down:
		if !i.RecoveredAt.IsZero() {
			return Incident{}, nil, ErrInvalid
		}
		if existing != nil {
			if !sameIncident(*existing, i) {
				return Incident{}, nil, ErrConflict
			}
			return *existing, nil, nil
		}
		if activeID != "" {
			return Incident{}, nil, ErrConflict
		}
	case Recovered:
		if existing == nil {
			return Incident{}, nil, ErrNotFound
		}
		if i.RecoveredAt.IsZero() || !sameIncident(*existing, i) {
			return Incident{}, nil, ErrConflict
		}
		if !existing.RecoveredAt.IsZero() {
			return *existing, nil, nil
		}
		if activeID != i.ID {
			return Incident{}, nil, ErrConflict
		}
	default:
		return Incident{}, nil, ErrInvalid
	}
	at := i.ConfirmedAt
	if change.Type == Recovered {
		at = i.RecoveredAt
	}
	tasks := make([]Task, 0, len(i.ChannelIDs))
	for _, channel := range i.ChannelIDs {
		sum := sha256.Sum256([]byte(i.ID + "\x00" + string(change.Type) + "\x00" + channel))
		tasks = append(tasks, Task{EventID: hex.EncodeToString(sum[:]), IncidentID: i.ID, ChannelID: channel, Type: change.Type, OccurredAt: at, Status: "PENDING"})
	}
	// A non-nil empty slice still denotes a new transition with no channels.
	return i, tasks, nil
}

func decodeIncident(data []byte) (*Incident, error) {
	if data == nil {
		return nil, nil
	}
	var i Incident
	if err := json.Unmarshal(data, &i); err != nil {
		return nil, fmt.Errorf("%w: incident encoding", ErrInvalid)
	}
	if err := validateIncident(i); err != nil {
		return nil, err
	}
	return &i, nil
}

func decodeTask(data []byte) (Task, error) {
	var task Task
	if err := json.Unmarshal(data, &task); err != nil {
		return task, ErrInvalid
	}
	if task.Status != "PENDING" || task.EventID == "" || task.IncidentID == "" || !channelPattern.MatchString(task.ChannelID) || (task.Type != Down && task.Type != Recovered) || task.OccurredAt.IsZero() {
		return Task{}, ErrInvalid
	}
	return task, nil
}
