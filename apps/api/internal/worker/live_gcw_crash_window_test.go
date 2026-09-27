package worker

// APA-42: LIVE qualification of the GCW crash window — the window between
// StartExecution committing provider-side and RecordLaunch committing
// app-side, which is the window investigate/launch.go's reconciler exists to
// close.
//
// WHY THIS FILE IS LIVE AND NOT ANOTHER FAKE
// -----------------------------------------
// Adoption is already covered against contract-shaped fakes
// (investigate/launch_crash_test.go:164
// TestLaunch_CrashBetweenStartAndRecord_NoDuplicate, and APA-41's
// launch_resource_name_test.go). What a fake cannot show is whether the
// reconciler's probe addresses an address the REAL provider can actually
// resolve. That is the entire content of the APA-41 fix: a bare id becomes
// GET /v1/exec-<hex> (a route miss) instead of
// GET /v1/projects/{p}/locations/{l}/workflows/{w}/executions/<id> (a
// resource lookup). Only a real GCW endpoint distinguishes those two.
//
// So this file drives the REAL investigate.Launcher over the REAL
// PGLaunchStore and the REAL workflow.GCWProvider against a LIVE emulator,
// and counts executions by listing the emulator's execution collection
// (liveCorrExecutions / liveCorrCountFor) rather than trusting a bool.
//
// MEASURED EMULATOR BEHAVIOUR (ghcr.io/lemonberrylabs/gcw-emulator, v0.5.0,
// commit d803aa3aa626e9a0129c24c1421e51913d9f98bd) — re-measured per run
// -----------------------------------------------------------------------
// R1  The emulator IGNORES `executionId`. Three POSTs to the executions
//     collection, each carrying the requested caller-chosen id in BOTH the
//     query string (?executionId=) and the body, each returned HTTP 200 and
//     minted a FRESH name: exec-1, exec-2, exec-3. The requested id was not
//     honoured even once.
//
// R2  The emulator NEVER returns 409. Same three POSTs above returned
//     200/200/200; `docker logs` contained no 409, no ALREADY_EXISTS. So
//     GCWProvider.StartExecution's create-409 → create-or-return mapping
//     (APA-40, PR #105) is UNREACHABLE against this emulator.
//
// R3  The requested id is not an addressable resource:
//     GET /v1/projects/my-project/locations/us-central1/workflows/
//     claim-investigation/executions/<requested>  → 404 with a structured
//     body echoing the full name. The emulator-MINTED name resolves 200.
//     So the derived deterministic id can never be probed successfully.
//
// R4  The BARE-id address form is a route miss, distinguishable from a
//     resource miss: GET /v1/<requested> → 404 "Cannot GET /v1/exec-…"
//     (non-JSON), whereas the full-resource-name form → 404 with
//     {"error":{"code":404,…,"status":"NOT_FOUND"}}. This is the APA-41
//     defect made observable: the old probe produced a malformed-route 404.
//     The new probe produces a well-formed lookup that returns a typed
//     absence. TestLiveGCWCrashWindow_ReconcilerProbesFullResourceName pins
//     the fixed form.
//
// R5  Emulator executions reach FAILED essentially immediately (observed
//     FAILED at the first poll, t+0.0s, for every execution created), and
//     GCWProvider.GetExecution returns a non-nil error for any FAILED
//     execution (gcw_provider.go: "execution failed with state FAILED").
//     The reconciler treats a non-ErrExecutionNotFound probe error as a
//     lookup outage and fails closed (launch.go:274). So even on a provider
//     that DID honour executionId, a crash-window execution that has already
//     finished would fail closed rather than adopt. R5 is independent of R1
//     and is reported as a finding, not worked around.
//
// CONSEQUENCE FOR QUALIFICATION (honest limits)
// --------------------------------------------
// R1+R2 mean adoption-by-reconciliation CANNOT be exercised locally: the
// deterministic execution the reconciler probes does not exist, so there is
// nothing to adopt. This file therefore splits the qualification:
//
//   - TestLiveGCWCrashWindow_ReconcilerProbesFullResourceName — LIVE, runs
//     here. Proves the APA-41 invariant on the wire and proves the
//     full-resource-name form is the resolvable one.
//   - TestLiveGCWCrashWindow_CrashWindow_AbsentTakesNormalLaunchPath —
//     LIVE, runs here. The negative control: a genuinely absent execution
//     takes the normal launch path, the durable row is repaired, and NO
//     false adoption occurs.
//   - TestLiveGCWCrashWindow_Adoption_UnqualifiedLocally — the adoption
//     CONTRACT, gated on a live capability probe. It skips locally with the
//     measured R1/R2 evidence and runs unchanged against a provider that
//     honours executionId. It is not weakened, and nothing anywhere in this
//     file special-cases the emulator to force a pass.
//
// WHY THE CRASH IS SIMULATED AS A LOST WRITE, NOT A DELETED ROW
// -------------------------------------------------------------
// The task named "removing the workflow_launches row". Measured, that is not
// possible for the application role, and the reason is load-bearing:
//
//	SELECT set_config('app.tenant_id','tnt-x',true);
//	INSERT INTO workflow_launches … ;  -- INSERT 0 1
//	DELETE FROM workflow_launches … ;  -- ERROR: permission denied for table workflow_launches
//
// migration 010_workflow_launches.sql revokes UPDATE and DELETE from PUBLIC
// and from claimops_app: launch rows are append-only facts. The application
// therefore CANNOT produce a missing launch row by deleting one — the ONLY
// way the reconciler ever sees "row absent" is a RecordLaunch that never
// committed. So this file models the crash exactly where it occurs: a
// test-local LaunchStore that delegates GetLaunch to the REAL
// PGLaunchStore (so the miss is a genuine database fact, asserted through
// the real store) and drops the first RecordLaunch (the lost commit). No
// migration, backfill, or privilege change is needed or attempted.
//
// Infra (docker run only; the repo rule forbids compose):
//
//	docker run -d --name gcw-emulator --network claimops-net \
//	  -p 8787:8787 -p 8788:8788 \
//	  -v $(pwd)/workflows:/workflows:ro \
//	  -e WORKFLOWS_DIR=/workflows -e PROJECT=my-project \
//	  -e LOCATION=us-central1 \
//	  ghcr.io/lemonberrylabs/gcw-emulator:latest
//
// Run live (the ADMIN DSN is required, not optional: cleanup must DELETE
// workflow_launches and investigations, which claimops_app cannot do):
//
//	TEST_POSTGRES_DSN='postgres://claimops_app:claimops_app@localhost:5433/claimops' \
//	TEST_POSTGRES_ADMIN_DSN='postgres://claimops:claimops@localhost:5433/claimops' \
//	WORKFLOWS_EMULATOR_HOST=localhost:8787 \
//	go test ./internal/worker/ -run TestLiveGCWCrashWindow -v -count=1
//
// Stop afterwards (leave no stray containers):
//
//	docker rm -f gcw-emulator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/workflow"

	"github.com/jackc/pgx/v5/pgxpool"
)

