package postgres_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var outboxSeq atomic.Int64

func uniqueOutboxID(prefix string) string {
	n := outboxSeq.Add(1)
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), n)
}

func mustOutboxEvent(id string, tenant claims.TenantID) postgres.OutboxEvent {
	return postgres.OutboxEvent{
		EventID:       id,
		Tenant:        tenant,
		AggregateType: "claim",
		AggregateID:   "claim-" + id,
		EventType:     "claim.transitioned",
		EventVersion:  "claim-transitioned.v1",
		Payload:       []byte(`{"event_id":"` + id + `","schema_version":"claim-transitioned.v1"}`),
		OccurredAt:    time.Now().UTC().Truncate(time.Millisecond),
	}
}

func beginOutboxAs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant claims.TenantID) (context.Context, pgx.Tx) {
	t.Helper()
	return beginAs(t, ctx, pool, tenant)
}

// Append + duplicate-swallow: the replay append returns nil and leaves one row.
func TestOutbox_AppendDuplicateSwallow(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := claims.TenantID("ob-t1-tenant")
	eventID := uniqueOutboxID("ob-e1")

	tctx, tx := beginOutboxAs(t, ctx, pool, tenant)
	if err := repo.AppendOutbox(tctx, tx, mustOutboxEvent(eventID, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendOutbox #1: %v", err)
	}
	commit(t, tctx, tx)

	// Replay in a fresh txn: savepoint swallows 23505 -> nil, txn commits.
	tctx, tx = beginOutboxAs(t, ctx, pool, tenant)
	if err := repo.AppendOutbox(tctx, tx, mustOutboxEvent(eventID, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendOutbox #2 (replay): %v", err)
	}
	commit(t, tctx, tx)

	tctx, tx = beginOutboxAs(t, ctx, pool, tenant)
	var n int
	if err := tx.QueryRow(tctx, `SELECT COUNT(*) FROM outbox_events WHERE event_id = $1`, eventID).Scan(&n); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("count outbox_events: %v", err)
	}
	if n != 1 {
		rollback(t, tctx, tx)
		t.Fatalf("event count = %d, want 1", n)
	}
	rollback(t, tctx, tx)
}

// Claim/mark round-trip: claim returns the due row, MarkPublished hides it,
// MarkFailed re-arms it with attempts+1 and a future next_attempt_at.
func TestOutbox_ClaimMarkRoundTrip(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := claims.TenantID("ob-t2-tenant")
	evOK := uniqueOutboxID("ob-ok")
	evFail := uniqueOutboxID("ob-fail")

	tctx, tx := beginOutboxAs(t, ctx, pool, tenant)
	if err := repo.AppendOutbox(tctx, tx, mustOutboxEvent(evOK, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendOutbox ok: %v", err)
	}
	if err := repo.AppendOutbox(tctx, tx, mustOutboxEvent(evFail, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendOutbox fail: %v", err)
	}
	commit(t, tctx, tx)

	// Claim both (same txn holds SKIP LOCKED rows, then commit releases).
	tctx, tx = beginOutboxAs(t, ctx, pool, tenant)
	got, err := repo.ClaimUnpublished(tctx, tx, 10)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("ClaimUnpublished: %v", err)
	}
	found := map[string]bool{}
	for _, e := range got {
		found[e.EventID] = true
	}
	if !found[evOK] || !found[evFail] {
		rollback(t, tctx, tx)
		t.Fatalf("claimed = %v, want both %s and %s", found, evOK, evFail)
	}
	commit(t, tctx, tx)

	// Publish ok -> MarkPublished; fail -> MarkFailed with past-due retry
	// so it is immediately reclaimable.
	tctx, tx = beginOutboxAs(t, ctx, pool, tenant)
	if err := repo.MarkPublished(tctx, tx, evOK); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("MarkPublished: %v", err)
	}
	if err := repo.MarkFailed(tctx, tx, evFail, "boom", time.Now().UTC().Add(-time.Minute)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("MarkFailed: %v", err)
	}
	commit(t, tctx, tx)

	tctx, tx = beginOutboxAs(t, ctx, pool, tenant)
	got, err = repo.ClaimUnpublished(tctx, tx, 10)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("ClaimUnpublished #2: %v", err)
	}
	found = map[string]bool{}
	for _, e := range got {
		found[e.EventID] = true
	}
	if found[evOK] {
		rollback(t, tctx, tx)
		t.Fatalf("published %s still claimable", evOK)
	}
	if !found[evFail] {
		rollback(t, tctx, tx)
		t.Fatalf("failed %s not reclaimable, got %v", evFail, found)
	}
	var attempts int
	var lastErr string
	if err := tx.QueryRow(tctx, `SELECT publish_attempts, last_error FROM outbox_events WHERE event_id = $1`, evFail).Scan(&attempts, &lastErr); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 1 || lastErr != "boom" {
		rollback(t, tctx, tx)
		t.Fatalf("attempts=%d lastErr=%q, want (1,boom)", attempts, lastErr)
	}
	rollback(t, tctx, tx)
}

// Cross-tenant invisibility: tenant B claims zero while A's row is pending.
func TestOutbox_CrossTenantInvisible(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenantA := claims.TenantID("ob-t3-tenant-a")
	tenantB := claims.TenantID("ob-t3-tenant-b")
	eventID := uniqueOutboxID("ob-x")

	tctx, tx := beginOutboxAs(t, ctx, pool, tenantA)
	if err := repo.AppendOutbox(tctx, tx, mustOutboxEvent(eventID, tenantA)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendOutbox A: %v", err)
	}
	commit(t, tctx, tx)

	bctx, btx := beginOutboxAs(t, ctx, pool, tenantB)
	got, err := repo.ClaimUnpublished(bctx, btx, 10)
	if err != nil {
		rollback(t, bctx, btx)
		t.Fatalf("ClaimUnpublished B: %v", err)
	}
	for _, e := range got {
		if e.EventID == eventID {
			rollback(t, bctx, btx)
			t.Fatalf("tenant B claimed A's event %s", eventID)
		}
	}
	rollback(t, bctx, btx)

	// Sanity: A still sees its own row (proves B's miss is RLS, not absence).
	actx, atx := beginOutboxAs(t, ctx, pool, tenantA)
	got, err = repo.ClaimUnpublished(actx, atx, 10)
	if err != nil {
		rollback(t, actx, atx)
		t.Fatalf("ClaimUnpublished A: %v", err)
	}
	found := false
	for _, e := range got {
		if e.EventID == eventID {
			found = true
		}
	}
	if !found {
		rollback(t, actx, atx)
		t.Fatalf("tenant A cannot claim own event %s", eventID)
	}
	rollback(t, actx, atx)
}
