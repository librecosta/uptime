package alerts

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Use the existing opt-in URL and an owned prefix; never clear a database.
func alertRedis(t *testing.T) (*RedisStore, *redis.Client) {
	t.Helper()
	value := os.Getenv("UPTIME_TEST_REDIS_URL")
	if value == "" {
		t.Skip("UPTIME_TEST_REDIS_URL not configured")
	}
	opts, err := redis.ParseURL(value)
	if err != nil {
		t.Fatal("invalid Redis integration URL")
	}
	opts.ContextTimeoutEnabled = true
	opts.MaxRetries = -1
	client := redis.NewClient(opts)
	prefix := "librecosta:test:" + rand.Text()
	s, err := NewRedisStore(client, prefix)
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Every key created by this fixture is known; no scan or global reset.
		must(t, client.Del(ctx, s.incidents, s.active, s.outbox, prefix+":monitor").Err())
		must(t, client.Close())
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal("Redis integration instance unavailable")
	}
	return s, client
}

func TestRedisIntegrationAlerts(t *testing.T) {
	s, client := alertRedis(t)
	exerciseStore(t, s)
	ctx := context.Background()
	for _, key := range []string{s.incidents, s.active, s.outbox} {
		ttl, err := client.TTL(ctx, key).Result()
		must(t, err)
		// Redis deletes a hash when its final field is removed. Only the empty
		// active index may be absent; durable incidents/tasks must still exist.
		if ttl != -1 && !(key == s.active && ttl == -2) {
			t.Fatalf("durable alert key has TTL %v", ttl)
		}
	}
	other, _ := alertRedis(t)
	if i, err := other.Active(ctx, "race"); err != nil || i != nil {
		t.Fatal("prefix isolation failed")
	}
	tasks, err := other.Outbox(ctx)
	must(t, err)
	if len(tasks) != 0 {
		t.Fatal("outbox crossed prefixes")
	}
	monitorKey := strings.TrimSuffix(s.incidents, "alerts:v1:incidents") + "monitor"
	must(t, client.Set(ctx, monitorKey, "untouched", 0).Err())
	_, err = s.Apply(ctx, Transition{Down, testIncident("isolation", "isolated")})
	must(t, err)
	value, err := client.Get(ctx, monitorKey).Result()
	must(t, err)
	if value != "untouched" {
		t.Fatal("changed monitoring key")
	}
	// Reconstructing the store and engine must restore FIRING without memory.
	reopened, err := NewRedisStore(client, strings.TrimSuffix(s.incidents, ":alerts:v1:incidents"))
	must(t, err)
	e, err := NewEngine(reopened, nil)
	must(t, err)
	state, err := e.Observe(ctx, Observation{EndpointID: "isolated", CheckedAt: testTime.Add(3 * time.Second), Success: true}, Policy{ChannelIDs: []string{"new"}})
	must(t, err)
	if state != Healthy {
		t.Fatal("Redis firing recovery failed")
	}
}

func TestRedisIntegrationAlertsAtomicFailure(t *testing.T) {
	// Mark the whole integration test skipped when no explicit instance exists.
	if os.Getenv("UPTIME_TEST_REDIS_URL") == "" {
		t.Skip("UPTIME_TEST_REDIS_URL not configured")
	}
	ctx := context.Background()
	for _, kind := range []EventType{Down, Recovered} {
		t.Run(string(kind), func(t *testing.T) {
			s, client := alertRedis(t)
			i := testIncident("atomic", "api")
			if kind == Recovered {
				_, err := s.Apply(ctx, Transition{Down, i})
				must(t, err)
				i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
			}
			must(t, client.Set(ctx, s.outbox, "wrong type", 0).Err())
			if _, err := s.Apply(ctx, Transition{kind, i}); err == nil {
				t.Fatal("invalid outbox did not abort transaction")
			}
			stored, err := s.Get(ctx, i.ID)
			must(t, err)
			if kind == Down && stored != nil {
				t.Fatal("partial DOWN")
			}
			if kind == Recovered && (stored == nil || !stored.RecoveredAt.IsZero()) {
				t.Fatal("partial recovery")
			}
		})
	}
}

