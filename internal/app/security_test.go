package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/librecosta/uptime/internal/alerts"
	"github.com/librecosta/uptime/internal/config"
	"github.com/librecosta/uptime/storage/bbolt"
	"golang.org/x/crypto/bcrypt"
)

// The configured username and request credentials are decomposed Unicode.
// Fiber normalizes credentials; config normalizes the username. Hash NFC bytes.
const securityUser = "ope\u0301rateur"
const securityPassword = "cafe\u0301-test"

func securityKeypair(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	must(t, err)
	ca, err = x509.ParseCertificate(caDER)
	must(t, err)
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	server := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, server, ca, &serverKey.PublicKey, caKey)
	must(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	must(t, err)
	certPath, keyPath := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server.key")
	must(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	must(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600))
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return certPath, keyPath, pool
}

func securityConfig(t *testing.T, target, uiPath string, useTLS, useAuth bool) (config.Config, *http.Client) {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath, pool := securityKeypair(t, dir)
	hash, err := bcrypt.GenerateFromPassword([]byte("café-test"), bcrypt.MinCost)
	must(t, err)
	file := filepath.Join(dir, "uptime.yaml")
	data := fmt.Sprintf("server: {address: '127.0.0.1:8080', shutdown_timeout: 2s, tls: {enabled: %t, cert_file: %q, key_file: %q}}\nstorage: {bbolt: {path: %q}}\nauth: {enabled: %t, basic: {username: %q, password_hash: %q}}\nuptime: {interval: 1s}\nui: {path: %q}\nendpoints: [{id: target, url: %q, timeout: 500ms}]\n",
		useTLS, certPath, keyPath, filepath.Join(dir, "data", "uptime.db"), useAuth, securityUser, string(hash), uiPath, target)
	must(t, os.WriteFile(file, []byte(data), 0600))
	cfg, err := config.LoadFile(file)
	must(t, err)
	// Run must use the preloaded keypair even after both source files disappear.
	must(t, os.Remove(certPath))
	must(t, os.Remove(keyPath))
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	return cfg, client
}

func securityRequest(t *testing.T, r *runningServer, path, user, password string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.base+path, nil)
	must(t, err)
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	response, err := r.client.Do(req)
	must(t, err)
	return response
}

func assertChallenge(t *testing.T, response *http.Response) {
	t.Helper()
	if response.StatusCode != 401 || response.Header.Get("WWW-Authenticate") != `Basic realm="LibreCosta Uptime", charset="UTF-8"` || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Vary") != "Authorization" {
		t.Fatalf("unexpected challenge: status=%d headers=%v", response.StatusCode, response.Header)
	}
}

