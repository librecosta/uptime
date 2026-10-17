package app

import (
	"context"
	"errors"

	"github.com/gofiber/contrib/v3/uptime"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/librecosta/uptime/internal/config"
)

// ServiceStorage provides standalone history access without an HTTP server or
// monitoring workers. Use OpenServiceStorage; do not copy it. Queries retain the
// backend's consistency guarantees, not a transaction across separate calls.
type ServiceStorage struct {
	store     uptimestorage.Store
	resources *runtimeStorage
}

// OpenServiceStorage accepts normalized configuration from config.LoadFile and
// opens only its selected storage. bbolt requires exclusive file access; Redis
// initialization uses the caller's context with a five-second upper bound.
// Context cancellation cannot forcibly interrupt all underlying disk/network I/O.
// The caller owns the result and must Close it after all operations have stopped.
func OpenServiceStorage(ctx context.Context, cfg config.Config) (*ServiceStorage, error) {
	if err := ctx.Err(); err != nil {
		return nil, safeError("cannot open service storage", err)
	}
	resources, err := openStorage(cfg.Storage)
	if err != nil {
		return nil, safeError("cannot open service storage", err)
	}
	return newServiceStorage(ctx, cfg.Uptime.Retention, resources)
}

// newServiceStorage takes ownership of resources, including on failure.
func newServiceStorage(ctx context.Context, retention config.CalendarDays, resources *runtimeStorage) (_ *ServiceStorage, result error) {
	defer func() {
		if result != nil {
			result = errors.Join(result, safeError("cannot close service storage", resources.Close()))
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, safeError("cannot initialize service storage", err)
	}
	var store uptimestorage.Store = resources.bbolt
	if resources.kind == config.StorageRedis {
		initCtx, cancel := context.WithTimeout(ctx, redisStartupTimeout)
		defer cancel()
		var err error
		store, err = uptime.NewRedisStore(initCtx, uptime.RedisStoreConfig{
			Store: resources.redis, StorageKeyPrefix: resources.keyPrefix, RetentionDays: int(retention),
		})
		if err != nil {
			return nil, safeError("cannot initialize Redis service storage", err)
		}
		if err := initCtx.Err(); err != nil {
			return nil, safeError("cannot initialize Redis service storage", err)
		}
	}
	return &ServiceStorage{store: store, resources: resources}, nil
}

// ListServices returns the backend's registered services.
func (s *ServiceStorage) ListServices(ctx context.Context) ([]uptimestorage.Service, error) {
	rows, err := s.store.ListServices(ctx)
	return rows, safeError("cannot list services", err)
}

// QueryDaily returns daily history using the upstream query contract.
func (s *ServiceStorage) QueryDaily(ctx context.Context, options uptimestorage.QueryDailyOptions) ([]uptimestorage.DailyStatus, error) {
	rows, err := s.store.QueryDaily(ctx, options)
	return rows, safeError("cannot query daily service history", err)
}

// QueryTodaySamples returns samples for the requested day and services.
func (s *ServiceStorage) QueryTodaySamples(ctx context.Context, options uptimestorage.QueryTodaySamplesOptions) ([]uptimestorage.TodaySampleStatus, error) {
	rows, err := s.store.QueryTodaySamples(ctx, options)
	return rows, safeError("cannot query service samples", err)
}

// RemoveService explicitly removes a service and its history. It does not check
// whether a monitor is active; callers must coordinate deletion with writers.
// Atomicity follows the selected backend's contract.
func (s *ServiceStorage) RemoveService(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return safeError("cannot remove service", err)
	}
	var err error
	if s.resources.kind == config.StorageRedis {
		err = uptime.RemoveService(ctx, s.resources.redis, s.resources.keyPrefix, id)
	} else {
		err = s.resources.bbolt.RemoveService(ctx, id)
	}
	return safeError("cannot remove service", err)
}

// Close releases this session's resources once. It must follow all queries and
// deletions; Redis closes its borrowed Fiber handle before its owned client.
func (s *ServiceStorage) Close() error {
	return safeError("cannot close service storage", s.resources.Close())
}
