// Reader seam for Chunk B (issue #54): tenant-scoped PostgreSQL reads
// behind narrow interfaces. Tools program to ClaimReader,
// DocumentMetaReader, and EvidenceReader; PGReaders is the pgxpool
// implementation. Production wires *PGReaders (structurally satisfying the
// tools' constructors); tests inject fakes.
//
// Tenancy: every method validates its tenant/claim echo, scopes ctx via
// postgres.WithTenant, and opens a postgres.BeginTenantTx so RLS sees the
// acting tenant on a transaction-local GUC (never a session SET on a pooled
// conn). Cross-tenant rows are simply absent (RLS); a row whose stored
// tenant differs from the request is a loud ErrTenantMismatch.
//
// Paging: cursors are opaque last-seen row IDs externally ("" opens the
// first page). Internally they resolve to a (created_at|retrieved_at, id)
// tuple for stable keyset paging. Unknown cursors fail closed
// (ErrContract). Every list fetches limit+1 rows to compute Truncated and
// the next cursor; limits clamp to the tool.go MaxRows caps.
//
// SearchEvidence is lexical only (MVP): ILIKE over field_evidence field,
// value, and anchor with a claim predicate, LIKE metacharacters escaped,
// SET LOCAL statement_timeout 3s, ORDER BY rank, id. Snippets are truncated
// rune-aware to MaxSnippetRunes HERE at the reader; the T5 tool carries
// them no further than its own response and the generic envelope drops
// them by construction (IDs only).
//
// This file NEVER touches *postgres.Repository or any BlobStore: all SQL
// is local to these methods and projects metadata/ID/hash columns only —
// never document bytes, values beyond the snippet window, or floats
// beyond confidence-free shapes. Logging: none.
package investigate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxSnippetRunes caps T5 snippets at the reader (rune-aware cut, no
// marker appended). The tool layer carries snippets verbatim and never
// expands them.
const MaxSnippetRunes = 200

// readerStatementTimeout bounds the T5 lexical scan per attempt
// (ADR-004 3s precedent, applied via SET LOCAL inside the tenant tx).
const readerStatementTimeout = "3s"

// DocumentPage is one document-metadata page. Documents reuse the
// tool.go DocumentMeta shape (metadata only, never bytes). NextCursor is
// the opaque last-seen document ID ("" when the listing is exhausted).
type DocumentPage struct {
	Documents  []DocumentMeta
	Truncated  bool
	NextCursor string
}

// EvidencePage is one evidence page over the evidence table. NextCursor
// is the opaque last-seen evidence ID ("" when exhausted).
type EvidencePage struct {
	Rows       []EvidenceRow
	Truncated  bool
	NextCursor string
}

// EvidenceHit is one T5 lexical hit: locators plus a reader-truncated
// snippet. ContentHash is empty for field rows (field_evidence carries no
// hash column); SourceType is always "field" and SourceID the parent
// document ID.
type EvidenceHit struct {
	EvidenceID  string
	SourceType  string
	SourceID    string
	ContentHash string
	FieldKey    string
	Anchor      string
	Snippet     string
}

// ClaimReader loads one claim header projection. Absent rows report
// ErrNotFound (RLS-scoped: another tenant's claim is simply absent).
type ClaimReader interface {
	LoadClaim(ctx context.Context, tenantID, claimID string) (ClaimHeader, error)
}

// DocumentMetaReader pages document metadata for one claim (never bytes).
// limit <= 0 selects MaxRowsGetDocuments; above-cap clamps. cursor is the
// opaque last-seen document ID ("" for the first page).
type DocumentMetaReader interface {
	ListDocuments(ctx context.Context, tenantID, claimID string, limit int, cursor string) (DocumentPage, error)
}

// EvidenceReader pages evidence rows (T4) and runs lexical search (T5).
// ListEvidence filters by source type ("" = all; otherwise one of the six
// invest source types). SearchEvidence matches query against
// field/value/anchor metadata; snippets arrive truncated to
// MaxSnippetRunes.
type EvidenceReader interface {
	ListEvidence(ctx context.Context, tenantID, claimID string, limit int, cursor, sourceType string) (EvidencePage, error)
	SearchEvidence(ctx context.Context, tenantID, claimID, query string, limit int) ([]EvidenceHit, error)
}

// PGReaders is the pgxpool-backed reader implementation. The zero value
// is unusable (nil pool fails closed); build with NewPGReaders.
type PGReaders struct {
	pool *pgxpool.Pool
}

// NewPGReaders backs the reader seam with pool. A nil pool is accepted
// here and rejected per call (fail closed) so wiring mistakes surface at
// first use, never as a nil dereference.
func NewPGReaders(pool *pgxpool.Pool) *PGReaders {
	return &PGReaders{pool: pool}
}

// scopedTx validates the tenant/claim echo, scopes ctx to the tenant, and
// opens the tenant transaction. Validation failures wrap ErrContract;
// transaction failures wrap ErrUpstream (transient: reconnect, retry).
func (r *PGReaders) scopedTx(ctx context.Context, tenantID, claimID string) (context.Context, pgx.Tx, error) {
	if r == nil || r.pool == nil {
		return nil, nil, fmt.Errorf("investigate: readers need a pool: %w", ErrContract)
	}
	if err := claims.TenantID(tenantID).Validate(); err != nil {
		return nil, nil, fmt.Errorf("investigate: readers: %v: %w", err, ErrContract)
	}
	if err := claims.ClaimID(claimID).Validate(); err != nil {
		return nil, nil, fmt.Errorf("investigate: readers: %v: %w", err, ErrContract)
	}
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenantID))
	tx, err := postgres.BeginTenantTx(tctx, r.pool)
	if err != nil {
		return nil, nil, fmt.Errorf("investigate: readers begin: %w", ErrUpstream)
	}
	return tctx, tx, nil
}

