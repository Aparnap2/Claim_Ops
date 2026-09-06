// Pure unit tests for the deterministic evidence core: no network, no I/O,
// no clock reads. Covers New() validation, content-hash determinism,
// default status, and ID uniqueness.
package evidence_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"claimops-api/internal/evidence"
)

// evidenceAt is the fixed retrieval timestamp for tests that need a
// non-zero time without reading the clock.
var evidenceAt = time.Date(2026, time.February, 10, 8, 0, 0, 0, time.UTC)

func TestNewRejects(t *testing.T) {
	body := []byte(`{"policy_id":"POL-001"}`)
	cases := []struct {
		name       string
		tenant     string
		claim      string
		sourceType string
		sourceID   string
		at         time.Time
	}{
		{name: "blank tenant", tenant: "", claim: "CLM-001", sourceType: "policy", sourceID: "SRC-001", at: evidenceAt},
		{name: "whitespace tenant", tenant: "   ", claim: "CLM-001", sourceType: "policy", sourceID: "SRC-001", at: evidenceAt},
		{name: "blank claim", tenant: "tenant-a", claim: "", sourceType: "policy", sourceID: "SRC-001", at: evidenceAt},
		{name: "whitespace claim", tenant: "tenant-a", claim: "  ", sourceType: "policy", sourceID: "SRC-001", at: evidenceAt},
		{name: "blank source id", tenant: "tenant-a", claim: "CLM-001", sourceType: "policy", sourceID: "", at: evidenceAt},
		{name: "whitespace source id", tenant: "tenant-a", claim: "CLM-001", sourceType: "policy", sourceID: "  ", at: evidenceAt},
		{name: "blank source type", tenant: "tenant-a", claim: "CLM-001", sourceType: "", sourceID: "SRC-001", at: evidenceAt},
		{name: "whitespace source type", tenant: "tenant-a", claim: "CLM-001", sourceType: "   ", sourceID: "SRC-001", at: evidenceAt},
		{name: "bad source type", tenant: "tenant-a", claim: "CLM-001", sourceType: "email", sourceID: "SRC-001", at: evidenceAt},
		{name: "uppercase source type", tenant: "tenant-a", claim: "CLM-001", sourceType: "POLICY", sourceID: "SRC-001", at: evidenceAt},
		{name: "zero time", tenant: "tenant-a", claim: "CLM-001", sourceType: "policy", sourceID: "SRC-001", at: time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := evidence.New(tc.tenant, tc.claim, tc.sourceType, tc.sourceID, body, tc.at); err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

func TestNewHashAndStatus(t *testing.T) {
	body := []byte(`{"policy_id":"POL-001","tenant_id":"tenant-a"}`)
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	got, err := evidence.New("tenant-a", "CLM-001", "policy", "SRC-001", body, evidenceAt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ContentHash != want {
		t.Fatalf("want content hash %q, got %q", want, got.ContentHash)
	}
	if got.Status != evidence.EvidenceStatusCaptured {
		t.Fatalf("want status %q, got %q", evidence.EvidenceStatusCaptured, got.Status)
	}
	if got.ID == "" {
		t.Fatalf("want non-blank ID")
	}
}

func TestNewDistinctIDs(t *testing.T) {
	body := []byte(`{"policy_id":"POL-001"}`)
	first, err := evidence.New("tenant-a", "CLM-001", "policy", "SRC-001", body, evidenceAt)
	if err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	second, err := evidence.New("tenant-a", "CLM-001", "policy", "SRC-001", body, evidenceAt)
	if err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if first.ID == "" || second.ID == "" {
		t.Fatalf("want non-blank IDs, got %q and %q", first.ID, second.ID)
	}
	if first.ID == second.ID {
		t.Fatalf("want distinct IDs, both were %q", first.ID)
	}
}
