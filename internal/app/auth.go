package app

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/basicauth"
	"github.com/librecosta/uptime/internal/config"
)

func isProtectedUIPath(path, uiPath string) bool {
	return path == uiPath || strings.HasPrefix(path, uiPath+"/")
}

// Contain only constructor rejection; Fiber owns request credential handling.
func newBasicAuth(cfg config.BasicAuthConfig, uiPath string) (handler fiber.Handler, err error) {
	defer func() {
		if recover() != nil {
			handler = nil
			err = errors.New("cannot initialize Basic Auth")
		}
	}()
	return basicauth.New(basicauth.Config{
		Users: map[string]string{cfg.Username: cfg.PasswordHash},
		Realm: "DeepFurry Uptime",
		Next:  func(c fiber.Ctx) bool { return !isProtectedUIPath(c.Path(), uiPath) },
	}), nil
}
