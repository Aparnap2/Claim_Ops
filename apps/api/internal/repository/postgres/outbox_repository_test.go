package postgres_test

import (
	"context"
	"fmt"
	"os"
	"slices"
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

// uniqueOutboxTenant returns a tenant ID unique to this test within this
// process: the pid-scoped idiom already used by hitl_test.go and
// hitl_expiry_test.go.
//
// ISOLATION IS BY UNIQUE IDENTITY, NOT BY CLEANUP. Migration 005 grants
// claimops_app no DELETE on outbox_events ("published rows age out via a future
// retention job, never via the app role"), so a test cannot remove the rows it
// wrote and must not try — the volume is shared and its contents are evidence.
// A run-unique tenant is therefore the only scoping available, and it is the
// one this package already settled on.
//
// WHY THE TENANT, SPECIFICALLY, IS THE ISOLATION (the defect these tests had).
// ClaimUnpublished is RLS-scoped to the ctx tenant and returns
// `ORDER BY created_at ASC LIMIT n`. Given a tenant shared by every run ever
// pointed at the volume, each run's claim window is occupied by the OLDER rows
// earlier runs left behind, so the rows this run just seeded sort last and fall
// outside LIMIT. Asserting that the seeded rows appear in that result set
// therefore held only against a pristine volume. With a run-unique tenant the
// visible row set is exactly the rows this run seeded, whatever else the
// volume holds — so every claim below is compared against the full returned set
// and equality, never membership on a filtered slice.
func uniqueOutboxTenant(prefix string) claims.TenantID {
	return claims.TenantID("tnt-ob-" + string(uniqueClaimID(prefix)))
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

// idsOf returns every event ID in a claim result, sorted. It reads the result
// exactly as the query returned it and never filters, so an assertion built on
// it is a statement about the WHOLE result set: a claim that over-reaches
// (foreign rows, or stale rows from an earlier run) cannot be hidden by
// narrowing the slice to the IDs the test happens to own.
func idsOf(got []postgres.OutboxEvent) []string {
	ids := make([]string, 0, len(got))
	for _, e := range got {
		ids = append(ids, e.EventID)
	}
	slices.Sort(ids)
	return ids
}

// sortedIDs returns a sorted copy of ids, for expressing an expected claim set
// without depending on the lexicographic order of the generated IDs.
func sortedIDs(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

// countClaimable returns how many rows this tenant can claim right now. Run
// inside a tenant-scoped transaction, so RLS has already hidden every other
// tenant: the count is that tenant's own claimable set. Asserted as a
// precondition by tests that then prove those rows were EXCLUDED, so the
// exclusion is demonstrably an exclusion of present, due data rather than a
// vacuous pass over an empty result.
func countClaimable(t *testing.T, ctx context.Context, tx pgx.Tx) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx, `
SELECT count(*)
  FROM outbox_events
 WHERE published_at IS NULL
   AND next_attempt_at <= now()`).Scan(&n); err != nil {
		t.Fatalf("count claimable: %v", err)
	}
	return n
}

// Append + duplicate-swallow: the replay append returns nil and leaves one row.
func TestOutbox_AppendDuplicateSwallow(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := uniqueOutboxTenant("t1")
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

// Claim/mark round-trip: claim returns exactly the seeded rows, MarkPublished
// hides one, MarkFailed re-arms the other with attempts+1 and a future
// next_attempt_at.
//
// The claim window is LIMIT 10 and this run's tenant holds exactly two rows, so
// the comparison is against the entire returned set: had any row from another
// tenant or an earlier run been visible, it would displace one of these and the
// equality would fail. That is what makes the assertion bite rather than merely
// "the rows I own are somewhere in there".
func TestOutbox_ClaimMarkRoundTrip(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := uniqueOutboxTenant("t2")
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
	claimed := idsOf(got)
	if !slices.Equal(claimed, sortedIDs(evOK, evFail)) {
		rollback(t, tctx, tx)
		t.Fatalf("claimed = %v, want exactly [%s %s]", claimed, evOK, evFail)
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

	// Re-claim: published_at hides the published row, the re-armed row returns.
	// Exactly one row, so this asserts both the hide AND the re-arm.
	tctx, tx = beginOutboxAs(t, ctx, pool, tenant)
	got, err = repo.ClaimUnpublished(tctx, tx, 10)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("ClaimUnpublished #2: %v", err)
	}
	claimed = idsOf(got)
	if slices.Contains(claimed, evOK) {
		rollback(t, tctx, tx)
		t.Fatalf("published %s still claimable, claimed = %v", evOK, claimed)
	}
	if !slices.Equal(claimed, sortedIDs(evFail)) {
		rollback(t, tctx, tx)
		t.Fatalf("reclaimed = %v, want exactly [%s]", claimed, evFail)
	}
	// Backoff state actually landed: attempts incremented, error recorded, and
	// next_attempt_at set into the past so the row is genuinely due again.
	var attempts int
	var lastErr string
	var nextAttempt, publishedAt *time.Time
	if err := tx.QueryRow(tctx, `
SELECT publish_attempts, last_error, next_attempt_at, published_at
  FROM outbox_events
 WHERE event_id = $1`, evFail).Scan(&attempts, &lastErr, &nextAttempt, &publishedAt); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 1 || lastErr != "boom" {
		rollback(t, tctx, tx)
		t.Fatalf("attempts=%d lastErr=%q, want (1,boom)", attempts, lastErr)
	}
	if nextAttempt == nil || nextAttempt.After(time.Now().UTC()) {
		rollback(t, tctx, tx)
		t.Fatalf("next_attempt_at = %v, want a past instant (due now)", nextAttempt)
	}
	// MarkPublished set only published_at: the published row carries it, the
	// re-armed row stays NULL so it is claimable again.
	var publishedUnset, failedUnset bool
	if err := tx.QueryRow(tctx, `
SELECT published_at IS NULL
  FROM outbox_events
 WHERE event_id = $1`, evOK).Scan(&publishedUnset); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("read published_at (ok): %v", err)
	}
	if publishedUnset {
		rollback(t, tctx, tx)
		t.Fatalf("MarkPublished left published_at NULL on %s", evOK)
	}
	if err := tx.QueryRow(tctx, `
SELECT published_at IS NULL
  FROM outbox_events
 WHERE event_id = $1`, evFail).Scan(&failedUnset); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("read published_at (fail): %v", err)
	}
	if !failedUnset {
		rollback(t, tctx, tx)
		t.Fatalf("MarkFailed published %s; only published_at/mark columns may change and the row must stay claimable", evFail)
	}
	rollback(t, tctx, tx)
}

