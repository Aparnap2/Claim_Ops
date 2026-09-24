package worker

// APA-28 restart adversary: the tenant-binding invariant must survive
// processor restart.
//
// Why this file exists: the same-run swap proof
// (tenant_swap_redelivery_test.go) rests on the in-memory processed-set
// (`done`), which is process-local by design (migration/backfill NONE —
// see the `done` field comment). After a restart `done` is empty, so the
// honest question is which DURABLE authority rejects tenant B's redelivery
// of tenant A's doc-X. The doubles below replicate the production durable
// boundary faithfully (never the tenancy-ignoring rig of the same-run
// proof), and the comments cite the authority chain file:line:
//
//  1. Fetch metadata resolves through a tenant-scoped listing. Production:
//     BlobFetchBridge.Fetch lists via the RLS-scoped store and misses when
//     the document belongs to another tenant ("unknown document")
//     (internal/adapters/worker/blob_fetch_bridge.go:59-73); lists require
//     the ctx tenant and run inside BeginTenantTx
//     (internal/adapters/worker/store_bridge.go:64-79).
//  2. RLS/FORCE RLS + tenant_isolation policy on documents, field_evidence,
//     and claims: another tenant's rows are simply absent
//     (infra/postgres/migrations/004_documents.sql:62-76,
//     infra/postgres/migrations/001_init.sql:71-109;
//     internal/repository/postgres/claim_repository.go:100-102,
//     internal/repository/postgres/document_repository.go:55-59).
//  3. The blob object key itself is tenant-partitioned
//     (ports.DocumentObjectKey), so B's key misses even if metadata existed
//     (internal/adapters/worker/blob_fetch_bridge.go:74).
//  4. The investigation identity hashes the tenant in
//     (internal/worker/launch.go:34-40): B derives a DIFFERENT
//     investigation ID, so B can never address (adopt) A's execution —
//     launch convergence is per-(tenant, claim, document), never global.
//  5. Fetch runs BEFORE every write and before launch
//     (internal/worker/processor.go:462-486 Tier-1,
//     runNewPipeline fetch-first): a fetch miss means zero downstream IO.
//
// Option-B verdict (owner's taxonomy): `done` is explicitly a short-lived
// in-process optimization (same-run redelivery dedupe); cross-tenant
// safety after restart comes from the durable tenant boundary above,
// which rejects B BEFORE any store write or workflow launch. What the
// boundary yields is TRANSIENT, not TERMINAL: an unknown document under
// this tenant's scope is the fetch contract's retryable miss
// (internal/adapters/worker/blob_fetch_bridge.go:38-43 "redelivery
// retries") and S3 owns unknown-document as transient (frozen
// retry_contract_test.go). Promoting it to TERMINAL would also terminate
// genuine first-delivery ingest races (event arrives before the blob row
// commits) — retry-semantics vandalism the owner fenced off, so the test
// asserts TRANSIENT explicitly and the mismatch is adjudicated here, not
// hidden. Fail-closed is preserved either way: TRANSIENT never ACKs as
// success (pull: non-nil error → Nack, internal/app/worker.go:53-60;
// push: 503, internal/app/pushevents.go:109-110), never writes, never
// launches, never adopts.
//
// Five-boundary terminology used here (ruling 2): (1) event identity =
// the (claim, document) IDs in the event envelope; (2) transport tenant
// binding = attrs-vs-bytes agreement at the app seam; (3) in-memory dedup
// = `done`, process-local only; (4) durable workflow identity = the
// tenant-hashed investigation ID + envelope/launch tables; (5) durable DB
// isolation = RLS + tenant-partitioned blob keys (+ the global documents
// PK backstop, infra/postgres/migrations/004_documents.sql:23, unreachable
// behind fetch-first ordering).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"
)

// restartFetch replicates BlobFetchBridge over an RLS-scoped store: blob
// metadata resolves through a (tenant, claim, document) registry, and a
// miss under the requesting tenant's scope is a plain transient miss —
// byte-identical in kind to "blobfetch: unknown document"
// (blob_fetch_bridge.go:71-73). Tenancy-ignoring fakes would let B adopt
// A's bytes; this one cannot.
type restartFetch struct {
	mu       sync.Mutex
	blobs    map[string]restartBlob // key: tenant|claim|doc
	calls    int
	seenTen  []string
	seenDocs []string
}

// restartBlob is one tenant-scoped blob: serving triple + recorded hash.
type restartBlob struct {
	fileName, mime, content, sha string
}

