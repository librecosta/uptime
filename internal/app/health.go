package app

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"
)

func healthResponse(c fiber.Ctx, status int, body string) error {
	c.Set(fiber.HeaderContentType, "text/plain; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Status(status).SendString(body)
}

// registerHealth reports process/storage readiness, not monitored endpoint health.
// It is registered before authentication so health checks remain public.
func (s *Server) registerHealth() {
	s.app.Get("/livez", func(c fiber.Ctx) error { return healthResponse(c, fiber.StatusOK, "ok\n") })
	s.app.Get("/readyz", func(c fiber.Ctx) error {
		if s.ready.Load() {
			ctx, cancel := context.WithTimeout(c.Context(), time.Second)
			defer cancel()
			if s.store.Ping(ctx) == nil {
				return healthResponse(c, fiber.StatusOK, "ready\n")
			}
		}
		return healthResponse(c, fiber.StatusServiceUnavailable, "not ready\n")
	})
}
