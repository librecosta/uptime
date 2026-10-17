package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	fiberredis "github.com/gofiber/storage/redis/v3"
	"github.com/librecosta/uptime/internal/config"
	"github.com/redis/go-redis/v9"
)

func TestRedisIntegrationServiceStorage(t *testing.T) {
	redisURL := redisIntegrationURL(t)
	prefix := redisIntegrationPrefix(t, redisURL)
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer target.Close()
	cfg := redisIntegrationConfig(t, redisURL, prefix, "target", target.URL)
	cfg.Server.Address = target.Listener.Addr().String() // Opening storage must not bind.
	cfg.Uptime.Retention = 7
	owner, err := openStorage(cfg.Storage)
	must(t, err)
	defer owner.Close()
	writer, err := uptime.NewRedisStore(context.Background(), uptime.RedisStoreConfig{
		Store: owner.redis, StorageKeyPrefix: prefix, RetentionDays: 7,
	})
	must(t, err)
	seedServiceHistory(t, writer, "history", 1)
	seedServiceHistory(t, writer, "keep", 2)
	session, err := OpenServiceStorage(context.Background(), cfg)
	must(t, err)
	defer session.Close()
	assertServiceHistory(t, session, "history", true)
	assertReadOnlyServiceExport(t, session)
	// Inspect only this test's prefix; production access never knows Redis keys.
	ctx := context.Background()
	seedServiceHistory(t, session.store, "session", 3)
	ttl, err := owner.redisClient.PTTL(ctx, prefix+":service:session").Result()
	must(t, err)
	if ttl <= 8*24*time.Hour-5*time.Second || ttl > 8*24*time.Hour {
		t.Fatalf("configured retention lost: TTL = %v", ttl)
	}
	otherCfg := cfg
	otherRedis := *cfg.Storage.Redis
	otherRedis.KeyPrefix = redisIntegrationPrefix(t, redisURL)
	otherCfg.Storage.Redis = &otherRedis
	other, err := OpenServiceStorage(ctx, otherCfg)
	must(t, err)
	defer other.Close()
	rows, err := other.ListServices(ctx)
	must(t, err)
	if len(rows) != 0 {
		t.Fatal("query crossed configured Redis prefixes")
	}
	must(t, session.RemoveService(ctx, "history"))
	assertServiceHistory(t, session, "history", false)
	assertServiceHistory(t, session, "keep", true)
	rows, err = session.ListServices(ctx)
	must(t, err)
	if len(rows) != 2 || requests.Load() != 0 {
		t.Fatal("standalone access started monitoring")
	}
	must(t, session.Close())
	must(t, session.Close())
	assertRedisClosed(t, session.resources)
	must(t, owner.Ping(ctx)) // Closing the session cannot close another owner.
	_, err = writer.ListServices(ctx)
	must(t, err)
	_, err = session.ListServices(ctx)
	if !errors.Is(err, redis.ErrClosed) {
		t.Fatal("query after Close did not preserve client error")
	}
}

func TestRedisIntegrationServiceStorageStartupFailure(t *testing.T) {
	redisURL := redisIntegrationURL(t)
	u, err := url.Parse(redisURL)
	must(t, err)
	u.User = url.UserPassword("missing-"+rand.Text(), "SECRET")
	cfg := redisIntegrationConfig(t, u.String(), redisIntegrationPrefix(t, redisURL), "target", "http://never-resolves.invalid")
	session, err := OpenServiceStorage(context.Background(), cfg)
	if session != nil || err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), u.String()) {
		t.Fatal("missing or unsafe standalone Redis initialization failure")
	}
}

func redisIntegrationURL(t *testing.T) string {
	t.Helper()
	value := os.Getenv("UPTIME_TEST_REDIS_URL")
	if value == "" {
		t.Skip("Redis integration URL not configured")
	}
	if _, err := redis.ParseURL(value); err != nil {
		t.Fatal("invalid Redis integration URL")
	}
	return value
}

func redisIntegrationConfig(t *testing.T, redisURL, prefix, id, target string) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "uptime.yaml")
	data := fmt.Sprintf("server: {address: '127.0.0.1:8080', shutdown_timeout: 3s}\nstorage: {type: redis, redis: {url: %q, key_prefix: %q}}\nuptime: {interval: 1s}\nui: {path: /status}\nendpoints: [{id: %q, url: %q, timeout: 500ms}]\n", redisURL, prefix, id, target)
	must(t, os.WriteFile(path, []byte(data), 0600))
	cfg, err := config.LoadFile(path)
	must(t, err)
	if cfg.Storage.Bbolt != nil {
		t.Fatal("Redis YAML retained bbolt")
	}
	return cfg
}

