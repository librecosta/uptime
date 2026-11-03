package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	"github.com/gofiber/fiber/v3/log"
	"github.com/librecosta/uptime/internal/alerts"
	"github.com/librecosta/uptime/internal/config"
	"github.com/redis/go-redis/v9"
)

const alertBufferCapacity = 256

// openAlertStore shares the monitoring resource owner, but never its schema.
// The Redis store borrows the existing client and must not close it.
func openAlertStore(ctx context.Context, resources *runtimeStorage, cfg config.Config) (alerts.Store, func() error, error) {
	if resources.kind == config.StorageRedis {
		client, ok := resources.redisClient.(*redis.Client)
		if !ok {
			return nil, nil, errors.New("alerts require the owned standalone Redis client")
		}
		store, err := alerts.NewRedisStore(client, resources.keyPrefix)
		if err == nil {
			checkCtx, cancel := context.WithTimeout(ctx, redisStartupTimeout)
			defer cancel()
			err = store.Check(checkCtx)
		}
		return store, func() error { return nil }, err
	}
	path := filepath.Join(filepath.Dir(cfg.Storage.Bbolt.Path), "alerts.db")
	monitor, err := filepath.Abs(cfg.Storage.Bbolt.Path)
	if err != nil {
		return nil, nil, err
	}
	alert, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	if monitor == alert || runtime.GOOS == "windows" && strings.EqualFold(monitor, alert) {
		return nil, nil, errors.New("alert and monitoring database paths must differ")
	}
	// Also reject aliases of an existing monitoring file, before taking another lock.
	if a, err := os.Stat(alert); err == nil {
		if m, err := os.Stat(monitor); err == nil && os.SameFile(a, m) {
			return nil, nil, errors.New("alert and monitoring database paths must differ")
		}
	}
	store, err := alerts.OpenBbolt(path, cfg.Server.ShutdownTimeout)
	if err != nil {
		return nil, nil, err
	}
	return store, store.Close, nil
}

type alertEvent struct {
	observation alerts.Observation
	sequence    uint64
}

// Upstream serializes callbacks for each endpoint, while different endpoints may
// call concurrently. Counters and the immutable lookup table share that boundary.
type alertIngressEndpoint struct{ sequence atomic.Uint64 }

type alertRuntime struct {
	engine     *alerts.Engine
	store      alerts.Store
	closeStore func() error
	policy     alerts.Policy
	endpoints  map[string]*alertIngressEndpoint
	queue      chan alertEvent
	drain      chan context.Context
	done       chan struct{}
	opTimeout  time.Duration
	gate       sync.RWMutex // Protects channel closure, never surrounds store/log I/O.
	accepting  bool
	dropped    atomic.Uint64
	lastLog    time.Time // Worker-owned; hook logging could block a probe.
	stopOnce   sync.Once
	stopErr    error
	cancel     context.CancelFunc
}

func newAlertRuntime(store alerts.Store, closeStore func() error, cfg config.Config, capacity int) (*alertRuntime, error) {
	engine, err := alerts.NewEngine(store, nil)
	if err != nil {
		return nil, err
	}
	r := &alertRuntime{
		engine: engine, store: store, closeStore: closeStore,
		policy:    alerts.Policy{FailureThreshold: cfg.Alerts.ConsecutiveFailures},
		endpoints: make(map[string]*alertIngressEndpoint, len(cfg.Endpoints)),
		queue:     make(chan alertEvent, capacity), drain: make(chan context.Context, 1),
		done: make(chan struct{}), accepting: true, opTimeout: cfg.Server.ShutdownTimeout,
	}
	for _, endpoint := range cfg.Endpoints {
		r.endpoints[endpoint.ID] = &alertIngressEndpoint{}
	}
	// Probe cancellation must not discard a completed observation. This context
	// belongs to the worker; stop supplies a separate deadline for draining it.
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.work(ctx)
	return r, nil
}

func (r *alertRuntime) hook(_ context.Context, result uptime.ProbeResult) {
	// Only stop takes the write lock, after upstream has joined its probes.
	// Even an unexpected late caller must never wait for ingress closure.
	if !r.gate.TryRLock() {
		return
	}
	defer r.gate.RUnlock()
	endpoint := r.endpoints[result.EndpointID]
	if !r.accepting || endpoint == nil {
		return
	}
	event := alertEvent{sequence: endpoint.sequence.Add(1), observation: alerts.Observation{
		EndpointID: result.EndpointID, CheckedAt: result.CheckedAt,
		Success: result.Success, StatusCode: result.StatusCode,
	}}
	select {
	case r.queue <- event:
	default:
		r.dropped.Add(1)
	}
}

func (r *alertRuntime) work(ctx context.Context) {
	defer close(r.done)
	last := make(map[string]uint64, len(r.endpoints))
	broken := make(map[string]bool, len(r.endpoints))
	for event := range r.queue {
		select {
		case ctx = <-r.drain:
		default:
		}
		if ctx.Err() != nil {
			break
		}
		// Redis clients can honor deadlines without immediately interrupting I/O
		// on cancellation. Bound normal operations too, including an operation
		// already in flight when shutdown starts; draining inherits its deadline.
		opCtx, cancel := context.WithTimeout(ctx, r.opTimeout)
		id := event.observation.EndpointID
		var err error
		if broken[id] || event.sequence != last[id]+1 {
			// Equal completion times let the real probe follow the gap without
			// being rejected by the engine's older-observation filter.
			_, err = r.engine.Observe(opCtx, alerts.Observation{EndpointID: id, CheckedAt: event.observation.CheckedAt, Gap: true}, r.policy)
		}
		if err == nil {
			_, err = r.engine.Observe(opCtx, event.observation, r.policy)
		}
		cancel()
		broken[id] = err != nil
		if err == nil {
			last[id] = event.sequence
		}
		r.report(err != nil, false)
	}
	r.report(false, true)
}

func (r *alertRuntime) report(failed, final bool) {
	dropped := r.dropped.Load()
	if (!failed && dropped == 0) || (!final && !r.lastLog.IsZero() && time.Since(r.lastLog) < 5*time.Second) {
		return
	}
	r.lastLog = time.Now()
	if n := r.dropped.Swap(0); n != 0 {
		log.Warnf("alert ingress dropped %d probe results; incomplete sequences reset pending failures", n)
	}
	if failed {
		log.Warn("alert storage operation failed; incomplete sequence retained")
	}
}

// stop runs only after upstream's shutdown hook has joined every probe. Never
// close persistence while an observation can still be using it. As with HTTP
// draining, uninterruptible disk/client I/O may outlast the requested deadline.
func (r *alertRuntime) stop(timeout time.Duration) error {
	r.stopOnce.Do(func() {
		drain, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		r.gate.Lock()
		r.accepting = false
		r.drain <- drain
		close(r.queue)
		r.gate.Unlock()
		stopCancel := context.AfterFunc(drain, r.cancel)
		defer stopCancel()
		<-r.done
		r.cancel()
		r.stopErr = errors.Join(safeError("alert drain timed out", drain.Err()), safeError("cannot close alert storage", r.closeStore()))
	})
	return r.stopErr
}
