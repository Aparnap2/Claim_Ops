package worker

// APA-39: worker→investigate→workflow correlation against the REAL
// GCWProvider, the REAL PG stores, and a LIVE GCW emulator.
//
// This joins the two landed halves and nothing else:
//   - investigate/launch_live_test.go:75 drives Launcher.EnsureLaunched
//     through PGEnvelopeStore + PGLaunchStore but against a fakeProvider.
//   - workflow/gcw_live_test.go:122 drives the real GCWProvider
//     Deploy/Start/Get against a live emulator, but not through the
//     Launcher and not against PG.
//
// Gap closed here: the provider seam itself is real. A bool from
// EnsureLaunched is NOT accepted as proof of "one execution"; every
// execution claim in this file is counted by listing the emulator's
// execution collection (see liveCorrExecutions / liveCorrCountFor).
//
// WHY COUNTING IS NECESSARY (measured, not assumed)
// ----------------------------------------------------
// The emulator is an ADVERSARIAL provider with respect to idempotency:
// three identical POSTs to
//
//	/v1/projects/{p}/locations/{l}/workflows/{id}/executions
//
// produce three distinct executions (exec-2, exec-3, exec-4), each
// carrying the same argument. Verified live before this test was
// written. So "exactly one execution" cannot come from the provider —
// it can only come from the durable workflow_launches row. That is the
// property under qualification.
//
// The emulator also mints its OWN execution names (exec-N) and ignores
// the `execution_name` key carried in the launch argument. That is
// recorded, not papered over: see
// TestLiveGCWCorrelation_CrashWindowDuplicate and the file tail.
//
// The executed workflow is the REAL frozen workflows/claim-investigation.yaml
// (the ID the worker actually launches, worker/launch.go:17). It
// terminates FAILED at the `investigate` step on
// `sys.get_env("AGENT_URL")` — an immediate terminal state with no
// backend attached, proven live:
//
//	{"state":"FAILED","error":{"payload":"{\"message\":\"environment
//	 variable 'AGENT_URL' not found\",\"tags\":[\"KeyError\"]}"}}
//
// NO assertion here depends on continuation past that step, and none
// depends on a terminal state at all: an execution that exists and was
// created by the emulator is sufficient evidence for the launch
// contract. The documented emulator retry-success short-circuit
// (APA-36) and the still-broken continuation (APA-37) are therefore out
// of scope and unobserved by this slice.
//
// Live-gating: skips when TEST_POSTGRES_DSN or WORKFLOWS_EMULATOR_HOST
// is unset, or when either is unreachable — the same live-gated skip
// pattern as audit_test.go:38 and gcw_live_test.go:80.
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
// Run live:
//
//	TEST_POSTGRES_DSN='postgres://claimops:claimops@localhost:5433/claimops' \
//	WORKFLOWS_EMULATOR_HOST=localhost:8787 \
//	go test ./internal/worker/ -run TestLiveGCWCorrelation -v -count=1
//
// Stop afterwards (leave no stray containers):
//
//	docker rm -f gcw-emulator

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/workflow"

	"github.com/jackc/pgx/v5/pgxpool"
)

// liveCorrProject / liveCorrLocation are the emulator coordinates the
// repo's other live GCW tests deploy to. The provider constructor
// defaults match, but they are stated here so the counting URLs below
// and the provider's URLs cannot silently diverge.
const (
	liveCorrProject  = "my-project"
	liveCorrLocation = "us-central1"
	// liveCorrWorkflowID is the ID the worker launches for
	// worker-built exception envelopes (worker/launch.go:17). The test
	// uses the production value, never a test-only workflow, so the
	// real frozen YAML is what runs.
	liveCorrWorkflow = defaultWorkflowID
	// liveCorrPageSize is sent on the execution-list request. The
	// emulator ignores it but a future version must not silently cap
	// the list: liveCorrExecutions fails closed on a non-empty
	// nextPageToken rather than reporting a truncated count as "one".
	liveCorrPageSize = "1000"
)

