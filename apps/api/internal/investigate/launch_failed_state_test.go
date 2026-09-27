package investigate

// APA-42 at the reconciler seam: a RESOLVED execution must be adopted, whatever
// its outcome — including a terminal FAILED one.
//
// R5 (measured live, reproduced here): GCWProvider.GetExecution reported a
// resolved-but-FAILED execution as a non-nil error that does NOT wrap
// workflow.ErrExecutionNotFound. launch.go:261-276 adopts only when the probe
// error is nil and starts only when it is ErrExecutionNotFound, so a
// crash-window execution that had already terminated was classified as a LOOKUP
// OUTAGE: never adopted, the durable workflow_launches row never repaired, and
// every redelivery retried. That is a provider/reconciler property, not an
// emulator artifact — a real GCW execution that failed (including via the
// workflow's own timeout → EXPIRE path) is misread identically.
//
// These tests drive the REAL Launcher over the REAL GCWProvider against an
// httptest GCW endpoint that models create-or-return, so the reconciler's
// probe, the provider's classification and the durable store are all the
// production code. Nothing here special-cases a fake: the endpoint returns
// whatever state the test puts the execution in.
//
// INVARIANT under test, unchanged in shape from APA-41:
//	probe resolves  => adopt the execution, repair the row, start nothing
//	typed absence   => normal launch path
//	lookup failure  => fail closed, start nothing, record nothing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"claimops-api/internal/workflow"
)

const (
	apa42Project  = "claimops-live"
	apa42Location = "us-central1"
)

// gcwStub is a minimal GCW endpoint: it honours the caller-chosen executionId
// (real GCW does; the emulator used in local qualification does not — see the
// R1 finding in internal/worker/live_gcw_crash_window_test.go) and serves
// execution state by full resource name, exactly the address the reconciler
// probes.
type gcwStub struct {
	mu       sync.Mutex
	execs    map[string]string // full resource name -> state
	creates  int
	probes   int
	getFails int // when non-zero, every GET answers this status instead
}

func newGCWStub() *gcwStub { return &gcwStub{execs: map[string]string{}} }

func (s *gcwStub) setState(fullName, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs[fullName] = state
}

func (s *gcwStub) counts() (creates, probes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates, s.probes
}

func (s *gcwStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost:
		// POST /v1/projects/{p}/locations/{l}/workflows/{wf}/executions[?executionId=]
		prefix := "/v1/projects/" + apa42Project + "/locations/" + apa42Location +
			"/workflows/" + launchTestWorkflow + "/executions"
		if r.URL.Path != prefix {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"status":"NOT_FOUND"}}`))
			return
		}
		bare := r.URL.Query().Get("executionId")
		if bare == "" {
			bare = "exec-unidentified"
		}
		full := s.ExecutionResourceName(bare)
		s.mu.Lock()
		_, exists := s.execs[full]
		if !exists {
			s.execs[full] = "ACTIVE"
			s.creates++
		}
		s.mu.Unlock()
		if exists {
			// Create-or-return: GCW's ALREADY_EXISTS for a taken identifier.
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":6,"status":"ALREADY_EXISTS"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"` + full + `","state":"ACTIVE"}`))

	case r.Method == http.MethodGet:
		full := strings.TrimPrefix(r.URL.Path, "/v1/")
		s.mu.Lock()
		s.probes++
		status, state := s.getFails, s.execs[full]
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`upstream unavailable`))
			return
		}
		if state == "" {
			// A real GCW answers a missing resource with a structured 404.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Execution not found.","status":"NOT_FOUND"}}`))
			return
		}
		body := `{"name":"` + full + `","state":"` + state + `"`
		if state == "FAILED" {
			// Realistic terminal shape: the outcome payload lives in
			// result/error, never in the HTTP status of the GET.
			body += `,"result":"{\"message\":\"step investigate failed\"}",` +
				`"error":{"payload":"step investigate failed","context":"workflow timed out"}`
		}
		_, _ = w.Write([]byte(body + `}`))

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ExecutionResourceName mirrors the GCW shape: the provider owns it, and the
// reconciler must obtain the same value through workflow.GCWProvider.
func (s *gcwStub) ExecutionResourceName(bareID string) string {
	return fmt.Sprintf("projects/%s/locations/%s/workflows/%s/executions/%s",
		apa42Project, apa42Location, launchTestWorkflow, bareID)
}

