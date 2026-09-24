package app

// APA-28 restart adversary, transport layer: after a processor restart the
// consistent tenant-swap (attrs B + bytes B) MUST pass the app
// transport-binding seam (B == B — nothing to compare) and be rejected by
// the DURABLE tenant boundary instead. This test drives the REAL
// DocumentOutcomeHandler over a REAL Tier-1 worker.Processor with
// production-faithful scoped doubles (tenant-scoped fetch registry +
// ctx-tenant-scoped store lists, mirroring BlobFetchBridge + StoreBridge +
// RLS), then proves the rejection never false-ACKs: pull returns non-nil
// (Nack) and push returns 503.
//
// Authority chain + TRANSIENT adjudication (why not TERMINAL): see the
// header of internal/worker/restart_tenant_swap_test.go (option B is true
// of this codebase; fetch-miss owns transient per the frozen S3 retry
// contract). Five-boundary terminology per ruling 2 is used throughout.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/worker"
)

// restartFetcher replicates BlobFetchBridge over an RLS-scoped store:
// metadata resolves per (tenant, claim, document); a miss under the
// requesting tenant's scope is the transient "unknown document" miss
// (blob_fetch_bridge.go:71-73), never another tenant's bytes.
type restartFetcher struct {
	mu    sync.Mutex
	blobs map[string]restartBlob
	calls int
	seen  []string
}

// restartBlob is one tenant-scoped serving triple.
type restartBlob struct {
	fileName, mime, content string
}

func (f *restartFetcher) Fetch(_ context.Context, tenant, claimID, docID string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = append(f.seen, tenant)
	b, ok := f.blobs[tenant+"|"+claimID+"|"+docID]
	if !ok {
		return "", "", "", fmt.Errorf("restartfetch: unknown document %q under tenant %q", docID, tenant)
	}
	return b.fileName, b.mime, b.content, nil
}

func (f *restartFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// restartStore replicates StoreBridge + RLS: inserts scope from the domain
// value (store_bridge.go:54-62); lists filter by the ctx tenant
// (store_bridge.go:64-79, tenant_isolation policy). Counters prove zero
// durable mutation under B.
type restartStore struct {
	mu                sync.Mutex
	docs              []documents.Document
	docWritesByTenant map[string]int
	evWritesByTenant  map[string]int
}

func newRestartStore() *restartStore {
	return &restartStore{
		docs: []documents.Document{
			{ID: "doc-dis", Tenant: "tA", ClaimID: "CLM-X", Type: documents.DocDischargeSummary, Status: documents.StProcessed},
			{ID: "doc-bill", Tenant: "tA", ClaimID: "CLM-X", Type: documents.DocHospitalBill, Status: documents.StProcessed},
		},
		docWritesByTenant: map[string]int{},
		evWritesByTenant:  map[string]int{},
	}
}

func (s *restartStore) InsertDocument(_ context.Context, d documents.Document) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docWritesByTenant[string(d.Tenant)]++
	s.docs = append(s.docs, d)
	return true, nil
}

