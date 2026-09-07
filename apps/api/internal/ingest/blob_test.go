package ingest_test

import (
	"errors"
	"testing"

	"claimops-api/internal/ingest"
)

func TestBlobPutGetRoundTrip(t *testing.T) {
	b := ingest.NewBlobStore()
	if err := b.Put("doc-1", "bill.pdf", "application/pdf", "hello"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fn, mime, content, err := b.Get("doc-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fn != "bill.pdf" || mime != "application/pdf" || content != "hello" {
		t.Fatalf("round trip = %q %q %q, want bill.pdf application/pdf hello", fn, mime, content)
	}
}

func TestBlobPutBlankDocID(t *testing.T) {
	b := ingest.NewBlobStore()
	if err := b.Put("  ", "a.pdf", "text/plain", "x"); !errors.Is(err, ingest.ErrBlankDocID) {
		t.Fatalf("Put blank = %v, want ErrBlankDocID", err)
	}
}

func TestBlobGetMissing(t *testing.T) {
	b := ingest.NewBlobStore()
	if _, _, _, err := b.Get("doc-nope"); !errors.Is(err, ingest.ErrBlobNotFound) {
		t.Fatalf("Get missing = %v, want ErrBlobNotFound", err)
	}
}

func TestBlobPutOverwrite(t *testing.T) {
	b := ingest.NewBlobStore()
	if err := b.Put("doc-1", "a.pdf", "text/plain", "v1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Put("doc-1", "a.pdf", "text/plain", "v2"); err != nil {
		t.Fatalf("re-Put: %v", err)
	}
	_, _, content, err := b.Get("doc-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if content != "v2" {
		t.Fatalf("content = %q, want v2 (last write wins)", content)
	}
	if b.Len() != 1 {
		t.Fatalf("Len = %d, want 1", b.Len())
	}
}
