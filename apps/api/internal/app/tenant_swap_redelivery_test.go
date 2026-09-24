package app

// APA-28 RED: tenant-swapped redelivery must fail closed at the
// transport-binding seam.
//
// Seam choice: DocumentOutcomeHandler is the single scoping site shared by
// the push endpoint (pushevents.go attrs scoping) and the pull consumer
// (DocumentEventHandler); cmd/api's pull loop applies the identical
// attrs->ctx pattern before delegating here, so driving this handler with
// an attrs-scoped ctx exercises both wirings. The processor-seam proof
// (durable identity binding vs the processed-set) lives in
// internal/worker/tenant_swap_redelivery_test.go; THIS file proves the
// transport seam: when the attrs tenant and the event-bytes tenant
// disagree, the handler must return an explicit TERMINAL rejection
// without invoking the processor at all.
//
// Inverse (same run): legitimate same-tenant redelivery still dedupes
// (S6 convergence) — guards against over-blocking.

import (
	"context"
	"encoding/json"
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

// tswapFetcher serves one benign claim-form triple and counts calls.
type tswapFetcher struct {
	mu    sync.Mutex
	calls int
}

func (f *tswapFetcher) Fetch(context.Context, string, string, string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return "claim_form.pdf", "application/pdf", "patient_name: Alice\npolicy_number: POL-1\n", nil
}

func (f *tswapFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// tswapStore records Tier-1 document/evidence writes per tenant and seeds
// the sibling doc types so the rig converges with zero verify exceptions
// (mirrors the worker happyFixture read models).
type tswapStore struct {
	mu                sync.Mutex
	docWritesByTenant map[string]int
	evWritesByTenant  map[string]int
}

func newTswapStore() *tswapStore {
	return &tswapStore{docWritesByTenant: map[string]int{}, evWritesByTenant: map[string]int{}}
}

func (s *tswapStore) InsertDocument(_ context.Context, d documents.Document) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docWritesByTenant[string(d.Tenant)]++
	return true, nil
}

func (s *tswapStore) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	return []documents.Document{
		{ID: "doc-dis", Tenant: "tA", ClaimID: "CLM-X", Type: documents.DocDischargeSummary, Status: documents.StProcessed},
		{ID: "doc-bill", Tenant: "tA", ClaimID: "CLM-X", Type: documents.DocHospitalBill, Status: documents.StProcessed},
	}, nil
}

func (s *tswapStore) InsertEvidence(_ context.Context, e evidence.FieldEvidence) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evWritesByTenant[string(e.Tenant)]++
	return true, nil
}

func (s *tswapStore) ListEvidence(_ context.Context, _ claims.ClaimID) ([]evidence.FieldEvidence, error) {
	return nil, nil
}

func (s *tswapStore) writes(tenant string) (docs, ev int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docWritesByTenant[tenant], s.evWritesByTenant[tenant]
}

type tswapClaims struct{}

func (tswapClaims) LoadClaim(context.Context, string, string) (worker.ClaimView, error) {
	return worker.ClaimView{PolicyNumber: "POL-1", PatientName: "Alice", HospitalName: "City Hospital", ClaimedPaise: 5000}, nil
}

type tswapPolicy struct{}

func (tswapPolicy) CheckPolicy(context.Context, string, string) (worker.PolicyData, error) {
	return worker.PolicyData{Number: "POL-1", Patient: "Alice", Active: true}, nil
}

// tswapRig wires the REAL DocumentOutcomeHandler over the REAL
// worker.Processor (Tier-1, deterministic) with counting fakes.
func tswapRig() (func(ctx context.Context, event []byte) worker.Outcome, *tswapFetcher, *tswapStore) {
	fetch := &tswapFetcher{}
	store := newTswapStore()
	proc := worker.NewProcessor(fetch, store, tswapClaims{}, tswapPolicy{})
	return DocumentOutcomeHandler(proc), fetch, store
}

func tswapEvent(t *testing.T, tenant string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"schema_version": ports.DocumentIngestedSchemaVersion,
		"tenant":         tenant,
		"claim":          "CLM-X",
		"document_id":    "doc-X",
		"sha256":         "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTenantSwap_TransportSeam_FailClosed(t *testing.T) {
	handle, fetch, store := tswapRig()
	ctxA := postgres.WithTenant(context.Background(), claims.TenantID("tA"))
	ctxB := postgres.WithTenant(context.Background(), claims.TenantID("tB"))
	evA := tswapEvent(t, "tA")
	evB := tswapEvent(t, "tB")

	// Original delivery under tenant A must succeed (rig proof).
	first := handle(ctxA, evA)
	if first.Kind != worker.OutcomeSuccess {
		t.Fatalf("original delivery kind = %q (%v), want SUCCESS (rig broken)", first.Kind, first.Err)
	}
	fetchAfterA, docAfterA, evAfterA := fetch.count(), mustTswapWrites(t, store, "tA"), mustTswapWritesB(t, store)

	// Case 1 — attrs swap: transport says B, event bytes say A.
	swapAttrs := handle(ctxB, evA)
	assertTswapTerminal(t, "attrs-swap", swapAttrs)
	// Case 2 — bytes swap: transport says A, event bytes say B.
	swapBytes := handle(ctxA, evB)
	assertTswapTerminal(t, "bytes-swap", swapBytes)
	// Case 3 — consistent swap: both channels say B for A's durable identity.
	swapBoth := handle(ctxB, evB)
	assertTswapTerminal(t, "consistent-swap", swapBoth)

	// No processor IO and no store mutation on any adversarial delivery.
	if got := fetch.count(); got != fetchAfterA {
		t.Errorf("fetch calls moved %d -> %d on swap, want processor never invoked", fetchAfterA, got)
	}
	if d, e := store.writes("tB"); d != 0 || e != 0 {
		t.Errorf("writes under tenant B = docs %d evidence %d, want 0/0", d, e)
	}
	if d, e := store.writes("tA"); d != docAfterA || e != evAfterA {
		t.Errorf("writes under tenant A moved (%d,%d) -> (%d,%d), want frozen", docAfterA, evAfterA, d, e)
	}

	// Inverse (same run): legitimate same-tenant redelivery of X under A
	// still converges as S6 established (DUPLICATE, no new side effects).
	again := handle(ctxA, evA)
	if again.Kind != worker.OutcomeDuplicate || !again.Duplicate {
		t.Errorf("same-tenant redelivery kind = %q duplicate = %v, want DUPLICATE/true", again.Kind, again.Duplicate)
	}
	if got := fetch.count(); got != fetchAfterA {
		t.Errorf("fetch calls moved %d -> %d on same-tenant redelivery, want dedupe before IO", fetchAfterA, got)
	}

	// Pull seam: the terminal rejection must not spin the Nack consumer —
	// DocumentEventHandler maps TERMINAL to nil (ack), never to redelivery.
	if err := DocumentEventHandler(newTswapPullProc(t))(ctxB, evA); err != nil {
		t.Errorf("pull handler on tenant-swapped event = %v, want nil (terminal acks, no poison spin)", err)
	}
}