func TestSecurityCombinations(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		for _, useAuth := range []bool{false, true} {
			t.Run(fmt.Sprintf("tls=%v/auth=%v", useTLS, useAuth), func(t *testing.T) {
				var logs bytes.Buffer
				log.SetOutput(&logs)
				defer log.SetOutput(os.Stderr)
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
				defer target.Close()
				uiPath := "/status"
				if useTLS && useAuth {
					uiPath = "/uptime"
				} else if useTLS {
					uiPath = "/nested/status"
				}
				cfg, client := securityConfig(t, target.URL, uiPath, useTLS, useAuth)
				cfg.Endpoints[0].Headers = map[string]string{"Authorization": "Bearer PRIVATE-HEADER"}
				if useTLS && buildTLSConfig(cfg.Server.TLS).MinVersion != tls.VersionTLS12 {
					t.Fatal("TLS policy changed")
				}
				s, err := New(cfg)
				must(t, err)
				r := startServerWithClient(t, s, client)
				r.health(t, "/livez", 200, "ok\n")
				r.health(t, "/readyz", 200, "ready\n")
				if s.app.Config().AppName != "LibreCosta Uptime" {
					t.Fatal("incorrect Fiber product name")
				}
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					// Inspect the redirect itself, then follow it with no credentials.
					noRedirect := *client
					noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
					req, err := http.NewRequest(method, r.base+"/?token=PRIVATE&next=https://example.invalid", nil)
					must(t, err)
					response, err := noRedirect.Do(req)
					must(t, err)
					body, err := io.ReadAll(response.Body)
					response.Body.Close()
					must(t, err)
					if response.StatusCode != 302 || response.Header.Get("Location") != uiPath || response.Header.Get("WWW-Authenticate") != "" || (method == http.MethodHead && len(body) != 0) {
						t.Fatalf("root redirect: %s status=%d location=%q", method, response.StatusCode, response.Header.Get("Location"))
					}
					response, err = client.Do(req)
					must(t, err)
					response.Body.Close()
					if response.Request.URL.Path != uiPath || response.Request.URL.RawQuery != "" {
						t.Fatal("redirect propagated query or reached wrong path")
					}
					if useAuth {
						assertChallenge(t, response)
					} else if response.StatusCode != 200 {
						t.Fatal("redirect did not reach public dashboard")
					}
				}
				for _, path := range []string{uiPath + "2", uiPath + "-page", "/random"} {
					response := securityRequest(t, r, path, "", "")
					response.Body.Close()
					if response.StatusCode != 404 || response.Header.Get("WWW-Authenticate") != "" {
						t.Fatalf("unprotected path %s: %d", path, response.StatusCode)
					}
				}
				for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
					req, err := http.NewRequest(method, r.base+"/", nil)
					must(t, err)
					response, err := client.Do(req)
					must(t, err)
					response.Body.Close()
					if response.StatusCode != 404 || response.Header.Get("Location") != "" {
						t.Fatalf("unexpected root behavior for %s: %d", method, response.StatusCode)
					}
				}
				for _, path := range []string{uiPath, uiPath + "/api/status", uiPath + "/unknown", uiPath + "/"} {
					response := securityRequest(t, r, path, "", "")
					response.Body.Close()
					if useAuth {
						assertChallenge(t, response)
					} else if path == uiPath || path == uiPath+"/api/status" {
						if response.StatusCode != 200 {
							t.Fatal("public UI/API rejected")
						}
					}
				}
				if useAuth {
					for _, creds := range [][2]string{{securityUser, "wrong"}, {"wrong", securityPassword}} {
						response := securityRequest(t, r, uiPath, creds[0], creds[1])
						response.Body.Close()
						assertChallenge(t, response)
					}
				}
				for _, path := range []string{uiPath, uiPath + "/api/status", uiPath + "/unknown"} {
					response := securityRequest(t, r, path, securityUser, securityPassword)
					response.Body.Close()
					want := 200
					if strings.HasSuffix(path, "/unknown") {
						want = 404
					}
					if response.StatusCode != want {
						t.Fatalf("authenticated %s: %d", path, response.StatusCode)
					}
					if useTLS && (response.TLS == nil || response.TLS.Version < tls.VersionTLS12 || len(response.TLS.VerifiedChains) == 0) {
						t.Fatal("TLS verification/min version failed")
					}
				}
				poll(t, func() bool {
					response := securityRequest(t, r, uiPath+"/api/status", securityUser, securityPassword)
					defer response.Body.Close()
					var v uptime.StatusResponse
					return json.NewDecoder(response.Body).Decode(&v) == nil && v.Storage.Status == "ok" && len(v.Services) == 1 && v.Services[0].CurrentStatus == "up"
				})
				if useTLS {
					response, err := client.Get(strings.Replace(r.base, "https://", "http://", 1) + "/livez")
					if err == nil {
						response.Body.Close()
						t.Fatal("TLS listener accepted plaintext HTTP")
					}
					legacy := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: client.Transport.(*http.Transport).TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}}}
					defer legacy.CloseIdleConnections()
					if response, err := legacy.Get(r.base + "/livez"); err == nil {
						response.Body.Close()
						t.Fatal("TLS <1.2 accepted")
					}
				}
				r.cancel()
				must(t, r.wait(t))
				if !strings.Contains(logs.String(), startupMessage(s.listener.Addr().String(), uiPath, useTLS)) {
					t.Fatal("successful startup did not log bound address and dashboard entry")
				}
				for _, secret := range []string{target.URL, "PRIVATE", securityUser, securityPassword} {
					if strings.Contains(logs.String(), secret) {
						t.Fatal("runtime log exposed target URL or credentials")
					}
				}
				if cfg.Auth != nil && (strings.Contains(logs.String(), cfg.Auth.Username) || strings.Contains(logs.String(), cfg.Auth.PasswordHash)) {
					t.Fatal("runtime log exposed normalized credentials")
				}
				if s.ready.Load() {
					t.Fatal("ready after shutdown")
				}
				reopen(t, cfg.Storage.Bbolt.Path)
			})
		}
	}
}

