package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/librecosta/uptime/internal/alerts"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/storage/bbolt"
)

const testDeadline = 8 * time.Second

func poll(t *testing.T, condition func() bool) {
	t.Helper()
	timer := time.NewTimer(testDeadline)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("condition not met before deadline")
		case <-ticker.C:
		}
	}
}

type runningServer struct {
	base   string
	client *http.Client
	cancel context.CancelFunc
	done   chan struct{}
	err    error // Read only after done closes.
}

func startServer(t *testing.T, s *Server) *runningServer {
	t.Helper()
	return startServerWithClient(t, s, &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}})
}

func startServerWithClient(t *testing.T, s *Server, client *http.Client) *runningServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &runningServer{cancel: cancel, done: make(chan struct{}), client: client}
	bound := make(chan string, 1)
	listen := s.deps.listen
	s.deps.listen = func(network, _ string) (net.Listener, error) {
		ln, err := listen(network, "127.0.0.1:0")
		if err == nil {
			scheme := "http://"
			if s.cfg.Server.TLS != nil {
				scheme = "https://"
			}
			bound <- scheme + ln.Addr().String()
		}
		return ln, err
	}
	go func() { r.err = s.Run(ctx); close(r.done) }()
	t.Cleanup(func() { cancel(); r.wait(t); r.client.CloseIdleConnections() })
	select {
	case r.base = <-bound:
	case <-r.done:
		t.Fatalf("startup failed: %v", r.err)
	case <-time.After(testDeadline):
		t.Fatal("listener startup deadline")
	}
	poll(t, func() bool {
		if !s.ready.Load() {
			return false
		}
		response, err := r.client.Get(r.base + "/livez")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == 200
	})
	return r
}

func (r *runningServer) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(testDeadline):
		t.Fatal("Run did not return before deadline")
		return nil
	}
}

func assertHealth(t *testing.T, response *http.Response, status int, body string) {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	if response.StatusCode != status || string(data) != body || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("health response: status=%d body=%q headers=%v", response.StatusCode, data, response.Header)
	}
}

func (r *runningServer) health(t *testing.T, path string, status int, body string) {
	t.Helper()
	response, err := r.client.Get(r.base + path)
	must(t, err)
	defer response.Body.Close()
	assertHealth(t, response, status, body)
}

func (r *runningServer) snapshot(path string) (uptime.StatusResponse, bool) {
	var snapshot uptime.StatusResponse
	response, err := r.client.Get(r.base + path + "/api/status")
	if err != nil {
		return snapshot, false
	}
	defer response.Body.Close()
	err = json.NewDecoder(response.Body).Decode(&snapshot)
	return snapshot, err == nil && response.StatusCode == 200
}

