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

// seedClaimAt inserts a claim at the given status, version 1, plus one
// pre-existing event row at seq 1 representing how the claim reached that
// status (creation "" -> RECEIVED for a RECEIVED seed, RECEIVED ->
// VALIDATING otherwise). The pre-existing event is load-bearing: the
// producer's new event must be seq 2 (derived from MAX(seq)+1), not a
// hard-coded 1.
func seedClaimAt(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID, status claims.ClaimStatus, seedEventID string) {
	t.Helper()
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	repo := postgres.New(pool)
	c, err := claims.NewClaim(claimID, tenant, claims.PolicyID("pol-"+string(claimID)), "ref-"+string(claimID),
		claims.MustPaise(100, 0), status, 1, time.Time{}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("NewClaim: %v", err)
	}
	if err := repo.SaveClaim(ctx, tx, *c); err != nil {
		t.Fatalf("SaveClaim(seed): %v", err)
	}
	seedFrom := claims.ClaimStatusReceived
	if status == claims.ClaimStatusReceived {
		seedFrom = ""
	}
	if err := repo.AppendEvent(ctx, tx, workflow.Event{
		Seq:     1,
		Type:    "claim.transitioned",
		ClaimID: claimID,
		Tenant:  tenant,
		From:    seedFrom,
		To:      status,
		EventID: seedEventID,
	}); err != nil {
		t.Fatalf("AppendEvent(seed): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

// seedValidatingClaim inserts a claim at VALIDATING, version 1, plus one
// pre-existing event row at seq 1 (RECEIVED -> VALIDATING).
func seedValidatingClaim(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID, seedEventID string) {
	t.Helper()
	seedClaimAt(t, pool, tenant, claimID, claims.ClaimStatusValidating, seedEventID)
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
	err := bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 1, eventID)
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
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
	// A nil return is the idempotent-replay disposition; the replay
	// semantics (nothing new written, claim unchanged) are asserted below.
	err = bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 2, eventID)
	if err != nil {
		t.Fatalf("replay: unexpected error: %v", err)
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
	err = bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 3, eventID)
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
	err = bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 1, eventID+"-other")
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
	err := bridge.RecordInsufficientEvidenceTransition(context.Background(), "t-intruder-"+id, string(claimID), 1, "evt-"+id)
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

// TestClaimBridge_InsufficientTransition_SecondDocIllegal (review F1): a
// second insufficient document on a claim already at EXCEPTION carries a NEW
// event identity, so the replay gate does not fire and the state machine
// rejects EXCEPTION -> EXCEPTION with CodeIllegalTransition. The bridge must
// surface that error unwrapped — the processor matches it with errors.As and
// acks the delivery — and write nothing.
func TestClaimBridge_InsufficientTransition_SecondDocIllegal(t *testing.T) {
	pool := pgBridgePool(t)
	id := bridgeIDs()
	tenant := claims.TenantID("t-pg2-" + id)
	claimID := claims.ClaimID("c-pg2-" + id)
	seedValidatingClaim(t, pool, tenant, claimID, "seed-evt-"+id)

	bridge := workeradapter.NewClaimBridge(pool)
	ctx := context.Background()

	// First insufficient document stamps VALIDATING -> EXCEPTION (v2).
	first := "insufficient-evt-1-" + id
	if err := bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 1, first); err != nil {
		t.Fatalf("first doc: unexpected error: %v", err)
	}

	// Second insufficient document, new event identity: the state machine
	// must reject it as illegal and the bridge must surface that error
	// unwrapped, writing nothing.
	second := "insufficient-evt-2-" + id
	err := bridge.RecordInsufficientEvidenceTransition(ctx, string(tenant), string(claimID), 2, second)
	var terr *claims.TransitionError
	if !errors.As(err, &terr) || terr.Code != claims.CodeIllegalTransition {
		t.Fatalf("second doc: err=%v, want *claims.TransitionError CodeIllegalTransition (the processor's errors.As predicate)", err)
	}
	if terr.From != string(claims.ClaimStatusException) || terr.To != string(claims.ClaimStatusException) {
		t.Fatalf("second doc: transition = %s->%s, want EXCEPTION->EXCEPTION", terr.From, terr.To)
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusException || version != 2 {
		t.Fatalf("second doc: claim=%s v%d, want unchanged EXCEPTION v2", status, version)
	}
	if got := len(eventRows(t, pool, tenant, claimID)); got != 2 {
		t.Fatalf("second doc: %d event rows, want 2 (seed + first transition only)", got)
	}
	if got := len(auditRows(t, pool, tenant, claimID)); got != 1 {
		t.Fatalf("second doc: %d audit rows, want 1 (first transition only)", got)
	}
}

// TestClaimBridge_InsufficientTransition_PreValidationIllegal (review F2):
// the producer is authorized only for the VALIDATING -> EXCEPTION edge. A
// claim still pre-validation (RECEIVED) rejects the transition with
// CodeIllegalTransition and nothing is written. Until the intake
// progression slice (D1) advances claims into VALIDATING, this documents
// the deliberate fail-closed disposition: the processor acks the delivery
// (no poison loop) and the claim stays untouched.
func TestClaimBridge_InsufficientTransition_PreValidationIllegal(t *testing.T) {
	pool := pgBridgePool(t)
	id := bridgeIDs()
	tenant := claims.TenantID("t-pg3-" + id)
	claimID := claims.ClaimID("c-pg3-" + id)
	seedClaimAt(t, pool, tenant, claimID, claims.ClaimStatusReceived, "seed-evt-"+id)

	bridge := workeradapter.NewClaimBridge(pool)
	err := bridge.RecordInsufficientEvidenceTransition(context.Background(), string(tenant), string(claimID), 1, "insufficient-evt-"+id)
	var terr *claims.TransitionError
	if !errors.As(err, &terr) || terr.Code != claims.CodeIllegalTransition {
		t.Fatalf("pre-validation: err=%v, want *claims.TransitionError CodeIllegalTransition (the processor's errors.As predicate)", err)
	}
	if terr.From != string(claims.ClaimStatusReceived) || terr.To != string(claims.ClaimStatusException) {
		t.Fatalf("pre-validation: transition = %s->%s, want RECEIVED->EXCEPTION", terr.From, terr.To)
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusReceived || version != 1 {
		t.Fatalf("pre-validation: claim=%s v%d, want unchanged RECEIVED v1", status, version)
	}
	if got := len(eventRows(t, pool, tenant, claimID)); got != 1 {
		t.Fatalf("pre-validation: %d event rows, want 1 (seed only)", got)
	}
	if got := len(auditRows(t, pool, tenant, claimID)); got != 0 {
		t.Fatalf("pre-validation: %d audit rows, want 0 (nothing written)", got)
	}
}

// TestClaimBridge_IntakeProgression_FromReceived drives the D1 intake chain
// end to end against real PostgreSQL: RECEIVED -> REGISTERED ->
// DOCUMENTS_RECEIVED -> VALIDATING in three independent transactions, each
// writing claim CAS + claim_events + audit atomically. Then a replay call
// must be a no-op (progression complete).
func TestClaimBridge_IntakeProgression_FromReceived(t *testing.T) {
	pool := pgBridgePool(t)
	id := bridgeIDs()
	tenant := claims.TenantID("t-intake-" + id)
	claimID := claims.ClaimID("c-intake-" + id)
	docID := "doc-intake-" + id
	seedClaimAt(t, pool, tenant, claimID, claims.ClaimStatusReceived, "seed-evt-"+id)

	bridge := workeradapter.NewClaimBridge(pool)
	ctx := context.Background()

	if err := bridge.AdvanceToIntakeValidating(ctx, string(tenant), string(claimID), docID); err != nil {
		t.Fatalf("advance: unexpected error: %v", err)
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusValidating || version != 4 {
		t.Fatalf("advance: claim=%s v%d, want VALIDATING v4 (three steps)", status, version)
	}
	events := eventRows(t, pool, tenant, claimID)
	if len(events) != 4 {
		t.Fatalf("advance: %d event rows, want 4 (seed + three steps)", len(events))
	}
	wantSteps := []struct {
		seq  int
		to   claims.ClaimStatus
		from claims.ClaimStatus
	}{
		{2, claims.ClaimStatusRegistered, claims.ClaimStatusReceived},
		{3, claims.ClaimStatusDocumentsReceived, claims.ClaimStatusRegistered},
		{4, claims.ClaimStatusValidating, claims.ClaimStatusDocumentsReceived},
	}
	for i, w := range wantSteps {
		got := events[i+1]
		wantID := fmt.Sprintf("intake:%s:%s:%s", claimID, docID, w.to)
		if got.Seq != w.seq || got.EventID != wantID ||
			got.From != string(w.from) || got.To != string(w.to) {
			t.Fatalf("advance: step event = %+v, want seq%d %s->%s eventID %q", got, w.seq, w.from, w.to, wantID)
		}
	}
	audit := auditRows(t, pool, tenant, claimID)
	if len(audit) != 3 {
		t.Fatalf("advance: %d audit rows, want 3 (one per step)", len(audit))
	}
	for i, w := range wantSteps {
		if audit[i].Action != "transition.intake" || audit[i].Trace != fmt.Sprintf("intake:%s:%s:%s", claimID, docID, w.to) {
			t.Fatalf("advance: audit[%d] = %+v, want action transition.intake with the step event identity", i, audit[i])
		}
		if b := decodeImage(t, audit[i].Before); b.Status != string(w.from) || b.Version != i+1 {
			t.Fatalf("advance: audit[%d] before = %+v, want %s v%d", i, b, w.from, i+1)
		}
		if a := decodeImage(t, audit[i].After); a.Status != string(w.to) || a.Version != i+2 {
			t.Fatalf("advance: audit[%d] after = %+v, want %s v%d", i, a, w.to, i+2)
		}
	}

	// Replay: progression complete — nothing new written.
	if err := bridge.AdvanceToIntakeValidating(ctx, string(tenant), string(claimID), docID); err != nil {
		t.Fatalf("replay: unexpected error: %v", err)
	}
	if got := len(eventRows(t, pool, tenant, claimID)); got != 4 {
		t.Fatalf("replay: %d event rows, want 4 (no new event)", got)
	}
	if got := len(auditRows(t, pool, tenant, claimID)); got != 3 {
		t.Fatalf("replay: %d audit rows, want 3 (no new audit)", got)
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusValidating || version != 4 {
		t.Fatalf("replay: claim=%s v%d, want unchanged VALIDATING v4", status, version)
	}
}

// TestClaimBridge_IntakeProgression_NonIntakeNoOp proves the intake chain
// never touches claims outside its authorized statuses: EXCEPTION (human
// review owns it) and EXPIRED (lifecycle owns it) are no-ops.
func TestClaimBridge_IntakeProgression_NonIntakeNoOp(t *testing.T) {
	pool := pgBridgePool(t)
	id := bridgeIDs()
	bridge := workeradapter.NewClaimBridge(pool)
	ctx := context.Background()

	for _, status := range []claims.ClaimStatus{claims.ClaimStatusException, claims.ClaimStatusExpired} {
		tenant := claims.TenantID("t-noop-" + string(status) + "-" + id)
		claimID := claims.ClaimID("c-noop-" + string(status) + "-" + id)
		seedClaimAt(t, pool, tenant, claimID, status, "seed-evt-"+id)

		if err := bridge.AdvanceToIntakeValidating(ctx, string(tenant), string(claimID), "doc-noop-"+id); err != nil {
			t.Fatalf("%s: unexpected error: %v", status, err)
		}
		if got, version := claimRow(t, pool, tenant, claimID); got != status || version != 1 {
			t.Fatalf("%s: claim=%s v%d, want unchanged %s v1", status, got, version, status)
		}
		if got := len(eventRows(t, pool, tenant, claimID)); got != 1 {
			t.Fatalf("%s: %d event rows, want 1 (seed only)", status, got)
		}
		if got := len(auditRows(t, pool, tenant, claimID)); got != 0 {
			t.Fatalf("%s: %d audit rows, want 0 (nothing written)", status, got)
		}
	}
}

// TestClaimBridge_IntakeProgression_TenantIsolated proves the intake chain
// cannot see or mutate another tenant's claim: RLS makes the row absent.
func TestClaimBridge_IntakeProgression_TenantIsolated(t *testing.T) {
	pool := pgBridgePool(t)
	id := bridgeIDs()
	tenant := claims.TenantID("t-intake-owner-" + id)
	claimID := claims.ClaimID("c-intake-" + id)
	seedClaimAt(t, pool, tenant, claimID, claims.ClaimStatusReceived, "seed-evt-"+id)

	bridge := workeradapter.NewClaimBridge(pool)
	err := bridge.AdvanceToIntakeValidating(context.Background(), "t-intruder-"+id, string(claimID), "doc-intake-"+id)
	if err == nil {
		t.Fatal("tenant isolation: cross-tenant advance must fail, got nil")
	}
	if errors.Is(err, coreworker.ErrTransitionSuperseded) {
		t.Fatalf("tenant isolation: got superseded, want an absence/not-found error: %v", err)
	}
	if status, version := claimRow(t, pool, tenant, claimID); status != claims.ClaimStatusReceived || version != 1 {
		t.Fatalf("tenant isolation: owner claim mutated to %s v%d, want RECEIVED v1", status, version)
	}
	if got := len(eventRows(t, pool, tenant, claimID)); got != 1 {
		t.Fatalf("tenant isolation: %d event rows, want 1 (seed only)", got)
	}
}