func TestBasicAuthConstructorFailure(t *testing.T) {
	handler, err := newBasicAuth(config.Config{Auth: &config.BasicAuthConfig{Username: "private-user", PasswordHash: "PRIVATE-INVALID-HASH", ProtectUptime: true}, UI: config.UIConfig{Path: "/status"}})
	if handler != nil || err == nil || err.Error() != "cannot initialize Basic Auth" {
		t.Fatal("missing safe constructor error")
	}
	cfg := testConfig(t, "http://never-resolves.invalid")
	cfg.Auth = &config.BasicAuthConfig{Username: "private-user", PasswordHash: "PRIVATE-INVALID-HASH"}
	s, err := New(cfg)
	must(t, err)
	var writes atomic.Int32
	s.deps.openStorage = func(c config.StorageConfig) (*runtimeStorage, error) {
		store, err := bbolt.Open(bbolt.Config{Path: c.Bbolt.Path})
		if err != nil {
			return nil, err
		}
		return wrapBbolt(&testStore{Store: store, upsert: func(context.Context, uptimestorage.Service) error { writes.Add(1); return nil }}), nil
	}
	s.deps.listen = func(string, string) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	err = s.Run(context.Background())
	if err == nil || err.Error() != "cannot initialize Basic Auth" || writes.Load() != 0 {
		t.Fatal("auth failure started Uptime or exposed secrets")
	}
	reopen(t, cfg.Storage.Bbolt.Path)
}

func TestAuthHealthAlwaysPublic(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("private"), bcrypt.MinCost)
	must(t, err)
	auth, err := newBasicAuth(config.Config{Auth: &config.BasicAuthConfig{Username: "admin", PasswordHash: string(hash), ProtectUptime: true}, UI: config.UIConfig{Path: "/status"}})
	must(t, err)
	calls := 0
	s := &Server{app: fiber.New(), store: wrapBbolt(&testStore{ping: func(context.Context) error {
		calls++
		return errors.New("PRIVATE storage failure")
	}})}
	s.registerHealth()
	s.app.Use(auth)
	for _, ready := range []bool{false, true} {
		s.ready.Store(ready)
		for _, path := range []string{"/livez", "/readyz"} {
			response, err := s.app.Test(httptest.NewRequest(http.MethodGet, path, nil))
			must(t, err)
			status, body := 200, "ok\n"
			if path == "/readyz" {
				status, body = 503, "not ready\n"
			}
			assertHealth(t, response, status, body)
			response.Body.Close()
			if response.Header.Get("WWW-Authenticate") != "" {
				t.Fatal("health requested authentication")
			}
		}
	}
	if calls != 1 {
		t.Fatal("health Ping gating changed")
	}
}

func TestStalledTLSHandshakeShutdown(t *testing.T) {
	// A missed probe refreshes registration, allowing a worker to remain inside
	// storage until Uptime's shutdown hook cancels it.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer target.Close()
	cfg, client := securityConfig(t, target.URL, "/uptime", true, true)
	cfg.Server.ShutdownTimeout = 10 * time.Millisecond
	s, err := New(cfg)
	must(t, err)
	entered, exited := make(chan struct{}), make(chan struct{})
	var writes atomic.Int32
	var closed atomic.Bool
	s.deps.openStorage = func(c config.StorageConfig) (*runtimeStorage, error) {
		store, err := bbolt.Open(bbolt.Config{Path: c.Bbolt.Path})
		if err != nil {
			return nil, err
		}
		return wrapBbolt(&testStore{Store: store, upsert: func(ctx context.Context, v uptimestorage.Service) error {
			if writes.Add(1) == 2 {
				close(entered)
				<-ctx.Done()
				close(exited)
				return nil
			}
			return store.UpsertService(ctx, v)
		}, closeStore: func() error {
			select {
			case <-exited:
			default:
				t.Error("storage closed before worker stopped")
			}
			s.listener.mu.Lock()
			remaining, forced := len(s.listener.connections), s.listener.forced
			s.listener.mu.Unlock()
			if remaining != 0 || !forced || s.ready.Load() {
				t.Error("storage closed before TLS transport drain")
			}
			closed.Store(true)
			return store.Close()
		}}), nil
	}
	r := startServerWithClient(t, s, client)
	select {
	case <-entered:
	case <-time.After(testDeadline):
		t.Fatal("worker not entered")
	}
	// Ensure startup health connection has drained before adding the stalled one.
	poll(t, func() bool {
		s.listener.mu.Lock()
		defer s.listener.mu.Unlock()
		return len(s.listener.connections) == 0
	})
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(r.base, "https://"), time.Second)
	must(t, err)
	defer conn.Close()
	poll(t, func() bool {
		s.listener.mu.Lock()
		defer s.listener.mu.Unlock()
		return len(s.listener.connections) == 1
	})
	r.cancel()
	if err := r.wait(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown deadline lost: %v", err)
	}
	if !closed.Load() {
		t.Fatal("storage not closed")
	}
	must(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	var one [1]byte
	_, err = conn.Read(one[:])
	if err == nil {
		t.Fatal("stalled TLS transport still open")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("stalled TLS transport not force-closed")
	}
	if s.app.Server().GetCurrentConcurrency() != 0 {
		t.Fatal("TLS worker remains")
	}
	reopen(t, cfg.Storage.Bbolt.Path)
}