func TestEndpointIntegration(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		name := "up"
		if status != http.StatusOK {
			name = "down"
		}
		t.Run(name, func(t *testing.T) {
			var probes atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer PRIVATE" {
					t.Error("probe header mapping")
				}
				probes.Add(1)
				w.WriteHeader(status)
			}))
			defer target.Close()
			cfg := testConfig(t, target.URL+"/health?secret=PRIVATE")
			cfg.Endpoints[0].Headers = map[string]string{"Authorization": "Bearer PRIVATE"}
			s, err := New(cfg)
			must(t, err)
			r := startServer(t, s)
			poll(t, func() bool {
				v, ok := r.snapshot(cfg.UI.Path)
				return ok && probes.Load() > 0 && len(v.Services) == 1 && v.Services[0].ID == "target" && v.Services[0].CurrentStatus == name && v.Storage.Driver == "bbolt" && v.Storage.Status == "ok"
			})
			r.health(t, "/livez", 200, "ok\n")
			r.health(t, "/readyz", 200, "ready\n")
			for _, path := range []string{cfg.UI.Path, cfg.UI.Path + "/api/status", "/random"} {
				response, err := r.client.Get(r.base + path)
				must(t, err)
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				must(t, err)
				want := 200
				if path == "/random" {
					want = 404
				}
				if response.StatusCode != want || strings.Contains(string(data), "PRIVATE") {
					t.Fatalf("route %s: status=%d or secret exposed", path, response.StatusCode)
				}
				if path == cfg.UI.Path+"/api/status" {
					var snapshot uptime.StatusResponse
					must(t, json.Unmarshal(data, &snapshot))
					if len(snapshot.Services) != 1 || len(snapshot.Services[0].Daily) != int(cfg.Uptime.Window) || snapshot.SampleIntervalSeconds != 1 || snapshot.GeneratedAt.IsZero() {
						t.Fatal("upstream status snapshot contract changed")
					}
					for _, field := range []string{`"summary":`, `"has_data":`, `"availability_rate":`, `"estimated_downtime_seconds":`, `"affected_days":`, `"stable_streak_days":`, `"stable_streak_capped":`} {
						if !strings.Contains(string(data), field) {
							t.Fatalf("status JSON missing Insights field %s", field)
						}
					}
				}
				if path == cfg.UI.Path && (!strings.Contains(string(data), cfg.UI.Title) || !strings.Contains(string(data), "Powered by LibreCosta Uptime.")) {
					t.Fatal("built-in dashboard missing title or product footer")
				}
			}
			r.cancel()
			must(t, r.wait(t))
			if s.alerts != nil {
				t.Fatal("disabled alerts started a worker")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.Storage.Bbolt.Path), "alerts.db")); !os.IsNotExist(err) {
				t.Fatal("disabled alerts created storage")
			}
			if s.ready.Load() {
				t.Fatal("ready after shutdown")
			}
			if err := s.Run(context.Background()); err == nil {
				t.Fatal("Server reused after stop")
			}
			if response, err := r.client.Get(r.base + "/livez"); err == nil {
				response.Body.Close()
				t.Fatal("listener still accepts")
			}
			store := reopen(t, cfg.Storage.Bbolt.Path)
			services, err := store.ListServices(context.Background())
			must(t, err)
			if len(services) != 1 || services[0].ID != "target" {
				t.Fatal("registration not persisted or self service invented")
			}
			if name == "up" {
				day := services[0].LastSeenAt.In(cfg.Uptime.Timezone).Format(time.DateOnly)
				rows, err := store.QueryTodaySamples(context.Background(), uptimestorage.QueryTodaySamplesOptions{Day: day})
				must(t, err)
				if len(rows) != 1 || rows[0].UpSlots < 1 {
					t.Fatal("heartbeat history not persisted")
				}
			}
		})
	}
}

func TestServingWithClosedStore(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	s, err := New(testConfig(t, target.URL))
	must(t, err)
	r := startServer(t, s)
	r.health(t, "/readyz", 200, "ready\n")
	must(t, s.store.Close()) // Deliberate failure injection, not product shutdown.
	r.health(t, "/readyz", 503, "not ready\n")
	r.health(t, "/livez", 200, "ok\n")
	r.cancel()
	must(t, r.wait(t))
}

func TestUpstreamDegradedDoesNotStopServer(t *testing.T) {
	cfg := testConfig(t, "http://never-resolves.invalid/")
	s, err := New(cfg)
	must(t, err)
	s.deps.openStorage = func(storageCfg config.StorageConfig) (*runtimeStorage, error) {
		store, err := bbolt.Open(bbolt.Config{Path: storageCfg.Bbolt.Path})
		if err != nil {
			return nil, err
		}
		return wrapBbolt(&testStore{Store: store, upsert: func(context.Context, uptimestorage.Service) error {
			return errors.New("injected runtime storage failure")
		}}), nil
	}
	r := startServer(t, s)
	poll(t, func() bool { v, ok := r.snapshot(cfg.UI.Path); return ok && v.Storage.Status == "degraded" })
	r.health(t, "/readyz", 200, "ready\n") // Ping is healthy; Uptime owns its state.
	r.cancel()
	must(t, r.wait(t))
}

