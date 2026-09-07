package memblob_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"claimops-api/internal/adapters/memblob"
	"claimops-api/internal/ports"
)

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := memblob.New()
	ref := ports.ObjectRef{Key: ports.DocumentObjectKey("t1", "CLM-1", "doc-abc"), SHA256: "deadbeef", MIME: "application/pdf", SizeBytes: 5}
	if err := s.Put(ctx, ref, bytes.NewReader([]byte("hello"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello" {
		t.Fatalf("round trip = %q, want %q", got, "hello")
	}
}

func TestGetMissingKey(t *testing.T) {
	s := memblob.New()
	if _, err := s.Get(context.Background(), ports.ObjectRef{Key: "nope"}); err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

func TestPutBlankKey(t *testing.T) {
	s := memblob.New()
	if err := s.Put(context.Background(), ports.ObjectRef{}, bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("expected error for blank key, got nil")
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	s := memblob.New()
	ref := ports.ObjectRef{Key: "k1"}
	if err := s.Put(ctx, ref, bytes.NewReader([]byte("v"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, ref); err == nil {
		t.Fatal("expected missing after Delete, got value")
	}
	// Missing-key delete stays nil (best-effort cleanup contract).
	if err := s.Delete(ctx, ref); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if n := s.Len(); n != 0 {
		t.Fatalf("Len = %d, want 0", n)
	}
}
