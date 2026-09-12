package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"claimops-api/internal/claims"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGReportStore is the Postgres-backed Store for T11: write-once insert
// into investigation_reports with hash verification. Duplicate inserts
// for the same ID return the stored identity with replayed=true.
type PGReportStore struct {
	pool *pgxpool.Pool
}

// NewPGReportStore backs the Store seam with pool. A nil pool fails
// closed per call.
func NewPGReportStore(pool *pgxpool.Pool) *PGReportStore {
	return &PGReportStore{pool: pool}
}

// Insert implements Store: verifies ContentHash against a re-marshal of
// the row's canonical content (fail closed on mismatch — never store
// bytes whose hash the tool did not commit to), then inserts write-once.
// ON CONFLICT on the PK returns the stored row with replayed=true.
func (s *PGReportStore) Insert(ctx context.Context, row ReportRow) (bool, error) {
	if s == nil || s.pool == nil {
		return false, fmt.Errorf("report: store needs a pool: %w", ports.ErrContract)
	}
	if err := checkReportRow(row); err != nil {
		return false, err
	}
	canonical, err := json.Marshal(canonicalReport{
		TenantID:        row.TenantID,
		ClaimID:         row.ClaimID,
		ExceptionID:     row.ExceptionID,
		InvestigationID: row.InvestigationID,
		Hypotheses:      row.Hypotheses,
		Findings:        row.Findings,
		Recommendations: row.Recommendations,
		MissingEvidence: row.MissingEvidence,
	})
	if err != nil {
		return false, fmt.Errorf("%w: report: store canonicalize: %v", ports.ErrContract, err)
	}
	sum := sha256.Sum256(canonical)
	if got := hex.EncodeToString(sum[:]); got != row.ContentHash {
		return false, fmt.Errorf("%w: report: content hash mismatch", ports.ErrContract)
	}
	tctx := postgres.WithTenant(ctx, claims.TenantID(row.TenantID))
	tx, err := postgres.BeginTenantTx(tctx, s.pool)
	if err != nil {
		return false, fmt.Errorf("report: store begin: %w", ports.ErrUpstream)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	var storedID, storedHash string
	var storedVersion int
	err = tx.QueryRow(tctx, `
INSERT INTO investigation_reports
	(id, tenant_id, claim_id, exception_id, report, report_hash, version)
VALUES
	($1, $2, $3, $4, $5, $6, 1)
ON CONFLICT (id) DO NOTHING
RETURNING id, report_hash, version`,
		row.ID, row.TenantID, row.ClaimID, row.ExceptionID, string(canonical), row.ContentHash,
	).Scan(&storedID, &storedHash, &storedVersion)
	if err != nil {
		// ON CONFLICT DO NOTHING yields zero rows → replay path.
		if errors.Is(err, pgx.ErrNoRows) {
			if cerr := tx.Commit(tctx); cerr != nil {
				return false, fmt.Errorf("report: store commit: %w", ports.ErrUpstream)
			}
			return true, nil
		}
		return false, fmt.Errorf("report: store insert: %w", ports.ErrUpstream)
	}
	if err := tx.Commit(tctx); err != nil {
		return false, fmt.Errorf("report: store commit: %w", ports.ErrUpstream)
	}
	if storedHash != row.ContentHash {
		return false, fmt.Errorf("%w: report: stored hash differs", ports.ErrContract)
	}
	return false, nil
}

// checkReportRow validates the row shape before any DB touch.
func checkReportRow(row ReportRow) error {
	if err := claims.TenantID(row.TenantID).Validate(); err != nil {
		return fmt.Errorf("report: %v: %w", err, ports.ErrContract)
	}
	if err := claims.ClaimID(row.ClaimID).Validate(); err != nil {
		return fmt.Errorf("report: %v: %w", err, ports.ErrContract)
	}
	if row.ID == "" || row.ID != row.InvestigationID {
		return fmt.Errorf("%w: report: ID must equal InvestigationID", ports.ErrContract)
	}
	if row.ContentHash == "" {
		return fmt.Errorf("%w: report: blank content hash", ports.ErrContract)
	}
	return nil
}