// Cross-tenant invisibility: tenant B's claim returns exactly B's own row and
// NOT A's, and A's claim returns exactly A's own row.
//
// Both legs are pinned to an exact set on a NON-EMPTY result, which is what
// keeps the test from passing vacuously: "B did not see A" would also hold if
// B saw nothing at all, so B is given a row of its own and must find it. A
// tenant scope that stopped scoping would hand B A's row as well and break
// B's equality immediately.
func TestOutbox_CrossTenantInvisible(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenantA := uniqueOutboxTenant("t3-a")
	tenantB := uniqueOutboxTenant("t3-b")
	eventA := uniqueOutboxID("ob-x-a")
	eventB := uniqueOutboxID("ob-x-b")

	tctx, tx := beginOutboxAs(t, ctx, pool, tenantA)
	if err := repo.AppendOutbox(tctx, tx, mustOutboxEvent(eventA, tenantA)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendOutbox A: %v", err)
	}
	commit(t, tctx, tx)

	// B owns a row too, so B's claim below cannot pass by finding nothing.
	tctx, tx = beginOutboxAs(t, ctx, pool, tenantB)
	if err := repo.AppendOutbox(tctx, tx, mustOutboxEvent(eventB, tenantB)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("AppendOutbox B: %v", err)
	}
	commit(t, tctx, tx)

	// Precondition: A's row is present and due as A, so the miss below is
	// exclusion and not absence.
	actx, atx := beginOutboxAs(t, ctx, pool, tenantA)
	if n := countClaimable(t, actx, atx); n != 1 {
		rollback(t, actx, atx)
		t.Fatalf("A claimable rows = %d, want exactly 1 (A's own seeded row)", n)
	}
	rollback(t, actx, atx)

	bctx, btx := beginOutboxAs(t, ctx, pool, tenantB)
	got, err := repo.ClaimUnpublished(bctx, btx, 10)
	if err != nil {
		rollback(t, bctx, btx)
		t.Fatalf("ClaimUnpublished B: %v", err)
	}
	claimedB := idsOf(got)
	if slices.Contains(claimedB, eventA) {
		rollback(t, bctx, btx)
		t.Fatalf("tenant B claimed A's event %s, claimed = %v", eventA, claimedB)
	}
	if !slices.Equal(claimedB, sortedIDs(eventB)) {
		rollback(t, bctx, btx)
		t.Fatalf("tenant B claimed %v, want exactly [%s] (own row only)", claimedB, eventB)
	}
	rollback(t, bctx, btx)

	// Sanity: A still sees its own row (proves B's miss is RLS, not absence).
	actx, atx = beginOutboxAs(t, ctx, pool, tenantA)
	got, err = repo.ClaimUnpublished(actx, atx, 10)
	if err != nil {
		rollback(t, actx, atx)
		t.Fatalf("ClaimUnpublished A: %v", err)
	}
	claimedA := idsOf(got)
	if slices.Contains(claimedA, eventB) {
		rollback(t, actx, atx)
		t.Fatalf("tenant A claimed B's event %s, claimed = %v", eventB, claimedA)
	}
	if !slices.Equal(claimedA, sortedIDs(eventA)) {
		rollback(t, actx, atx)
		t.Fatalf("tenant A claimed %v, want exactly [%s] (own row only)", claimedA, eventA)
	}
	rollback(t, actx, atx)
}