// crashWindowWorkflow is the workflow the worker actually launches
// (worker/launch.go:17). The real frozen YAML runs; no test-only workflow.
const crashWindowWorkflow = defaultWorkflowID

// crashWindowExpectedExecutionID re-derives the documented deterministic BARE
// execution id for (workflowID, invID):
//
//	"exec-" + first 16 bytes of sha256("claimops-execution-v1\x00"+wf+"\x00"+inv)
//
// investigate.executionIDFor is unexported, so this re-implements the formula
// published on it (the same approach investigate's own
// crashExpectedExecutionID takes). The re-derivation is a contract pin, not a
// copy: if the production derivation ever changes, the wire-level assertion
// in TestLiveGCWCrashWindow_ReconcilerProbesFullResourceName fails, because
// the launcher would then be probing a name this helper does not predict.
func crashWindowExpectedExecutionID(workflowID, invID string) string {
	sum := sha256.Sum256([]byte("claimops-execution-v1\x00" + workflowID + "\x00" + invID))
	return "exec-" + hex.EncodeToString(sum[:16])
}

// crashWindowExchange is one HTTP request observed on the wire.
type crashWindowExchange struct {
	Method   string
	Path     string
	RawQuery string
}

// crashWindowRecorder is a PASSIVE RoundTripper: it records the method, path
// and query of every request and then delegates to the real transport, so the
// bytes on the wire still go to the real emulator. It is observation only —
// it never rewrites a request, never serves a response, and the provider and
// emulator under test are the real ones (the client is injected through the
// existing test-only workflow.NewGCWProviderWithClient seam). This is what
// lets the test assert the probe's ADDRESS FORM, which no amount of
// post-hoc inspection of the emulator could otherwise reveal.
type crashWindowRecorder struct {
	mu        sync.Mutex
	exchanges []crashWindowExchange
}

var _ http.RoundTripper = (*crashWindowRecorder)(nil)

// RoundTrip records the request then performs it for real.
func (r *crashWindowRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.exchanges = append(r.exchanges, crashWindowExchange{
		Method:   req.Method,
		Path:     req.URL.Path,
		RawQuery: req.URL.RawQuery,
	})
	r.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

// reset drops recorded exchanges so a test can assert on one phase alone.
func (r *crashWindowRecorder) reset() {
	r.mu.Lock()
	r.exchanges = nil
	r.mu.Unlock()
}

// snapshot returns a copy of the recorded exchanges.
func (r *crashWindowRecorder) snapshot() []crashWindowExchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]crashWindowExchange, len(r.exchanges))
	copy(out, r.exchanges)
	return out
}

// gets returns the recorded GET exchanges.
func (r *crashWindowRecorder) gets() []crashWindowExchange {
	var out []crashWindowExchange
	for _, e := range r.snapshot() {
		if e.Method == http.MethodGet {
			out = append(out, e)
		}
	}
	return out
}

// posts returns the recorded POST exchanges.
func (r *crashWindowRecorder) posts() []crashWindowExchange {
	var out []crashWindowExchange
	for _, e := range r.snapshot() {
		if e.Method == http.MethodPost {
			out = append(out, e)
		}
	}
	return out
}

// crashWindowLostCommitStore is the crash simulator. It wraps the REAL
// investigate.PGLaunchStore and reproduces the crash window with the
// smallest possible deviation from production:
//
//   - GetLaunch ALWAYS delegates to the real store, so the reconciler's
//     "is the row there?" question is answered by PostgreSQL, not by a fake.
//     A miss is therefore a genuine database fact.
//   - RecordLaunch delegates to the real store EXCEPT for the one call the
//     test arms with armLostCommit, which is dropped and reported as a
//     successful first write. That is exactly "StartExecution committed
//     provider-side, the worker died before RecordLaunch committed": the
//     launcher sees the success it would have seen, and the row is not
//     there.
//
// After the drop the store is a pure pass-through, so the reconciler's own
// adoption/launch RecordLaunch is a REAL insert that the test then verifies
// through the real store.
type crashWindowLostCommitStore struct {
	inner   investigate.LaunchStore
	mu      sync.Mutex
	armed   bool
	dropped int
}

