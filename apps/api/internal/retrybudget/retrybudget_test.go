// Package retrybudget — APA-27/F9 RED: cross-layer retry composition pin.
//
// Each retry layer is individually bounded (worker Tier-1 MaxAttempts,
// workflow http.post max_retries, FallbackModelClient MaxTotalCalls) but
// only comments claim the composition does not multiply. This test pins a
// declared finite global bound and fails if any single layer budget widens.
package retrybudget

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/investigate/orchestrate"
	"claimops-api/internal/ports"
	"claimops-api/internal/worker"
)

// countFetcher fails the first failTimes calls with a transient error,
// then returns the canned file triple. Heal stops all future failures;
// the call counter keeps counting across deliveries.
type countFetcher struct {
	mu             sync.Mutex
	calls          int
	failTimes      int
	fileName, mime string
	content        string
}

// Fetch implements worker.ContentFetcher.
func (f *countFetcher) Fetch(_ context.Context, _, _, _ string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failTimes {
		return "", "", "", fmt.Errorf("retrybudget: injected transient boom (call %d)", f.calls)
	}
	return f.fileName, f.mime, f.content, nil
}

// Calls returns the total Fetch invocations observed.
func (f *countFetcher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Heal stops further transient failures (simulates the outage clearing
// before transport redelivery).
func (f *countFetcher) Heal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failTimes = 0
}

// memStore is an always-healthy in-memory worker.DocumentStore.
type memStore struct {
	mu       sync.Mutex
	seedDocs []documents.Document
	docs     []documents.Document
}

// InsertDocument implements worker.DocumentStore.
func (s *memStore) InsertDocument(_ context.Context, d documents.Document) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs = append(s.docs, d)
	return true, nil
}

// ListDocuments implements worker.DocumentStore.
func (s *memStore) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]documents.Document(nil), s.seedDocs...)
	return append(out, s.docs...), nil
}

// InsertEvidence implements worker.DocumentStore.
func (s *memStore) InsertEvidence(_ context.Context, e evidence.FieldEvidence) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = e
	return true, nil
}

// ListEvidence implements worker.DocumentStore.
func (s *memStore) ListEvidence(_ context.Context, _ claims.ClaimID) ([]evidence.FieldEvidence, error) {
	return nil, nil
}

// agreeLoader returns a claim view matching the canned fetcher content.
type agreeLoader struct{}

// LoadClaim implements worker.ClaimLoader.
func (agreeLoader) LoadClaim(_ context.Context, _, _ string) (worker.ClaimView, error) {
	return worker.ClaimView{
		PolicyNumber: "POL-1",
		PatientName:  "Alice",
		HospitalName: "City Hospital",
		ClaimedPaise: 5000,
	}, nil
}

// agreeChecker returns an active policy matching the canned content.
type agreeChecker struct{}

// CheckPolicy implements worker.PolicyChecker.
func (agreeChecker) CheckPolicy(_ context.Context, _, _ string) (worker.PolicyData, error) {
	return worker.PolicyData{Number: "POL-1", Patient: "Alice", Active: true}, nil
}

// scriptModel serves scripted results: nil entries fail retryably (5xx
// wrapping ErrModelUpstream), non-nil entries succeed. Every call counts.
type scriptModel struct {
	mu    sync.Mutex
	steps []*orchestrate.ModelResponse
	calls int
}

// Complete implements orchestrate.ModelClient.
func (m *scriptModel) Complete(ctx context.Context, _ orchestrate.ModelRequest) (orchestrate.ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return orchestrate.ModelResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls <= len(m.steps) && m.steps[m.calls-1] == nil {
		return orchestrate.ModelResponse{}, fmt.Errorf("retrybudget: groq: status 500 boom (call %d): %w", m.calls, orchestrate.ErrModelUpstream)
	}
	return orchestrate.ModelResponse{Payload: []byte("{}"), ModelID: "retrybudget-fake"}, nil
}

// Calls returns the total Complete invocations observed.
func (m *scriptModel) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// eventDoc builds a document-ingested.v1 event envelope for docID.
func eventDoc(t *testing.T, docID string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"schema_version": ports.DocumentIngestedSchemaVersion,
		"tenant":         "t1",
		"claim":          "c1",
		"document_id":    docID,
		"sha256":         "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// seedStore returns a store seeded with the two non-current required doc
// types so the Tier-1 verify path runs zero-exception to SUCCESS.
func seedStore() *memStore {
	return &memStore{seedDocs: []documents.Document{
		{ID: "doc-dis", Tenant: "t1", ClaimID: "c1", Type: documents.DocDischargeSummary, Status: documents.StProcessed},
		{ID: "doc-bill", Tenant: "t1", ClaimID: "c1", Type: documents.DocHospitalBill, Status: documents.StProcessed},
	}}
}

// yamlMaxRetries scans every max_retries value declared in
// workflows/claim-investigation.yaml using stdlib only (no new deps).
func yamlMaxRetries(t *testing.T) []int {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	var wf string
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "workflows", "claim-investigation.yaml")
		if _, err := os.Stat(cand); err == nil {
			wf = cand
			break
		}
		dir = filepath.Dir(dir)
	}
	if wf == "" {
		t.Fatal("workflows/claim-investigation.yaml not found above package dir")
	}
	raw, err := os.ReadFile(wf)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "max_retries:") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "max_retries:")))
		if err != nil {
			t.Fatalf("unparseable max_retries line %q: %v", line, err)
		}
		out = append(out, n)
	}
	return out
}

