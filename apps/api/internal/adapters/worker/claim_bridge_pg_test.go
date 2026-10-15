package workeradapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"
	coreworker "claimops-api/internal/worker"
	"claimops-api/internal/workflow"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgBridgePool dials the live DB as the NON-superuser app role (RLS
// enforced). If the DB is unreachable the test self-skips so unit CI stays
// green; qualify.sh (empty ephemeral DB) makes it actually run.
func pgBridgePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://claimops_app:claimops_app@localhost:5433/claimops"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable (dial): %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unavailable (ping): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func bridgeIDs() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

// seedValidatingClaim inserts a claim at VALIDATING, version 1, plus one
// pre-existing event row at seq 1. The pre-existing event is load-bearing:
// the producer's new event must be seq 2 (derived from MAX(seq)+1), not a
// hard-coded 1.
func seedValidatingClaim(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID, seedEventID string) {
	t.Helper()
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	repo := postgres.New(pool)
	c, err := claims.NewClaim(claimID, tenant, claims.PolicyID("pol-"+string(claimID)), "ref-"+string(claimID),
		claims.MustPaise(100, 0), claims.ClaimStatusValidating, 1, time.Time{}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("NewClaim: %v", err)
	}
	if err := repo.SaveClaim(ctx, tx, *c); err != nil {
		t.Fatalf("SaveClaim(seed): %v", err)
	}
	if err := repo.AppendEvent(ctx, tx, workflow.Event{
		Seq:     1,
		Type:    "claim.transitioned",
		ClaimID: claimID,
		Tenant:  tenant,
		From:    claims.ClaimStatusReceived,
		To:      claims.ClaimStatusValidating,
		EventID: seedEventID,
	}); err != nil {
		t.Fatalf("AppendEvent(seed): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// claimRow reads (status, version) for a claim inside a tenant-scoped tx.
func claimRow(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID) (claims.ClaimStatus, int) {
	t.Helper()
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var status string
	var version int
	if err := tx.QueryRow(ctx, `SELECT status, version FROM claims WHERE id = $1`, string(claimID)).Scan(&status, &version); err != nil {
		t.Fatalf("read claim: %v", err)
	}
	return claims.ClaimStatus(status), version
}

func eventRows(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID) []struct {
	Seq     int
	EventID string
	From    string
	To      string
} {
	t.Helper()
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT seq, event_id, from_status, to_status FROM claim_events WHERE tenant_id=$1 AND claim_id=$2 ORDER BY seq`, string(tenant), string(claimID))
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	var out []struct {
		Seq     int
		EventID string
		From    string
		To      string
	}
	for rows.Next() {
		var r struct {
			Seq     int
			EventID string
			From    string
			To      string
		}
		if err := rows.Scan(&r.Seq, &r.EventID, &r.From, &r.To); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("events rows.Err: %v", err)
	}
	return out
}

type transitionImage struct {
	Status  string `json:"status"`
	Version int    `json:"version"`
}

func decodeImage(t *testing.T, raw string) transitionImage {
	t.Helper()
	var img transitionImage
	if err := json.Unmarshal([]byte(raw), &img); err != nil {
		t.Fatalf("decode audit image %q: %v", raw, err)
	}
	return img
}

type auditRow struct {
	Action string
	Before string
	After  string
	Trace  string
}

func auditRows(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID) []auditRow {
	t.Helper()
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT action, "before"::text, "after"::text, trace_id FROM audit_log WHERE tenant_id=$1 AND claim_id=$2 ORDER BY id`, string(tenant), string(claimID))
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.Action, &r.Before, &r.After, &r.Trace); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows.Err: %v", err)
	}
	return out
}

// TestClaimBridge_InsufficientTransition_AppliedReplaySuperseded exercises the
// full disposition matrix against real PostgreSQL, including the invariants
// the pre-fix code violated: MAX(seq)+1 sequencing, structured audit payloads,
// replay-query error propagation (fail-closed), and superseded-after-advance.
func TestClaimBridge_InsufficientTransition_AppliedReplaySuperseded(t *testing.T) {
	pool := pgBridgePool(t)
	id := bridgeIDs()
	tenant := claims.TenantID("t-pg-" + id)
	claimID := claims.ClaimID("c-pg-" + id)
	seedValidatingClaim(t, pool, tenant, claimID, "seed-evt-"+id)

	bridge := workeradapter.NewClaimBridge(pool)
	ctx := context.Background()
	eventID := "insufficient-evt-" + id

	// ---- phase 1: first application -------------------------------------
	replay, err := bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 1, eventID)
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}
	if replay {
		t.Fatal("apply: replay must be false on the first application")
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusException || version != 2 {
		t.Fatalf("apply: claim=%s v%d, want EXCEPTION v2", status, version)
	}
	events := eventRows(t, pool, tenant, claimID)
	if len(events) != 2 {
		t.Fatalf("apply: %d event rows, want 2 (seed + transition)", len(events))
	}
	if got := events[1]; got.Seq != 2 || got.EventID != eventID ||
		got.From != string(claims.ClaimStatusValidating) || got.To != string(claims.ClaimStatusException) {
		t.Fatalf("apply: producer event = %+v, want seq2 %s VALIDATING->EXCEPTION", got, eventID)
	}
	audit := auditRows(t, pool, tenant, claimID)
	if len(audit) != 1 {
		t.Fatalf("apply: %d audit rows, want 1", len(audit))
	}
	if audit[0].Action != "transition.insufficient_to_exception" || audit[0].Trace != eventID {
		t.Fatalf("apply: audit = %+v, want action + trace_id=%s", audit[0], eventID)
	}
	if b := decodeImage(t, audit[0].Before); b.Status != string(claims.ClaimStatusValidating) || b.Version != 1 {
		t.Fatalf("apply: audit before = %+v, want VALIDATING v1", b)
	}
	if a := decodeImage(t, audit[0].After); a.Status != string(claims.ClaimStatusException) || a.Version != 2 {
		t.Fatalf("apply: audit after = %+v, want EXCEPTION v2", a)
	}

	// ---- phase 2: idempotent replay (still EXCEPTION) --------------------
	replay, err = bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 2, eventID)
	if err != nil {
		t.Fatalf("replay: unexpected error: %v", err)
	}
	if !replay {
		t.Fatal("replay: want replay==true for a still-EXCEPTION redelivery")
	}
	if got := len(eventRows(t, pool, tenant, claimID)); got != 2 {
		t.Fatalf("replay: %d event rows, want 2 (no new event)", got)
	}
	if got := len(auditRows(t, pool, tenant, claimID)); got != 1 {
		t.Fatalf("replay: %d audit rows, want 1 (no new audit)", got)
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusException || version != 2 {
		t.Fatalf("replay: claim=%s v%d, want unchanged EXCEPTION v2", status, version)
	}

	// ---- phase 3: advance past EXCEPTION (human/lifecycle) ---------------
	advanceClaim(t, pool, tenant, claimID, claims.ClaimStatusInvestigationRequired, "human-evt-"+id, 2)

	// ---- phase 4: superseded redelivery must be refused -----------------
	_, err = bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 3, eventID)
	if !errors.Is(err, coreworker.ErrTransitionSuperseded) {
		t.Fatalf("superseded: err=%v, want ErrTransitionSuperseded", err)
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusInvestigationRequired || version != 3 {
		t.Fatalf("superseded: claim=%s v%d, want unchanged INVESTIGATION_REQUIRED v3", status, version)
	}
	if got := len(eventRows(t, pool, tenant, claimID)); got != 3 {
		t.Fatalf("superseded: %d event rows, want 3 (no new event written)", got)
	}
	if got := len(auditRows(t, pool, tenant, claimID)); got != 1 {
		t.Fatalf("superseded: %d audit rows, want 1 (no new audit written)", got)
	}

	// ---- phase 5: stale version conflicts, writes nothing ----------------
	_, err = bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 1, eventID+"-other")
	if !errors.Is(err, postgres.ErrVersionConflict) {
		t.Fatalf("stale: err=%v, want ErrVersionConflict", err)
	}
	if got := len(eventRows(t, pool, tenant, claimID)); got != 3 {
		t.Fatalf("stale: %d event rows, want 3 (conflict wrote nothing)", got)
	}
	if got := len(auditRows(t, pool, tenant, claimID)); got != 1 {
		t.Fatalf("stale: %d audit rows, want 1 (conflict wrote nothing)", got)
	}
}

