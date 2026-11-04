package app

import (
	"errors"

	"github.com/gofiber/contrib/v3/monitor"
	"github.com/gofiber/fiber/v3"
	recoverer "github.com/gofiber/fiber/v3/middleware/recover"
)

const monitorPath = "/monitor"

// registerMiddleware keeps one access policy but applies Monitor's guard before
// its terminating handler. Other requests reach auth inside Monitor/Recover so
// their final statuses, including recovered panics and 401s, are counted.
func (s *Server) registerMiddleware() error {
	var auth fiber.Handler
	if s.cfg.Auth != nil {
		var err error
		auth, err = newBasicAuth(s.cfg)
		if err != nil {
			return err
		}
	}
	if s.cfg.Monitor.Enabled {
		// Normal configuration loading rejects this; fail closed for internal callers too.
		if auth == nil {
			return errors.New("Monitor requires Basic Auth")
		}
		s.app.Use(func(c fiber.Ctx) error {
			if withinPath(c.Path(), monitorPath) {
				return auth(c)
			}
			return c.Next()
		})
		s.app.Use(monitor.New(monitor.Config{
			Next: func(c fiber.Ctx) bool { return !withinPath(c.Path(), monitorPath) },
		})) // Retain native UI, 3s refresh/cache, and disabled exact GC pause metrics.
	}
	s.app.Use(recoverer.New())
	s.registerHealth()
	s.app.Use(func(c fiber.Ctx) error {
		if c.Path() != "/" || (c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead) {
			return c.Next()
		}
		return c.Redirect().Status(fiber.StatusFound).To(s.cfg.UI.Path)
	})
	if auth != nil {
		s.app.Use(auth)
	}
	return nil
}
