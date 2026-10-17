package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	fiberredis "github.com/gofiber/storage/redis/v3"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/storage/bbolt"
	"github.com/redis/go-redis/v9"
)

const redisStartupTimeout = 5 * time.Second

// runtimeStorage owns exactly one backend. Redis persistence itself belongs to
// Fiber Uptime; this type only owns handles, health checks and configuration wiring.
// The go-redis client is owned here; the Fiber handle borrows that same client.
type runtimeStorage struct {
	kind        config.StorageType
	bbolt       runtimeStore
	redis       *fiberredis.Storage
	redisClient redis.UniversalClient
	keyPrefix   string
	closeOnce   sync.Once
	closeErr    error
}

func openStorage(cfg config.StorageConfig) (*runtimeStorage, error) {
	return openStorageWithExistingBbolt(cfg, false)
}

func openStorageWithExistingBbolt(cfg config.StorageConfig, mustExist bool) (*runtimeStorage, error) {
	switch cfg.Type {
	case config.StorageBbolt:
		store, err := bbolt.Open(bbolt.Config{Path: cfg.Bbolt.Path, MustExist: mustExist})
		if err != nil {
			return nil, err
		}
		return &runtimeStorage{kind: cfg.Type, bbolt: store}, nil
	case config.StorageRedis:
		opts, err := redis.ParseURL(cfg.Redis.URL.String())
		if err != nil {
			return nil, safeError("invalid Redis connection configuration", err)
		}
		// Enable context-aware Redis I/O for preflight and readiness; this does
		// not make every underlying network operation forcibly interruptible.
		opts.ContextTimeoutEnabled = true
		client := redis.NewClient(opts)
		return &runtimeStorage{kind: cfg.Type, redis: fiberredis.NewFromConnection(client), redisClient: client, keyPrefix: cfg.Redis.KeyPrefix}, nil
	default:
		return nil, errors.New("unsupported storage type")
	}
}

func (s *runtimeStorage) Ping(ctx context.Context) error {
	if s.kind == config.StorageRedis {
		return s.redisClient.Ping(ctx).Err()
	}
	return s.bbolt.Ping(ctx)
}

func (s *runtimeStorage) Close() error {
	s.closeOnce.Do(func() {
		if s.kind == config.StorageRedis {
			// NewFromConnection borrows its client. Close both, in order, even
			// if one fails; runtime or standalone session operations must have stopped.
			wrapperErr := s.redis.Close()
			clientErr := s.redisClient.Close()
			s.closeErr = errors.Join(wrapperErr, clientErr)
		} else {
			s.closeErr = s.bbolt.Close()
		}
	})
	return s.closeErr
}

func (s *runtimeStorage) applyToUptime(u *uptime.Config) {
	u.Storage, u.Store, u.StorageKeyPrefix = nil, nil, ""
	if s.kind == config.StorageRedis {
		u.Store, u.StorageKeyPrefix = s.redis, s.keyPrefix
	} else {
		u.Storage = s.bbolt
	}
}
