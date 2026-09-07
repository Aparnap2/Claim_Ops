package config

import (
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with clean env failed: %v", err)
	}
	if cfg.GCPProject != "local-dev" {
		t.Fatalf("GCPProject = %q", cfg.GCPProject)
	}
	if cfg.PubSubTopicDocuments != "document.ingested" {
		t.Fatalf("topic = %q", cfg.PubSubTopicDocuments)
	}
	if cfg.PubSubSubDocuments != "document.ingested-worker" {
		t.Fatalf("subscription = %q", cfg.PubSubSubDocuments)
	}
	if cfg.GCSBucketDocuments != "claimops-documents-local" {
		t.Fatalf("bucket = %q", cfg.GCSBucketDocuments)
	}
	if cfg.OutboxPollMS != 1000 {
		t.Fatalf("poll = %d", cfg.OutboxPollMS)
	}
}

func TestLoadRejectsBadPoll(t *testing.T) {
	t.Setenv("OUTBOX_POLL_MS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for OUTBOX_POLL_MS=0")
	}
	t.Setenv("OUTBOX_POLL_MS", "fast")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for OUTBOX_POLL_MS=fast")
	}
}
