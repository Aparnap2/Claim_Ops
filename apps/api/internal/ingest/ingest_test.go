package ingest_test

import (
	"errors"
	"strings"
	"testing"

	"claimops-api/internal/ingest"
)

func TestPutIdempotentSameTenantClaimContent(t *testing.T) {
	s := ingest.New()
	first, created, err := s.Put("t-apollo", "CLM-1", "bill.pdf", "application/pdf", "hello")
	if err != nil {
		t.Fatalf("first Put failed: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on first Put")
	}
	if !strings.HasPrefix(first.ID, "doc-") || len(first.ID) != len("doc-")+12 {
		t.Fatalf("expected ID doc-+12 hex, got %q", first.ID)
	}
	if first.Status != "RECEIVED" {
		t.Fatalf("expected Status RECEIVED, got %q", first.Status)
	}
	if first.SHA256 == "" {
		t.Fatal("expected non-empty SHA256")
	}
	if first.SizeBytes != int64(len("hello")) {
		t.Fatalf("expected SizeBytes %d, got %d", len("hello"), first.SizeBytes)
	}
	second, created, err := s.Put("t-apollo", "CLM-1", "bill.pdf", "application/pdf", "hello")
	if err != nil {
		t.Fatalf("second Put failed: %v", err)
	}
	if created {
		t.Fatal("expected created=false on duplicate Put")
	}
	if second.ID != first.ID || second.SHA256 != first.SHA256 {
		t.Fatalf("duplicate must return original document: %+v vs %+v", first, second)
	}
}

func TestPutBlankRejections(t *testing.T) {
	s := ingest.New()
	cases := []struct {
		name    string
		tenant  string
		claim   string
		file    string
		content string
		wantErr error // nil means any non-nil plain error; ErrEmptyDocument checked explicitly
	}{
		{"blank tenant", "  ", "CLM-1", "a.pdf", "x", nil},
		{"blank claim", "t-1", "  ", "a.pdf", "x", nil},
		{"blank fileName", "t-1", "CLM-1", "  ", "x", nil},
		{"empty content", "t-1", "CLM-1", "a.pdf", "", ingest.ErrEmptyDocument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.Put(tc.tenant, tc.claim, tc.file, "text/plain", tc.content)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}
