package app

import (
	"errors"
	"maps"
	"slices"

	"github.com/gofiber/contrib/v3/uptime"
	"github.com/gofiber/fiber/v3"
	"github.com/librecosta/uptime/internal/config"
)

func buildUptimeConfig(app *fiber.App, store *runtimeStorage, cfg config.Config) uptime.Config {
	u := uptime.Config{
		App:            app,
		SampleInterval: cfg.Uptime.Interval,
		RetentionDays:  int(cfg.Uptime.Retention), DaysToShow: int(cfg.Uptime.Window),
		Timezone: cfg.Uptime.Timezone,
		UI: uptime.UIConfig{
			Path: cfg.UI.Path, Title: cfg.UI.Title, Description: cfg.UI.Description, Footer: cfg.UI.Footer,
			GreenThreshold: cfg.UI.Thresholds.Green, YellowThreshold: cfg.UI.Thresholds.Yellow,
		},
		Endpoints: make([]uptime.EndpointConfig, len(cfg.Endpoints)),
	}
	store.applyToUptime(&u)
	if cfg.UI.FaviconURL != nil {
		u.UI.FaviconURL = cfg.UI.FaviconURL.String()
	}
	for i, endpoint := range cfg.Endpoints {
		u.Endpoints[i] = uptime.EndpointConfig{
			ID: endpoint.ID, Name: endpoint.Name, Description: endpoint.Description,
			URL: endpoint.URL.String(), Method: endpoint.Method,
			Interval: endpoint.Interval, Timeout: endpoint.Timeout,
			Headers: maps.Clone(endpoint.Headers), ExpectedStatusCodes: slices.Clone(endpoint.ExpectedStatusCodes),
		}
	}
	return u
}

// v0.2.0 returns a handler and panics on constructor rejection. Convert that
// boundary to an operational error; do not intercept runtime degraded storage.
func newUptime(cfg uptime.Config) (handler fiber.Handler, err error) {
	defer func() {
		if value := recover(); value != nil {
			cause, ok := value.(error)
			if !ok {
				cause = errors.New("uptime constructor panicked")
			}
			err = safeError("cannot initialize uptime monitoring", cause)
		}
	}()
	return uptime.New(cfg), nil
}
