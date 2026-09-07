// Package memblob is a map-backed ports.BlobStore for tests, handler
// unit tests, and local fallback. Blobs are opaque bytes: content is
// stored and returned verbatim and never logged.
package memblob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"claimops-api/internal/ports"
)

// entry carries one stored blob. Data is never logged: file names and
// bytes may contain PII, and every observe edge emits IDs and status only.
type entry struct {
	data      []byte
	sha256    string
	mime      string
	sizeBytes int64
}

// Store is a mutex-guarded in-memory ports.BlobStore keyed by
// ObjectRef.Key. Put overwrites (re-putting a key is byte-identical under
// ingest dedupe, where keys embed the content hash's document ID).
type Store struct {
	mu    sync.Mutex
	blobs map[string]entry
}

// New builds an empty Store.
func New() *Store {
	return &Store{blobs: make(map[string]entry)}
}

// compile-time check: Store implements the blob port.
var _ ports.BlobStore = (*Store)(nil)

// Put reads content fully and stores it under ref.Key. Only a blank key
// fails.
func (s *Store) Put(ctx context.Context, ref ports.ObjectRef, content io.Reader) error {
	_ = ctx
	if strings.TrimSpace(ref.Key) == "" {
		return fmt.Errorf("memblob: blank object key")
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("memblob: read content: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blobs == nil {
		s.blobs = make(map[string]entry)
	}
	s.blobs[ref.Key] = entry{data: data, sha256: ref.SHA256, mime: ref.MIME, sizeBytes: ref.SizeBytes}
	return nil
}

// Get returns a fresh reader over the stored bytes for ref.Key, or an
// error when absent.
func (s *Store) Get(ctx context.Context, ref ports.ObjectRef) (io.ReadCloser, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.blobs[ref.Key]
	if !ok {
		return nil, fmt.Errorf("memblob: blob not found for key %q", ref.Key)
	}
	return io.NopCloser(bytes.NewReader(e.data)), nil
}

// Delete removes the blob at ref.Key. Deleting a missing key is a no-op
// nil so staged-upload cleanup stays best-effort without error plumbing.
func (s *Store) Delete(ctx context.Context, ref ports.ObjectRef) error {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blobs, ref.Key)
	return nil
}

// Len reports the number of stored blobs. Test hook for asserting
// duplicate uploads leave the blob store untouched.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.blobs)
}
