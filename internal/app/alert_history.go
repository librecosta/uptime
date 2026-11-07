package app

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"errors"
	"html/template"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/librecosta/uptime/internal/alerts"
)

//go:embed alert_history.html
var alertHistoryHTML string

type incidentView struct {
	ID              string    `json:"id"`
	EndpointID      string    `json:"endpoint_id"`
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	FirstFailureAt  time.Time `json:"first_failure_at"`
	ConfirmedAt     time.Time `json:"confirmed_at"`
	RecoveredAt     time.Time `json:"recovered_at,omitzero"`
	DurationSeconds int64     `json:"duration_seconds"`
	ReminderCount   int       `json:"reminder_count"`
}

type taskView struct {
	EventID          string            `json:"event_id"`
	ChannelID        string            `json:"channel_id"`
	Type             alerts.EventType  `json:"type"`
	ReminderSequence int               `json:"reminder_sequence,omitempty"`
	Status           alerts.TaskStatus `json:"status"`
	Attempts         int               `json:"attempts"`
	OccurredAt       time.Time         `json:"occurred_at"`
	LastAttemptAt    time.Time         `json:"last_attempt_at,omitzero"`
	NextAttemptAt    time.Time         `json:"next_attempt_at,omitzero"`
	FinishedAt       time.Time         `json:"finished_at,omitzero"`
	LastErrorCode    alerts.ErrorCode  `json:"last_error_code,omitempty"`
}

func historyTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format(time.RFC3339)
}

func historyDuration(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	if seconds >= 86400 {
		return strconv.FormatInt(seconds/86400, 10) + "d " + strconv.FormatInt(seconds%86400/3600, 10) + "h"
	}
	return d.String()
}

func incidentSummary(i alerts.Incident, names map[string]string, now time.Time) incidentView {
	name := names[i.EndpointID]
	if name == "" {
		name = i.EndpointID
	}
	status, end := "FIRING", now
	if !i.RecoveredAt.IsZero() {
		status, end = "RECOVERED", i.RecoveredAt
	}
	return incidentView{i.ID, i.EndpointID, name, status, i.FirstFailureAt.UTC(), i.ConfirmedAt.UTC(), i.RecoveredAt.UTC(), max(0, int64(end.Sub(i.FirstFailureAt)/time.Second)), i.ReminderCount}
}

// historyParameters rejects duplicate, unknown and noncanonical parameters. The
// stores validate cursor structure separately, before any I/O. Never echo input.
func historyParameters(c fiber.Ctx) (int, string, error) {
	raw := string(c.Request().URI().QueryString())
	if len(raw) > 2048 {
		return 0, "", alerts.ErrInvalid
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return 0, "", alerts.ErrInvalid
	}
	limit, cursor := 0, ""
	for key, items := range values {
		if len(items) != 1 || items[0] == "" {
			return 0, "", alerts.ErrInvalid
		}
		switch key {
		case "cursor":
			cursor = items[0]
		case "limit":
			limit, err = strconv.Atoi(items[0])
			if err != nil || limit < 1 || limit > 50 || strconv.Itoa(limit) != items[0] {
				return 0, "", alerts.ErrInvalid
			}
		default:
			return 0, "", alerts.ErrInvalid
		}
	}
	return limit, cursor, nil
}

