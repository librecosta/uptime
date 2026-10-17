package app

import (
	"fmt"
	"net"
	"strings"
)

// Use the bound TCP address, never a request Host header or an endpoint URL.
func startupMessage(address, uiPath string, useTLS bool) string {
	protocol, scheme := "HTTP", "http"
	host, port, err := net.SplitHostPort(address)
	if useTLS {
		protocol, scheme = "HTTPS", "https"
		// The bind address says nothing about the certificate's identities.
		host = "<certificate-host>"
	} else if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "<host>"
	}
	// Escape only the authority's IPv6 zone separator, preserving the UI path.
	entry := scheme + "://" + strings.ReplaceAll(net.JoinHostPort(host, port), "%", "%25") + uiPath
	if err != nil {
		entry = uiPath
	}
	message := fmt.Sprintf("LibreCosta Uptime listening on %s (%s); dashboard: %s", address, protocol, entry)
	if useTLS {
		message += " (use a hostname or IP covered by your TLS certificate)"
	}
	return message
}
