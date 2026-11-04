package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3/log"
	"github.com/librecosta/uptime/internal/alerts"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/internal/notify"
)

type deliverySender func(context.Context, notify.Message) (notify.Result, error)

func (f deliverySender) Send(ctx context.Context, m notify.Message) (notify.Result, error) {
	return f(ctx, m)
}

var deliveryTime = time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)

func pendingIncident(t *testing.T, store alerts.Store, id, endpoint string, channels ...string) alerts.Incident {
	t.Helper()
	i := alerts.Incident{ID: id, EndpointID: endpoint, FirstFailureAt: deliveryTime.Add(-time.Minute), ConfirmedAt: deliveryTime, ChannelIDs: channels}
	_, err := store.Apply(t.Context(), alerts.Transition{Type: alerts.Down, Incident: i})
	must(t, err)
	return i
}

func workerForTest(t *testing.T, store alerts.Store) *deliveryWorker {
	t.Helper()
	d, err := newDeliveryWorker(store, config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Second}})
	must(t, err)
	d.now = func() time.Time { return deliveryTime }
	d.warn = func(string, string, alerts.ErrorCode) {}
	t.Cleanup(d.closeSenders)
	return d
}

func taskForTest(t *testing.T, store alerts.Store, kind alerts.EventType, channel string) alerts.Task {
	t.Helper()
	tasks, err := store.Outbox(t.Context())
	must(t, err)
	for _, task := range tasks {
		if task.Type == kind && task.ChannelID == channel {
			return task
		}
	}
	t.Fatal("task missing")
	return alerts.Task{}
}

func TestDeliveryBackoffAndExhaustion(t *testing.T) {
	for _, code := range []alerts.ErrorCode{alerts.ErrorNetwork, alerts.ErrorTimeout, alerts.ErrorRateLimited, alerts.ErrorUnavailable, alerts.ErrorRejected} {
		t.Run(string(code), func(t *testing.T) {
			store := testAlertBbolt(t)
			i := pendingIncident(t, store, "backoff", "api", "ops")
			i.RecoveredAt = deliveryTime
			_, err := store.Apply(t.Context(), alerts.Transition{Type: alerts.Recovered, Incident: i})
			must(t, err)
			d := workerForTest(t, store)
			now := deliveryTime
			calls := 0
			d.now = func() time.Time { return now }
			d.senders["ops"] = deliverySender(func(_ context.Context, m notify.Message) (notify.Result, error) {
				calls++
				if m.Task.Type != alerts.Down {
					t.Error("recovery sent before DOWN success")
				}
				status := alerts.TaskPending
				if code == alerts.ErrorRejected {
					status = alerts.TaskFailed
				}
				return notify.Result{Status: status, ErrorCode: code, AttemptedAt: now}, nil
			})
			for n := 1; n <= alerts.MaxAttempts; n++ {
				d.scan(t.Context())
				task := taskForTest(t, store, alerts.Down, "ops")
				if task.Attempts != n || calls != n {
					t.Fatal("one send did not produce one attempt")
				}
				if code == alerts.ErrorRejected || n == alerts.MaxAttempts {
					if task.Status != alerts.TaskFailed || taskForTest(t, store, alerts.Recovered, "ops").Status != alerts.TaskSkipped {
						t.Fatal("terminal failure/dependency skip missing")
					}
					break
				}
				want, err := alerts.RetryTime(n, now, time.Time{})
				must(t, err)
				if !task.NextAttemptAt.Equal(want) {
					t.Fatal("runtime changed C1 backoff")
				}
				d.scan(t.Context())
				if calls != n {
					t.Fatal("retry sent early")
				}
				now = want
			}
			d.scan(t.Context())
			final := taskForTest(t, store, alerts.Down, "ops")
			if calls != final.Attempts {
				t.Fatal("terminal task was retried")
			}
		})
	}
}

type deliveryFaultStore struct {
	*alerts.BboltStore
	before, after, read atomic.Bool
	query               func(context.Context, alerts.DueQuery) (alerts.TaskPage, error)
	receipts            []alerts.Outcome // Synchronous test-worker access only.
}

