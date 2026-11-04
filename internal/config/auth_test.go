package config

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func authHash(t *testing.T) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(hash)
}

func TestBcryptContract(t *testing.T) {
	hash := authHash(t)
	for _, family := range []string{"$2a$", "$2b$", "$2y$"} {
		for _, cost := range []string{"04", "12", "31"} {
			value := family + cost + hash[6:]
			cfg := mustLoad(t, configData(t, map[string]any{"auth.enabled": true, "auth.basic.username": "admin", "auth.basic.password_hash": value}), emptyEnv)
			if cfg.Auth.PasswordHash != value {
				t.Fatal("hash modified")
			}
		}
	}
	for _, value := range []string{
		"plaintext-SECRET", "{SHA256}" + strings.Repeat("A", 44), "{SHA512}" + strings.Repeat("A", 88),
		strings.Repeat("a", 64), "$2x$" + hash[4:], "$2$" + hash[4:],
		hash[:len(hash)-1], hash + "a", hash[:len(hash)-1] + "!", " " + hash, hash + "\n",
		"$2a$03$" + hash[7:], "$2a$32$" + hash[7:], "$2a$xx$" + hash[7:],
		"${NESTED}",
	} {
		for _, input := range []string{value, "${HASH}"} {
			_, err := load(configData(t, map[string]any{"auth.enabled": true, "auth.basic.username": "private-user", "auth.basic.password_hash": input}), env(map[string]string{"HASH": value, "NESTED": "invalid-SECRET"}))
			if err == nil || !strings.Contains(err.Error(), "auth.basic.password_hash") || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "private-user") {
				t.Fatalf("missing or unsafe bcrypt error: %v", err)
			}
		}
	}
}

func TestBasicAuthUsername(t *testing.T) {
	hash := authHash(t)
	for _, tc := range []struct{ input, want string }{
		{"admin", "admin"}, {"operator@example", "operator@example"}, {"运维", "运维"}, {"ope\u0301rateur", "opérateur"},
	} {
		for _, input := range []string{tc.input, "${USER}"} {
			cfg := mustLoad(t, configData(t, map[string]any{"auth.enabled": true, "auth.basic.username": input, "auth.basic.password_hash": hash}), env(map[string]string{"USER": tc.input}))
			if cfg.Auth.Username != tc.want {
				t.Fatal("username not NFC normalized")
			}
		}
	}
	for _, value := range []string{"", "private:SECRET", "private\nSECRET", "private\rSECRET", "private\tSECRET", "private\x00SECRET", "private\x7fSECRET", "private\u0085SECRET"} {
		for _, input := range []string{value, "${USER}"} {
			_, err := load(configData(t, map[string]any{"auth.enabled": true, "auth.basic.username": input, "auth.basic.password_hash": hash}), env(map[string]string{"USER": value}))
			if err == nil || !strings.Contains(err.Error(), "auth.basic.username") || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), hash) {
				t.Fatalf("missing or unsafe username error: %v", err)
			}
		}
	}
	for _, values := range [][2]string{{"bad:username", "plaintext"}, {"${MISSING}", "${INVALID:-expression}"}} {
		cfg := mustLoad(t, configData(t, map[string]any{"auth.enabled": false, "auth.basic.username": values[0], "auth.basic.password_hash": values[1]}), func(string) (string, bool) { t.Fatal("inactive auth resolved env"); return "", false })
		if cfg.Auth != nil {
			t.Fatal("inactive auth retained")
		}
	}
}

func TestMonitorAccessConfig(t *testing.T) {
	old := mustLoad(t, []byte(minimal), emptyEnv)
	if old.Monitor.Enabled || old.Auth != nil {
		t.Fatal("old YAML enabled Monitor/auth")
	}
	hash := authHash(t)
	base := map[string]any{"auth.enabled": true, "auth.basic.username": "admin", "auth.basic.password_hash": hash}
	cfg := mustLoad(t, configData(t, base), emptyEnv)
	if !cfg.Auth.ProtectUptime {
		t.Fatal("old authenticated YAML became public")
	}
	base["monitor.enabled"] = true
	cfg = mustLoad(t, configData(t, base), emptyEnv)
	if !cfg.Monitor.Enabled || !cfg.Auth.ProtectUptime {
		t.Fatal("Monitor mapping changed defaults")
	}
	base["auth.protect_uptime"] = false
	cfg = mustLoad(t, configData(t, base), emptyEnv)
	if cfg.Auth.ProtectUptime || !cfg.Monitor.Enabled {
		t.Fatal("independent access selection lost")
	}
	for _, overrides := range []map[string]any{
		{"monitor.enabled": true},
		{"monitor.enabled": true, "auth.enabled": true, "auth.basic.username": "private", "auth.basic.password_hash": "SECRET"},
		{"monitor.enabled": "true"}, {"monitor.enabled": nil}, {"auth.protect_uptime": "false"}, {"auth.protect_uptime": nil},
	} {
		_, err := load(configData(t, overrides), emptyEnv)
		if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid or unsafe Monitor config: %v", err)
		}
	}
	// Disabled auth stays inactive even when Monitor configuration is rejected.
	_, err := load(configData(t, map[string]any{"monitor.enabled": true, "auth.basic.password_hash": "${INACTIVE}"}), func(string) (string, bool) { t.Fatal("inactive credentials expanded"); return "", false })
	if err == nil || !strings.Contains(err.Error(), "monitor.enabled") {
		t.Fatal("Monitor accepted disabled authentication")
	}
	for _, path := range []string{"/monitor", "/monitor/child", "/MONITOR", "/%6donitor", "/monitor%2fchild", "/alerts", "/alerts/child", "/%61lerts"} {
		base["ui.path"] = path
		wantInvalid(t, configData(t, base), emptyEnv, "ui.path")
	}
	for _, path := range []string{"/monitoring", "/monitor-page", "/alerts2", "/status"} {
		base["ui.path"] = path
		mustLoad(t, configData(t, base), emptyEnv)
	}
	// /monitor is available to Uptime when Monitor is disabled; /alerts is reserved.
	base["monitor.enabled"], base["ui.path"] = false, "/monitor"
	mustLoad(t, configData(t, base), emptyEnv)
	base["ui.path"] = "/alerts"
	wantInvalid(t, configData(t, base), emptyEnv, "ui.path")
}