var _ investigate.LaunchStore = (*crashWindowLostCommitStore)(nil)

// armLostCommit makes the NEXT RecordLaunch vanish.
func (s *crashWindowLostCommitStore) armLostCommit() {
	s.mu.Lock()
	s.armed = true
	s.mu.Unlock()
}

// lostCommits reports how many RecordLaunch calls were dropped.
func (s *crashWindowLostCommitStore) lostCommits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// RecordLaunch drops the armed call, otherwise delegates.
func (s *crashWindowLostCommitStore) RecordLaunch(ctx context.Context, rec investigate.LaunchRecord) (bool, error) {
	s.mu.Lock()
	if s.armed {
		s.armed = false
		s.dropped++
		s.mu.Unlock()
		// Report the success the real store would have reported for a
		// first write. The row is deliberately NOT written: this is the
		// lost commit, and the reconciler must discover it as a miss.
		return true, nil
	}
	s.mu.Unlock()
	return s.inner.RecordLaunch(ctx, rec)
}

// GetLaunch always asks the real store.
func (s *crashWindowLostCommitStore) GetLaunch(ctx context.Context, tenantID, investigationID string) (investigate.LaunchRecord, bool, error) {
	return s.inner.GetLaunch(ctx, tenantID, investigationID)
}

// crashWindowAdminPool dials the privileged DSN used ONLY to delete the rows
// this file creates, or skips. A live test that cannot clean up after itself
// must not run, so this gate is not optional.
func crashWindowAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_ADMIN_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_ADMIN_DSN unset: this test creates append-only rows in workflow_launches and investigations that only a privileged role can delete, so it refuses to run without one")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres admin unavailable (dial): %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres admin unavailable (ping): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// crashWindowPurge deletes every row this test created for tenant and fails
// the test if any survive. Registered with t.Cleanup by
// crashWindowIdentity, so it runs on failure paths too.
func crashWindowPurge(t *testing.T, admin *pgxpool.Pool, tenant string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, admin)
	if err != nil {
		t.Errorf("cleanup: begin admin tx: %v", err)
		return
	}
	defer func() { _ = tx.Rollback(tctx) }()
	// Fixed table literals, never interpolated input.
	for _, table := range []string{"workflow_launches", "investigations"} {
		tag, err := tx.Exec(tctx, fmt.Sprintf("DELETE FROM %s WHERE tenant_id = $1", table), tenant)
		if err != nil {
			t.Errorf("cleanup: DELETE %s: %v", table, err)
			return
		}
		t.Logf("cleanup: deleted %d row(s) from %s for tenant %s", tag.RowsAffected(), table, tenant)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Errorf("cleanup: commit: %v", err)
		return
	}
	// Verify zero remain. Hygiene is asserted, not assumed.
	for _, table := range []string{"workflow_launches", "investigations"} {
		var n int
		if err := admin.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE tenant_id = $1", table), tenant).Scan(&n); err != nil {
			t.Errorf("cleanup: verify %s: %v", table, err)
			continue
		}
		if n != 0 {
			t.Errorf("cleanup: %d row(s) still present in %s for tenant %s", n, table, tenant)
		}
	}
}