type lostExecReply struct{ fail atomic.Bool }

// Force another executor to recover between the incident and active-index reads.
// The losing reader must revalidate WATCH rather than report a false conflict.
type interleavedRecovery struct {
	activeKey string
	once      atomic.Bool
	recover   func() error
}

func (h *interleavedRecovery) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *interleavedRecovery) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *interleavedRecovery) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "hget" && len(cmd.Args()) > 1 && cmd.Args()[1] == h.activeKey && h.once.CompareAndSwap(false, true) {
			if err := h.recover(); err != nil {
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func TestRedisIntegrationRecoveryTornRead(t *testing.T) {
	s, client := alertRedis(t)
	ctx := context.Background()
	i := testIncident("interleaved", "api")
	_, err := s.Apply(ctx, Transition{Down, i})
	must(t, err)
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	opts := *client.Options()
	otherClient := redis.NewClient(&opts)
	defer otherClient.Close()
	other, err := NewRedisStore(otherClient, strings.TrimSuffix(s.incidents, ":alerts:v1:incidents"))
	must(t, err)
	h := &interleavedRecovery{activeKey: s.active, recover: func() error { _, err := other.Apply(ctx, Transition{Recovered, i}); return err }}
	client.AddHook(h)
	_, err = s.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	if !h.once.Load() {
		t.Fatal("interleaving was not exercised")
	}
	tasks, err := s.Outbox(ctx)
	must(t, err)
	if len(tasks) != 4 {
		t.Fatal("interleaved recovery duplicated or lost tasks")
	}
	// A stable conflicting request must still fail, not be accepted as a retry.
	i.FirstFailureAt = i.FirstFailureAt.Add(-time.Second)
	if _, err := s.Apply(ctx, Transition{Recovered, i}); !errors.Is(err, ErrConflict) {
		t.Fatal("real conflict accepted")
	}
}

func (h *lostExecReply) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (h *lostExecReply) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h *lostExecReply) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		err := next(ctx, commands)
		for _, command := range commands {
			if command.Name() == "hset" && err == nil && h.fail.CompareAndSwap(true, false) {
				return errInjected
			}
		}
		return err
	}
}

func TestRedisIntegrationAlertsUncertainCommit(t *testing.T) {
	s, client := alertRedis(t)
	hook := &lostExecReply{}
	client.AddHook(hook)
	ctx := context.Background()
	tr := Transition{Down, testIncident("uncertain", "api")}
	hook.fail.Store(true)
	_, err := s.Apply(ctx, tr)
	if !errors.Is(err, errInjected) {
		t.Fatalf("missing injected lost EXEC response: %v", err)
	}
	i, err := s.Get(ctx, tr.Incident.ID)
	must(t, err)
	if i == nil {
		t.Fatal("test did not commit before losing response")
	}
	_, err = s.Apply(ctx, tr)
	must(t, err)
	tasks, err := s.Outbox(ctx)
	must(t, err)
	if len(tasks) != 2 {
		t.Fatal("uncertain DOWN duplicated tasks")
	}
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	hook.fail.Store(true)
	e, err := NewEngine(s, nil)
	must(t, err)
	state, err := e.Observe(ctx, Observation{EndpointID: "api", CheckedAt: i.RecoveredAt, Success: true}, Policy{})
	must(t, err)
	if state != Healthy {
		t.Fatal("uncertain recovery not reconciled")
	}
	tasks, err = s.Outbox(ctx)
	must(t, err)
	if len(tasks) != 4 {
		t.Fatal("uncertain recovery duplicated tasks")
	}
}

