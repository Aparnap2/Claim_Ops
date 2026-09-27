package investigate

// APA-41: the launch reconciler probed a BARE execution id where a FULL
// GCW resource name is required.
//
// Invariant under test (owner-mandated, APA-41):
//
//	execution ID != execution resource name
//
// The bare deterministic id is authoritative for GCW's caller-chosen
// `executionId` (the StartExecution argument, PR #105). The full resource
// name — projects/{p}/locations/{l}/workflows/{w}/executions/{e} — is the
// only thing GetExecution can resolve (gcw_provider.go: "a bare execution id
// is NOT a valid input to GetExecution").
//
// The provider below is contract-shaped: it keys executions by FULL resource
// name only. A bare id resolves to typed absence, exactly as the emulator's
// GET /v1/exec-<hex> returns 404.
//
// TestAPA41_Reconcile_ProbesBareID_NotResourceName is the RED
// reproduction: before the fix the reconciler sent the bare id, read typed
// absence, and started a duplicate execution. The remaining tests pin the
// fixed behaviour and the persisted-shape compatibility the append-only
// column requires.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"claimops-api/internal/workflow"
)

// redProject and redLocation are the provider's own naming scope. The
// reconciler must not have to know them: the provider owns the mapping.
const (
	redProject  = "claimops-live"
	redLocation = "us-central1"
)

// redResourceName is the full resource name shape GCW mints and accepts.
func redResourceName(workflowID, executionID string) string {
	return fmt.Sprintf("projects/%s/locations/%s/workflows/%s/executions/%s",
		redProject, redLocation, workflowID, executionID)
}

// redContractProvider is a contract-shaped WorkflowProvider: its registry is
// keyed by FULL resource name, and it can resolve nothing else.
//
// It also carries ExecutionResourceName because that is the capability the
// fix requires of a provider. On the unfixed code the Launcher never calls
// it — which is precisely the defect under test.
type redContractProvider struct {
	mu     sync.Mutex
	starts int
	execs  map[string]string
}

func newRedContractProvider() *redContractProvider {
	return &redContractProvider{execs: map[string]string{}}
}

// ExecutionResourceName is the provider's naming authority. Additive
// capability; the unfixed Launcher ignores it.
func (p *redContractProvider) ExecutionResourceName(workflowID, executionID string) string {
	return redResourceName(workflowID, executionID)
}

// StartExecution registers under the FULL resource name derived from the
// bare execution_name argument — mirroring PR #105, where the bare id is
// forwarded as GCW's executionId and the provider mints the full name.
func (p *redContractProvider) StartExecution(_ context.Context, workflowID string, argument any) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.starts++
	bare := redBareID(argument)
	full := p.ExecutionResourceName(workflowID, bare)
	p.execs[full] = "ACTIVE"
	return full, nil
}

// GetExecution resolves FULL names only. A bare id — anything without the
// "projects/.../executions/" collection prefix — is a typed absence, which
// is what the emulator's 404 on GET /v1/exec-<hex> produces.
func (p *redContractProvider) GetExecution(_ context.Context, name string) (string, json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !strings.HasPrefix(name, "projects/") || !strings.Contains(name, "/executions/") {
		return "", nil, fmt.Errorf("red: %q is a bare execution id, not a resource name: %w",
			name, workflow.ErrExecutionNotFound)
	}
	if state, ok := p.execs[name]; ok {
		return state, nil, nil
	}
	return "", nil, fmt.Errorf("red: execution %q absent: %w", name, workflow.ErrExecutionNotFound)
}

func (p *redContractProvider) SendCallback(_ context.Context, _ string, _ any) error {
	return fmt.Errorf("red: no callbacks")
}

func (p *redContractProvider) DeployWorkflow(_ context.Context, _ string, _ string) error {
	return fmt.Errorf("red: no deploy")
}

func (p *redContractProvider) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

// executionCount is the number of DISTINCT executions the provider holds.
func (p *redContractProvider) executionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.execs)
}

func (p *redContractProvider) has(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.execs[name]
	return ok
}