type controlledListener struct {
	net.Listener
	fail        atomic.Bool
	once        sync.Once
	acceptError error
	closeError  error
}

func (ln *controlledListener) Accept() (net.Conn, error) {
	c, err := ln.Listener.Accept()
	if err != nil && ln.fail.Load() {
		return nil, ln.acceptError
	}
	return c, err
}
func (ln *controlledListener) Close() error {
	ln.once.Do(func() { _ = ln.Listener.Close() })
	return ln.closeError
}

func TestUnexpectedServeFailureAndCleanupOrder(t *testing.T) {
	for _, eof := range []bool{false, true} {
		t.Run(map[bool]string{false: "accept error and cleanup errors", true: "unexpected nil Serve result"}[eof], func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
			defer target.Close()
			cfg := testConfig(t, target.URL)
			s, err := New(cfg)
			must(t, err)
			acceptErr, shutdownErr, closeErr := errors.New("accept SECRET"), errors.New("shutdown SECRET"), errors.New("close SECRET")
			if eof {
				acceptErr = io.EOF
				shutdownErr = nil
				closeErr = nil
			}
			var ln *controlledListener
			s.deps.listen = func(network, address string) (net.Listener, error) {
				base, err := net.Listen(network, address)
				if err != nil {
					return nil, err
				}
				ln = &controlledListener{Listener: base, acceptError: acceptErr, closeError: shutdownErr}
				return ln, nil
			}
			entered, exited := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			s.deps.openStorage = func(storageCfg config.StorageConfig) (*runtimeStorage, error) {
				store, err := bbolt.Open(bbolt.Config{Path: storageCfg.Bbolt.Path})
				if err != nil {
					return nil, err
				}
				return wrapBbolt(&testStore{Store: store, upsert: func(ctx context.Context, v uptimestorage.Service) error {
					if calls.Add(1) == 2 {
						close(entered)
						<-ctx.Done()
						close(exited)
						return nil
					}
					return store.UpsertService(ctx, v)
				}, closeStore: func() error {
					select {
					case <-exited:
					default:
						t.Error("storage closed before Uptime worker stopped")
					}
					if s.ready.Load() {
						t.Error("storage closed while ready")
					}
					return errors.Join(store.Close(), closeErr)
				}}), nil
			}
			r := startServer(t, s)
			select {
			case <-entered:
			case <-time.After(testDeadline):
				t.Fatal("probe worker did not enter store")
			}
			ln.fail.Store(true)
			_ = ln.Close()
			err = r.wait(t)
			if err == nil || s.ready.Load() || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unexpected return: %v", err)
			}
			if !eof && (!errors.Is(err, acceptErr) || !errors.Is(err, shutdownErr) || !errors.Is(err, closeErr)) {
				t.Fatal("serve/shutdown/close errors not all preserved")
			}
			reopen(t, cfg.Storage.Bbolt.Path)
		})
	}
}

func TestShutdownTimeoutDrainsConnections(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	cfg := testConfig(t, target.URL)
	cfg.Server.ShutdownTimeout = time.Millisecond
	s, err := New(cfg)
	must(t, err)
	r := startServer(t, s)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(r.base, "http://"), time.Second)
	must(t, err)
	defer conn.Close()
	_, err = io.WriteString(conn, "GET /livez HTTP/1.1\r\nHost: localhost\r\n") // Incomplete request cannot finish naturally.
	must(t, err)
	poll(t, func() bool {
		s.listener.mu.Lock()
		defer s.listener.mu.Unlock()
		return len(s.listener.connections) > 0
	})
	r.cancel()
	if err := r.wait(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown deadline lost: %v", err)
	}
	if s.app.Server().GetCurrentConcurrency() != 0 {
		t.Fatal("HTTP handlers remain after store close")
	}
	s.listener.mu.Lock()
	remaining := len(s.listener.connections)
	s.listener.mu.Unlock()
	if remaining != 0 {
		t.Fatal("HTTP connections remain after store close")
	}
	reopen(t, cfg.Storage.Bbolt.Path)
}

