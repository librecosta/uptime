package app

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/basicauth"
	"github.com/librecosta/uptime/internal/config"
)

func withinPath(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// Contain only constructor rejection; Fiber owns request credential handling.
func newBasicAuth(cfg config.Config) (handler fiber.Handler, err error) {
	defer func() {
		if recover() != nil {
			handler = nil
			err = errors.New("cannot initialize Basic Auth")
		}
	}()
	return basicauth.New(basicauth.Config{
		Users: map[string]string{cfg.Auth.Username: cfg.Auth.PasswordHash},
		Realm: "LibreCosta Uptime",
		Next: func(c fiber.Ctx) bool {
			path := c.Path() // Use the same Fiber-normalized path as both upstream handlers.
			return !(cfg.Auth.ProtectUptime && withinPath(path, cfg.UI.Path) ||
				cfg.Monitor.Enabled && withinPath(path, monitorPath) || withinPath(path, "/alerts"))
		},
	}), nil
}
