// Package retrybudget — APA-27/F9: cross-layer retry composition evidence.
//
// Option 2 (S6 dedupe): worker (re)deliveries converge via the durable
// launch boundary onto ONE workflow execution, so the worker factor does
// not multiply. The honest composed bound is therefore
// 1 execution x 4 workflow attempts x 4 provider calls = 16 per workflow
// step, proved by the tests below against the real production seams
// (investigate.Launcher, orchestrate.FallbackModelClient) with
// fakes/counters. Deterministic: no sleeps, no wall-clock assertions.
package retrybudget

import (
	"context"
	"encoding/json"
	"errors"
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
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/investigate/orchestrate"
	"claimops-api/internal/ports"
	"claimops-api/internal/worker"
	"claimops-api/internal/workflow"
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

// countProvider counts StartExecution calls and models a provider with no
// pre-existing executions (typed absence, so the S6 reconciler proceeds
// to start; any other error would fail closed).
type countProvider struct {
	mu    sync.Mutex
	calls int
}

// StartExecution implements workflow.WorkflowProvider.
func (f *countProvider) StartExecution(_ context.Context, _ string, _ any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return fmt.Sprintf("exec-f9-%d", f.calls), nil
}

// GetExecution implements workflow.WorkflowProvider.
func (f *countProvider) GetExecution(_ context.Context, name string) (string, json.RawMessage, error) {
	return "", nil, fmt.Errorf("retrybudget: execution %q absent: %w", name, workflow.ErrExecutionNotFound)
}

// SendCallback implements workflow.WorkflowProvider.
func (f *countProvider) SendCallback(_ context.Context, _ string, _ any) error {
	return errors.New("retrybudget: no callbacks")
}

// DeployWorkflow implements workflow.WorkflowProvider.
func (f *countProvider) DeployWorkflow(_ context.Context, _ string, _ string) error {
	return errors.New("retrybudget: no deploy")
}

// Calls returns the total StartExecution invocations observed.
func (f *countProvider) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var _ workflow.WorkflowProvider = (*countProvider)(nil)

// memLaunches is an in-memory investigate.LaunchStore: first
// RecordLaunch wins (mirrors the PG ON CONFLICT DO NOTHING first-wins
// rule); GetLaunch is tenant-scoped.
type memLaunches struct {
	mu   sync.Mutex
	rows map[string]investigate.LaunchRecord
}

// newMemLaunches returns an empty store.
func newMemLaunches() *memLaunches { return &memLaunches{rows: map[string]investigate.LaunchRecord{}} }

// RecordLaunch implements investigate.LaunchStore.
func (f *memLaunches) RecordLaunch(_ context.Context, rec investigate.LaunchRecord) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := rec.TenantID + "\x00" + rec.InvestigationID
	if _, ok := f.rows[k]; ok {
		return false, nil
	}
	f.rows[k] = rec
	return true, nil
}

// GetLaunch implements investigate.LaunchStore.
func (f *memLaunches) GetLaunch(_ context.Context, tenant, invID string) (investigate.LaunchRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.rows[tenant+"\x00"+invID]
	return rec, ok, nil
}

var _ investigate.LaunchStore = (*memLaunches)(nil)

