package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// TaskStatus describes delivery persistence, independently of incident state.
type TaskStatus string

const (
	TaskPending TaskStatus = "PENDING"
	TaskSent    TaskStatus = "SENT"
	TaskFailed  TaskStatus = "FAILED"
	TaskSkipped TaskStatus = "SKIPPED"
)

// ErrorCode is an allowlisted classification, never an underlying error or
// response body. Adapters must classify errors before calling RecordOutcome.
type ErrorCode string

const (
	ErrorNetwork           ErrorCode = "network"
	ErrorTimeout           ErrorCode = "timeout"
	ErrorRateLimited       ErrorCode = "rate_limited"
	ErrorRejected          ErrorCode = "rejected"
	ErrorUnavailable       ErrorCode = "unavailable"
	ErrorInvalidRequest    ErrorCode = "invalid_request"
	ErrorCanceled          ErrorCode = "canceled"
	ErrorInternal          ErrorCode = "internal"
	ErrorDependencyFailed  ErrorCode = "dependency_failed"
	ErrorDependencySkipped ErrorCode = "dependency_skipped"
	ErrorIncidentRecovered ErrorCode = "incident_recovered" // Non-attempt reminder cancellation only.
)

// MaxAttempts includes the first delivery attempt. MaxDuePageSize bounds both
// returned tasks and the number of scanned records evaluated per query.
const (
	MaxAttempts    = 10
	MaxDuePageSize = 100
)

// AttemptReceipt retains a bounded replay ledger (at most MaxAttempts entries).
// Digest covers the normalized outcome, including its ID and expected count.
// Keeping older receipts lets delayed duplicates return current state safely.
type AttemptReceipt struct {
	AttemptID string `json:"attempt_id"`
	Digest    string `json:"digest"`
}

// Outcome records one completed attempt, not a claim or permission to send.
// ExpectedAttempts is the count before this attempt; AttemptID must be unique
// within the task and reused verbatim when reconciling an uncertain commit.
// AttemptedAt is the completion time. PENDING requests retry; FAILED and SKIPPED
// are terminal dispositions. RetryAt is an optional later adapter retry bound.
type Outcome struct {
	EventID          string
	AttemptID        string
	ExpectedAttempts int
	AttemptedAt      time.Time
	Status           TaskStatus
	ErrorCode        ErrorCode
	RetryAt          time.Time
}

// DueQuery scans at most Limit records, which need not all be deliverable.
// Now must be explicit and fixed throughout a pass. Cursor is backend-specific
// and opaque; an empty cursor starts a pass. Limit must be 1..MaxDuePageSize.
type DueQuery struct {
	Now    time.Time
	Cursor string
	Limit  int
}

// TaskPage may be empty with a nonempty NextCursor; follow it to finish the pass.
// Pages are advisory, do not claim tasks, and are not a snapshot across calls.
// Concurrent writes can cause repeats/omissions; start subsequent passes at "".
type TaskPage struct {
	Tasks      []Task
	NextCursor string
}

// DeliveryStore conditionally updates outbox state without probing or sending.
// RecordOutcome atomically includes dependent recovery skips. On an uncertain
// result, read GetTask and compare AttemptReceipts, or replay the same Outcome.
// A caller must coordinate a single executor; this interface provides no leases
// or exactly-once external delivery, and a due page is not a reservation.
type DeliveryStore interface {
	GetTask(context.Context, string) (*Task, error)
	// ReadDelivery atomically rechecks the current task, incident and dependencies.
	// A nil task means it is no longer ready; it is not a reservation.
	ReadDelivery(context.Context, string, time.Time) (*Task, *Incident, error)
	QueryDue(context.Context, DueQuery) (TaskPage, error)
	RecordOutcome(context.Context, Outcome) (Task, error)
}

// HasOutcome verifies a read-back receipt after an uncertain commit, including
// an older attempt followed by newer outcomes. Read Task from GetTask first;
// false is not proof of rollback if the read itself failed or was unavailable.
func (t Task) HasOutcome(o Outcome) (bool, error) {
	o, digest, err := normalizeOutcome(o)
	if err != nil {
		return false, err
	}
	if err := validateTask(t); err != nil {
		return false, err
	}
	return t.EventID == o.EventID && o.ExpectedAttempts < len(t.AttemptReceipts) && t.AttemptReceipts[o.ExpectedAttempts] == (AttemptReceipt{o.AttemptID, digest}), nil
}

