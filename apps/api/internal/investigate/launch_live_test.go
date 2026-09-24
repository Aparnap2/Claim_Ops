package investigate

// S6 live: PGLaunchStore round-trip + RLS isolation + full EnsureLaunched
// through PG stores. Gated on TEST_POSTGRES_DSN; requires migration 010
// applied (CI applies infra/postgres/migrations/*.sql; local: psql -f).

import (
	"context"
	"fmt"
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

func TestLaunchLive_RecordGet_Idempotent(t *testing.T) {
	pool := liveLaunchPool(t)
	store := NewPGLaunchStore(pool)
	ctx := context.Background()
	invID := fmt.Sprintf("inv-%032d", 61001)
	rec := LaunchRecord{TenantID: "tnt-s6-live", ClaimID: "clm-s6-live", InvestigationID: invID, WorkflowID: "claim-investigation", ExecutionName: "exec-live-1"}

	inserted, err := store.RecordLaunch(ctx, rec)
	if err != nil {
		t.Fatalf("RecordLaunch: %v", err)
	}
	if !inserted {
		t.Fatal("first record must insert (clean volume required for this assertion)")
	}
	again, err := store.RecordLaunch(ctx, rec)
	if err != nil {
		t.Fatalf("second RecordLaunch: %v", err)
	}
	if again {
		t.Fatal("second record must converge (inserted=false), not duplicate")
	}
	got, found, err := store.GetLaunch(ctx, "tnt-s6-live", invID)
	if err != nil || !found {
		t.Fatalf("GetLaunch: found=%v err=%v", found, err)
	}
	if got.ExecutionName != "exec-live-1" {
		t.Fatalf("execution = %q, want exec-live-1 (first wins)", got.ExecutionName)
	}
}

func TestLaunchLive_TenantIsolation(t *testing.T) {
	pool := liveLaunchPool(t)
	store := NewPGLaunchStore(pool)
	ctx := context.Background()
	invID := fmt.Sprintf("inv-%032d", 61002)
	rec := LaunchRecord{TenantID: "tnt-s6-live-a", ClaimID: "clm-s6-live", InvestigationID: invID, WorkflowID: "claim-investigation", ExecutionName: "exec-live-a"}

	if _, err := store.RecordLaunch(ctx, rec); err != nil {
		t.Fatalf("RecordLaunch: %v", err)
	}
	if _, found, err := store.GetLaunch(ctx, "tnt-s6-live-b", invID); err != nil || found {
		t.Fatalf("cross-tenant GetLaunch: found=%v err=%v, want miss", found, err)
	}
}

func TestLaunchLive_EnsureLaunched_EndToEnd(t *testing.T) {
	pool := liveLaunchPool(t)
	ctx := context.Background()
	env := launchTestEnv(t, "tnt-s6-live", "clm-s6-live", fmt.Sprintf("inv-%032d", 61003))
	prov := &fakeProvider{}
	l := NewLauncher(NewPGEnvelopeStore(pool), prov, NewPGLaunchStore(pool))

	name, launched, err := l.EnsureLaunched(ctx, "tnt-s6-live", "clm-s6-live", env.InvestigationID, env, "claim-investigation")
	if err != nil || !launched || name == "" {
		t.Fatalf("EnsureLaunched: launched=%v name=%q err=%v", launched, name, err)
	}
	// Redelivery converges with zero new provider calls (durable state).
	name2, launched2, err := l.EnsureLaunched(ctx, "tnt-s6-live", "clm-s6-live", env.InvestigationID, env, "claim-investigation")
	if err != nil || launched2 || name2 != name {
		t.Fatalf("redelivery: launched=%v name=%q err=%v, want converge %q", launched2, name2, err, name)
	}
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.callCount())
	}
}
