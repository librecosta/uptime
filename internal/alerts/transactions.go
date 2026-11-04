package alerts

import (
	"context"
	"slices"
	"time"
)

// ReminderChange identifies one conditional reminder issuance. Reuse the same
// count and time after an uncertain commit; sequence IDs never depend on retries.
type ReminderChange struct {
	IncidentID    string
	ExpectedCount int
	CheckedAt     time.Time
}

type taskReader func(string) (*Task, error)

func prepareReminderChange(i *Incident, activeID string, c ReminderChange, read taskReader) (Incident, []Task, error) {
	if i == nil {
		return Incident{}, nil, ErrNotFound
	}
	if c.IncidentID != i.ID || c.ExpectedCount < 0 || !validTime(c.CheckedAt) || i.ReminderPolicy == nil {
		return Incident{}, nil, ErrInvalid
	}
	if i.ReminderCount > c.ExpectedCount {
		// A later recovery or issuance may already exist. Verify the fixed event
		// identities rather than comparing mutable incident counters for equality.
		for _, channel := range i.ChannelIDs {
			id, _ := ReminderEventID(i.ID, c.ExpectedCount+1, channel)
			t, err := read(id)
			if err != nil {
				return Incident{}, nil, err
			}
			if t == nil || t.ValidateReminder(*i) != nil || !t.OccurredAt.Equal(c.CheckedAt) {
				return Incident{}, nil, ErrConflict
			}
		}
		return *i, nil, nil
	}
	if i.ReminderCount != c.ExpectedCount || activeID != i.ID || !i.RecoveredAt.IsZero() {
		return Incident{}, nil, ErrConflict
	}
	next, tasks, err := PrepareReminder(*i, Observation{EndpointID: i.EndpointID, CheckedAt: c.CheckedAt})
	if err != nil {
		return Incident{}, nil, err
	}
	if tasks == nil {
		return Incident{}, nil, ErrConflict
	}
	return next, tasks, nil
}

func (e *Engine) commitPending(ctx context.Context, p pendingTransition) (Incident, error) {
	if p.reminder == nil {
		return e.commit(ctx, p.change)
	}
	c := *p.reminder
	i, err := e.store.ApplyReminder(ctx, c)
	if err == nil {
		return i, nil
	}
	observed, readErr := e.store.Get(ctx, c.IncidentID)
	ds, ok := e.store.(DeliveryStore)
	if readErr == nil && observed != nil && observed.ReminderCount > c.ExpectedCount && ok {
		result, _, checkErr := prepareReminderChange(observed, "", c, func(id string) (*Task, error) { return ds.GetTask(ctx, id) })
		if checkErr == nil {
			return result, nil
		}
	}
	return Incident{}, err
}

func deliveryDependencies(t Task, i Incident, read taskReader) (*Task, *Task, error) {
	if t.Type == Down {
		return nil, nil, nil
	}
	down, err := read(eventID(i.ID, Down, t.ChannelID))
	if err != nil {
		return nil, nil, err
	}
	sequence := i.ReminderCount
	if t.Type == Reminder {
		sequence = t.ReminderSequence - 1
	}
	var previous *Task
	if sequence > 0 {
		id, _ := ReminderEventID(i.ID, sequence, t.ChannelID)
		previous, err = read(id)
	}
	return down, previous, err
}

// transitionTasks includes non-attempt cancellations in the same transaction as
// recovery. All reads occur before writes; backends commit this plan atomically.
func transitionTasks(i Incident, tasks []Task, read taskReader) ([]Task, error) {
	result := make([]Task, 0, len(tasks))
	for _, t := range tasks {
		if t.Type != Down {
			down, err := read(eventID(i.ID, Down, t.ChannelID))
			if err != nil {
				return nil, err
			}
			t, err = skipRecovery(t, down)
			if err != nil {
				return nil, err
			}
		}
		old, err := read(t.EventID)
		if err != nil {
			return nil, err
		}
		if old != nil {
			return nil, ErrConflict
		}
		result = append(result, t)
	}
	if !i.RecoveredAt.IsZero() {
		for _, channel := range i.ChannelIDs {
			for seq := 1; seq <= i.ReminderCount; seq++ {
				id, _ := ReminderEventID(i.ID, seq, channel)
				t, err := read(id)
				if err != nil {
					return nil, err
				}
				if t == nil {
					return nil, ErrInvalid
				}
				if t.Status == TaskPending {
					s, err := CancelReminder(*t, i)
					if err != nil {
						return nil, err
					}
					result = append(result, s)
				}
			}
		}
	}
	return result, nil
}