// RetryTime computes 10s * 2^(attempts-1), capped at 30 minutes. A later valid
// RetryAt can exceed that cap. The tenth attempt is exhausted (zero next time).
// No clock, jitter, I/O, or retry scheduling is hidden here.
func RetryTime(attempts int, at, retryAt time.Time) (time.Time, error) {
	at, retryAt = at.UTC(), retryAt.UTC()
	if attempts < 1 || attempts > MaxAttempts || !validTime(at) || (!retryAt.IsZero() && (!validTime(retryAt) || retryAt.Before(at))) {
		return time.Time{}, ErrInvalid
	}
	if attempts == MaxAttempts {
		return time.Time{}, nil
	}
	next := at.Add(min(10*time.Second*time.Duration(1<<uint(attempts-1)), 30*time.Minute))
	if retryAt.After(next) {
		next = retryAt
	}
	if !validTime(next) {
		return time.Time{}, ErrInvalid
	}
	return next.UTC(), nil
}

func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }

func validCode(code ErrorCode) bool {
	switch code {
	case ErrorNetwork, ErrorTimeout, ErrorRateLimited, ErrorRejected, ErrorUnavailable, ErrorInvalidRequest, ErrorCanceled, ErrorInternal, ErrorDependencyFailed, ErrorDependencySkipped, ErrorIncidentRecovered:
		return true
	}
	return false
}

