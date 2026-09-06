package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/workflow"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var idSeq atomic.Int64

func testDSN() string {
	if v := os.Getenv("TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://claimops_app:claimops_app@localhost:5433/claimops"
}

// requirePool dials the live DB as the NON-superuser app role (RLS enforced).
// If the DB is unreachable the test is skipped so unit CI stays green.
func requirePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN())
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

// uniqueClaimID is deterministic within and across runs without time or
// randomness: per-process pid isolates reruns (leftover rows cannot be
// deleted/truncated by the app role), the atomic counter isolates tests.
func uniqueClaimID(prefix string) claims.ClaimID {
	n := idSeq.Add(1)
	return claims.ClaimID(fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), n))
}

func mustNewClaim(t *testing.T, id claims.ClaimID, tenant claims.TenantID) claims.Claim {
	t.Helper()
	c, err := claims.NewClaim(
		id,
		tenant,
		claims.PolicyID("pol-"+string(id)),
		"ref-"+string(id),
		claims.MustPaise(100, 0),
		claims.ClaimStatusReceived,
		1,
		// Zero dates -> NULL DATE columns.
		time.Time{},
		time.Time{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewClaim: %v", err)
	}
	return *c
}

// beginAs scopes a fresh transaction to tenant via BeginTenantTx.
func beginAs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant claims.TenantID) (context.Context, pgx.Tx) {
	t.Helper()
	tctx := postgres.WithTenant(ctx, tenant)
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx(%s): %v", tenant, err)
	}
	return tctx, tx
}

func rollback(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	_ = tx.Rollback(ctx)
}

