package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"time"
)

// ReminderPolicy is an immutable incident snapshot. A nil snapshot disables
// reminders, including for legacy incidents. Schema v1 cannot persist it.
type ReminderPolicy struct {
	Interval     time.Duration `json:"interval"`
	MaxReminders int           `json:"max_reminders"`
}

func sameReminderPolicy(a, b *ReminderPolicy) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func validateReminders(i Incident) error {
	if i.ReminderPolicy == nil {
		if i.ReminderCount != 0 || !i.NextReminderAt.IsZero() {
			return ErrInvalid
		}
		return nil
	}
	p := i.ReminderPolicy
	if p.Interval <= 0 || p.MaxReminders <= 0 || i.ReminderCount < 0 || i.ReminderCount > p.MaxReminders || !validTime(i.FirstFailureAt) || !validTime(i.ConfirmedAt) || !i.RecoveredAt.IsZero() && !validTime(i.RecoveredAt) {
		return ErrInvalid
	}
	if !i.RecoveredAt.IsZero() || i.ReminderCount == p.MaxReminders {
		if !i.NextReminderAt.IsZero() {
			return ErrInvalid
		}
	} else if !validTime(i.NextReminderAt) || i.NextReminderAt.Before(i.ConfirmedAt.Add(p.Interval)) {
		return ErrInvalid
	}
	return nil
}

// ReminderEventID gives each per-channel sequence a stable identity, independent
// of occurrence/attempt time. DOWN and RECOVERED keep their original identities.
func ReminderEventID(incident string, sequence int, channel string) (string, error) {
	if incident == "" || sequence <= 0 || !channelPattern.MatchString(channel) {
		return "", ErrInvalid
	}
	sum := sha256.Sum256([]byte(incident + "\x00REMINDER\x00" + strconv.Itoa(sequence) + "\x00" + channel))
	return hex.EncodeToString(sum[:]), nil
}

// ValidateReminder checks identity, snapshot and event times without I/O. Closed
// incidents remain valid for history/cancellation; senders must reject them.
func (t Task) ValidateReminder(i Incident) error {
	if validateTask(t) != nil || validateIncident(i) != nil || t.Type != Reminder || t.IncidentID != i.ID || !slices.Contains(i.ChannelIDs, t.ChannelID) || i.ReminderPolicy == nil || t.ReminderSequence > i.ReminderCount || t.OccurredAt.Before(i.ConfirmedAt.Add(i.ReminderPolicy.Interval)) || !i.RecoveredAt.IsZero() && t.OccurredAt.After(i.RecoveredAt) {
		return ErrInvalid
	}
	return nil
}

// PrepareReminder is pure. Only a due, real failed observation advances a frozen
// policy, at most once regardless of downtime. B2 must call it inside an atomic
// reminder transaction; Engine and Schema v1 deliberately never call it.
func PrepareReminder(i Incident, o Observation) (Incident, []Task, error) {
	if err := validateIncident(i); err != nil {
		return Incident{}, nil, err
	}
	if o.EndpointID != i.EndpointID || !validTime(o.CheckedAt) || o.StatusCode < 0 || o.StatusCode > 599 {
		return Incident{}, nil, ErrInvalid
	}
	if o.Gap || o.Success || !i.RecoveredAt.IsZero() || i.ReminderPolicy == nil || i.ReminderCount == i.ReminderPolicy.MaxReminders || o.CheckedAt.Before(i.NextReminderAt) {
		return i, nil, nil
	}
	i.ReminderCount++
	i.NextReminderAt = time.Time{}
	if i.ReminderCount < i.ReminderPolicy.MaxReminders {
		i.NextReminderAt = o.CheckedAt.UTC().Add(i.ReminderPolicy.Interval)
	}
	if err := validateIncident(i); err != nil {
		return Incident{}, nil, err
	}
	tasks := make([]Task, 0, len(i.ChannelIDs))
	for _, channel := range i.ChannelIDs {
		id, err := ReminderEventID(i.ID, i.ReminderCount, channel)
		if err != nil {
			return Incident{}, nil, err
		}
		tasks = append(tasks, Task{EventID: id, IncidentID: i.ID, ChannelID: channel, Type: Reminder, ReminderSequence: i.ReminderCount, OccurredAt: o.CheckedAt.UTC(), Status: TaskPending})
	}
	return i, tasks, nil
}