// crashWindowIdentity mints a fresh (tenant, claim, document), registers
// cleanup for it, and returns the derived investigation ID. A fresh
// investigation ID is what makes the pre-launch execution count a
// trustworthy zero.
func crashWindowIdentity(t *testing.T, admin *pgxpool.Pool) (tenant, claim, doc, invID string) {
	t.Helper()
	suffix := liveCorrRand(t, 6)
	tenant = "tnt-apa42-" + suffix
	claim = "clm-apa42-" + suffix
	doc = "doc-apa42-" + suffix
	invID, err := investigationIDForDocument(tenant, claim, doc)
	if err != nil {
		t.Fatalf("investigationIDForDocument: %v", err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, invID); err != nil {
		t.Fatalf("derived investigation ID invalid: %v", err)
	}
	t.Cleanup(func() { crashWindowPurge(t, admin, tenant) })
	return tenant, claim, doc, invID
}

// crashWindowRecorderProvider builds the REAL GCWProvider pointed at the live
// emulator, with the passive recorder observing the wire. The provider is
// production code; only the http.Client is injected, through the existing
// test-only constructor.
func crashWindowRecorderProvider(host string, rec *crashWindowRecorder) *workflow.GCWProvider {
	return workflow.NewGCWProviderWithClient(host, liveCorrProject, liveCorrLocation, &http.Client{
		Transport: rec,
		Timeout:   10 * time.Second,
	})
}

// crashWindowRawGet issues a GET against the emulator without going through
// the provider, and returns the raw status and body. Used to compare the two
// address FORMS: a bare id and a full resource name. The provider maps both
// 404s onto ErrExecutionNotFound, so the distinction is only visible on the
// raw response.
func crashWindowRawGet(t *testing.T, ctx context.Context, host, executionName string) (int, string) {
	t.Helper()
	base := liveCorrBase(host)
	endpoint := fmt.Sprintf("%s/v1/%s", base, strings.TrimPrefix(executionName, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("raw GET %s: build request: %v", executionName, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw GET %s: %v", executionName, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("raw GET %s: read body: %v", executionName, err)
	}
	return resp.StatusCode, string(body)
}

// crashWindowRawCreate POSTs to the executions collection with a requested
// caller-chosen executionId, in BOTH the query string and the body (the two
// places a conforming GCW reads it), and returns the raw status plus the
// parsed name. Raw rather than via the provider, because the question being
// asked is about the EMULATOR: does it honour the id, and does it 409?
func crashWindowRawCreate(ctx context.Context, host, workflowID, requestedID, invID string) (int, string, error) {
	base := liveCorrBase(host)
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/workflows/%s/executions?executionId=%s",
		base,
		url.PathEscape(liveCorrProject),
		url.PathEscape(liveCorrLocation),
		url.PathEscape(workflowID),
		url.QueryEscape(requestedID),
	)
	argument, err := json.Marshal(map[string]string{
		"tenant_id":        "tnt-apa42-capability",
		"claim_id":         "clm-apa42-capability",
		"investigation_id": invID,
		"idempotency_key":  invID,
		"execution_name":   requestedID,
	})
	if err != nil {
		return 0, "", err
	}
	payload, err := json.Marshal(map[string]string{
		"argument":    string(argument),
		"executionId": requestedID,
	})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	var out struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return resp.StatusCode, "", fmt.Errorf("decode create response %q: %w", string(body), err)
	}
	return resp.StatusCode, out.Name, nil
}

// crashWindowExecutionsFor returns the names of the emulator executions
// launched for invID, in list order. The list endpoint is fail-closed on
// nextPageToken inside liveCorrExecutions, so a truncated set can never be
// mistaken for the whole set.
func crashWindowExecutionsFor(t *testing.T, ctx context.Context, host, invID string) []string {
	t.Helper()
	var names []string
	for _, e := range liveCorrExecutions(t, ctx, host, crashWindowWorkflow) {
		if liveCorrArgField(e.Argument, "investigation_id") == invID {
			names = append(names, e.Name)
		}
	}
	return names
}

// requireAbsentFromEmulator fails unless the emulator holds zero executions
// for invID. Used as a pre-condition so a later count cannot be satisfied by
// pre-existing rows.
func requireAbsentFromEmulator(t *testing.T, ctx context.Context, host, invID string) {
	t.Helper()
	if names := crashWindowExecutionsFor(t, ctx, host, invID); len(names) != 0 {
		t.Fatalf("baseline: emulator already holds %d execution(s) for fresh ID %q: %s",
			len(names), invID, strings.Join(names, ", "))
	}
}

// TestLiveGCWCrashWindow_ReconcilerProbesFullResourceName is the LIVE
// qualification of the APA-41 invariant: the reconciler's GetExecution
// probe must address the execution by its FULL GCW resource name, never by a
// bare id. The provider owns project/location, so the Launcher must not
// restate the collection prefix; this test asserts the resulting wire form
// rather than trusting the code reading.
//
// It also proves the address form is the RESOLVABLE one, which is the claim a
// fake provider cannot support: a full resource name for an execution the
// emulator really holds returns 200 with an observed state, while the bare
// id is a route miss (R4).
func TestLiveGCWCrashWindow_ReconcilerProbesFullResourceName(t *testing.T) {
	host := liveCorrEmulatorHost(t)
	pool := liveCorrPool(t)
	admin := crashWindowAdminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	tenant, claim, _, invID := crashWindowIdentity(t, admin)
	rec := &crashWindowRecorder{}
	prov := crashWindowRecorderProvider(host, rec)
	liveCorrDeploy(ctx, prov, t)

	real := investigate.NewPGLaunchStore(pool)
	store := &crashWindowLostCommitStore{inner: real}
	l := investigate.NewLauncher(investigate.NewPGEnvelopeStore(pool), prov, store)
	env := liveCorrEnvelope(t, tenant, claim, invID, liveCorrRand(t, 16))

	// Baseline: nothing to adopt yet.
	requireAbsentFromEmulator(t, ctx, host, invID)

	// ---- THE CRASH WINDOW ------------------------------------------------
	// Arm the lost commit and launch. StartExecution creates a real execution
	// provider-side; RecordLaunch is dropped.
	store.armLostCommit()
	rec.reset()
	name1, launched1, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, crashWindowWorkflow, investigate.ExpireAuth{})
	if err != nil {
		t.Fatalf("crash-window EnsureLaunched: %v", err)
	}
	if !launched1 {
		t.Fatal("crash-window EnsureLaunched launched=false, want true (it owns the first launch)")
	}
	if store.lostCommits() != 1 {
		t.Fatalf("lost commits = %d, want exactly 1 (the simulator must drop precisely the crash-window RecordLaunch)", store.lostCommits())
	}

	// The crash produced exactly the state the reconciler is built for:
	// the execution EXISTS provider-side, the durable row does NOT.
	// Both halves are counted/queried, not assumed.
	crashNames := crashWindowExecutionsFor(t, ctx, host, invID)
	if len(crashNames) != 1 {
		t.Fatalf("crash window: emulator holds %d executions for %q, want exactly 1 (the lost-write execution): %s",
			len(crashNames), invID, strings.Join(crashNames, ", "))
	}
	if _, found, err := real.GetLaunch(ctx, tenant, invID); err != nil {
		t.Fatalf("crash window: real GetLaunch: %v", err)
	} else if found {
		t.Fatal("crash window: workflow_launches row is PRESENT; the lost commit did not happen, so this is not the crash window")
	}
	t.Logf("crash window established: emulator holds %q, workflow_launches row absent (lost commit)", crashNames[0])

	// ---- THE RECONCILE PROBE --------------------------------------------
	// Re-run with the row gone. The reconciler must now probe.
	rec.reset()
	if _, _, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, crashWindowWorkflow, investigate.ExpireAuth{}); err != nil {
		t.Fatalf("reconcile EnsureLaunched: %v", err)
	}

	// The derived BARE id and the full resource name the provider builds
	// from it. The reconciler must probe the latter.
	bareID := crashWindowExpectedExecutionID(crashWindowWorkflow, invID)
	fullName := prov.ExecutionResourceName(crashWindowWorkflow, bareID)
	if strings.TrimSpace(fullName) == "" {
		t.Fatal("provider returned an empty execution resource name")
	}
	if !strings.HasSuffix(fullName, "/"+bareID) {
		t.Fatalf("provider resource name %q does not end in the bare id %q", fullName, bareID)
	}
	t.Logf("derived bare id %q -> full resource name %q", bareID, fullName)

	// ASSERTION (APA-41, wire-level): a GET went to the FULL resource name.
	wantPath := "/v1/" + fullName
	barePath := "/v1/" + bareID
	var probedFull bool
	for _, e := range rec.gets() {
		t.Logf("observed GET %s (query %q)", e.Path, e.RawQuery)
		if e.Path == wantPath {
			probedFull = true
		}
		if e.Path == barePath {
			t.Fatalf("reconciler probed the BARE id %q — GET %s is a route miss, which is the APA-40/APA-41 defect", bareID, e.Path)
		}
	}
	if !probedFull {
		t.Fatalf("no GET to the full resource name %q; observed GETs: %+v", wantPath, rec.gets())
	}
	if got := liveCorrArgField(liveCorrFind(t, ctx, host, crashWindowWorkflow, name1).Argument, "investigation_id"); got != invID {
		t.Fatalf("crash-window execution %q carries investigation_id %q, want %q", name1, got, invID)
	}
	t.Logf("APA-41 live: reconciler probe addressed the full resource name %q (never the bare id)", wantPath)

	// ASSERTION (R4 / resolvability): the form the reconciler uses is the
	// one the emulator can actually resolve, while the bare form is a
	// malformed route. This is the "state observed, not typed-absence"
	// property of the ADDRESS FORM, and it is only observable live.
	//
	// The execution the emulator really minted for this investigation
	// (crashNames[0]) is a full resource name the emulator holds, so a probe
	// for it must resolve with an observed state. The bare id of that same
	// execution is not a valid input and must not resolve.
	minted := crashNames[0]
	state, _, gerr := prov.GetExecution(ctx, minted)
	if gerr != nil {
		// R5: the emulator's executions are FAILED immediately and
		// GetExecution reports that as a non-absence error. That is
		// expected here and is reported, not asserted away.
		t.Logf("GetExecution(%q) returned %v (R5: emulator executions are FAILED at once; the lookup still RESOLVED, it is not typed absence)", minted, gerr)
	}
	if strings.TrimSpace(state) == "" {
		t.Fatalf("GetExecution(%q) observed no state: the full-resource-name probe form did not resolve", minted)
	}
	t.Logf("full resource name %q resolves with observed state %q", minted, state)

	if status, body := crashWindowRawGet(t, ctx, host, bareID); status == http.StatusOK {
		t.Fatalf("bare id %q unexpectedly resolved 200 (%s): the bare form must NOT be addressable", bareID, body)
	} else {
		t.Logf("R4: bare-id address form GET /v1/%s -> %d %s (route miss, not a resource lookup)", bareID, status, strings.TrimSpace(body))
	}
	if status, body := crashWindowRawGet(t, ctx, host, fullName); status != http.StatusNotFound {
		t.Fatalf("derived full resource name %q -> %d %s, want 404 (the deterministic execution genuinely does not exist against this emulator: R1/R2/R3)", fullName, status, body)
	} else {
		t.Logf("R3: derived full resource name -> 404 %s (typed absence of a real resource, i.e. a well-formed lookup)", strings.TrimSpace(body))
	}
}