// startGCW wires the stub to the real provider and the real launcher.
func startGCW(t *testing.T, stub *gcwStub) (workflow.WorkflowProvider, func()) {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return workflow.NewGCWProviderWithClient(srv.URL, apa42Project, apa42Location, srv.Client()), srv.Close
}

// apa42ExpectedName is the identity the reconciler probes and the stub mints —
// the provider's own naming authority, never a hand-assembled string.
func apa42ExpectedName(t *testing.T, prov workflow.WorkflowProvider, invID string) string {
	t.Helper()
	bare := crashExpectedExecutionID(launchTestWorkflow, invID)
	return prov.ExecutionResourceName(launchTestWorkflow, bare)
}

// AREA 6 (THE DEFECT): the crash window, with the execution already FAILED by
// the time redelivery probes. It must be ADOPTED: the durable row repaired, no
// second start, and no repeated probe loop afterwards.
func TestLaunch_CrashWindow_ResolvedFailedExecution_IsAdopted(t *testing.T) {
	const tenant, claim = "tnt-apa42-01", "clm-apa42-01"
	const invID = "inv-eeeeeeeeeeeeeeeeeeeeeeeeeeeeee42"
	env := launchTestEnv(t, tenant, claim, invID)

	stub := newGCWStub()
	prov, _ := startGCW(t, stub)
	store := newCrashFailFirstStore(true) // RecordLaunch never commits: the crash
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
	ctx := context.Background()

	// Delivery 1: the start commits provider-side, the durable row does not.
	if _, _, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{}); err == nil {
		t.Fatal("delivery 1: want the record crash surfaced as an error, got success")
	}
	creates, probes := stub.counts()
	if creates != 1 {
		t.Fatalf("delivery 1: creates=%d, want exactly 1 provider start", creates)
	}
	if probes != 1 {
		t.Fatalf("delivery 1: probes=%d, want 1 (the pre-start absence probe that authorised the start)", probes)
	}
	want := apa42ExpectedName(t, prov, invID)

	// The execution terminates FAILED before redelivery reaches the seam:
	// this is the state a real GCW reports, and the state the provider used to
	// report as a lookup failure.
	stub.setState(want, "FAILED")

	// Delivery 2: redelivery must ADOPT, not fail closed and not duplicate.
	name, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil {
		t.Fatalf("redelivery after a FAILED crash-window execution: err = %v, want adoption "+
			"(a resolved execution is present, whatever its outcome)", err)
	}
	if launched {
		t.Fatal("redelivery must adopt (launched=false), got launched=true (duplicate)")
	}
	if name != want {
		t.Fatalf("adopted name = %q, want the probed resource name %q", name, want)
	}
	creates, probes = stub.counts()
	if creates != 1 {
		t.Fatalf("creates = %d, want exactly 1 — a FAILED execution must not trigger a second start", creates)
	}
	if probes != 2 {
		t.Fatalf("probes = %d, want 2 (delivery 1 absence probe + the reconciler probe)", probes)
	}

	// The durable row is REPAIRED, holding the adopted identity.
	rec, found, err := store.GetLaunch(ctx, tenant, invID)
	if err != nil || !found {
		t.Fatalf("adopted launch must be recorded: found=%v err=%v", found, err)
	}
	if rec.ExecutionName != want {
		t.Fatalf("recorded execution = %q, want %q", rec.ExecutionName, want)
	}

	// How the FAILED state reaches the caller: the provider's FIRST return
	// value, alongside a nil error. That is the channel the reconciler reads
	// and any other caller can read — a resolved-but-failed execution is
	// reported, never an opaque success and never an error to parse.
	state, payload, gerr := prov.GetExecution(ctx, want)
	if gerr != nil {
		t.Fatalf("GetExecution(%q) on a resolved FAILED execution = %v, want nil: "+
			"the lookup succeeded, so a terminal outcome must not read as an outage", want, gerr)
	}
	if state != "FAILED" {
		t.Fatalf("state = %q, want FAILED reported to the caller", state)
	}
	if !strings.Contains(string(payload), "step investigate failed") {
		t.Fatalf("payload = %s, want the execution's own failure detail surfaced with the state", string(payload))
	}

	// Delivery 3: the repaired row converges. No probe, no start, no loop.
	name, launched, err = l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil || launched || name != want {
		t.Fatalf("delivery 3: name=%q launched=%v err=%v, want converge on %q", name, launched, err, want)
	}
	if creates, probes = stub.counts(); creates != 1 || probes != 3 {
		t.Fatalf("after convergence: creates=%d probes=%d, want 1/3 (no retry loop)", creates, probes)
	}
}

