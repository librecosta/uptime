package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

const validConfig = "endpoints: [{id: web, url: 'https://example.invalid/health'}]\n"

func writeConfig(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, "uptime.yaml", validConfig)
	writeConfig(t, "custom.yaml", validConfig)
	writeConfig(t, "alerts.yaml", validConfig+"alerts: {enabled: true, consecutive_failures: 3}\n")
	for _, args := range [][]string{
		{"config", "check"}, {"config", "check", "--config", "custom.yaml"},
		{"config", "check", "--config=" + filepath.Join(dir, "custom.yaml")},
		{"config", "check", "--config", "alerts.yaml"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 0 || stdout.String() != "configuration is valid\n" || stderr.Len() != 0 {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		t.Fatal("config check created unexpected files")
	}
}

func TestCommandsRejectSubMillisecondTimeout(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, "uptime.yaml", "endpoints: [{id: web, url: 'https://example.invalid/health', timeout: 500us}]\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Even a canceled serve must reject invalid config before Run.
	for _, args := range [][]string{{"config", "check"}, {"serve"}} {
		var stdout, stderr bytes.Buffer
		code := run(ctx, args, &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "endpoints[0].timeout") {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", args, code, &stdout, &stderr)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("invalid timeout created runtime files")
	}
}

func TestRedisConfigCheckOffline(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, "uptime.yaml", validConfig+"storage: {type: redis, redis: {url: 'rediss://user:SECRET@never-resolves.invalid:6380/2?read_timeout=2s', key_prefix: 'prod:asia:uptime'}}\n")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"config", "check"}, &stdout, &stderr); code != 0 || stdout.String() != "configuration is valid\n" || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stdout.Reset()
	if code := run(ctx, []string{"serve"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("canceled Redis serve: code=%d stderr=%q", code, &stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("offline Redis validation created storage files")
	}
}

func TestConfigHelp(t *testing.T) {
	for _, option := range []string{"--help", "-h"} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"config", "check", "--config", "missing.yaml", option}, &stdout, &stderr); code != 0 || stderr.Len() != 0 {
			t.Fatalf("help code=%d stderr=%q", code, &stderr)
		}
		for _, text := range []string{"uptime config check [--config PATH]", "./uptime.yaml", "-h, --help"} {
			if !strings.Contains(stdout.String(), text) {
				t.Fatalf("missing help text %q", text)
			}
		}
	}
}

func TestConfigUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"config"}, {"config", "unknown"}, {"config", "--help"},
		{"config", "check", "--unknown"}, {"config", "check", "--config"},
		{"config", "check", "extra"}, {"config", "check", "--help", "extra"},
		{"config", "check", "--help=bad"}, {"config", "check", "--unknown=secret"},
		{"--config", "uptime.yaml"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr)
			if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--help") || strings.Contains(stderr.String(), "secret") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
}

func TestConfigFailures(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// Isolate this one process-env case; config package tests inject lookup.
	const missing = "UPTIME_CONFIG_TEST_MISSING_381A"
	t.Setenv(missing, "")
	if err := os.Unsetenv(missing); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, data, want string }{
		{"malformed", "server: [", "YAML"},
		{"unknown", validConfig + "unknown: value", "unknown field"},
		{"semantic", validConfig + "uptime: {interval: 0s}", "uptime.interval"},
		{"env", validConfig + "ui: {title: '${" + missing + "}'}", "is not set"},
		{"tls", validConfig + "server: {tls: {enabled: true, cert_file: missing.crt, key_file: missing.key}}", "server.tls"},
		{"secret URL", "endpoints: [{id: web, url: 'https://user:SECRET@example.invalid?token=SECRET'}]", ".url"},
		{"secret YAML", validConfig + "ui: {thresholds: {green: SECRET}}", "type"},
		{"secret header", "endpoints: [{id: web, url: 'https://example.invalid', headers: {Authorization: \"Bearer SECRET\\n\"}}]", "headers"},
		{"secret hash", validConfig + "auth: {enabled: true, basic: {password_hash: SECRET}}", "username"},
		{"secret redis", validConfig + "storage: {type: redis, redis: {url: 'redis://user:SECRET@example.invalid:bad'}}", "storage.redis.url"},
		{"redis parser", validConfig + "storage: {type: redis, redis: {url: 'redis://user:SECRET@example.invalid/SECRET'}}", "storage.redis.url"},
		{"redis fragment", validConfig + "storage: {type: redis, redis: {url: 'redis://user:SECRET@example.invalid/0#SECRET'}}", "storage.redis.url"},
		{"redis prefix", validConfig + "storage: {type: redis, redis: {url: 'redis://example.invalid', key_prefix: ':SECRET:'}}", "storage.redis.key_prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, "invalid.yaml", tc.data)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"config", "check", "--config", "invalid.yaml"}, &stdout, &stderr)
			if code != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "config check: ") || !strings.Contains(stderr.String(), tc.want) || strings.Contains(stderr.String(), "SECRET") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
	for _, args := range [][]string{{"config", "check"}, {"config", "check", "--config", "missing.yaml"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 1 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "config check: ") {
			t.Fatalf("missing config: code=%d stderr=%q", code, &stderr)
		}
	}
}

func TestConfigOutputFailures(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	writeConfig(t, file, validConfig)
	for _, args := range [][]string{{"config", "check", "--config", file}, {"config", "check", "--help"}} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, failingWriter{}, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "config check: ") {
			t.Fatalf("write error: code=%d stderr=%q", code, &stderr)
		}
	}
	for _, args := range [][]string{{"config"}, {"config", "check", "--config", "missing.yaml"}, {"config", "check", "--unknown"}} {
		var stdout bytes.Buffer
		if code := run(context.Background(), args, &stdout, failingWriter{}); code != 1 {
			t.Fatalf("stderr failure: code=%d", code)
		}
	}
}

func TestConfigCheckDoesNotMigrateAlertDB(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	db, err := bolt.Open("alerts.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"meta", "incidents", "active", "outbox"} {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		meta := tx.Bucket([]byte("meta"))
		if err := meta.Put([]byte("format"), []byte("librecosta-uptime-alerts")); err != nil {
			return err
		}
		return meta.Put([]byte("schema_version"), binary.BigEndian.AppendUint64(nil, 1))
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile("alerts.db")
	if err != nil {
		t.Fatal(err)
	}
	writeConfig(t, "uptime.yaml", validConfig+"storage: {type: bbolt, bbolt: {path: ./uptime.db}}\nalerts: {enabled: true, reminder_interval: 1m, reminder_limit: 2}\n")
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"config", "check"}, &stdout, &stderr); code != 0 {
		t.Fatalf("offline check: %s", &stderr)
	}
	after, err := os.ReadFile("alerts.db")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("config check changed existing v1 alert DB")
	}
	if _, err := os.Stat("uptime.db"); !os.IsNotExist(err) {
		t.Fatal("config check opened monitoring database")
	}
}