func TestTLSAuthBindFailure(t *testing.T) {
	cfg, client := securityConfig(t, "http://never-resolves.invalid", "/status", true, true)
	defer client.CloseIdleConnections()
	cfg.Auth.PasswordHash = "invalid-constructor-SECRET" // Bind must fail first.
	s, err := New(cfg)
	must(t, err)
	s.deps.listen = func(string, string) (net.Listener, error) { return nil, io.ErrUnexpectedEOF }
	err = s.Run(context.Background())
	if !errors.Is(err, io.ErrUnexpectedEOF) || s.app != nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("bind ordering changed")
	}
	reopen(t, cfg.Storage.Bbolt.Path)
}

// Exercise the production composition without starting probes or opening storage.
func TestMonitorHTTPMetrics(t *testing.T) {
	cfg, _ := securityConfig(t, "http://never-resolves.invalid/", "/status", false, true)
	cfg.Monitor.Enabled = true
	s := &Server{cfg: cfg, app: fiber.New()}
	must(t, s.registerMiddleware())
	s.app.Get("/ok", func(c fiber.Ctx) error { return c.SendStatus(204) })
	s.app.Get("/panic", func(fiber.Ctx) error { panic("test panic") })
	request := func(method, path, accept string, authenticated bool) (*http.Response, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Accept", accept)
		if authenticated {
			req.SetBasicAuth(securityUser, securityPassword)
		}
		response, err := s.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
		must(t, err)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		must(t, err)
		return response, body
	}
	// None of these Monitor requests, including authentication refusals, count.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response, _ := request(method, "/monitor", "application/json", false)
		assertChallenge(t, response)
		response, body := request(method, "/monitor/", "text/html", true)
		if response.StatusCode != 200 || method == http.MethodGet && (!bytes.Contains(body, []byte("Fiber Monitor")) || !regexp.MustCompile(`const REFRESH_MS =\s*3000\s*;`).Match(body)) {
			t.Fatalf("native Monitor dashboard/default refresh changed: status=%d title=%v", response.StatusCode, bytes.Contains(body, []byte("Fiber Monitor")))
		}
	}
	for _, test := range []struct {
		path   string
		status int
	}{{"/livez", 200}, {"/readyz", 503}, {"/", 302}, {"/missing", 404}, {"/panic", 500}, {"/status", 401}, {"/ok", 204}} {
		response, _ := request(http.MethodGet, test.path, "text/html", false)
		if response.StatusCode != test.status {
			t.Fatalf("%s: got %d want %d", test.path, response.StatusCode, test.status)
		}
	}
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			response, _ := request(http.MethodGet, "/ok", "text/html", false)
			if response.StatusCode != 204 {
				t.Error("concurrent request failed")
			}
		})
	}
	wg.Wait()
	type snapshot struct {
		CollectedAt time.Time `json:"collected_at"`
		HTTP        struct {
			Requests uint64            `json:"requests"`
			InFlight uint64            `json:"in_flight"`
			Status   map[string]uint64 `json:"status"`
		} `json:"http"`
		Runtime struct {
			GCPauseMetricsEnabled bool    `json:"gc_pause_metrics_enabled"`
			GCPauseTotalNS        *uint64 `json:"gc_pause_total_ns"`
			Goroutines            int     `json:"goroutines"`
		} `json:"runtime"`
	}
	read := func() snapshot {
		response, body := request(http.MethodGet, "/monitor", "application/json", true)
		if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("invalid snapshot response")
		}
		var value snapshot
		must(t, json.Unmarshal(body, &value))
		return value
	}
	before := read()
	if before.CollectedAt.IsZero() || before.HTTP.Requests != 31 || before.HTTP.InFlight != 0 || before.HTTP.Status["2xx"] != 26 || before.HTTP.Status["3xx"] != 1 || before.HTTP.Status["4xx"] != 2 || before.HTTP.Status["5xx"] != 2 {
		t.Fatalf("HTTP metrics: %+v", before.HTTP)
	}
	if before.Runtime.GCPauseMetricsEnabled || before.Runtime.GCPauseTotalNS != nil || before.Runtime.Goroutines <= 0 {
		t.Fatal("runtime/GC defaults changed")
	}
	// Wait for a genuinely fresh snapshot so caching cannot hide self-counting.
	poll(t, func() bool {
		after := read()
		if !after.CollectedAt.After(before.CollectedAt) {
			return false
		}
		if after.HTTP.Requests != before.HTTP.Requests {
			t.Fatal("Monitor refresh counted itself")
		}
		return true
	})
}