// Each test owns only its generated namespace. Never reset/flush the database.
func redisIntegrationPrefix(t *testing.T, redisURL string) string {
	t.Helper()
	prefix := "deepfurry:test:" + rand.Text()
	t.Cleanup(func() {
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			t.Error("invalid Redis cleanup URL")
			return
		}
		opts.ContextTimeoutEnabled = true
		client := redis.NewClient(opts)
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, prefix+":*", 100).Result()
			if err != nil {
				t.Error("cannot scan owned Redis test prefix")
				return
			}
			for _, key := range keys {
				if !strings.HasPrefix(key, prefix+":") {
					t.Error("cleanup key outside owned prefix")
					return
				}
			}
			if len(keys) > 0 {
				if err := client.Del(ctx, keys...).Err(); err != nil {
					t.Error("cannot clean owned Redis test keys")
					return
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
	})
	return prefix
}

func redisSnapshot(t *testing.T, r *runningServer, path, id, status string) uptime.StatusResponse {
	t.Helper()
	var result uptime.StatusResponse
	poll(t, func() bool {
		v, ok := r.snapshot(path)
		if !ok || v.Storage.Driver != "redis" || v.Storage.Status != "ok" || len(v.Services) != 1 || v.Services[0].ID != id || (status != "" && v.Services[0].CurrentStatus != status) {
			return false
		}
		result = v
		return true
	})
	return result
}

func assertRedisClosed(t *testing.T, store *runtimeStorage) {
	t.Helper()
	if _, err := store.redis.Get("unused"); !errors.Is(err, fiberredis.ErrClosed) {
		t.Fatal("Fiber Redis handle left open")
	}
	if err := store.redisClient.Ping(context.Background()).Err(); !errors.Is(err, redis.ErrClosed) {
		t.Fatal("owned Redis client left open")
	}
	if store.bbolt != nil {
		t.Fatal("Redis runtime constructed bbolt")
	}
}

func TestRedisIntegrationEndpoints(t *testing.T) {
	redisURL := redisIntegrationURL(t)
	for _, code := range []int{200, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
			defer target.Close()
			cfg := redisIntegrationConfig(t, redisURL, redisIntegrationPrefix(t, redisURL), "target", target.URL)
			s, err := New(cfg)
			must(t, err)
			r := startServer(t, s)
			status := "up"
			if code == 500 {
				status = "down"
			}
			redisSnapshot(t, r, cfg.UI.Path, "target", status)
			r.health(t, "/livez", 200, "ok\n")
			r.health(t, "/readyz", 200, "ready\n")
			r.cancel()
			must(t, r.wait(t))
			assertRedisClosed(t, s.store)
		})
	}
}

func TestRedisIntegrationRestart(t *testing.T) {
	redisURL := redisIntegrationURL(t)
	prefix := redisIntegrationPrefix(t, redisURL)
	var code atomic.Int32
	code.Store(200)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(code.Load())) }))
	defer target.Close()
	cfg := redisIntegrationConfig(t, redisURL, prefix, "persistent", target.URL)
	s, err := New(cfg)
	must(t, err)
	r := startServer(t, s)
	var before uptime.StatusResponse
	poll(t, func() bool {
		v, ok := r.snapshot(cfg.UI.Path)
		if !ok || len(v.Services) != 1 || v.Services[0].CurrentStatus != "up" {
			return false
		}
		for _, day := range v.Services[0].Daily {
			if day.UpSlots > 0 {
				before = v
				return true
			}
		}
		return false
	})
	code.Store(500) // The restarted runtime cannot manufacture a new UP sample.
	r.cancel()
	must(t, r.wait(t))
	assertRedisClosed(t, s.store)
	cfg = redisIntegrationConfig(t, redisURL, prefix, "persistent", target.URL)
	s, err = New(cfg)
	must(t, err)
	r = startServer(t, s)
	after := redisSnapshot(t, r, cfg.UI.Path, "persistent", "")
	if after.Services[0].LastSeenAt.Before(before.Services[0].LastSeenAt) {
		t.Fatal("restart lost last-seen history")
	}
	for _, old := range before.Services[0].Daily {
		if old.UpSlots == 0 {
			continue
		}
		found := false
		for _, day := range after.Services[0].Daily {
			if day.Day == old.Day && day.UpSlots >= old.UpSlots {
				found = true
			}
		}
		if !found {
			t.Fatal("restart lost successful heartbeat history")
		}
	}
	r.cancel()
	must(t, r.wait(t))
	assertRedisClosed(t, s.store)
}

