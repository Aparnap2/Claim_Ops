// Package config loads service configuration from the environment.
// Fail-fast: invalid values are rejected at startup, never mid-request.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds the runtime configuration for the Go API edge.
type Config struct {
	Port        string
	AppEnv      string
	OtelEnabled bool
}

// Load reads configuration from the environment with safe defaults.
func Load() (Config, error) {
	cfg := Config{
		Port:   envOr("PORT", "8000"),
		AppEnv: envOr("APP_ENV", "local"),
	}
	otelRaw := envOr("OTEL_ENABLED", "false")
	enabled, err := strconv.ParseBool(otelRaw)
	if err != nil {
		return Config{}, fmt.Errorf("invalid OTEL_ENABLED %q: %w", otelRaw, err)
	}
	cfg.OtelEnabled = enabled
	if cfg.Port == "" {
		return Config{}, fmt.Errorf("PORT must not be empty")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
