package investigate

// S6 crash-consistency: StartExecution succeeds but RecordLaunch never
// commits (crash window) => redelivery must reconcile via the deterministic
// execution identity instead of starting a duplicate.
//
// The identity has two distinct forms (APA-41 — they are NOT interchangeable):
//   - executionIDFor(workflowID, invID) is a BARE deterministic id,
//     "exec-" + first 32 hex of
//     sha256("claimops-execution-v1\x00"+workflowID+"\x00"+invID). It is what
//     the launch argument forwards as GCW's caller-chosen executionId.
//   - the FULL resource name the provider mints from that id is the only
//     form GetExecution can resolve, and it is what reconciliation probes.
//
// EnsureLaunched reconcile on launch-state miss: GetExecution(fullName);
// found => RecordLaunch adopt, return (name,false,nil) with NO new start;
// typed not-found (errors.Is err, workflow.ErrExecutionNotFound) => start;
// any other GetExecution error => fail closed, no start.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"claimops-api/internal/workflow"
)

// crashExpectedExecutionID inlines the documented derivation to pin the
// contract independently of executionIDFor. It yields a BARE id, never a
// resource name (APA-41).
func crashExpectedExecutionID(workflowID, invID string) string {
	sum := sha256.Sum256([]byte("claimops-execution-v1\x00" + workflowID + "\x00" + invID))
	return "exec-" + hex.EncodeToString(sum[:])[:32]
}

// registryFakeProvider is a contract-shaped WorkflowProvider with an
// execution registry, mirroring real GCW (APA-41):
//
//   - the registry is keyed by FULL resource name;
//   - StartExecution receives the bare execution_name argument, registers
//     the execution under the full resource name it mints from it (PR #105),
//     and returns that full name;
//   - GetExecution resolves full names ONLY — a bare id is typed absence,
//     exactly as GET /v1/exec-<hex> 404s against the emulator.
//
// A configured getErr overrides everything (outage simulation).
type registryFakeProvider struct {
	mu     sync.Mutex
	starts int
	execs  map[string]string
	getErr error
}

func newRegistryFake(getErr error) *registryFakeProvider {
	return &registryFakeProvider{execs: map[string]string{}, getErr: getErr}
}

// ExecutionResourceName is the provider's naming authority (APA-41). The
// Launcher must call this rather than assembling the prefix itself.
func (f *registryFakeProvider) ExecutionResourceName(workflowID, executionID string) string {
	return fmt.Sprintf("projects/fake/locations/fake/workflows/%s/executions/%s", workflowID, executionID)
}

func (f *registryFakeProvider) StartExecution(_ context.Context, workflowID string, arg any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	bare := crashExecNameOf(arg)
	if bare == "" {
		bare = fmt.Sprintf("exec-fallback-%d", f.starts)
	}
	full := f.ExecutionResourceName(workflowID, bare)
	f.execs[full] = "SUCCEEDED"
	return full, nil
}

// GetExecution resolves full resource names only; a bare id cannot be
// addressed and reads as typed absence (APA-41).
func (f *registryFakeProvider) GetExecution(_ context.Context, name string) (string, json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return "", nil, f.getErr
	}
	if state, ok := f.execs[name]; ok {
		return state, nil, nil
	}
	return "", nil, fmt.Errorf("fake: execution %q absent: %w", name, workflow.ErrExecutionNotFound)
}

func (f *registryFakeProvider) SendCallback(_ context.Context, _ string, _ any) error {
	return errors.New("fake: no callbacks")
}

func (f *registryFakeProvider) DeployWorkflow(_ context.Context, _ string, _ string) error {
	return errors.New("fake: no deploy")
}

func (f *registryFakeProvider) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

var _ workflow.WorkflowProvider = (*registryFakeProvider)(nil)

// crashExecNameOf extracts the bare execution_name from the StartExecution
// argument (production passes map[string]string).
func crashExecNameOf(arg any) string {
	switch v := arg.(type) {
	case map[string]string:
		return v["execution_name"]
	case map[string]any:
		if s, ok := v["execution_name"].(string); ok {
			return s
		}
	}
	return ""
}

// crashFailFirstStore fails the first RecordLaunch only (crash between start
// and record), then behaves as first-write-wins tenant-scoped memory.
type crashFailFirstStore struct {
	mu       sync.Mutex
	rows     map[string]LaunchRecord
	failNext bool
}

func newCrashFailFirstStore(failFirst bool) *crashFailFirstStore {
	return &crashFailFirstStore{rows: map[string]LaunchRecord{}, failNext: failFirst}
}

func (f *crashFailFirstStore) RecordLaunch(_ context.Context, rec LaunchRecord) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return false, errors.New("crash: commit failed between start and record")
	}
	k := rec.TenantID + "\x00" + rec.InvestigationID
	if _, ok := f.rows[k]; ok {
		return false, nil
	}
	f.rows[k] = rec
	return true, nil
}

func (f *crashFailFirstStore) GetLaunch(_ context.Context, tenant, invID string) (LaunchRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.rows[tenant+"\x00"+invID]
	return rec, ok, nil
}