// DeliveryReady checks the per-channel chain using an incident snapshot from the
// same store read/transaction. Previous is reminder N-1 for a reminder, or the
// last issued reminder for recovery; nil is valid only if none is required.
// Missing/corrupt dependencies are errors, not permission to send.
func DeliveryReady(t Task, i Incident, down, previous *Task, now time.Time) (bool, error) {
	if validateTask(t) != nil || validateIncident(i) != nil || !validTime(now) || t.IncidentID != i.ID || !slices.Contains(i.ChannelIDs, t.ChannelID) {
		return false, ErrInvalid
	}
	if t.Type == Down {
		if !t.OccurredAt.Equal(i.ConfirmedAt) {
			return false, ErrInvalid
		}
		return deliverable(t, nil, now), nil
	}
	if down == nil || validateTask(*down) != nil || down.Type != Down || down.EventID != eventID(i.ID, Down, t.ChannelID) || !down.OccurredAt.Equal(i.ConfirmedAt) {
		return false, ErrInvalid
	}
	sequence := i.ReminderCount
	if t.Type == Reminder {
		if t.ValidateReminder(i) != nil {
			return false, ErrInvalid
		}
		if !i.RecoveredAt.IsZero() {
			return false, nil
		}
		sequence = t.ReminderSequence - 1
	} else if i.RecoveredAt.IsZero() || !t.OccurredAt.Equal(i.RecoveredAt) {
		return false, ErrInvalid
	}
	if sequence == 0 {
		if previous != nil {
			return false, ErrInvalid
		}
	} else {
		id, _ := ReminderEventID(i.ID, sequence, t.ChannelID)
		if previous == nil || validateTask(*previous) != nil || previous.EventID != id || previous.Type != Reminder || previous.OccurredAt.Before(i.ConfirmedAt) || previous.OccurredAt.After(t.OccurredAt) {
			return false, ErrInvalid
		}
	}
	return t.Status == TaskPending && !dueAt(t).After(now) && down.Status == TaskSent && (previous == nil || previous.Status != TaskPending), nil
}

// CancelReminder prepares recovery's non-attempt cancellation. Receipts, attempt
// count and last-attempt fields survive; B2 must persist this with recovery.
func CancelReminder(t Task, i Incident) (Task, error) {
	if validateReminderRecovery(t, i) != nil {
		return Task{}, ErrInvalid
	}
	if t.Status != TaskPending {
		return t, nil
	}
	t.Status, t.LastErrorCode, t.NextAttemptAt = TaskSkipped, ErrorIncidentRecovered, time.Time{}
	t.FinishedAt = i.RecoveredAt.UTC()
	if t.LastAttemptAt.After(t.FinishedAt) {
		t.FinishedAt = t.LastAttemptAt
	}
	return t, validateTask(t)
}

func validateReminderRecovery(t Task, i Incident) error {
	if t.ValidateReminder(i) != nil || i.RecoveredAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

// CanceledByRecovery reconciles a completed send superseded by an atomic
// recovery cancellation, without claiming that its outcome was recorded. A
// validated closed incident and the unchanged pre-send attempt count are required.
func (t Task) CanceledByRecovery(i Incident, o Outcome) (bool, error) {
	if _, _, err := normalizeOutcome(o); err != nil {
		return false, err
	}
	if validateTask(t) != nil {
		return false, ErrInvalid
	}
	if t.Type != Reminder || t.Status != TaskSkipped || t.LastErrorCode != ErrorIncidentRecovered || t.EventID != o.EventID || t.Attempts != o.ExpectedAttempts || o.AttemptedAt.Before(t.OccurredAt) || o.AttemptedAt.Before(t.LastAttemptAt) {
		return false, nil
	}
	for _, receipt := range t.AttemptReceipts {
		if receipt.AttemptID == o.AttemptID {
			return false, nil
		}
	}
	if err := validateReminderRecovery(t, i); err != nil {
		return false, err
	}
	at := i.RecoveredAt
	if t.LastAttemptAt.After(at) {
		at = t.LastAttemptAt
	}
	return at.Equal(t.FinishedAt), nil
}

// PrepareDeliveryOutcome is pure conditional delivery logic for B2's atomic
// transaction. Dependencies must be read in that transaction; this neither
// claims a task nor writes a Schema v1 record.
func PrepareDeliveryOutcome(t Task, i Incident, down, previous *Task, o Outcome) (Task, bool, error) {
	ready, err := DeliveryReady(t, i, down, previous, o.AttemptedAt)
	if err != nil {
		return Task{}, false, err
	}
	return applyOutcome(t, o, ready)
}