type cancelOnAddr struct {
	net.Listener
	cancel context.CancelFunc
}

func (ln cancelOnAddr) Addr() net.Addr { ln.cancel(); return ln.Listener.Addr() }

func TestCancellationBetweenOnListenAndServe(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	cfg := testConfig(t, target.URL)
	s, err := New(cfg)
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.deps.listen = func(string, string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		return cancelOnAddr{Listener: ln, cancel: cancel}, nil
	}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(testDeadline):
		t.Fatal("startup cancellation missed listener")
	}
	if s.ready.Load() {
		t.Fatal("ready after cancellation")
	}
	reopen(t, cfg.Storage.Bbolt.Path)
}

type testAlertStore struct {
	alerts.Store
	active func(context.Context, string) (*alerts.Incident, error)
	apply  func(context.Context, alerts.Transition) (alerts.Incident, error)
}

func (s *testAlertStore) GetTask(ctx context.Context, id string) (*alerts.Task, error) {
	return s.Store.(alerts.DeliveryStore).GetTask(ctx, id)
}
func (s *testAlertStore) QueryDue(ctx context.Context, q alerts.DueQuery) (alerts.TaskPage, error) {
	return s.Store.(alerts.DeliveryStore).QueryDue(ctx, q)
}
func (s *testAlertStore) RecordOutcome(ctx context.Context, o alerts.Outcome) (alerts.Task, error) {
	return s.Store.(alerts.DeliveryStore).RecordOutcome(ctx, o)
}

func (s *testAlertStore) Active(ctx context.Context, id string) (*alerts.Incident, error) {
	if s.active != nil {
		return s.active(ctx, id)
	}
	return s.Store.Active(ctx, id)
}

func (s *testAlertStore) Apply(ctx context.Context, change alerts.Transition) (alerts.Incident, error) {
	if s.apply != nil {
		return s.apply(ctx, change)
	}
	return s.Store.Apply(ctx, change)
}

func testAlertBbolt(t *testing.T) *alerts.BboltStore {
	t.Helper()
	s, err := alerts.OpenBbolt(filepath.Join(t.TempDir(), "alerts.db"), time.Second)
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	return s
}

// A barrier endpoint is read after all earlier queued events have been handled.
// Tests can then change injected failures without relying on sleeps or queue size.
func startAlertWorker(t *testing.T, store alerts.Store, capacity int, ids ...string) (*alertRuntime, func()) {
	t.Helper()
	barrier := make(chan struct{}, 1)
	wrapper := &testAlertStore{Store: store, active: func(ctx context.Context, id string) (*alerts.Incident, error) {
		if id == "barrier" {
			barrier <- struct{}{}
			return nil, nil
		}
		return store.Active(ctx, id)
	}}
	cfg := config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Second}, Alerts: config.AlertsConfig{Enabled: true, ConsecutiveFailures: 3}}
	for _, id := range append(ids, "barrier") {
		cfg.Endpoints = append(cfg.Endpoints, config.EndpointConfig{ID: id})
	}
	r, err := newAlertRuntime(wrapper, func() error { return nil }, cfg, capacity)
	must(t, err)
	t.Cleanup(func() { must(t, r.stop(time.Second)) })
	wait := func() {
		t.Helper()
		// Blocking is confined to the test barrier, never the production hook.
		event := alertEvent{sequence: r.endpoints["barrier"].sequence.Add(1), observation: alerts.Observation{EndpointID: "barrier", CheckedAt: time.Now(), Success: true}}
		select {
		case r.queue <- event:
		case <-time.After(testDeadline):
			t.Fatal("alert worker did not accept barrier")
		}
		select {
		case <-barrier:
		case <-time.After(testDeadline):
			t.Fatal("alert worker did not reach barrier")
		}
	}
	return r, wait
}