func TestRedisIntegrationDelivery(t *testing.T) {
	s, _ := alertRedis(t)
	exerciseDelivery(t, s)
}

func TestRedisIntegrationDeliveryConcurrency(t *testing.T) {
	s, _ := alertRedis(t)
	exerciseDeliveryConcurrency(t, s)
}

func TestRedisIntegrationDeliveryLegacyAndUncertainCommit(t *testing.T) {
	s, client := alertRedis(t)
	ctx := context.Background()
	i := testIncident("legacy-outcome", "api")
	_, err := s.Apply(ctx, Transition{Down, i})
	must(t, err)
	task := readTask(t, s, i, Down, "a")
	must(t, client.HSet(ctx, s.outbox, task.EventID, legacyTaskJSON(t, task)).Err())
	hook := &lostExecReply{}
	client.AddHook(hook)
	o := deliveryOutcome(task, TaskPending, i.ConfirmedAt)
	hook.fail.Store(true)
	if _, err := s.RecordOutcome(ctx, o); !errors.Is(err, errInjected) {
		t.Fatal("lost EXEC response was hidden")
	}
	// Recreate the store as on restart, borrowing the same owned client.
	reopened, err := NewRedisStore(client, strings.TrimSuffix(s.incidents, ":alerts:v1:incidents"))
	must(t, err)
	persisted := readTask(t, reopened, i, Down, "a")
	matched, err := persisted.HasOutcome(o)
	must(t, err)
	if !matched || persisted.Attempts != 1 {
		t.Fatal("lost response was not reconcilable after reopening")
	}
	_, err = reopened.RecordOutcome(ctx, o)
	must(t, err)
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	_, err = reopened.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	failed := deliveryOutcome(persisted, TaskFailed, persisted.NextAttemptAt)
	hook.fail.Store(true)
	if _, err := reopened.RecordOutcome(ctx, failed); !errors.Is(err, errInjected) {
		t.Fatal("missing lost failed-outcome response")
	}
	if readTask(t, reopened, i, Recovered, "a").Status != TaskSkipped {
		t.Fatal("lost response left partial skip")
	}
	_, err = reopened.RecordOutcome(ctx, failed)
	must(t, err)
	if readTask(t, reopened, i, Down, "a").Attempts != 2 {
		t.Fatal("replayed outcome incremented attempts")
	}
	other, _ := alertRedis(t)
	if task, err := other.GetTask(ctx, persisted.EventID); err != nil || task != nil {
		t.Fatal("task query crossed namespaces")
	}
	if len(allDue(t, other, testTime.Add(time.Hour), 1)) != 0 {
		t.Fatal("due query crossed namespaces")
	}
	for _, key := range []string{s.incidents, s.active, s.outbox} {
		ttl, err := client.TTL(ctx, key).Result()
		must(t, err)
		if ttl != -1 && !(key == s.active && ttl == -2) {
			t.Fatal("delivery introduced a TTL")
		}
	}
}

func TestRedisIntegrationDeliveryAtomicFailure(t *testing.T) {
	s, client := alertRedis(t)
	ctx := context.Background()
	i := testIncident("delivery-rollback", "api")
	_, err := s.Apply(ctx, Transition{Down, i})
	must(t, err)
	i.RecoveredAt = i.ConfirmedAt.Add(time.Second)
	_, err = s.Apply(ctx, Transition{Recovered, i})
	must(t, err)
	down := readTask(t, s, i, Down, "a")
	id := eventID(i.ID, Recovered, "a")
	must(t, client.HSet(ctx, s.outbox, id, "corrupt SECRET data").Err())
	if _, err := s.RecordOutcome(ctx, deliveryOutcome(down, TaskFailed, i.RecoveredAt)); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid recovery accepted")
	}
	if task := readTask(t, s, i, Down, "a"); task.Attempts != 0 || task.Status != TaskPending {
		t.Fatal("partial Redis outcome")
	}
}
