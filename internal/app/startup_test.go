package app

import (
	"strings"
	"testing"
)

func TestStartupMessage(t *testing.T) {
	for _, tc := range []struct {
		address, path, entry string
		tls                  bool
	}{
		{"127.0.0.1:18580", "/uptime", "http://127.0.0.1:18580/uptime", false},
		{"127.0.0.1:43210", "/nested/status", "http://127.0.0.1:43210/nested/status", false},
		{"[::1]:8080", "/status", "http://[::1]:8080/status", false},
		{"[fe80::1%eth0]:18580", "/status", "http://[fe80::1%25eth0]:18580/status", false},
		{"0.0.0.0:18580", "/status", "http://<host>:18580/status", false},
		{"[::]:18580", "/status", "http://<host>:18580/status", false},
		{"127.0.0.1:18580", "/status", "https://<certificate-host>:18580/status", true},
		{"[::]:8443", "/nested/status", "https://<certificate-host>:8443/nested/status", true},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			message := startupMessage(tc.address, tc.path, tc.tls)
			protocol := "HTTP"
			if tc.tls {
				protocol = "HTTPS"
				if !strings.Contains(message, "covered by your TLS certificate") || strings.Contains(message, "localhost") {
					t.Fatal("TLS entry assumes a certificate identity")
				}
			}
			want := "LibreCosta Uptime listening on " + tc.address + " (" + protocol + "); dashboard: " + tc.entry
			if !strings.HasPrefix(message, want) {
				t.Fatalf("startup message = %q, want prefix %q", message, want)
			}
		})
	}
}
