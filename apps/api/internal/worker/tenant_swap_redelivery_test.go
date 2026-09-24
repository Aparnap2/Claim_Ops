package worker

// APA-28 RED: tenant-swapped redelivery must fail closed with a terminal
// REJECTION (permanent, non-recoverable for that delivery; never
// SUCCESS/DUPLICATE). No new outcome kind: TERMINAL is the rejection.
//
// Seam choice and boundary vocabulary (ruling 2 — `done` is an in-process
// processed-set, NOT durable work identity; the five boundaries are kept
// separate everywhere): (1) event identity = the (claim, document) IDs in
// the event envelope; (2) transport tenant binding = attrs-vs-bytes
// agreement at the app seam (internal/app/tenant_swap_redelivery_test.go
// proves it); (3) in-memory dedup = Processor.done, process-local
// short-lived optimization keyed by document ID with the deciding tenant
// bound per entry; (4) durable workflow identity = the tenant-hashed
// investigation ID plus the envelope/launch tables; (5) durable DB
// isolation = RLS plus tenant-partitioned blob keys (proven across
// restart in restart_tenant_swap_test.go, option B). THIS file proves the
// seam where (1)+(3) meet: once document X is decided under tenant A in
// this lifetime, a redelivery of the same IDs under tenant B must be an
// explicit TERMINAL rejection — never SUCCESS, never DUPLICATE (which
// would adopt tenant A's execution, envelope included) — with no new
// workflow launch and no store writes under either tenant.
//
// Inverse (same run): legitimate same-tenant redelivery of X under A
// still converges exactly as S6/F9 established (DUPLICATE, single
// launch, SUCCESS preserved).

import (
	"context"
	"strings"
	"sync"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"
)

// tswapStore wraps *dedupStore with per-tenant insert counters so the
// test can prove no cross-tenant DB mutation (writes under B) and no
// further writes under A on adversarial redelivery.
type tswapStore struct {
	*dedupStore
	mu                sync.Mutex
	docWritesByTenant map[string]int
	evWritesByTenant  map[string]int
}

func newTswapStore() *tswapStore {
	return &tswapStore{
		dedupStore:        newDedupStore(),
		docWritesByTenant: map[string]int{},
		evWritesByTenant:  map[string]int{},
	}
}

func (s *tswapStore) InsertDocument(ctx context.Context, d documents.Document) (bool, error) {
	s.mu.Lock()
	s.docWritesByTenant[string(d.Tenant)]++
	s.mu.Unlock()
	return s.dedupStore.InsertDocument(ctx, d)
}

func (s *tswapStore) InsertEvidence(ctx context.Context, e evidence.FieldEvidence) (bool, error) {
	s.mu.Lock()
	s.evWritesByTenant[string(e.Tenant)]++
	s.mu.Unlock()
	return s.dedupStore.InsertEvidence(ctx, e)
}

func (s *tswapStore) docWrites(tenant string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docWritesByTenant[tenant]
}

func (s *tswapStore) evWrites(tenant string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evWritesByTenant[tenant]
}

// tswapRig builds a launch-capable processor (low-OCR mock parser forces
// the exception-envelope + EnsureLaunched path) over tenant-counting
// fakes. Mirrors lowOCRProcessor but keeps the fetcher handle for
// tenant-visibility assertions.
func tswapRig(t *testing.T) (*Processor, *fakeFetcher, *tswapStore, *fakeLauncher) {
	t.Helper()
	fetch, _, loader, checker := happyFixture()
	store := newTswapStore()
	p := NewProcessor(fetch, store, loader, checker)
	p.Parser = mockOCRParser{conf: 0.70, name: "mock-ocr", version: "test"}
	fl := &fakeLauncher{}
	p.Launcher = fl
	return p, fetch, store, fl
}

