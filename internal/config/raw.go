package config

// Pointers preserve omission. Explicit YAML null is rejected by the structural
// decoder, so it cannot accidentally receive an omitted-field default.
type rawConfig struct {
	Server struct {
		Address         *string `yaml:"address"`
		ShutdownTimeout *string `yaml:"shutdown_timeout"`
		TLS             struct {
			Enabled  bool    `yaml:"enabled"`
			CertFile *string `yaml:"cert_file"`
			KeyFile  *string `yaml:"key_file"`
		} `yaml:"tls"`
	} `yaml:"server"`
	Storage struct {
		Type  *string `yaml:"type"`
		Bbolt struct {
			Path *string `yaml:"path"`
		} `yaml:"bbolt"`
		Redis struct {
			URL       *string `yaml:"url"`
			KeyPrefix *string `yaml:"key_prefix"`
		} `yaml:"redis"`
	} `yaml:"storage"`
	Uptime struct {
		Interval  *string `yaml:"interval"`
		Retention *string `yaml:"retention"`
		Window    *string `yaml:"window"`
		Timezone  *string `yaml:"timezone"`
	} `yaml:"uptime"`
	UI struct {
		Path        *string `yaml:"path"`
		Title       *string `yaml:"title"`
		Description *string `yaml:"description"`
		Footer      *string `yaml:"footer"`
		FaviconURL  *string `yaml:"favicon_url"`
		Thresholds  struct {
			Green  *float64 `yaml:"green"`
			Yellow *float64 `yaml:"yellow"`
		} `yaml:"thresholds"`
	} `yaml:"ui"`
	Auth struct {
		Enabled bool `yaml:"enabled"`
		Basic   struct {
			Username     *string `yaml:"username"`
			PasswordHash *string `yaml:"password_hash"`
		} `yaml:"basic"`
	} `yaml:"auth"`
	Endpoints []rawEndpoint `yaml:"endpoints"`
}

type rawEndpoint struct {
	ID                  string            `yaml:"id"`
	Name                *string           `yaml:"name"`
	Description         *string           `yaml:"description"`
	URL                 *string           `yaml:"url"`
	Method              *string           `yaml:"method"`
	Interval            *string           `yaml:"interval"`
	Timeout             *string           `yaml:"timeout"`
	Headers             map[string]string `yaml:"headers"`
	ExpectedStatusCodes []int             `yaml:"expected_status_codes"`
}

func defaultTo[T any](field **T, value T) {
	if *field == nil {
		*field = &value
	}
}

func (r *rawConfig) defaults() {
	defaultTo(&r.Server.Address, "127.0.0.1:18580")
	defaultTo(&r.Server.ShutdownTimeout, "10s")
	defaultTo(&r.Storage.Type, "bbolt")
	defaultTo(&r.Storage.Bbolt.Path, "./data/uptime.db")
	defaultTo(&r.Storage.Redis.KeyPrefix, "fiber:uptime")
	defaultTo(&r.Uptime.Interval, "10s")
	defaultTo(&r.Uptime.Retention, "90d")
	defaultTo(&r.Uptime.Window, "30d")
	defaultTo(&r.Uptime.Timezone, "UTC")
	defaultTo(&r.UI.Path, "/uptime")
	defaultTo(&r.UI.Title, "Service Status")
	defaultTo(&r.UI.Description, "Current service availability.")
	defaultTo(&r.UI.Footer, "Powered by LibreCosta Uptime.")
	defaultTo(&r.UI.Thresholds.Green, 0.999)
	defaultTo(&r.UI.Thresholds.Yellow, 0.99)
	for i := range r.Endpoints {
		defaultTo(&r.Endpoints[i].Name, r.Endpoints[i].ID)
		defaultTo(&r.Endpoints[i].Method, "GET")
	}
	// Omitted endpoint interval/timeout depend on the parsed effective interval;
	// normalize derives them once, before returning the final Config.
}
