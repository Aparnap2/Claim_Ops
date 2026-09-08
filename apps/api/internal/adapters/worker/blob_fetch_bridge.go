package workeradapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/ports"
	"claimops-api/internal/worker"
)

// DocLister supplies document metadata for content resolution.
type DocLister interface {
	ListDocuments(ctx context.Context, claim claims.ClaimID) ([]documents.Document, error)
}

// MaxFetchBytes bounds a single blob read (10MB, matching the upload
// admission limit). Reads use LimitReader(MaxFetchBytes+1) so an oversized
// object is detected without unbounded worker memory growth.
const MaxFetchBytes = 10 << 20

var (
	// ErrCorruptedBlob is returned when SHA256(blob bytes) != the
	// ingestion-recorded hash. Alias of the contract sentinel owned by
	// the worker package (which this package already imports for
	// ContentFetcher; a local errors.New here would be a distinct
	// instance invisible to errors.Is across the interface boundary).
	ErrCorruptedBlob = worker.ErrCorruptedBlob
	// ErrBlobTooLarge is returned when the stored object exceeds
	// MaxFetchBytes. Alias of the worker contract sentinel (see above).
	ErrBlobTooLarge = worker.ErrBlobTooLarge
)

// BlobFetchBridge implements worker.ContentFetcher over a ports.BlobStore
// (GCS in production, emulator locally, memblob in tests). File name and
// MIME come from the document row (ListDocuments); bytes come from the
// object key. A missing row or object is transient: redelivery retries,
// then the outcome lands terminal FAILED — never a crash, never a partial
// write (persistence happens downstream of fetch).
type BlobFetchBridge struct {
	Blobs ports.BlobStore
	Docs  DocLister
}

// NewBlobFetchBridge builds a BlobFetchBridge over blobs with metadata
// resolved through docs.
func NewBlobFetchBridge(blobs ports.BlobStore, docs DocLister) *BlobFetchBridge {
	return &BlobFetchBridge{Blobs: blobs, Docs: docs}
}

// compile-time check: BlobFetchBridge satisfies the worker contract.
var _ worker.ContentFetcher = (*BlobFetchBridge)(nil)

// Fetch resolves metadata then bytes for docID.
func (b *BlobFetchBridge) Fetch(ctx context.Context, tenant, claimID, docID string) (string, string, string, error) {
	docs, err := b.Docs.ListDocuments(ctx, claims.ClaimID(claimID))
	if err != nil {
		return "", "", "", fmt.Errorf("blobfetch: list documents: %w", err)
	}
	var meta *documents.Document
	for i, d := range docs {
		if d.ID == docID {
			meta = &docs[i]
			break
		}
	}
	if meta == nil {
		return "", "", "", fmt.Errorf("blobfetch: unknown document %q", docID)
	}
	rc, err := b.Blobs.Get(ctx, ports.ObjectRef{Key: ports.DocumentObjectKey(tenant, claimID, docID)})
	if err != nil {
		return "", "", "", fmt.Errorf("blobfetch: get object: %w", err)
	}
	defer func() { _ = rc.Close() }()
	// Bounded read: LimitReader caps worker memory at MaxFetchBytes+1 so
	// a pathological object cannot OOM the worker via unbounded ReadAll.
	data, err := io.ReadAll(io.LimitReader(rc, MaxFetchBytes+1))
	if err != nil {
		return "", "", "", fmt.Errorf("blobfetch: read object: %w", err)
	}
	if len(data) > MaxFetchBytes {
		return "", "", "", fmt.Errorf("blobfetch: blob for document %q exceeds %d bytes: %w", docID, MaxFetchBytes, ErrBlobTooLarge)
	}
	// Integrity boundary: hash what was read and compare against the
	// ingestion-recorded hash. On mismatch return NOTHING: the bytes must
	// never be parsed. Hashes are not PII; both ride in the error for
	// operator triage.
	sum := sha256.Sum256(data)
	actual := hex.EncodeToString(sum[:])
	if actual != meta.SHA256 {
		return "", "", "", fmt.Errorf("blobfetch: blob SHA256 mismatch for document %q: expected %s got %s: %w", docID, meta.SHA256, actual, ErrCorruptedBlob)
	}
	return meta.FileName, meta.MIME, string(data), nil
}