func (s *deliveryFaultStore) RecordOutcome(ctx context.Context, o alerts.Outcome) (alerts.Task, error) {
	s.receipts = append(s.receipts, o)
	if s.before.Load() {
		return alerts.Task{}, errors.New("private storage URL")
	}
	t, err := s.BboltStore.RecordOutcome(ctx, o)
	if err == nil && s.after.Load() {
		return alerts.Task{}, errors.New("private lost response")
	}
	return t, err
}
func (s *deliveryFaultStore) GetTask(ctx context.Context, id string) (*alerts.Task, error) {
	if s.read.Load() {
		return nil, errors.New("private storage credentials")
	}
	return s.BboltStore.GetTask(ctx, id)
}
func (s *deliveryFaultStore) QueryDue(ctx context.Context, q alerts.DueQuery) (alerts.TaskPage, error) {
	if s.query != nil {
		return s.query(ctx, q)
	}
	return s.BboltStore.QueryDue(ctx, q)
}

func TestDeliveryUncertainOutcomeFencesSending(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			store := &deliveryFaultStore{BboltStore: testAlertBbolt(t)}
			pendingIncident(t, store, "uncertain", "retired", "ops")
			d := workerForTest(t, store)
			calls := 0
			d.senders["ops"] = deliverySender(func(context.Context, notify.Message) (notify.Result, error) {
				calls++
				store.before.Store(!committed)
				store.after.Store(committed)
				store.read.Store(true)
				return notify.Result{Status: alerts.TaskSent, AttemptedAt: deliveryTime}, nil
			})
			d.scan(t.Context())
			d.scan(t.Context())
			if d.pending == nil || calls != 1 {
				t.Fatal("uncertain outcome allowed another send")
			}
			store.before.Store(false)
			store.after.Store(false)
			store.read.Store(false)
			d.scan(t.Context())
			if d.pending != nil || calls != 1 || taskForTest(t, store, alerts.Down, "ops").Attempts != 1 {
				t.Fatal("reconciliation duplicated or lost the attempt")
			}
			for _, o := range store.receipts {
				if o != store.receipts[0] {
					t.Fatal("reconciliation changed Outcome or AttemptID")
				}
			}
		})
	}
	// A readable matching receipt reconciles the first lost reply immediately.
	store := &deliveryFaultStore{BboltStore: testAlertBbolt(t)}
	pendingIncident(t, store, "readback", "api", "ops")
	store.after.Store(true)
	d := workerForTest(t, store)
	d.senders["ops"] = deliverySender(func(context.Context, notify.Message) (notify.Result, error) {
		return notify.Result{Status: alerts.TaskSent, AttemptedAt: deliveryTime}, nil
	})
	d.scan(t.Context())
	if d.pending != nil || len(store.receipts) != 1 {
		t.Fatal("matching receipt not recognized")
	}
}

func TestDeliveryEmptyPagesAndBoundedPass(t *testing.T) {
	store := &deliveryFaultStore{BboltStore: testAlertBbolt(t)}
	pendingIncident(t, store, "pages", "api", "ops")
	task := taskForTest(t, store, alerts.Down, "ops")
	queries, calls := 0, 0
	store.query = func(_ context.Context, q alerts.DueQuery) (alerts.TaskPage, error) {
		if q.Limit != deliveryPageSize || !q.Now.Equal(deliveryTime) {
			t.Error("pass changed time or bounds")
		}
		if queries > 0 && q.Cursor != fmt.Sprint(queries) {
			t.Error("cursor not followed")
		}
		queries++
		if queries == deliveryPagesPerTick+2 {
			return alerts.TaskPage{Tasks: []alerts.Task{task, task}}, nil
		}
		return alerts.TaskPage{NextCursor: fmt.Sprint(queries)}, nil
	}
	d := workerForTest(t, store)
	d.senders["ops"] = deliverySender(func(context.Context, notify.Message) (notify.Result, error) {
		calls++
		return notify.Result{Status: alerts.TaskSent, AttemptedAt: deliveryTime.Add(time.Second)}, nil
	})
	d.scan(t.Context())
	if queries != deliveryPagesPerTick || d.cursor == "" || d.passAt.IsZero() {
		t.Fatal("unbounded pass or empty page stopped scan")
	}
	d.now = func() time.Time { return deliveryTime.Add(time.Second) }
	d.scan(t.Context())
	if queries != deliveryPagesPerTick+2 || calls != 1 || d.cursor != "" || !d.passAt.IsZero() {
		t.Fatal("resumed pass failed or duplicate page sent twice")
	}
}

func TestDeliveryRoutingSnapshotsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.db")
	store, err := alerts.OpenBbolt(path, time.Second)
	must(t, err)
	i := pendingIncident(t, store, "historical", "removed", "old")
	must(t, store.Close())
	store, err = alerts.OpenBbolt(path, time.Second)
	must(t, err)
	defer store.Close()
	d := workerForTest(t, store)
	calls := 0
	warnings := 0
	d.warn = func(event, ch string, code alerts.ErrorCode) {
		if ch != "old" || event == "" || code != alerts.ErrorUnavailable {
			t.Error("unsafe missing-channel classification")
		}
		warnings++
	}
	d.senders["new"] = deliverySender(func(context.Context, notify.Message) (notify.Result, error) {
		t.Error("historical task rerouted")
		return notify.Result{}, nil
	})
	d.scan(t.Context())
	d.scan(t.Context())
	if warnings != 1 || taskForTest(t, store, alerts.Down, "old").Attempts != 0 {
		t.Fatal("missing channel dropped task or log not limited")
	}
	d.senders["old"] = deliverySender(func(_ context.Context, m notify.Message) (notify.Result, error) {
		calls++
		if m.Incident.EndpointID != "removed" || m.EndpointName != "" {
			t.Error("deleted endpoint lost identity/name fallback")
		}
		return notify.Result{Status: alerts.TaskSent, AttemptedAt: deliveryTime}, nil
	})
	d.scan(t.Context())
	engine, err := alerts.NewEngine(store, nil)
	must(t, err)
	_, err = engine.Observe(t.Context(), alerts.Observation{EndpointID: i.EndpointID, CheckedAt: deliveryTime, Success: true}, alerts.Policy{ChannelIDs: []string{"new"}})
	must(t, err)
	d.scan(t.Context())
	if calls != 2 || taskForTest(t, store, alerts.Recovered, "old").Status != alerts.TaskSent {
		t.Fatal("restart/recovery changed snapshot or delivery order")
	}
}

func TestDeliveryFeishuBotLimit(t *testing.T) {
	store := testAlertBbolt(t)
	pendingIncident(t, store, "bots", "api", "a", "b")
	cfg := config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Second}, Alerts: config.AlertsConfig{Channels: []config.AlertChannelConfig{
		{ID: "a", Type: "feishu", Feishu: &notify.FeishuConfig{URL: "https://open.feishu.cn/open-apis/bot/v2/hook/private"}},
		{ID: "b", Type: "feishu", Feishu: &notify.FeishuConfig{URL: "https://open.feishu.cn:443/open-apis/bot/v2/hook/private"}},
	}}}
	d, err := newDeliveryWorker(store, cfg)
	must(t, err)
	defer d.closeSenders()
	now := deliveryTime
	d.now = func() time.Time { return now }
	calls := 0
	for _, id := range []string{"a", "b"} {
		d.senders[id] = deliverySender(func(context.Context, notify.Message) (notify.Result, error) {
			calls++
			return notify.Result{Status: alerts.TaskSent, AttemptedAt: now}, nil
		})
	}
	d.scan(t.Context())
	d.scan(t.Context())
	if calls != 1 {
		t.Fatal("same bot burst across channel IDs")
	}
	now = now.Add(feishuSendInterval)
	d.scan(t.Context())
	if calls != 2 {
		t.Fatal("bot did not become eligible")
	}
}

