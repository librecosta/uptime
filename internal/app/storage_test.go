package app

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	fiberredis "github.com/gofiber/storage/redis/v3"
	"github.com/librecosta/uptime/internal/config"
	"github.com/redis/go-redis/v9"
)

// Only health/lifecycle commands are substituted; no Redis persistence engine
// is emulated. Persistence behavior is covered by opt-in real Redis tests.
type pingClient struct {
	redis.UniversalClient
	ping        func(context.Context) error
	closeClient func() error
}

func (c *pingClient) Ping(ctx context.Context) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	cmd.SetVal("PONG")
	if c.ping != nil {
		cmd.SetErr(c.ping(ctx))
	}
	return cmd
}
func (c *pingClient) Close() error {
	if c.closeClient != nil {
		return c.closeClient()
	}
	return nil
}
func wrapRedis(client redis.UniversalClient) *runtimeStorage {
	return &runtimeStorage{kind: config.StorageRedis, redis: fiberredis.NewFromConnection(client), redisClient: client, keyPrefix: "test:uptime"}
}
func redisTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := testConfig(t, "http://never-resolves.invalid")
	cfg.Storage = config.StorageConfig{Type: config.StorageRedis, Redis: &config.RedisConfig{URL: &url.URL{Scheme: "redis", Host: "never-resolves.invalid"}, KeyPrefix: "test:uptime"}}
	return cfg
}

func TestRedisStorageMappingAndOwnership(t *testing.T) {
	closeErr := errors.New("injected close error")
	var closes atomic.Int32
	client := &pingClient{}
	store := wrapRedis(client)
	client.closeClient = func() error {
		closes.Add(1)
		if _, err := store.redis.Get("unused"); !errors.Is(err, fiberredis.ErrClosed) {
			t.Error("client closed before Fiber handle")
		}
		return closeErr
	}
	cfg := redisTestConfig(t)
	u := buildUptimeConfig(fiber.New(), store, cfg)
	if u.Storage != nil || u.Store != store.redis || u.StorageKeyPrefix != store.keyPrefix || u.ServiceID != "" || u.ServiceName != "" || u.ServiceDescription != "" {
		t.Fatal("Redis mapping")
	}
	// Switching the mapping cannot leave an inactive branch behind.
	b := wrapBbolt(&testStore{})
	b.applyToUptime(&u)
	if u.Storage != b.bbolt || u.Store != nil || u.StorageKeyPrefix != "" {
		t.Fatal("Redis branch retained in bbolt mapping")
	}
	store.applyToUptime(&u)
	if u.Storage != nil || u.Store != store.redis {
		t.Fatal("bbolt branch retained in Redis mapping")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if !errors.Is(store.Close(), closeErr) {
				t.Error("Close error lost")
			}
		})
	}
	wg.Wait()
	if closes.Load() != 1 {
		t.Fatal("owned client closed more than once")
	}
}

func TestRedisClientOptions(t *testing.T) {
	cfg := redisTestConfig(t)
	u, err := url.Parse("rediss://user:SECRET@never-resolves.invalid:6380/3?read_timeout=2s")
	must(t, err)
	cfg.Storage.Redis.URL = u
	store, err := openStorage(cfg.Storage)
	must(t, err)
	defer store.Close()
	client, ok := store.redisClient.(*redis.Client)
	if !ok {
		t.Fatal("expected owned go-redis client")
	}
	opts := client.Options()
	if !opts.ContextTimeoutEnabled || opts.DB != 3 || opts.Username != "user" || opts.Password != "SECRET" || opts.ReadTimeout != 2*time.Second || opts.TLSConfig == nil {
		t.Fatal("parsed Redis client options lost")
	}
	if store.redis.Conn() != client {
		t.Fatal("Fiber storage does not borrow the owned client")
	}
}

func TestStoragePreflight(t *testing.T) {
	for _, kind := range []config.StorageType{config.StorageBbolt, config.StorageRedis} {
		for _, outcome := range []string{"failure", "deadline", "cancel", "cancel with cleanup error"} {
			t.Run(string(kind)+"/"+outcome, func(t *testing.T) {
				cfg := testConfig(t, "http://never-resolves.invalid")
				if kind == config.StorageRedis {
					cfg = redisTestConfig(t)
				}
				s, err := New(cfg)
				must(t, err)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				cause := errors.New("redis://user:SECRET@host")
				closeErr := errors.New("close SECRET")
				closed := false
				ping := func(pingCtx context.Context) error {
					deadline, hasDeadline := pingCtx.Deadline()
					if kind == config.StorageRedis && (!hasDeadline || time.Until(deadline) <= 0 || time.Until(deadline) > redisStartupTimeout) {
						t.Error("Redis preflight missing 5s context")
					}
					if outcome == "deadline" {
						return context.DeadlineExceeded
					}
					if strings.HasPrefix(outcome, "cancel") {
						cancel()
						return pingCtx.Err()
					}
					return cause
				}
				closeFn := func() error {
					closed = true
					if outcome == "cancel with cleanup error" {
						return closeErr
					}
					return nil
				}
				s.deps.openStorage = func(config.StorageConfig) (*runtimeStorage, error) {
					if kind == config.StorageRedis {
						return wrapRedis(&pingClient{ping: ping, closeClient: closeFn}), nil
					}
					return wrapBbolt(&testStore{ping: ping, closeStore: closeFn}), nil
				}
				s.deps.listen = func(string, string) (net.Listener, error) {
					t.Error("listen reached after failed preflight")
					return nil, cause
				}
				err = s.Run(ctx)
				if !closed || s.app != nil || s.listener != nil {
					t.Fatal("preflight leaked resources or constructed runtime")
				}
				switch outcome {
				case "cancel":
					must(t, err)
				case "cancel with cleanup error":
					if !errors.Is(err, closeErr) {
						t.Fatal("cancellation lost cleanup error")
					}
				case "deadline":
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("startup timeout reported as success")
					}
				default:
					if !errors.Is(err, cause) {
						t.Fatal("preflight cause lost")
					}
				}
				if err != nil && strings.Contains(err.Error(), "SECRET") {
					t.Fatal("unsafe startup diagnostic")
				}
			})
		}
	}
}

func TestRedisAlreadyCanceled(t *testing.T) {
	s, err := New(redisTestConfig(t))
	must(t, err)
	s.deps.openStorage = func(config.StorageConfig) (*runtimeStorage, error) {
		t.Error("storage constructed")
		return nil, errors.New("unexpected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	must(t, s.Run(ctx))
	if s.store != nil || s.app != nil {
		t.Fatal("canceled Run created runtime state")
	}
}

func TestRedisReadinessRecovery(t *testing.T) {
	var fail atomic.Bool
	client := &pingClient{ping: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
			t.Error("readiness missing 1s context")
		}
		if fail.Load() {
			return errors.New("SECRET")
		}
		return nil
	}}
	s := &Server{app: fiber.New(), store: wrapRedis(client)}
	s.registerHealth()
	s.ready.Store(true)
	for _, failed := range []bool{false, true, false} {
		fail.Store(failed)
		status, body := 200, "ready\n"
		if failed {
			status, body = 503, "not ready\n"
		}
		response, err := s.app.Test(httptest.NewRequest("GET", "/readyz", nil))
		must(t, err)
		assertHealth(t, response, status, body)
		response.Body.Close()
		response, err = s.app.Test(httptest.NewRequest("GET", "/livez", nil))
		must(t, err)
		assertHealth(t, response, 200, "ok\n")
		response.Body.Close()
	}
	must(t, s.store.Close())
}