func commit(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func countClaimEvents(t *testing.T, ctx context.Context, tx pgx.Tx, id claims.ClaimID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM claim_events WHERE claim_id = $1`, string(id)).Scan(&n); err != nil {
		t.Fatalf("count claim_events: %v", err)
	}
	return n
}

// 1. Tenant A saves a claim and applies one transition; Tenant B sees
// nothing: LoadClaim -> ErrNotFound and the event trail reads empty under
// B's RLS scope (COUNT(*) == 0, never asserted via WHERE-clause text).
func TestTenantIsolation_GetAndEventsHidden(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenantA := claims.TenantID("t1-tenant-a")
	tenantB := claims.TenantID("t1-tenant-b")
	claimID := uniqueClaimID("t1-claim")

	c := mustNewClaim(t, claimID, tenantA)

	// Put as A.
	tctx, tx := beginAs(t, ctx, pool, tenantA)
	if err := repo.SaveClaim(tctx, tx, c); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim A: %v", err)
	}
	commit(t, tctx, tx)

	// Apply as A: valid RECEIVED -> REGISTERED flow, then persist + audit.
	next, err := claims.Transition(c, claims.ClaimStatusRegistered, "ev-t1-1", 1, tenantA)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	tctx, tx = beginAs(t, ctx, pool, tenantA)
	if err := repo.SaveClaim(tctx, tx, next); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim A v2: %v", err)
	}
	if err := repo.AppendEvent(tctx, tx, workflow.Event{
		Seq:     1,
		Type:    workflow.EventTypeTransitioned,
		ClaimID: claimID,
		Tenant:  tenantA,
		From:    claims.ClaimStatusReceived,
		To:      claims.ClaimStatusRegistered,
		EventID: "ev-t1-1",
	}); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendEvent A: %v", err)
	}
	commit(t, tctx, tx)

	// B must not see the claim.
	bctx, btx := beginAs(t, ctx, pool, tenantB)
	_, err = repo.LoadClaim(bctx, btx, claimID)
	if !errors.Is(err, workflow.ErrNotFound) {
		rollback(t, bctx, btx)
		t.Fatalf("B LoadClaim = %v, want ErrNotFound", err)
	}
	if n := countClaimEvents(t, bctx, btx, claimID); n != 0 {
		rollback(t, bctx, btx)
		t.Fatalf("B event count = %d, want 0", n)
	}
	rollback(t, bctx, btx)

	// Sanity: A still sees its own row (proves B's miss is RLS, not absence).
	actx, atx := beginAs(t, ctx, pool, tenantA)
	got, err := repo.LoadClaim(actx, atx, claimID)
	if err != nil {
		rollback(t, actx, atx)
		t.Fatalf("A LoadClaim: %v", err)
	}
	if got.Status != claims.ClaimStatusRegistered || got.Version != 2 {
		rollback(t, actx, atx)
		t.Fatalf("A row = %+v, want REGISTERED v2", got)
	}
	if n := countClaimEvents(t, actx, atx, claimID); n != 1 {
		rollback(t, actx, atx)
		t.Fatalf("A event count = %d, want 1", n)
	}
	rollback(t, actx, atx)
}

// 2. Same claim ID written under Tenant B cannot clobber Tenant A's row.
//
// NOTE on schema reality: infra/postgres/migrations/001_init.sql declares
// claims(id TEXT PRIMARY KEY) — a GLOBAL primary key, not a composite
// (tenant_id, id) key. Two tenants therefore cannot hold separate rows with
// the same id; B's overwrite attempt collides globally. RLS additionally
// hides A's row from B's transaction, so the ON CONFLICT ... WHERE
// claims.version = $oldVersion branch matches zero rows and SaveClaim
// reports ErrVersionConflict. This test asserts that real behavior (reject
// + A unaffected) rather than a fictional duplicate row.
func TestCrossTenantOverwriteBlocked(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenantA := claims.TenantID("t2-tenant-a")
	tenantB := claims.TenantID("t2-tenant-b")
	claimID := uniqueClaimID("t2-claim")

	a := mustNewClaim(t, claimID, tenantA)
	tctx, tx := beginAs(t, ctx, pool, tenantA)
	if err := repo.SaveClaim(tctx, tx, a); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim A: %v", err)
	}
	commit(t, tctx, tx)

	// B tries to save the SAME id under its own tenant.
	b := mustNewClaim(t, claimID, tenantB)
	bctx, btx := beginAs(t, ctx, pool, tenantB)
	err := repo.SaveClaim(bctx, btx, b)
	if !errors.Is(err, postgres.ErrVersionConflict) {
		rollback(t, bctx, btx)
		t.Fatalf("B SaveClaim over A's id = %v, want ErrVersionConflict", err)
	}
	rollback(t, bctx, btx)

	// B still cannot read the id; A is unaffected.
	bctx2, btx2 := beginAs(t, ctx, pool, tenantB)
	if _, err := repo.LoadClaim(bctx2, btx2, claimID); !errors.Is(err, workflow.ErrNotFound) {
		rollback(t, bctx2, btx2)
		t.Fatalf("B LoadClaim = %v, want ErrNotFound", err)
	}
	rollback(t, bctx2, btx2)

	actx, atx := beginAs(t, ctx, pool, tenantA)
	got, err := repo.LoadClaim(actx, atx, claimID)
	if err != nil {
		rollback(t, actx, atx)
		t.Fatalf("A LoadClaim: %v", err)
	}
	if got.Tenant != tenantA || got.Version != 1 || got.Reference != a.Reference {
		rollback(t, actx, atx)
		t.Fatalf("A row changed = %+v", got)
	}
	rollback(t, actx, atx)
}

// 3. No tenant in context -> BeginTenantTx refuses before touching the pool.
func TestMissingTenant_BeginTenantTxFails(t *testing.T) {
	pool := requirePool(t)
	_, err := postgres.BeginTenantTx(context.Background(), pool)
	if !errors.Is(err, postgres.ErrNoTenant) {
		t.Fatalf("BeginTenantTx without tenant = %v, want ErrNoTenant", err)
	}
}

// 4. Pooled-connection reuse never leaks tenant scope: txn1 as A rolls back
// an uncommitted write; txn2 as B (likely the same pooled connection, with
// only a transaction-local SET LOCAL tenant) still sees exactly its own rows.
func TestConnectionReuse_NoTenantLeak(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenantA := claims.TenantID("t4-tenant-a")
	tenantB := claims.TenantID("t4-tenant-b")
	claimA := uniqueClaimID("t4-claim-a")
	claimB := uniqueClaimID("t4-claim-b")
	claimATmp := uniqueClaimID("t4-claim-tmp")

	// Committed rows via independent paths.
	tctx, tx := beginAs(t, ctx, pool, tenantA)
	if err := repo.SaveClaim(tctx, tx, mustNewClaim(t, claimA, tenantA)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim A: %v", err)
	}
	commit(t, tctx, tx)

	tctx, tx = beginAs(t, ctx, pool, tenantB)
	if err := repo.SaveClaim(tctx, tx, mustNewClaim(t, claimB, tenantB)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim B: %v", err)
	}
	commit(t, tctx, tx)

	// txn1 as A: sees A, not B; stages an uncommitted row, then rolls back.
	a1ctx, a1tx := beginAs(t, ctx, pool, tenantA)
	if _, err := repo.LoadClaim(a1ctx, a1tx, claimA); err != nil {
		rollback(t, a1ctx, a1tx)
		t.Fatalf("txn1 A LoadClaim own: %v", err)
	}
	if _, err := repo.LoadClaim(a1ctx, a1tx, claimB); !errors.Is(err, workflow.ErrNotFound) {
		rollback(t, a1ctx, a1tx)
		t.Fatalf("txn1 A LoadClaim B's id = %v, want ErrNotFound", err)
	}
	if err := repo.SaveClaim(a1ctx, a1tx, mustNewClaim(t, claimATmp, tenantA)); err != nil {
		rollback(t, a1ctx, a1tx)
		t.Fatalf("txn1 A staged write: %v", err)
	}
	rollback(t, a1ctx, a1tx)

	// txn2 as B: sees only B rows — neither A's committed row nor A's
	// rolled-back staging row.
	bctx, btx := beginAs(t, ctx, pool, tenantB)
	if _, err := repo.LoadClaim(bctx, btx, claimB); err != nil {
		rollback(t, bctx, btx)
		t.Fatalf("txn2 B LoadClaim own: %v", err)
	}
	if _, err := repo.LoadClaim(bctx, btx, claimA); !errors.Is(err, workflow.ErrNotFound) {
		rollback(t, bctx, btx)
		t.Fatalf("txn2 B LoadClaim A's id = %v, want ErrNotFound", err)
	}
	if _, err := repo.LoadClaim(bctx, btx, claimATmp); !errors.Is(err, workflow.ErrNotFound) {
		rollback(t, bctx, btx)
		t.Fatalf("txn2 B LoadClaim A's tmp id = %v, want ErrNotFound", err)
	}
	rollback(t, bctx, btx)

	// Fresh A txn: staged row is gone, committed row remains.
	actx, atx := beginAs(t, ctx, pool, tenantA)
	if _, err := repo.LoadClaim(actx, atx, claimATmp); !errors.Is(err, workflow.ErrNotFound) {
		rollback(t, actx, atx)
		t.Fatalf("A LoadClaim tmp = %v, want ErrNotFound after rollback", err)
	}
	if _, err := repo.LoadClaim(actx, atx, claimA); err != nil {
		rollback(t, actx, atx)
		t.Fatalf("A LoadClaim committed: %v", err)
	}
	rollback(t, actx, atx)
}

// 5. Optimistic concurrency: re-saving v1 without incrementing conflicts;
// advancing via claims.Transition to v2 succeeds.
func TestOptimisticConcurrency_VersionConflict(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := claims.TenantID("t5-tenant")
	claimID := uniqueClaimID("t5-claim")

	v1 := mustNewClaim(t, claimID, tenant)
	tctx, tx := beginAs(t, ctx, pool, tenant)
	if err := repo.SaveClaim(tctx, tx, v1); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim v1: %v", err)
	}
	commit(t, tctx, tx)

	// Stale writer: same id, version NOT incremented.
	tctx, tx = beginAs(t, ctx, pool, tenant)
	if err := repo.SaveClaim(tctx, tx, v1); !errors.Is(err, postgres.ErrVersionConflict) {
		rollback(t, tctx, tx)
		t.Fatalf("stale SaveClaim v1 = %v, want ErrVersionConflict", err)
	}
	rollback(t, tctx, tx)

	// Correct next version via the valid state flow.
	v2, err := claims.Transition(v1, claims.ClaimStatusRegistered, "ev-t5-1", 1, tenant)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	tctx, tx = beginAs(t, ctx, pool, tenant)
	if err := repo.SaveClaim(tctx, tx, v2); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim v2: %v", err)
	}
	commit(t, tctx, tx)

	tctx, tx = beginAs(t, ctx, pool, tenant)
	got, err := repo.LoadClaim(tctx, tx, claimID)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("LoadClaim: %v", err)
	}
	if got.Version != 2 || got.Status != claims.ClaimStatusRegistered {
		rollback(t, tctx, tx)
		t.Fatalf("row = %+v, want REGISTERED v2", got)
	}
	rollback(t, tctx, tx)
}

// 6. Idempotent replay: appending the same (tenant, claim, event) twice
// returns nil both times and leaves exactly one row.
//
// NOTE on implementation reality: AppendEvent swallows the 23505 unique
// violation and returns nil, but PostgreSQL still aborts the surrounding
// transaction on that error (no SAVEPOINT is taken inside AppendEvent), so
// the replay transaction can never COMMIT — it must be rolled back. The
// test therefore performs the two appends in separate transactions: the
// first commits, the replay returns nil and is rolled back, and a fresh
// transaction observes exactly one row.
func TestIdempotentReplay_AppendEventTwice(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := claims.TenantID("t6-tenant")
	claimID := uniqueClaimID("t6-claim")

	tctx, tx := beginAs(t, ctx, pool, tenant)
	if err := repo.SaveClaim(tctx, tx, mustNewClaim(t, claimID, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim: %v", err)
	}
	commit(t, tctx, tx)

	evt := workflow.Event{
		Seq:     1,
		Type:    workflow.EventTypeTransitioned,
		ClaimID: claimID,
		Tenant:  tenant,
		From:    claims.ClaimStatusReceived,
		To:      claims.ClaimStatusRegistered,
		EventID: "ev-t6-1",
	}
	tctx, tx = beginAs(t, ctx, pool, tenant)
	if err := repo.AppendEvent(tctx, tx, evt); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendEvent #1: %v", err)
	}
	commit(t, tctx, tx)

	// Replay in the SAME txn: savepoint swallows 23505 -> nil, and the
	// txn stays healthy enough to commit. Exactly one row survives.
	tctx, tx = beginAs(t, ctx, pool, tenant)
	if err := repo.AppendEvent(tctx, tx, evt); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendEvent #2 (replay): %v", err)
	}
	commit(t, tctx, tx)

	tctx, tx = beginAs(t, ctx, pool, tenant)
	if n := countClaimEvents(t, tctx, tx, claimID); n != 1 {
		rollback(t, tctx, tx)
		t.Fatalf("event count = %d, want 1", n)
	}
	rollback(t, tctx, tx)
}

// 7. Audit log is append-only: the service role holds SELECT+INSERT only,
// so a direct UPDATE is denied at the privilege layer (migration 002
// revoked UPDATE/DELETE explicitly after the probe showed REVOKE FROM
// PUBLIC alone does not bind roles with explicit grants).
func TestAuditAppendOnly_UpdateRejected(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := claims.TenantID("t7-tenant")
	claimID := uniqueClaimID("t7-claim")

	tctx, tx := beginAs(t, ctx, pool, tenant)
	if err := repo.SaveClaim(tctx, tx, mustNewClaim(t, claimID, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_log
		(tenant_id, claim_id, actor_type, actor_id, action, entity, entity_id, trace_id)
		VALUES ($1, $2, 'user', 'u1', 'create', 'claim', $2, 't1')`,
		string(tenant), string(claimID)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("insert audit_log: %v", err)
	}
	commit(t, tctx, tx)

	tctx, tx = beginAs(t, ctx, pool, tenant)
	_, err := tx.Exec(tctx, `UPDATE audit_log SET tenant_id = $1 WHERE claim_id = $2`,
		"t7-other-tenant", string(claimID))
	if err == nil {
		rollback(t, tctx, tx)
		t.Fatalf("UPDATE audit_log succeeded, want error (append-only / RLS)")
	}
	rollback(t, tctx, tx)
}

// 8. Rollback discards the staged claim: a fresh transaction cannot see it.
func TestRollback_RowInvisible(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := claims.TenantID("t8-tenant")
	claimID := uniqueClaimID("t8-claim")

	tctx, tx := beginAs(t, ctx, pool, tenant)
	if err := repo.SaveClaim(tctx, tx, mustNewClaim(t, claimID, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim: %v", err)
	}
	rollback(t, tctx, tx)

	tctx2, tx2 := beginAs(t, ctx, pool, tenant)
	defer rollback(t, tctx2, tx2)
	if _, err := repo.LoadClaim(tctx2, tx2, claimID); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("LoadClaim after rollback = %v, want ErrNotFound", err)
	}
}
