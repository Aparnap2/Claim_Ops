// Package ports defines the outbound port boundaries for the ClaimOps
// application edge. This file adds the blob port used by document
// ingestion to persist opaque upload bytes without coupling callers to
// any storage provider.
package ports

import (
	"context"
	"io"
)

// ObjectRef addresses one opaque blob in provider-neutral terms. Key is
// the object name inside the bucket (never a URI: no scheme, no bucket
// prefix); the adapter resolves the bucket. SHA256/MIME/SizeBytes ride
// along as metadata for writers that accept them.
type ObjectRef struct {
	Key       string
	SHA256    string
	MIME      string
	SizeBytes int64
}

// BlobStore persists opaque byte blobs keyed by ObjectRef.Key. Blobs are
// opaque bytes: implementations must never inspect, log, or derive
// meaning from content.
type BlobStore interface {
	Put(ctx context.Context, ref ObjectRef, content io.Reader) error
	Get(ctx context.Context, ref ObjectRef) (io.ReadCloser, error)
	Delete(ctx context.Context, ref ObjectRef) error
}

// DocumentObjectKey returns the provider-neutral object key for a
// document's original upload bytes:
//
//	tenants/{tenant}/claims/{claimID}/documents/{docID}/original
//
// Callers pass trimmed identifiers; the key carries no scheme or bucket.
func DocumentObjectKey(tenant, claimID, docID string) string {
	return "tenants/" + tenant + "/claims/" + claimID + "/documents/" + docID + "/original"
}
