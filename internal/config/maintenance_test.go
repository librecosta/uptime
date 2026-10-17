package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMaintenanceConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"explicit empty", "endpoints: []", true},
		{"ordinary", minimal, true},
		{"omitted", "storage: {type: bbolt}", false},
		{"null", "endpoints: null", false},
		{"wrong type", "endpoints: {}", false},
		{"unknown", "endpoints: []\nSECRET: true", false},
		{"duplicate", "endpoints: []\nendpoints: []", false},
		{"documents", "endpoints: []\n---\nendpoints: []", false},
		{"active TLS", "endpoints: []\nserver: {tls: {enabled: true, cert_file: SECRET, key_file: SECRET}}", false},
		{"active auth", "endpoints: []\nauth: {enabled: true, basic: {username: test, password_hash: SECRET}}", false},
		{"active redis", "endpoints: []\nstorage: {type: redis, redis: {url: 'redis://SECRET@host:bad'}}", false},
		{"active env", "endpoints: []\nui: {title: '${MISSING_TITLE}'}", false},
		{"inactive env", "endpoints: []\nauth: {basic: {username: '${MISSING_SECRET}'}}", true},
		{"invalid endpoint", "endpoints: [{id: web, url: SECRET}]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadMode([]byte(tc.data), emptyEnv, true)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if err != nil && (strings.Contains(err.Error(), "SECRET") || !reflect.DeepEqual(cfg, Config{})) {
				t.Fatal("maintenance loader leaked input or returned partial config")
			}
			if tc.name == "ordinary" {
				standard := mustLoad(t, []byte(tc.data), emptyEnv)
				if !reflect.DeepEqual(cfg, standard) {
					t.Fatal("maintenance mode changed normal configuration")
				}
			}
		})
	}
	path := filepath.Join(t.TempDir(), "maintenance.yaml")
	if err := os.WriteFile(path, []byte("endpoints: []"), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadServiceFile(path); err != nil || len(cfg.Endpoints) != 0 {
		t.Fatalf("explicit maintenance file: %v", err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("standard loader accepted empty endpoints")
	}
	if _, err := LoadServiceFile(filepath.Join(t.TempDir(), "SECRET")); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("missing or unsafe maintenance read error")
	}
}
