// Package gcsblob implements ports.BlobStore over Google Cloud Storage.
// Object keys stay provider-neutral (ports.DocumentObjectKey: no scheme,
// no bucket); this adapter resolves the bucket supplied to New.
//
// Emulator note: there is deliberately NO custom-endpoint code here.
// STORAGE_EMULATOR_HOST is honored by the storage client library itself,
// so tests and local runs point the client at the emulator purely via
// that environment variable.
//
// Writer attributes are kept minimal — ContentType only. GCS computes
// integrity hashes server-side on upload, and the client library sets
// CRC32C when it can; pinning hashes from here would duplicate that
// mechanism and complicate emulator parity, so ref.SHA256/SizeBytes ride
// on ObjectRef for callers that need them without being pushed to GCS.
package gcsblob

import (
	"context"
	"fmt"
	"io"

	"cloud.google.com/go/storage"

	"claimops-api/internal/ports"
)

// Store is a ports.BlobStore backed by one GCS bucket.
type Store struct {
	bucket string
	client *storage.Client
}

// New builds a Store writing to bucket via client. Client construction
// (including emulator selection via STORAGE_EMULATOR_HOST) is the
// caller's job.
func New(bucket string, client *storage.Client) *Store {
	return &Store{bucket: bucket, client: client}
}

// compile-time check: Store implements the blob port.
var _ ports.BlobStore = (*Store)(nil)

// Put streams content to bucket/object ref.Key with ContentType=ref.MIME.
func (s *Store) Put(ctx context.Context, ref ports.ObjectRef, content io.Reader) error {
	w := s.client.Bucket(s.bucket).Object(ref.Key).NewWriter(ctx)
	w.ObjectAttrs.ContentType = ref.MIME
	if _, err := io.Copy(w, content); err != nil {
		_ = w.Close()
		return fmt.Errorf("gcsblob: put %q: %w", ref.Key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcsblob: put %q: %w", ref.Key, err)
	}
	return nil
}

// Get opens a reader for bucket/object ref.Key.
func (s *Store) Get(ctx context.Context, ref ports.ObjectRef) (io.ReadCloser, error) {
	rc, err := s.client.Bucket(s.bucket).Object(ref.Key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcsblob: get %q: %w", ref.Key, err)
	}
	return rc, nil
}

// Delete removes bucket/object ref.Key.
func (s *Store) Delete(ctx context.Context, ref ports.ObjectRef) error {
	if err := s.client.Bucket(s.bucket).Object(ref.Key).Delete(ctx); err != nil {
		return fmt.Errorf("gcsblob: delete %q: %w", ref.Key, err)
	}
	return nil
}