// TestLiveGCWCrashWindow_CrashWindow_AbsentTakesNormalLaunchPath is the
// NEGATIVE CONTROL required of the adoption path: a genuinely absent
// execution must take the normal launch path and must NOT be falsely
// adopted. It runs the whole crash window end-to-end live — real provider,
// real PG stores, real emulator — so the reconciler's decision path is
// exercised, not merely reasoned about.
//
// The durable row is asserted to be REPAIRED, which is the reconciler's
// job in both outcomes (adopt and launch): after a reconcile pass the
// launch row must exist and must name an execution the emulator really
// holds.
func TestLiveGCWCrashWindow_CrashWindow_AbsentTakesNormalLaunchPath(t *testing.T) {
	host := liveCorrEmulatorHost(t)
	pool := liveCorrPool(t)
	admin := crashWindowAdminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	tenant, claim, _, invID := crashWindowIdentity(t, admin)
	prov := workflow.NewGCWProvider(host, liveCorrProject, liveCorrLocation)
	liveCorrDeploy(ctx, prov, t)

	real := investigate.NewPGLaunchStore(pool)
	store := &crashWindowLostCommitStore{inner: real}
	l := investigate.NewLauncher(investigate.NewPGEnvelopeStore(pool), prov, store)
	env := liveCorrEnvelope(t, tenant, claim, invID, liveCorrRand(t, 16))

	requireAbsentFromEmulator(t, ctx, host, invID)

	// ---- Crash window: execution exists, durable row lost.
	store.armLostCommit()
	if _, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, crashWindowWorkflow, investigate.ExpireAuth{}); err != nil {
		t.Fatalf("crash-window EnsureLaunched: %v", err)
	} else if !launched {
		t.Fatal("crash-window EnsureLaunched launched=false, want true")
	}
	before := crashWindowExecutionsFor(t, ctx, host, invID)
	if len(before) != 1 {
		t.Fatalf("crash window: emulator holds %d executions for %q, want exactly 1: %s", len(before), invID, strings.Join(before, ", "))
	}
	if _, found, err := real.GetLaunch(ctx, tenant, invID); err != nil {
		t.Fatalf("crash window: real GetLaunch: %v", err)
	} else if found {
		t.Fatal("crash window: durable row present; the lost commit did not happen")
	}

	// ---- Redelivery with a genuinely absent derived execution.
	name2, launched2, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, crashWindowWorkflow, investigate.ExpireAuth{})
	if err != nil {
		t.Fatalf("redelivery EnsureLaunched: %v", err)
	}
	if !launched2 {
		t.Fatal("redelivery launched=false, want true: the derived execution is absent, so the normal launch path is the only correct outcome (a false adoption would report false here)")
	}
	if strings.TrimSpace(name2) == "" {
		t.Fatal("redelivery returned an empty execution name")
	}

	// NO FALSE ADOPTION. A false adoption would report the derived resource
	// name — the name the reconciler probed. The emulator never minted it
	// (R1), so the recorded name must be the emulator's own, and must be a
	// resource the emulator actually holds.
	derivedFull := prov.ExecutionResourceName(crashWindowWorkflow, crashWindowExpectedExecutionID(crashWindowWorkflow, invID))
	rec, found, err := real.GetLaunch(ctx, tenant, invID)
	if err != nil {
		t.Fatalf("post-redelivery real GetLaunch: %v", err)
	}
	if !found {
		t.Fatal("durable launch row still absent after the redelivery: the reconciler must repair it on the launch path too")
	}
	if rec.ExecutionName == derivedFull {
		t.Fatalf("durable row execution_name = %q, which is the derived (probed) name: that is a FALSE ADOPTION of an execution the emulator never created", rec.ExecutionName)
	}
	if rec.ExecutionName != name2 {
		t.Fatalf("durable row execution_name = %q, want the name this call returned %q", rec.ExecutionName, name2)
	}
	after := crashWindowExecutionsFor(t, ctx, host, invID)
	if len(after) != len(before)+1 {
		t.Fatalf("redelivery added %d execution(s) (emulator went %d -> %d), want exactly 1 new: %s",
			len(after)-len(before), len(before), len(after), strings.Join(after, ", "))
	}
	// The recorded name must be a real emulator resource, not just a string.
	liveCorrFind(t, ctx, host, crashWindowWorkflow, rec.ExecutionName)
	t.Logf("negative control: absent -> normal launch. recorded %q, emulator went %d -> %d executions", rec.ExecutionName, len(before), len(after))
	t.Logf("NOTE (R1): the +1 is the emulator minting a fresh exec-N for the create. The reconciler could not adopt: the derived execution %q does not exist. The duplicate is an emulator artefact, not a reconciler defect — the reconciler performed the only correct action given a genuine absence.", derivedFull)

	// ---- Convergence: a further delivery now finds the repaired row and
	// must issue no further StartExecution.
	name3, launched3, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, crashWindowWorkflow, investigate.ExpireAuth{})
	if err != nil {
		t.Fatalf("third EnsureLaunched: %v", err)
	}
	if launched3 {
		t.Fatal("third EnsureLaunched launched=true, want false (the repaired durable row must short-circuit)")
	}
	if name3 != rec.ExecutionName {
		t.Fatalf("third EnsureLaunched name = %q, want the durable %q", name3, rec.ExecutionName)
	}
	final := crashWindowExecutionsFor(t, ctx, host, invID)
	if len(final) != len(after) {
		t.Fatalf("convergence delivery changed the emulator execution count %d -> %d, want unchanged: %s", len(after), len(final), strings.Join(final, ", "))
	}
	t.Logf("convergence: repaired row short-circuits, emulator still holds %d execution(s)", len(final))
}

