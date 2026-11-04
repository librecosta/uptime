package notify

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/librecosta/uptime/internal/alerts"
)

var testNow = time.Unix(1599360473, 0).UTC()

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func message(kind alerts.EventType) Message {
	i := alerts.Incident{ID: "incident-1", EndpointID: "api", FirstFailureAt: testNow.Add(-3 * time.Minute), ConfirmedAt: testNow.Add(-2 * time.Minute), ChannelIDs: []string{"ops"}}
	at := i.ConfirmedAt
	if kind == alerts.Recovered {
		i.RecoveredAt = testNow.Add(-time.Minute)
		at = i.RecoveredAt
	}
	sum := sha256.Sum256([]byte(i.ID + "\x00" + string(kind) + "\x00ops"))
	return Message{Task: alerts.Task{EventID: hex.EncodeToString(sum[:]), IncidentID: i.ID, ChannelID: "ops", Type: kind, OccurredAt: at, Status: alerts.TaskPending}, Incident: i, EndpointName: "Public API"}
}

func options(rt http.RoundTripper) Options {
	return Options{Client: &http.Client{Transport: rt}, Now: func() time.Time { return testNow }}
}

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func checkResult(t *testing.T, got Result, err error, status alerts.TaskStatus, code alerts.ErrorCode) {
	t.Helper()
	if err != nil || got.Status != status || got.ErrorCode != code || !got.AttemptedAt.Equal(testNow) {
		t.Fatalf("Send = %+v, %v; want %s/%s at fixed clock", got, err, status, code)
	}
}

func TestWebhookMessages(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	var ids []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("incorrect request method/headers")
		}
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		ids = append(ids, r.Header.Get("X-LibreCosta-Event-ID"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender, err := NewWebhook(WebhookConfig{URL: server.URL + "/private?key=sensitive", BearerToken: "test-token"}, options(nil))
	if err != nil {
		t.Fatal(err)
	}
	down := message(alerts.Down)
	// Equivalent local-zone times must serialize in UTC. Closing an incident must
	// not change the payload of a DOWN that is being retried later.
	down.Incident.FirstFailureAt = down.Incident.FirstFailureAt.In(time.FixedZone("local", -7*3600))
	for range 2 {
		got, err := sender.Send(t.Context(), down)
		checkResult(t, got, err, alerts.TaskSent, "")
		down.Incident.RecoveredAt = testNow
	}
	recovered := message(alerts.Recovered)
	got, err := sender.Send(t.Context(), recovered)
	checkResult(t, got, err, alerts.TaskSent, "")
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(bodies[0], bodies[1]) || ids[0] != down.Task.EventID || ids[0] != ids[1] || ids[2] != recovered.Task.EventID {
		t.Fatal("event identity/payload changed on repeat")
	}
	for idx, body := range bodies {
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["schema_version"] != float64(1) || fields["incident_id"] != down.Incident.ID || fields["endpoint_id"] != "api" || fields["endpoint_name"] != "Public API" || fields["first_failure_at"] != down.Incident.FirstFailureAt.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("incorrect payload: %s", body)
		}
		_, hasRecovery := fields["recovered_at"]
		if hasRecovery != (idx == 2) || fields["event_type"] != string([]alerts.EventType{alerts.Down, alerts.Down, alerts.Recovered}[idx]) {
			t.Fatal("incorrect event/recovery fields")
		}
		if strings.Contains(string(body), "test-token") || strings.Contains(string(body), "sensitive") {
			t.Fatal("payload contains configuration")
		}
	}
}

func TestWebhookHTTPClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   alerts.TaskStatus
		code   alerts.ErrorCode
	}{
		{200, alerts.TaskSent, ""}, {204, alerts.TaskSent, ""}, {299, alerts.TaskSent, ""},
		{408, alerts.TaskPending, alerts.ErrorTimeout}, {429, alerts.TaskPending, alerts.ErrorRateLimited},
		{500, alerts.TaskPending, alerts.ErrorUnavailable}, {503, alerts.TaskPending, alerts.ErrorUnavailable},
		{400, alerts.TaskFailed, alerts.ErrorRejected}, {401, alerts.TaskFailed, alerts.ErrorRejected}, {404, alerts.TaskFailed, alerts.ErrorRejected},
		{301, alerts.TaskFailed, alerts.ErrorRejected}, {302, alerts.TaskFailed, alerts.ErrorRejected}, {307, alerts.TaskFailed, alerts.ErrorRejected}, {308, alerts.TaskFailed, alerts.ErrorRejected},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			calls := 0
			s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid/hook"}, options(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.GetBody != nil {
					t.Error("POST must not be replayable by Transport")
				}
				return reply(tc.status, ""), nil
			})))
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Send(t.Context(), message(alerts.Down))
			checkResult(t, got, err, tc.want, tc.code)
			if calls != 1 {
				t.Fatal("implicit retries")
			}
		})
	}
}

func TestWebhookRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		delay  time.Duration
	}{
		{"seconds", []string{"60"}, time.Minute}, {"date", []string{testNow.Add(2 * time.Hour).Format(http.TimeFormat)}, 2 * time.Hour},
		{"upper-bound", []string{"86400"}, 24 * time.Hour}, {"zero", []string{"0"}, 0}, {"negative", []string{"-1"}, 0},
		{"far-seconds", []string{"86401"}, 0}, {"far-date", []string{testNow.Add(25 * time.Hour).Format(http.TimeFormat)}, 0},
		{"past", []string{testNow.Add(-time.Minute).Format(http.TimeFormat)}, 0}, {"invalid", []string{"invalid"}, 0},
		{"overflow", []string{"999999999999999999999"}, 0}, {"duplicate", []string{"60", "120"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid"}, options(roundTripFunc(func(*http.Request) (*http.Response, error) {
				r := reply(429, "")
				r.Header["Retry-After"] = tc.values
				return r, nil
			})))
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Send(t.Context(), message(alerts.Down))
			checkResult(t, got, err, alerts.TaskPending, alerts.ErrorRateLimited)
			want := time.Time{}
			if tc.delay > 0 {
				want = testNow.Add(tc.delay)
			}
			if !got.RetryAt.Equal(want) {
				t.Fatalf("RetryAt = %v, want %v", got.RetryAt, want)
			}
		})
	}
}

func TestWebhookNoRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { t.Error("borrowed redirect callback called"); return nil }}
	s, err := NewWebhook(WebhookConfig{URL: source.URL, BearerToken: "private-token"}, Options{Client: client, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Send(t.Context(), message(alerts.Down))
	checkResult(t, got, err, alerts.TaskFailed, alerts.ErrorRejected)
	if targetCalls.Load() != 0 || client.Timeout != 0 || client.CheckRedirect == nil {
		t.Fatal("redirect followed or client mutated")
	}
}

func TestAdapterValidation(t *testing.T) {
	for _, raw := range []string{"", "://bad", "ftp://host/path", "https://user:secret@host/hook", "https://host/hook#fragment", "https://host:", "https://host:0", "https://host:65536"} {
		if _, err := NewWebhook(WebhookConfig{URL: raw}, Options{}); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("invalid URL not rejected safely: %v", err)
		}
	}
	for _, raw := range []string{"http://example.invalid/hook?private=secret", "http://localhost.example.invalid", "http://192.168.1.1"} {
		_, err := NewWebhook(WebhookConfig{URL: raw, BearerToken: "private-token"}, Options{})
		if !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), "private") {
			t.Fatal("insecure credential destination/error")
		}
	}
	for _, raw := range []string{"http://localhost", "http://127.0.0.1:12345", "http://[::1]:12345", "https://example.invalid"} {
		if _, err := NewWebhook(WebhookConfig{URL: raw, BearerToken: "token-._~+/=="}, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, token := range []string{"token\r\nInjected: true", "with space", strings.Repeat("x", 4097)} {
		if _, err := NewWebhook(WebhookConfig{URL: "https://example.invalid", BearerToken: token}, Options{}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("bad bearer accepted")
		}
	}
	for _, opts := range []Options{{Timeout: -1}, {Timeout: 31 * time.Second}, {Client: &http.Client{Timeout: -1}}, {Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}}} {
		if _, err := NewWebhook(WebhookConfig{URL: "https://example.invalid"}, opts); !errors.Is(err, ErrInvalidConfig) {
			t.Fatal("unsafe client configuration accepted")
		}
	}
	client := &http.Client{Timeout: time.Second}
	s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid"}, Options{Client: client})
	if err != nil || s.client.timeout != time.Second || client.Timeout != time.Second {
		t.Fatal("shorter client timeout not retained")
	}
}