func restartKey(tenant, claim, doc string) string {
	return tenant + "|" + claim + "|" + doc
}

func (f *restartFetch) Fetch(_ context.Context, tenant, claimID, docID string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seenTen = append(f.seenTen, tenant)
	f.seenDocs = append(f.seenDocs, docID)
	b, ok := f.blobs[restartKey(tenant, claimID, docID)]
	if !ok {
		return "", "", "", fmt.Errorf("restartfetch: unknown document %q under tenant %q", docID, tenant)
	}
	return b.fileName, b.mime, b.content, nil
}

func (f *restartFetch) stats() (calls int, tenants []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]string(nil), f.seenTen...)
}

// restartClaims replicates ClaimBridge over RLS: only (tenant, claim)
// pairs owned by that tenant resolve; anything else is absent, mirroring
// LoadClaim → workflow.ErrNotFound under another tenant's scope
// (claim_bridge.go:29-36, claim_repository.go:138-141).
type restartClaims struct {
	mu    sync.Mutex
	views map[string]ClaimView // key: tenant|claim
	calls int
}

func (c *restartClaims) LoadClaim(_ context.Context, tenant, claimID string) (ClaimView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	v, ok := c.views[tenant+"|"+claimID]
	if !ok {
		return ClaimView{}, fmt.Errorf("restartclaims: claim %q absent under tenant %q", claimID, tenant)
	}
	return v, nil
}

// restartPolicy is a static upstream stub that records the tenant it was
// consulted under (the Mockoon policy source is not a tenant-isolation
// boundary; it rides after fetch + claim load, both of which reject B).
type restartPolicy struct {
	mu      sync.Mutex
	data    PolicyData
	tenants []string
}

func (p *restartPolicy) CheckPolicy(_ context.Context, tenant, _ string) (PolicyData, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tenants = append(p.tenants, tenant)
	return p.data, nil
}

// restartStore embeds the production-faithful dedupStore (ON CONFLICT DO
// NOTHING incl. the tenant-namespaced unique key,
// retry_contract_test.go:53-68) and adds per-tenant applied counters so
// the test proves zero durable mutation under B and frozen rows on the
// same-tenant inverse. NOTE: production ALSO carries a global
// documents.id PRIMARY KEY (004_documents.sql:23) whose violation would
// surface as class-23 TERMINAL; it is unreachable behind fetch-first
// ordering, so this double (like dedupStore) does not replicate it —
// deliberately, per "do not fabricate".
type restartStore struct {
	*dedupStore
	mu              sync.Mutex
	docAppliedByTen map[string]int
	evAppliedByTen  map[string]int
}

func newRestartStore() *restartStore {
	return &restartStore{dedupStore: newDedupStore(), docAppliedByTen: map[string]int{}, evAppliedByTen: map[string]int{}}
}

func (s *restartStore) InsertDocument(ctx context.Context, d documents.Document) (bool, error) {
	inserted, err := s.dedupStore.InsertDocument(ctx, d)
	if err == nil && inserted {
		s.mu.Lock()
		s.docAppliedByTen[string(d.Tenant)]++
		s.mu.Unlock()
	}
	return inserted, err
}

func (s *restartStore) InsertEvidence(ctx context.Context, e evidence.FieldEvidence) (bool, error) {
	inserted, err := s.dedupStore.InsertEvidence(ctx, e)
	if err == nil && inserted {
		s.mu.Lock()
		s.evAppliedByTen[string(e.Tenant)]++
		s.mu.Unlock()
	}
	return inserted, err
}

func (s *restartStore) applied(tenant string) (docs, ev int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docAppliedByTen[tenant], s.evAppliedByTen[tenant]
}

// restartLauncher replicates investigate.Launcher.EnsureLaunched's durable
// memory: executions are keyed by investigation ID and survive processor
// restarts (the double outlives any Processor, like the
// workflow_launches table, 010_workflow_launches.sql:14-15). A repeat
// EnsureLaunched for a known ID converges (launched=false, same execution
// name — launch_red_test.go:231 "already-launched"); an unknown ID mints
// a new execution. Tenant is recorded per execution to prove B never
// gains one.
type restartLauncher struct {
	mu         sync.Mutex
	execs      map[string]string // invID -> execution name
	execTenant map[string]string // invID -> tenant
	calls      int
	n          int
}