// redBareID extracts the bare execution_name argument the Launcher passes.
func redBareID(argument any) string {
	switch v := argument.(type) {
	case map[string]string:
		return v["execution_name"]
	case map[string]any:
		if s, ok := v["execution_name"].(string); ok {
			return s
		}
	}
	return ""
}

var _ workflow.WorkflowProvider = (*redContractProvider)(nil)

// TestAPA41_Reconcile_ProbesBareID_NotResourceName reproduces APA-41.
//
// Arrange: the provider already holds an execution for this investigation
// under its FULL resource name (crash between StartExecution and
// RecordLaunch). The durable launch row is absent.
// Act: redelivery calls EnsureLaunched.
// Assert: the reconciler resolves the existing execution and adopts it —
// one execution total, no new start.
//
// RED today: the probe sends the bare id, the provider answers typed
// absence, and the launcher starts a duplicate.
func TestAPA41_Reconcile_ProbesBareID_NotResourceName(t *testing.T) {
	const tenant, claim = "tnt-apa41-01", "clm-apa41-01"
	const invID = "inv-eeeeeeeeeeeeeeeeeeeeeeeeeeeeee01"
	const wfID = launchTestWorkflow

	env := launchTestEnv(t, tenant, claim, invID)
	prov := newRedContractProvider()
	store := newFakeLaunches()
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
	ctx := context.Background()

	// Arrange: pre-seed the crash window. The bare id is deterministic, so
	// the reconciler must arrive at exactly this execution.
	bare := executionIDFor(wfID, invID)
	full := redResourceName(wfID, bare)
	prov.execs[full] = "ACTIVE"

	// Act: redelivery after the crash.
	name, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, wfID, ExpireAuth{})

	// Assert: adoption, not a second start.
	if err != nil {
		t.Fatalf("EnsureLaunched after crash window: %v", err)
	}
	if launched {
		t.Errorf("launched=true: reconciler probed %q, which the provider cannot resolve; a duplicate execution was started", bare)
	}
	if name != full {
		t.Errorf("returned name = %q, want the full resource name %q that GetExecution can resolve", name, full)
	}
	if got := prov.startCount(); got != 0 {
		t.Errorf("StartExecution calls = %d, want 0 (existing execution must be adopted)", got)
	}
	if got := prov.executionCount(); got != 1 {
		t.Errorf("provider holds %d executions, want exactly 1 (duplicate created)", got)
	}

	// Assert: the durable row is repaired with the resolvable name.
	rec, found, err := store.GetLaunch(ctx, tenant, invID)
	if err != nil || !found {
		t.Fatalf("launch row must be recorded by adoption: found=%v err=%v", found, err)
	}
	if rec.ExecutionName != full {
		t.Errorf("persisted execution_name = %q, want full resource name %q", rec.ExecutionName, full)
	}
	if !prov.has(full) {
		t.Errorf("adopted name %q is not an execution the provider holds", full)
	}
}

// TestAPA41_AbsentExecution_TakesNormalLaunchPath is the guard against
// over-correcting into FALSE ADOPTION. When nothing exists under the full
// resource name, the reconciler must read typed absence and start exactly
// one execution — it must not "adopt" an execution it never found, and it
// must not skip the start.
func TestAPA41_AbsentExecution_TakesNormalLaunchPath(t *testing.T) {
	const tenant, claim = "tnt-apa41-02", "clm-apa41-02"
	const invID = "inv-eeeeeeeeeeeeeeeeeeeeeeeeeeeeee02"
	const wfID = launchTestWorkflow

	env := launchTestEnv(t, tenant, claim, invID)
	prov := newRedContractProvider() // registry deliberately empty
	store := newFakeLaunches()
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
	ctx := context.Background()

	// Act: first delivery, nothing pre-existing.
	name, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, wfID, ExpireAuth{})

	// Assert: a real launch happened.
	if err != nil {
		t.Fatalf("EnsureLaunched: %v", err)
	}
	if !launched {
		t.Fatal("launched=false: typed absence must proceed to start, never false-adopt")
	}
	if got := prov.startCount(); got != 1 {
		t.Fatalf("StartExecution calls = %d, want exactly 1", got)
	}
	if got := prov.executionCount(); got != 1 {
		t.Fatalf("provider holds %d executions, want exactly 1", got)
	}

	// Assert: the recorded name is the provider's full resource name, and it
	// is an execution the provider actually holds (not a fabricated adopt).
	want := redResourceName(wfID, executionIDFor(wfID, invID))
	if name != want {
		t.Fatalf("returned name = %q, want full resource name %q", name, want)
	}
	rec, found, err := store.GetLaunch(ctx, tenant, invID)
	if err != nil || !found {
		t.Fatalf("launch row must be recorded: found=%v err=%v", found, err)
	}
	if rec.ExecutionName != want {
		t.Fatalf("persisted execution_name = %q, want %q", rec.ExecutionName, want)
	}
	if !prov.has(name) {
		t.Fatalf("returned name %q is not an execution the provider holds", name)
	}
}