// AREA 6 control: a resolved ACTIVE execution adopts identically. The fix must
// not make adoption depend on the terminal state.
func TestLaunch_CrashWindow_ResolvedActiveExecution_IsAdopted(t *testing.T) {
	const tenant, claim = "tnt-apa42-02", "clm-apa42-02"
	const invID = "inv-ffffffffffffffffffffffffffffff42"
	env := launchTestEnv(t, tenant, claim, invID)

	stub := newGCWStub()
	prov, _ := startGCW(t, stub)
	store := newCrashFailFirstStore(true)
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
	ctx := context.Background()

	if _, _, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{}); err == nil {
		t.Fatal("delivery 1: want the record crash surfaced as an error")
	}
	want := apa42ExpectedName(t, prov, invID)

	name, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil {
		t.Fatalf("redelivery after a crash-window ACTIVE execution: %v", err)
	}
	if launched || name != want {
		t.Fatalf("name=%q launched=%v, want adoption of %q (launched=false)", name, launched, want)
	}
	if creates, _ := stub.counts(); creates != 1 {
		t.Fatalf("creates = %d, want 1", creates)
	}
}

// AREA 4 control: a genuinely absent execution takes the normal launch path.
// The classification fix must not turn absence into adoption.
func TestLaunch_CrashWindow_AbsentExecution_StartsNormally(t *testing.T) {
	const tenant, claim = "tnt-apa42-03", "clm-apa42-03"
	const invID = "inv-99999999999999999999999999999942"
	env := launchTestEnv(t, tenant, claim, invID)

	stub := newGCWStub()
	prov, _ := startGCW(t, stub)
	store := newFakeLaunches()
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
	ctx := context.Background()

	name, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{})
	if err != nil {
		t.Fatalf("EnsureLaunched with an absent execution: %v", err)
	}
	if !launched {
		t.Fatal("an absent execution must take the launch path (launched=true)")
	}
	if name != apa42ExpectedName(t, prov, invID) {
		t.Fatalf("started name = %q, want the deterministic resource name", name)
	}
	if creates, probes := stub.counts(); creates != 1 || probes != 1 {
		t.Fatalf("creates=%d probes=%d, want 1/1", creates, probes)
	}
	rec, found, err := store.GetLaunch(ctx, tenant, invID)
	if err != nil || !found || rec.ExecutionName != name {
		t.Fatalf("launch must be recorded: found=%v rec=%+v err=%v", found, rec, err)
	}
}

// AREA 5 at the reconciler seam: a provider outage is NOT absence. The launch
// must fail closed — no start, no row — so redelivery retries safely.
func TestLaunch_CrashWindow_ProviderOutage_FailsClosed(t *testing.T) {
	const tenant, claim = "tnt-apa42-04", "clm-apa42-04"
	const invID = "inv-88888888888888888888888888888842"
	env := launchTestEnv(t, tenant, claim, invID)

	stub := newGCWStub()
	stub.getFails = http.StatusServiceUnavailable
	prov, _ := startGCW(t, stub)
	store := newFakeLaunches()
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
	ctx := context.Background()

	_, _, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{})
	if err == nil {
		t.Fatal("probe outage: want fail-closed error, got success")
	}
	if creates, _ := stub.counts(); creates != 0 {
		t.Fatalf("creates = %d, want 0 — an outage must never read as absence", creates)
	}
	if _, found, _ := store.GetLaunch(ctx, tenant, invID); found {
		t.Fatal("fail-closed must record nothing")
	}
}

// AREA 5b at the reconciler seam: a probe that cannot reach the provider at all
// is also not absence.
func TestLaunch_CrashWindow_ProbeUnreachable_FailsClosed(t *testing.T) {
	const tenant, claim = "tnt-apa42-05", "clm-apa42-05"
	const invID = "inv-77777777777777777777777777777742"
	env := launchTestEnv(t, tenant, claim, invID)

	stub := newGCWStub()
	prov, closeSrv := startGCW(t, stub)
	store := newFakeLaunches()
	l := NewLauncher(NewInMemoryEnvelopeStore(), prov, store)
	ctx := context.Background()

	closeSrv() // the endpoint is gone: transport failure, not absence

	if _, _, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, launchTestWorkflow, ExpireAuth{}); err == nil {
		t.Fatal("unreachable probe: want fail-closed error, got success")
	}
	if _, found, _ := store.GetLaunch(ctx, tenant, invID); found {
		t.Fatal("fail-closed must record nothing")
	}
}
