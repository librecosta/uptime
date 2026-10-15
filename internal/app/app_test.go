package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/gofiber/fiber/v3"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/storage/bbolt"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func testConfig(t *testing.T, target string) config.Config {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "uptime.yaml")
	data := fmt.Sprintf("server: {address: '127.0.0.1:8080', shutdown_timeout: 2s}\nstorage: {bbolt: {path: %q}}\nuptime: {interval: 1s}\nui: {path: /status}\nendpoints: [{id: target, url: %q, timeout: 500ms}]\n", filepath.Join(dir, "data", "uptime.db"), target)
	must(t, os.WriteFile(file, []byte(data), 0600))
	cfg, err := config.LoadFile(file)
	must(t, err)
	return cfg
}

func TestNewHasNoIO(t *testing.T) {
	for _, capability := range []string{"supported", "redis", "tls", "auth", "redis-tls-auth"} {
		t.Run(capability, func(t *testing.T) {
			cfg := testConfig(t, "http://never-resolves.invalid/")
			path := cfg.Storage.Bbolt.Path
			switch capability {
			case "redis", "redis-tls-auth":
				cfg.Storage.Type = config.StorageRedis
				cfg.Storage.Bbolt = nil
				cfg.Storage.Redis = &config.RedisConfig{URL: &url.URL{Scheme: "redis", Host: "never-resolves.invalid"}, KeyPrefix: "test"}
			case "tls":
				cfg.Server.TLS = &config.TLSConfig{}
			case "auth":
				cfg.Auth = &config.BasicAuthConfig{}
			}
			if capability == "redis-tls-auth" {
				cfg.Server.TLS = &config.TLSConfig{CertFile: "missing", KeyFile: "missing"}
				cfg.Auth = &config.BasicAuthConfig{}
			}
			s, err := New(cfg)
			must(t, err)
			if s.app != nil || s.store != nil || s.listener != nil || s.ready.Load() || s.started.Load() {
				t.Fatal("New created runtime state")
			}
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatal("New created storage directory")
			}
		})
	}
}

func TestAlreadyCanceledAndSingleUse(t *testing.T) {
	cfg := testConfig(t, "http://never-resolves.invalid/")
	s, err := New(cfg)
	must(t, err)
	s.deps.openStorage = func(config.StorageConfig) (*runtimeStorage, error) {
		t.Error("opened storage")
		return nil, errors.New("unexpected open")
	}
	s.deps.listen = func(string, string) (net.Listener, error) {
		t.Error("created listener")
		return nil, errors.New("unexpected listen")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	must(t, s.Run(ctx))
	if err := s.Run(ctx); err == nil {
		t.Fatal("Server reused")
	}
	if s.app != nil || s.store != nil || s.listener != nil {
		t.Fatal("canceled Run constructed runtime")
	}
	if _, err := os.Stat(filepath.Dir(cfg.Storage.Bbolt.Path)); !os.IsNotExist(err) {
		t.Fatal("canceled Run created directory")
	}
}

func TestUptimeMapping(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid/health?key=secret")
	cfg.Endpoints[0].Method = "HEAD"
	cfg.Endpoints[0].Headers = map[string]string{"Authorization": "Bearer secret"}
	cfg.Endpoints[0].ExpectedStatusCodes = []int{200, 204}
	f := fiber.New(fiber.Config{AppName: "LibreCosta Uptime"})
	store := &bbolt.Store{}
	mapped := buildUptimeConfig(f, wrapBbolt(store), cfg)
	if mapped.App != f || mapped.Storage != store || mapped.Store != nil || mapped.ServiceID != "" || mapped.ServiceName != "" || mapped.ServiceDescription != "" {
		t.Fatal("app/store/self mapping")
	}
	if mapped.SampleInterval != cfg.Uptime.Interval || mapped.RetentionDays != int(cfg.Uptime.Retention) || mapped.DaysToShow != int(cfg.Uptime.Window) || mapped.Timezone != cfg.Uptime.Timezone {
		t.Fatal("uptime mapping")
	}
	u := mapped.UI
	if u.Path != cfg.UI.Path || u.Title != cfg.UI.Title || u.Description != cfg.UI.Description || u.Footer != cfg.UI.Footer || u.FaviconURL != "" || u.GreenThreshold != cfg.UI.Thresholds.Green || u.YellowThreshold != cfg.UI.Thresholds.Yellow {
		t.Fatal("UI mapping")
	}
	e := mapped.Endpoints[0]
	want := cfg.Endpoints[0]
	if len(mapped.Endpoints) != 1 || e.ID != want.ID || e.Name != want.Name || e.Description != want.Description || e.URL != want.URL.String() || e.Method != "HEAD" || e.Interval != want.Interval || e.Timeout != want.Timeout || e.Headers["Authorization"] != "Bearer secret" || len(e.ExpectedStatusCodes) != 2 {
		t.Fatal("endpoint mapping")
	}
	e.Headers["Authorization"] = "changed"
	e.ExpectedStatusCodes[0] = 500
	if want.Headers["Authorization"] != "Bearer secret" || want.ExpectedStatusCodes[0] != 200 {
		t.Fatal("mapping aliases normalized config")
	}
	cfg.UI.FaviconURL = cfg.Endpoints[0].URL
	cfg.Endpoints[0].Method = "GET"
	mapped = buildUptimeConfig(f, wrapBbolt(store), cfg)
	if mapped.UI.FaviconURL != cfg.UI.FaviconURL.String() || mapped.Endpoints[0].Method != "GET" {
		t.Fatal("favicon/GET mapping")
	}
}

type testStore struct {
	*bbolt.Store
	ping       func(context.Context) error
	upsert     func(context.Context, uptimestorage.Service) error
	closeStore func() error
}

func (s *testStore) Ping(ctx context.Context) error {
	if s.ping != nil {
		return s.ping(ctx)
	}
	return s.Store.Ping(ctx)
}
func (s *testStore) UpsertService(ctx context.Context, v uptimestorage.Service) error {
	if s.upsert != nil {
		return s.upsert(ctx, v)
	}
	return s.Store.UpsertService(ctx, v)
}
func (s *testStore) Close() error {
	if s.closeStore != nil {
		return s.closeStore()
	}
	return s.Store.Close()
}

func TestHealth(t *testing.T) {
	store, err := bbolt.Open(bbolt.Config{Path: filepath.Join(t.TempDir(), "health.db")})
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	pingCalls := 0
	wrapped := &testStore{Store: store, ping: func(ctx context.Context) error {
		pingCalls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			return errors.New("invalid health timeout")
		}
		return store.Ping(ctx)
	}}
	s := &Server{app: fiber.New(), store: wrapBbolt(wrapped)}
	s.registerHealth()
	check := func(path string, status int, body string) {
		t.Helper()
		response, err := s.app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		must(t, err)
		defer response.Body.Close()
		assertHealth(t, response, status, body)
	}
	check("/livez", 200, "ok\n")
	check("/readyz", 503, "not ready\n")
	if pingCalls != 0 {
		t.Fatal("health accessed store while not ready")
	}
	s.ready.Store(true)
	check("/readyz", 200, "ready\n")
	if pingCalls != 1 {
		t.Fatal("missing ready Ping")
	}
	must(t, store.Close())
	check("/livez", 200, "ok\n")
	check("/readyz", 503, "not ready\n")
	if pingCalls != 2 {
		t.Fatal("liveness accessed storage")
	}
}

