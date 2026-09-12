// Evidence pinner for Chunk B (issue #54): insert-or-get over the
// evidence table. Tools program to the Pinner seam
// (Pin of canonical bytes -> stable row ID); PGPinner is the pgxpool
// implementation. Tests inject fakes; the tools package mirrors this
// interface locally per file (see tools/policy.go) so each tool file
// compiles independently — the signatures are identical by design.
//
// Keying: the evidence UNIQUE is (tenant_id, claim_id, source_type,
// source_id). Pin inserts under a deterministic ev- ID derived from that
// 4-tuple (PinEvidenceID) with content_hash = sha256 of the canonical
// bytes (PinContentHash), then SELECTs the row. ON CONFLICT DO NOTHING +
// SELECT makes the first writer win: a re-pin with different bytes for an
// existing key returns the ORIGINAL row (original ID and original hash),
// never overwrites (the table is append-only for the service role).
// Tenancy mirrors readers.go: validate echo, WithTenant, BeginTenantTx.
// Logging: none.
package investigate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pinner pins one canonical payload as an evidence row and returns its
// stable row ID. Re-pinning an existing key returns the stored row
// (first-wins); implementations never update.
type Pinner interface {
	Pin(ctx context.Context, tenant, claim, sourceType, sourceID string, canonicalBytes []byte) (evidenceID string, err error)
}

// pinIDDomain separates evidence IDs from every other ev- mint
// (evidence.newID is random; investigation envelopes use ex-/inv-).
const pinIDDomain = "claimops-evidence-v1"

// PinEvidenceID derives the deterministic row ID for an evidence key:
// "ev-" + first 32 hex chars of sha256 over the domain-separated 4-tuple.
// Same key, same ID, every process, no coordination.
func PinEvidenceID(tenant, claim, sourceType, sourceID string) string {
	sum := sha256.Sum256([]byte(pinIDDomain + "\x00" + tenant + "\x00" + claim + "\x00" + sourceType + "\x00" + sourceID))
	return "ev-" + hex.EncodeToString(sum[:])[:32]
}

// PinContentHash is the sha256 hex of the canonical bytes (the stored
// content_hash). It is a pure function of bytes only: same bytes, same
// hash, regardless of key.
func PinContentHash(canonicalBytes []byte) string {
	sum := sha256.Sum256(canonicalBytes)
	return hex.EncodeToString(sum[:])
}

// checkPinKey validates the 4-tuple echo (fail closed, ErrContract).
func checkPinKey(tenant, claim, sourceType, sourceID string) error {
	if err := claims.TenantID(tenant).Validate(); err != nil {
		return fmt.Errorf("investigate: pin: %v: %w", err, ErrContract)
	}
	if err := claims.ClaimID(claim).Validate(); err != nil {
		return fmt.Errorf("investigate: pin: %v: %w", err, ErrContract)
	}
	if strings.TrimSpace(sourceType) == "" || sourceType != strings.TrimSpace(sourceType) {
		return fmt.Errorf("investigate: pin needs a trimmed source type: %w", ErrContract)
	}
	switch invest.EvidenceSourceType(sourceType) {
	case invest.EvidenceSourceDocument, invest.EvidenceSourceField,
		invest.EvidenceSourcePolicy, invest.EvidenceSourceTPA,
		invest.EvidenceSourceProvider, invest.EvidenceSourceRisk:
	default:
		return fmt.Errorf("investigate: pin unknown source type %q: %w", sourceType, ErrContract)
	}
	if strings.TrimSpace(sourceID) == "" || sourceID != strings.TrimSpace(sourceID) {
		return fmt.Errorf("investigate: pin needs a trimmed source id: %w", ErrContract)
	}
	return nil
}

// PGPinner is the pgxpool-backed Pinner. The zero value is unusable (nil
// pool fails closed); build with NewPGPinner.
type PGPinner struct {
	pool *pgxpool.Pool
}

// NewPGPinner backs the pin seam with pool. A nil pool is accepted here
// and rejected per call (fail closed).
func NewPGPinner(pool *pgxpool.Pool) *PGPinner {
	return &PGPinner{pool: pool}
}

// Pin inserts the canonical payload as an evidence row, or returns the
// stored row when the key already exists (first-wins: original ID and
// original content hash win; the loser's bytes are discarded, never
// stored). Empty canonical bytes fail closed — every tool canonicalizes
// to non-empty JSON before pinning.
func (p *PGPinner) Pin(ctx context.Context, tenant, claim, sourceType, sourceID string, canonicalBytes []byte) (string, error) {
	if err := checkPinKey(tenant, claim, sourceType, sourceID); err != nil {
		return "", err
	}
	if len(canonicalBytes) == 0 {
		return "", fmt.Errorf("investigate: pin needs non-empty canonical bytes: %w", ErrContract)
	}
	if p == nil || p.pool == nil {
		return "", fmt.Errorf("investigate: pin needs a pool: %w", ErrContract)
	}
	id := PinEvidenceID(tenant, claim, sourceType, sourceID)
	hash := PinContentHash(canonicalBytes)

	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, p.pool)
	if err != nil {
		return "", fmt.Errorf("investigate: pin begin: %w", ErrUpstream)
	}
	defer func() { _ = tx.Rollback(tctx) }()

	if _, err := tx.Exec(tctx, `
INSERT INTO evidence
	(id, tenant_id, claim_id, source_type, source_id, retrieved_at, content_hash, status)
VALUES
	($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT DO NOTHING`,
		id, tenant, claim, sourceType, sourceID, time.Now().UTC(), hash, "CAPTURED",
	); err != nil {
		_ = tx.Rollback(tctx)
		return "", fmt.Errorf("investigate: pin insert: %w", ErrUpstream)
	}
	var storedID string
	if err := tx.QueryRow(tctx, `
SELECT id FROM evidence
 WHERE tenant_id = $1 AND claim_id = $2 AND source_type = $3 AND source_id = $4`,
		tenant, claim, sourceType, sourceID,
	).Scan(&storedID); err != nil {
		_ = tx.Rollback(tctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("investigate: pin lost its row (invariant): %w", ErrUpstream)
		}
		return "", fmt.Errorf("investigate: pin select: %w", ErrUpstream)
	}
	if err := tx.Commit(tctx); err != nil {
		return "", fmt.Errorf("investigate: pin commit: %w", ErrUpstream)
	}
	return storedID, nil
}
