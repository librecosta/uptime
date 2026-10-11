package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/librecosta/uptime/storage/bbolt"
)

func TestServeHelpAndUsage(t *testing.T) {
	for _, option := range []string{"--help", "-h"} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{"serve", "--config", "missing.yaml", option}, &stdout, &stderr)
		if code != 0 || stderr.Len() != 0 || !strings.Contains(stdout.String(), "uptime serve [--config PATH]") || !strings.Contains(stdout.String(), "./uptime.yaml") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
		}
	}
	for _, args := range [][]string{
		{"--unknown=SECRET"}, {"--config"}, {"--help=SECRET"}, {"extra"}, {"--help", "extra"},
		{"--host", "localhost"}, {"--port", "8080"}, {"--storage", "redis"}, {"--tls"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), append([]string{"serve"}, args...), &stdout, &stderr)
		if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "uptime serve --help") || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
		}
	}
}

func TestServeDefaultAndCanceled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeConfig(t, "uptime.yaml", validConfig)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"serve"}, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("already-canceled serve created files")
	}
}

func serveKeypair(t *testing.T, dir string) (string, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1)}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	for path, block := range map[string]*pem.Block{certPath: {Type: "CERTIFICATE", Bytes: cert}, keyPath: {Type: "PRIVATE KEY", Bytes: key}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return certPath, keyPath
}

func TestServeFailures(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cert, key := serveKeypair(t, dir)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	for _, tc := range []struct{ name, data, want string }{
		{"missing", "", "cannot read"},
		{"invalid", validConfig + "uptime: {interval: 0s}", "uptime.interval"},
		{"Auth", validConfig + "auth: {enabled: true, basic: {username: operator, password_hash: SECRET}}", "auth.basic.password_hash"},
		{"TLS", validConfig + fmt.Sprintf("server: {tls: {enabled: true, cert_file: '%s', key_file: missing.key}}", cert), "server.tls"},
		{"TLS-bind", validConfig + fmt.Sprintf("server: {address: '%s', tls: {enabled: true, cert_file: '%s', key_file: '%s'}}\nstorage: {bbolt: {path: './tls-bind.db'}}", occupied.Addr(), cert, key), "cannot bind HTTP listener"},
		{"open", validConfig + "storage: {bbolt: {path: '.'}}", "cannot open bbolt storage"},
		{"bind", validConfig + fmt.Sprintf("server: {address: '%s'}\nstorage: {bbolt: {path: './bind.db'}}", occupied.Addr()), "cannot bind HTTP listener"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.name + ".yaml"
			if tc.data != "" {
				writeConfig(t, path, tc.data)
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"serve", "--config", path}, &stdout, &stderr)
			if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) || strings.Contains(stderr.String(), "SECRET") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
	store, err := bbolt.Open(bbolt.Config{Path: "bind.db", Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal("bind failure did not release DB:", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServeCancellation(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "uptime.db")
	file := filepath.Join(dir, "uptime.yaml")
	writeConfig(t, file, fmt.Sprintf("server: {address: '%s'}\nstorage: {bbolt: {path: '%s'}}\nendpoints: [{id: target, url: '%s'}]\n", address, dbPath, target.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve", "--config", file}, &stdout, &stderr) }()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := client.Get("http://" + address + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		select {
		case code := <-done:
			t.Fatalf("early exit=%d stderr=%s", code, &stderr)
		case <-deadline.C:
			t.Fatal("serve did not become ready")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("serve did not stop")
	}
	store, err := bbolt.Open(bbolt.Config{Path: dbPath, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServeOutputFailures(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"serve", "--help"}, failingWriter{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "cannot write output") {
		t.Fatalf("code=%d stderr=%q", code, &stderr)
	}
	for _, args := range [][]string{{"serve", "--unknown"}, {"serve", "--config", "missing.yaml"}} {
		var stdout bytes.Buffer
		if code := run(context.Background(), args, &stdout, failingWriter{}); code != 1 {
			t.Fatalf("stderr failure: code=%d", code)
		}
	}
}

func TestServeLockedDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.db")
	store, err := bbolt.Open(bbolt.Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	file := filepath.Join(dir, "uptime.yaml")
	writeConfig(t, file, validConfig+fmt.Sprintf("storage: {bbolt: {path: '%s'}}\n", path))
	var stdout, stderr bytes.Buffer
	// Exercise the production five-second lock timeout, without a CLI-only seam.
	code := run(context.Background(), []string{"serve", "--config", file}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "cannot open bbolt storage") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	if err := store.Ping(context.Background()); err != nil {
		t.Fatal("failed serve disturbed the lock owner:", err)
	}
}

func TestServeRedisStartupFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, "uptime.yaml", validConfig+fmt.Sprintf("storage: {type: redis, redis: {url: 'redis://user:SECRET@%s/0?dial_timeout=50ms&max_retries=-1'}}\n", address))
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"serve"}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "cannot connect Redis storage") || strings.Contains(stderr.String(), "SECRET") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("Redis failure fell back to local storage")
	}
}
