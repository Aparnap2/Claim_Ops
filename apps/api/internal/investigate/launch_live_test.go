package investigate

// S6 live: PGLaunchStore round-trip + RLS isolation + full EnsureLaunched
// through PG stores. Gated on TEST_POSTGRES_DSN; requires migration 010
// applied (CI applies infra/postgres/migrations/*.sql; local: psql -f).

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func liveLaunchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := investigateLiveDSN(t)
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

// liveLaunchSeq mints identity for the live launch tests that is unique to this
// process, so a rerun against a volume that already holds a previous run's rows
// behaves exactly like a rerun against an empty one.
var liveLaunchSeq atomic.Int64

// uniqueLiveInvID returns a process-unique investigation_id in the shape
// invest.ValidateID requires: the "inv-" prefix plus exactly 32 hex chars
// (8 for the pid, 24 for the counter). Deterministic, no clock and no
// randomness.
//
// WHY THIS IS NEEDED. workflow_launches keys on a GLOBAL investigation_id
// primary key (not per tenant), and RecordLaunch is ON CONFLICT DO NOTHING —
// first write wins, inserted=false on conflict. Migration 010 also revokes
// UPDATE and DELETE from claimops_app, so a live test cannot reuse or remove a
// key it already wrote: with a fixed id the very first run inserted the row and
// every later run converged, so "first record must insert" and "first
// EnsureLaunched launches" could only hold against a pristine volume. Same
// isolate-by-unique-identity rule the postgres repository tests follow, since
// neither table grants the app role a way to clean up after itself.
func uniqueLiveInvID() string {
	return fmt.Sprintf("inv-%08x%024x", os.Getpid(), liveLaunchSeq.Add(1))
}

// uniqueLiveTenant returns a process-unique tenant ID, so one run's launch rows
// can never be visible under another run's RLS scope.
func uniqueLiveTenant(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), liveLaunchSeq.Add(1))
}

func TestLaunchLive_RecordGet_Idempotent(t *testing.T) {
	pool := liveLaunchPool(t)
	store := NewPGLaunchStore(pool)
	ctx := context.Background()
	// Unique per run: this key is a global primary key that no test may reuse
	// (see uniqueLiveInvID), so "first record must insert" is a statement about
	// idempotency, not about whether an earlier run got there first.
	tenant := uniqueLiveTenant("tnt-s6-live")
	invID := uniqueLiveInvID()
	rec := LaunchRecord{TenantID: tenant, ClaimID: "clm-s6-live", InvestigationID: invID, WorkflowID: "claim-investigation", ExecutionName: "exec-live-1"}

	inserted, err := store.RecordLaunch(ctx, rec)
	if err != nil {
		t.Fatalf("RecordLaunch: %v", err)
	}
	if !inserted {
		t.Fatal("first record must insert (a per-run investigation_id must not already exist)")
	}
	again, err := store.RecordLaunch(ctx, rec)
	if err != nil {
		t.Fatalf("second RecordLaunch: %v", err)
	}
	if again {
		t.Fatal("second record must converge (inserted=false), not duplicate")
	}
	got, found, err := store.GetLaunch(ctx, tenant, invID)
	if err != nil || !found {
		t.Fatalf("GetLaunch: found=%v err=%v", found, err)
	}
	if got.ExecutionName != "exec-live-1" {
		t.Fatalf("execution = %q, want exec-live-1 (first wins)", got.ExecutionName)
	}
	if got.TenantID != tenant {
		t.Fatalf("tenant = %q, want %q", got.TenantID, tenant)
	}
}

func TestLaunchLive_TenantIsolation(t *testing.T) {
	pool := liveLaunchPool(t)
	store := NewPGLaunchStore(pool)
	ctx := context.Background()
	tenantA := uniqueLiveTenant("tnt-s6-live-a")
	tenantB := uniqueLiveTenant("tnt-s6-live-b")
	invID := uniqueLiveInvID()
	rec := LaunchRecord{TenantID: tenantA, ClaimID: "clm-s6-live", InvestigationID: invID, WorkflowID: "claim-investigation", ExecutionName: "exec-live-a"}

	inserted, err := store.RecordLaunch(ctx, rec)
	if err != nil {
		t.Fatalf("RecordLaunch: %v", err)
	}
	if !inserted {
		t.Fatal("first record must insert (a per-run investigation_id must not already exist)")
	}
	// Non-vacuity: A really can see the row it just wrote, so the miss below is
	// tenant scoping rather than the row being absent.
	if _, found, err := store.GetLaunch(ctx, tenantA, invID); err != nil || !found {
		t.Fatalf("same-tenant GetLaunch: found=%v err=%v, want hit", found, err)
	}
	if _, found, err := store.GetLaunch(ctx, tenantB, invID); err != nil || found {
		t.Fatalf("cross-tenant GetLaunch: found=%v err=%v, want miss", found, err)
	}
}

func TestLaunchLive_EnsureLaunched_EndToEnd(t *testing.T) {
	pool := liveLaunchPool(t)
	ctx := context.Background()
	tenant := uniqueLiveTenant("tnt-s6-live")
	invID := uniqueLiveInvID()
	env := launchTestEnv(t, tenant, "clm-s6-live", invID)
	prov := &fakeProvider{}
	l := NewLauncher(NewPGEnvelopeStore(pool), prov, NewPGLaunchStore(pool))

	name, launched, err := l.EnsureLaunched(ctx, tenant, "clm-s6-live", env.InvestigationID, env, "claim-investigation", ExpireAuth{})
	if err != nil || !launched || name == "" {
		t.Fatalf("EnsureLaunched: launched=%v name=%q err=%v", launched, name, err)
	}
	// Redelivery converges with zero new provider calls (durable state).
	name2, launched2, err := l.EnsureLaunched(ctx, tenant, "clm-s6-live", env.InvestigationID, env, "claim-investigation", ExpireAuth{})
	if err != nil || launched2 || name2 != name {
		t.Fatalf("redelivery: launched=%v name=%q err=%v, want converge %q", launched2, name2, err, name)
	}
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.callCount())
	}
}