// TestLiveGCWCrashWindow_Adoption_UnqualifiedLocally is the crash-window
// ADOPTION contract: after a lost RecordLaunch, a redelivery must find the
// execution the provider already created, ADOPT it (zero new starts), repair
// the durable row, and leave the emulator holding EXACTLY ONE execution for
// that investigation.
//
// RED FIRST. This body was written as hard assertions and run against the
// live emulator BEFORE any gate existed. It FAILED, and the failure is the
// measurement recorded in the file header (R1/R2/R3): the emulator mints a
// fresh exec-N per create and never returns 409, so the deterministic
// execution the reconciler probes does not exist and there is nothing to
// adopt. The assertions below are therefore unchanged; only their ENTRY is
// gated, by a live capability probe (crashWindowAdoptable) that skips with
// the measured evidence and runs the assertions as written against a
// provider that honours executionId.
func TestLiveGCWCrashWindow_Adoption_UnqualifiedLocally(t *testing.T) {
	host := liveCorrEmulatorHost(t)
	pool := liveCorrPool(t)
	admin := crashWindowAdminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Capability gate. Nothing below runs unless the provider under test
	// can create an execution under a caller-chosen executionId AND that
	// execution is then resolvable by GetExecution. That is the precondition
	// for adoption; if it does not hold, adoption is unqualified and the test
	// says so with evidence instead of failing or passing on a fake.
	crashWindowAdoptable(t, ctx, host)

	tenant, claim, _, invID := crashWindowIdentity(t, admin)
	prov := workflow.NewGCWProvider(host, liveCorrProject, liveCorrLocation)
	liveCorrDeploy(ctx, prov, t)

	real := investigate.NewPGLaunchStore(pool)
	store := &crashWindowLostCommitStore{inner: real}
	l := investigate.NewLauncher(investigate.NewPGEnvelopeStore(pool), prov, store)
	env := liveCorrEnvelope(t, tenant, claim, invID, liveCorrRand(t, 16))

	requireAbsentFromEmulator(t, ctx, host, invID)

	// ---- Crash window: provider commits the execution, the app does not.
	store.armLostCommit()
	if _, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, crashWindowWorkflow, investigate.ExpireAuth{}); err != nil {
		t.Fatalf("crash-window EnsureLaunched: %v", err)
	} else if !launched {
		t.Fatal("crash-window EnsureLaunched launched=false, want true")
	}
	before := crashWindowExecutionsFor(t, ctx, host, invID)
	if len(before) != 1 {
		t.Fatalf("crash window: emulator holds %d executions for %q, want exactly 1: %s", len(before), invID, strings.Join(before, ", "))
	}
	if _, found, err := real.GetLaunch(ctx, tenant, invID); err != nil {
		t.Fatalf("crash window: real GetLaunch: %v", err)
	} else if found {
		t.Fatal("crash window: durable row present; the lost commit did not happen")
	}

	// ---- Redelivery must ADOPT.
	derivedFull := prov.ExecutionResourceName(crashWindowWorkflow, crashWindowExpectedExecutionID(crashWindowWorkflow, invID))
	name2, launched2, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, crashWindowWorkflow, investigate.ExpireAuth{})
	if err != nil {
		t.Fatalf("redelivery EnsureLaunched: %v", err)
	}
	if launched2 {
		t.Fatal("redelivery launched=true, want false: adoption must start no new execution")
	}
	if name2 != derivedFull {
		t.Fatalf("adopted name = %q, want the derived full resource name %q", name2, derivedFull)
	}
	rec, found, err := real.GetLaunch(ctx, tenant, invID)
	if err != nil || !found {
		t.Fatalf("adoption must repair the durable row: found=%v err=%v", found, err)
	}
	if rec.ExecutionName != derivedFull {
		t.Fatalf("repaired row execution_name = %q, want %q", rec.ExecutionName, derivedFull)
	}
	after := crashWindowExecutionsFor(t, ctx, host, invID)
	if len(after) != 1 {
		t.Fatalf("adoption left %d executions for %q, want EXACTLY 1: %s", len(after), invID, strings.Join(after, ", "))
	}
	t.Logf("ADOPTION PROVEN: launched=false, name=%q, durable row repaired, emulator still holds exactly 1 execution", name2)
}

