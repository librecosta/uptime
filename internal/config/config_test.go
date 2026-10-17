package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const minimal = "endpoints:\n  - id: web\n    url: https://example.invalid/health\n"

func emptyEnv(string) (string, bool) { return "", false }

func env(values map[string]string) envLookup {
	return func(name string) (string, bool) { value, ok := values[name]; return value, ok }
}

func configData(t *testing.T, changes map[string]any) []byte {
	t.Helper()
	root := map[string]any{"endpoints": []any{map[string]any{"id": "web", "url": "https://example.invalid/health"}}}
	for field, value := range changes {
		parts := strings.Split(field, ".")
		var node any = root
		for i, part := range parts {
			switch container := node.(type) {
			case map[string]any:
				if i == len(parts)-1 {
					container[part] = value
					break
				}
				if container[part] == nil {
					container[part] = map[string]any{}
				}
				node = container[part]
			case []any:
				index, err := strconv.Atoi(part)
				if err != nil {
					t.Fatal(err)
				}
				if i == len(parts)-1 {
					container[index] = value
				} else {
					node = container[index]
				}
			default:
				t.Fatalf("invalid test field %s", field)
			}
		}
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustLoad(t *testing.T, data []byte, lookup envLookup) Config {
	t.Helper()
	cfg, err := load(data, lookup)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func wantInvalid(t *testing.T, data []byte, lookup envLookup, field string) {
	t.Helper()
	cfg, err := load(data, lookup)
	if err == nil || !strings.Contains(err.Error(), field) {
		t.Fatalf("error = %v; want field/category %s", err, field)
	}
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Fatal("returned partial configuration on error")
	}
}

func TestDefaults(t *testing.T) {
	cfg := mustLoad(t, []byte(minimal), emptyEnv)
	if cfg.Server.Address != "127.0.0.1:18580" || cfg.Server.ShutdownTimeout != 10*time.Second || cfg.Server.TLS != nil || cfg.Auth != nil {
		t.Fatal("server/auth defaults")
	}
	if cfg.Storage.Type != StorageBbolt || cfg.Storage.Bbolt == nil || cfg.Storage.Bbolt.Path != "./data/uptime.db" || cfg.Storage.Redis != nil {
		t.Fatal("storage defaults")
	}
	if cfg.Uptime.Interval != 10*time.Second || cfg.Uptime.Retention != CalendarDays(90) || cfg.Uptime.Window != CalendarDays(30) || cfg.Uptime.Timezone != time.UTC {
		t.Fatal("uptime defaults")
	}
	if cfg.UI.Path != "/uptime" || cfg.UI.Title != "Service Status" || cfg.UI.Description != "Current service availability." || cfg.UI.Footer != "Powered by LibreCosta Uptime." || cfg.UI.FaviconURL != nil || cfg.UI.Thresholds != (ThresholdConfig{0.999, 0.99}) {
		t.Fatal("UI defaults")
	}
	e := cfg.Endpoints[0]
	if e.ID != "web" || e.Name != "web" || e.Description != "" || e.URL.Scheme != "https" || e.URL.Host != "example.invalid" || e.Method != "GET" || e.Interval != 10*time.Second || e.Timeout != 5*time.Second || len(e.Headers) != 0 || len(e.ExpectedStatusCodes) != 0 {
		t.Fatal("endpoint defaults")
	}
}

func TestStrictYAML(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"empty", "", "YAML"},
		{"comment only", "# hello", "YAML"},
		{"malformed", "server: [", "YAML"},
		{"root list", "[]", "type"},
		{"root null", "null", "type"},
		{"unknown", minimal + "unknown: value\n", "unknown field"},
		{"unknown nested", minimal + "uptime: {intervaal: 10s}\n", "unknown field"},
		{"unknown inactive", minimal + "auth: {enabled: false, basic: {password: secret}}\n", "unknown field"},
		{"duplicate root", minimal + "ui: {}\nui: {}\n", "duplicate"},
		{"duplicate nested", minimal + "uptime: {interval: 10s, interval: 20s}\n", "duplicate"},
		{"duplicate header", "endpoints: [{id: web, url: 'https://example.invalid', headers: {X-A: a, X-A: b}}]", "duplicate"},
		{"multiple documents", minimal + "---\n" + minimal, "one YAML document"},
		{"empty second document", minimal + "---\n", "one YAML document"},
		{"trailing malformed document", minimal + "---\n[", "one YAML document"},
		{"numeric string", minimal + "server: {address: 8080}\n", "type"},
		{"null field", minimal + "server: {address: null}\n", "type"},
		{"empty scalar", minimal + "server:\n  address:\n", "type"},
		{"null inactive", minimal + "auth: {enabled: false, basic: null}\n", "type"},
		{"wrong inactive shape", minimal + "storage: {redis: [a]}\n", "type"},
		{"nonstring header key", "endpoints: [{id: web, url: 'https://example.invalid', headers: {123: secret}}]", "string key"},
		{"merge key", minimal + "ui: {<<: {title: value}}\n", "string key"},
		{"bool env", minimal + "auth: {enabled: '${BOOL}'}\n", "type"},
		{"numeric env", minimal + "ui: {thresholds: {green: '${GREEN}'}}\n", "type"},
		{"list env", "endpoints: '${ENDPOINTS}'", "type"},
		{"status env", "endpoints: [{id: web, url: 'https://example.invalid', expected_status_codes: ['${CODE}']}]", "type"},
		{"quoted bool", minimal + "auth: {enabled: 'false'}\n", "type"},
		{"scalar overflow", "endpoints: [{id: web, url: 'https://example.invalid', expected_status_codes: [999999999999999999999999]}]", "YAML"},
	} {
		t.Run(tc.name, func(t *testing.T) { wantInvalid(t, []byte(tc.data), emptyEnv, tc.want) })
	}
	// Anchors/aliases with the correct shape remain ordinary strict YAML.
	mustLoad(t, []byte("ui: {title: &title Status, description: *title}\n"+minimal), emptyEnv)
}

