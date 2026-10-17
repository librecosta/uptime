package config

import (
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"net/textproto"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/text/unicode/norm"
)

var (
	endpointID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	daySpan    = regexp.MustCompile(`^[1-9][0-9]*d$`)
	bcryptHash = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)
)

// A normalizer keeps the first field error. Nothing partially normalized is
// returned, and no error includes a configured value or a parser's raw error.
type normalizer struct {
	lookup envLookup
	err    error
}

func (n *normalizer) fail(field, message string) {
	if n.err == nil {
		n.err = fmt.Errorf("%s: %s", field, message)
	}
}

func (n *normalizer) text(field string, raw *string, required bool) string {
	if n.err != nil {
		return ""
	}
	var value string
	if raw != nil {
		var err error
		value, err = interpolate(*raw, n.lookup)
		if err != nil {
			n.fail(field, err.Error())
			return ""
		}
	}
	if required && value == "" {
		n.fail(field, "value is required")
	}
	return value
}

func (n *normalizer) duration(field string, raw *string, minimum time.Duration) time.Duration {
	value := n.text(field, raw, true)
	d, err := time.ParseDuration(value)
	if err != nil {
		n.fail(field, "invalid duration")
	} else if d < minimum {
		n.fail(field, "duration is below the allowed minimum")
	}
	return d
}

func (n *normalizer) days(field string, raw *string) CalendarDays {
	value := n.text(field, raw, true)
	if !daySpan.MatchString(value) {
		n.fail(field, "expected positive calendar days in Nd format")
		return 0
	}
	days, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || days <= 0 {
		n.fail(field, "calendar days must be positive and fit in an int")
		return 0
	}
	return CalendarDays(days)
}