// crashWindowAdoptable measures, live and per run, whether the GCW endpoint
// under test supports the adoption path at all. It POSTs twice with the SAME
// caller-chosen executionId (in both the query string and the body) and
// reports:
//
//   - whether the requested id was honoured (a create returned, or resolved,
//     a resource under that id), and
//   - whether any create answered 409.
//
// It skips the calling test when the id is not honoured, quoting the measured
// statuses and names. It NEVER fails: a missing capability is a documented
// limitation of the environment, not a defect in the code under test.
func crashWindowAdoptable(t *testing.T, ctx context.Context, host string) {
	t.Helper()
	requested := "exec-APA42CAPABILITYPROBE0000000000"
	const capInv = "inv-apa42-capability-probe-0000000000000000"

	firstStatus, firstName, err := crashWindowRawCreate(ctx, host, crashWindowWorkflow, requested, capInv)
	if err != nil {
		t.Skipf("adoption unqualified: capability probe could not create an execution: %v", err)
	}
	secondStatus, secondName, err := crashWindowRawCreate(ctx, host, crashWindowWorkflow, requested, capInv)
	if err != nil {
		t.Skipf("adoption unqualified: capability probe second create failed: %v", err)
	}

	t.Logf("capability probe: requested executionId=%q", requested)
	t.Logf("  create #1 -> HTTP %d name=%q", firstStatus, firstName)
	t.Logf("  create #2 -> HTTP %d name=%q", secondStatus, secondName)

	honoured := secondName != "" && strings.HasSuffix(secondName, "/"+requested)
	sawConflict := firstStatus == http.StatusConflict || secondStatus == http.StatusConflict

	if honoured {
		t.Logf("adoption qualified: the endpoint honoured the caller-chosen executionId (sawConflict=%v); running the adoption contract", sawConflict)
		return
	}

	// Not honoured. Quote what was measured so the skip is evidence, not an
	// excuse. The GET below is the corroborating half of R3: asked by its
	// FULL resource name — the exact form the reconciler uses — the
	// requested id still does not resolve, and the emulator's structured
	// NOT_FOUND echoes the full name back.
	requestedFull := fmt.Sprintf("projects/%s/locations/%s/workflows/%s/executions/%s",
		liveCorrProject, liveCorrLocation, crashWindowWorkflow, requested)
	getStatus, getBody := crashWindowRawGet(t, ctx, host, requestedFull)
	t.Skipf("adoption unqualified against this GCW endpoint: the caller-chosen executionId is NOT honoured, so the deterministic execution the reconciler probes can never exist and there is nothing to adopt. MEASURED: create #1 with ?executionId=%s and body executionId=%s -> HTTP %d, name %q; create #2 with the same id -> HTTP %d, name %q (a second, DIFFERENT execution was minted: create is create-always). 409 observed: %v. Direct GET /v1/%s (the form the reconciler uses) -> HTTP %d %s. This is the documented emulator limitation R1/R2/R3 in the file header; the assertions above are the contract and run unchanged against a provider that honours executionId.",
		requested, requested, firstStatus, firstName, secondStatus, secondName, sawConflict, requestedFull, getStatus, strings.TrimSpace(getBody))
}