func alertProbe(id string, n int, success bool) uptime.ProbeResult {
	return uptime.ProbeResult{EndpointID: id, CheckedAt: time.Date(2026, 11, 1, 0, 0, n, 0, time.UTC), Success: success, StatusCode: 500}
}

func TestAlertWorkerSequenceAndRestart(t *testing.T) {
	store := testAlertBbolt(t)
	r, wait := startAlertWorker(t, store, 64, "pending", "firing")
	for n := 1; n <= 2; n++ {
		r.hook(context.Background(), alertProbe("pending", n, false))
	}
	for n := 1; n <= 4; n++ {
		r.hook(context.Background(), alertProbe("firing", n, false))
	}
	wait()
	active, err := store.Active(context.Background(), "firing")
	must(t, err)
	if active == nil || !active.FirstFailureAt.Equal(alertProbe("firing", 1, false).CheckedAt) || !active.ConfirmedAt.Equal(alertProbe("firing", 3, false).CheckedAt) || len(active.ChannelIDs) != 0 {
		t.Fatal("missing, duplicate, or wrongly timed incident")
	}
	must(t, r.stop(time.Second))
	r2, wait2 := startAlertWorker(t, store, 64, "pending", "firing")
	r2.hook(context.Background(), alertProbe("pending", 3, false))
	r2.hook(context.Background(), alertProbe("firing", 5, false))
	wait2()
	pending, err := store.Active(context.Background(), "pending")
	must(t, err)
	still, err := store.Active(context.Background(), "firing")
	must(t, err)
	if pending != nil || still == nil || still.ID != active.ID {
		t.Fatal("restart did not reset PENDING or preserve FIRING")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // A completed result entering the hook survives probe cancellation.
	r2.hook(ctx, alertProbe("firing", 6, true))
	must(t, r2.stop(time.Second))
	closed, err := store.Get(context.Background(), active.ID)
	must(t, err)
	if closed.RecoveredAt != alertProbe("firing", 6, true).CheckedAt {
		t.Fatal("drain lost completed recovery")
	}
	tasks, err := store.Outbox(context.Background())
	must(t, err)
	if len(tasks) != 0 {
		t.Fatal("runtime invented notification channels")
	}
}

func TestAlertIngressOverflowAndStorageFailure(t *testing.T) {
	for _, mode := range []string{"overflow", "read failure", "write failure", "uncertain commit"} {
		t.Run(mode, func(t *testing.T) {
			store := testAlertBbolt(t)
			var fail atomic.Bool
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			injected := errors.New("Redis URL and HEADER SECRET")
			wrapper := &testAlertStore{Store: store}
			wrapper.active = func(ctx context.Context, id string) (*alerts.Incident, error) {
				if mode == "overflow" {
					once.Do(func() {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
						}
					})
				}
				if fail.Load() && mode == "read failure" {
					return nil, injected
				}
				return store.Active(ctx, id)
			}
			wrapper.apply = func(ctx context.Context, change alerts.Transition) (alerts.Incident, error) {
				if fail.Load() && mode == "write failure" {
					return alerts.Incident{}, injected
				}
				result, err := store.Apply(ctx, change)
				if fail.Load() && mode == "uncertain commit" && err == nil {
					return alerts.Incident{}, injected
				}
				return result, err
			}
			capacity := 16
			if mode == "overflow" {
				capacity = 1
			}
			r, wait := startAlertWorker(t, wrapper, capacity, "target")
			r.hook(context.Background(), alertProbe("target", 1, false))
			if mode == "overflow" {
				select {
				case <-entered:
				case <-time.After(testDeadline):
					t.Fatal("alert worker did not enter storage")
				}
				r.hook(context.Background(), alertProbe("target", 2, false))
				r.hook(context.Background(), alertProbe("target", 3, true)) // Lost success must break the sequence.
				lost := r.dropped.Load()
				close(release)
				if lost != 1 {
					t.Fatal("overflow was not counted")
				}
				wait()
			} else {
				r.hook(context.Background(), alertProbe("target", 2, false))
				wait()
				fail.Store(true)
				r.hook(context.Background(), alertProbe("target", 3, false))
				wait()
				fail.Store(false)
			}
			for n := 4; n <= 6; n++ {
				r.hook(context.Background(), alertProbe("target", n, false))
				wait()
				active, err := store.Active(context.Background(), "target")
				must(t, err)
				// Failed/uncertain writes retain B1's original transition for
				// reconciliation; read/loss gaps instead reset unconfirmed counts.
				if mode == "overflow" || mode == "read failure" {
					if (active == nil) != (n < 6) {
						t.Fatal("pending failures crossed a gap")
					}
				} else if active == nil || !active.ConfirmedAt.Equal(alertProbe("target", 3, false).CheckedAt) {
					t.Fatal("original transition was lost during reconciliation")
				}
			}
			r.hook(context.Background(), alertProbe("target", 7, true))
			wait()
			active, err := store.Active(context.Background(), "target")
			must(t, err)
			if active != nil {
				t.Fatal("real recovery was not consumed")
			}
		})
	}
}