func TestMessageValidation(t *testing.T) {
	changes := map[string]func(*Message){
		"incident":              func(m *Message) { m.Task.IncidentID = "other-secret" },
		"event":                 func(m *Message) { m.Task.EventID = "wrong" },
		"channel":               func(m *Message) { m.Incident.ChannelIDs = []string{"other"} },
		"type":                  func(m *Message) { m.Task.Type = "OTHER" },
		"terminal":              func(m *Message) { m.Task.Status = alerts.TaskSent },
		"exhausted":             func(m *Message) { m.Task.Attempts = alerts.MaxAttempts },
		"finished":              func(m *Message) { m.Task.FinishedAt = testNow },
		"zero-time":             func(m *Message) { m.Incident.FirstFailureAt = time.Time{} },
		"first-after-confirmed": func(m *Message) { m.Incident.FirstFailureAt = testNow },
		"occurrence":            func(m *Message) { m.Task.OccurredAt = testNow },
		"recovery":              func(m *Message) { m.Incident.RecoveredAt = m.Incident.FirstFailureAt },
		"last-attempt":          func(m *Message) { m.Task.LastAttemptAt = m.Incident.FirstFailureAt },
		"next-attempt":          func(m *Message) { m.Task.NextAttemptAt = m.Incident.FirstFailureAt },
		"control-id":            func(m *Message) { m.Incident.EndpointID = "api\nsecret" },
		"invalid-text":          func(m *Message) { m.EndpointName = "\xff" },
		"oversize":              func(m *Message) { m.EndpointName = strings.Repeat("<", maxRequestBytes/2) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid"}, options(roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid input sent"); return nil, nil })))
			if err != nil {
				t.Fatal(err)
			}
			m := message(alerts.Down)
			change(&m)
			got, err := s.Send(t.Context(), m)
			if !errors.Is(err, ErrInvalidMessage) || got != (Result{}) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe validation result: %+v, %v", got, err)
			}
		})
	}
}

type trackedBody struct {
	io.Reader
	closed  bool
	onClose func()
}

func (b *trackedBody) Close() error {
	b.closed = true
	if b.onClose != nil {
		b.onClose()
	}
	return nil
}

func TestSendFailuresAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport roundTripFunc
		want      alerts.ErrorCode
	}{
		{"network", func(*http.Request) (*http.Response, error) {
			return nil, errors.New("sensitive URL https://host/secret?token=private")
		}, alerts.ErrorNetwork},
		{"timeout", func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }, alerts.ErrorTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options(tc.transport)
			opts.Timeout = time.Millisecond
			s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid/secret?token=private"}, opts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Send(t.Context(), message(alerts.Down))
			checkResult(t, got, err, alerts.TaskPending, tc.want)
			if strings.Contains(fmt.Sprint(got, err), "private") || strings.Contains(fmt.Sprint(got, err), "secret") {
				t.Fatal("HTTP error leaked")
			}
		})
	}
	for _, alreadyCanceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if alreadyCanceled {
			cancel()
		}
		calls := 0
		s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid"}, options(roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; cancel(); return nil, context.Canceled })))
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Send(ctx, message(alerts.Down))
		cancel()
		if !errors.Is(err, context.Canceled) || got != (Result{}) || (alreadyCanceled && calls != 0) {
			t.Fatal("cancellation treated as completed delivery")
		}
	}
	// A completed response is still a fact if shutdown races with body Close.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := &trackedBody{Reader: strings.NewReader("ok"), onClose: cancel}
	s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid"}, options(roundTripFunc(func(*http.Request) (*http.Response, error) { r := reply(200, ""); r.Body = body; return r, nil })))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Send(ctx, message(alerts.Down))
	checkResult(t, got, err, alerts.TaskSent, "")
	if !body.closed {
		t.Fatal("body not closed")
	}
}

func TestResponseLimitAndTLS(t *testing.T) {
	body := &trackedBody{Reader: strings.NewReader(strings.Repeat("x", maxResponseBytes+1))}
	s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid"}, options(roundTripFunc(func(*http.Request) (*http.Response, error) { r := reply(200, ""); r.Body = body; return r, nil })))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Send(t.Context(), message(alerts.Down))
	checkResult(t, got, err, alerts.TaskFailed, alerts.ErrorRejected)
	if !body.closed {
		t.Fatal("oversized body not closed")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	for _, trusted := range []bool{false, true} {
		opts := options(nil)
		if trusted {
			opts.Client = server.Client()
		}
		s, err := NewWebhook(WebhookConfig{URL: server.URL}, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Send(t.Context(), message(alerts.Down))
		if trusted {
			checkResult(t, got, err, alerts.TaskSent, "")
		} else {
			checkResult(t, got, err, alerts.TaskFailed, alerts.ErrorRejected)
		}
	}
}

func TestSenderConcurrentUse(t *testing.T) {
	var calls atomic.Int32
	s, err := NewWebhook(WebhookConfig{URL: "https://example.invalid"}, options(roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return reply(200, ""), nil })))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			got, err := s.Send(t.Context(), message(alerts.Down))
			checkResult(t, got, err, alerts.TaskSent, "")
		})
	}
	wg.Wait()
	if calls.Load() != 16 {
		t.Fatal("incorrect attempt count")
	}
}
