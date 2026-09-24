package postgres_test

// S5 pin: EXPIRED claims are never in the pending HITL set (APA-10 intact).
// Pending is exactly {HITL, ACTION_PENDING}; expiry removes the claim from
// the human-action queue (re-drive is an explicit new decision, not a
// pending row). Passes pre-fix too — it pins the query contract against
// the new status value.

import (
	"context"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"
)

func TestHITL_ExpiredExcludedFromPending(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	tenant := claims.TenantID("tnt-" + string(uniqueClaimID("exp-tenant")))
	ctx := postgres.WithTenant(context.Background(), tenant)
	tx, err := postgres.BeginTenantTx(ctx, pool)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	hitl := newHITLClaim(string(tenant), string(uniqueClaimID("exp-hitl")), claims.ClaimStatusHITL)
	if err := repo.SaveClaim(ctx, tx, hitl); err != nil {
		t.Fatalf("save hitl: %v", err)
	}
	expired := newHITLClaim(string(tenant), string(uniqueClaimID("exp-done")), claims.ClaimStatusExpired)
	if err := repo.SaveClaim(ctx, tx, expired); err != nil {
		t.Fatalf("save expired: %v", err)
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
	pending, err := repo.ListPendingHITL(ctx2, tx2)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	for _, c := range pending {
		if c.ID == expired.ID {
			t.Fatalf("EXPIRED claim %q listed as pending", c.ID)
		}
		if c.Status == claims.ClaimStatusExpired {
			t.Fatalf("pending set contains EXPIRED status")
		}
	}
	found := false
	for _, c := range pending {
		if c.ID == hitl.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("HITL claim %q missing from pending", hitl.ID)
	}
}
