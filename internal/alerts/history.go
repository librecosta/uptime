package alerts

import (
	"context"
	"encoding/base64"
	"encoding/hex"
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