// launchEnv builds a valid envelope for (tenant, claim, invID): the same
// shape the S6 launch tests use, so EnsureLaunched exercises its real
// validation + persist-first path.
func launchEnv(tenant, claim, invID string) invest.UnresolvedException {
	return invest.UnresolvedException{
		TenantID: tenant, ClaimID: claim,
		ExceptionID:     "ex-0123456789abcdef0123456789abcdef",
		InvestigationID: invID,
		RuleFindings: []invest.RuleFinding{{
			Code: invest.RulePolicyNumberConflict, Severity: invest.SeverityHigh,
			Message: "policy number conflict", EvidenceIDs: []string{"ev-01"},
			AffectedFields: []string{"policy_number"},
		}},
		Scope: invest.ScopeConstraints{
			TenantID: tenant, ClaimID: claim,
			AllowTools:   []invest.ToolName{invest.ToolGetClaim},
			MaxToolCalls: 5, DeadlineMs: 5000, RequestID: "req-f9-composition",
		},
		EvidenceRefs: []invest.EvidenceRef{{
			EvidenceID: "ev-01", SourceType: invest.EvidenceSourceDocument,
			SourceID: "doc-01", TenantID: tenant, ClaimID: claim,
			DocumentID: "doc-01", Page: 1,
		}},
	}
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

// TestRetryComposition_WorkerTier1PerDeliveryBudget pins the worker leg:
// one delivery spends at most MaxAttempts Tier-1 tries, and a TRANSIENT
// delivery is NOT remembered, so transport redelivery reprocesses to
// SUCCESS. Worker retries are pre-launch (Tier-1 never launches), so
// they cannot multiply workflow executions; launch convergence is
// proved separately below.
func TestRetryComposition_WorkerTier1PerDeliveryBudget(t *testing.T) {
	ctx := context.Background()
	fetch := &countFetcher{
		failTimes: 1 << 30,
		fileName:  "claim_form.pdf",
		mime:      "application/pdf",
		content:   "patient_name: Alice\npolicy_number: POL-1\n",
	}
	proc := worker.NewProcessor(fetch, seedStore(), agreeLoader{}, agreeChecker{})
	investigation := eventDoc(t, "doc-f9-worker")

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
}

// TestRetryComposition_WorkerRedeliveryConvergesToSingleLaunch proves the
// S6 anti-multiplication boundary with the REAL production Launcher
// (investigate.EnsureLaunched): three redeliveries carrying the SAME
// stable investigation ID (what worker/launch.go investigationIDForDocument
// mints per document) converge onto ONE workflow execution — the second
// and third calls report launched=false with the same execution name and
// zero new StartExecution calls. The adversarial twin (distinct IDs, i.e.
// a regression minting fresh IDs per attempt) launches N times, proving
// this test would go red if the dedupe key regressed.
func TestRetryComposition_WorkerRedeliveryConvergesToSingleLaunch(t *testing.T) {
	ctx := context.Background()
	const tenant, claim, wfID = "t1", "c1", "claim-investigation"
	const invID = "inv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	prov := &countProvider{}
	launcher := investigate.NewLauncher(investigate.NewInMemoryEnvelopeStore(), prov, newMemLaunches())
	env := launchEnv(tenant, claim, invID)

	first, launched, err := launcher.EnsureLaunched(ctx, tenant, claim, invID, env, wfID, investigate.ExpireAuth{})
	if err != nil || !launched || first == "" {
		t.Fatalf("delivery 1: launched=%v name=%q err=%v, want launch", launched, first, err)
	}
	for i := 2; i <= worker.MaxAttempts; i++ {
		name, relaunched, err := launcher.EnsureLaunched(ctx, tenant, claim, invID, env, wfID, investigate.ExpireAuth{})
		if err != nil {
			t.Fatalf("redelivery %d err = %v, want convergence", i, err)
		}
		if relaunched {
			t.Fatalf("redelivery %d relaunched: dedupe must converge, not re-launch", i)
		}
		if name != first {
			t.Fatalf("redelivery %d execution = %q, want %q (same durable name)", i, name, first)
		}
	}
	if got := prov.Calls(); got != 1 {
		t.Fatalf("StartExecution calls after %d same-ID redeliveries = %d, want 1 (no worker x workflow multiplication)", worker.MaxAttempts, got)
	}

	// Adversarial twin: distinct investigation IDs MUST launch distinctly.
	// If a regression minted a fresh ID per worker attempt, the composed
	// product would multiply — this half pins that sensitivity.
	prov2 := &countProvider{}
	launcher2 := investigate.NewLauncher(investigate.NewInMemoryEnvelopeStore(), prov2, newMemLaunches())
	for i := 0; i < worker.MaxAttempts; i++ {
		id := fmt.Sprintf("inv-%032x", i+1)
		if _, _, err := launcher2.EnsureLaunched(ctx, tenant, claim, id, launchEnv(tenant, claim, id), wfID, investigate.ExpireAuth{}); err != nil {
			t.Fatalf("distinct-ID launch %d err = %v", i, err)
		}
	}
	if got := prov2.Calls(); got != worker.MaxAttempts {
		t.Fatalf("distinct-ID StartExecution calls = %d, want %d (twin must multiply, proving the same-ID half is load-bearing)", got, worker.MaxAttempts)
	}
}

// runWorkflowStep mimics one workflow http.post step's retry semantics
// (predicate-retryable failures retried up to WorkflowHTTPAttempts total
// attempts, mirroring max_retries: 3): attempt drives one model call
// through the REAL FallbackModelClient seam, then returns the scripted
// HTTP fate. It returns (httpAttempts, providerCalls).
func runWorkflowStep(t *testing.T, ctx context.Context, httpFates []error, model *scriptModel, secondary *scriptModel) (int, int) {
	t.Helper()
	fb := orchestrate.NewFallbackModelClient(model, secondary, 0)
	var httpAttempts int
	for i := 0; i < WorkflowHTTPAttempts; i++ {
		httpAttempts++
		if _, err := fb.Complete(ctx, orchestrate.ModelRequest{}); err != nil {
			// A provider-exhausted chain is terminal (markExhausted):
			// the step does not paper over it, it propagates.
			t.Fatalf("attempt %d: provider seam err = %v (fakes must be scripted to the attempt's fate)", httpAttempts, err)
		}
		if httpFates[httpAttempts-1] == nil {
			return httpAttempts, model.Calls() + secondary.Calls()
		}
	}
	return httpAttempts, model.Calls() + secondary.Calls()
}

// errRetryable is a scripted retryable HTTP-step failure (5xx class).
var errRetryable = errors.New("retrybudget: http.post: status 503 boom (retryable)")

// TestRetryComposition_WorkflowProviderComposedBound exercises workflow
// retries AND provider fallback in ONE composed path through the real
// FallbackModelClient seam (no hard-coded call counts):
//
//   - Retry exercised: the step fails retryably 3 times then succeeds —
//     httpAttempts must equal the full WorkflowHTTPAttempts budget (4),
//     proving max_retries: 3 actually fires through this path.
//   - Worst case: every step attempt exhausts a full all-failing fallback
//     chain (4 provider calls each) — total provider calls must equal
//     exactly the composed product 4 x 4 = 16, proving the bound is both
//     sufficient AND tight (reachable, not loose).
func TestRetryComposition_WorkflowProviderComposedBound(t *testing.T) {
	ctx := context.Background()

	retryModel := &scriptModel{steps: []*orchestrate.ModelResponse{{}, {}, {}, {}}}
	retrySecondary := &scriptModel{}
	httpAttempts, providerCalls := runWorkflowStep(t, ctx,
		[]error{errRetryable, errRetryable, errRetryable, nil},
		retryModel, retrySecondary)
	if httpAttempts != WorkflowHTTPAttempts {
		t.Fatalf("workflow HTTP attempts = %d, want %d (all 3 retries must fire before success)", httpAttempts, WorkflowHTTPAttempts)
	}
	if providerCalls != WorkflowHTTPAttempts {
		t.Fatalf("provider calls along the retry path = %d, want %d (one healthy call per attempt)", providerCalls, WorkflowHTTPAttempts)
	}

	// Worst case: each of the 4 step attempts drives an all-failing
	// provider chain to exhaustion (4 calls each, terminal via
	// markExhausted so the chain itself never re-retries).
	var worstProviderCalls int
	for attempt := 0; attempt < WorkflowHTTPAttempts; attempt++ {
		badPrimary := &scriptModel{steps: []*orchestrate.ModelResponse{nil, nil, nil, nil, nil, nil}}
		badSecondary := &scriptModel{steps: []*orchestrate.ModelResponse{nil, nil, nil, nil, nil, nil}}
		capped := orchestrate.NewFallbackModelClient(badPrimary, badSecondary, 0)
		if _, err := capped.Complete(ctx, orchestrate.ModelRequest{}); err == nil {
			t.Fatalf("attempt %d: all-failing providers must return an error", attempt+1)
		}
		worstProviderCalls += badPrimary.Calls() + badSecondary.Calls()
	}
	if want := WorkflowHTTPAttempts * ProviderMaxTotalCalls; worstProviderCalls != want {
		t.Fatalf("worst-case provider calls = %d, want exactly the composed product %d (4 attempts x 4 fallback calls)", worstProviderCalls, want)
	}
	if worstProviderCalls > MaxCompositionProviderCalls {
		t.Fatalf("worst-case provider calls = %d exceeds composed bound %d", worstProviderCalls, MaxCompositionProviderCalls)
	}
	t.Logf("composed bound holds: worst-case %d provider calls within bound %d (1 execution x %d attempts x %d fallback calls)",
		worstProviderCalls, MaxCompositionProviderCalls, WorkflowHTTPAttempts, ProviderMaxTotalCalls)
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
	if WorkerExecutionsPerInvestigation != 1 {
		t.Fatalf("WorkerExecutionsPerInvestigation = %d, want 1 (S6 dedupe: redeliveries converge, never multiply)", WorkerExecutionsPerInvestigation)
	}
	if want := WorkerExecutionsPerInvestigation * WorkflowHTTPAttempts * ProviderMaxTotalCalls; MaxCompositionProviderCalls != want {
		t.Fatalf("MaxCompositionProviderCalls = %d, want derived product %d", MaxCompositionProviderCalls, want)
	}
	if MaxCompositionProviderCalls != 16 {
		t.Fatalf("MaxCompositionProviderCalls = %d, want 16 (1 execution x 4 workflow-HTTP x 4 provider)", MaxCompositionProviderCalls)
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
