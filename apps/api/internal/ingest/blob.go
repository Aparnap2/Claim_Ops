package ingest

import (
	"errors"
	"strings"
	"sync"
)

// ErrBlankDocID is returned when a document identifier is empty or blank.
var ErrBlankDocID = errors.New("ingest: blank document id")

// ErrBlobNotFound is returned when no blob is stored for a document ID.
var ErrBlobNotFound = errors.New("ingest: blob not found")

// blobEntry carries the fetchable payload for one ingested document.
// Content is never logged: file names and text may contain PII, and the
// worker's observe edge emits IDs and status only.
type blobEntry struct {
	fileName string
	mime     string
	content  string
}

// BlobStore is an in-memory content-addressable-by-docID payload store.
// The ingest handler Puts the raw upload on create so the document worker
// can Fetch it later via the fetch bridge. Mutex-guarded; Put overwrites
// (re-putting the same docID is byte-identical under ingest dedupe).
type BlobStore struct {
	mu    sync.Mutex
	blobs map[string]blobEntry
}

// NewBlobStore builds an empty BlobStore.
func NewBlobStore() *BlobStore {
	return &BlobStore{blobs: make(map[string]blobEntry)}
}

// Put stores the upload payload for docID. Only a blank docID fails;
// memory pressure is not modelled, so callers treat Put as infallible.
func (b *BlobStore) Put(docID, fileName, mime, content string) error {
	if strings.TrimSpace(docID) == "" {
		return ErrBlankDocID
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blobs == nil {
		b.blobs = make(map[string]blobEntry)
	}
	b.blobs[docID] = blobEntry{fileName: fileName, mime: mime, content: content}
	return nil
}

// Get returns the stored payload for docID, or ErrBlobNotFound when absent.
func (b *BlobStore) Get(docID string) (fileName, mime, content string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.blobs[docID]
	if !ok {
		return "", "", "", ErrBlobNotFound
	}
	return e.fileName, e.mime, e.content, nil
}

// Len reports the number of stored blobs. Test hook for asserting
// duplicate uploads leave the blob store untouched.
func (b *BlobStore) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.blobs)
}