func TestMonitorAccessAndUptimePolicy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	for _, tlsEnabled := range []bool{false, true} {
		for _, protect := range []bool{false, true} {
			t.Run(fmt.Sprintf("tls=%v/protect=%v", tlsEnabled, protect), func(t *testing.T) {
				cfg, client := securityConfig(t, target.URL, "/status", tlsEnabled, true)
				cfg.Monitor.Enabled = true
				cfg.Auth.ProtectUptime = protect
				client.Timeout = 10 * time.Second
				server, err := New(cfg)
				must(t, err)
				running := startServerWithClient(t, server, client)
				for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
					for _, path := range []string{"/monitor", "/monitor/", "/monitor/child", "/%6donitor", "/x/../monitor"} {
						req, err := http.NewRequest(method, running.base+path, nil)
						must(t, err)
						req.Header.Set("Accept", "application/json")
						response, err := client.Do(req)
						must(t, err)
						response.Body.Close()
						assertChallenge(t, response)
					}
				}
				for _, path := range []string{"/monitor%2fchild", "/%256donitor", "/Monitor", "/monitoring", "/monitor-page", "/api/status"} {
					response := securityRequest(t, running, path, "", "")
					response.Body.Close()
					if response.StatusCode != 404 && response.StatusCode != 401 {
						t.Fatalf("path alias exposed data: %s status=%d", path, response.StatusCode)
					}
				}
				for _, path := range []string{"/alerts", "/alerts/", "/%61lerts", "/alerts/child"} {
					response := securityRequest(t, running, path, "", "")
					response.Body.Close()
					assertChallenge(t, response)
					response = securityRequest(t, running, path, securityUser, securityPassword)
					response.Body.Close()
					if response.StatusCode != 404 {
						t.Fatal("reserved alerts namespace gained a handler")
					}
				}
				for _, path := range []string{"/status", "/status/", "/status/api/status", "/%73tatus/api/status", "/x/../status/api/status"} {
					response := securityRequest(t, running, path, "", "")
					body, err := io.ReadAll(response.Body)
					response.Body.Close()
					must(t, err)
					if protect {
						assertChallenge(t, response)
					} else if response.StatusCode != 200 {
						t.Fatalf("public Uptime rejected: %s status=%d", path, response.StatusCode)
					}
					if bytes.Contains(body, []byte("password_hash")) || bytes.Contains(body, []byte("Authorization")) {
						t.Fatal("Uptime leaked configuration")
					}
				}
				for _, path := range []string{"/livez", "/readyz"} {
					response := securityRequest(t, running, path, "", "")
					response.Body.Close()
					if response.StatusCode != 200 {
						t.Fatal("Monitor changed public health")
					}
				}
				for _, test := range []struct{ method, path, accept string }{{http.MethodGet, "/monitor", "text/html"}, {http.MethodGet, "/monitor", "application/json"}, {http.MethodHead, "/monitor/child", "application/json"}} {
					req, err := http.NewRequest(test.method, running.base+test.path, nil)
					must(t, err)
					req.SetBasicAuth(securityUser, securityPassword)
					req.Header.Set("Accept", test.accept)
					response, err := client.Do(req)
					must(t, err)
					body, err := io.ReadAll(response.Body)
					response.Body.Close()
					must(t, err)
					if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), test.accept) || test.method == http.MethodHead && len(body) != 0 {
						t.Fatal("authorized Monitor failed")
					}
				}
				response := securityRequest(t, running, "/monitor", "wrong", "wrong")
				response.Body.Close()
				assertChallenge(t, response)
				running.cancel()
				running.wait(t)
				reopen(t, cfg.Storage.Bbolt.Path)
			})
		}
	}
}

