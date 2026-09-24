package worker

// S6 GREEN-B: worker→launcher wiring contracts.
//
//   - Envelope built + Launcher set => EnsureLaunched called exactly once
//     with the stable per-document ID, explicit tenant/claim, and the
//     default workflow ID; outcome stays SUCCESS.
//   - Launch failure => TRANSIENT (envelope bytes + codes preserved) so
//     transport redelivery re-enters the lookup and converges.
//   - Launcher nil => launch skipped, outcome unchanged (all pre-existing
//     full-chain tests run this path).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"claimops-api/internal/documents"
	"claimops-api/internal/invest"
)

type fakeLauncher struct {
	mu       sync.Mutex
	calls    int
	tenant   string
	claim    string
	invID    string
	env      invest.UnresolvedException
	workflow string
	err      error
}

func (f *fakeLauncher) EnsureLaunched(_ context.Context, tenantID, claimID, investigationID string, env invest.UnresolvedException, workflowID string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.tenant, f.claim, f.invID, f.env, f.workflow = tenantID, claimID, investigationID, env, workflowID
	if f.err != nil {
		return "", false, f.err
	}
	return "exec-wiring-1", true, nil
}

func (f *fakeLauncher) snapshot() (calls int, tenant, claim, invID, workflow string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.tenant, f.claim, f.invID, f.workflow
}

func lowOCRProcessor(store DocumentStore) (*Processor, *fakeFetcher) {
	fetch, _, loader, checker := happyFixture()
	p := NewProcessor(fetch, store, loader, checker)
	p.Parser = mockOCRParser{conf: 0.70, name: "mock-ocr", version: "test"}
	return p, fetch
}

func TestLaunchWiring_CalledOnceWithStableID(t *testing.T) {
	store := newDedupStore()
	p, _ := lowOCRProcessor(store)
	fl := &fakeLauncher{}
	p.Launcher = fl

	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-wire-01", "")
	if out.Kind != OutcomeSuccess {
		t.Fatalf("kind = %q (%v), want SUCCESS", out.Kind, out.Err)
	}
	if out.ExceptionEnvelope == nil {
		t.Fatal("want envelope bytes preserved")
	}
	calls, tenant, claim, invID, wf := fl.snapshot()
	if calls != 1 {
		t.Fatalf("launcher calls = %d, want 1", calls)
	}
	wantID, err := investigationIDForDocument("t1", "c1", "doc-wire-01")
	if err != nil {
		t.Fatalf("stable ID: %v", err)
	}
	if invID != wantID {
		t.Fatalf("launcher invID = %q, want stable %q", invID, wantID)
	}
	if tenant != "t1" || claim != "c1" {
		t.Fatalf("launcher tenant/claim = %q/%q, want t1/c1 (explicit, never from payload)", tenant, claim)
	}
	if wf != defaultWorkflowID {
		t.Fatalf("workflow = %q, want default %q", wf, defaultWorkflowID)
	}
}

func TestLaunchWiring_Failure_IsTransientKeepsEnvelope(t *testing.T) {
	store := newDedupStore()
	p, _ := lowOCRProcessor(store)
	fl := &fakeLauncher{err: errors.New("workflow down")}
	p.Launcher = fl

	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-wire-02", "")
	if out.Kind != OutcomeTransient {
		t.Fatalf("kind = %q (%v), want TRANSIENT (redelivery recovers via lookup)", out.Kind, out.Err)
	}
	if out.ExceptionEnvelope == nil {
		t.Fatal("envelope bytes must survive launch failure (durable evidence for HITL/redelivery)")
	}
	found := false
	for _, c := range out.ExceptionCodes {
		if c == "LOW_OCR_CONFIDENCE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("codes = %v, want LOW_OCR_CONFIDENCE preserved", out.ExceptionCodes)
	}
	if out.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no worker-level relaunch; transport owns redelivery)", out.Attempts)
	}
}

func TestLaunchWiring_NilLauncher_SkipsLaunch(t *testing.T) {
	store := newDedupStore()
	p, _ := lowOCRProcessor(store)
	p.Launcher = nil

	out, _ := p.runNewPipeline(context.Background(), "t1", "c1", "abc123", "doc-wire-03", "")
	if out.Kind != OutcomeSuccess {
		t.Fatalf("kind = %q (%v), want SUCCESS", out.Kind, out.Err)
	}
	if out.ExceptionEnvelope == nil {
		t.Fatal("want envelope bytes (unchanged legacy behavior)")
	}
	_ = documents.StProcessed
}
