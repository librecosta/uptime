package notify

import (
	"context"
	"time"

	"github.com/librecosta/uptime/internal/alerts"
)

// WebhookConfig contains a destination and optional Bearer token. Treat both
// fields as sensitive; do not log or dump configuration.
type WebhookConfig struct{ URL, BearerToken string }

// Webhook sends schema v1 JSON over HTTP(S), without following redirects.
type Webhook struct {
	client     senderClient
	url, token string
}

var _ Sender = (*Webhook)(nil)

// NewWebhook validates configuration without network I/O. Bearer credentials
// require HTTPS except for literal loopback addresses or localhost.
func NewWebhook(cfg WebhookConfig, opts Options) (*Webhook, error) {
	u, err := destination(cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.BearerToken != "" && (len(cfg.BearerToken) > 4096 || !bearerToken.MatchString(cfg.BearerToken) || u.Scheme != "https" && !localHost(u.Hostname())) {
		return nil, ErrInvalidConfig
	}
	client, err := newClient(opts)
	if err != nil {
		return nil, err
	}
	return &Webhook{client: client, url: u.String(), token: cfg.BearerToken}, nil
}

type webhookPayload struct {
	SchemaVersion  int              `json:"schema_version"`
	EventID        string           `json:"event_id"`
	IncidentID     string           `json:"incident_id"`
	EventType      alerts.EventType `json:"event_type"`
	EndpointID     string           `json:"endpoint_id"`
	EndpointName   string           `json:"endpoint_name"`
	OccurredAt     time.Time        `json:"occurred_at"`
	FirstFailureAt time.Time        `json:"first_failure_at"`
	ConfirmedAt    time.Time        `json:"confirmed_at"`
	RecoveredAt    *time.Time       `json:"recovered_at,omitempty"`
}

// Send reports one attempt. Even for a now-closed incident, a delayed DOWN
// retains its original payload and omits recovery-only fields.
func (s *Webhook) Send(ctx context.Context, m Message) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := validateMessage(m); err != nil {
		return Result{}, err
	}
	i, t := m.Incident, m.Task
	payload := webhookPayload{SchemaVersion: 1, EventID: t.EventID, IncidentID: i.ID, EventType: t.Type, EndpointID: i.EndpointID, EndpointName: displayName(m), OccurredAt: t.OccurredAt.UTC(), FirstFailureAt: i.FirstFailureAt.UTC(), ConfirmedAt: i.ConfirmedAt.UTC()}
	if t.Type == alerts.Recovered {
		at := i.RecoveredAt.UTC()
		payload.RecoveredAt = &at
	}
	body, err := marshal(payload, maxRequestBytes)
	if err != nil {
		return Result{}, err
	}
	r, err := s.client.post(ctx, s.url, body, t.EventID, s.token)
	if err != nil {
		return s.client.failure(ctx, err)
	}
	return httpResult(r, s.client.now()), nil
}