func TestMonitorDisabledAndGuard(t *testing.T) {
	cfg := testConfig(t, "http://never-resolves.invalid/")
	s := &Server{cfg: cfg, app: fiber.New()}
	must(t, s.registerMiddleware())
	for _, path := range []string{"/monitor", "/monitor/child", "/alerts"} {
		response, err := s.app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		must(t, err)
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("disabled Monitor served a route")
		}
	}
	cfg.Monitor.Enabled = true
	s = &Server{cfg: cfg, app: fiber.New()}
	if err := s.registerMiddleware(); err == nil {
		t.Fatal("Monitor accepted missing auth")
	}
}

func seedHistoryHTTP(t *testing.T, store alerts.Store) {
	t.Helper()
	ctx := t.Context()
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	i := alerts.Incident{ID: "history-http", EndpointID: "web", FirstFailureAt: at, ConfirmedAt: at.Add(time.Second), ChannelIDs: []string{"a", "b"}, ReminderPolicy: &alerts.ReminderPolicy{Interval: 5 * time.Minute, MaxReminders: 2}, NextReminderAt: at.Add(time.Second + 5*time.Minute)}
	_, err := store.Apply(ctx, alerts.Transition{Type: alerts.Down, Incident: i})
	must(t, err)
	hs := store.(alerts.HistoryStore)
	ds := store.(alerts.DeliveryStore)
	page, err := hs.ReadIncident(ctx, alerts.DetailQuery{IncidentID: i.ID})
	must(t, err)
	for _, task := range page.Tasks {
		status := alerts.TaskSent
		code := alerts.ErrorCode("")
		if task.ChannelID == "b" {
			status = alerts.TaskFailed
			code = alerts.ErrorRejected
		}
		_, err = ds.RecordOutcome(ctx, alerts.Outcome{EventID: task.EventID, AttemptID: rand.Text(), AttemptedAt: i.ConfirmedAt, Status: status, ErrorCode: code})
		must(t, err)
	}
	i, err = store.ApplyReminder(ctx, alerts.ReminderChange{IncidentID: i.ID, CheckedAt: i.NextReminderAt})
	must(t, err)
	id, err := alerts.ReminderEventID(i.ID, 1, "a")
	must(t, err)
	_, err = ds.RecordOutcome(ctx, alerts.Outcome{EventID: id, AttemptID: rand.Text(), AttemptedAt: i.ConfirmedAt.Add(5 * time.Minute), Status: alerts.TaskPending, ErrorCode: alerts.ErrorTimeout})
	must(t, err)
	i.RecoveredAt = i.ConfirmedAt.Add(6 * time.Minute)
	i.NextReminderAt = time.Time{}
	_, err = store.Apply(ctx, alerts.Transition{Type: alerts.Recovered, Incident: i})
	must(t, err)
	for n := range 23 {
		old := alerts.Incident{ID: fmt.Sprintf("old-%02d", n), EndpointID: "retired", FirstFailureAt: at, ConfirmedAt: at.Add(time.Duration(n) * time.Hour)}
		_, err = store.Apply(ctx, alerts.Transition{Type: alerts.Down, Incident: old})
		must(t, err)
		if n < 22 {
			old.RecoveredAt = old.ConfirmedAt.Add(time.Minute)
			_, err = store.Apply(ctx, alerts.Transition{Type: alerts.Recovered, Incident: old})
			must(t, err)
		}
	}
}