// registerAlertHistory borrows the running alert store. Fiber drains handlers
// before alertRuntime closes it; no connection, worker or schema work is added.
func (s *Server) registerAlertHistory() error {
	if !s.cfg.Alerts.HistoryEnabled {
		return nil
	}
	if !s.cfg.Alerts.Enabled || s.cfg.Auth == nil || s.alerts == nil {
		return errors.New("alert history requires alerts and Basic Auth")
	}
	store, ok := s.alerts.store.(alerts.HistoryStore)
	if !ok {
		return errors.New("alert history storage is unavailable")
	}
	tmpl, err := template.New("alerts").Funcs(template.FuncMap{"utc": historyTime, "duration": historyDuration}).Parse(alertHistoryHTML)
	if err != nil {
		return errors.New("cannot initialize alert history page")
	}
	names := make(map[string]string, len(s.cfg.Endpoints))
	for _, endpoint := range s.cfg.Endpoints {
		names[endpoint.ID] = endpoint.Name
	}
	s.app.Use(func(c fiber.Ctx) error {
		path := c.Path()
		if !withinPath(path, "/alerts") {
			return c.Next()
		}
		c.Set("Cache-Control", "no-store")
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Referrer-Policy", "no-referrer")
		pageRequest := path == "/alerts" || path == "/alerts/"
		listRequest := path == "/alerts/api/incidents"
		detailRequest := strings.HasPrefix(path, "/alerts/api/incidents/")
		if !pageRequest && !listRequest && !detailRequest {
			return c.SendStatus(404)
		}
		if c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead {
			c.Set("Allow", "GET, HEAD")
			return c.SendStatus(405)
		}
		fail := func(status int, message string) error { return c.Status(status).JSON(fiber.Map{"error": message}) }
		limit, cursor, err := historyParameters(c)
		if err != nil {
			return fail(400, "Invalid history query.")
		}
		ctx, cancel := context.WithTimeout(c.Context(), s.cfg.Server.ShutdownTimeout)
		defer cancel()
		now := time.Now().UTC()
		if detailRequest {
			id := strings.TrimPrefix(path, "/alerts/api/incidents/")
			q := alerts.DetailQuery{IncidentID: id, Cursor: cursor, Limit: limit}
			if q.Validate() != nil {
				return fail(400, "Invalid history query.")
			}
			detail, err := store.ReadIncident(ctx, q)
			if errors.Is(err, alerts.ErrNotFound) {
				return fail(404, "Incident not found.")
			}
			if err != nil {
				return fail(503, "Alert history could not be read. Try again later.")
			}
			tasks := make([]taskView, 0, len(detail.Tasks))
			for _, t := range detail.Tasks {
				tasks = append(tasks, taskView{t.EventID, t.ChannelID, t.Type, t.ReminderSequence, t.Status, t.Attempts, t.OccurredAt.UTC(), t.LastAttemptAt.UTC(), t.NextAttemptAt.UTC(), t.FinishedAt.UTC(), t.LastErrorCode})
			}
			return c.JSON(fiber.Map{"incident": incidentSummary(detail.Incident, names, now), "tasks": tasks, "next_cursor": detail.NextCursor})
		}
		q := alerts.HistoryQuery{Limit: limit, Cursor: cursor}
		if q.Validate() != nil {
			return fail(400, "Invalid history query.")
		}
		page, readErr := store.ListIncidents(ctx, q)
		rows := make([]incidentView, 0, len(page.Incidents))
		for _, i := range page.Incidents {
			rows = append(rows, incidentSummary(i, names, now))
		}
		if listRequest {
			if readErr != nil {
				return fail(503, "Alert history could not be read. Try again later.")
			}
			return c.JSON(fiber.Map{"incidents": rows, "next_cursor": page.NextCursor})
		}
		message := ""
		if readErr != nil {
			rows = nil
			message = "Alert history could not be read. Try refreshing this page later."
			c.Status(503)
		}
		nonce := rand.Text()
		c.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		var body bytes.Buffer
		data := struct {
			Rows                                   []incidentView
			NextCursor, Message, Nonce, UptimePath string
			Monitor                                bool
		}{rows, page.NextCursor, message, nonce, s.cfg.UI.Path, s.cfg.Monitor.Enabled}
		if tmpl.Execute(&body, data) != nil {
			return fail(500, "Alert history is unavailable.")
		}
		c.Set("Content-Type", "text/html; charset=utf-8")
		return c.Send(body.Bytes())
	})
	return nil
}
