package postgres

import (
	"context"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"

	"github.com/jackc/pgx/v5"
)

// InsertDocument inserts d inside the caller's transaction tx (started via
// BeginTenantTx, which scopes row-level security to the ctx tenant).
//
// Status is stored exactly as given: the documents table is append-only for
// the service role (SELECT + INSERT only, no UPDATE — see migration 004),
// so status transitions create new rows rather than mutating this one.
//
// Dedupe is by the (tenant_id, claim_id, sha256) unique constraint:
//
//	INSERT ... ON CONFLICT (tenant_id, claim_id, sha256) DO NOTHING
//
// so a redelivered upload appends nothing. The bool reports whether the
// row was actually inserted (tag.RowsAffected() == 1); a conflict reports
// (false, nil), never an error, so no SAVEPOINT juggling is needed.
//
// storage_uri is a legacy NOT NULL column from 001_init.sql with no domain
// counterpart on documents.Document; it is written as ” to satisfy the
// constraint without inventing a URI.
func (r *Repository) InsertDocument(ctx context.Context, tx pgx.Tx, d documents.Document) (bool, error) {
	tag, err := tx.Exec(ctx, `
INSERT INTO documents
	(id, tenant_id, claim_id, type, file_name, mime, sha256, size_bytes, status, storage_uri)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (tenant_id, claim_id, sha256) DO NOTHING`,
		d.ID,
		string(d.Tenant),
		string(d.ClaimID),
		string(d.Type),
		d.FileName,
		d.MIME,
		d.SHA256,
		d.SizeBytes,
		d.Status,
		"",
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ListDocumentsByClaim returns every document for claimID inside the
// caller's transaction tx. Row-level security scopes the read to the
// transaction tenant set by BeginTenantTx, so another tenant's ClaimID is
// simply absent (zero rows, nil error). Rows come back in created_at
// order (id breaks ties from same-instant inserts).
func (r *Repository) ListDocumentsByClaim(ctx context.Context, tx pgx.Tx, claimID claims.ClaimID) ([]documents.Document, error) {
	rows, err := tx.Query(ctx, `
SELECT id, tenant_id, claim_id, type, file_name, mime, sha256, size_bytes, status
  FROM documents
 WHERE claim_id = $1
 ORDER BY created_at ASC, id ASC`,
		string(claimID),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []documents.Document
	for rows.Next() {
		var (
			id        string
			tenantID  string
			claimIDSc string
			docType   string
			fileName  string
			mime      string
			sha256    string
			sizeBytes int64
			status    string
		)
		if err := rows.Scan(
			&id,
			&tenantID,
			&claimIDSc,
			&docType,
			&fileName,
			&mime,
			&sha256,
			&sizeBytes,
			&status,
		); err != nil {
			return nil, err
		}
		out = append(out, documents.Document{
			ID:        id,
			Tenant:    claims.TenantID(tenantID),
			ClaimID:   claims.ClaimID(claimIDSc),
			Type:      documents.DocType(docType),
			FileName:  fileName,
			MIME:      mime,
			SHA256:    sha256,
			SizeBytes: sizeBytes,
			Status:    status,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// InsertFieldEvidence inserts e inside the caller's transaction tx
// (started via BeginTenantTx, which scopes row-level security to the ctx
// tenant).
//
// The table is append-only for the service role (SELECT + INSERT only, no
// UPDATE — see migration 004), and dedupe is by the
// (tenant_id, claim_id, document_id, field) unique constraint:
//
//	INSERT ... ON CONFLICT (tenant_id, claim_id, document_id, field) DO NOTHING
//
// so replayed extraction appends nothing. The bool reports whether the row
// was actually inserted (tag.RowsAffected() == 1); a conflict reports
// (false, nil), never an error.
//
// Column note: field_evidence carries no doc_type column in 004, so
// DocType is not a physical column here — it rides on the parent
// documents.type row (the worker always builds evidence with
// e.DocType == parent doc type). Every physical column the table does
// have (anchor, extractor, page, confidence, field, value) is persisted
// explicitly; DocType round-trips on read via the JOIN in
// ListEvidenceByClaim.
func (r *Repository) InsertFieldEvidence(ctx context.Context, tx pgx.Tx, e evidence.FieldEvidence) (bool, error) {
	tag, err := tx.Exec(ctx, `
INSERT INTO field_evidence
	(id, tenant_id, claim_id, document_id, field, value, page, anchor, confidence, extractor)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (tenant_id, claim_id, document_id, field) DO NOTHING`,
		e.ID,
		string(e.Tenant),
		string(e.ClaimID),
		e.DocumentID,
		e.Field,
		e.Value,
		e.Page,
		e.Anchor,
		e.Confidence,
		e.Extractor,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ListEvidenceByClaim returns every field-evidence row for claimID inside
// the caller's transaction tx. Row-level security scopes the read to the
// transaction tenant set by BeginTenantTx, so another tenant's ClaimID is
// simply absent (zero rows, nil error). Rows come back in created_at
// order (id breaks ties from same-instant inserts).
//
// DocType has no physical column on field_evidence (see InsertFieldEvidence),
// so it is restored from the parent documents.type via an inner join —
// both tables are FORCE-RLS on the same tenant, so the join never leaks
// across tenants, and the FK guarantees the parent row exists.
func (r *Repository) ListEvidenceByClaim(ctx context.Context, tx pgx.Tx, claimID claims.ClaimID) ([]evidence.FieldEvidence, error) {
	rows, err := tx.Query(ctx, `
SELECT fe.id, fe.tenant_id, fe.claim_id, fe.document_id, fe.field, fe.value,
       fe.page, fe.anchor, fe.confidence, fe.extractor, d.type
  FROM field_evidence fe
  JOIN documents d ON d.id = fe.document_id
 WHERE fe.claim_id = $1
 ORDER BY fe.created_at ASC, fe.id ASC`,
		string(claimID),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []evidence.FieldEvidence
	for rows.Next() {
		var (
			id         string
			tenantID   string
			claimIDSc  string
			documentID string
			field      string
			value      string
			page       int
			anchor     string
			confidence float64
			extractor  string
			docType    string
		)
		if err := rows.Scan(
			&id,
			&tenantID,
			&claimIDSc,
			&documentID,
			&field,
			&value,
			&page,
			&anchor,
			&confidence,
			&extractor,
			&docType,
		); err != nil {
			return nil, err
		}
		out = append(out, evidence.FieldEvidence{
			ID:         id,
			Tenant:     claims.TenantID(tenantID),
			ClaimID:    claims.ClaimID(claimIDSc),
			DocumentID: documentID,
			Field:      field,
			Value:      value,
			Anchor:     anchor,
			Extractor:  extractor,
			DocType:    docType,
			Page:       page,
			Confidence: confidence,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
