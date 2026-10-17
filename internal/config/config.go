// Package config loads the standalone product's configuration without starting
// a runtime, opening storage, or making network requests.
// YAML structure is checked in every branch; environment expansion and semantic
// validation apply only to active branches. Enabled TLS keypairs are loaded here.
package config

import (
	"crypto/tls"
	"net/url"
	"time"
	_ "time/tzdata" // Keep IANA locations available without system zoneinfo.
)

// StorageType selects one persistence backend without migrating existing history.
type StorageType string

const (
	// StorageBbolt selects an embedded database with exclusive file access.
	StorageBbolt StorageType = "bbolt"
	// StorageRedis selects Fiber Uptime's native Redis persistence.
	StorageRedis StorageType = "redis"
)

// CalendarDays is a positive calendar-day span, not a multiple of 24 hours.
type CalendarDays int

// Config contains only validated, active configuration. It must not be logged
// or dumped: URLs, headers, and authentication fields may contain secrets.
type Config struct {
	Server    ServerConfig
	Storage   StorageConfig
	Uptime    UptimeConfig
	UI        UIConfig
	Auth      *BasicAuthConfig // nil disables authentication.
	Endpoints []EndpointConfig // Current YAML membership, not the persisted service registry.
}

// ServerConfig controls the listener and graceful shutdown. Address is host:port;
// an empty host binds all interfaces, while the default uses IPv4 loopback.
type ServerConfig struct {
	Address         string
	ShutdownTimeout time.Duration
	TLS             *TLSConfig // nil means disabled.
}

// TLSConfig contains a keypair loaded during configuration validation. The runtime
// uses Certificate without rereading CertFile or KeyFile; relative paths use CWD.
type TLSConfig struct {
	CertFile    string
	KeyFile     string
	Certificate tls.Certificate
}

// StorageConfig contains exactly one non-nil backend configuration, selected by Type.
type StorageConfig struct {
	Type  StorageType
	Bbolt *BboltConfig
	Redis *RedisConfig
}

// BboltConfig selects a database path relative to the process working directory
// when not absolute. Runtime use may create it; maintenance requires it to exist.
type BboltConfig struct{ Path string }

// RedisConfig selects the database and namespace. URL may contain credentials and
// must not be logged. Shared persistence does not coordinate monitoring workers.
type RedisConfig struct {
	URL       *url.URL
	KeyPrefix string
}

// UptimeConfig separates probe timing, history retention and dashboard presentation.
type UptimeConfig struct {
	Interval  time.Duration  // Default probe interval, also used when stored metadata lacks one.
	Retention CalendarDays   // Stored history horizon; Redis additionally limits its expiry duration.
	Window    CalendarDays   // Dashboard range, no greater than Retention; does not bound exports.
	Timezone  *time.Location // Calendar-day boundaries, including daylight saving transitions.
}

// UIConfig controls the upstream dashboard and its API namespace.
type UIConfig struct {
	Path        string // Validated non-root path; also the root redirect and auth boundary.
	Title       string
	Description string
	Footer      string
	FaviconURL  *url.URL // nil selects the built-in favicon.
	Thresholds  ThresholdConfig
}

// ThresholdConfig holds uptime ratios, not percentages: 0 < Yellow <= Green <= 1.
type ThresholdConfig struct{ Green, Yellow float64 }

// BasicAuthConfig holds an NFC-normalized username and a validated bcrypt hash.
// PasswordHash is sensitive and must not be logged; password verification belongs
// to Fiber's middleware, not configuration loading.
type BasicAuthConfig struct{ Username, PasswordHash string }

// EndpointConfig is a normalized HTTP probe. ID is the persistent service identity:
// renaming it starts separate history, and omitting it from YAML never deletes data.
type EndpointConfig struct {
	ID                  string
	Name                string
	Description         string
	URL                 *url.URL
	Method              string
	Interval            time.Duration
	Timeout             time.Duration
	Headers             map[string]string // Canonical names; values may contain secrets.
	ExpectedStatusCodes []int             // Empty selects the upstream 2xx/3xx policy.
}
