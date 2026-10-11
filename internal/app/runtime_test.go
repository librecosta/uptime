package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
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
			for _, path := range []string{cfg.UI.Path, cfg.UI.Path + "/api/status", "/"} {
				response, err := r.client.Get(r.base + path)
				must(t, err)
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				must(t, err)
				want := 200
				if path == "/" {
					want = 404
				}
				if response.StatusCode != want || strings.Contains(string(data), "PRIVATE") {
					t.Fatalf("route %s: status=%d or secret exposed", path, response.StatusCode)
				}
				if path == cfg.UI.Path && !strings.Contains(string(data), cfg.UI.Title) {
					t.Fatal("built-in dashboard missing configured title")
				}
			}
			r.cancel()
			must(t, r.wait(t))
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