func (l *restartLauncher) EnsureLaunched(_ context.Context, tenantID, claimID, investigationID string, env invest.UnresolvedException, workflowID string, expire investigate.ExpireAuth) (string, bool, error) {
	_, _ = claimID, workflowID
	_, _ = env, expire
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if exec, ok := l.execs[investigationID]; ok {
		return exec, false, nil
	}
	l.n++
	exec := fmt.Sprintf("exec-restart-%d", l.n)
	if l.execs == nil {
		l.execs = map[string]string{}
		l.execTenant = map[string]string{}
	}
	l.execs[investigationID] = exec
	l.execTenant[investigationID] = tenantID
	return exec, true, nil
}

func (l *restartLauncher) snapshot() (calls, executions int, tenants map[string]string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]string{}
	for k, v := range l.execTenant {
		out[k] = v
	}
	return l.calls, len(l.execs), out
}

// restartRig wires a launch-capable (low-OCR envelope + launcher) rig over
// durables seeded for tenant A ONLY. The (tA, CLM-X, doc-X) triple mirrors
// the happyFixture content so A's original delivery converges with zero
// plumbing surprises; B owns nothing anywhere.
func restartRig(t *testing.T) (*restartFetch, *restartStore, *restartClaims, *restartPolicy, *restartLauncher) {
	t.Helper()
	fetch := &restartFetch{blobs: map[string]restartBlob{
		restartKey("tA", "CLM-X", "doc-X"): {
			fileName: "claim_form.pdf",
			mime:     "application/pdf",
			content:  "patient_name: Alice\npolicy_number: POL-1\n",
			sha:      "abc123",
		},
	}}
	store := newRestartStore()
	claimsDB := &restartClaims{views: map[string]ClaimView{
		"tA|CLM-X": {PolicyNumber: "POL-1", PatientName: "Alice", HospitalName: "City Hospital", ClaimedPaise: 5000},
	}}
	policy := &restartPolicy{data: PolicyData{Number: "POL-1", Patient: "Alice", Active: true}}
	launcher := &restartLauncher{}
	return fetch, store, claimsDB, policy, launcher
}

// restartProcessor builds a launch-capable Processor over shared durables.
// Each call is a new processor lifetime (empty in-memory dedup `done`);
// the durables (fetch/claims/policy registries, store rows, launcher
// execution memory) are the only state that survives.
func restartProcessor(fetch *restartFetch, store *restartStore, claimsDB *restartClaims, policy *restartPolicy, launcher *restartLauncher) *Processor {
	p := NewProcessor(fetch, store, claimsDB, policy)
	p.Parser = mockOCRParser{conf: 0.70, name: "mock-ocr", version: "test"}
	p.Launcher = launcher
	return p
}

func restartEvent(t *testing.T, tenant string) []byte {
	t.Helper()
	return eventBytes(t, ports.DocumentIngestedSchemaVersion, tenant, "CLM-X", "doc-X")
}

