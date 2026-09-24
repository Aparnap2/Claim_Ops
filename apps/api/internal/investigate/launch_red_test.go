package investigate

// S6 RED: worker→investigation persistence + workflow launch chain.
//
// Recovery model under test (EnsureLaunched):
//  1. Ownership: the Launcher persists the authoritative envelope.
//  2. Ordering: envelope durable BEFORE StartExecution.
//  3. Identity: investigation ID propagates worker→launch→agent unchanged.
//  4. Persist-OK/launch-unknown: redelivery looks up durable launch state;
//     absent launch => launch now (never orphan, never skip).
//  5. Post-launch redelivery: existing launch => no second StartExecution.
//  6. Idempotency: durable key = investigation ID; repeats converge.
//  7. Tenant: explicit param, must equal envelope tenant; launch record
//     scoped by tenant; never derived from untrusted payload fields.
//  8. States: persisted-not-launched => launch; launched => converge;
//     terminal/duplicate handled by callers, not by re-launch.
// Invariants: no retry inside EnsureLaunched (caller owns redelivery, so
// worker budget never multiplies workflow budget); bare
// SaveEnvelope()+StartExecution() with no durable state is rejected by Q4.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/workflow"
)

// fakeProvider scripts StartExecution results and counts calls.
type fakeProvider struct {
	mu    sync.Mutex
	calls int
	// script[i] is the error for call i (nil => success); successes beyond
	// the script return success. names[i] is the execution name on success.
	script []error
	names  []string
}

func (f *fakeProvider) StartExecution(_ context.Context, _ string, _ any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i < len(f.script) && f.script[i] != nil {
		return "", f.script[i]
	}
	name := fmt.Sprintf("exec-%d", i+1)
	if i < len(f.names) && f.names[i] != "" {
		name = f.names[i]
	}
	return name, nil
}

func (f *fakeProvider) GetExecution(_ context.Context, name string) (string, json.RawMessage, error) {
	// Models a provider with no executions: typed absence so the
	// reconciler proceeds to start (a generic error would fail closed).
	return "", nil, fmt.Errorf("fake: execution %q absent: %w", name, workflow.ErrExecutionNotFound)
}
func (f *fakeProvider) SendCallback(_ context.Context, _ string, _ any) error {
	return errors.New("fake: no callbacks")
}
func (f *fakeProvider) DeployWorkflow(_ context.Context, _ string, _ string) error {
	return errors.New("fake: no deploy")
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var _ workflow.WorkflowProvider = (*fakeProvider)(nil)

// fakeLaunches is an in-memory LaunchStore: first RecordLaunch wins.
type fakeLaunches struct {
	mu   sync.Mutex
	rows map[string]LaunchRecord
}

func newFakeLaunches() *fakeLaunches { return &fakeLaunches{rows: map[string]LaunchRecord{}} }

func (f *fakeLaunches) RecordLaunch(_ context.Context, rec LaunchRecord) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := rec.TenantID + "\x00" + rec.InvestigationID
	if _, ok := f.rows[k]; ok {
		return false, nil
	}
	f.rows[k] = rec
	return true, nil
}

func (f *fakeLaunches) GetLaunch(_ context.Context, tenant, invID string) (LaunchRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.rows[tenant+"\x00"+invID]
	return rec, ok, nil
}

func launchTestEnv(t *testing.T, tenant, claim, invID string) invest.UnresolvedException {
	t.Helper()
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
			MaxToolCalls: 5, DeadlineMs: 5000, RequestID: "req-s6-launch",
		},
		EvidenceRefs: []invest.EvidenceRef{{
			EvidenceID: "ev-01", SourceType: invest.EvidenceSourceDocument,
			SourceID: "doc-01", TenantID: tenant, ClaimID: claim,
			DocumentID: "doc-01", Page: 1,
		}},
	}
}

const launchTestWorkflow = "claim-investigation"

// Q1+Q2: first Ensure persists the envelope, then launches (ordering).
func TestLaunch_PersistBeforeLaunch_Ordering(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-01", "clm-s6-01", "inv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	launches := newFakeLaunches()
	prov := &fakeProvider{}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	name, launched, err := l.EnsureLaunched(context.Background(), "tnt-s6-01", "clm-s6-01", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil {
		t.Fatalf("EnsureLaunched: %v", err)
	}
	if !launched || name == "" {
		t.Fatalf("launched=%v name=%q, want launched with name", launched, name)
	}
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.callCount())
	}
	// Envelope must be durable (loadable back by tenant+ID).
	got, err := l.Envelopes.LoadEnvelope(context.Background(), "tnt-s6-01", env.InvestigationID)
	if err != nil {
		t.Fatalf("envelope not durable after EnsureLaunched: %v", err)
	}
	if got.InvestigationID != env.InvestigationID {
		t.Fatalf("loaded ID = %q, want %q", got.InvestigationID, env.InvestigationID)
	}
}

