// APA-36 Slice C: full workflows/claim-investigation.yaml execution
// against the live GCW emulator with real Agent + API backends.
//
// HONEST TERMINAL STATE (read before reinterpreting): the full-YAML run
// reaches `investigate` (live Agent returns 200) and then terminates
// SUCCEEDED with result null WITHOUT traversing `check` or creating the
// HITL callback. This is a GCW-emulator v0.5.0 defect, not YAML
// semantics: ANY successful http.* step carrying a `retry:` block ends
// the execution with result null instead of continuing. Minimal
// reproducer (no Agent, no claim YAML involved):
//
//	main:
//	  params: [args]
//	  steps:
//	    - fetch:
//	        call: http.get
//	        args:
//	          url: "http://claimops-api:8080/healthz"
//	          timeout: 10
//	        retry:
//	          predicate: ${http.default_retry_predicate}
//	          max_retries: 3
//	          backoff: {initial_delay: 1, max_delay: 10, multiplier: 2}
//	        result: r
//	    - done:
//	        return: after-fetch
//
// yields {"state":"SUCCEEDED","result":"null"} with emulator logs
// "HTTP GET ... -> 200" followed immediately by "completed
// successfully" (`done` never runs). The identical workflow WITHOUT the
// retry block returns "after-fetch". Custom predicates behave the same,
// and `ghcr.io/lemonberrylabs/gcw-emulator:latest` is already v0.5.0
// (digest 1146566c9f58), so no newer emulator fixes it. The frozen
// claim-investigation.yaml correctly carries `retry:` on investigate /
// apply (production GCW retries transient 5xx per http.default_retry_
// predicate), so the YAML is NOT changed to work around the emulator:
// retry semantics are frozen. Control probes show the rest of the
// machinery is sound: `switch`+`next` routes REPORT_READY (noretry
// probe returned "took-A" against the REAL agent 200 body), and
// events.create_callback_endpoint parks ACTIVE with a listed callback.
//
// What this test therefore proves (no fabrication):
// (a) Agent boundary: direct POST returns REPORT_READY with a grounded
// attempt log and report citations through the REAL Loop + tools +
// PGReaders (same envelope the workflow's investigate step sends).
// (b) Full-YAML execution: investigate reaches the live Agent (any
// 4xx/5xx/DNS would surface as FAILED with the HTTP payload — see the
// 400/405 FAILED controls), then the run terminates SUCCEEDED/null
// with ZERO callbacks (branch traversal BLOCKED on the emulator
// defect above; Slice D/F input — likely an emulator bump or
// Slice F HITL-resume harness, never a YAML edit).
// (c) Authoritative state untouched: claim row byte-identical
// before/after (agent + workflow wrote zero claim mutations).
//
// The HITL callback is deliberately NOT driven: no callback endpoint
// is ever created, and nothing here forces SUCCEEDED — SUCCEEDED/null
// is the observed emulator behavior, asserted exactly as observed.
//
// Infra (docker run only; repo rule forbids compose). All containers
// share claimops-net so emulator $AGENT_URL/$API_URL resolve by name:
//
//	docker network create claimops-net
//	docker network connect claimops-net claimops-postgres
//	docker run -d \
//	  --name gcw-emulator \
//	  --network claimops-net \
//	  -p 8787:8787 \
//	  -p 8788:8788 \
//	  -v $(pwd)/workflows:/workflows \
//	  -e WORKFLOWS_DIR=/workflows \
//	  -e PROJECT=my-project \
//	  -e LOCATION=us-central1 \
//	  -e AGENT_URL=http://claimops-agent:8081 \
//	  -e API_URL=http://claimops-api:8080 \
//	  ghcr.io/lemonberrylabs/gcw-emulator:latest
//	docker build -f apps/api/Dockerfile.agent -t claimops-agent:apa36 apps/api
//	docker run -d \
//	  --name claimops-agent \
//	  --network claimops-net \
//	  -p 8081:8081 \
//	  -e DATABASE_URL="postgres://claimops_app:claimops_app@claimops-postgres:5432/claimops" \
//	  -e APP_ENV=local \
//	  -e ALLOW_INLINE_ENVELOPE=false \
//	  -e MODEL_PROVIDER=mock \
//	  -e AGENT_PORT=8081 \
//	  -e PORT=8081 \
//	  claimops-agent:apa36
//	docker build -f apps/api/Dockerfile -t claimops-api:apa36 apps/api
//	docker run -d \
//	  --name claimops-api \
//	  --network claimops-net \
//	  -p 8080:8080 \
//	  -e DATABASE_URL="postgres://claimops_app:claimops_app@claimops-postgres:5432/claimops" \
//	  -e APP_ENV=local \
//	  -e PORT=8080 \
//	  -e HITL_WEBHOOK_SECRET="apa36-live-secret" \
//	  claimops-api:apa36
//
// Postgres :5433 is the existing claimops-postgres container (joined to
// claimops-net via `docker network connect`). Mockoon :3001 is NOT
// needed: the agent tool registry (get_claim, get_documents,
// get_evidence, search_evidence, get_verification_findings) is PG-read
// only, so this path performs zero external HTTP calls.
//
// Run live (all backends above must be up):
//
//	TEST_POSTGRES_DSN=postgres://claimops:claimops@localhost:5433/claimops \
//	WORKFLOWS_EMULATOR_HOST=localhost:8787 \
//	AGENT_URL=http://localhost:8081 \
//	go test ./internal/workflow/ -run TestGCWLive_ClaimInvestigationReportReady -v -count=1
//
// Stop afterwards (leave no stray containers):
//
//	docker rm -f gcw-emulator claimops-agent claimops-api
//
// The test skips when WORKFLOWS_EMULATOR_HOST/TEST_POSTGRES_DSN is
// unset or the emulator/PG is unreachable (same live-gated skip
// pattern as gcw_live_test.go). Agent failures are hard failures, not
// skips: the Agent is a required backend for this slice.
package workflow_test

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
	"os"
	"path/filepath"
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