func validateTask(t Task) error {
	id := eventID(t.IncidentID, t.Type, t.ChannelID)
	if t.Type == Reminder {
		var err error
		id, err = ReminderEventID(t.IncidentID, t.ReminderSequence, t.ChannelID)
		if err != nil {
			return err
		}
	} else if t.ReminderSequence != 0 {
		return ErrInvalid
	}
	if t.IncidentID == "" || !channelPattern.MatchString(t.ChannelID) || (t.Type != Down && t.Type != Recovered && t.Type != Reminder) || t.EventID != id || !validTime(t.OccurredAt) || t.Attempts < 0 || t.Attempts > MaxAttempts || len(t.AttemptReceipts) != t.Attempts {
		return ErrInvalid
	}
	canceled := t.Type == Reminder && t.Status == TaskSkipped && t.LastErrorCode == ErrorIncidentRecovered
	if t.LastErrorCode == ErrorIncidentRecovered && !canceled {
		return ErrInvalid
	}
	seen := make(map[string]bool, len(t.AttemptReceipts))
	for _, receipt := range t.AttemptReceipts {
		b, err := hex.DecodeString(receipt.Digest)
		if !channelPattern.MatchString(receipt.AttemptID) || err != nil || len(b) != sha256.Size || seen[receipt.AttemptID] {
			return ErrInvalid
		}
		seen[receipt.AttemptID] = true
	}
	if t.Attempts == 0 {
		if t.LastAttemptID != "" || !t.LastAttemptAt.IsZero() {
			return ErrInvalid
		}
	} else if t.LastAttemptID != t.AttemptReceipts[t.Attempts-1].AttemptID || !validTime(t.LastAttemptAt) || t.LastAttemptAt.Before(t.OccurredAt) {
		return ErrInvalid
	}
	switch t.Status {
	case TaskPending:
		if !t.FinishedAt.IsZero() || t.Attempts == MaxAttempts || (t.Attempts == 0 && (t.LastErrorCode != "" || !t.NextAttemptAt.IsZero())) || (t.Attempts > 0 && (!validCode(t.LastErrorCode) || !validTime(t.NextAttemptAt) || !t.NextAttemptAt.After(t.LastAttemptAt))) {
			return ErrInvalid
		}
	case TaskSent, TaskFailed, TaskSkipped:
		if !validTime(t.FinishedAt) || t.FinishedAt.Before(t.OccurredAt) || !t.NextAttemptAt.IsZero() || (t.Attempts > 0 && ((!canceled && !t.FinishedAt.Equal(t.LastAttemptAt)) || t.FinishedAt.Before(t.LastAttemptAt))) || (t.Status != TaskSkipped && t.Attempts == 0) || (t.Status == TaskSent && t.LastErrorCode != "") || (t.Status != TaskSent && !validCode(t.LastErrorCode)) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func normalizeOutcome(o Outcome) (Outcome, string, error) {
	o.AttemptedAt, o.RetryAt = o.AttemptedAt.UTC(), o.RetryAt.UTC()
	if o.EventID == "" || !channelPattern.MatchString(o.AttemptID) || o.ExpectedAttempts < 0 || o.ExpectedAttempts >= MaxAttempts || !validTime(o.AttemptedAt) || o.ErrorCode == ErrorIncidentRecovered {
		return o, "", ErrInvalid
	}
	switch o.Status {
	case TaskSent:
		if o.ErrorCode != "" {
			return o, "", ErrInvalid
		}
	case TaskPending, TaskFailed, TaskSkipped:
		if !validCode(o.ErrorCode) {
			return o, "", ErrInvalid
		}
	default:
		return o, "", ErrInvalid
	}
	if !o.RetryAt.IsZero() && (o.Status != TaskPending || !validTime(o.RetryAt) || o.RetryAt.Before(o.AttemptedAt)) {
		return o, "", ErrInvalid
	}
	data, err := json.Marshal(o)
	if err != nil {
		return o, "", ErrInvalid
	}
	sum := sha256.Sum256(data)
	return o, hex.EncodeToString(sum[:]), nil
}

func dueAt(t Task) time.Time {
	if t.NextAttemptAt.IsZero() {
		return t.OccurredAt
	}
	return t.NextAttemptAt
}

func deliverable(t Task, down *Task, now time.Time) bool {
	return t.Status == TaskPending && !dueAt(t).After(now) && (t.Type == Down || t.Type == Recovered && down != nil && down.Status == TaskSent)
}

// skipRecovery is used both when recovery is created after DOWN failed and when
// DOWN fails after recovery already exists. The same rule skips numbered
// reminders. Backends keep these writes atomic.
func skipRecovery(task Task, down *Task) (Task, error) {
	if (task.Type != Recovered && task.Type != Reminder) || down == nil || down.EventID != eventID(task.IncidentID, Down, task.ChannelID) {
		return Task{}, ErrInvalid
	}
	if task.Status == TaskPending && (down.Status == TaskFailed || down.Status == TaskSkipped) {
		task.Status, task.LastErrorCode = TaskSkipped, ErrorDependencyFailed
		if down.Status == TaskSkipped {
			task.LastErrorCode = ErrorDependencySkipped
		}
		task.NextAttemptAt = time.Time{}
		task.FinishedAt = down.FinishedAt
		if task.OccurredAt.After(task.FinishedAt) {
			task.FinishedAt = task.OccurredAt
		}
	}
	return task, validateTask(task)
}

func prepareOutcome(task Task, down *Task, o Outcome) (Task, bool, error) {
	return applyOutcome(task, o, deliverable(task, down, o.AttemptedAt))
}

func applyOutcome(task Task, o Outcome, ready bool) (Task, bool, error) {
	o, digest, err := normalizeOutcome(o)
	if err != nil {
		return Task{}, false, err
	}
	if task.EventID != o.EventID {
		return Task{}, false, ErrConflict
	}
	if o.ExpectedAttempts < len(task.AttemptReceipts) && task.AttemptReceipts[o.ExpectedAttempts] == (AttemptReceipt{o.AttemptID, digest}) {
		return task, false, nil
	}
	if task.Status != TaskPending || task.Attempts != o.ExpectedAttempts || !ready || o.AttemptedAt.Before(task.LastAttemptAt) {
		return Task{}, false, ErrConflict
	}
	for _, receipt := range task.AttemptReceipts {
		if receipt.AttemptID == o.AttemptID {
			return Task{}, false, ErrConflict
		}
	}
	task.Attempts++
	task.Status, task.LastAttemptAt, task.LastAttemptID, task.LastErrorCode = o.Status, o.AttemptedAt, o.AttemptID, o.ErrorCode
	task.AttemptReceipts = append(task.AttemptReceipts, AttemptReceipt{o.AttemptID, digest})
	if o.Status == TaskPending {
		task.NextAttemptAt, err = RetryTime(task.Attempts, o.AttemptedAt, o.RetryAt)
		if err != nil {
			return Task{}, false, err
		}
		if task.Attempts == MaxAttempts {
			task.Status = TaskFailed
		}
	}
	// A previously pending retry has a schedule that terminal outcomes clear.
	if task.Status != TaskPending {
		task.FinishedAt = o.AttemptedAt
		task.NextAttemptAt = time.Time{}
	}
	return task, true, validateTask(task)
}

func validateQuery(q DueQuery) error {
	if !validTime(q.Now) || q.Limit < 1 || q.Limit > MaxDuePageSize {
		return ErrInvalid
	}
	return nil
}