// Q3: investigation ID propagates unchanged to the launch record.
func TestLaunch_IdentifierPropagation(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-02", "clm-s6-02", "inv-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	launches := newFakeLaunches()
	prov := &fakeProvider{}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	name, _, err := l.EnsureLaunched(context.Background(), "tnt-s6-02", "clm-s6-02", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil {
		t.Fatalf("EnsureLaunched: %v", err)
	}
	rec, found, err := launches.GetLaunch(context.Background(), "tnt-s6-02", env.InvestigationID)
	if err != nil || !found {
		t.Fatalf("launch record missing: found=%v err=%v", found, err)
	}
	if rec.InvestigationID != env.InvestigationID {
		t.Fatalf("record ID = %q, want %q", rec.InvestigationID, env.InvestigationID)
	}
	if rec.ExecutionName != name || rec.ExecutionName == "" {
		t.Fatalf("record execution = %q, want %q (non-empty)", rec.ExecutionName, name)
	}
	if rec.TenantID != "tnt-s6-02" || rec.ClaimID != "clm-s6-02" {
		t.Fatalf("record tenant/claim = %q/%q, want tnt-s6-02/clm-s6-02", rec.TenantID, rec.ClaimID)
	}
}

// Q4: persist-OK + launch-FAILED => redelivery launches (no orphan, no skip).
func TestLaunch_PersistOKLaunchFailed_RedeliveryLaunches(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-03", "clm-s6-03", "inv-cccccccccccccccccccccccccccccccc")
	launches := newFakeLaunches()
	prov := &fakeProvider{script: []error{fmt.Errorf("workflow down")}}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	_, _, err := l.EnsureLaunched(context.Background(), "tnt-s6-03", "clm-s6-03", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{})
	if err == nil {
		t.Fatal("want launch error, got success")
	}
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (no internal retry)", prov.callCount())
	}
	// Envelope durable despite launch failure...
	if _, err := l.Envelopes.LoadEnvelope(context.Background(), "tnt-s6-03", env.InvestigationID); err != nil {
		t.Fatalf("envelope must survive launch failure: %v", err)
	}
	// ...and no launch recorded...
	if _, found, _ := launches.GetLaunch(context.Background(), "tnt-s6-03", env.InvestigationID); found {
		t.Fatal("failed launch must not record launch state")
	}
	// ...so redelivery launches exactly once more and converges.
	name, launched, err := l.EnsureLaunched(context.Background(), "tnt-s6-03", "clm-s6-03", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil || !launched || name == "" {
		t.Fatalf("redelivery: launched=%v name=%q err=%v, want launch", launched, name, err)
	}
	if prov.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2 (one per delivery, never duplicated)", prov.callCount())
	}
}