// finish commits on success; the deferred rollback covers every failure
// path (rollback after commit is a harmless no-op).
func finish(ctx context.Context, tx pgx.Tx, callErr error) error {
	if callErr != nil {
		_ = tx.Rollback(ctx)
		return callErr
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("investigate: readers commit: %w", ErrUpstream)
	}
	return nil
}

// LoadClaim reads one claim header projection: exactly the safe columns
// (id, reference, amount, status, version). No patient/hospital columns
// exist in this schema, and none are selected here, so they cannot cross
// this boundary even if the schema later gains them.
func (r *PGReaders) LoadClaim(ctx context.Context, tenantID, claimID string) (ClaimHeader, error) {
	tctx, tx, err := r.scopedTx(ctx, tenantID, claimID)
	if err != nil {
		return ClaimHeader{}, err
	}
	defer func() { _ = tx.Rollback(tctx) }()

	var (
		id        string
		rowTenant string
		reference string
		amount    int64
		status    string
		version   int
	)
	qerr := tx.QueryRow(tctx, `
SELECT id, tenant_id, reference, amount_paise, status, version
  FROM claims
 WHERE id = $1`,
		claimID,
	).Scan(&id, &rowTenant, &reference, &amount, &status, &version)
	if qerr != nil {
		if errors.Is(qerr, pgx.ErrNoRows) {
			_ = tx.Rollback(tctx)
			return ClaimHeader{}, fmt.Errorf("investigate: claim %q absent: %w", claimID, ErrNotFound)
		}
		_ = tx.Rollback(tctx)
		return ClaimHeader{}, fmt.Errorf("investigate: load claim: %w", ErrUpstream)
	}
	if rowTenant != tenantID {
		_ = tx.Rollback(tctx)
		return ClaimHeader{}, fmt.Errorf("investigate: claim row tenant %q != scope tenant %q: %w", rowTenant, tenantID, ErrTenantMismatch)
	}
	if err := tx.Commit(tctx); err != nil {
		return ClaimHeader{}, fmt.Errorf("investigate: readers commit: %w", ErrUpstream)
	}
	return ClaimHeader{
		ClaimID:     id,
		Status:      status,
		Reference:   reference,
		AmountPaise: amount,
		Version:     version,
	}, nil
}

// resolveDocCursor maps an opaque cursor to its (created_at, id) paging
// tuple. Unknown cursors fail closed; the lookup is claim-pinned so a
// cursor minted for another claim never pages this one.
func resolveDocCursor(ctx context.Context, tx pgx.Tx, claimID, cursor string) (any, string, error) {
	var createdAt any
	var id string
	err := tx.QueryRow(ctx, `
SELECT created_at, id FROM documents WHERE id = $1 AND claim_id = $2`,
		cursor, claimID,
	).Scan(&createdAt, &id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", fmt.Errorf("investigate: unknown documents cursor: %w", ErrContract)
		}
		return nil, "", fmt.Errorf("investigate: resolve documents cursor: %w", ErrUpstream)
	}
	return createdAt, id, nil
}

