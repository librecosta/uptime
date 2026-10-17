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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/v3/uptime"
	uptimestorage "github.com/gofiber/contrib/v3/uptime/storage"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
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
	handler, err := newBasicAuth(config.BasicAuthConfig{Username: "private-user", PasswordHash: "PRIVATE-INVALID-HASH"}, "/status")
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
	auth, err := newBasicAuth(config.BasicAuthConfig{Username: "admin", PasswordHash: string(hash)}, "/status")
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