// liveCorrExecution is one row of the emulator's execution collection.
// Argument is the raw JSON *string* the emulator echoes back, which is
// what carries the launch correlation keys.
type liveCorrExecution struct {
	Name      string          `json:"name"`
	State     string          `json:"state"`
	Argument  string          `json:"argument"`
	StartTime string          `json:"startTime"`
	EndTime   string          `json:"endTime"`
	Error     json.RawMessage `json:"error"`
}

// liveCorrExecutionList is the emulator's list response.
type liveCorrExecutionList struct {
	Executions []liveCorrExecution `json:"executions"`
	// NextPageToken is asserted empty. A non-empty value would mean the
	// count below is a lower bound, which cannot support "exactly one".
	NextPageToken string `json:"nextPageToken"`
}

// liveCorrEmulatorHost returns the emulator host, skipping when unset or
// unreachable (same pattern as workflow/gcw_live_test.go:80).
func liveCorrEmulatorHost(t *testing.T) string {
	t.Helper()
	host := strings.TrimSpace(os.Getenv("WORKFLOWS_EMULATOR_HOST"))
	if host == "" {
		t.Skip("WORKFLOWS_EMULATOR_HOST unset: skipping live GCW correlation test")
	}
	dial := strings.TrimSuffix(host, "/")
	dial = strings.TrimPrefix(strings.TrimPrefix(dial, "http://"), "https://")
	probe, err := net.DialTimeout("tcp", dial, 2*time.Second)
	if err != nil {
		t.Skipf("GCW emulator at %q unreachable: %v", host, err)
	}
	probe.Close()
	return host
}

