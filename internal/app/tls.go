package app

import (
	"crypto/tls"

	"github.com/librecosta/uptime/internal/config"
)

func buildTLSConfig(cfg *config.TLSConfig) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cfg.Certificate},
	}
}
