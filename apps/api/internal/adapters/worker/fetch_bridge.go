package workeradapter

import (
	"context"

	"claimops-api/internal/ingest"
)

// FetchBridge implements worker.ContentFetcher over the in-memory blob
// store staged at ingestion time. A missing blob (unknown document ID)
// surfaces ingest.ErrBlobNotFound, which the worker treats as transient:
// a redelivered event for a genuinely missing blob exhausts retries and
// lands terminal FAILED rather than crashing the consumer.
type FetchBridge struct {
	Blob *ingest.BlobStore
}

// NewFetchBridge builds a FetchBridge over blob.
func NewFetchBridge(blob *ingest.BlobStore) *FetchBridge {
	return &FetchBridge{Blob: blob}
}

// Fetch returns the staged file name, MIME, and text content for docID.
// Tenant/claimID scope the call signature; the blob store is keyed by
// document ID (unique per tenant+claim by construction at Put time).
func (b *FetchBridge) Fetch(_ context.Context, _, _, docID string) (string, string, string, error) {
	return b.Blob.Get(docID)
}