func TestTenantSwapRedelivery_TerminalRejection(t *testing.T) {
	p, fetch, store, fl := tswapRig(t)
	ctxA := postgres.WithTenant(context.Background(), claims.TenantID("tA"))
	ctxB := postgres.WithTenant(context.Background(), claims.TenantID("tB"))
	evA := eventBytes(t, ports.DocumentIngestedSchemaVersion, "tA", "CLM-X", "doc-X")

	// Original delivery under tenant A: must succeed with exactly one launch.
	first := p.Handle(ctxA, evA)
	if first.Kind != OutcomeSuccess {
		t.Fatalf("original delivery kind = %q (%v), want SUCCESS (rig broken)", first.Kind, first.Err)
	}
	if first.ExceptionEnvelope == nil {
		t.Fatal("original delivery must carry an exception envelope (launch path armed)")
	}
	if calls, _, _, _, _ := fl.snapshot(); calls != 1 {
		t.Fatalf("launcher calls after original = %d, want 1", calls)
	}
	fetchCallsAfterA := fetch.callCount()
	docWritesAfterA := store.docWrites("tA") + store.docWrites("tB")

	// Adversarial redelivery: SAME event identity (same claim and
	// document IDs, boundary 1) but tenant B, delivered the way the pull
	// loop would (attrs tenant B on ctx, bytes tenant B in the event).
	// The in-memory dedup entry (boundary 3) binds doc-X to tenant A, so
	// this must be a terminal REJECTION (permanent for this delivery).
	evB := eventBytes(t, ports.DocumentIngestedSchemaVersion, "tB", "CLM-X", "doc-X")
	swap := p.Handle(ctxB, evB)

	if swap.Kind == OutcomeSuccess || swap.Kind == OutcomeDuplicate {
		t.Errorf("tenant-swapped redelivery kind = %q, want explicit TERMINAL rejection (never SUCCESS/DUPLICATE)", swap.Kind)
	}
	if swap.Kind != OutcomeTerminal {
		t.Errorf("tenant-swapped redelivery kind = %q, want TERMINAL", swap.Kind)
	}
	if swap.Err == nil {
		t.Error("tenant-swapped redelivery Err = nil, want explicit rejection error")
	} else if !strings.Contains(strings.ToLower(swap.Err.Error()), "tenant") {
		t.Errorf("tenant-swapped redelivery Err = %q, want a tenant-binding rejection", swap.Err.Error())
	}
	if swap.Duplicate {
		t.Error("tenant-swapped redelivery Duplicate = true: adopted tenant-A execution (fail-closed must not dedupe across tenants)")
	}
	if len(swap.ExceptionEnvelope) != 0 {
		t.Error("tenant-swapped redelivery adopted tenant-A exception envelope bytes")
	}
	if calls, _, _, _, _ := fl.snapshot(); calls != 1 {
		t.Errorf("launcher calls after swap = %d, want 1 (no workflow launch for tenant B)", calls)
	}
	if got := store.docWrites("tB"); got != 0 {
		t.Errorf("document writes under tenant B = %d, want 0 (no cross-tenant DB mutation)", got)
	}
	if got := store.evWrites("tB"); got != 0 {
		t.Errorf("evidence writes under tenant B = %d, want 0 (no cross-tenant DB mutation)", got)
	}
	if got := store.docWrites("tA") + store.docWrites("tB"); got != docWritesAfterA {
		t.Errorf("total document writes moved %d -> %d on swap, want frozen", docWritesAfterA, got)
	}
	if fetch.callCount() != fetchCallsAfterA {
		t.Errorf("fetch calls moved %d -> %d on swap, want no downstream IO after rejection", fetchCallsAfterA, fetch.callCount())
	}

	// Inverse (same run): legitimate same-tenant redelivery of X under A
	// still converges exactly as S6 established: DUPLICATE, single launch.
	again := p.Handle(ctxA, evA)
	if again.Kind != OutcomeDuplicate || !again.Duplicate {
		t.Errorf("same-tenant redelivery kind = %q duplicate = %v, want DUPLICATE/true (S6 convergence)", again.Kind, again.Duplicate)
	}
	if calls, _, _, _, _ := fl.snapshot(); calls != 1 {
		t.Errorf("launcher calls after same-tenant redelivery = %d, want 1 (exactly-once launch)", calls)
	}
	if got := store.docWrites("tA") + store.docWrites("tB"); got != docWritesAfterA {
		t.Errorf("total document writes moved %d -> %d on same-tenant redelivery, want frozen", docWritesAfterA, got)
	}
}