// Q5+Q6: post-launch redelivery converges with zero new provider calls.
func TestLaunch_AlreadyLaunched_NoDuplicateStart(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-04", "clm-s6-04", "inv-dddddddddddddddddddddddddddddddd")
	launches := newFakeLaunches()
	prov := &fakeProvider{}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	first, launched, err := l.EnsureLaunched(context.Background(), "tnt-s6-04", "clm-s6-04", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil || !launched {
		t.Fatalf("first: launched=%v err=%v", launched, err)
	}
	second, launched2, err := l.EnsureLaunched(context.Background(), "tnt-s6-04", "clm-s6-04", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if launched2 {
		t.Fatal("redelivery must report already-launched (launched=false)")
	}
	if second != first {
		t.Fatalf("redelivery execution = %q, want %q (same durable name)", second, first)
	}
	if prov.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (no duplicate StartExecution)", prov.callCount())
	}
}

// Q7: tenant comes from explicit params and must equal the envelope tenant.
func TestLaunch_TenantMismatch_NoPersistNoLaunch(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-05", "clm-s6-05", "inv-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	launches := newFakeLaunches()
	prov := &fakeProvider{}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	_, _, err := l.EnsureLaunched(context.Background(), "tnt-s6-EVIL", "clm-s6-05", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{})
	if err == nil {
		t.Fatal("want tenant-mismatch error, got success")
	}
	if prov.callCount() != 0 {
		t.Fatalf("provider calls = %d, want 0 (mismatch launches nothing)", prov.callCount())
	}
	if _, err := l.Envelopes.LoadEnvelope(context.Background(), "tnt-s6-05", env.InvestigationID); err == nil {
		t.Fatal("mismatched envelope must not be persisted")
	}
}

// Q7b: launch state is tenant-scoped (cross-tenant lookup misses).
func TestLaunch_TenantScoping(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-06", "clm-s6-06", "inv-ffffffffffffffffffffffffffffffff")
	launches := newFakeLaunches()
	prov := &fakeProvider{}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	if _, _, err := l.EnsureLaunched(context.Background(), "tnt-s6-06", "clm-s6-06", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{}); err != nil {
		t.Fatalf("EnsureLaunched: %v", err)
	}
	if _, found, _ := launches.GetLaunch(context.Background(), "tnt-s6-OTHER", env.InvestigationID); found {
		t.Fatal("cross-tenant launch lookup must miss")
	}
}

// Q8: blank/invalid identifiers fail closed before any side effect.
func TestLaunch_InvalidIdentifiers_FailClosed(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-07", "clm-s6-07", "inv-00000000000000000000000000000007")
	launches := newFakeLaunches()
	prov := &fakeProvider{}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	for name, tc := range map[string]struct{ tenant, claim, inv, wf string }{
		"blank tenant": {"", "clm-s6-07", env.InvestigationID, launchTestWorkflow},
		"blank claim":  {"tnt-s6-07", "", env.InvestigationID, launchTestWorkflow},
		"bad inv":      {"tnt-s6-07", "clm-s6-07", "nope", launchTestWorkflow},
		"blank wf":     {"tnt-s6-07", "clm-s6-07", env.InvestigationID, ""},
	} {
		if _, _, err := l.EnsureLaunched(context.Background(), tc.tenant, tc.claim, tc.inv, env, tc.wf, ExpireAuth{}); err == nil {
			t.Fatalf("%s: want contract error, got success", name)
		}
	}
	if prov.callCount() != 0 {
		t.Fatalf("provider calls = %d, want 0 (validation first)", prov.callCount())
	}
}

// S5: the pre-signed expire credential travels opaquely in the launch
// argument when present, and is omitted when absent.
type argCaptureProvider struct {
	fakeProvider
	lastArg map[string]string
}

func (f *argCaptureProvider) StartExecution(ctx context.Context, workflowID string, arg any) (string, error) {
	if m, ok := arg.(map[string]string); ok {
		cp := make(map[string]string, len(m))
		for k, v := range m {
			cp[k] = v
		}
		f.lastArg = cp
	}
	return f.fakeProvider.StartExecution(ctx, workflowID, arg)
}

func TestLaunch_ExpireAuth_FlowIntoArgument(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-08", "clm-s6-08", "inv-88888888888888888888888888888888")
	launches := newFakeLaunches()
	prov := &argCaptureProvider{}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	auth := ExpireAuth{Body: `{"action":"EXPIRE"}`, Signature: "abc123"}
	if _, _, err := l.EnsureLaunched(context.Background(), "tnt-s6-08", "clm-s6-08", env.InvestigationID, env, launchTestWorkflow, auth); err != nil {
		t.Fatalf("EnsureLaunched: %v", err)
	}
	if prov.lastArg["expire_body"] != auth.Body || prov.lastArg["expire_signature"] != auth.Signature {
		t.Fatalf("argument = %v, want expire_body/signature forwarded opaquely", prov.lastArg)
	}
	if prov.lastArg["idempotency_key"] != env.InvestigationID {
		t.Fatalf("argument missing idempotency_key: %v", prov.lastArg)
	}
}

func TestLaunch_ExpireAuth_AbsentWhenZero(t *testing.T) {
	env := launchTestEnv(t, "tnt-s6-09", "clm-s6-09", "inv-99999999999999999999999999999999")
	launches := newFakeLaunches()
	prov := &argCaptureProvider{}
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, launches)

	if _, _, err := l.EnsureLaunched(context.Background(), "tnt-s6-09", "clm-s6-09", env.InvestigationID, env, launchTestWorkflow, ExpireAuth{}); err != nil {
		t.Fatalf("EnsureLaunched: %v", err)
	}
	if _, ok := prov.lastArg["expire_body"]; ok {
		t.Fatalf("zero auth must omit expire_body: %v", prov.lastArg)
	}
	if _, ok := prov.lastArg["expire_signature"]; ok {
		t.Fatalf("zero auth must omit expire_signature: %v", prov.lastArg)
	}
}