// TestAPA41_BothPersistedShapesRemainInterpretable pins area 5: the
// workflow_launches.execution_name column is append-only (REVOKE UPDATE,
// DELETE), so rows written before APA-41 hold a BARE id and rows written
// after hold a FULL resource name. BOTH must remain readable, and neither
// may be re-derived or rewritten on the lookup path.
//
// No backfill exists by design, so this is the compatibility contract.
func TestAPA41_BothPersistedShapesRemainInterpretable(t *testing.T) {
	const wfID = launchTestWorkflow

	// Two investigations, one row per historical shape.
	const bareInvID = "inv-eeeeeeeeeeeeeeeeeeeeeeeeeeeeee03"
	const fullInvID = "inv-eeeeeeeeeeeeeeeeeeeeeeeeeeeeee04"
	bareShape := executionIDFor(wfID, bareInvID)
	fullShape := redResourceName(wfID, executionIDFor(wfID, fullInvID))

	for _, tc := range []struct {
		name       string
		invID      string
		tenant     string
		claim      string
		storedName string
	}{
		{"historical bare id", bareInvID, "tnt-apa41-03", "clm-apa41-03", bareShape},
		{"full resource name", fullInvID, "tnt-apa41-04", "clm-apa41-04", fullShape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := launchTestEnv(t, tc.tenant, tc.claim, tc.invID)
			prov := newRedContractProvider()
			store := newFakeLaunches()
			l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
			ctx := context.Background()

			// Arrange: the row already exists, written by a previous era.
			seed := LaunchRecord{
				TenantID: tc.tenant, ClaimID: tc.claim,
				InvestigationID: tc.invID, WorkflowID: wfID,
				ExecutionName: tc.storedName,
			}
			if _, err := store.RecordLaunch(ctx, seed); err != nil {
				t.Fatalf("seed launch row: %v", err)
			}

			// Act: redelivery reads the row.
			got, launched, err := l.EnsureLaunched(ctx, tc.tenant, tc.claim, tc.invID, env, wfID, ExpireAuth{})

			// Assert: converge on the STORED value verbatim, whatever its shape.
			if err != nil {
				t.Fatalf("EnsureLaunched: %v", err)
			}
			if launched {
				t.Fatal("launched=true: a present launch row must converge, never re-launch")
			}
			if got != tc.storedName {
				t.Fatalf("returned name = %q, want the stored value %q verbatim (no re-derivation)", got, tc.storedName)
			}
			// Assert: no provider traffic at all on the found path, and the
			// row was not rewritten.
			if n := prov.startCount(); n != 0 {
				t.Fatalf("StartExecution calls = %d, want 0", n)
			}
			rec, found, err := store.GetLaunch(ctx, tc.tenant, tc.invID)
			if err != nil || !found {
				t.Fatalf("row must remain: found=%v err=%v", found, err)
			}
			if rec.ExecutionName != tc.storedName {
				t.Fatalf("stored execution_name mutated: %q, want %q", rec.ExecutionName, tc.storedName)
			}
		})
	}
}
