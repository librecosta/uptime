// Package notify builds and sends individual alert notifications. It owns no
// persistence, scheduling, attempt IDs, or retry loops. Inputs must not be mutated
// during Send; injected transports and clocks must support concurrent use.
package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/librecosta/uptime/internal/alerts"
)

const (
	defaultTimeout   = 10 * time.Second
	maxTimeout       = 30 * time.Second
	maxRequestBytes  = 64 * 1024
	maxResponseBytes = 64 * 1024
	maxRetryAfter    = 24 * time.Hour
)

var (
	// ErrInvalidConfig never includes a destination or credential value.
	ErrInvalidConfig = errors.New("notify: invalid adapter configuration")
	// ErrInvalidMessage denotes validation/build failure before any HTTP attempt.
	ErrInvalidMessage = errors.New("notify: invalid notification message")
	errResponseLimit  = errors.New("notify: response size limit exceeded")
	channelID         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	bearerToken       = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)
)

// Message contains persisted facts and an optional display name. It contains no
// endpoint URL, headers, or channel credentials. An empty name falls back to ID.
type Message struct {
	Task         alerts.Task
	Incident     alerts.Incident
	EndpointName string
}

// Result classifies one HTTP attempt using the persisted outcome vocabulary.
// PENDING means retryable; the caller supplies attempt IDs/counts and persists it.
type Result struct {
	Status      alerts.TaskStatus
	ErrorCode   alerts.ErrorCode
	AttemptedAt time.Time // Completion time, in UTC.
	RetryAt     time.Time // Optional provider lower bound; C1 applies ordinary backoff.
}

// Sender performs at most one HTTP attempt. Validation failures and parent
// cancellation return an error and zero Result; HTTP/transport outcomes return
// classified Results without raw errors. A completed response survives subsequent
// cancellation. An interrupted attempt may still have reached the receiver;
// this interface does not promise exactly-once delivery.
type Sender interface {
	Send(context.Context, Message) (Result, error)
}

// Options injects a client, completion/signing clock, and total request timeout.
// Zero timeout means 10s; positive values may not exceed 30s. A shorter client
// timeout is retained. The client is not mutated, redirects and cookies are
// disabled, and TLS verification is retained. Custom transports are trusted:
// they must honor contexts and must not add retries, redirects or sensitive logs.
type Options struct {
	Client  *http.Client
	Now     func() time.Time
	Timeout time.Duration
}

type senderClient struct {
	http    *http.Client
	now     func() time.Time
	timeout time.Duration
}

func newClient(opts Options) (senderClient, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < 0 || timeout > maxTimeout {
		return senderClient{}, ErrInvalidConfig
	}
	var transport http.RoundTripper = http.DefaultTransport
	if opts.Client != nil {
		if opts.Client.Timeout < 0 {
			return senderClient{}, ErrInvalidConfig
		}
		if opts.Client.Timeout > 0 {
			timeout = min(timeout, opts.Client.Timeout)
		}
		if opts.Client.Transport != nil {
			transport = opts.Client.Transport
		}
	}
	if t, ok := transport.(*http.Transport); ok {
		if t.TLSClientConfig != nil && t.TLSClientConfig.InsecureSkipVerify {
			return senderClient{}, ErrInvalidConfig
		}
		t = t.Clone()
		if t.MaxResponseHeaderBytes <= 0 || t.MaxResponseHeaderBytes > maxResponseBytes {
			t.MaxResponseHeaderBytes = maxResponseBytes
		}
		transport = t
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return senderClient{http: &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: now, timeout: timeout}, nil
}

func destination(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || strings.Contains(raw, "#") || u.Opaque != "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return nil, ErrInvalidConfig
	}
	if port := u.Port(); port != "" {
		p, err := strconv.ParseUint(port, 10, 16)
		if err != nil || p == 0 {
			return nil, ErrInvalidConfig
		}
	}
	return u, nil
}

func localHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func validTime(t time.Time) bool {
	t = t.UTC()
	return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999
}