func TestAlertConcurrentEndpoints(t *testing.T) {
	store := testAlertBbolt(t)
	ids := []string{"one", "two", "three", "four"}
	r, _ := startAlertWorker(t, store, 256, ids...)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			for n := 1; n <= 10; n++ {
				// Each group creates then recovers one incident. The final group
				// remains FIRING, exposing reordering of success/failure callbacks.
				r.hook(context.Background(), alertProbe(id, n, n == 4 || n == 8))
			}
			r.hook(context.Background(), alertProbe(id, 11, false))
		})
	}
	wg.Wait()
	must(t, r.stop(time.Second))
	for _, id := range ids {
		active, err := store.Active(context.Background(), id)
		must(t, err)
		if active == nil || !active.FirstFailureAt.Equal(alertProbe(id, 9, false).CheckedAt) || !active.ConfirmedAt.Equal(alertProbe(id, 11, false).CheckedAt) {
			t.Fatal("endpoint callback order changed")
		}
	}
}

func TestAlertRuntimeEndpointIntegration(t *testing.T) {
	var success atomic.Bool
	var probes atomic.Int32
	var first, second atomic.Pointer[time.Time]
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now()
		switch probes.Add(1) {
		case 1:
			first.Store(&now)
		case 2:
			second.Store(&now)
		}
		if !success.Load() {
			w.WriteHeader(500)
		}
	}))
	defer target.Close()
	cfg := testConfig(t, target.URL)
	cfg.Alerts.Enabled = true
	s, err := New(cfg)
	must(t, err)
	r := startServer(t, s)
	var incident *alerts.Incident
	poll(t, func() bool {
		incident, err = s.alerts.store.Active(context.Background(), "target")
		must(t, err)
		return incident != nil && probes.Load() >= 4
	})
	if first.Load() == nil || second.Load() == nil || incident.FirstFailureAt.Before(*first.Load()) || !incident.FirstFailureAt.Before(*second.Load()) {
		t.Fatal("first probe was lost during startup")
	}
	id := incident.ID
	success.Store(true)
	poll(t, func() bool {
		closed, err := s.alerts.store.Get(context.Background(), id)
		must(t, err)
		return closed != nil && !closed.RecoveredAt.IsZero()
	})
	r.health(t, "/readyz", 200, "ready\n")
	r.cancel()
	must(t, r.wait(t))
	// Late callbacks cannot reach the already-closed store or panic on send.
	s.alerts.hook(context.Background(), alertProbe("target", 20, false))
	path := filepath.Join(filepath.Dir(cfg.Storage.Bbolt.Path), "alerts.db")
	reopened, err := alerts.OpenBbolt(path, time.Second)
	must(t, err)
	defer reopened.Close()
	closed, err := reopened.Get(context.Background(), id)
	must(t, err)
	if closed == nil || closed.RecoveredAt.IsZero() {
		t.Fatal("recovery was not persisted before close")
	}
}