func TestStartupFailureCleanup(t *testing.T) {
	for _, failure := range []string{"open", "bind", "constructor"} {
		t.Run(failure, func(t *testing.T) {
			cfg := testConfig(t, "http://never-resolves.invalid/")
			if failure == "constructor" {
				cfg.Endpoints[0].Timeout = time.Nanosecond
			}
			s, err := New(cfg)
			must(t, err)
			cause := errors.New("injected SECRET failure")
			closeCause := errors.New("injected close SECRET")
			if failure == "open" {
				s.deps.openStorage = func(config.StorageConfig) (*runtimeStorage, error) { return nil, cause }
				s.deps.listen = func(string, string) (net.Listener, error) { t.Error("listen after failed open"); return nil, cause }
			} else {
				s.deps.openStorage = func(storageCfg config.StorageConfig) (*runtimeStorage, error) {
					store, err := bbolt.Open(bbolt.Config{Path: storageCfg.Bbolt.Path})
					if err != nil {
						return nil, err
					}
					return wrapBbolt(&testStore{Store: store, closeStore: func() error { return errors.Join(store.Close(), closeCause) }}), nil
				}
				s.deps.listen = func(string, string) (net.Listener, error) {
					if failure == "bind" {
						return nil, cause
					}
					return net.Listen("tcp", "127.0.0.1:0")
				}
			}
			err = s.Run(context.Background())
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe/missing startup error: %v", err)
			}
			if failure != "constructor" && !errors.Is(err, cause) {
				t.Fatal("primary error lost")
			}
			if failure != "open" {
				if !errors.Is(err, closeCause) {
					t.Fatal("cleanup error lost")
				}
				reopen(t, cfg.Storage.Bbolt.Path)
			}
		})
	}
}

func reopen(t *testing.T, path string) *bbolt.Store {
	t.Helper()
	store, err := bbolt.Open(bbolt.Config{Path: path, Timeout: 100 * time.Millisecond})
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	return store
}

func wrapBbolt(store runtimeStore) *runtimeStorage {
	return &runtimeStorage{kind: config.StorageBbolt, bbolt: store}
}