// ListDocuments pages document metadata (id, type, sha256, status only)
// in (created_at, id) order. Tuple paging internally, opaque last-seen ID
// externally.
func (r *PGReaders) ListDocuments(ctx context.Context, tenantID, claimID string, limit int, cursor string) (DocumentPage, error) {
	if limit <= 0 {
		limit = MaxRowsGetDocuments
	}
	if limit > MaxRowsGetDocuments {
		limit = MaxRowsGetDocuments
	}
	if cursor != "" && cursor != strings.TrimSpace(cursor) {
		return DocumentPage{}, fmt.Errorf("investigate: documents cursor must be trimmed: %w", ErrContract)
	}
	tctx, tx, err := r.scopedTx(ctx, tenantID, claimID)
	if err != nil {
		return DocumentPage{}, err
	}
	defer func() { _ = tx.Rollback(tctx) }()

	var rows pgx.Rows
	if cursor == "" {
		rows, err = tx.Query(tctx, `
SELECT id, type, sha256, status
  FROM documents
 WHERE claim_id = $1
 ORDER BY created_at ASC, id ASC
 LIMIT $2`,
			claimID, limit+1,
		)
	} else {
		anchor, anchorID, rerr := resolveDocCursor(tctx, tx, claimID, cursor)
		if rerr != nil {
			_ = tx.Rollback(tctx)
			return DocumentPage{}, rerr
		}
		rows, err = tx.Query(tctx, `
SELECT id, type, sha256, status
  FROM documents
 WHERE claim_id = $1
   AND (created_at, id) > ($2, $3)
 ORDER BY created_at ASC, id ASC
 LIMIT $4`,
			claimID, anchor, anchorID, limit+1,
		)
	}
	if err != nil {
		_ = tx.Rollback(tctx)
		return DocumentPage{}, fmt.Errorf("investigate: list documents: %w", ErrUpstream)
	}
	defer rows.Close()

	var docs []DocumentMeta
	for rows.Next() {
		var m DocumentMeta
		if serr := rows.Scan(&m.DocumentID, &m.DocType, &m.SHA256, &m.Status); serr != nil {
			_ = tx.Rollback(tctx)
			return DocumentPage{}, fmt.Errorf("investigate: scan document: %w", ErrUpstream)
		}
		docs = append(docs, m)
	}
	if rerr := rows.Err(); rerr != nil {
		_ = tx.Rollback(tctx)
		return DocumentPage{}, fmt.Errorf("investigate: list documents: %w", ErrUpstream)
	}
	page := DocumentPage{Documents: docs}
	if len(docs) > limit {
		page.Documents = docs[:limit]
		page.Truncated = true
		page.NextCursor = docs[limit-1].DocumentID
	}
	if page.Documents == nil {
		page.Documents = []DocumentMeta{}
	}
	if err := tx.Commit(tctx); err != nil {
		return DocumentPage{}, fmt.Errorf("investigate: readers commit: %w", ErrUpstream)
	}
	return page, nil
}

// checkEvidenceSource validates the T4 filter ("" = all; otherwise one
// of the six closed source types).
func checkEvidenceSource(sourceType string) error {
	if sourceType == "" {
		return nil
	}
	switch invest.EvidenceSourceType(sourceType) {
	case invest.EvidenceSourceDocument, invest.EvidenceSourceField,
		invest.EvidenceSourcePolicy, invest.EvidenceSourceTPA,
		invest.EvidenceSourceProvider, invest.EvidenceSourceRisk:
		return nil
	default:
		return fmt.Errorf("investigate: unknown evidence source type %q: %w", sourceType, ErrContract)
	}
}