func TestAlertStartupFailureCleanup(t *testing.T) {
	for _, mode := range []string{"foreign file", "path collision", "initialization", "constructor"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig(t, "http://never-resolves.invalid/")
			cfg.Alerts.Enabled = true
			if mode == "path collision" {
				cfg.Storage.Bbolt.Path = filepath.Join(filepath.Dir(cfg.Storage.Bbolt.Path), "alerts.db")
			}
			if mode == "foreign file" {
				must(t, os.MkdirAll(filepath.Dir(cfg.Storage.Bbolt.Path), 0750))
				must(t, os.WriteFile(filepath.Join(filepath.Dir(cfg.Storage.Bbolt.Path), "alerts.db"), []byte("unrelated SECRET data"), 0600))
			}
			if mode == "constructor" {
				cfg.Endpoints[0].Timeout = time.Nanosecond
			}
			s, err := New(cfg)
			must(t, err)
			s.deps.listen = func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") }
			if mode == "initialization" {
				s.deps.openAlerts = func(context.Context, *runtimeStorage, config.Config) (alerts.Store, func() error, error) {
					return nil, nil, errors.New("SECRET endpoint header or Redis URL")
				}
			}
			err = s.Run(context.Background())
			if err == nil || strings.Contains(err.Error(), "SECRET") || s.ready.Load() {
				t.Fatalf("unsafe or missing startup failure: %v", err)
			}
			if s.alerts != nil {
				select {
				case <-s.alerts.done:
				default:
					t.Fatal("constructor failure leaked worker")
				}
			}
			reopen(t, cfg.Storage.Bbolt.Path)
		})
	}
}

func TestAlertDrainDeadlineAndCloseOrder(t *testing.T) {
	store := testAlertBbolt(t)
	entered := make(chan struct{})
	var exited, closed atomic.Bool
	wrapper := &testAlertStore{Store: store, active: func(ctx context.Context, id string) (*alerts.Incident, error) {
		close(entered)
		<-ctx.Done()
		exited.Store(true)
		return nil, ctx.Err()
	}}
	cfg := config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Second}, Endpoints: []config.EndpointConfig{{ID: "target"}}}
	closeErr := errors.New("close SECRET")
	r, err := newAlertRuntime(wrapper, func() error {
		if !exited.Load() {
			t.Error("closed before worker left storage")
		}
		closed.Store(true)
		return closeErr
	}, cfg, 1)
	must(t, err)
	r.hook(context.Background(), alertProbe("target", 1, false))
	select {
	case <-entered:
	case <-time.After(testDeadline):
		t.Fatal("alert worker did not enter storage")
	}
	err = r.stop(20 * time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, closeErr) || strings.Contains(err.Error(), "SECRET") || !closed.Load() {
		t.Fatalf("drain/close failures not preserved safely: %v", err)
	}
	r.hook(context.Background(), alertProbe("target", 2, true))
}

func TestAlertFiringSurvivesStorageOutage(t *testing.T) {
	store := testAlertBbolt(t)
	var fail atomic.Bool
	wrapper := &testAlertStore{Store: store, active: func(ctx context.Context, id string) (*alerts.Incident, error) {
		if fail.Load() {
			return nil, errors.New("SECRET")
		}
		return store.Active(ctx, id)
	}}
	r, wait := startAlertWorker(t, wrapper, 16, "target")
	for n := 1; n <= 3; n++ {
		r.hook(context.Background(), alertProbe("target", n, false))
	}
	wait()
	initial, err := store.Active(context.Background(), "target")
	must(t, err)
	if initial == nil {
		t.Fatal("missing confirmed incident")
	}
	fail.Store(true)
	r.hook(context.Background(), alertProbe("target", 4, true))
	wait()
	fail.Store(false)
	r.hook(context.Background(), alertProbe("target", 5, false))
	wait()
	active, err := store.Active(context.Background(), "target")
	must(t, err)
	if active == nil || active.ID != initial.ID {
		t.Fatal("outage or gap fabricated recovery")
	}
	r.hook(context.Background(), alertProbe("target", 6, true))
	wait()
	closed, err := store.Get(context.Background(), initial.ID)
	must(t, err)
	if closed.RecoveredAt != alertProbe("target", 6, true).CheckedAt {
		t.Fatal("recovery did not require a consumed real success")
	}
}