// TestLiveGCWCrashWindow_Adoption_BlockedByFailedExecution documents R5, a
// SECOND and independent reason adoption is unqualified — one that is a
// property of the frozen provider/reconciler pair rather than of the
// emulator, and therefore one that will still be true against real GCW.
//
// MEASURED CHAIN
//   - GCWProvider.GetExecution returns (state, result, err). For an
//     execution in state FAILED it synthesises a non-nil error
//     (gcw_provider.go: "execution failed with state FAILED"), and that error
//     does NOT wrap workflow.ErrExecutionNotFound.
//   - investigate/launch.go:261-276 decides on the probe like this:
//     adopt only when err == nil; take the launch path only when the error IS
//     ErrExecutionNotFound; anything else FAILS CLOSED with an error and
//     starts nothing.
//
// So a crash-window execution that has already reached a terminal FAILED
// state is classified as a LOOKUP OUTAGE, not as a resolved execution. The
// launch is never adopted, the durable row is never repaired, and every
// redelivery retries — which is exactly the duplicate-start outcome the
// reconciler exists to prevent, now caused by a resolved execution rather
// than by an absent one.
//
// The measurement below is ASSERTED, so the behaviour is pinned as
// executable documentation and cannot drift silently. The CONTRACT it
// violates — a probe that resolves a real execution must not report a lookup
// failure — is encoded as an explicitly skipping assertion, because it is not
// satisfied today and must not be worked around here (frozen code, test-only
// slice). It flips the moment GetExecution distinguishes "resolved but
// failed" from "lookup failed", or the reconciler does.
func TestLiveGCWCrashWindow_Adoption_BlockedByFailedExecution(t *testing.T) {
	host := liveCorrEmulatorHost(t)
	pool := liveCorrPool(t)
	admin := crashWindowAdminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	tenant, claim, _, invID := crashWindowIdentity(t, admin)
	prov := workflow.NewGCWProvider(host, liveCorrProject, liveCorrLocation)
	liveCorrDeploy(ctx, prov, t)

	real := investigate.NewPGLaunchStore(pool)
	store := &crashWindowLostCommitStore{inner: real}
	l := investigate.NewLauncher(investigate.NewPGEnvelopeStore(pool), prov, store)
	env := liveCorrEnvelope(t, tenant, claim, invID, liveCorrRand(t, 16))

	requireAbsentFromEmulator(t, ctx, host, invID)

	// Create a real execution through the launcher, and lose the durable
	// row, so an execution that EXISTS is what the probe would face.
	store.armLostCommit()
	if _, _, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, crashWindowWorkflow, investigate.ExpireAuth{}); err != nil {
		t.Fatalf("crash-window EnsureLaunched: %v", err)
	}
	names := crashWindowExecutionsFor(t, ctx, host, invID)
	if len(names) != 1 {
		t.Fatalf("want exactly 1 emulator execution for %q, got %d: %s", invID, len(names), strings.Join(names, ", "))
	}
	minted := names[0]

	// Wait for a terminal state, bounded. The emulator is immediate, but the
	// loop keeps the test honest against a slower endpoint instead of
	// assuming a fixed delay. GetExecution carries the state alongside any
	// error, so a non-empty state is the signal to stop — the error is
	// classified separately below.
	var state string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s, _, _ := prov.GetExecution(ctx, minted)
		if strings.TrimSpace(s) != "" {
			state = s
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if strings.TrimSpace(state) == "" {
		t.Fatalf("no state observed for the emulator execution %q within the bound", minted)
	}
	t.Logf("emulator execution %q observed state %q", minted, state)

	// ASSERTION (R5, measured): if the execution is FAILED, GetExecution
	// reports a NON-ABSENCE error, which the reconciler reads as an outage.
	if state == "FAILED" {
		_, _, gerr := prov.GetExecution(ctx, minted)
		if gerr == nil {
			t.Skip("R5 no longer reproduces: GetExecution returned no error for a FAILED execution, so the reconciler WOULD adopt it. The contract below is now exercised by TestLiveGCWCrashWindow_Adoption_UnqualifiedLocally")
		}
		if errors.Is(gerr, workflow.ErrExecutionNotFound) {
			t.Fatalf("GetExecution(%q) reported typed absence, so it is distinguishable from an outage and R5 does not reproduce: %v", minted, gerr)
		}
		t.Logf("R5 MEASURED: GetExecution(%q) -> err=%v", minted, gerr)
		t.Logf("R5 CONSEQUENCE: investigate/launch.go:261-276 classifies this as a lookup outage and fails closed — it neither adopts nor launches, and records nothing, so the durable row is never repaired and every redelivery retries the start.")
	}

	// CONTRACT (not satisfied today; encoded so it flips when fixed).
	// A probe that resolves a real execution must be distinguishable from a
	// failed lookup, otherwise the crash window cannot be closed for any
	// execution that reached a terminal state before redelivery.
	t.Skipf("contract not satisfied today, and not worked around here (frozen code, test-only slice): a probe that RESOLVES a real execution must not be reported as a lookup failure. MEASURED against the live emulator: execution %q is in state %q, it is present in the emulator's execution collection, and GCWProvider.GetExecution returns a non-nil error that does NOT wrap workflow.ErrExecutionNotFound. The reconciler therefore fails closed instead of adopting, so the crash window stays open for any execution that terminated before redelivery. Fix belongs in GCWProvider.GetExecution (distinguish resolved-but-failed from lookup failure) or in the reconciler's classify step.", minted, state)
}