// TestTenantSwap_PushPath_FailClosed drives the full Pub/Sub push wiring
// (attrs scoping in DocumentPushHandler + DocumentOutcomeHandler) with a
// tenant-swapped envelope: the observed outcome kind must be TERMINAL
// (acked as 200 per terminal-ack semantics, never hidden as success or
// duplicate), and the processor must stay untouched.
func TestTenantSwap_PushPath_FailClosed(t *testing.T) {
	handle, fetch, _ := tswapRig()
	var kinds []worker.OutcomeKind
	capturing := func(ctx context.Context, event []byte) worker.Outcome {
		out := handle(ctx, event)
		kinds = append(kinds, out.Kind)
		return out
	}
	a := pushApp(capturing, nil, PushAuth{Mode: "none"})

	// Seed the durable identity under tenant A first (legit push).
	seed := httptest.NewRequest(http.MethodPost, "/events/document-ingested", strings.NewReader(pushBody(t,
		map[string]string{"tenant": "tA", "claim": "CLM-X", "document_id": "doc-X", "sha256": "abc123"},
		map[string]string{"tenant_id": "tA"})))
	seed.Header.Set("Content-Type", "application/json")
	resp, err := a.Test(seed)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("seed push status = %d, want 200 (rig broken)", resp.StatusCode)
	}
	if len(kinds) != 1 || kinds[0] != worker.OutcomeSuccess {
		t.Fatalf("seed push kinds = %v, want [SUCCESS] (rig broken)", kinds)
	}
	fetchAfterSeed := fetch.count()

	// Adversarial push: attrs tenant B, event-bytes tenant A.
	evil := httptest.NewRequest(http.MethodPost, "/events/document-ingested", strings.NewReader(pushBody(t,
		map[string]string{"tenant": "tA", "claim": "CLM-X", "document_id": "doc-X", "sha256": "abc123"},
		map[string]string{"tenant_id": "tB"})))
	evil.Header.Set("Content-Type", "application/json")
	resp, err = a.Test(evil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("swapped push status = %d, want 200 (terminal rejections ack, never 503-spin)", resp.StatusCode)
	}
	if len(kinds) != 2 {
		t.Fatalf("kinds = %v, want 2 observed outcomes", kinds)
	}
	if kinds[1] == worker.OutcomeSuccess || kinds[1] == worker.OutcomeDuplicate {
		t.Errorf("swapped push kind = %q, want explicit TERMINAL rejection", kinds[1])
	}
	if kinds[1] != worker.OutcomeTerminal {
		t.Errorf("swapped push kind = %q, want TERMINAL", kinds[1])
	}
	if got := fetch.count(); got != fetchAfterSeed {
		t.Errorf("fetch calls moved %d -> %d on swapped push, want processor never invoked", fetchAfterSeed, got)
	}
}

func assertTswapTerminal(t *testing.T, name string, out worker.Outcome) {
	t.Helper()
	if out.Kind == worker.OutcomeSuccess || out.Kind == worker.OutcomeDuplicate {
		t.Errorf("%s: kind = %q, want explicit TERMINAL rejection (never SUCCESS/DUPLICATE)", name, out.Kind)
	}
	if out.Kind != worker.OutcomeTerminal {
		t.Errorf("%s: kind = %q, want TERMINAL", name, out.Kind)
	}
	if out.Err == nil {
		t.Errorf("%s: Err = nil, want explicit rejection error", name)
	} else if !strings.Contains(strings.ToLower(out.Err.Error()), "tenant") {
		t.Errorf("%s: Err = %q, want a tenant-binding rejection", name, out.Err.Error())
	}
	if out.Duplicate {
		t.Errorf("%s: Duplicate = true: adopted tenant-A execution", name)
	}
}

func mustTswapWrites(t *testing.T, store *tswapStore, tenant string) int {
	t.Helper()
	d, _ := store.writes(tenant)
	return d
}

func mustTswapWritesB(t *testing.T, store *tswapStore) int {
	t.Helper()
	_, e := store.writes("tA")
	return e
}

// newTswapPullProc builds a Tier-1 processor for the pull-ack assertion;
// its outcome is irrelevant — only the nil/ack mapping is observed.
func newTswapPullProc(t *testing.T) *worker.Processor {
	t.Helper()
	return worker.NewProcessor(&tswapFetcher{}, newTswapStore(), tswapClaims{}, tswapPolicy{})
}

var _ = ports.DocumentIngestedSchemaVersion