// liveFullHost returns the emulator host or skips when absent/unreachable.
func liveFullHost(t *testing.T) string {
	t.Helper()
	host := strings.TrimSpace(os.Getenv("WORKFLOWS_EMULATOR_HOST"))
	if host == "" {
		t.Skip("WORKFLOWS_EMULATOR_HOST unset: skipping live full-YAML test")
	}
	dialHost := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSuffix(host, "/"), "http://"), "https://")
	probe, err := net.DialTimeout("tcp", dialHost, 2*time.Second)
	if err != nil {
		t.Skipf("GCW emulator at %q unreachable: %v", host, err)
	}
	probe.Close()
	return host
}

// liveFullPool dials live PG or skips (same gate as pg-gated suites).
func liveFullPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn, ok := os.LookupEnv("TEST_POSTGRES_DSN")
	if !ok || strings.TrimSpace(dsn) == "" {
		t.Skip("TEST_POSTGRES_DSN unset: skipping live full-YAML test")
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

// liveFullRand returns hex suffixes for unique tenant/claim/evidence IDs.
func liveFullRand(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// liveFullAgentURL is the Agent service base URL (host-side port).
func liveFullAgentURL() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_URL")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "http://localhost:8081"
}

// liveFullClaimSource reads workflows/claim-investigation.yaml by walking
// up from this test file so the test runs from any workdir.
func liveFullClaimSource(t *testing.T) string {
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

// liveFullSeedClaim inserts the parent claim row FK-bound rows require.
func liveFullSeedClaim(t *testing.T, pool *pgxpool.Pool, tenant, claim string) {
	t.Helper()
	c, err := claims.NewClaim(
		claims.ClaimID(claim),
		claims.TenantID(tenant),
		claims.PolicyID("pol-"+claim),
		"ref-"+claim,
		claims.MustPaise(100, 0),
		claims.ClaimStatusReceived,
		1,
		time.Time{},
		time.Time{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewClaim: %v", err)
	}
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx(%s): %v", tenant, err)
	}
	if err := postgres.New(pool).SaveClaim(tctx, tx, *c); err != nil {
		_ = tx.Rollback(tctx)
		t.Fatalf("SaveClaim: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}
}

// liveFullSeedEvidence inserts one evidence row the agent tools read.
func liveFullSeedEvidence(t *testing.T, pool *pgxpool.Pool, tenant, claim, evID, docID string) {
	t.Helper()
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("begin evidence tx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	if _, err := tx.Exec(tctx, `INSERT INTO evidence (id, tenant_id, claim_id, source_type, source_id, retrieved_at, content_hash, status) VALUES ($1,$2,$3,$4,$5,now(),$6,$7) ON CONFLICT (id) DO NOTHING`,
		evID, tenant, claim, string(invest.EvidenceSourceDocument), docID, "sha256-apa36-seed", "RETRIEVED"); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit evidence: %v", err)
	}
}

// liveFullSnapshot renders the full claim row canonically so the test can
// prove byte-identical state before/after the agent + workflow run.
func liveFullSnapshot(t *testing.T, pool *pgxpool.Pool, tenant, claim string) string {
	t.Helper()
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("begin snapshot tx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	var id, rowTenant, policy, ref, status string
	var amount int64
	var version int
	var incident, admission, discharge *time.Time
	err = tx.QueryRow(tctx, `SELECT id, tenant_id, policy_id, reference, amount_paise, status, version, incident_date, admission_date, discharge_date FROM claims WHERE id = $1`,
		claim).Scan(&id, &rowTenant, &policy, &ref, &amount, &status, &version, &incident, &admission, &discharge)
	if err != nil {
		t.Fatalf("snapshot claim: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit snapshot: %v", err)
	}
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s|%d|%v|%v|%v", id, rowTenant, policy, ref, amount, status, version, incident, admission, discharge)
}

// liveFullEnvelope builds the store-loaded envelope for the dynamic-mock
// REPORT_READY path: the Agent synthesizes CALL_TOOL get_claim +
// SUBMIT_REPORT grounded on EvidenceRefs[0], so the envelope must carry
// an AgreedSnapshot entry or grounding Gate B fails closed (ESCALATED).
func liveFullEnvelope(tenant, claim, invID, exID, reqID, evID, docID string) invest.UnresolvedException {
	return invest.UnresolvedException{
		TenantID:        tenant,
		ClaimID:         claim,
		ExceptionID:     exID,
		InvestigationID: invID,
		RuleFindings: []invest.RuleFinding{{
			Code:           invest.RulePolicyNumberConflict,
			Severity:       invest.SeverityHigh,
			Message:        "policy number conflict",
			EvidenceIDs:    []string{evID},
			AffectedFields: []string{"policy_number"},
		}},
		Scope: invest.ScopeConstraints{
			TenantID: tenant,
			ClaimID:  claim,
			// Single-tool scope: the dynamic mock issues CALL_TOOL on
			// the first allowlisted tool, and get_evidence returns
			// the seeded evidence ID so the attempt log is observably
			// grounded (get_claim would return only the claim ID).
			AllowTools:   []invest.ToolName{invest.ToolGetEvidence},
			MaxToolCalls: 5,
			DeadlineMs:   30000,
			RequestID:    reqID,
		},
		EvidenceRefs: []invest.EvidenceRef{{
			EvidenceID: evID,
			SourceType: invest.EvidenceSourceDocument,
			SourceID:   docID,
			TenantID:   tenant,
			ClaimID:    claim,
			DocumentID: docID,
			Page:       1,
		}},
		AgreedSnapshot: []invest.AgreedField{{
			Key:         "hospital_name",
			Agreed:      "City Hospital",
			EvidenceIDs: []string{evID},
		}},
	}
}

// TestGCWLive_ClaimInvestigationFullYaml executes the FULL
// workflows/claim-investigation.yaml against the live GCW emulator with
// the real Agent service backend.
//
// Boundary asserts: (a) direct Agent POST returns REPORT_READY with a
// grounded attempt log and report citations; (b) the workflow execution
// reaches investigate against the live Agent (200, else FAILED with the
// HTTP payload) and then terminates in the documented emulator
// short-circuit state SUCCEEDED/null with zero HITL callbacks (branch
// traversal blocked — see file header, never claimed here); (c) the
// claim row is byte-identical before/after.
func TestGCWLive_ClaimInvestigationFullYaml(t *testing.T) {
	host := liveFullHost(t)
	pool := liveFullPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	suffix := liveFullRand(t, 6)
	tenant := "tnt-apa36-" + suffix
	claim := "clm-apa36-" + suffix
	docID := "doc-apa36-" + suffix
	evID := "ev-apa36-" + suffix
	invID := "inv-" + liveFullRand(t, 16)
	exID := "ex-" + liveFullRand(t, 16)
	reqID := "req-apa36-" + suffix

	liveFullSeedClaim(t, pool, tenant, claim)
	liveFullSeedEvidence(t, pool, tenant, claim, evID, docID)
	env := liveFullEnvelope(tenant, claim, invID, exID, reqID, evID, docID)
	if err := invest.Validate(env); err != nil {
		t.Fatalf("Validate envelope: %v", err)
	}
	if err := investigate.NewPGEnvelopeStore(pool).SaveEnvelope(context.Background(), env); err != nil {
		t.Fatalf("SaveEnvelope: %v", err)
	}

	before := liveFullSnapshot(t, pool, tenant, claim)

	// (a) Agent boundary: direct POST proves REPORT_READY + grounded
	// attempt log through the REAL Loop + tools + PGReaders before the
	// workflow traverses the same path.
	agentBody, err := json.Marshal(map[string]any{
		"tenant_id":        tenant,
		"claim_id":         claim,
		"investigation_id": invID,
	})
	if err != nil {
		t.Fatalf("marshal agent body: %v", err)
	}
	agentReq, err := http.NewRequestWithContext(ctx, http.MethodPost, liveFullAgentURL()+"/v1/investigations", bytes.NewReader(agentBody))
	if err != nil {
		t.Fatalf("agent request: %v", err)
	}
	agentReq.Header.Set("Content-Type", "application/json")
	agentReq.Header.Set("X-Tenant-ID", tenant)
	agentResp, err := http.DefaultClient.Do(agentReq)
	if err != nil {
		t.Fatalf("POST agent /v1/investigations (live backend required): %v", err)
	}
	agentRaw, err := io.ReadAll(agentResp.Body)
	agentResp.Body.Close()
	if err != nil {
		t.Fatalf("read agent body: %v", err)
	}
	if agentResp.StatusCode != 200 {
		t.Fatalf("agent status = %d, want 200 (body %s)", agentResp.StatusCode, string(agentRaw))
	}
	var agentM map[string]any
	if err := json.Unmarshal(agentRaw, &agentM); err != nil {
		t.Fatalf("decode agent body: %v", err)
	}
	if got := agentM["outcome"]; got != "REPORT_READY" {
		t.Fatalf("agent outcome = %v, want REPORT_READY (full body %v)", got, agentM)
	}
	alog, ok := agentM["attempt_log"].([]any)
	if !ok || len(alog) == 0 {
		t.Fatalf("agent attempt_log missing/empty: %v", agentM)
	}
	grounded := false
	for _, entry := range alog {
		em, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		ids, _ := em["response_ids"].([]any)
		for _, id := range ids {
			if id == evID {
				grounded = true
			}
		}
	}
	if !grounded {
		t.Fatalf("agent attempt_log not grounded on seeded evidence %q: %v", evID, alog)
	}
	rep, ok := agentM["report"].(map[string]any)
	if !ok {
		t.Fatalf("agent report missing: %v", agentM)
	}
	hyps, _ := rep["hypotheses"].([]any)
	if len(hyps) != 1 {
		t.Fatalf("agent hypotheses = %v, want 1", hyps)
	}
	h0, _ := hyps[0].(map[string]any)
	cited, _ := h0["evidence_ids"].([]any)
	if len(cited) != 1 || cited[0] != evID {
		t.Fatalf("agent hypothesis evidence_ids = %v, want [%q]", cited, evID)
	}
	t.Logf("agent boundary: REPORT_READY grounded on %q", evID)

	// (b) Full-YAML execution: deploy, start, and assert the honest
	// terminal state. investigate (60s timeout, dormant retry) calls the
	// live Agent; any 4xx/5xx/DNS surfaces as FAILED with the HTTP error
	// payload (proven by the 400/405 FAILED controls), so reaching
	// SUCCEEDED proves investigate returned 2xx from the real backend.
	// The emulator retry-success defect then ends the run with result
	// null before `check`: assert exactly that, plus zero callbacks
	// (the run never reaches await_human — branch traversal is blocked,
	// recorded here, never faked).
	provider := workflow.NewGCWProvider(host, "my-project", "us-central1")
	claimSrc := liveFullClaimSource(t)
	if err := provider.DeployWorkflow(ctx, "claim-investigation", claimSrc); err != nil {
		if !strings.Contains(err.Error(), "status 409") && !strings.Contains(err.Error(), "ALREADY_EXISTS") {
			t.Fatalf("DeployWorkflow claim-investigation (live): %v", err)
		}
		t.Logf("deploy claim-investigation: already exists (emulator pre-load), continuing")
	}
	execName, err := provider.StartExecution(ctx, "claim-investigation", map[string]string{
		"claim_id":         claim,
		"tenant_id":        tenant,
		"investigation_id": invID,
	})
	if err != nil {
		t.Fatalf("StartExecution claim-investigation (live): %v", err)
	}
	if execName == "" {
		t.Fatal("StartExecution returned empty execution name")
	}
	t.Logf("execution: %s", execName)

	// Poll to terminal (investigate 60s timeout + agent run ~5s; the
	// short-circuit lands in seconds on success — allow 120s).
	terminal := ""
	var terminalResult json.RawMessage
	deadline := time.Now().Add(120 * time.Second)
	for {
		state, result, execErr := provider.GetExecution(ctx, execName)
		switch state {
		case "ACTIVE", "":
			// Still running; keep polling.
		case "FAILED":
			t.Fatalf("execution FAILED (honest terminal state, must be triaged): result=%s execErr=%v", string(result), execErr)
		case "SUCCEEDED":
			terminal, terminalResult = state, result
			goto TERMINAL
		default:
			t.Fatalf("unexpected execution state %q: result=%s execErr=%v", state, string(result), execErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for terminal state (last state %q)", state)
		}
		time.Sleep(2 * time.Second)
	}
TERMINAL:
	t.Logf("terminal state: %s result=%s", terminal, string(terminalResult))
	// Documented emulator short-circuit signature: SUCCEEDED with a
	// null result (the `done`/`check` steps never run). The emulator
	// encodes it as the JSON string "null" on the wire. If a future
	// emulator traverses to the HITL park (ACTIVE + one hitl-decision
	// callback), this assert must be revisited — that is Slice F input.
	if got := strings.Trim(string(terminalResult), `"`); got != "null" {
		t.Fatalf("result = %s, want null (emulator retry-success short-circuit; a non-null result means emulator behavior changed — revisit)", string(terminalResult))
	}
	cbRaw := getExecutionCallbacks(t, ctx, host, execName)
	var cbBody map[string]any
	if err := json.Unmarshal(cbRaw, &cbBody); err != nil {
		t.Fatalf("decode callbacks body: %v (raw %s)", err, string(cbRaw))
	}
	cbs, _ := cbBody["callbacks"].([]any)
	if len(cbs) != 0 {
		t.Fatalf("callbacks = %s, want zero (run never reaches await_human on emulator v0.5.0)", string(cbRaw))
	}
	t.Logf("full-YAML: investigate 200 via live agent; short-circuit SUCCEEDED/null; zero callbacks (branch blocked, documented)")

	// (c) Authoritative state untouched: the agent (Loop + tools) never
	// mutates claims; only the HITL-driven API decision endpoint may
	// (Slice F). The callback is deliberately NOT driven here.
	after := liveFullSnapshot(t, pool, tenant, claim)
	if before != after {
		t.Fatalf("claim row mutated by agent/workflow:\nbefore=%s\nafter =%s", before, after)
	}
}

// getExecutionCallbacks fetches the emulator's per-execution callback
// list: GET /v1/{executionName}/callbacks.
func getExecutionCallbacks(t *testing.T, ctx context.Context, host, execName string) json.RawMessage {
	t.Helper()
	base := strings.TrimSuffix(host, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	trimmed := strings.TrimPrefix(execName, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/"+trimmed+"/callbacks", nil)
	if err != nil {
		t.Fatalf("callbacks request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET callbacks (live): %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read callbacks body: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("GET callbacks status = %d (body %s)", resp.StatusCode, string(raw))
	}
	return json.RawMessage(raw)
}