func TestDeliveryCancellationAndCloseOrder(t *testing.T) {
	store := testAlertBbolt(t)
	pendingIncident(t, store, "cancel", "api", "ops")
	entered, exited := make(chan struct{}), make(chan struct{})
	var writes atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(exited)
	}))
	defer webhook.Close()
	cfg := config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Second}, Alerts: config.AlertsConfig{Channels: []config.AlertChannelConfig{{ID: "ops", Type: "webhook", Webhook: &notify.WebhookConfig{URL: webhook.URL}}}}}
	var runtime *alertRuntime
	runtime, err := newAlertRuntime(store, func() error {
		select {
		case <-runtime.delivery.done:
		default:
			t.Error("storage closed before delivery worker")
		}
		select {
		case <-runtime.done:
		default:
			t.Error("storage closed before ingress drained")
		}
		writes.Add(1)
		return nil
	}, cfg, 2)
	must(t, err)
	defer runtime.stop(time.Second)
	select {
	case <-entered:
	case <-time.After(testDeadline):
		t.Fatal("delivery did not start")
	}
	must(t, runtime.stop(time.Second))
	if writes.Load() != 1 || taskForTest(t, store, alerts.Down, "ops").Attempts != 0 {
		t.Fatal("shutdown cancellation recorded an attempt")
	}
	// Remote HTTP handler exit is separate from local worker joining.
	select {
	case <-exited:
	case <-time.After(testDeadline):
		t.Fatal("canceled request remained open")
	}
}

func TestDeliveryCompletedSendSurvivesCancellation(t *testing.T) {
	store := testAlertBbolt(t)
	pendingIncident(t, store, "completed", "api", "ops")
	d := workerForTest(t, store)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.senders["ops"] = deliverySender(func(context.Context, notify.Message) (notify.Result, error) {
		cancel() // A response completed just as shutdown canceled the HTTP context.
		return notify.Result{Status: alerts.TaskSent, AttemptedAt: deliveryTime}, nil
	})
	d.scan(ctx)
	task := taskForTest(t, store, alerts.Down, "ops")
	if task.Status != alerts.TaskSent || task.Attempts != 1 || d.pending != nil {
		t.Fatal("completed send lost during shutdown")
	}
}

func TestDeliveryBotRetryAfterGate(t *testing.T) {
	store := testAlertBbolt(t)
	pendingIncident(t, store, "cooldown", "api", "a", "b")
	d := workerForTest(t, store)
	now := deliveryTime
	d.now = func() time.Time { return now }
	calls := 0
	for _, id := range []string{"a", "b"} {
		d.botKeys[id] = "shared-bot"
		d.senders[id] = deliverySender(func(context.Context, notify.Message) (notify.Result, error) {
			calls++
			return notify.Result{Status: alerts.TaskPending, ErrorCode: alerts.ErrorRateLimited, AttemptedAt: now, RetryAt: now.Add(time.Minute)}, nil
		})
	}
	d.scan(t.Context())
	now = now.Add(59 * time.Second)
	d.scan(t.Context())
	if calls != 1 {
		t.Fatal("provider cooldown bypassed through another channel")
	}
	now = now.Add(time.Second)
	d.scan(t.Context())
	if calls != 2 {
		t.Fatal("provider cooldown never expired")
	}
}