func TestExplicitEmptyRequiredValues(t *testing.T) {
	for _, field := range []string{
		"server.address", "server.shutdown_timeout", "storage.type", "storage.bbolt.path",
		"uptime.interval", "uptime.retention", "uptime.window", "uptime.timezone", "ui.path", "ui.title",
		"ui.description", "ui.footer",
		"endpoints.0.id", "endpoints.0.name", "endpoints.0.url", "endpoints.0.method", "endpoints.0.interval", "endpoints.0.timeout",
	} {
		t.Run(field, func(t *testing.T) {
			wantInvalid(t, configData(t, map[string]any{field: ""}), emptyEnv, strings.ReplaceAll(field, ".0.", "[0]."))
		})
	}
	cfg := mustLoad(t, configData(t, map[string]any{"ui.favicon_url": "", "endpoints.0.description": ""}), emptyEnv)
	if cfg.UI.FaviconURL != nil || cfg.Endpoints[0].Description != "" {
		t.Fatal("clearable values replaced")
	}
}

func TestValidationMatrix(t *testing.T) {
	for i, tc := range []struct {
		field string
		value any
		want  string
	}{
		{"server.address", "localhost", "server.address"},
		{"server.address", ":http", "server.address"},
		{"server.address", ":0", "server.address"},
		{"server.address", ":65536", "server.address"},
		{"server.address", ":-1", "server.address"},
		{"server.address", "bad host:80", "server.address"},
		{"server.address", "::1:8080", "server.address"},
		{"server.shutdown_timeout", "0s", "shutdown_timeout"},
		{"server.shutdown_timeout", "-1s", "shutdown_timeout"},
		{"server.shutdown_timeout", "10days", "shutdown_timeout"},
		{"uptime.interval", "999ms", "uptime.interval"},
		{"uptime.interval", "0", "uptime.interval"},
		{"uptime.interval", "-1s", "uptime.interval"},
		{"uptime.interval", "invalid", "uptime.interval"},
		{"uptime.window", "91d", "uptime.window"},
		{"uptime.timezone", "Not/AZone", "uptime.timezone"},
		{"storage.type", "${TYPE}", "storage.type"},
		{"storage.type", "memory", "storage.type"},
		{"ui.path", "status", "ui.path"},
		{"ui.path", "/", "ui.path"},
		{"ui.path", "/status/", "ui.path"},
		{"ui.path", "//status", "ui.path"},
		{"ui.path", "/a/../status", "ui.path"},
		{"ui.path", "/a/./status", "ui.path"},
		{"ui.path", "/livez", "ui.path"},
		{"ui.path", "/readyz", "ui.path"},
		{"ui.thresholds.green", 0, "ui.thresholds"},
		{"ui.thresholds.green", 1.1, "ui.thresholds"},
		{"ui.thresholds.yellow", 0, "ui.thresholds"},
		{"ui.thresholds.yellow", 1, "ui.thresholds"},
		{"ui.favicon_url", "favicon.ico", "favicon_url"},
		{"ui.favicon_url", "//example.invalid/icon", "favicon_url"},
		{"ui.favicon_url", "data:image/png,secret", "favicon_url"},
		{"endpoints", []any{}, "endpoints"},
		{"endpoints.0.id", "${ID}", ".id"},
		{"endpoints.0.id", "-bad", ".id"},
		{"endpoints.0.id", "has space", ".id"},
		{"endpoints.0.id", strings.Repeat("a", 65), ".id"},
		{"endpoints.0.method", "get", ".method"},
		{"endpoints.0.method", "POST", ".method"},
		{"endpoints.0.method", "${METHOD}", ".method"},
		{"endpoints.0.url", "/health", ".url"},
		{"endpoints.0.url", "ftp://example.invalid", ".url"},
		{"endpoints.0.url", "HTTPS://example.invalid", ".url"},
		{"endpoints.0.url", "https:///health", ".url"},
		{"endpoints.0.url", "https://:80/health", ".url"},
		{"endpoints.0.url", "https://user:secret@example.invalid", ".url"},
		{"endpoints.0.url", "https://example.invalid/#frag", ".url"},
		{"endpoints.0.url", "https://example.invalid/#", ".url"},
		{"endpoints.0.url", "https://example.invalid/%zz", ".url"},
		{"endpoints.0.interval", "0s", ".interval"},
		{"endpoints.0.interval", "-1s", ".interval"},
		{"endpoints.0.interval", "500ms", ".interval"},
		{"endpoints.0.interval", "bad", ".interval"},
		{"endpoints.0.timeout", "0s", ".timeout"},
		{"endpoints.0.timeout", "-1s", ".timeout"},
		{"endpoints.0.timeout", "bad", ".timeout"},
		{"endpoints.0.timeout", "11s", ".timeout"},
		{"endpoints.0.expected_status_codes", []int{99}, "expected_status_codes"},
		{"endpoints.0.expected_status_codes", []int{600}, "expected_status_codes"},
		{"endpoints.0.expected_status_codes", []int{200, 200}, "expected_status_codes"},
	} {
		t.Run(tc.field+"/"+strconv.Itoa(i), func(t *testing.T) {
			wantInvalid(t, configData(t, map[string]any{tc.field: tc.value}), env(map[string]string{"TYPE": "bbolt", "ID": "web", "METHOD": "GET"}), tc.want)
		})
	}
	for _, threshold := range []string{".nan", ".inf", "-.inf"} {
		for _, field := range []string{"green", "yellow"} {
			wantInvalid(t, []byte(minimal+"ui: {thresholds: {"+field+": "+threshold+"}}\n"), emptyEnv, "ui.thresholds")
		}
	}
	wantInvalid(t, []byte("{}"), emptyEnv, "endpoints")
	wantInvalid(t, []byte("endpoints: [{id: web, url: 'https://example.invalid'}, {id: web, url: 'https://example.invalid'}]"), emptyEnv, "duplicate endpoint ID")
}