// resolveEvidenceCursor maps an opaque cursor to its (retrieved_at, id)
// tuple, pinned to the claim AND the active source filter so cursors never
// cross listings.
func resolveEvidenceCursor(ctx context.Context, tx pgx.Tx, claimID, cursor, sourceType string) (any, string, error) {
	var at any
	var id string
	var err error
	if sourceType == "" {
		err = tx.QueryRow(ctx, `
SELECT retrieved_at, id FROM evidence WHERE id = $1 AND claim_id = $2`,
			cursor, claimID,
		).Scan(&at, &id)
	} else {
		err = tx.QueryRow(ctx, `
SELECT retrieved_at, id FROM evidence WHERE id = $1 AND claim_id = $2 AND source_type = $3`,
			cursor, claimID, sourceType,
		).Scan(&at, &id)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", fmt.Errorf("investigate: unknown evidence cursor: %w", ErrContract)
		}
		return nil, "", fmt.Errorf("investigate: resolve evidence cursor: %w", ErrUpstream)
	}
	return at, id, nil
}

// ListEvidence pages evidence rows (IDs + hashes + provenance) in
// (retrieved_at, id) order, optionally filtered by source type.
func (r *PGReaders) ListEvidence(ctx context.Context, tenantID, claimID string, limit int, cursor, sourceType string) (EvidencePage, error) {
	if limit <= 0 {
		limit = MaxRowsGetEvidence
	}
	if limit > MaxRowsGetEvidence {
		limit = MaxRowsGetEvidence
	}
	if cursor != "" && cursor != strings.TrimSpace(cursor) {
		return EvidencePage{}, fmt.Errorf("investigate: evidence cursor must be trimmed: %w", ErrContract)
	}
	if err := checkEvidenceSource(sourceType); err != nil {
		return EvidencePage{}, err
	}
	tctx, tx, err := r.scopedTx(ctx, tenantID, claimID)
	if err != nil {
		return EvidencePage{}, err
	}
	defer func() { _ = tx.Rollback(tctx) }()

	var (
		rows pgx.Rows
		qerr error
	)
	switch {
	case cursor == "" && sourceType == "":
		rows, qerr = tx.Query(tctx, `
SELECT id, source_type, source_id, content_hash
  FROM evidence
 WHERE claim_id = $1
 ORDER BY retrieved_at ASC, id ASC
 LIMIT $2`,
			claimID, limit+1,
		)
	case cursor == "" /* filtered first page */ :
		rows, qerr = tx.Query(tctx, `
SELECT id, source_type, source_id, content_hash
  FROM evidence
 WHERE claim_id = $1 AND source_type = $2
 ORDER BY retrieved_at ASC, id ASC
 LIMIT $3`,
			claimID, sourceType, limit+1,
		)
	default:
		var anchor any
		var anchorID string
		anchor, anchorID, qerr = resolveEvidenceCursor(tctx, tx, claimID, cursor, sourceType)
		if qerr != nil {
			_ = tx.Rollback(tctx)
			return EvidencePage{}, qerr
		}
		if sourceType == "" {
			rows, qerr = tx.Query(tctx, `
SELECT id, source_type, source_id, content_hash
  FROM evidence
 WHERE claim_id = $1
   AND (retrieved_at, id) > ($2, $3)
 ORDER BY retrieved_at ASC, id ASC
 LIMIT $4`,
				claimID, anchor, anchorID, limit+1,
			)
		} else {
			rows, qerr = tx.Query(tctx, `
SELECT id, source_type, source_id, content_hash
  FROM evidence
 WHERE claim_id = $1 AND source_type = $2
   AND (retrieved_at, id) > ($3, $4)
 ORDER BY retrieved_at ASC, id ASC
 LIMIT $5`,
				claimID, sourceType, anchor, anchorID, limit+1,
			)
		}
	}
	if qerr != nil {
		_ = tx.Rollback(tctx)
		return EvidencePage{}, fmt.Errorf("investigate: list evidence: %w", ErrUpstream)
	}
	defer rows.Close()

	var out []EvidenceRow
	for rows.Next() {
		var w EvidenceRow
		if serr := rows.Scan(&w.EvidenceID, &w.SourceType, &w.SourceID, &w.ContentHash); serr != nil {
			_ = tx.Rollback(tctx)
			return EvidencePage{}, fmt.Errorf("investigate: scan evidence: %w", ErrUpstream)
		}
		out = append(out, w)
	}
	if rerr := rows.Err(); rerr != nil {
		_ = tx.Rollback(tctx)
		return EvidencePage{}, fmt.Errorf("investigate: list evidence: %w", ErrUpstream)
	}
	page := EvidencePage{Rows: out}
	if len(out) > limit {
		page.Rows = out[:limit]
		page.Truncated = true
		page.NextCursor = out[limit-1].EvidenceID
	}
	if page.Rows == nil {
		page.Rows = []EvidenceRow{}
	}
	if err := tx.Commit(tctx); err != nil {
		return EvidencePage{}, fmt.Errorf("investigate: readers commit: %w", ErrUpstream)
	}
	return page, nil
}

