package app

import (
	"context"
	"errors"
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
	fiberredis "github.com/gofiber/storage/redis/v3"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/storage/bbolt"
	bolt "go.etcd.io/bbolt"
)

// Seed through public Store operations, never through backend keys or buckets.
func seedServiceHistory(t *testing.T, store uptimestorage.Store, id string, instance int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	must(t, store.UpsertService(ctx, uptimestorage.Service{ID: id, Name: id, CreatedAt: now, SampleInterval: time.Minute}))
	must(t, store.UpsertInstance(ctx, uptimestorage.Instance{ID: instance, ServiceID: id, StartedAt: now}))
	for _, day := range []string{"2026-10-15", "2026-10-16"} {
		must(t, store.WriteHeartbeat(ctx, uptimestorage.Heartbeat{ServiceID: id, InstanceID: instance, Day: day, Slot: 0, SeenAt: now}))
	}
	must(t, store.RollupDaily(ctx, uptimestorage.RollupOptions{BeforeDay: "2026-10-16", ExpectedSlots: func(uptimestorage.Service, string) int { return 1440 }}))
}

func assertServiceHistory(t *testing.T, session *ServiceStorage, id string, present bool) {
	t.Helper()
	ctx := context.Background()
	services, err := session.ListServices(ctx)
	must(t, err)
	found := false
	for _, service := range services {
		found = found || service.ID == id
	}
	if found != present {
		t.Fatalf("service %s present = %v, want %v", id, found, present)
	}
	daily, err := session.QueryDaily(ctx, uptimestorage.QueryDailyOptions{ServiceIDs: []string{id}})
	must(t, err)
	samples, err := session.QueryTodaySamples(ctx, uptimestorage.QueryTodaySamplesOptions{ServiceIDs: []string{id}, Day: "2026-10-16"})
	must(t, err)
	if present {
		if len(daily) != 1 || daily[0].UpSlots != 1 || daily[0].ExpectedSlots != 1440 || !daily[0].Finalized || len(samples) != 1 || samples[0].UpSlots != 1 {
			t.Fatal("stored history changed")
		}
	} else if len(daily) != 0 || len(samples) != 0 {
		t.Fatal("removed service history remains")
	}
}

func TestServiceStorageBbolt(t *testing.T) {
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer target.Close()
	cfg := testConfig(t, target.URL)
	// Occupied address: independent access must not bind a listener.
	cfg.Server.Address = target.Listener.Addr().String()
	seed, err := bbolt.Open(bbolt.Config{Path: cfg.Storage.Bbolt.Path})
	must(t, err)
	seedServiceHistory(t, seed, "history", 1)
	seedServiceHistory(t, seed, "keep", 2)
	must(t, seed.Close())
	session, err := OpenServiceStorage(context.Background(), cfg)
	must(t, err)
	t.Cleanup(func() { must(t, session.Close()) })
	assertServiceHistory(t, session, "history", true)
	must(t, session.RemoveService(context.Background(), "history"))
	assertServiceHistory(t, session, "history", false)
	assertServiceHistory(t, session, "keep", true)
	services, err := session.ListServices(context.Background())
	must(t, err)
	if len(services) != 1 || requests.Load() != 0 {
		t.Fatal("standalone access started monitoring or deleted unrelated history")
	}
	must(t, session.Close())
	must(t, session.Close())
	if _, err := session.ListServices(context.Background()); !errors.Is(err, bolt.ErrDatabaseNotOpen) {
		t.Fatal("session did not close its database")
	}
	reopened, err := bbolt.Open(bbolt.Config{Path: cfg.Storage.Bbolt.Path})
	must(t, err)
	must(t, reopened.Ping(context.Background()))
	must(t, reopened.Close())
}

func TestServiceStorageBboltLock(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t, "http://never-resolves.invalid")
	owner, err := bbolt.Open(bbolt.Config{Path: cfg.Storage.Bbolt.Path})
	must(t, err)
	defer owner.Close()
	seedServiceHistory(t, owner, "locked", 1)
	session, err := OpenServiceStorage(context.Background(), cfg)
	if session != nil || !errors.Is(err, bolt.ErrTimeout) {
		t.Fatalf("file lock was bypassed: %v", err)
	}
	must(t, owner.Close())
	session, err = OpenServiceStorage(context.Background(), cfg)
	must(t, err)
	defer session.Close()
	assertServiceHistory(t, session, "locked", true)
}