func TestRedisIntegrationPrefixIsolation(t *testing.T) {
	redisURL := redisIntegrationURL(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	var runs []*runningServer
	for _, id := range []string{"alpha", "beta"} {
		cfg := redisIntegrationConfig(t, redisURL, redisIntegrationPrefix(t, redisURL), id, target.URL)
		s, err := New(cfg)
		must(t, err)
		r := startServer(t, s)
		runs = append(runs, r)
		redisSnapshot(t, r, cfg.UI.Path, id, "up")
	}
	// Recheck A after B has written data to the same Redis database.
	redisSnapshot(t, runs[0], "/status", "alpha", "up")
	for _, r := range runs {
		r.cancel()
		must(t, r.wait(t))
	}
}

type redisOperationGate struct {
	fail           atomic.Bool
	pings          atomic.Int32
	allowPreflight bool
}

func (*redisOperationGate) DialHook(next redis.DialHook) redis.DialHook { return next }
func (g *redisOperationGate) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "ping" {
			n := g.pings.Add(1)
			if g.allowPreflight && n == 1 {
				// Allow connection initialization and the real preflight Ping to
				// finish before failing upstream's subsequent operations.
				err := next(ctx, cmd)
				g.fail.Store(true)
				return err
			}
		}
		if g.fail.Load() {
			return errors.New("injected Redis outage")
		}
		return next(ctx, cmd)
	}
}
func (g *redisOperationGate) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if g.fail.Load() {
			return errors.New("injected Redis outage")
		}
		return next(ctx, cmds)
	}
}

func TestRedisIntegrationFailureAndRecovery(t *testing.T) {
	redisURL := redisIntegrationURL(t)
	for _, afterPreflight := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-preflight=%v", afterPreflight), func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
			defer target.Close()
			cfg := redisIntegrationConfig(t, redisURL, redisIntegrationPrefix(t, redisURL), "target", target.URL)
			s, err := New(cfg)
			must(t, err)
			gate := &redisOperationGate{allowPreflight: afterPreflight}
			s.deps.openStorage = func(c config.StorageConfig) (*runtimeStorage, error) {
				store, err := openStorage(c)
				if err == nil {
					store.redisClient.AddHook(gate)
				}
				return store, err
			}
			r := startServer(t, s)
			if gate.pings.Load() < 2 {
				t.Fatal("upstream initialization Ping was suppressed")
			}
			if !afterPreflight {
				redisSnapshot(t, r, cfg.UI.Path, "target", "up")
				r.health(t, "/readyz", 200, "ready\n")
				gate.fail.Store(true)
			}
			r.health(t, "/livez", 200, "ok\n")
			r.health(t, "/readyz", 503, "not ready\n")
			select {
			case <-r.done:
				t.Fatal("Redis outage stopped the process")
			default:
			}
			gate.fail.Store(false)
			r.health(t, "/readyz", 200, "ready\n")
			redisSnapshot(t, r, cfg.UI.Path, "target", "up")
			r.cancel()
			must(t, r.wait(t))
			assertRedisClosed(t, s.store)
		})
	}
}

func TestRedisIntegrationStartupFailure(t *testing.T) {
	redisURL := redisIntegrationURL(t)
	u, err := url.Parse(redisURL)
	must(t, err)
	u.User = url.UserPassword("missing-"+rand.Text(), "SECRET")
	cfg := redisIntegrationConfig(t, u.String(), "deepfurry:test:"+rand.Text(), "target", "http://never-resolves.invalid")
	s, err := New(cfg)
	must(t, err)
	s.deps.listen = func(string, string) (net.Listener, error) {
		t.Error("listener created after failed Redis preflight")
		return nil, errors.New("unexpected listener")
	}
	err = s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot connect Redis storage") || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("missing or unsafe Redis startup error")
	}
	if s.app != nil || s.listener != nil {
		t.Fatal("failed preflight started runtime")
	}
	if s.store == nil {
		t.Fatal("expected real Redis preflight")
	}
	assertRedisClosed(t, s.store)
}
