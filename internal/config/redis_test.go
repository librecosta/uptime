package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestRedisRetentionLimit(t *testing.T) {
	for _, backend := range []string{"bbolt", "redis"} {
		for _, days := range []int{1, 90, 106750, 106751, 200000} {
			changes := map[string]any{"storage.type": backend, "storage.redis.url": "redis://user:SECRET@never-resolves.invalid", "uptime.retention": fmt.Sprintf("%dd", days), "uptime.window": "1d"}
			cfg, err := load(configData(t, changes), emptyEnv)
			if backend == "redis" && days > 106750 {
				if err == nil || !strings.Contains(err.Error(), "uptime.retention") || strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("missing or unsafe Redis retention error: %v", err)
				}
			} else if err != nil || cfg.Uptime.Retention != CalendarDays(days) {
				t.Fatalf("%s retention %d: %v", backend, days, err)
			}
		}
		cfg := mustLoad(t, configData(t, map[string]any{"storage.type": backend, "storage.redis.url": "redis://never-resolves.invalid"}), emptyEnv)
		if cfg.Uptime.Retention != 90 {
			t.Fatal("retention default changed")
		}
	}
}

func TestRedisURLParserContract(t *testing.T) {
	for _, value := range []string{
		"redis://never-resolves.invalid",
		"redis://user:SECRET@never-resolves.invalid:6379/2?read_timeout=2s&max_retries=1",
		"rediss://user:SECRET@never-resolves.invalid:6380/3?db=4&protocol=2",
		"redis://[::1]:6379/0",
	} {
		cfg := mustLoad(t, configData(t, map[string]any{"storage.type": "redis", "storage.redis.url": value}), emptyEnv)
		if cfg.Storage.Redis.URL.String() != value || cfg.Storage.Bbolt != nil {
			t.Fatal("normalized Redis URL changed")
		}
		opts, err := redis.ParseURL(cfg.Storage.Redis.URL.String())
		if err != nil {
			t.Fatal("configuration accepted a URL rejected by go-redis")
		}
		if strings.HasPrefix(value, "rediss://") && opts.TLSConfig == nil {
			t.Fatal("rediss lost client TLS")
		}
	}
	for _, value := range []string{
		"redis://user:SECRET@host/0#SECRET", "redis://host/0#",
		"redis://user:SECRET@host/SECRET", "redis://host/1/SECRET",
		"redis://host?unknown=SECRET", "redis://host?read_timeout=SECRET",
		"redis://host?db=SECRET", "redis://host?max_retries=SECRET",
		"unix:///SECRET", "redis:///1", "redis://user:SECRET@:6379/0",
	} {
		for _, input := range []string{value, "${REDIS_URL}"} {
			_, err := load(configData(t, map[string]any{"storage.type": "redis", "storage.redis.url": input}), env(map[string]string{"REDIS_URL": value}))
			if err == nil || !strings.Contains(err.Error(), "storage.redis.url") || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), value) {
				t.Fatalf("unsafe/missing Redis validation error: %v", err)
			}
		}
	}
	// Inactive values remain opaque, even when invalid or unresolved.
	mustLoad(t, configData(t, map[string]any{"storage.redis.url": "redis://user:SECRET@host/invalid", "storage.redis.key_prefix": "${MISSING}"}), emptyEnv)
}

func TestRedisKeyPrefix(t *testing.T) {
	for _, prefix := range []string{"fiber:uptime", "prod:asia:uptime", "生产:监控", "internal space", "nested::prefix"} {
		for _, input := range []string{prefix, "${PREFIX}"} {
			cfg := mustLoad(t, configData(t, map[string]any{"storage.type": "redis", "storage.redis.url": "redis://never-resolves.invalid", "storage.redis.key_prefix": input}), env(map[string]string{"PREFIX": prefix}))
			if cfg.Storage.Redis.KeyPrefix != prefix {
				t.Fatal("prefix changed")
			}
		}
	}
	for _, prefix := range []string{"", ":uptime", "uptime:", ":", " uptime", "uptime ", "\u2003uptime", "uptime\u00a0", "up\ntime", "up\ttime", "up\x00time", "up\x7ftime", "up\u0085time"} {
		for _, input := range []string{prefix, "${PREFIX}"} {
			wantInvalid(t, configData(t, map[string]any{"storage.type": "redis", "storage.redis.url": "redis://never-resolves.invalid", "storage.redis.key_prefix": input}), env(map[string]string{"PREFIX": prefix}), "storage.redis.key_prefix")
		}
	}
	for _, field := range []string{"pool_size", "sentinel", "cluster", "reset", "dial_timeout", "state_ttl"} {
		wantInvalid(t, configData(t, map[string]any{"storage.type": "redis", "storage.redis.url": "redis://never-resolves.invalid", "storage.redis." + field: 1}), emptyEnv, "unknown field")
	}
}
