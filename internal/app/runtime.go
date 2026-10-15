package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	recoverer "github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/librecosta/uptime/internal/config"
)

// Run opens storage, binds before constructing Uptime, and blocks until stopped.
// Cancellation is successful only when shutdown and resource cleanup succeed.
func (s *Server) Run(ctx context.Context) (result error) {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("server has already been run")
	}
	if ctx.Err() != nil {
		return nil
	}
	backend := "bbolt"
	if s.cfg.Storage.Type == config.StorageRedis {
		backend = "Redis"
	}
	store, err := s.deps.openStorage(s.cfg.Storage)
	if err != nil {
		return safeError("cannot open "+backend+" storage", err)
	}
	s.store = store
	defer func() { result = errors.Join(result, safeError("cannot close "+backend+" storage", store.Close())) }()
	pingCtx := ctx
	var cancelPing context.CancelFunc
	if store.kind == config.StorageRedis {
		pingCtx, cancelPing = context.WithTimeout(ctx, redisStartupTimeout)
	}
	err = store.Ping(pingCtx)
	if cancelPing != nil {
		cancelPing()
	}
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return safeError("cannot connect "+backend+" storage", err)
	}
	ln, err := s.deps.listen("tcp", s.cfg.Server.Address)
	if err != nil {
		return safeError("cannot bind HTTP listener", err)
	}
	s.listener = wrapListener(ln)
	defer func() { result = errors.Join(result, safeError("cannot close HTTP listener", s.listener.Close())) }()
	if ctx.Err() != nil {
		return nil
	}

	serveListener := net.Listener(s.listener)
	if s.cfg.Server.TLS != nil {
		// Track raw transports inside TLS so stalled handshakes can be
		// force-closed and drained before releasing persistence.
		serveListener = tls.NewListener(s.listener, buildTLSConfig(s.cfg.Server.TLS))
	}
	s.app = fiber.New(fiber.Config{AppName: "LibreCosta Uptime"})
	s.app.Use(recoverer.New())
	s.registerHealth()
	s.app.Use(func(c fiber.Ctx) error {
		if c.Path() != "/" || (c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead) {
			return c.Next()
		}
		return c.Redirect().Status(fiber.StatusFound).To(s.cfg.UI.Path)
	})
	s.app.Hooks().OnPreShutdown(func() error { s.ready.Store(false); return nil })
	// Covers constructor rejection and every subsequent failure path.
	defer func() { result = errors.Join(result, s.shutdown()) }()
	if s.cfg.Auth != nil {
		auth, err := newBasicAuth(*s.cfg.Auth, s.cfg.UI.Path)
		if err != nil {
			return err
		}
		s.app.Use(auth)
	}
	handler, err := newUptime(buildUptimeConfig(s.app, store, s.cfg))
	if err != nil {
		return err
	}
	s.app.Use(handler)
	s.app.Hooks().OnListen(func(fiber.ListenData) error {
		s.ready.Store(true)
		log.Info(startupMessage(s.listener.Addr().String(), s.cfg.UI.Path, s.cfg.Server.TLS != nil))
		return nil
	})

	finished := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			// Shutdown before Serve registers ln can otherwise miss the listener.
			select {
			case <-s.listener.accepting:
				_ = s.shutdown()
			case <-finished:
			}
		case <-finished:
		}
	}()
	err = s.app.Listener(serveListener, fiber.ListenConfig{DisableStartupMessage: true})
	unrequested := ctx.Err() == nil
	close(finished)
	shutdownErr := s.shutdown() // sync.Once also waits for a watcher in shutdown.
	<-watcherDone
	if unrequested {
		if err == nil {
			err = errors.New("listener returned without a shutdown request")
		}
		result = safeError("HTTP serving stopped unexpectedly", err)
	} else if err != nil && !errors.Is(err, context.Canceled) {
		result = safeError("HTTP serving failed", err)
	}
	// The deferred shutdown returns this same error once, alongside Close errors.
	if result == nil && shutdownErr == nil {
		log.Info("LibreCosta Uptime server stopped")
	}
	return result
}

func (s *Server) shutdown() error {
	s.shutdownOnce.Do(func() {
		s.ready.Store(false)
		err := s.app.ShutdownWithTimeout(s.cfg.Server.ShutdownTimeout)
		if err != nil {
			// fasthttp can return a deadline while handlers still use the store.
			// Force connection I/O closed, then drain before releasing storage.
			s.listener.closeConnections()
			s.listener.drained.Wait()
		}
		s.shutdownErr = safeError("HTTP shutdown failed", err)
	})
	return s.shutdownErr
}
