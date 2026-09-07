package workeradapter

import (
	"context"
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
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", "", "", fmt.Errorf("blobfetch: read object: %w", err)
	}
	return meta.FileName, meta.MIME, string(data), nil
}