// TestClaimBridge_InsufficientTransition_TenantIsolated proves the producer
// cannot see or mutate another tenant's claim: RLS makes the row absent.
func TestClaimBridge_InsufficientTransition_TenantIsolated(t *testing.T) {
	pool := pgBridgePool(t)
	id := bridgeIDs()
	tenant := claims.TenantID("t-owner-" + id)
	claimID := claims.ClaimID("c-pg-" + id)
	seedValidatingClaim(t, pool, tenant, claimID, "seed-evt-"+id)

	bridge := workeradapter.NewClaimBridge(pool)
	_, err := bridge.RecordInsufficientEvidenceTransition(context.Background(), "t-intruder-"+id, string(claimID), 1, "evt-"+id)
	if err == nil {
		t.Fatal("tenant isolation: cross-tenant transition must fail, got nil")
	}
	if errors.Is(err, coreworker.ErrTransitionSuperseded) {
		t.Fatalf("tenant isolation: got superseded, want an absence/not-found error: %v", err)
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusValidating || version != 1 {
		t.Fatalf("tenant isolation: owner claim mutated to %s v%d, want VALIDATING v1", status, version)
	}
	if got := len(eventRows(t, pool, tenant, claimID)); got != 1 {
		t.Fatalf("tenant isolation: %d event rows, want 1 (seed only)", got)
	}
}

// advanceClaim moves a claim to next inside a tenant-scoped tx (simulating a
// human/lifecycle decision), using the same CAS the repository enforces.
func advanceClaim(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID, to claims.ClaimStatus, eventID string, expectedVersion int) {
	t.Helper()
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	repo := postgres.New(pool)
	c, err := repo.LoadClaim(ctx, tx, claimID)
	if err != nil {
		t.Fatalf("LoadClaim: %v", err)
	}
	next, terr := claims.Transition(c, to, eventID, expectedVersion, tenant)
	if terr != nil {
		t.Fatalf("Transition to %s: %v", to, terr)
	}
	if err := repo.SaveClaim(ctx, tx, next); err != nil {
		t.Fatalf("SaveClaim(advance): %v", err)
	}
	var maxSeq int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0) FROM claim_events WHERE tenant_id=$1 AND claim_id=$2`, string(tenant), string(claimID)).Scan(&maxSeq); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	if err := repo.AppendEvent(ctx, tx, workflow.Event{
		Seq:     maxSeq + 1,
		Type:    "claim.transitioned",
		ClaimID: claimID,
		Tenant:  tenant,
		From:    claims.ClaimStatusException,
		To:      to,
		EventID: eventID,
	}); err != nil {
		t.Fatalf("AppendEvent(advance): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit advance: %v", err)
	}
}