// TestRestartTenantSwap_DurableBoundaryRejectsFailClosed proves the
// tenant-binding invariant survives processor restart WITHOUT the
// in-memory `done` (dropped with the old instance): tenant B's redelivery
// of A's doc-X is rejected by the durable tenant boundary BEFORE any
// store write or workflow launch.
//
// Fail-closed here is TRANSIENT-retryable, never SUCCESS/DUPLICATE, never
// a false ACK: the tenant-scoped fetch misses under B's scope (authority
// 1), so the pipeline stops before persistence/launch (authority 5) and
// the transport redelivers (Nack/503) rather than adopting. See the file
// header for the TERMINAL adjudication (fetch-miss owns transient per the
// frozen S3 retry contract; TERMINAL would poison genuine ingest races).
func TestRestartTenantSwap_DurableBoundaryRejectsFailClosed(t *testing.T) {
	fetch, store, claimsDB, policy, launcher := restartRig(t)
	ctxA := postgres.WithTenant(context.Background(), claims.TenantID("tA"))
	ctxB := postgres.WithTenant(context.Background(), claims.TenantID("tB"))

	// Tenant A processes doc-X to completion in processor lifetime 1.
	p1 := restartProcessor(fetch, store, claimsDB, policy, launcher)
	first := p1.Handle(ctxA, restartEvent(t, "tA"))
	if first.Kind != OutcomeSuccess {
		t.Fatalf("A original kind = %q (%v), want SUCCESS (rig broken)", first.Kind, first.Err)
	}
	if first.ExceptionEnvelope == nil {
		t.Fatal("A original must carry an exception envelope (launch path armed)")
	}
	calls, execs, _ := launcher.snapshot()
	if execs != 1 {
		t.Fatalf("executions after A original = %d, want 1", execs)
	}
	fetchCallsAfterA, _ := fetch.stats()
	docA, evA := store.applied("tA")

	// Processor lifetime ends: drop p1 (its in-memory dedup `done` dies
	// with it — migration/backfill NONE, see the `done` field comment).
	// Lifetime 2 shares ONLY the durables.
	p1 = nil
	p2 := restartProcessor(fetch, store, claimsDB, policy, launcher)

	// Consistent tenant-swap: transport (ctx) and bytes BOTH say B, so the
	// app transport-binding seam passes by construction — rejection must
	// come from the durable boundary, which is exactly what this proves.
	swap := p2.Handle(ctxB, restartEvent(t, "tB"))

	if swap.Kind == OutcomeSuccess || swap.Kind == OutcomeDuplicate {
		t.Errorf("post-restart B redelivery kind = %q, want fail-closed rejection (never SUCCESS/DUPLICATE)", swap.Kind)
	}
	if swap.Kind != OutcomeTransient {
		t.Errorf("post-restart B redelivery kind = %q, want TRANSIENT (tenant-scoped fetch miss under B; TERMINAL would poison ingest races — see file header)", swap.Kind)
	}
	if swap.Err == nil {
		t.Error("post-restart B redelivery Err = nil, want the durable-boundary miss error")
	} else if !strings.Contains(strings.ToLower(swap.Err.Error()), "unknown document") {
		t.Errorf("post-restart B redelivery Err = %q, want the tenant-scoped fetch miss (authority 1)", swap.Err.Error())
	}
	if swap.Duplicate {
		t.Error("post-restart B redelivery Duplicate = true: cross-tenant dedupe adoption")
	}
	if len(swap.ExceptionEnvelope) != 0 {
		t.Error("post-restart B redelivery carries envelope bytes: cross-tenant adoption")
	}
	if swap.Attempts != 1 {
		t.Errorf("post-restart B redelivery Attempts = %d, want 1 (new-pipeline single attempt; transport owns redelivery)", swap.Attempts)
	}

	// No durable effect under EITHER tenant: no launch, no writes, no
	// B-side execution identity minted.
	if c2, e2, tenants := launcher.snapshot(); c2 != calls || e2 != 1 {
		t.Errorf("launcher calls/execs moved (%d,1) -> (%d,%d), want frozen (no workflow launch for B)", calls, c2, e2)
	} else {
		for invID, ten := range tenants {
			if ten == "tB" {
				t.Errorf("launcher minted execution for tenant B (inv %q): cross-tenant launch", invID)
			}
		}
	}
	if d, e := store.applied("tB"); d != 0 || e != 0 {
		t.Errorf("durable rows applied under tenant B = docs %d evidence %d, want 0/0", d, e)
	}
	if d, e := store.applied("tA"); d != docA || e != evA {
		t.Errorf("durable rows under tenant A moved (%d,%d) -> (%d,%d), want frozen", docA, evA, d, e)
	}

	// The boundary was actually consulted under B's scope: exactly one
	// fetch attempt carrying tenant B, then stop (no claim/policy/store
	// reads downstream of the miss — authority 5).
	if c, tens := fetch.stats(); c != fetchCallsAfterA+1 {
		t.Errorf("fetch calls moved %d -> %d, want exactly +1 (single new-pipeline attempt)", fetchCallsAfterA, c)
	} else if tens[len(tens)-1] != "tB" {
		t.Errorf("last fetch tenant = %q, want tB (boundary consulted under B's scope)", tens[len(tens)-1])
	}
	if n := len(policy.tenants); n != 1 {
		t.Errorf("policy consultations = %d, want 1 (A original only; B never reaches policy)", n)
	}

	// Tier-1 path, same boundary: a parser-less fresh lifetime must also
	// stop at the tenant-scoped fetch miss — bounded retries (MaxAttempts),
	// then TRANSIENT, still zero durable effect.
	p3 := NewProcessor(fetch, store, claimsDB, policy)
	fetchBeforeTier1, _ := fetch.stats()
	tier1 := p3.Handle(ctxB, restartEvent(t, "tB"))
	if tier1.Kind != OutcomeTransient {
		t.Errorf("Tier-1 post-restart B kind = %q, want TRANSIENT (same durable boundary)", tier1.Kind)
	}
	if tier1.Attempts != MaxAttempts {
		t.Errorf("Tier-1 post-restart B Attempts = %d, want %d (bounded fetch retry, then stop)", tier1.Attempts, MaxAttempts)
	}
	if c, _ := fetch.stats(); c != fetchBeforeTier1+MaxAttempts {
		t.Errorf("Tier-1 fetch calls moved %d -> %d, want +%d (bounded, no spin)", fetchBeforeTier1, c, MaxAttempts)
	}
	if d, e := store.applied("tB"); d != 0 || e != 0 {
		t.Errorf("Tier-1 durable rows under B = docs %d evidence %d, want 0/0", d, e)
	}
	if _, e2, _ := launcher.snapshot(); e2 != 1 {
		t.Errorf("Tier-1 executions = %d, want 1 (Tier-1 never launches; boundary held)", e2)
	}
}

