// Package ingest owns idempotent document intake for the edge. This
// file adds the Postgres-backed upload service: opaque bytes go to the
// blob port first, then the document row and its outbox event commit
// atomically in one tenant-scoped transaction.
//
// Transport note (parser-agnostic): blobs are opaque bytes and events
// stay IDs-only. The upload payload carries exactly the envelope keys
// {schema_version, event_id, occurred_at, tenant, claim, document_id,
// sha256} — no other keys, ever (see TestUploadEventHasNoParserFields).
package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// errValidation marks request-shape failures (blank identifiers, empty
// content). Handlers map these to 400; everything else is a 500-class
// backend failure. Use IsValidation to test membership.
var errValidation = errors.New("ingest: validation")

// IsValidation reports whether err is a request-shape failure.
func IsValidation(err error) bool {
	return errors.Is(err, errValidation)
}

func validationError(msg string) error {
	return fmt.Errorf("%w: %s", errValidation, msg)
}

// uploadEventKeys is the closed key set for the document.ingested.v1
// payload. The service builds exactly these keys; tests assert the set.
var uploadEventKeys = []string{
	"schema_version",
	"event_id",
	"occurred_at",
	"tenant",
	"claim",
	"document_id",
	"sha256",
}

// Service stages opaque upload bytes in a ports.BlobStore and records
// the document row plus its outbox event in Postgres. Repo is built from
// pool by NewService so callers pass only the blob port and the pool.
type Service struct {
	Blobs ports.BlobStore
	Pool  *pgxpool.Pool
	Repo  *postgres.Repository
}

// NewService builds a Service with a repository backed by pool.
func NewService(blobs ports.BlobStore, pool *pgxpool.Pool) *Service {
	return &Service{Blobs: blobs, Pool: pool, Repo: postgres.New(pool)}
}

// BuildUploadPayload returns the document.ingested.v1 event payload for
// doc: exactly the envelope keys of uploadEventKeys, nothing else.
// occurredAt is rendered RFC3339 UTC. Exported so tests can assert the
// key set without reaching into transaction internals.
func BuildUploadPayload(doc documents.Document, occurredAt time.Time) map[string]string {
	return map[string]string{
		"schema_version": ports.DocumentIngestedSchemaVersion,
		"event_id":       "evt-" + doc.ID,
		"occurred_at":    occurredAt.UTC().Format(time.RFC3339),
		"tenant":         string(doc.Tenant),
		"claim":          string(doc.ClaimID),
		"document_id":    doc.ID,
		"sha256":         doc.SHA256,
	}
}

// Upload ingests one document's opaque bytes idempotently.
//
//  1. Validates identifiers/content (blank -> validation error).
//  2. Hashes content (sha256 hex) and mints docID "doc-"+12hex via newID.
//  3. Puts bytes to the blob port FIRST under
//     ports.DocumentObjectKey(tenant, claimID, docID). A blob failure is
//     a 500-class error with zero DB writes.
//  4. Inserts the RECEIVED document row plus the document.ingested.v1
//     outbox event in one tenant-scoped transaction and commits.
//
// Storage-column note: postgres.InsertDocument writes storage_uri as ""
// (legacy NOT NULL column with no domain counterpart on
// documents.Document — see document_repository.go). This service follows
// that behavior and does not invent a URI: the object address is
// deterministically reconstructible from (tenant, claim, docID) via
// DocumentObjectKey.
//
// Duplicate note: on inserted=false the transaction rolls back, the
// staged key is best-effort deleted, and Upload returns
// (Document{}, false, nil). The existing row cannot be fetched back
// without a lookup the repository does not offer, so callers treat
// created=false as "already stored". Race limitation: a crash between
// the blob Put and the best-effort Delete orphans that staged key;
// staged keys are content-addressed by unique docID so they never
// collide, and a future lifecycle sweeper owns their cleanup.
func (s *Service) Upload(ctx context.Context, tenant, claimID, fileName, mime string, content []byte) (documents.Document, bool, error) {
	t := strings.TrimSpace(tenant)
	if t == "" {
		return documents.Document{}, false, validationError("blank tenant")
	}
	c := strings.TrimSpace(claimID)
	if c == "" {
		return documents.Document{}, false, validationError("blank claim id")
	}
	f := strings.TrimSpace(fileName)
	if f == "" {
		return documents.Document{}, false, validationError("blank file name")
	}
	if len(content) == 0 {
		return documents.Document{}, false, fmt.Errorf("%w: %w", errValidation, ErrEmptyDocument)
	}
	m := strings.TrimSpace(mime)

	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	docID := newID(sha)
	docType, _ := documents.Classify(f, m)
	doc := documents.Document{
		ID:        docID,
		Tenant:    claims.TenantID(t),
		ClaimID:   claims.ClaimID(c),
		Type:      docType,
		FileName:  f,
		MIME:      m,
		SHA256:    sha,
		SizeBytes: int64(len(content)),
		Status:    documents.StReceived,
	}

	key := ports.DocumentObjectKey(t, c, docID)
	ref := ports.ObjectRef{Key: key, SHA256: sha, MIME: m, SizeBytes: int64(len(content))}
	if err := s.Blobs.Put(ctx, ref, bytes.NewReader(content)); err != nil {
		return documents.Document{}, false, fmt.Errorf("ingest: stage blob: %w", err)
	}

	tctx := postgres.WithTenant(ctx, claims.TenantID(t))
	tx, err := postgres.BeginTenantTx(tctx, s.Pool)
	if err != nil {
		_ = s.Blobs.Delete(ctx, ref)
		return documents.Document{}, false, fmt.Errorf("ingest: begin tx: %w", err)
	}
	inserted, err := s.Repo.InsertDocument(tctx, tx, doc)
	if err != nil {
		_ = tx.Rollback(tctx)
		_ = s.Blobs.Delete(ctx, ref)
		return documents.Document{}, false, fmt.Errorf("ingest: insert document: %w", err)
	}
	if !inserted {
		_ = tx.Rollback(tctx)
		_ = s.Blobs.Delete(ctx, ref)
		return documents.Document{}, false, nil
	}
	payload, err := json.Marshal(BuildUploadPayload(doc, time.Now().UTC()))
	if err != nil {
		_ = tx.Rollback(tctx)
		_ = s.Blobs.Delete(ctx, ref)
		return documents.Document{}, false, fmt.Errorf("ingest: marshal event: %w", err)
	}
	// EventType/EventVersion split the schema_version contract marker
	// "document-ingested.v1": topic name on the left, version on the right.
	outboxErr := s.Repo.AppendOutbox(tctx, tx, postgres.OutboxEvent{
		EventID:       "evt-" + docID,
		Tenant:        claims.TenantID(t),
		AggregateType: "document",
		AggregateID:   docID,
		EventType:     ports.TopicDocumentIngested,
		EventVersion:  "v1",
		Payload:       payload,
		OccurredAt:    time.Now().UTC(),
	})
	if outboxErr != nil {
		_ = tx.Rollback(tctx)
		_ = s.Blobs.Delete(ctx, ref)
		return documents.Document{}, false, fmt.Errorf("ingest: append outbox: %w", outboxErr)
	}
	if err := tx.Commit(tctx); err != nil {
		_ = s.Blobs.Delete(ctx, ref)
		return documents.Document{}, false, fmt.Errorf("ingest: commit: %w", err)
	}
	return doc, true, nil
}