// ANTI-VACUITY GUARD for the run-unique tenant scoping above.
//
// Claim scoping must exclude rows that are genuinely present and genuinely due,
// not merely return an empty set because nothing is there. This seeds rows for
// two foreign tenants, proves under each foreign tenant's OWN scope that its
// rows really are claimable at this instant, and only then asserts our claim
// returns exactly our own rows with every foreign ID absent.
//
// It fails if the isolation is faked:
//   - scopes collapse (our tenant equals a foreign tenant, or the claim stops
//     being tenant-scoped): the foreign rows come back and equality fails, and
//     the foreign claims come back with our rows in them.
//   - the assertion is weakened to membership on owned IDs: this test never
//     filters the result, so a leaked row still breaks equality.
//
// The claim limit is far above the seeded row count, so the verdict does not
// depend on created_at ordering or on which rows happen to fill the
// ORDER BY window — any leak at all is detected, whatever its position.
func TestOutbox_ClaimScopeExcludesPresentForeignRows(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()

	tenant := uniqueOutboxTenant("scope-own")
	tenantY := uniqueOutboxTenant("scope-y")
	tenantZ := uniqueOutboxTenant("scope-z")

	own := sortedIDs(uniqueOutboxID("ob-own"), uniqueOutboxID("ob-own"))
	foreignY := make([]string, 0, 12)
	for range 12 {
		foreignY = append(foreignY, uniqueOutboxID("ob-fy"))
	}
	foreignZ := sortedIDs(uniqueOutboxID("ob-fz"), uniqueOutboxID("ob-fz"))

	// Seed in dependency order. Foreign tenants are written in EARLIER
	// committed transactions, so their created_at strictly precedes ours: a
	// leaked scope would then also be the scope filling the front of the claim
	// window, which is the starvation mode these tests originally had.
	seed := func(tenant claims.TenantID, ids []string) {
		t.Helper()
		tctx, tx := beginOutboxAs(t, ctx, pool, tenant)
		for _, id := range ids {
			if err := repo.AppendOutbox(tctx, tx, mustOutboxEvent(id, tenant)); err != nil {
				rollback(t, tctx, tx)
				t.Fatalf("AppendOutbox %s: %v", id, err)
			}
		}
		commit(t, tctx, tx)
	}
	seed(tenantY, foreignY)
	seed(tenantZ, foreignZ)
	seed(tenant, own)

	// Non-vacuity precondition: each foreign tenant's rows really are claimable
	// right now. Asserted in the foreign tenant's own scope, so it is a fact
	// about the data, not about our view of it.
	for _, foreign := range []struct {
		tenant claims.TenantID
		want   int
	}{
		{tenantY, len(foreignY)},
		{tenantZ, len(foreignZ)},
	} {
		tctx, tx := beginOutboxAs(t, ctx, pool, foreign.tenant)
		if n := countClaimable(t, tctx, tx); n != foreign.want {
			rollback(t, tctx, tx)
			t.Fatalf("foreign tenant %s claimable = %d, want %d (precondition: the rows this test proves excluded must exist)", foreign.tenant, n, foreign.want)
		}
		rollback(t, tctx, tx)
	}

	// Our claim: exactly our rows. All 16 seeded rows are due, and the limit
	// admits every one of them, so a single leaked foreign row breaks this.
	tctx, tx := beginOutboxAs(t, ctx, pool, tenant)
	got, err := repo.ClaimUnpublished(tctx, tx, 50)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("ClaimUnpublished own tenant: %v", err)
	}
	claimed := idsOf(got)
	if !slices.Equal(claimed, own) {
		leaked := foreignY
		leaked = append(leaked, foreignZ...)
		for _, id := range claimed {
			if slices.Contains(own, id) {
				continue
			}
			t.Errorf("own tenant claim leaked foreign row %s", id)
		}
		rollback(t, tctx, tx)
		t.Fatalf("own tenant claimed %d rows, want exactly %d (own only); %d foreign rows were due and present at claim time",
			len(claimed), len(own), len(leaked))
	}
	rollback(t, tctx, tx)

	// Mirror direction: the foreign tenants' claims exclude our rows too, so
	// the guarantee is mutual rather than an artefact of one query.
	for _, foreign := range []struct {
		tenant claims.TenantID
		want   []string
	}{
		{tenantY, sortedIDs(foreignY...)},
		{tenantZ, foreignZ},
	} {
		tctx, tx := beginOutboxAs(t, ctx, pool, foreign.tenant)
		got, err := repo.ClaimUnpublished(tctx, tx, 50)
		if err != nil {
			rollback(t, tctx, tx)
			t.Fatalf("ClaimUnpublished %s: %v", foreign.tenant, err)
		}
		claimed = idsOf(got)
		if !slices.Equal(claimed, foreign.want) {
			rollback(t, tctx, tx)
			t.Fatalf("tenant %s claimed %d rows, want exactly its own %d", foreign.tenant, len(claimed), len(foreign.want))
		}
		rollback(t, tctx, tx)
	}
}
