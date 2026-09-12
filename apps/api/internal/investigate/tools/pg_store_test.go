package tools

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPGStoreNilPoolFailsClosed pins the zero-value guard.
func TestPGStoreNilPoolFailsClosed(t *testing.T) {
	s := NewPGReportStore(nil)
	row := ReportRow{ID: "inv-0123456789abcdef0123456789abcdef",
		TenantID: "t1", ClaimID: "c1", ContentHash: "abc"}
	if _, err := s.Insert(context.Background(), row); err == nil {
		t.Fatal("nil pool must fail closed")
	}
	var nilStore *PGReportStore
	if _, err := nilStore.Insert(context.Background(), row); err == nil {
		t.Fatal("nil store must fail closed")
	}
}

// TestPGStoreRejectsBadRow pins shape validation without a DB.
func TestPGStoreRejectsBadRow(t *testing.T) {
	base := ReportRow{ID: "inv-0123456789abcdef0123456789abcdef",
		TenantID: "t1", ClaimID: "c1", ContentHash: "abc"}
	cases := map[string]ReportRow{
		"blank tenant": {ID: base.ID, ClaimID: "c1", ContentHash: "abc"},
		"id mismatch":  {ID: "inv-ffffffffffffffffffffffffffffffff", InvestigationID: base.ID, TenantID: "t1", ClaimID: "c1", ContentHash: "abc"},
		"blank hash":   {ID: base.ID, InvestigationID: base.ID, TenantID: "t1", ClaimID: "c1"},
	}
	for name, row := range cases {
		if err := checkReportRow(row); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

// liveToolPool connects to live PG or skips when unreachable.
func liveToolPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN unset, skipping live postgres test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("postgres unavailable (dial): %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("postgres unavailable (ping): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPGStoreRejectsHashMismatch pins commitment: a row whose hash does
// not match its content fails closed without touching the DB (non-nil
// zero pool: hash check precedes any pool use).
func TestPGStoreRejectsHashMismatch(t *testing.T) {
	s := NewPGReportStore(&pgxpool.Pool{})
	row := ReportRow{ID: "inv-0123456789abcdef0123456789abcdef",
		InvestigationID: "inv-0123456789abcdef0123456789abcdef",
		TenantID:        "t1", ClaimID: "c1", ContentHash: "deadbeef",
		Hypotheses: nil, Findings: nil, Recommendations: nil, MissingEvidence: nil,
	}
	// checkReportRow passes shape; the store's re-marshal must reject.
	if err := checkReportRow(row); err != nil {
		t.Fatalf("shape: %v", err)
	}
	_, err := s.Insert(context.Background(), row)
	if err == nil {
		t.Fatal("hash mismatch must fail closed")
	}
}

// TestPGStoreWriteOnceLive drives the real tool path (NewReportTool with
// PGReportStore) twice: first insert stores (replayed=false), second
// replays (replayed=true, same hash).
func TestPGStoreWriteOnceLive(t *testing.T) {
	pool := liveToolPool(t)
	tenant := "pgstore-tnt"
	claim := "pgstore-clm-01"
	seedToolClaim(t, pool, tenant, claim)

	// Unique investigation ID per run: write-once rows persist, so a
	// fixed ID would replay (not store) on re-runs.
	invID := fmt.Sprintf("inv-%032x", uint64(os.Getpid())<<32|uint64(time.Now().UnixNano()&0xffffffff))
	exc := validReportException()
	exc.TenantID = tenant
	exc.ClaimID = claim
	exc.InvestigationID = invID
	exc.Scope.TenantID = tenant
	exc.Scope.ClaimID = claim
	for i := range exc.EvidenceRefs {
		exc.EvidenceRefs[i].TenantID = tenant
		exc.EvidenceRefs[i].ClaimID = claim
	}
	req := validReportRequest(exc)
	req.TenantID = tenant
	req.ClaimID = claim
	req.InvestigationID = invID
	resolve := staticResolver(map[string][2]string{
		"ev-01": {tenant, claim},
	})

	tool := NewReportTool(NewPGReportStore(pool), resolve)
	ctx := context.Background()
	first, err := tool(ctx, req)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if first.Replayed {
		t.Fatal("first insert must have replayed=false")
	}
	if first.ContentHash == "" {
		t.Fatal("ContentHash must be set")
	}
	second, err := tool(ctx, req)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if !second.Replayed {
		t.Fatal("second insert must have replayed=true")
	}
	if second.ContentHash != first.ContentHash || second.ReportID != first.ReportID {
		t.Fatal("replay must return stored identity")
	}
}

// seedToolClaim inserts a parent claim row for FK-bound live tests
// (mirrors the investigate-package helper; raw INSERT keeps this
// package independent of claims-constructor drift).
func seedToolClaim(t *testing.T, pool *pgxpool.Pool, tenant, claim string) {
	t.Helper()
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(tctx)
	if _, err := tx.Exec(tctx, `INSERT INTO claims (id, tenant_id, policy_id, reference, amount_paise, status, version) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (id) DO NOTHING`,
		claim, tenant, "pol-"+claim, "ref-"+claim, 10000, "RECEIVED", 1); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}
}