func TestAlertShutdownDrainsAcceptedProbe(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer target.Close()
	cfg := testConfig(t, target.URL)
	cfg.Alerts = config.AlertsConfig{Enabled: true, ConsecutiveFailures: 1}
	s, err := New(cfg)
	must(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var storeClosed atomic.Bool
	s.deps.openAlerts = func(ctx context.Context, resources *runtimeStorage, cfg config.Config) (alerts.Store, func() error, error) {
		store, closeStore, err := openAlertStore(ctx, resources, cfg)
		if err != nil {
			return nil, nil, err
		}
		wrapper := &testAlertStore{Store: store, active: func(ctx context.Context, id string) (*alerts.Incident, error) {
			once.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
			if storeClosed.Load() || ctx.Err() != nil {
				t.Error("accepted probe used a closed store or canceled probe context")
			}
			return store.Active(ctx, id)
		}}
		return wrapper, func() error { storeClosed.Store(true); return closeStore() }, nil
	}
	r := startServer(t, s)
	select {
	case <-entered:
	case <-time.After(testDeadline):
		close(release)
		t.Fatal("initial probe did not enter worker")
	}
	r.cancel()
	poll(t, func() bool { return !s.ready.Load() })
	close(release)
	must(t, r.wait(t))
	if !storeClosed.Load() {
		t.Fatal("alert store left open")
	}
	reopened, err := alerts.OpenBbolt(filepath.Join(filepath.Dir(cfg.Storage.Bbolt.Path), "alerts.db"), time.Second)
	must(t, err)
	defer reopened.Close()
	incident, err := reopened.Active(context.Background(), "target")
	must(t, err)
	if incident == nil {
		t.Fatal("shutdown lost a completed, accepted probe")
	}
}

func TestAlertStorageDeadlines(t *testing.T) {
	store := testAlertBbolt(t)
	entered, release := make(chan struct{}), make(chan struct{})
	deadline := make(chan time.Time, 1)
	var calls atomic.Int32
	wrapper := &testAlertStore{Store: store, active: func(ctx context.Context, id string) (*alerts.Incident, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("storage operation has no deadline")
		}
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		} else {
			deadline <- d
		}
		return store.Active(ctx, id)
	}}
	// An exaggerated normal budget makes it unambiguous that queued work
	// inherits the independent drain deadline, rather than another full budget.
	cfg := config.Config{Server: config.ServerConfig{ShutdownTimeout: time.Hour}, Endpoints: []config.EndpointConfig{{ID: "target"}}}
	r, err := newAlertRuntime(wrapper, func() error { return nil }, cfg, 2)
	must(t, err)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); _ = r.stop(time.Second) })
	r.hook(context.Background(), alertProbe("target", 1, false))
	r.hook(context.Background(), alertProbe("target", 2, false))
	select {
	case <-entered:
	case <-time.After(testDeadline):
		t.Fatal("worker did not start observation")
	}
	stopped := make(chan error, 1)
	stopAt := time.Now()
	go func() { stopped <- r.stop(time.Second) }()
	poll(t, func() bool {
		r.gate.RLock()
		defer r.gate.RUnlock()
		return !r.accepting
	})
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-stopped:
		must(t, err)
	case <-time.After(testDeadline):
		t.Fatal("worker did not drain")
	}
	select {
	case d := <-deadline:
		if d.IsZero() || d.After(stopAt.Add(2*time.Second)) {
			t.Fatal("draining did not inherit its own deadline")
		}
	default:
		t.Fatal("accepted second observation was not drained")
	}
}