// liveCorrPool dials live PG or skips (same gate as investigate's
// investigateLiveDSN at audit_test.go:38). No default DSN: these tests
// must only ever touch an explicitly provided database.
func liveCorrPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn, ok := os.LookupEnv("TEST_POSTGRES_DSN")
	if !ok || strings.TrimSpace(dsn) == "" {
		t.Skip("TEST_POSTGRES_DSN unset: skipping live GCW correlation test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable (dial): %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unavailable (ping): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// liveCorrRand returns a hex suffix so each run owns a fresh
// tenant/claim/document and therefore a fresh investigation ID, which is
// what makes the pre-launch execution count a trustworthy zero.
func liveCorrRand(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// liveCorrBase normalizes the emulator host to a scheme-qualified base.
func liveCorrBase(host string) string {
	base := strings.TrimSuffix(strings.TrimSpace(host), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	return base
}

// liveCorrExecutions lists the emulator's executions for workflowID.
//
// This is the counting primitive. It deliberately does not use
// GCWProvider: the provider interface has no List method, and inventing
// one would change production code. The list endpoint is
//
//	GET /v1/projects/{p}/locations/{l}/workflows/{id}/executions
//
// which the emulator answers with every execution it holds (verified
// live: 29 executions listed, no nextPageToken, no cap).
func liveCorrExecutions(t *testing.T, ctx context.Context, host, workflowID string) []liveCorrExecution {
	t.Helper()
	base := liveCorrBase(host)
	endpoint := fmt.Sprintf("%s/v1/projects/%s/locations/%s/workflows/%s/executions?pageSize=%s",
		base,
		url.PathEscape(liveCorrProject),
		url.PathEscape(liveCorrLocation),
		url.PathEscape(workflowID),
		liveCorrPageSize,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("list executions request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list executions (live): %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read execution list: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("list executions status = %d (body %s)", resp.StatusCode, string(raw))
	}
	var out liveCorrExecutionList
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode execution list: %v (raw %s)", err, string(raw))
	}
	if out.NextPageToken != "" {
		t.Fatalf("execution list is paginated (nextPageToken=%q); a truncated count cannot prove 'exactly one' — raise liveCorrPageSize and re-verify the emulator honours it", out.NextPageToken)
	}
	return out.Executions
}

// liveCorrCountFor counts emulator executions whose launch argument
// carries invID as its investigation_id.
//
// The match is on the parsed field, not a substring, so a count of one
// means one execution launched for this investigation specifically —
// not one execution that happens to share a prefix with another test's
// identity. Other tests' executions for the same workflow are ignored
// by construction.
func liveCorrCountFor(t *testing.T, ctx context.Context, host, workflowID, invID string) int {
	t.Helper()
	all := liveCorrExecutions(t, ctx, host, workflowID)
	n := 0
	for _, e := range all {
		if liveCorrArgField(e.Argument, "investigation_id") == invID {
			n++
		}
	}
	return n
}

// liveCorrArgField extracts one field from an execution's launch
// argument (a JSON object encoded as a string). Returns "" when absent
// or unparseable — an unparseable argument is a correlation failure the
// caller will see as a zero count, not a silent pass.
func liveCorrArgField(argument, field string) string {
	if strings.TrimSpace(argument) == "" {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(argument), &m); err != nil {
		return ""
	}
	return m[field]
}

// liveCorrFind returns the single emulator execution named name. Fails
// when the name the Launcher returned is not a resource the emulator
// actually holds — this is what makes "real execution name" a verified
// claim rather than a non-empty string.
func liveCorrFind(t *testing.T, ctx context.Context, host, workflowID, name string) liveCorrExecution {
	t.Helper()
	for _, e := range liveCorrExecutions(t, ctx, host, workflowID) {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("emulator holds no execution named %q for workflow %q: the name EnsureLaunched returned is not a real emulator resource", name, workflowID)
	return liveCorrExecution{}
}

// liveCorrClaimSource reads the real frozen workflows/claim-investigation.yaml
// by walking up from this test file so the test runs from any workdir.
func liveCorrClaimSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: cannot locate test file")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "workflows", "claim-investigation.yaml")
		if src, err := os.ReadFile(candidate); err == nil {
			return string(src)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("workflows/claim-investigation.yaml not found above test file")
	return ""
}

// liveCorrDeploy deploys the real YAML, tolerating the emulator's 409
// ALREADY_EXISTS: the emulator pre-loads WORKFLOWS_DIR at startup and
// does not update on redeploy. Same tolerance as
// workflow/gcw_live_test.go:68.
func liveCorrDeploy(ctx context.Context, prov *workflow.GCWProvider, t *testing.T) {
	t.Helper()
	if err := prov.DeployWorkflow(ctx, liveCorrWorkflow, liveCorrClaimSource(t)); err != nil {
		if strings.Contains(err.Error(), "status 409") || strings.Contains(err.Error(), "ALREADY_EXISTS") {
			t.Logf("deploy %s: already exists (emulator pre-load), continuing", liveCorrWorkflow)
			return
		}
		t.Fatalf("DeployWorkflow %s (live): %v", liveCorrWorkflow, err)
	}
}

// liveCorrEnvelope builds a valid envelope for the correlation identity.
// The shape is the same one the worker hands the launcher on the
// exception path (compare investigate's launchTestEnv). body is 32 hex
// chars, so every ID satisfies invest.ValidateID's minted shape
// (ex-/inv- plus exactly 32 hex).
func liveCorrEnvelope(t *testing.T, tenant, claim, invID, body string) invest.UnresolvedException {
	t.Helper()
	evID := "ev-" + body
	docID := "doc-" + body
	env := invest.UnresolvedException{
		TenantID: tenant, ClaimID: claim,
		ExceptionID:     "ex-" + body,
		InvestigationID: invID,
		RuleFindings: []invest.RuleFinding{{
			Code: invest.RulePolicyNumberConflict, Severity: invest.SeverityHigh,
			Message: "policy number conflict", EvidenceIDs: []string{evID},
			AffectedFields: []string{"policy_number"},
		}},
		Scope: invest.ScopeConstraints{
			TenantID: tenant, ClaimID: claim,
			AllowTools:   []invest.ToolName{invest.ToolGetClaim},
			MaxToolCalls: 5, DeadlineMs: 5000, RequestID: "req-" + body,
		},
		EvidenceRefs: []invest.EvidenceRef{{
			EvidenceID: evID, SourceType: invest.EvidenceSourceDocument,
			SourceID: docID, TenantID: tenant, ClaimID: claim,
			DocumentID: docID, Page: 1,
		}},
	}
	if err := invest.Validate(env); err != nil {
		t.Fatalf("validate envelope: %v", err)
	}
	return env
}

// liveCorrRawEnvelope reads the raw jsonb column for invID. Reading the
// column (not the decoded struct) is what lets the byte-level claim be
// made at all.
func liveCorrRawEnvelope(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant, invID string) []byte {
	t.Helper()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("begin raw envelope tx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	var raw []byte
	if err := tx.QueryRow(tctx, `SELECT envelope::text FROM investigations WHERE id = $1`, invID).Scan(&raw); err != nil {
		_ = tx.Rollback(tctx)
		t.Fatalf("read raw envelope %q: %v", invID, err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit raw envelope read: %v", err)
	}
	return raw
}

// liveCorrDeleteLaunchRow removes the durable launch record for invID,
// simulating the crash window the reconciler at launch.go:224-247 exists
// to close: StartExecution succeeded, RecordLaunch never committed.
//
// The row belongs to this test run alone (fresh random investigation
// ID per run), so removing it mutates test-owned state only. The
// app role cannot DELETE (migration 010 revokes it), which is the point:
// this is a deliberate, owner-scoped surgery to reach the crash window,
// not an operation production code performs.
func liveCorrDeleteLaunchRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, invID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM workflow_launches WHERE investigation_id = $1`, invID); err != nil {
		t.Fatalf("delete launch row %q (owner role required): %v", invID, err)
	}
}

// TestLiveGCWCorrelation_EnsureLaunched_RealProvider is the APA-39
// qualification. It drives Launcher.EnsureLaunched — the worker's real
// launch boundary — with the REAL GCWProvider and the REAL PG stores
// against a live emulator, and proves the four contract points by
// counting executions in the emulator rather than trusting a bool.
func TestLiveGCWCorrelation_EnsureLaunched_RealProvider(t *testing.T) {
	host := liveCorrEmulatorHost(t)
	pool := liveCorrPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	prov := workflow.NewGCWProvider(host, liveCorrProject, liveCorrLocation)
	liveCorrDeploy(ctx, prov, t)

	// Identity. Same (tenant, claim, document) across every delivery
	// below, so the investigation ID must be identical by construction.
	suffix := liveCorrRand(t, 6)
	body := liveCorrRand(t, 16)
	tenant := "tnt-apa39-" + suffix
	claim := "clm-apa39-" + suffix
	doc := "doc-apa39-" + suffix

	// ASSERTION 4 (identity half): the worker's derivation is stable
	// per document and distinct across documents. This is the property
	// that makes every other dedupe below possible, so it is pinned
	// before the launcher is involved.
	invA, err := investigationIDForDocument(tenant, claim, doc)
	if err != nil {
		t.Fatalf("investigationIDForDocument: %v", err)
	}
	invSame, err := investigationIDForDocument(tenant, claim, doc)
	if err != nil {
		t.Fatalf("investigationIDForDocument (repeat): %v", err)
	}
	if invA != invSame {
		t.Fatalf("investigation ID not stable for the same document: %q vs %q", invA, invSame)
	}
	invOtherDoc, err := investigationIDForDocument(tenant, claim, doc+"-other")
	if err != nil {
		t.Fatalf("investigationIDForDocument (other document): %v", err)
	}
	if invOtherDoc == invA {
		t.Fatalf("distinct documents produced the same investigation ID %q: dedupe would collapse unrelated work", invA)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, invA); err != nil {
		t.Fatalf("derived investigation ID invalid: %v", err)
	}

	env := liveCorrEnvelope(t, tenant, claim, invA, body)
	envs := investigate.NewPGEnvelopeStore(pool)
	launches := investigate.NewPGLaunchStore(pool)
	l := investigate.NewLauncher(envs, prov, launches)

	// Baseline. A fresh investigation ID must own zero emulator
	// executions before the first launch; without this the "exactly
	// one" count below could be satisfied by pre-existing rows.
	if n := liveCorrCountFor(t, ctx, host, liveCorrWorkflow, invA); n != 0 {
		t.Fatalf("baseline: emulator already holds %d executions for fresh ID %q", n, invA)
	}

	// ASSERTION 1: first EnsureLaunched persists the envelope, calls
	// StartExecution exactly once, and returns a real execution name.
	name1, launched1, err := l.EnsureLaunched(ctx, tenant, claim, invA, env, liveCorrWorkflow, investigate.ExpireAuth{})
	if err != nil {
		t.Fatalf("first EnsureLaunched: %v", err)
	}
	if !launched1 {
		t.Fatalf("first EnsureLaunched launched=false, want true (this call owns the launch)")
	}
	if strings.TrimSpace(name1) == "" {
		t.Fatal("first EnsureLaunched returned an empty execution name")
	}
	if n := liveCorrCountFor(t, ctx, host, liveCorrWorkflow, invA); n != 1 {
		t.Fatalf("after first launch: emulator holds %d executions for %q, want exactly 1 (name %q)", n, invA, name1)
	}
	// The name is a resource the emulator actually holds, not just a
	// non-empty string: this is the "real execution name" proof.
	exec1 := liveCorrFind(t, ctx, host, liveCorrWorkflow, name1)
	if exec1.StartTime == "" {
		t.Fatalf("emulator execution %q has no startTime: not a real started execution (%+v)", name1, exec1)
	}
	t.Logf("assertion 1: launched=%v name=%q state=%s (counted via emulator execution list)", launched1, name1, exec1.State)

	// Correlation: the launch argument carried the identity worker →
	// launcher → emulator unchanged. The emulator echoes the argument
	// verbatim, so this is the wire-level proof that the investigation
	// ID, the durable idempotency key, and the tenant binding all
	// crossed the seam intact.
	if got := liveCorrArgField(exec1.Argument, "investigation_id"); got != invA {
		t.Fatalf("emulator argument investigation_id = %q, want %q", got, invA)
	}
	if got := liveCorrArgField(exec1.Argument, "idempotency_key"); got != invA {
		t.Fatalf("emulator argument idempotency_key = %q, want %q (durable key must reach the provider)", got, invA)
	}
	if got := liveCorrArgField(exec1.Argument, "tenant_id"); got != tenant {
		t.Fatalf("emulator argument tenant_id = %q, want %q", got, tenant)
	}
	if got := liveCorrArgField(exec1.Argument, "claim_id"); got != claim {
		t.Fatalf("emulator argument claim_id = %q, want %q", got, claim)
	}
	if got := liveCorrArgField(exec1.Argument, "execution_name"); got == "" {
		t.Fatalf("emulator argument carries no execution_name: the launcher↔provider contract was not honored (%s)", exec1.Argument)
	}

	// ASSERTION 4 (envelope half): the envelope is durable BEFORE the
	// launch and its bytes survived the round trip.
	//
	// Two byte claims, both on the canonical form, and neither on the
	// raw column text: jsonb reorders object keys, so comparing column
	// text to invest.Marshal output would test Postgres' key ordering
	// rather than ClaimOps.
	//
	//   (a) struct claim: the value LoadEnvelope returns deep-equals the
	//       canonical form of what was handed in.
	//   (b) byte claim: invest.Marshal of the loaded envelope is
	//       byte-identical to invest.Marshal of the written envelope,
	//       and the STORED column bytes decode to that same struct.
	//
	// Comparison is against the canonicalized input, not the raw
	// in-memory struct: invest.Marshal applies normalizeNil (nil slice
	// becomes empty slice), so a pre-canonicalization struct and a
	// post-canonicalization one differ by nil-vs-empty even when every
	// field survived. Comparing those directly would report a phantom
	// loss.
	wantBytes, err := invest.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	raw := liveCorrRawEnvelope(t, ctx, pool, tenant, invA)
	if len(raw) == 0 {
		t.Fatalf("envelope column for %q is empty", invA)
	}
	loaded, err := envs.LoadEnvelope(ctx, tenant, invA)
	if err != nil {
		t.Fatalf("LoadEnvelope: %v", err)
	}
	wantStruct, err := invest.Decode(wantBytes)
	if err != nil {
		t.Fatalf("decode canonical envelope: %v", err)
	}
	if !reflect.DeepEqual(loaded, wantStruct) {
		t.Fatalf("envelope did not survive the round trip:\n want %+v\n  got %+v", wantStruct, loaded)
	}
	// The stored column bytes themselves must decode to the same struct:
	// nothing is lost between the write and the load.
	fromColumn, err := invest.Decode(raw)
	if err != nil {
		t.Fatalf("decode stored column bytes %s: %v", raw, err)
	}
	if !reflect.DeepEqual(fromColumn, loaded) {
		t.Fatalf("stored column decodes differently from LoadEnvelope:\n column %+v\n   store %+v", fromColumn, loaded)
	}
	gotBytes, err := invest.Marshal(loaded)
	if err != nil {
		t.Fatalf("re-marshal loaded envelope: %v", err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("envelope canonical bytes drifted across the PG round trip:\n want %s\n  got %s", wantBytes, gotBytes)
	}
	if !bytes.Contains(gotBytes, []byte(invA)) {
		t.Fatalf("round-tripped envelope lost the investigation ID %q: %s", invA, gotBytes)
	}
	t.Logf("assertion 4: envelope %d canonical bytes identical through PG (raw column %d bytes)", len(gotBytes), len(raw))

	// ASSERTION 2: redelivery converges. The second EnsureLaunched sees
	// the durable launch row (launch.go:219-223) and issues NO second
	// StartExecution — proven by the emulator count, not by a bool.
	name2, launched2, err := l.EnsureLaunched(ctx, tenant, claim, invA, env, liveCorrWorkflow, investigate.ExpireAuth{})
	if err != nil {
		t.Fatalf("second EnsureLaunched: %v", err)
	}
	if launched2 {
		t.Fatalf("second EnsureLaunched launched=true, want false (durable launch row must short-circuit)")
	}
	if name2 != name1 {
		t.Fatalf("second EnsureLaunched name = %q, want the durable %q (convergence, not a new execution)", name2, name1)
	}
	if n := liveCorrCountFor(t, ctx, host, liveCorrWorkflow, invA); n != 1 {
		t.Fatalf("after redelivery: emulator holds %d executions for %q, want exactly 1 (a second StartExecution escaped)", n, invA)
	}
	// The durable record is the reason: it must name the execution the
	// emulator actually holds.
	rec, found, err := launches.GetLaunch(ctx, tenant, invA)
	if err != nil || !found {
		t.Fatalf("GetLaunch: found=%v err=%v", found, err)
	}
	if rec.ExecutionName != name1 {
		t.Fatalf("durable launch row execution_name = %q, want %q", rec.ExecutionName, name1)
	}
	t.Logf("assertion 2: redelivery launched=%v name=%q, emulator still holds exactly 1 execution", launched2, name2)

	// ASSERTION 3: N+1 further same-ID deliveries converge to exactly ONE
	// real emulator execution. Against a provider that demonstrably
	// creates a new execution per POST, this is the load-bearing proof
	// that dedupe comes from the durable row.
	const extraDeliveries = 4
	for i := 0; i < extraDeliveries; i++ {
		nameN, launchedN, err := l.EnsureLaunched(ctx, tenant, claim, invA, env, liveCorrWorkflow, investigate.ExpireAuth{})
		if err != nil {
			t.Fatalf("redelivery %d/%d: %v", i+1, extraDeliveries, err)
		}
		if launchedN {
			t.Fatalf("redelivery %d/%d launched=true, want false", i+1, extraDeliveries)
		}
		if nameN != name1 {
			t.Fatalf("redelivery %d/%d name = %q, want %q", i+1, extraDeliveries, nameN, name1)
		}
	}
	total := liveCorrCountFor(t, ctx, host, liveCorrWorkflow, invA)
	if total != 1 {
		names := make([]string, 0, total)
		for _, e := range liveCorrExecutions(t, ctx, host, liveCorrWorkflow) {
			if liveCorrArgField(e.Argument, "investigation_id") == invA {
				names = append(names, e.Name)
			}
		}
		t.Fatalf("after %d deliveries the emulator holds %d executions for %q, want exactly 1: %s",
			extraDeliveries+2, total, invA, strings.Join(names, ", "))
	}
	t.Logf("assertion 3: %d deliveries -> exactly %d emulator execution for %q (counted by listing the emulator)", extraDeliveries+2, total, invA)

	// Assertion 4 (stability across deliveries, restated): every
	// delivery above used an envelope built from the SAME derived ID,
	// and the emulator's stored argument still matches it. Nothing in
	// the chain re-minted identity.
	if got := liveCorrArgField(exec1.Argument, "investigation_id"); got != invSame {
		t.Fatalf("identity unstable across deliveries: emulator holds %q, fresh derivation %q", got, invSame)
	}
}

// TestLiveGCWCorrelation_CrashWindowDuplicate records a GENUINE DEFECT
// found while qualifying the reconciler at launch.go:224-247 against the
// real provider. Production code is deliberately NOT changed here (this
// is a test-only slice); the finding is recorded so a fix has a
// regression target.
//
// The defect
// ----------
// launch.go:76-88 documents the contract: "Conforming providers
// create-or-return this name when the launch argument carries it, so a
// crash between StartExecution and RecordLaunch is recoverable by
// reconciliation instead of a duplicate start." EnsureLaunched
// therefore puts `execution_name` in the launch argument (launch.go:256).
//
// GCWProvider.StartExecution marshals the whole argument map into
// `{"argument": "<json>"}` and POSTs it to the executions COLLECTION.
// It never forwards `execution_name` as Cloud Workflows' `executionId`
// request field, and it does not map a create-409 onto the
// create-or-return outcome. So the documented contract is unimplemented
// on the provider side, and the reconciler at launch.go:232 probes for
// a name the provider will never have minted.
//
// Observed live: with the launch row deleted (the crash window), the
// reconciler's GetExecution(expected) returns typed absence, the code
// proceeds to StartExecution, and the emulator creates a SECOND real
// execution for the same investigation. In production GCW the same
// sequence duplicates the run. The durable row is the only thing
// preventing this, and a crash between the two calls is precisely the
// case the reconciler was written for.
//
// What this test asserts is the OBSERVED behaviour, not an endorsement:
// a second execution is created, and the emulator's minted name never
// equals the requested one. If GCWProvider is fixed to forward
// executionId and honour 409-as-return, this test FAILS with a message
// saying so — which is the intended regression signal. The failure
// message is the contract; do not relax it to make the suite green
// without also fixing the provider.
func TestLiveGCWCorrelation_CrashWindowDuplicate(t *testing.T) {
	host := liveCorrEmulatorHost(t)
	pool := liveCorrPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	prov := workflow.NewGCWProvider(host, liveCorrProject, liveCorrLocation)
	liveCorrDeploy(ctx, prov, t)

	suffix := liveCorrRand(t, 6)
	body := liveCorrRand(t, 16)
	tenant := "tnt-apa39cw-" + suffix
	claim := "clm-apa39cw-" + suffix
	doc := "doc-apa39cw-" + suffix
	invID, err := investigationIDForDocument(tenant, claim, doc)
	if err != nil {
		t.Fatalf("investigationIDForDocument: %v", err)
	}
	env := liveCorrEnvelope(t, tenant, claim, invID, body)
	launches := investigate.NewPGLaunchStore(pool)
	l := investigate.NewLauncher(investigate.NewPGEnvelopeStore(pool), prov, launches)

	first, launched, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, liveCorrWorkflow, investigate.ExpireAuth{})
	if err != nil || !launched || first == "" {
		t.Fatalf("first EnsureLaunched: launched=%v name=%q err=%v", launched, first, err)
	}
	firstExec := liveCorrFind(t, ctx, host, liveCorrWorkflow, first)
	requested := liveCorrArgField(firstExec.Argument, "execution_name")
	if requested == "" {
		t.Fatalf("launch argument carried no execution_name: %s", firstExec.Argument)
	}
	// Observation 1: the emulator minted its own name, so the name the
	// launcher asked for is absent. The reconciler probes exactly that
	// name, so the probe can never adopt anything on this provider.
	if requested == first {
		t.Fatalf("emulator honoured the requested execution_name %q — GCWProvider may now implement the create-or-return contract; revisit this test", requested)
	}
	if _, _, gerr := prov.GetExecution(ctx, requested); gerr == nil {
		t.Fatalf("GetExecution(%q) succeeded: the requested name exists, so the reconciler would adopt it — revisit this test", requested)
	} else if !strings.Contains(gerr.Error(), workflow.ErrExecutionNotFound.Error()) {
		t.Fatalf("GetExecution(%q) error = %v, want typed absence (ErrExecutionNotFound) so the reconciler proceeds to start", requested, gerr)
	}

	// Enter the crash window: StartExecution succeeded, RecordLaunch
	// never committed.
	liveCorrDeleteLaunchRow(t, ctx, pool, invID)
	if _, found, err := launches.GetLaunch(ctx, tenant, invID); err != nil || found {
		t.Fatalf("crash-window setup: launch row still present (found=%v err=%v)", found, err)
	}

	// Redelivery inside the crash window.
	second, launched2, err := l.EnsureLaunched(ctx, tenant, claim, invID, env, liveCorrWorkflow, investigate.ExpireAuth{})
	if err != nil {
		t.Fatalf("crash-window EnsureLaunched: %v", err)
	}
	if !launched2 {
		t.Fatalf("crash-window EnsureLaunched launched=false and adopted %q: the reconciler DID adopt an existing execution. "+
			"If this fires, GCWProvider now implements create-or-return (executionId + 409-as-return); update this test to assert adoption instead of duplication.", second)
	}
	if second == first {
		t.Fatalf("crash-window EnsureLaunched re-returned the existing name %q but reported launched=true", second)
	}

	// Observation 2 (the defect): a duplicate real execution now exists
	// for the same investigation. This is the recorded DEFECT — see the
	// doc comment. If this assert ever fails because the provider began
	// honouring executionId, the fix landed and this test must be
	// rewritten to assert adoption.
	n := liveCorrCountFor(t, ctx, host, liveCorrWorkflow, invID)
	if n < 2 {
		t.Fatalf("crash-window: emulator holds %d executions for %q, expected the duplicate this test documents "+
			"(GCWProvider does not forward execution_name as executionId, so reconciliation cannot adopt). "+
			"If the provider was fixed, rewrite this test to assert adoption of %q instead of duplication", n, invID, first)
	}
	t.Logf("DEFECT RECORDED: crash-window redelivery for %q created %d emulator executions (%q, %q). "+
		"GCWProvider.StartExecution drops the execution_name idempotency key, so launch.go:224-247 reconciliation cannot adopt; "+
		"the durable row is the sole duplicate guard. Production code intentionally untouched in this test-only slice.", invID, n, first, second)
}