// escapeLike escapes LIKE metacharacters so the query is matched
// literally (ESCAPE '\').
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// truncateSnippet cuts s to MaxSnippetRunes runes (byte-safe, no marker).
func truncateSnippet(s string) string {
	if len([]rune(s)) <= MaxSnippetRunes {
		return s
	}
	return string([]rune(s)[:MaxSnippetRunes])
}

// SearchEvidence runs the T5 lexical scan: ILIKE over field_evidence
// field, value, and anchor with a claim predicate. The scan is bounded by
// SET LOCAL statement_timeout and ordered by a deterministic rank (field
// hit first, then value hit, then anchor-only hit) with id breaking ties.
// Snippets are truncated HERE (see truncateSnippet); callers carry them
// verbatim and never expand them.
func (r *PGReaders) SearchEvidence(ctx context.Context, tenantID, claimID, query string, limit int) ([]EvidenceHit, error) {
	if strings.TrimSpace(query) == "" || query != strings.TrimSpace(query) {
		return nil, fmt.Errorf("investigate: search needs a trimmed non-blank query: %w", ErrContract)
	}
	if len([]rune(query)) > invest.SearchMaxQueryLen {
		return nil, fmt.Errorf("investigate: search query exceeds %d runes: %w", invest.SearchMaxQueryLen, ErrContract)
	}
	if limit <= 0 {
		limit = MaxRowsSearchEvidence
	}
	if limit > MaxRowsSearchEvidence {
		limit = MaxRowsSearchEvidence
	}
	tctx, tx, err := r.scopedTx(ctx, tenantID, claimID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(tctx) }()

	if _, err := tx.Exec(tctx, `SET LOCAL statement_timeout = '`+readerStatementTimeout+`'`); err != nil {
		_ = tx.Rollback(tctx)
		return nil, fmt.Errorf("investigate: search timeout guard: %w", ErrUpstream)
	}
	pattern := "%" + escapeLike(query) + "%"
	rows, err := tx.Query(tctx, `
SELECT fe.id, fe.document_id, fe.field, fe.anchor, fe.value,
       CASE WHEN fe.field ILIKE $2 ESCAPE '\' THEN 0
            WHEN fe.value ILIKE $2 ESCAPE '\' THEN 1
            ELSE 2 END AS rank
  FROM field_evidence fe
 WHERE fe.claim_id = $1
   AND (fe.field ILIKE $2 ESCAPE '\'
        OR fe.value ILIKE $2 ESCAPE '\'
        OR fe.anchor ILIKE $2 ESCAPE '\')
 ORDER BY rank ASC, fe.id ASC
 LIMIT $3`,
		claimID, pattern, limit,
	)
	if err != nil {
		_ = tx.Rollback(tctx)
		return nil, fmt.Errorf("investigate: search evidence: %w", ErrUpstream)
	}
	defer rows.Close()

	var hits []EvidenceHit
	for rows.Next() {
		var (
			id     string
			docID  string
			field  string
			anchor string
			value  string
			rank   int
		)
		if serr := rows.Scan(&id, &docID, &field, &anchor, &value, &rank); serr != nil {
			_ = tx.Rollback(tctx)
			return nil, fmt.Errorf("investigate: scan search hit: %w", ErrUpstream)
		}
		hits = append(hits, EvidenceHit{
			EvidenceID:  id,
			SourceType:  string(invest.EvidenceSourceField),
			SourceID:    docID,
			ContentHash: "",
			FieldKey:    field,
			Anchor:      anchor,
			Snippet:     truncateSnippet(value),
		})
	}
	if rerr := rows.Err(); rerr != nil {
		_ = tx.Rollback(tctx)
		return nil, fmt.Errorf("investigate: search evidence: %w", ErrUpstream)
	}
	if hits == nil {
		hits = []EvidenceHit{}
	}
	if err := tx.Commit(tctx); err != nil {
		return nil, fmt.Errorf("investigate: readers commit: %w", ErrUpstream)
	}
	return hits, nil
}
