// Package config reads TT_* environment variables with defaults.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Common holds settings every service shares. Defaults match deploy/docker-compose.yml from the host.
type Common struct {
	PostgresURL   string
	ClickHouseURL string
	KafkaBrokers  []string
	RedisURL      string
	LogLevel      string
}

// Load reads the common settings.
func Load() Common {
	return Common{
		PostgresURL:   String("TT_POSTGRES_URL", "postgres://trooper:trooper@localhost:5432/trooper?sslmode=disable"),
		ClickHouseURL: String("TT_CLICKHOUSE_URL", "clickhouse://trooper:trooper@localhost:9000/telemetry"),
		KafkaBrokers:  List("TT_KAFKA_BROKERS", "localhost:19092"),
		RedisURL:      String("TT_REDIS_URL", "redis://localhost:6379/0"),
		LogLevel:      String("TT_LOG_LEVEL", "info"),
	}
}

// String returns env key or def.
func String(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// Int returns env key parsed as int, or def.
func Int(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// Duration returns env key parsed by time.ParseDuration, or def.
func Duration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// Bool returns env key parsed as bool, or def.
func Bool(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// List returns env key split on commas, or def split on commas.
func List(key, def string) []string {
	var out []string
	for _, s := range strings.Split(String(key, def), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
