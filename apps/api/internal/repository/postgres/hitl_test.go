package postgres_test

import (
	"context"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"
)

func newHITLClaim(tenant, idSuffix string, status claims.ClaimStatus) claims.Claim {
	now := time.Now().Truncate(time.Second)
	c, err := claims.NewClaim(
		claims.ClaimID("hitl-"+idSuffix),
		claims.TenantID(tenant),
		claims.PolicyID("pol-hitl-01"),
		"REF-HITL-01",
		claims.MustPaise(1000, 0),
		status,
		1,
		now, now, time.Time{},
	)
	if err != nil {
		panic(err)
	}
	return *c
}

func TestHITL_PendingCanonicalMapping(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	tenant := claims.TenantID("tnt-" + string(uniqueClaimID("hitl-tenant")))
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	claim := newHITLClaim(string(tenant), string(uniqueClaimID("pending")), claims.ClaimStatusActionPending)
	if err := repo.SaveClaim(ctx, tx, claim); err != nil {
		t.Fatalf("save pending: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ctx2 := postgres.WithTenant(context.Background(), tenant)
	tx2, err := postgres.BeginTenantTx(ctx2, pool)
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	defer tx2.Rollback(ctx2)
	got, err := repo.LoadClaim(ctx2, tx2, claim.ID)
	if err != nil {
		t.Fatalf("load pending: %v", err)
	}
	if got.Status != claims.ClaimStatusActionPending {
		t.Fatalf("status = %q, want %q (canonical)", string(got.Status), string(claims.ClaimStatusActionPending))
	}
	pending, err := repo.ListPendingHITL(ctx2, tx2)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	found := false
	for _, c := range pending {
		if c.ID == claim.ID {
			found = true
			if c.Status != claims.ClaimStatusActionPending {
				t.Fatalf("listed status = %q, want canonical %q", string(c.Status), string(claims.ClaimStatusActionPending))
			}
		}
	}
	if !found {
		t.Fatalf("pending claim %q not returned by ListPendingHITL (query used canonical status, not 'pending')", string(claim.ID))
	}
	var cnt int
	if err := tx2.QueryRow(ctx2, "SELECT COUNT(*) FROM claims WHERE status = 'pending'").Scan(&cnt); err != nil {
		t.Fatalf("count pending lowercase: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("lowercase 'pending' count = %d, want 0 (canonical rows store 'ACTION_PENDING', not 'pending')", cnt)
	}
}

func TestHITL_AllStatesRoundTrip(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	tenant := claims.TenantID("tnt-" + string(uniqueClaimID("hitl-all")))
	cases := []claims.ClaimStatus{
		claims.ClaimStatusHITL,
		claims.ClaimStatusActionPending,
		claims.ClaimStatusActioned,
		claims.ClaimStatusVerified,
		claims.ClaimStatusReceived,
		claims.ClaimStatusException,
	}
	for _, want := range cases {
		id := uniqueClaimID("hitl-state-" + string(want))
		claim := newHITLClaim(string(tenant), string(id), want)
		ctx := postgres.WithTenant(context.Background(), tenant)
		tx, err := postgres.BeginTenantTx(ctx, pool)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := repo.SaveClaim(ctx, tx, claim); err != nil {
			t.Fatalf("save %q: %v", string(want), err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %q: %v", string(want), err)
		}
		ctx2 := postgres.WithTenant(context.Background(), tenant)
		tx2, err := postgres.BeginTenantTx(ctx2, pool)
		if err != nil {
			t.Fatalf("begin2: %v", err)
		}
		got, err := repo.LoadClaim(ctx2, tx2, claim.ID)
		_ = tx2.Rollback(ctx2)
		if err != nil {
			t.Fatalf("load %q: %v", string(want), err)
		}
		if got.Status != want {
			t.Fatalf("round-trip status = %q, want %q", string(got.Status), string(want))
		}
	}
}

func TestHITL_ListPendingReturnsPendingOnly(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	tenant := claims.TenantID("tnt-" + string(uniqueClaimID("hitl-pending-only")))
	pendingIDs := map[claims.ClaimID]bool{}
	allIDs := map[claims.ClaimID]claims.ClaimStatus{}
	statuses := []claims.ClaimStatus{
		claims.ClaimStatusHITL,
		claims.ClaimStatusActionPending,
		claims.ClaimStatusActioned,
		claims.ClaimStatusReceived,
		claims.ClaimStatusRegistered,
		claims.ClaimStatusClosed,
		claims.ClaimStatusVerified,
	}
	for _, st := range statuses {
		id := uniqueClaimID("hitl-list-" + string(st))
		c := newHITLClaim(string(tenant), string(id), st)
		ctx := postgres.WithTenant(context.Background(), tenant)
		tx, err := postgres.BeginTenantTx(ctx, pool)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := repo.SaveClaim(ctx, tx, c); err != nil {
			t.Fatalf("save %q: %v", string(st), err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit %q: %v", string(st), err)
		}
		allIDs[c.ID] = st
		if st == claims.ClaimStatusHITL || st == claims.ClaimStatusActionPending {
			pendingIDs[c.ID] = true
		}
	}
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("begin list: %v", err)
	}
	defer tx.Rollback(ctx)
	pending, err := repo.ListPendingHITL(ctx, tx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	gotPending := map[claims.ClaimID]bool{}
	for _, c := range pending {
		gotPending[c.ID] = true
		if c.Status != claims.ClaimStatusHITL && c.Status != claims.ClaimStatusActionPending {
			t.Fatalf("pending list returned non-pending status %q for claim %q", string(c.Status), string(c.ID))
		}
	}
	for id := range pendingIDs {
		if !gotPending[id] {
			t.Fatalf("pending claim %q (%q) missing from ListPendingHITL", string(id), string(allIDs[id]))
		}
	}
	for id := range gotPending {
		if !pendingIDs[id] {
			t.Fatalf("ListPendingHITL returned unexpected claim %q (not in expected pending set; possibly stale row or wrong status predicate)", string(id))
		}
	}
}

func TestHITL_ListPendingTenantIsolation(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	tenantA := claims.TenantID("tnt-" + string(uniqueClaimID("hitl-tenant-a")))
	tenantB := claims.TenantID("tnt-" + string(uniqueClaimID("hitl-tenant-b")))
	claimA := newHITLClaim(string(tenantA), string(uniqueClaimID("hitl-iso-a")), claims.ClaimStatusActionPending)
	ctxA := postgres.WithTenant(context.Background(), tenantA)
	tx, err := postgres.BeginTenantTx(ctxA, pool)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.SaveClaim(ctxA, tx, claimA); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := tx.Commit(ctxA); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	ctxB := postgres.WithTenant(context.Background(), tenantB)
	tx2, err := postgres.BeginTenantTx(ctxB, pool)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer tx2.Rollback(ctxB)
	pendingB, err := repo.ListPendingHITL(ctxB, tx2)
	if err != nil {
		t.Fatalf("list pending B: %v", err)
	}
	for _, c := range pendingB {
		if c.ID == claimA.ID {
			t.Fatalf("tenant isolation leak: tenant B saw tenant A's pending claim %q", string(claimA.ID))
		}
	}
}
