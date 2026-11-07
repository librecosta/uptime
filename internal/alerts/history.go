package alerts

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HistoryQuery uses an opaque exclusive keyset cursor. Zero Limit selects 20;
// at most 50 records are returned. Pages are not a cross-call snapshot.
type HistoryQuery struct {
	Limit  int
	Cursor string
}

// IncidentPage is ordered by descending confirmation time, then descending ID.
type IncidentPage struct {
	Incidents  []Incident
	NextCursor string
}

// HistoryStore reads persisted incidents without probing or changing delivery.
type HistoryStore interface {
	ListIncidents(context.Context, HistoryQuery) (IncidentPage, error)
	ReadIncident(context.Context, DetailQuery) (IncidentDetail, error)
}

// Validate checks user input without opening storage. Stored corruption must not
// be confused with a malformed request when both otherwise return ErrInvalid.
func (q HistoryQuery) Validate() error {
	_, _, err := historyQuery(q)
	return err
}

// DetailQuery pages derived task identities, never an outbox scan. Zero Limit
// selects 20; at most 50 tasks are read. Cursor is bound to the incident ID.
type DetailQuery struct {
	IncidentID string
	Cursor     string
	Limit      int
}

// IncidentDetail is a consistent read of an incident and one bounded task page.
// Separate pages are not a snapshot. Event-major order appends new reminders and
// recovery without changing earlier positions; channels use the frozen order.
type IncidentDetail struct {
	Incident   Incident
	Tasks      []Task
	NextCursor string
}

var historyIncidentID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)

// Validate rejects malformed IDs, cursors and limits before any storage access.
func (q DetailQuery) Validate() error {
	_, _, err := detailQuery(q)
	return err
}

func detailQuery(q DetailQuery) (int, int, error) {
	if !historyIncidentID.MatchString(q.IncidentID) || len(q.Cursor) > 1024 {
		return 0, 0, ErrInvalid
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	if q.Limit < 1 || q.Limit > 50 {
		return 0, 0, ErrInvalid
	}
	if q.Cursor == "" {
		return q.Limit, 0, nil
	}
	if !strings.HasPrefix(q.Cursor, "v1.") {
		return 0, 0, ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(q.Cursor[3:])
	parts := strings.Split(string(data), "\x00")
	if err != nil || len(parts) != 2 || parts[0] != q.IncidentID {
		return 0, 0, ErrInvalid
	}
	offset, err := strconv.Atoi(parts[1])
	if err != nil || offset < 1 || detailCursor(q.IncidentID, offset) != q.Cursor {
		return 0, 0, ErrInvalid
	}
	return q.Limit, offset, nil
}

func detailCursor(id string, offset int) string {
	return "v1." + base64.RawURLEncoding.EncodeToString([]byte(id+"\x00"+strconv.Itoa(offset)))
}

// readIncidentDetail must run inside the backend's read transaction. A missing
// expected task is corruption, not an empty delivery page. Nothing is repaired.
func readIncidentDetail(i *Incident, q DetailQuery, read taskReader) (IncidentDetail, error) {
	limit, offset, err := detailQuery(q)
	if err != nil {
		return IncidentDetail{}, err
	}
	if i == nil {
		return IncidentDetail{}, ErrNotFound
	}
	if i.ID != q.IncidentID || validateIncident(*i) != nil {
		return IncidentDetail{}, ErrInvalid
	}
	page := IncidentDetail{Incident: *i, Tasks: []Task{}}
	events := i.ReminderCount + 1
	if !i.RecoveredAt.IsZero() {
		events++
	}
	if len(i.ChannelIDs) > math.MaxInt/events {
		return IncidentDetail{}, ErrInvalid
	}
	total := events * len(i.ChannelIDs)
	for pos := offset; pos < total && len(page.Tasks) < limit; pos++ {
		ordinal, channel := pos/len(i.ChannelIDs), i.ChannelIDs[pos%len(i.ChannelIDs)]
		kind, sequence := Down, 0
		if ordinal > 0 {
			kind, sequence = Reminder, ordinal
			if ordinal > i.ReminderCount {
				kind, sequence = Recovered, 0
			}
		}
		id := eventID(i.ID, kind, channel)
		if kind == Reminder {
			id, err = ReminderEventID(i.ID, sequence, channel)
		}
		if err != nil {
			return IncidentDetail{}, err
		}
		task, err := read(id)
		if err != nil {
			return IncidentDetail{}, err
		}
		if task == nil || task.EventID != id || task.ChannelID != channel || task.Type != kind || task.ReminderSequence != sequence || validateStoredTask(*task, i) != nil {
			return IncidentDetail{}, ErrInvalid
		}
		page.Tasks = append(page.Tasks, *task)
	}
	if len(page.Tasks) > 0 && offset+len(page.Tasks) < total {
		page.NextCursor = detailCursor(i.ID, offset+len(page.Tasks))
	}
	return page, nil
}

const historyTimeFormat = "2006-01-02T15:04:05.000000000Z"

func historyKey(i Incident) (string, error) {
	at := i.ConfirmedAt.UTC()
	if !validTime(at) || i.ID == "" || len(i.ID) > 256 {
		return "", ErrInvalid
	}
	return at.Format(historyTimeFormat) + "\x00" + hex.EncodeToString([]byte(i.ID)), nil
}

func historyID(key string) (string, error) {
	parts := strings.Split(key, "\x00")
	if len(parts) != 2 || len(parts[1]) > 512 {
		return "", ErrInvalid
	}
	at, err := time.Parse(historyTimeFormat, parts[0])
	if err != nil {
		return "", ErrInvalid
	}
	id, err := hex.DecodeString(parts[1])
	if err != nil {
		return "", ErrInvalid
	}
	canonical, err := historyKey(Incident{ID: string(id), ConfirmedAt: at})
	if err != nil || canonical != key {
		return "", ErrInvalid
	}
	return string(id), nil
}

func historyQuery(q HistoryQuery) (int, string, error) {
	if q.Limit == 0 {
		q.Limit = 20
	}
	if q.Limit < 1 || q.Limit > 50 || len(q.Cursor) > 1024 {
		return 0, "", ErrInvalid
	}
	if q.Cursor == "" {
		return q.Limit, "", nil
	}
	if !strings.HasPrefix(q.Cursor, "v1.") {
		return 0, "", ErrInvalid
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(q.Cursor, "v1."))
	if err != nil || historyCursor(string(key)) != q.Cursor {
		return 0, "", ErrInvalid
	}
	if _, err := historyID(string(key)); err != nil {
		return 0, "", err
	}
	return q.Limit, string(key), nil
}

func historyCursor(key string) string {
	return "v1." + base64.RawURLEncoding.EncodeToString([]byte(key))
}

func indexedIncident(key string, i *Incident) error {
	id, err := historyID(key)
	if err != nil || i == nil || i.ID != id {
		return ErrInvalid
	}
	actual, err := historyKey(*i)
	if err != nil || actual != key {
		return ErrInvalid
	}
	return nil
}