func TestCalendarDays(t *testing.T) {
	for _, value := range []string{"0d", "01d", "001d", "-1d", "24h", "1.5d", "90days", "1D", "+1d", " 1d", "99999999999999999999999999d"} {
		for _, field := range []string{"uptime.retention", "uptime.window"} {
			t.Run(field+"/"+value, func(t *testing.T) { wantInvalid(t, configData(t, map[string]any{field: value}), emptyEnv, field) })
		}
	}
	for _, value := range []int{1, 30, 90, 365} {
		cfg := mustLoad(t, configData(t, map[string]any{"uptime.retention": strconv.Itoa(value) + "d", "uptime.window": "1d"}), emptyEnv)
		if cfg.Uptime.Retention != CalendarDays(value) || cfg.Uptime.Window != 1 {
			t.Fatal("calendar days changed")
		}
	}
}

func TestValidOverrides(t *testing.T) {
	for _, address := range []string{":1", ":65535", ":8080", ":18580", "127.0.0.1:18580", "127.0.0.1:8080", "localhost:8080", "[::]:8080", "[::1]:8080"} {
		cfg := mustLoad(t, configData(t, map[string]any{"server.address": address}), emptyEnv)
		if cfg.Server.Address != address {
			t.Fatal("address changed")
		}
	}
	for _, zone := range []string{"UTC", "Asia/Shanghai", "America/New_York", "Local"} {
		cfg := mustLoad(t, configData(t, map[string]any{"uptime.timezone": zone}), emptyEnv)
		if cfg.Uptime.Timezone == nil || cfg.Uptime.Timezone.String() != zone {
			t.Fatal("timezone not resolved")
		}
	}
	for _, tc := range []struct {
		global, endpoint  string
		interval, timeout time.Duration
	}{
		{"2s", "", 2 * time.Second, 2 * time.Second},
		{"20s", "", 20 * time.Second, 5 * time.Second},
		{"20s", "3s", 3 * time.Second, 3 * time.Second},
		{"20s", "1s", time.Second, time.Second},
	} {
		changes := map[string]any{"uptime.interval": tc.global}
		if tc.endpoint != "" {
			changes["endpoints.0.interval"] = tc.endpoint
		}
		cfg := mustLoad(t, configData(t, changes), emptyEnv)
		if cfg.Endpoints[0].Interval != tc.interval || cfg.Endpoints[0].Timeout != tc.timeout {
			t.Fatal("derived endpoint defaults")
		}
	}
	for _, favicon := range []string{"/icon.svg", "https://example.invalid/icon.svg"} {
		cfg := mustLoad(t, configData(t, map[string]any{
			"ui.path": "/status", "ui.title": "Status", "ui.favicon_url": favicon,
			"ui.thresholds.green": 1, "ui.thresholds.yellow": 1,
			"endpoints.0.id": "A" + strings.Repeat("a", 60) + "._-", "endpoints.0.method": "HEAD",
			"endpoints.0.url": "http://example.invalid/health?token=secret", "endpoints.0.timeout": "10s",
			"endpoints.0.expected_status_codes": []int{100, 200, 399, 599},
		}), emptyEnv)
		if cfg.UI.FaviconURL.String() != favicon || cfg.Endpoints[0].URL.RawQuery != "token=secret" || cfg.Endpoints[0].Timeout != cfg.Endpoints[0].Interval {
			t.Fatal("typed custom values")
		}
	}
}