// exerciseAlertDelivery uses real endpoint probes, real HTTP sends, and whichever
// alert persistence the runtime opens. Redis calls this same contract.
func exerciseAlertDelivery(t *testing.T, cfg config.Config, success *atomic.Bool) {
	t.Helper()
	var mu sync.Mutex
	var events []string
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			EventID      string `json:"event_id"`
			EventType    string `json:"event_type"`
			EndpointName string `json:"endpoint_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p.EventID != r.Header.Get("X-LibreCosta-Event-ID") || p.EndpointName != cfg.Endpoints[0].Name {
			t.Error("incorrect notification metadata")
		}
		mu.Lock()
		events = append(events, p.EventType)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer webhook.Close()
	cfg.Alerts = config.AlertsConfig{Enabled: true, ConsecutiveFailures: 3, Channels: []config.AlertChannelConfig{{ID: "ops", Type: "webhook", Webhook: &notify.WebhookConfig{URL: webhook.URL}}}}
	cfg.Endpoints[0].AlertChannels = []string{"ops"}
	s, err := New(cfg)
	must(t, err)
	r := startServer(t, s)
	var incident *alerts.Incident
	poll(t, func() bool {
		incident, err = s.alerts.store.Active(t.Context(), cfg.Endpoints[0].ID)
		must(t, err)
		if incident == nil {
			return false
		}
		tasks, err := s.alerts.store.Outbox(t.Context())
		must(t, err)
		return len(tasks) == 1 && tasks[0].Status == alerts.TaskSent
	})
	if len(incident.ChannelIDs) != 1 || incident.ChannelIDs[0] != "ops" {
		t.Fatal("runtime did not snapshot route")
	}
	r.health(t, "/readyz", 200, "ready\n")
	if _, ok := r.snapshot(cfg.UI.Path); !ok {
		t.Fatal("dashboard unavailable")
	}
	success.Store(true)
	poll(t, func() bool {
		tasks, err := s.alerts.store.Outbox(t.Context())
		must(t, err)
		if len(tasks) != 2 {
			return false
		}
		return tasks[0].Status == alerts.TaskSent && tasks[1].Status == alerts.TaskSent
	})
	mu.Lock()
	correct := strings.Join(events, ",") == "DOWN,RECOVERED"
	mu.Unlock()
	if !correct {
		t.Fatal("delivery order/incident deduplication")
	}
	r.cancel()
	must(t, r.wait(t))
}

func TestAlertDeliveryEndpointIntegration(t *testing.T) {
	var success atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !success.Load() {
			w.WriteHeader(500)
		}
	}))
	defer target.Close()
	exerciseAlertDelivery(t, testConfig(t, target.URL), &success)
}

func TestDeliveryFailureDoesNotAffectMonitoring(t *testing.T) {
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer webhook.Close()
	var probes atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { probes.Add(1); w.WriteHeader(500) }))
	defer target.Close()
	cfg := testConfig(t, target.URL)
	cfg.Alerts = config.AlertsConfig{Enabled: true, ConsecutiveFailures: 1, Channels: []config.AlertChannelConfig{{ID: "ops", Type: "webhook", Webhook: &notify.WebhookConfig{URL: webhook.URL}}}}
	cfg.Endpoints[0].AlertChannels = []string{"ops"}
	s, err := New(cfg)
	must(t, err)
	r := startServer(t, s)
	poll(t, func() bool {
		tasks, err := s.alerts.store.Outbox(t.Context())
		must(t, err)
		return len(tasks) == 1 && tasks[0].Attempts == 1 && tasks[0].Status == alerts.TaskPending && tasks[0].LastErrorCode == alerts.ErrorUnavailable
	})
	r.health(t, "/readyz", 200, "ready\n")
	r.health(t, "/livez", 200, "ok\n")
	poll(t, func() bool { return probes.Load() >= 3 })
	response, err := r.client.Get(r.base + cfg.UI.Path)
	must(t, err)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Type"), "text/html") || len(body) == 0 {
		t.Fatal("notification failure broke dashboard HTML")
	}
	if _, ok := r.snapshot(cfg.UI.Path); !ok {
		t.Fatal("notification failure broke dashboard")
	}
	r.cancel()
	must(t, r.wait(t))
}

func TestAlertDisabledHasNoDeliverySideEffects(t *testing.T) {
	var calls atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer webhook.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer target.Close()
	cfg := testConfig(t, target.URL)
	// Even a caller-provided inactive branch cannot allocate notification resources.
	cfg.Alerts.Channels = []config.AlertChannelConfig{{ID: "ops", Type: "webhook", Webhook: &notify.WebhookConfig{URL: webhook.URL}}}
	s, err := New(cfg)
	must(t, err)
	s.deps.openAlerts = func(context.Context, *runtimeStorage, config.Config) (alerts.Store, func() error, error) {
		t.Error("disabled alerts opened persistence")
		return nil, nil, errors.New("unexpected open")
	}
	r := startServer(t, s)
	r.health(t, "/readyz", 200, "ready\n")
	r.cancel()
	must(t, r.wait(t))
	if s.alerts != nil || calls.Load() != 0 {
		t.Fatal("disabled notifications started")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.Storage.Bbolt.Path), "alerts.db")); !os.IsNotExist(err) {
		t.Fatal("disabled alerts created a database")
	}
}

type deliveryTransport func(*http.Request) (*http.Response, error)

func (f deliveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeliveryLogsExcludeCredentials(t *testing.T) {
	const token = "private-hook-token"
	const bearer = "private-bearer-token"
	const secret = "private-feishu-secret"
	for _, adapter := range []string{"webhook", "feishu"} {
		t.Run(adapter, func(t *testing.T) {
			var output bytes.Buffer
			log.SetOutput(&output)
			defer log.SetOutput(os.Stderr)
			store := testAlertBbolt(t)
			pendingIncident(t, store, "safe-log", "api", "ops")
			d, err := newDeliveryWorker(store, config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Second}})
			must(t, err)
			d.now = func() time.Time { return deliveryTime }
			opts := notify.Options{Now: d.now, Client: &http.Client{Transport: deliveryTransport(func(r *http.Request) (*http.Response, error) {
				// Real HTTP errors can include the destination; never log them verbatim.
				return nil, fmt.Errorf("transport failure: %s %s %s", r.URL, bearer, secret)
			})}}
			if adapter == "webhook" {
				sender, err := notify.NewWebhook(notify.WebhookConfig{URL: "https://example.invalid/hook?token=" + token, BearerToken: bearer}, opts)
				must(t, err)
				defer sender.CloseIdleConnections()
				d.senders["ops"] = sender
			} else {
				sender, err := notify.NewFeishu(notify.FeishuConfig{URL: "https://open.feishu.cn/open-apis/bot/v2/hook/" + token, Secret: secret}, opts)
				must(t, err)
				defer sender.CloseIdleConnections()
				d.senders["ops"] = sender
			}
			d.scan(t.Context())
			task := taskForTest(t, store, alerts.Down, "ops")
			text := output.String()
			if task.LastErrorCode != alerts.ErrorNetwork || !strings.Contains(text, task.EventID) || !strings.Contains(text, `channel="ops"`) || !strings.Contains(text, "code=network") {
				t.Fatal("delivery warning omitted safe identifiers/classification")
			}
			for _, value := range []string{token, bearer, secret, "https://", "transport failure"} {
				if strings.Contains(text, value) {
					t.Fatal("delivery log exposed destination or credentials")
				}
			}
		})
	}
}

func TestDeliveryPersistsAdapterClassifications(t *testing.T) {
	for _, status := range []int{204, 408, 429, 503, 401, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			store := testAlertBbolt(t)
			pendingIncident(t, store, "http-result", "api", "ops")
			d := workerForTest(t, store)
			sender, err := notify.NewWebhook(notify.WebhookConfig{URL: "https://example.invalid/private-url"}, notify.Options{Now: func() time.Time { return deliveryTime }, Client: &http.Client{Transport: deliveryTransport(func(*http.Request) (*http.Response, error) {
				if status == 0 {
					return nil, context.DeadlineExceeded
				}
				h := make(http.Header)
				h.Set("Retry-After", "60")
				return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("private-response"))}, nil
			})}})
			must(t, err)
			defer sender.CloseIdleConnections()
			d.senders["ops"] = sender
			d.scan(t.Context())
			task := taskForTest(t, store, alerts.Down, "ops")
			want, code := alerts.TaskPending, alerts.ErrorTimeout
			switch status {
			case 204:
				want, code = alerts.TaskSent, ""
			case 429:
				code = alerts.ErrorRateLimited
			case 503:
				code = alerts.ErrorUnavailable
			case 401:
				want, code = alerts.TaskFailed, alerts.ErrorRejected
			}
			if task.Status != want || task.LastErrorCode != code || task.Attempts != 1 {
				t.Fatal("HTTP result was not persisted faithfully")
			}
			if status == 429 && !task.NextAttemptAt.Equal(deliveryTime.Add(time.Minute)) {
				t.Fatal("provider retry bound lost")
			}
		})
	}
}

func TestAlertEndpointPolicies(t *testing.T) {
	store := testAlertBbolt(t)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); w.WriteHeader(204) }))
	defer webhook.Close()
	cfg := config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Second}, Alerts: config.AlertsConfig{Enabled: true, ConsecutiveFailures: 1, RecoverySuccesses: 3}}
	for _, id := range []string{"a", "b"} {
		cfg.Alerts.Channels = append(cfg.Alerts.Channels, config.AlertChannelConfig{ID: id, Type: "webhook", Webhook: &notify.WebhookConfig{URL: webhook.URL}})
	}
	cfg.Endpoints = []config.EndpointConfig{{ID: "all", AlertChannels: []string{"a", "b"}}, {ID: "quiet", AlertChannels: []string{}}, {ID: "selected", AlertChannels: []string{"b"}}}
	r, err := newAlertRuntime(store, func() error { return nil }, cfg, 16)
	must(t, err)
	var wg sync.WaitGroup
	for _, e := range cfg.Endpoints {
		wg.Go(func() { r.hook(t.Context(), alertProbe(e.ID, 1, false)) })
	}
	wg.Wait()
	for _, e := range cfg.Endpoints {
		r.hook(t.Context(), alertProbe(e.ID, 2, true))
		r.hook(t.Context(), alertProbe(e.ID, 3, true))
	}
	must(t, r.stop(time.Second))
	for _, e := range cfg.Endpoints {
		i, err := store.Active(t.Context(), e.ID)
		must(t, err)
		if i == nil || strings.Join(i.ChannelIDs, ",") != strings.Join(e.AlertChannels, ",") {
			t.Fatal("endpoint policy changed channel snapshot")
		}
	}
}

// This fixture models B2's atomic recovery cancellation without inserting any
// future reminder record into a real Schema v1 backend.
type reminderCancelStore struct {
	alerts.Store
	mu                     sync.Mutex
	incident               alerts.Incident
	task                   alerts.Task
	readTask, readIncident bool
	outcomes               []alerts.Outcome
}

func (s *reminderCancelStore) Get(context.Context, string) (*alerts.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readIncident {
		return nil, errors.New("unavailable")
	}
	i := s.incident
	return &i, nil
}
func (s *reminderCancelStore) GetTask(context.Context, string) (*alerts.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readTask {
		return nil, errors.New("unavailable")
	}
	task := s.task
	return &task, nil
}
func (s *reminderCancelStore) QueryDue(context.Context, alerts.DueQuery) (alerts.TaskPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return alerts.TaskPage{Tasks: []alerts.Task{s.task}}, nil
}
func (s *reminderCancelStore) RecordOutcome(_ context.Context, o alerts.Outcome) (alerts.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = append(s.outcomes, o)
	return alerts.Task{}, alerts.ErrConflict
}

func TestDeliveryReminderCanceledDuringSend(t *testing.T) {
	for _, fault := range []string{"none", "task-read", "incident-read"} {
		t.Run(fault, func(t *testing.T) {
			i := alerts.Incident{ID: "canceled", EndpointID: "api", FirstFailureAt: deliveryTime.Add(-3 * time.Hour), ConfirmedAt: deliveryTime.Add(-2 * time.Hour), ChannelIDs: []string{"ops"}, ReminderPolicy: &alerts.ReminderPolicy{Interval: time.Hour, MaxReminders: 2}, NextReminderAt: deliveryTime.Add(-time.Hour)}
			sum := sha256.Sum256([]byte(i.ID + "\x00DOWN\x00ops"))
			down := alerts.Task{EventID: hex.EncodeToString(sum[:]), IncidentID: i.ID, ChannelID: "ops", Type: alerts.Down, OccurredAt: i.ConfirmedAt, Status: alerts.TaskPending}
			down, _, err := alerts.PrepareDeliveryOutcome(down, i, nil, nil, alerts.Outcome{EventID: down.EventID, AttemptID: "down", AttemptedAt: i.ConfirmedAt, Status: alerts.TaskSent})
			must(t, err)
			i, tasks, err := alerts.PrepareReminder(i, alerts.Observation{EndpointID: i.EndpointID, CheckedAt: i.NextReminderAt})
			must(t, err)
			old := alerts.Outcome{EventID: tasks[0].EventID, AttemptID: "prior", AttemptedAt: tasks[0].OccurredAt, Status: alerts.TaskPending, ErrorCode: alerts.ErrorUnavailable}
			pending, _, err := alerts.PrepareDeliveryOutcome(tasks[0], i, &down, nil, old)
			must(t, err)
			store := &reminderCancelStore{incident: i, task: pending}
			d := workerForTest(t, store)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			d.senders["ops"] = deliverySender(func(context.Context, notify.Message) (notify.Result, error) {
				calls.Add(1)
				close(entered)
				<-release
				return notify.Result{Status: alerts.TaskSent, AttemptedAt: deliveryTime}, nil
			})
			go func() { defer close(done); d.scan(t.Context()) }()
			<-entered
			store.mu.Lock()
			store.incident.RecoveredAt = deliveryTime.Add(-time.Second)
			store.incident.NextReminderAt = time.Time{}
			store.task, err = alerts.CancelReminder(store.task, store.incident)
			store.readTask, store.readIncident = fault == "task-read", fault == "incident-read"
			store.mu.Unlock()
			must(t, err)
			close(release)
			<-done
			if fault != "none" {
				if d.pending == nil {
					t.Fatal("unknown state released reconciliation fence")
				}
				d.scan(t.Context())
				if calls.Load() != 1 || d.pending == nil {
					t.Fatal("unknown result permitted resend")
				}
				store.mu.Lock()
				store.readTask, store.readIncident = false, false
				store.mu.Unlock()
				d.scan(t.Context())
			}
			if d.pending != nil || calls.Load() != 1 {
				t.Fatal("legal cancellation blocked or repeated send")
			}
			if store.task.Status != alerts.TaskSkipped || store.task.Attempts != 1 || store.task.LastAttemptID != "prior" {
				t.Fatal("late send overwrote cancellation/history")
			}
			matched, err := store.task.HasOutcome(old)
			must(t, err)
			if !matched {
				t.Fatal("prior receipt lost")
			}
			for _, o := range store.outcomes {
				if o != store.outcomes[0] {
					t.Fatal("reconciliation changed attempt identity")
				}
			}
		})
	}
}

func TestDeliveryRestartResumesPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.db")
	store, err := alerts.OpenBbolt(path, time.Second)
	must(t, err)
	i := pendingIncident(t, store, "restart-pending", "retired", "ops")
	must(t, store.Close())
	store, err = alerts.OpenBbolt(path, time.Second)
	must(t, err)
	defer store.Close()
	var calls atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			EndpointName string `json:"endpoint_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p.EndpointName != "retired" {
			t.Error("historical endpoint name did not fall back to ID")
		}
		calls.Add(1)
		w.WriteHeader(204)
	}))
	defer webhook.Close()
	cfg := config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Second}, Alerts: config.AlertsConfig{Enabled: true, Channels: []config.AlertChannelConfig{{ID: "ops", Type: "webhook", Webhook: &notify.WebhookConfig{URL: webhook.URL}}}}}
	r, err := newAlertRuntime(store, func() error { return nil }, cfg, 16)
	must(t, err)
	defer r.stop(time.Second)
	poll(t, func() bool { return taskForTest(t, store, alerts.Down, "ops").Status == alerts.TaskSent })
	must(t, r.stop(time.Second))
	active, err := store.Active(t.Context(), "retired")
	must(t, err)
	if calls.Load() != 1 || active == nil || active.ID != i.ID {
		t.Fatal("restart duplicated incident or attempt")
	}
}