// Both real backends use the production auth and history handler composition.
func exerciseHistoryHTTP(t *testing.T, store alerts.Store) {
	t.Helper()
	seedHistoryHTTP(t, store)
	cfg, _ := securityConfig(t, "http://never-resolves.invalid", "/status", false, true)
	cfg.Alerts.Enabled = true
	cfg.Alerts.HistoryEnabled = true
	cfg.Monitor.Enabled = true
	cfg.Auth.ProtectUptime = false
	const malicious = `<img src=x onerror=alert(1)> & "quoted"`
	cfg.Endpoints[0].ID = "web"
	cfg.Endpoints[0].Name = malicious
	s := &Server{cfg: cfg, app: fiber.New(), alerts: &alertRuntime{store: store}}
	must(t, s.registerMiddleware())
	must(t, s.registerAlertHistory())
	s.app.Get("/status", func(c fiber.Ctx) error { return c.SendStatus(200) })
	request := func(method, path string, auth bool) (*http.Response, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if auth {
			req.SetBasicAuth(securityUser, securityPassword)
		}
		response, err := s.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
		must(t, err)
		data, err := io.ReadAll(response.Body)
		response.Body.Close()
		must(t, err)
		return response, data
	}
	for _, path := range []string{"/alerts", "/alerts/", "/alerts/api/incidents", "/alerts/api/incidents/history-http", "/alerts/unknown", "/%61lerts/api/incidents", "/x/../alerts/api/incidents", "/monitor"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			response, _ := request(method, path, false)
			assertChallenge(t, response)
		}
	}
	for _, path := range []string{"/alerts%2fapi%2fincidents", "/%2561lerts", "/Alerts"} {
		response, _ := request(http.MethodGet, path, false)
		if response.StatusCode != 404 && response.StatusCode != 401 {
			t.Fatal("encoded path bypass")
		}
	}
	for _, path := range []string{"/status", "/livez"} {
		r, _ := request(http.MethodGet, path, false)
		if r.StatusCode != 200 {
			t.Fatal("history changed public routes")
		}
	}
	response, body := request(http.MethodGet, "/alerts?limit=50", true)
	if response.StatusCode != 200 || !bytes.Contains(body, []byte("&lt;img")) || bytes.Contains(body, []byte(malicious)) || response.Header.Get("Cache-Control") != "no-store" || !strings.Contains(response.Header.Get("Content-Security-Policy"), "script-src 'nonce-") {
		t.Fatalf("unsafe HTML: status=%d", response.StatusCode)
	}
	_, defaultBody := request(http.MethodGet, "/alerts", true)
	if !bytes.Contains(defaultBody, []byte("Older incidents")) || !bytes.Contains(body, []byte("System</option>")) {
		t.Fatal("page pagination/theme absent")
	}
	response, body = request(http.MethodGet, "/alerts/api/incidents?limit=2", true)
	var list struct {
		Incidents []incidentView `json:"incidents"`
		Cursor    string         `json:"next_cursor"`
	}
	must(t, json.Unmarshal(body, &list))
	if response.StatusCode != 200 || len(list.Incidents) != 2 || list.Incidents[0].ID != "old-22" || list.Incidents[0].Name != "retired" || list.Cursor == "" {
		t.Fatal("list order/fallback/pagination")
	}
	response, body = request(http.MethodGet, "/alerts/api/incidents?limit=2&cursor="+list.Cursor, true)
	must(t, json.Unmarshal(body, &list))
	if response.StatusCode != 200 || list.Incidents[0].ID != "old-20" {
		t.Fatal("API keyset pagination")
	}
	response, body = request(http.MethodGet, "/alerts/api/incidents/history-http?limit=3", true)
	var detail struct {
		Incident incidentView `json:"incident"`
		Tasks    []taskView   `json:"tasks"`
		Cursor   string       `json:"next_cursor"`
	}
	must(t, json.Unmarshal(body, &detail))
	if response.StatusCode != 200 || detail.Incident.Name != malicious || detail.Incident.Status != "RECOVERED" || detail.Incident.ReminderCount != 1 || len(detail.Tasks) != 3 || detail.Cursor == "" {
		t.Fatal("detail summary/task page")
	}
	response, body = request(http.MethodGet, "/alerts/api/incidents/history-http?limit=3&cursor="+detail.Cursor, true)
	must(t, json.Unmarshal(body, &detail))
	if response.StatusCode != 200 || len(detail.Tasks) != 3 || detail.Tasks[0].Status != alerts.TaskSkipped {
		t.Fatal("detail continuation")
	}
	first, err := store.(alerts.HistoryStore).ReadIncident(t.Context(), alerts.DetailQuery{IncidentID: "history-http"})
	must(t, err)
	found := false
	for _, task := range first.Tasks {
		if task.Type == alerts.Reminder && task.ChannelID == "a" {
			found = true
			if task.Attempts != 1 || task.Status != alerts.TaskSkipped || task.LastErrorCode != alerts.ErrorIncidentRecovered {
				t.Fatal("canceled attempt lost")
			}
		}
	}
	if !found {
		t.Fatal("missing reminder")
	}
	for _, secret := range []string{"attempt_receipts", "last_attempt_id", "reminder_policy", "password_hash", "bearer_token", "SECRET"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("internal history data leaked")
		}
	}
	for _, path := range []string{"/alerts?limit=0", "/alerts?limit=01", "/alerts?limit=51", "/alerts?cursor=bad", "/alerts?limit=2&limit=3", "/alerts?unknown=SECRET", "/alerts?cursor=%zz", "/alerts/api/incidents/history-http?cursor=bad", "/alerts/api/incidents/x/y"} {
		r, b := request(http.MethodGet, path, true)
		if r.StatusCode != 400 || bytes.Contains(b, []byte("SECRET")) {
			t.Fatalf("unsafe invalid query: %s status=%d", path, r.StatusCode)
		}
	}
	r, _ := request(http.MethodGet, "/alerts/api/incidents/missing", true)
	if r.StatusCode != 404 {
		t.Fatal("missing incident")
	}
	r, _ = request(http.MethodPost, "/alerts/api/incidents", true)
	if r.StatusCode != 405 {
		t.Fatal("history accepted write")
	}
	r, b := request(http.MethodHead, "/alerts/api/incidents", true)
	if r.StatusCode != 200 || len(b) != 0 {
		t.Fatal("HEAD semantics")
	}
}