func normalize(raw rawConfig, lookup envLookup, maintenance bool) (Config, error) {
	n := normalizer{lookup: lookup}
	var cfg Config
	cfg.Server.Address = n.text("server.address", raw.Server.Address, true)
	if !validAddress(cfg.Server.Address) {
		n.fail("server.address", "expected host:port with port 1..65535")
	}
	cfg.Server.ShutdownTimeout = n.duration("server.shutdown_timeout", raw.Server.ShutdownTimeout, time.Nanosecond)
	if raw.Server.TLS.Enabled {
		t := &TLSConfig{
			CertFile: n.text("server.tls.cert_file", raw.Server.TLS.CertFile, true),
			KeyFile:  n.text("server.tls.key_file", raw.Server.TLS.KeyFile, true),
		}
		if n.err == nil {
			var err error
			t.Certificate, err = tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
			if err != nil {
				n.fail("server.tls", "cannot load matching certificate and private key")
			}
		}
		cfg.Server.TLS = t
	}

	// Select on literal YAML only. Inactive branches are never passed to text
	// or validators, and have no representation in the returned Config.
	cfg.Storage.Type = StorageType(*raw.Storage.Type)
	switch cfg.Storage.Type {
	case StorageBbolt:
		cfg.Storage.Bbolt = &BboltConfig{Path: n.text("storage.bbolt.path", raw.Storage.Bbolt.Path, true)}
	case StorageRedis:
		value := n.text("storage.redis.url", raw.Storage.Redis.URL, true)
		u, err := url.Parse(value)
		if err != nil || u.Hostname() == "" || u.Opaque != "" ||
			(u.Scheme != "redis" && u.Scheme != "rediss") || strings.Contains(value, "#") {
			n.fail("storage.redis.url", "invalid Redis URL")
		} else if _, err := redis.ParseURL(value); err != nil {
			// Parser errors can contain credentials, paths or query values.
			n.fail("storage.redis.url", "invalid Redis URL")
		}
		prefix := n.text("storage.redis.key_prefix", raw.Storage.Redis.KeyPrefix, true)
		if strings.HasPrefix(prefix, ":") || strings.HasSuffix(prefix, ":") ||
			strings.TrimSpace(prefix) != prefix || strings.IndexFunc(prefix, unicode.IsControl) >= 0 {
			n.fail("storage.redis.key_prefix", "must not have surrounding colons or whitespace, or control characters")
		}
		cfg.Storage.Redis = &RedisConfig{URL: u, KeyPrefix: prefix}
	default:
		n.fail("storage.type", "expected literal bbolt or redis")
	}

	cfg.Uptime.Interval = n.duration("uptime.interval", raw.Uptime.Interval, time.Second)
	cfg.Uptime.Retention = n.days("uptime.retention", raw.Uptime.Retention)
	// Match Uptime's Redis expiry backstop without opening storage. Embedded
	// storage uses calendar-day retention and has no Redis duration limit.
	if cfg.Storage.Type == StorageRedis && cfg.Uptime.Retention > CalendarDays(math.MaxInt64/(24*time.Hour)-1) {
		n.fail("uptime.retention", "must not exceed 106750d for Redis storage")
	}
	cfg.Uptime.Window = n.days("uptime.window", raw.Uptime.Window)
	if cfg.Uptime.Window > cfg.Uptime.Retention {
		n.fail("uptime.window", "must not exceed retention")
	}
	zone := n.text("uptime.timezone", raw.Uptime.Timezone, true)
	if n.err == nil {
		var err error
		cfg.Uptime.Timezone, err = time.LoadLocation(zone)
		if err != nil {
			n.fail("uptime.timezone", "unknown timezone")
		}
	}

	cfg.UI.Path = n.text("ui.path", raw.UI.Path, true)
	if !strings.HasPrefix(cfg.UI.Path, "/") || cfg.UI.Path == "/" ||
		path.Clean(cfg.UI.Path) != cfg.UI.Path || strings.ContainsAny(cfg.UI.Path, "?#\\\r\n") ||
		cfg.UI.Path == "/livez" || cfg.UI.Path == "/readyz" {
		n.fail("ui.path", "expected a clean absolute path without trailing slash or health-route collision")
	}
	cfg.UI.Title = n.text("ui.title", raw.UI.Title, true)
	cfg.UI.Description = n.text("ui.description", raw.UI.Description, true)
	cfg.UI.Footer = n.text("ui.footer", raw.UI.Footer, true)
	if favicon := n.text("ui.favicon_url", raw.UI.FaviconURL, false); favicon != "" {
		u, err := url.Parse(favicon)
		if err != nil || !(strings.HasPrefix(favicon, "/") && !strings.HasPrefix(favicon, "//") || validHTTPURL(favicon, u)) {
			n.fail("ui.favicon_url", "expected a root-relative or absolute HTTP(S) URL")
		} else {
			cfg.UI.FaviconURL = u
		}
	}
	cfg.UI.Thresholds = ThresholdConfig{Green: *raw.UI.Thresholds.Green, Yellow: *raw.UI.Thresholds.Yellow}
	g, y := cfg.UI.Thresholds.Green, cfg.UI.Thresholds.Yellow
	if math.IsNaN(g) || math.IsNaN(y) || math.IsInf(g, 0) || math.IsInf(y, 0) || y <= 0 || y > g || g > 1 {
		n.fail("ui.thresholds", "expected finite values with 0 < yellow <= green <= 1")
	}

	if raw.Auth.Enabled {
		cfg.Auth = &BasicAuthConfig{
			Username:     norm.NFC.String(n.text("auth.basic.username", raw.Auth.Basic.Username, true)),
			PasswordHash: n.text("auth.basic.password_hash", raw.Auth.Basic.PasswordHash, true),
		}
		if strings.Contains(cfg.Auth.Username, ":") || strings.IndexFunc(cfg.Auth.Username, unicode.IsControl) >= 0 {
			n.fail("auth.basic.username", "invalid Basic Auth username")
		}
		if !bcryptHash.MatchString(cfg.Auth.PasswordHash) {
			n.fail("auth.basic.password_hash", "invalid bcrypt password hash")
		} else if _, err := bcrypt.Cost([]byte(cfg.Auth.PasswordHash)); err != nil {
			n.fail("auth.basic.password_hash", "invalid bcrypt password hash")
		}
	}
	if raw.Endpoints == nil || (!maintenance && len(raw.Endpoints) == 0) {
		n.fail("endpoints", "at least one endpoint is required")
	}
	seen := make(map[string]bool)
	for i, r := range raw.Endpoints {
		field := fmt.Sprintf("endpoints[%d]", i)
		if !endpointID.MatchString(r.ID) {
			n.fail(field+".id", "expected a literal ID matching [A-Za-z0-9][A-Za-z0-9._-]{0,63}")
		} else if seen[r.ID] {
			n.fail(field+".id", "duplicate endpoint ID")
		}
		seen[r.ID] = true
		e := EndpointConfig{
			ID: r.ID, Name: n.text(field+".name", r.Name, true),
			Description: n.text(field+".description", r.Description, false),
			Method:      *r.Method, Interval: cfg.Uptime.Interval,
			ExpectedStatusCodes: r.ExpectedStatusCodes,
		}
		value := n.text(field+".url", r.URL, true)
		u, err := url.Parse(value)
		if err != nil || !validHTTPURL(value, u) {
			n.fail(field+".url", "invalid HTTP URL; host required, userinfo and fragment forbidden")
		}
		e.URL = u
		if e.Method != "GET" && e.Method != "HEAD" {
			n.fail(field+".method", "expected literal uppercase GET or HEAD")
		}
		if r.Interval != nil {
			e.Interval = n.duration(field+".interval", r.Interval, time.Second)
		}
		e.Timeout = min(5*time.Second, e.Interval)
		if r.Timeout != nil {
			e.Timeout = n.duration(field+".timeout", r.Timeout, time.Millisecond)
		}
		if e.Timeout > e.Interval {
			n.fail(field+".timeout", "must not exceed endpoint interval")
		}
		codes := make(map[int]bool)
		for _, code := range e.ExpectedStatusCodes {
			if code < 100 || code > 599 || codes[code] {
				n.fail(field+".expected_status_codes", "expected unique status codes in 100..599")
			}
			codes[code] = true
		}
		e.Headers = n.headers(field+".headers", r.Headers)
		cfg.Endpoints = append(cfg.Endpoints, e)
	}
	if n.err != nil {
		return Config{}, n.err
	}
	return cfg, nil
}

func validAddress(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || strings.ContainsAny(host, " /?#@\\\t\r\n") {
		return false
	}
	p, err := strconv.ParseUint(port, 10, 16)
	return err == nil && p > 0
}

func validHTTPURL(value string, u *url.URL) bool {
	return u != nil && (strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")) &&
		u.Hostname() != "" && u.User == nil && u.Opaque == "" && !strings.Contains(value, "#")
}

func (n *normalizer) headers(field string, raw map[string]string) map[string]string {
	headers := make(map[string]string, len(raw))
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		p := fmt.Sprintf("%s[%d]", field, i)
		if !validHeaderName(key) || strings.Contains(key, "${") {
			n.fail(p, "invalid literal header name")
			continue
		}
		canonical := textproto.CanonicalMIMEHeaderKey(key)
		if canonical == "Host" {
			n.fail(p, "Host header is not allowed")
		}
		if _, exists := headers[canonical]; exists {
			n.fail(p, "duplicate header name (case-insensitive)")
		}
		rawValue := raw[key]
		value := n.text(p, &rawValue, false)
		if strings.ContainsAny(value, "\r\n") {
			n.fail(p, "header value must not contain CR or LF")
		}
		headers[canonical] = value
	}
	return headers
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range []byte(value) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return true
}