func validText(s string, required bool) bool {
	return (!required || s != "") && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

func validateMessage(m Message) error {
	t, i := m.Task, m.Incident
	if t.Status != alerts.TaskPending || t.Attempts < 0 || t.Attempts >= alerts.MaxAttempts || !t.FinishedAt.IsZero() || !validText(i.ID, true) || !validText(i.EndpointID, true) || !validText(m.EndpointName, false) || len(i.ID)+len(i.EndpointID)+len(m.EndpointName) > maxRequestBytes || t.IncidentID != i.ID || !channelID.MatchString(t.ChannelID) || !slices.Contains(i.ChannelIDs, t.ChannelID) {
		return ErrInvalidMessage
	}
	if !validTime(i.FirstFailureAt) || !validTime(i.ConfirmedAt) || i.ConfirmedAt.Before(i.FirstFailureAt) || !validTime(t.OccurredAt) || (!i.RecoveredAt.IsZero() && (!validTime(i.RecoveredAt) || i.RecoveredAt.Before(i.ConfirmedAt))) {
		return ErrInvalidMessage
	}
	if (!t.LastAttemptAt.IsZero() && (!validTime(t.LastAttemptAt) || t.LastAttemptAt.Before(t.OccurredAt))) || (!t.NextAttemptAt.IsZero() && (!validTime(t.NextAttemptAt) || t.NextAttemptAt.Before(t.OccurredAt) || (!t.LastAttemptAt.IsZero() && !t.NextAttemptAt.After(t.LastAttemptAt)))) {
		return ErrInvalidMessage
	}
	switch t.Type {
	case alerts.Down:
		if t.ReminderSequence != 0 || !t.OccurredAt.Equal(i.ConfirmedAt) {
			return ErrInvalidMessage
		}
	case alerts.Recovered:
		if t.ReminderSequence != 0 || i.RecoveredAt.IsZero() || !t.OccurredAt.Equal(i.RecoveredAt) {
			return ErrInvalidMessage
		}
	case alerts.Reminder:
		if t.ValidateReminder(i) != nil || !i.RecoveredAt.IsZero() {
			return ErrInvalidMessage
		}
		return nil
	default:
		return ErrInvalidMessage
	}
	// Check the frozen C1 identity without assigning or replacing an EventID.
	sum := sha256.Sum256([]byte(i.ID + "\x00" + string(t.Type) + "\x00" + t.ChannelID))
	if t.EventID != hex.EncodeToString(sum[:]) {
		return ErrInvalidMessage
	}
	return nil
}

func displayName(m Message) string {
	if m.EndpointName != "" {
		return m.EndpointName
	}
	return m.Incident.EndpointID
}

func marshal(v any, limit int) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil || len(data) > limit {
		return nil, ErrInvalidMessage
	}
	return data, nil
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (c senderClient) post(ctx context.Context, target string, body []byte, eventID, token string) (response, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	// A replayable body can make Transport retry a POST after a reused-connection
	// failure. Keep this single attempt non-replayable; the caller owns retries.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	if eventID != "" {
		req.Header.Set("X-LibreCosta-Event-ID", eventID)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return response{}, err
	}
	if len(data) > maxResponseBytes {
		return response{}, errResponseLimit
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func (c senderClient) failure(ctx context.Context, err error) (Result, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return Result{}, context.Canceled
	}
	result := Result{Status: alerts.TaskPending, ErrorCode: alerts.ErrorNetwork, AttemptedAt: c.now().UTC()}
	var netErr net.Error
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var invalidCert x509.CertificateInvalidError
	var hostname x509.HostnameError
	switch {
	case errors.Is(err, errResponseLimit):
		result.Status, result.ErrorCode = alerts.TaskFailed, alerts.ErrorRejected
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		result.ErrorCode = alerts.ErrorTimeout
	case errors.As(err, &certErr) || errors.As(err, &unknownCA) || errors.As(err, &invalidCert) || errors.As(err, &hostname):
		result.Status, result.ErrorCode = alerts.TaskFailed, alerts.ErrorRejected
	}
	return result, nil // Never return or log the URL-bearing underlying error.
}

func httpResult(r response, now time.Time) Result {
	result := Result{Status: alerts.TaskFailed, ErrorCode: alerts.ErrorRejected, AttemptedAt: now.UTC()}
	switch {
	case r.status >= 200 && r.status < 300:
		result.Status, result.ErrorCode = alerts.TaskSent, ""
	case r.status == 408:
		result.Status, result.ErrorCode = alerts.TaskPending, alerts.ErrorTimeout
	case r.status == 429:
		result.Status, result.ErrorCode = alerts.TaskPending, alerts.ErrorRateLimited
	case r.status >= 500 && r.status <= 599:
		result.Status, result.ErrorCode = alerts.TaskPending, alerts.ErrorUnavailable
	}
	if result.Status == alerts.TaskPending {
		result.RetryAt = retryAfter(r.header, now)
	}
	return result
}

func retryAfter(header http.Header, now time.Time) time.Time {
	values := header.Values("Retry-After")
	if len(values) != 1 || !validTime(now) {
		return time.Time{}
	}
	value := strings.TrimSpace(values[0])
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		if seconds > 0 && seconds <= uint64(maxRetryAfter/time.Second) {
			return now.Add(time.Duration(seconds) * time.Second).UTC()
		}
		return time.Time{}
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) && !at.After(now.Add(maxRetryAfter)) {
		return at.UTC()
	}
	return time.Time{}
}