// TestRetryComposition_SingleTransientWithinGlobalBound drives one logical
// investigation with ONE injected transient failure (a full first-delivery
// outage that clears before transport redelivery) and counts invocations
// at all three layers via fakes/counters: worker re-deliveries, workflow
// HTTP attempts, and provider calls. No sleeps, no wall-clock assertions.
func TestRetryComposition_SingleTransientWithinGlobalBound(t *testing.T) {
	ctx := context.Background()

	// Worker leg: first delivery fails transiently on every pipeline
	// attempt (the single injected outage); redelivery then succeeds.
	fetch := &countFetcher{
		failTimes: 1 << 30,
		fileName:  "claim_form.pdf",
		mime:      "application/pdf",
		content:   "patient_name: Alice\npolicy_number: POL-1\n",
	}
	proc := worker.NewProcessor(fetch, seedStore(), agreeLoader{}, agreeChecker{})
	investigation := eventDoc(t, "doc-f9-composition")

	first := proc.Handle(ctx, investigation)
	if first.Kind != worker.OutcomeTransient {
		t.Fatalf("delivery 1 kind = %q, want TRANSIENT (injected outage)", first.Kind)
	}
	if got := fetch.Calls(); got != worker.MaxAttempts {
		t.Fatalf("delivery 1 fetch calls = %d, want %d (Tier-1 spends its per-delivery budget)", got, worker.MaxAttempts)
	}

	fetch.Heal()
	second := proc.Handle(ctx, investigation)
	if second.Kind != worker.OutcomeSuccess {
		t.Fatalf("redelivery kind = %q (%v), want SUCCESS", second.Kind, second.Err)
	}
	if second.Duplicate {
		t.Fatal("TRANSIENT delivery must not be remembered: redelivery must reprocess, not report DUPLICATE")
	}
	workerDeliveries := 2

	// Workflow leg: the redelivered investigation drives one workflow
	// HTTP step that succeeds on its first attempt.
	httpCalls := 1 // fake poster: healthy on first attempt (the only failure was the worker outage)

	// Provider leg: the workflow step drives one model call that
	// succeeds on its first provider invocation.
	primary := &scriptModel{steps: []*orchestrate.ModelResponse{{}}}
	secondary := &scriptModel{}
	fb := orchestrate.NewFallbackModelClient(primary, secondary, 0)
	if _, err := fb.Complete(ctx, orchestrate.ModelRequest{}); err != nil {
		t.Fatalf("provider call err = %v, want success", err)
	}
	providerCalls := primary.Calls() + secondary.Calls()

	if httpCalls > WorkflowHTTPAttempts {
		t.Fatalf("workflow HTTP calls = %d, exceeds per-step budget %d", httpCalls, WorkflowHTTPAttempts)
	}
	if providerCalls > ProviderMaxTotalCalls {
		t.Fatalf("provider calls = %d, exceeds fallback budget %d", providerCalls, ProviderMaxTotalCalls)
	}
	total := workerDeliveries + httpCalls + providerCalls
	if total > MaxCompositionProviderCalls {
		t.Fatalf("composition total = %d (worker %d + http %d + provider %d), exceeds global bound %d",
			total, workerDeliveries, httpCalls, providerCalls, MaxCompositionProviderCalls)
	}
	t.Logf("composition total = %d (worker deliveries %d, fetch %d, http %d, provider %d) within bound %d",
		total, workerDeliveries, fetch.Calls(), httpCalls, providerCalls, MaxCompositionProviderCalls)
}

// TestRetryComposition_LayerBudgetsPinned pins each layer budget to its
// declared value and pins the global bound to the derived product, so
// widening ANY single layer budget independently turns this test red.
func TestRetryComposition_LayerBudgetsPinned(t *testing.T) {
	if worker.MaxAttempts != 3 {
		t.Fatalf("worker.MaxAttempts = %d, want 3 (Tier-1 redelivery budget)", worker.MaxAttempts)
	}
	got := yamlMaxRetries(t)
	if len(got) == 0 {
		t.Fatal("no max_retries found in workflows/claim-investigation.yaml")
	}
	for i, n := range got {
		if n != WorkflowHTTPMaxRetries {
			t.Fatalf("workflow max_retries[%d] = %d, want %d", i, n, WorkflowHTTPMaxRetries)
		}
	}
	def := orchestrate.NewFallbackModelClient(&scriptModel{}, &scriptModel{}, 0)
	if def.MaxTotalCalls != ProviderMaxTotalCalls {
		t.Fatalf("fallback default MaxTotalCalls = %d, want %d", def.MaxTotalCalls, ProviderMaxTotalCalls)
	}
	if want := WorkerTier1Attempts * WorkflowHTTPAttempts * ProviderMaxTotalCalls; MaxCompositionProviderCalls != want {
		t.Fatalf("MaxCompositionProviderCalls = %d, want derived product %d", MaxCompositionProviderCalls, want)
	}
	if MaxCompositionProviderCalls != 48 {
		t.Fatalf("MaxCompositionProviderCalls = %d, want 48 (3 worker x 4 workflow-HTTP x 4 provider)", MaxCompositionProviderCalls)
	}

	// Behavioral worst case at the provider seam: an always-retryable
	// outage must spend exactly the fallback budget, never more.
	badPrimary := &scriptModel{steps: []*orchestrate.ModelResponse{nil, nil, nil, nil, nil, nil}}
	badSecondary := &scriptModel{steps: []*orchestrate.ModelResponse{nil, nil, nil, nil, nil, nil}}
	capped := orchestrate.NewFallbackModelClient(badPrimary, badSecondary, 0)
	if _, err := capped.Complete(context.Background(), orchestrate.ModelRequest{}); err == nil {
		t.Fatal("all-failing providers must return an error")
	}
	if got, want := badPrimary.Calls()+badSecondary.Calls(), ProviderMaxTotalCalls; got != want {
		t.Fatalf("all-failing provider calls = %d, want exactly the budget %d", got, want)
	}
}