func outcomeTasks(t Task, i Incident, o Outcome, read taskReader) (Task, []Task, error) {
	down, previous, err := deliveryDependencies(t, i, read)
	if err != nil {
		return Task{}, nil, err
	}
	result, changed, err := PrepareDeliveryOutcome(t, i, down, previous, o)
	if err != nil || !changed {
		return result, nil, err
	}
	tasks := []Task{result}
	if result.Type == Down && (result.Status == TaskFailed || result.Status == TaskSkipped) {
		ids := []string{eventID(i.ID, Recovered, result.ChannelID)}
		for seq := 1; seq <= i.ReminderCount; seq++ {
			id, _ := ReminderEventID(i.ID, seq, result.ChannelID)
			ids = append(ids, id)
		}
		for _, id := range ids {
			dependent, err := read(id)
			if err != nil {
				return Task{}, nil, err
			}
			if dependent == nil && (id != eventID(i.ID, Recovered, result.ChannelID) || !i.RecoveredAt.IsZero()) {
				return Task{}, nil, ErrInvalid
			}
			if dependent != nil {
				skipped, err := skipRecovery(*dependent, &result)
				if err != nil {
					return Task{}, nil, err
				}
				tasks = append(tasks, skipped)
			}
		}
	}
	return result, tasks, nil
}

// Legacy validation rejects impossible Schema v1 contents before a migration
// commits. JSON is not rewritten, so existing receipts survive.
func validateLegacyIncident(i Incident) error {
	if validateIncident(i) != nil || i.ReminderPolicy != nil || i.ReminderCount != 0 || !i.NextReminderAt.IsZero() {
		return ErrInvalid
	}
	_, err := historyKey(i)
	return err
}

func validateLegacyLinks(i Incident, activeID string, read taskReader) error {
	if i.RecoveredAt.IsZero() && activeID != i.ID || !i.RecoveredAt.IsZero() && activeID == i.ID {
		return ErrInvalid
	}
	for _, ch := range i.ChannelIDs {
		down, err := read(eventID(i.ID, Down, ch))
		if err != nil {
			return err
		}
		if down == nil || validateStoredTask(*down, &i) != nil {
			return ErrInvalid
		}
		recovery, err := read(eventID(i.ID, Recovered, ch))
		if err != nil {
			return err
		}
		if i.RecoveredAt.IsZero() {
			if recovery != nil {
				return ErrInvalid
			}
		} else {
			if recovery == nil || validateStoredTask(*recovery, &i) != nil {
				return ErrInvalid
			}
			if (down.Status == TaskFailed || down.Status == TaskSkipped) && recovery.Status != TaskSkipped {
				return ErrInvalid
			}
		}
	}
	return nil
}

func validateStoredTask(t Task, i *Incident) error {
	if i == nil || validateTask(t) != nil || validateIncident(*i) != nil || t.IncidentID != i.ID || !slices.Contains(i.ChannelIDs, t.ChannelID) {
		return ErrInvalid
	}
	// Membership, event identity and times are checked even for terminal tasks.
	if t.Type == Reminder {
		return t.ValidateReminder(*i)
	}
	at := i.ConfirmedAt
	if t.Type == Recovered {
		at = i.RecoveredAt
	}
	if at.IsZero() || !t.OccurredAt.Equal(at) {
		return ErrInvalid
	}
	return nil
}
