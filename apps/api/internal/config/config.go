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

	// GCP/transport settings (same code local + prod; only values change).
	GCPProject           string
	PubSubTopicDocuments string
	PubSubSubDocuments   string
	GCSBucketDocuments   string
	OutboxPollMS         int
	WorkerDatabaseURL    string
	DatabaseURL          string
	PolicyBaseURL        string
}

// Load reads configuration from the environment with safe defaults.
func Load() (Config, error) {
	cfg := Config{
		Port:                 envOr("PORT", "8000"),
		AppEnv:               envOr("APP_ENV", "local"),
		GCPProject:           envOr("GCP_PROJECT", "local-dev"),
		PubSubTopicDocuments: envOr("PUBSUB_TOPIC_DOCUMENTS", "document.ingested"),
		PubSubSubDocuments:   envOr("PUBSUB_SUBSCRIPTION_DOCUMENTS", "document.ingested-worker"),
		GCSBucketDocuments:   envOr("GCS_BUCKET_DOCUMENTS", "claimops-documents-local"),
		WorkerDatabaseURL:    envOr("WORKER_DATABASE_URL", "postgres://claimops_worker:claimops_worker@localhost:5433/claimops"),
		DatabaseURL:          envOr("DATABASE_URL", os.Getenv("TEST_POSTGRES_DSN")),
		PolicyBaseURL:        envOr("POLICY_BASE_URL", "http://localhost:3001"),
	}
	pollRaw := envOr("OUTBOX_POLL_MS", "1000")
	poll, err := strconv.Atoi(pollRaw)
	if err != nil || poll <= 0 {
		return Config{}, fmt.Errorf("invalid OUTBOX_POLL_MS %q: must be a positive integer", pollRaw)
	}
	cfg.OutboxPollMS = poll
	otelRaw := envOr("OTEL_ENABLED", "false")
	enabled, err := strconv.ParseBool(otelRaw)
	if err != nil {
		return Config{}, fmt.Errorf("invalid OTEL_ENABLED %q: %w", otelRaw, err)
	}
	cfg.OtelEnabled = enabled
	if cfg.Port == "" {
		return Config{}, fmt.Errorf("PORT must not be empty")
	}
	if cfg.GCPProject == "" {
		return Config{}, fmt.Errorf("GCP_PROJECT must not be empty")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
