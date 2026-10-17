// Package app composes the Fiber/Uptime runtime and owns its storage lifetime.
// It also opens standalone storage sessions without listeners or monitoring workers.
// Both entry points require normalized configuration from package config.
package app

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"

	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/gofiber/fiber/v3"
	"github.com/librecosta/uptime/internal/config"
)

type runtimeStore interface {
	uptimestorage.Store
	Ping(context.Context) error
	RemoveService(context.Context, string) error
	Close() error
}

type dependencies struct {
	openStorage func(config.StorageConfig) (*runtimeStorage, error)
	listen      func(string, string) (net.Listener, error)
}

// Server is single-use. Its configuration must come from config.LoadFile and
// must not be mutated while the Server is in use.
type Server struct {
	cfg          config.Config
	ready        atomic.Bool
	started      atomic.Bool
	app          *fiber.App
	store        *runtimeStorage
	listener     *runtimeListener
	shutdownOnce sync.Once
	shutdownErr  error
	deps         dependencies
}

// New accepts normalized configuration and allocates memory only. Run owns all I/O.
func New(cfg config.Config) (*Server, error) {
	switch cfg.Storage.Type {
	case config.StorageBbolt:
		if cfg.Storage.Bbolt == nil {
			return nil, errors.New("bbolt storage configuration is required")
		}
	case config.StorageRedis:
		if cfg.Storage.Redis == nil || cfg.Storage.Redis.URL == nil {
			return nil, errors.New("Redis storage configuration is required")
		}
	default:
		return nil, errors.New("unsupported storage type")
	}
	return &Server{cfg: cfg, deps: dependencies{
		openStorage: openStorage,
		listen:      net.Listen,
	}}, nil
}

// Keep error provenance for callers without printing paths or secret-bearing
// values from an OS/upstream error. Joined errors retain each operation label.
type operationError struct {
	operation string
	cause     error
}

func (e *operationError) Error() string { return e.operation }
func (e *operationError) Unwrap() error { return e.cause }

// safeError exposes only a fixed operation label: upstream errors may include
// Redis/endpoint URLs, headers or hashes. Unwrap preserves errors.Is/errors.As;
// callers must not print the unwrapped cause to user-facing logs or diagnostics.
func safeError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return &operationError{operation: operation, cause: cause}
}