func (s *restartStore) ListDocuments(ctx context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	ten, err := postgres.TenantFrom(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []documents.Document
	for _, d := range s.docs {
		if d.Tenant == ten {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *restartStore) InsertEvidence(_ context.Context, e evidence.FieldEvidence) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evWritesByTenant[string(e.Tenant)]++
	return true, nil
}

func (s *restartStore) ListEvidence(context.Context, claims.ClaimID) ([]evidence.FieldEvidence, error) {
	return nil, nil
}

func (s *restartStore) writes(tenant string) (docs, ev int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docWritesByTenant[tenant], s.evWritesByTenant[tenant]
}

// restartClaims replicates ClaimBridge over RLS: only claims owned by the
// requesting tenant resolve (claim_bridge.go:29-36 → workflow.ErrNotFound
// under another scope, claim_repository.go:138-141).
type restartClaims struct {
	views map[string]worker.ClaimView
}

func (c restartClaims) LoadClaim(_ context.Context, tenant, claimID string) (worker.ClaimView, error) {
	if v, ok := c.views[tenant+"|"+claimID]; ok {
		return v, nil
	}
	return worker.ClaimView{}, fmt.Errorf("restartclaims: claim %q absent under tenant %q", claimID, tenant)
}

func TestRestartConsistentSwap_DurableBoundaryRejectsNoFalseAck(t *testing.T) {
	fetch := &restartFetcher{blobs: map[string]restartBlob{
		"tA|CLM-X|doc-X": {fileName: "claim_form.pdf", mime: "application/pdf", content: "patient_name: Alice\npolicy_number: POL-1\n"},
	}}
	store := newRestartStore()
	claimsDB := restartClaims{views: map[string]worker.ClaimView{
		"tA|CLM-X": {PolicyNumber: "POL-1", PatientName: "Alice", HospitalName: "City Hospital", ClaimedPaise: 5000},
	}}
	newProc := func() *worker.Processor {
		return worker.NewProcessor(fetch, store, claimsDB, tswapPolicy{})
	}
	ctxA := postgres.WithTenant(context.Background(), claims.TenantID("tA"))
	ctxB := postgres.WithTenant(context.Background(), claims.TenantID("tB"))
	evA := tswapEvent(t, "tA")
	evB := tswapEvent(t, "tB")

	// Lifetime 1: tenant A processes doc-X to completion (rig proof).
	p1 := newProc()
	if first := DocumentOutcomeHandler(p1)(ctxA, evA); first.Kind != worker.OutcomeSuccess {
		t.Fatalf("A original kind = %q (%v), want SUCCESS (rig broken)", first.Kind, first.Err)
	}
	docAfterA, evAfterA := store.writes("tA")

	// Lifetime 2: fresh processor, SAME durables. Consistent swap —
	// transport attrs B, bytes B — passes the app seam (B == B); the
	// durable boundary must reject.
	p1 = nil
	p2 := newProc()
	swap := DocumentOutcomeHandler(p2)(ctxB, evB)

	if swap.Kind == worker.OutcomeSuccess || swap.Kind == worker.OutcomeDuplicate {
		t.Errorf("post-restart consistent-swap kind = %q, want fail-closed rejection (never SUCCESS/DUPLICATE)", swap.Kind)
	}
	if swap.Kind != worker.OutcomeTransient {
		t.Errorf("post-restart consistent-swap kind = %q, want TRANSIENT (tenant-scoped fetch miss; TERMINAL adjudicated in worker/restart_tenant_swap_test.go)", swap.Kind)
	}
	if swap.Err == nil || !strings.Contains(strings.ToLower(swap.Err.Error()), "unknown document") {
		t.Errorf("post-restart consistent-swap Err = %v, want the tenant-scoped fetch miss", swap.Err)
	}
	if swap.Duplicate {
		t.Error("post-restart consistent-swap Duplicate = true: cross-tenant adoption")
	}
	// Boundary consulted under B; zero durable mutation under B; A frozen.
	fetch.mu.Lock()
	lastSeen := fetch.seen[len(fetch.seen)-1]
	fetch.mu.Unlock()
	if lastSeen != "tB" {
		t.Errorf("last fetch tenant = %q, want tB (durable boundary consulted under B's scope)", lastSeen)
	}
	if d, e := store.writes("tB"); d != 0 || e != 0 {
		t.Errorf("writes under tenant B = docs %d evidence %d, want 0/0", d, e)
	}
	if d, e := store.writes("tA"); d != docAfterA || e != evAfterA {
		t.Errorf("writes under tenant A moved (%d,%d) -> (%d,%d), want frozen", docAfterA, evAfterA, d, e)
	}

	// No false ACK, pull path: TRANSIENT maps to non-nil (Nack) —
	// DocumentEventHandler, app/worker.go:53-60.
	if err := DocumentEventHandler(p2)(ctxB, evB); err == nil {
		t.Error("pull handler on post-restart swap = nil, want non-nil (Nack; never ACK-as-success)")
	}

	// No false ACK, push path: TRANSIENT maps to 503 (pushevents.go:109).
	var kinds []worker.OutcomeKind
	capturing := func(ctx context.Context, event []byte) worker.Outcome {
		out := DocumentOutcomeHandler(p2)(ctx, event)
		kinds = append(kinds, out.Kind)
		return out
	}
	a := pushApp(capturing, nil, PushAuth{Mode: "none"})
	req := httptest.NewRequest(http.MethodPost, "/events/document-ingested", strings.NewReader(pushBody(t,
		map[string]string{"tenant": "tB", "claim": "CLM-X", "document_id": "doc-X", "sha256": "abc123"},
		map[string]string{"tenant_id": "tB"})))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("consistent-swap push status = %d, want 503 (transient redelivery, never 200-ACK)", resp.StatusCode)
	}
	if len(kinds) != 1 || kinds[0] != worker.OutcomeTransient {
		t.Errorf("consistent-swap push kinds = %v, want [TRANSIENT]", kinds)
	}
	if d, e := store.writes("tB"); d != 0 || e != 0 {
		t.Errorf("writes under tenant B after push = docs %d evidence %d, want 0/0", d, e)
	}
}

var _ = ports.DocumentIngestedSchemaVersion