func TestServiceStorageCanceledBeforeOpen(t *testing.T) {
	for _, cfg := range []config.Config{testConfig(t, "http://never-resolves.invalid"), redisTestConfig(t)} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		session, err := OpenServiceStorage(ctx, cfg)
		if session != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled open did not return context error")
		}
		if cfg.Storage.Bbolt != nil {
			if _, err := os.Stat(filepath.Dir(cfg.Storage.Bbolt.Path)); !os.IsNotExist(err) {
				t.Fatal("canceled open created database directory")
			}
		}
	}
}

func TestServiceStorageOpenFailure(t *testing.T) {
	cfg := testConfig(t, "http://never-resolves.invalid")
	cfg.Storage.Bbolt.Path = filepath.Join(t.TempDir(), "SECRET.db")
	// An existing invalid file must never be adopted or rewritten.
	must(t, os.WriteFile(cfg.Storage.Bbolt.Path, nil, 0600))
	session, err := OpenServiceStorage(context.Background(), cfg)
	if session != nil || err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("missing or unsafe open error")
	}
	info, err := os.Stat(cfg.Storage.Bbolt.Path)
	must(t, err)
	if info.Size() != 0 {
		t.Fatal("existing invalid file changed")
	}
}

type serviceQueryFailure struct {
	uptimestorage.Store
	cause error
}

func (s serviceQueryFailure) ListServices(context.Context) ([]uptimestorage.Service, error) {
	return nil, s.cause
}
func (s serviceQueryFailure) QueryDaily(context.Context, uptimestorage.QueryDailyOptions) ([]uptimestorage.DailyStatus, error) {
	return nil, s.cause
}
func (s serviceQueryFailure) QueryTodaySamples(context.Context, uptimestorage.QueryTodaySamplesOptions) ([]uptimestorage.TodaySampleStatus, error) {
	return nil, s.cause
}

func TestServiceStorageSafeQueryAndDeleteErrors(t *testing.T) {
	cause := errors.New("redis://user:SECRET@host")
	session := &ServiceStorage{
		store:     serviceQueryFailure{cause: cause},
		resources: wrapRedis(&pingClient{ping: func(context.Context) error { return cause }}),
	}
	defer session.Close()
	ctx := context.Background()
	_, listErr := session.ListServices(ctx)
	_, dailyErr := session.QueryDaily(ctx, uptimestorage.QueryDailyOptions{})
	_, todayErr := session.QueryTodaySamples(ctx, uptimestorage.QueryTodaySamplesOptions{})
	removeErr := session.RemoveService(ctx, "SECRET")
	for _, err := range []error{listErr, dailyErr, todayErr, removeErr} {
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("missing or unsafe storage error: %v", err)
		}
	}
}

func TestServiceStorageInitializationAndOwnership(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "canceled", "overflow"} {
		t.Run(outcome, func(t *testing.T) {
			cause, closeErr := errors.New("redis://user:SECRET@host"), errors.New("close SECRET")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var pings, closes atomic.Int32
			client := &pingClient{ping: func(ctx context.Context) error {
				pings.Add(1)
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > redisStartupTimeout {
					t.Error("factory initialization is not bounded")
				}
				if outcome == "canceled" {
					cancel()
				}
				if outcome != "success" {
					return cause
				}
				return nil
			}}
			resources := wrapRedis(client)
			client.closeClient = func() error {
				closes.Add(1)
				if _, err := resources.redis.Get("unused"); !errors.Is(err, fiberredis.ErrClosed) {
					t.Error("client closed before Fiber handle")
				}
				return closeErr
			}
			retention := config.CalendarDays(7)
			if outcome == "overflow" {
				retention = 106751
			}
			session, err := newServiceStorage(ctx, retention, resources)
			if outcome == "success" {
				must(t, err)
				if closes.Load() != 0 || pings.Load() != 1 {
					t.Fatal("factory closes resources or performs extra preflight")
				}
				var wg sync.WaitGroup
				for range 4 {
					wg.Go(func() {
						err := session.Close()
						if !errors.Is(err, closeErr) || strings.Contains(err.Error(), "SECRET") {
							t.Error("unsafe or missing close error")
						}
					})
				}
				wg.Wait()
			} else {
				want := cause
				if outcome == "overflow" {
					want = uptime.ErrRetentionDaysOverflow
				}
				if session != nil || !errors.Is(err, want) || !errors.Is(err, closeErr) || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("initialization lost safe error chain: %v", err)
				}
				if outcome == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatal("context cancellation lost")
				}
			}
			if closes.Load() != 1 {
				t.Fatal("resources not closed exactly once")
			}
		})
	}
}