func TestEndpointTimeoutBounds(t *testing.T) {
	for _, tc := range []struct {
		name, timeout, interval string
		valid                   bool
	}{
		{"below minimum", "999999ns", "", false},
		{"minimum", "1ms", "", true},
		{"inherited interval", "2s", "", true},
		{"above inherited interval", "2.000000001s", "", false},
		{"longer endpoint interval", "3s", "3s", true},
		{"shorter endpoint interval", "1s", "1s", true},
		{"above endpoint interval", "1.000000001s", "1s", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, timeout := range []string{tc.timeout, "${TIMEOUT}"} {
				changes := map[string]any{"uptime.interval": "2s", "endpoints.0.timeout": timeout}
				if tc.interval != "" {
					changes["endpoints.0.interval"] = tc.interval
				}
				lookup := env(map[string]string{"TIMEOUT": tc.timeout})
				data := configData(t, changes)
				if !tc.valid {
					wantInvalid(t, data, lookup, "endpoints[0].timeout")
					continue
				}
				cfg := mustLoad(t, data, lookup)
				want, err := time.ParseDuration(tc.timeout)
				if err != nil || cfg.Endpoints[0].Timeout != want {
					t.Fatal("timeout changed during normalization")
				}
			}
		})
	}
}

func TestStorageAndNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "missing", "uptime.db")
	cfg := mustLoad(t, configData(t, map[string]any{"storage.bbolt.path": dataPath}), emptyEnv)
	if cfg.Storage.Bbolt.Path != dataPath || cfg.Storage.Redis != nil {
		t.Fatal("bbolt active branch")
	}
	if _, err := os.Stat(filepath.Dir(dataPath)); !os.IsNotExist(err) {
		t.Fatal("config created storage directory")
	}
	// An existing invalid database also remains untouched; validation never opens it.
	dataPath = filepath.Join(dir, "invalid.db")
	if err := os.WriteFile(dataPath, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	mustLoad(t, configData(t, map[string]any{"storage.bbolt.path": dataPath}), emptyEnv)
	data, err := os.ReadFile(dataPath)
	if err != nil || string(data) != "not a database" {
		t.Fatal("existing file changed")
	}
	for _, scheme := range []string{"redis", "rediss"} {
		cfg := mustLoad(t, configData(t, map[string]any{
			"storage.type": "redis", "storage.redis.url": scheme + "://user:secret@never-resolves.invalid:6379/0",
			"storage.bbolt.path": "${INVALID:-expression}",
		}), emptyEnv)
		if cfg.Storage.Bbolt != nil || cfg.Storage.Redis == nil || cfg.Storage.Redis.URL.Scheme != scheme || cfg.Storage.Redis.KeyPrefix != "fiber:uptime" {
			t.Fatal("redis active branch")
		}
	}
	for _, value := range []string{"", "http://example.invalid", "redis:///0", "redis://:6379", "redis://host:bad", "redis://%zz"} {
		wantInvalid(t, configData(t, map[string]any{"storage.type": "redis", "storage.redis.url": value}), emptyEnv, "storage.redis.url")
	}
	wantInvalid(t, configData(t, map[string]any{"storage.type": "redis", "storage.redis.url": "redis://example.invalid", "storage.redis.key_prefix": ""}), emptyEnv, "storage.redis.key_prefix")
}

func TestOfficialExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "configs", "uptime.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustLoad(t, data, func(string) (string, bool) { t.Fatal("example requested environment"); return "", false })
	if cfg.Storage.Bbolt == nil || cfg.Storage.Redis != nil || cfg.Server.TLS != nil || cfg.Auth != nil {
		t.Fatal("example active branches")
	}
}

func TestLoadFileAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, configData(t, map[string]any{"storage.bbolt.path": "relative/data.db"}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Bbolt.Path != "relative/data.db" {
		t.Fatal("path was resolved against YAML directory")
	}
	if _, err := LoadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("missing file succeeded")
	}
	if _, err := LoadFile(dir); err == nil {
		t.Fatal("directory read succeeded")
	}
}