// TestRestartSameTenant_ConvergesSingleExecution is the post-restart
// inverse: A's redelivery of X after restart must converge — recomputed
// (the in-memory dedup `done` died with lifetime 1, so Duplicate is
// false by design: `done` is a short-lived optimization, option B), one
// durable execution total (launcher memory converges), no duplicate
// durable rows. Guards against over-blocking: the boundary rejects only
// cross-tenant redelivery.
func TestRestartSameTenant_ConvergesSingleExecution(t *testing.T) {
	fetch, store, claimsDB, policy, launcher := restartRig(t)
	ctxA := postgres.WithTenant(context.Background(), claims.TenantID("tA"))

	p1 := restartProcessor(fetch, store, claimsDB, policy, launcher)
	first := p1.Handle(ctxA, restartEvent(t, "tA"))
	if first.Kind != OutcomeSuccess {
		t.Fatalf("A original kind = %q (%v), want SUCCESS (rig broken)", first.Kind, first.Err)
	}
	wantInvID, err := investigationIDForDocument("tA", "CLM-X", "doc-X")
	if err != nil {
		t.Fatalf("stable investigation ID: %v", err)
	}

	// Restart: fresh lifetime, same durables.
	p1 = nil
	p2 := restartProcessor(fetch, store, claimsDB, policy, launcher)
	again := p2.Handle(ctxA, restartEvent(t, "tA"))

	if again.Kind != OutcomeSuccess {
		t.Errorf("post-restart A redelivery kind = %q (%v), want SUCCESS (durable convergence, not rejection)", again.Kind, again.Err)
	}
	if again.Duplicate {
		t.Error("post-restart A redelivery Duplicate = true: in-memory `done` cannot survive restart (it is short-lived by design)")
	}
	if again.DocumentID != first.DocumentID {
		t.Errorf("post-restart DocumentID = %q, want canonical %q (converged identity)", again.DocumentID, first.DocumentID)
	}
	if len(again.ExceptionEnvelope) == 0 {
		t.Error("post-restart A redelivery envelope empty: converged re-execution must rebuild it deterministically")
	}
	// Single durable execution: the launcher memory (like
	// workflow_launches, first-wins) converges on the stable per-document
	// investigation ID — no second workflow. The total is summed across
	// the lifetime-1 durable object and lifetime-2's launcher so an
	// amnesiac launcher (durable memory lost on restart) is caught: two
	// objects minting one execution each totals 2, not 1.
	calls, execs, tenants := launcher.snapshot()
	l2, ok := p2.Launcher.(*restartLauncher)
	if !ok {
		t.Fatal("lifetime-2 launcher is not the shared durable *restartLauncher (rig broken)")
	}
	c2, e2, _ := l2.snapshot()
	totalExecs := execs
	if l2 != launcher {
		totalExecs = execs + e2
	}
	if calls != 2 && c2 != 2 {
		t.Errorf("launcher consultations = %d/%d, want 2 on the shared object (one per lifetime; convergence happens inside EnsureLaunched)", calls, c2)
	}
	if totalExecs != 1 {
		t.Errorf("durable executions = %d, want 1 (exactly-once launch across restart)", totalExecs)
	}
	for invID, ten := range tenants {
		if invID != wantInvID {
			t.Errorf("execution invID = %q, want stable %q", invID, wantInvID)
		}
		if ten != "tA" {
			t.Errorf("execution tenant = %q, want tA", ten)
		}
	}
	// No duplicate durable work: the new-pipeline success path writes no
	// store rows, and recomputation fetched under A's scope only.
	if d, e := store.applied("tA"); d != 0 || e != 0 {
		t.Errorf("new-pipeline rows under tA = docs %d evidence %d, want 0/0 (success path is launch-only)", d, e)
	}
	_, tens := fetch.stats()
	for _, ten := range tens {
		if ten != "tA" {
			t.Errorf("fetch consulted under tenant %q, want tA only", ten)
		}
	}
}

var (
	_ = documents.StProcessed
	_ = evidence.FieldEvidence{}
)