func TestAlertHistoryHTTP(t *testing.T) {
	store, err := alerts.OpenBbolt(filepath.Join(t.TempDir(), "alerts.db"), time.Second)
	must(t, err)
	defer store.Close()
	exerciseHistoryHTTP(t, store)
}

type brokenHistory struct {
	alerts.Store
	alerts.HistoryStore
}

func (brokenHistory) ListIncidents(context.Context, alerts.HistoryQuery) (alerts.IncidentPage, error) {
	return alerts.IncidentPage{}, errors.New("redis://SECRET@private/path")
}
func (brokenHistory) ReadIncident(context.Context, alerts.DetailQuery) (alerts.IncidentDetail, error) {
	return alerts.IncidentDetail{}, errors.New("redis://SECRET@private/path")
}
func TestAlertHistoryErrorsAndDisabled(t *testing.T) {
	cfg, _ := securityConfig(t, "http://never-resolves.invalid", "/status", false, true)
	cfg.Alerts.Enabled = true
	cfg.Alerts.HistoryEnabled = true
	s := &Server{cfg: cfg, app: fiber.New(), alerts: &alertRuntime{store: brokenHistory{}}}
	must(t, s.registerMiddleware())
	must(t, s.registerAlertHistory())
	for _, path := range []string{"/alerts", "/alerts/api/incidents", "/alerts/api/incidents/example"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth(securityUser, securityPassword)
		r, err := s.app.Test(req)
		must(t, err)
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		must(t, err)
		if r.StatusCode != 503 || bytes.Contains(b, []byte("SECRET")) || bytes.Contains(b, []byte("redis://")) {
			t.Fatal("unsafe history error")
		}
	}
	cfg.Alerts.HistoryEnabled = false
	s = &Server{cfg: cfg, app: fiber.New()}
	must(t, s.registerMiddleware())
	must(t, s.registerAlertHistory())
	for _, path := range []string{"/alerts", "/alerts/api/incidents"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth(securityUser, securityPassword)
		r, err := s.app.Test(req)
		must(t, err)
		r.Body.Close()
		if r.StatusCode != 404 {
			t.Fatal("disabled history registered handler")
		}
	}
}

func TestAlertHistoryRuntimeOwnership(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer target.Close()
	cfg, client := securityConfig(t, target.URL, "/status", false, true)
	cfg.Alerts.Enabled = true
	cfg.Alerts.HistoryEnabled = true
	s, err := New(cfg)
	must(t, err)
	var closed atomic.Int32
	var store alerts.Store
	s.deps.openAlerts = func(ctx context.Context, r *runtimeStorage, c config.Config) (alerts.Store, func() error, error) {
		st, close, err := openAlertStore(ctx, r, c)
		store = st
		return st, func() error { closed.Add(1); return close() }, err
	}
	running := startServerWithClient(t, s, client)
	r := securityRequest(t, running, "/alerts/api/incidents", securityUser, securityPassword)
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	must(t, err)
	if r.StatusCode != 200 || !bytes.Contains(b, []byte(`"incidents":[]`)) {
		t.Fatal("runtime history empty state")
	}
	running.cancel()
	running.wait(t)
	if closed.Load() != 1 {
		t.Fatal("history changed storage ownership")
	}
	if _, err := store.Get(context.Background(), "missing"); err == nil {
		t.Fatal("alert store remained open")
	}
	st, err := alerts.OpenBbolt(filepath.Join(filepath.Dir(cfg.Storage.Bbolt.Path), "alerts.db"), time.Second)
	must(t, err)
	must(t, st.Close())
	reopen(t, cfg.Storage.Bbolt.Path)
}
