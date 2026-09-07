package gcsblob_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"claimops-api/internal/adapters/gcsblob"
	"claimops-api/internal/ports"
)

// TestDocumentObjectKeyShape is a pure unit test: the key carries no
// scheme, no bucket, no illegal characters, and the exact segment shape.
func TestDocumentObjectKeyShape(t *testing.T) {
	key := ports.DocumentObjectKey("t-apollo", "CLM-1", "doc-abc123")
	want := "tenants/t-apollo/claims/CLM-1/documents/doc-abc123/original"
	if key != want {
		t.Fatalf("key = %q, want %q", key, want)
	}
	for _, illegal := range []string{"gs://", " ", "\\", "?", "#"} {
		if strings.Contains(key, illegal) {
			t.Fatalf("key %q contains illegal %q", key, illegal)
		}
	}
	if strings.HasPrefix(key, "/") {
		t.Fatalf("key %q must not start with /", key)
	}
}

// emulatorClient returns a storage client pointed at the emulator, or
// skips the test when no emulator is reachable. `go test` passes WITHOUT
// an emulator via this skip: the probe is STORAGE_EMULATOR_HOST, falling
// back to a localhost:4443 TCP dial.
func emulatorClient(t *testing.T) *storage.Client {
	t.Helper()
	host := os.Getenv("STORAGE_EMULATOR_HOST")
	if host == "" {
		host = "localhost:4443"
		conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
		if err != nil {
			t.Skipf("storage emulator unreachable at %s: %v", host, err)
		}
		conn.Close()
		os.Setenv("STORAGE_EMULATOR_HOST", host)
		t.Cleanup(func() { os.Unsetenv("STORAGE_EMULATOR_HOST") })
	}
	ctx := context.Background()
	client, err := storage.NewClient(ctx, option.WithoutAuthentication())
	if err != nil {
		t.Skipf("storage emulator client: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// TestStoreRoundTrip exercises the REAL adapter against the emulator when
// up, skipping otherwise.
func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := emulatorClient(t)
	bucket := "claimops-test-gcsblob"
	if err := client.Bucket(bucket).Create(ctx, "test-project", nil); err != nil &&
		!strings.Contains(err.Error(), "409") &&
		!strings.Contains(strings.ToLower(err.Error()), "already exists") &&
		!strings.Contains(strings.ToLower(err.Error()), "you already own this bucket") {
		t.Fatalf("create bucket: %v", err)
	}
	var _ ports.BlobStore = gcsblob.New(bucket, client)
	s := gcsblob.New(bucket, client)
	ref := ports.ObjectRef{
		Key:       ports.DocumentObjectKey("t-apollo", "CLM-1", "doc-emulator1"),
		SHA256:    "emulatorsha",
		MIME:      "application/pdf",
		SizeBytes: int64(len("opaque-bytes")),
	}
	if err := s.Put(ctx, ref, bytes.NewReader([]byte("opaque-bytes"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "opaque-bytes" {
		t.Fatalf("round trip = %q, want %q", got, "opaque-bytes")
	}
	if err := s.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, ref); err == nil {
		t.Fatal("expected error after Delete, got nil")
	}
}