// TestLaunch_CrashBetweenStartAndRecord_NoDuplicate pins the S6 crash window:
// StartExecution commits provider-side but RecordLaunch never commits.
// Redelivery must reconcile (adopt) with zero new starts.
func TestLaunch_CrashBetweenStartAndRecord_NoDuplicate(t *testing.T) {
	const tenant, claim = "tnt-s6-crash-01", "clm-s6-crash-01"
	const invID = "inv-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01"
	env := launchTestEnv(t, tenant, claim, invID)
	store := newCrashFailFirstStore(true)
	prov := newRegistryFake(nil)
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
	ctx := context.Background()

	// Arrange: first delivery starts the execution but the launch commit fails.
	// Act:
	_, _, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{})
	// Assert: the crash surfaces as an error (no silent orphan).
	if err == nil {
		t.Fatal("first EnsureLaunched: want record-crash error, got success")
	}
	if got := prov.startCount(); got != 1 {
		t.Fatalf("after crash: StartExecution count = %d, want 1", got)
	}

	// Arrange: redelivery of the same envelope after the crash.
	// Act:
	name, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{})
	// Assert: reconcile adopts the existing execution, starts nothing new.
	if err != nil {
		t.Fatalf("redelivery EnsureLaunched: %v", err)
	}
	if launched {
		t.Fatal("redelivery must adopt (launched=false), got launched=true (duplicate)")
	}
	// The adopted identity is the FULL resource name the provider minted
	// from the bare id — the same form GetExecution can resolve (APA-41),
	// not the bare id itself.
	wantID := crashExpectedExecutionID(launchTestWorkflow, invID)
	want := prov.ExecutionResourceName(launchTestWorkflow, wantID)
	if name != want {
		t.Fatalf("adopted name = %q, want full resource name %q", name, want)
	}
	if got := prov.startCount(); got != 1 {
		t.Fatalf("StartExecution count across both calls = %d, want exactly 1 (no duplicate)", got)
	}
	rec, found, err := store.GetLaunch(ctx, tenant, invID)
	if err != nil || !found {
		t.Fatalf("adopted launch must be recorded: found=%v err=%v", found, err)
	}
	if rec.ExecutionName != want {
		t.Fatalf("recorded execution = %q, want %q", rec.ExecutionName, want)
	}
}

// TestLaunch_ReconcileOutage_FailsClosed: a non-not-found GetExecution error
// (lookup outage) must fail closed with zero StartExecution calls.
func TestLaunch_ReconcileOutage_FailsClosed(t *testing.T) {
	const tenant, claim = "tnt-s6-crash-02", "clm-s6-crash-02"
	const invID = "inv-bbbbbbbbbbbbbbbbbbbbbbbbbbbbb002"
	env := launchTestEnv(t, tenant, claim, invID)
	prov := newRegistryFake(errors.New("reconcile store unavailable"))
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, newFakeLaunches())
	ctx := context.Background()

	// Arrange: launch state absent, reconcile lookup outages.
	// Act:
	_, _, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{})
	// Assert: fail closed, never start on unknown reconcile state.
	if err == nil {
		t.Fatal("outage EnsureLaunched: want reconcile error, got success")
	}
	if got := prov.startCount(); got != 0 {
		t.Fatalf("StartExecution count on reconcile outage = %d, want 0 (fail closed)", got)
	}
}

// TestLaunch_ExecutionID_Deterministic pins the ID derivation contract:
// same inputs => same id; different invID => different; non-empty exec-
// prefix; equals the documented inline derivation. It also pins that the
// derivation yields a BARE id and never a resource name (APA-41) — a bare
// id is what GCW accepts as executionId, and conflating the two is the
// defect this issue fixes.
func TestLaunch_ExecutionID_Deterministic(t *testing.T) {
	const wf = "claim-investigation"
	const invA = "inv-cccccccccccccccccccccccccccccc03"
	const invB = "inv-dddddddddddddddddddddddddddd04"

	// Arrange + Act:
	a1 := executionIDFor(wf, invA)
	a2 := executionIDFor(wf, invA)
	b := executionIDFor(wf, invB)

	// Assert: determinism, sensitivity, shape, and documented derivation.
	if a1 == "" || !strings.HasPrefix(a1, "exec-") {
		t.Fatalf("execution id = %q, want non-empty exec- prefix", a1)
	}
	if a1 != a2 {
		t.Fatalf("same inputs gave different ids: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("different invID gave same id %q (must differ)", a1)
	}
	if want := crashExpectedExecutionID(wf, invA); a1 != want {
		t.Fatalf("executionIDFor = %q, want documented derivation %q", a1, want)
	}
	// The id must stay bare: it is forwarded verbatim as GCW's
	// caller-chosen executionId, and the provider mints the resource name.
	if strings.Contains(a1, "projects/") || strings.Contains(a1, "/") {
		t.Fatalf("execution id %q contains a collection path; it must be a bare id", a1)
	}
}
