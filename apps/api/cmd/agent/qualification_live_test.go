// APA-49: real-LLM production qualification, service level.
//
// This file drives the production HTTP boundary end to end with REAL
// inference through the REAL GroqModelClient (MODEL_PROVIDER-style
// production wiring: the handler's default model client is a real
// orchestrate.GroqModelClient, and no mock_script is ever sent, so the
// handler's per-request mock override never engages). Below the model
// everything is production too:
//
//   - real Fiber handler (investigationHandler)
//   - real PGEnvelopeStore load: the envelope is authoritative state,
//     never inlined
//   - real buildAgentRegistry tool map
//   - real investigate.PGReaders over live PG
//   - real audit hooks (tool-call + loop lifecycle rows)
//
// It covers the scenarios that need authoritative state to judge, and
// that therefore cannot be decided at the loop level:
//
//   - S4  cross-tenant evidence must never leak
//   - S6  missing required evidence -> R8 -> HITL (the APA-31 path)
//   - S8  tool/transport failure and retry, with no duplicate mutation
//   - S9  budget exhaustion -> deterministic ESCALATED
//   - S10 cancellation/deadline -> context propagates, no orphan
//   - S11 repeated/idempotent execution -> stable, no duplicate write
//
// The loop-level scenarios (S1, S2, S3, S5, S7) and the full wire
// capture live in internal/investigate/orchestrate/groq_qualification_live_test.go,
// which sits in package orchestrate precisely so it can observe the wire
// evidence the production ModelResponse has no field for.
//
// The qualification rule applies here unchanged: pass/fail is decided by
// the deterministic layer, not by the model being right. A run that
// escalated because the model produced an invalid act PASSES; a run that
// accepted an ungrounded, cross-tenant, or duplicate-mutating result
// FAILS. No assertion is weakened to reach a green.
//
// Gating: skips without TEST_POSTGRES_DSN (live PG) and without
// GROQ_API_KEY (live inference), so `go test ./...` stays green in a
// credential-free environment.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	fiber "github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/investigate/orchestrate"
	"claimops-api/internal/repository/postgres"
	"claimops-api/internal/verify"
)

// ---------------------------------------------------------------------------
// Live gate
// ---------------------------------------------------------------------------

// requireLiveQualification returns a live PG pool and a REAL Groq client,
// or skips. The client is built by the production constructor, so the
// model, base URL, timeout, and retry policy are the production ones.
func requireLiveQualification(t *testing.T) (*pgxpool.Pool, orchestrate.ModelClient) {
	t.Helper()
	pool := liveServicePool(t)
	return pool, requireLiveModel(t)
}

// qualUpstream is the real provider base URL. The production client
// defaults to it, so the pacer forwards to the same endpoint production
// uses rather than to a second configuration.
const qualUpstream = "https://api.groq.com/openai/v1"

// requireLiveModel returns a REAL Groq client or skips.
//
// It is separate from the PG gate on purpose. The gate halves of the
// matrix below prove a deterministic refusal and need live PG but NO
// credentials, so a credential-free environment can still verify that
// the specific gates fire. Only the live halves pay for inference.
func requireLiveModel(t *testing.T) orchestrate.ModelClient {
	t.Helper()
	if strings.TrimSpace(os.Getenv("GROQ_API_KEY")) == "" {
		t.Skip("GROQ_API_KEY unset; this subtest requires live inference")
	}
	// Paced through a loopback forwarder; see qualPacer for why the
	// real provider is reached through it rather than directly.
	pacer := newQualPacer(t, qualUpstream)
	return pacer.client(t)
}

// qualApp builds the production handler with the given default model
// client. The real Groq client is not an "empty mock", so the handler's
// dynamic-scrub fallback never engages and real inference happens.
func qualApp(pool *pgxpool.Pool, model orchestrate.ModelClient) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(recover.New())
	app.Post("/v1/investigations", investigationHandler(pool, model))
	return app
}

// ---------------------------------------------------------------------------
// Authoritative-state probes
// ---------------------------------------------------------------------------

// qualAuthoritativeState is the claim-side state a run must not change.
// Every field is read from live PG under the tenant's RLS transaction.
type qualAuthoritativeState struct {
	ClaimRow       string
	ReportRows     int
	OutboxRows     int
	WorkflowLaunch int
}

// readAuthoritativeState snapshots the claim row and the count of every
// authoritative side-effect table for this claim.
func readAuthoritativeState(t *testing.T, pool *pgxpool.Pool, tenant, claim string) qualAuthoritativeState {
	t.Helper()
	var s qualAuthoritativeState
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	s.ClaimRow = snapshotLiveServiceClaim(t, pool, tenant, claim)
	if err := tx.QueryRow(tctx, `SELECT count(*) FROM investigation_reports WHERE tenant_id = $1 AND claim_id = $2`, tenant, claim).Scan(&s.ReportRows); err != nil {
		t.Fatalf("count investigation_reports: %v", err)
	}
	if err := tx.QueryRow(tctx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1 AND aggregate_id = $2`, tenant, claim).Scan(&s.OutboxRows); err != nil {
		t.Fatalf("count outbox_events: %v", err)
	}
	if err := tx.QueryRow(tctx, `SELECT count(*) FROM workflow_launches WHERE tenant_id = $1 AND claim_id = $2`, tenant, claim).Scan(&s.WorkflowLaunch); err != nil {
		t.Fatalf("count workflow_launches: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit state snapshot: %v", err)
	}
	return s
}

// countAuditRows returns the number of audit_log rows the agent wrote for
// this claim. Non-zero is expected and healthy: it is the observability
// evidence, not a mutation.
func countAuditRows(t *testing.T, pool *pgxpool.Pool, tenant, claim string) int {
	t.Helper()
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	var n int
	if err := tx.QueryRow(tctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND claim_id = $2`, tenant, claim).Scan(&n); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit audit count: %v", err)
	}
	return n
}

// assertNoAuthoritativeMutation is the decision rule shared by every
// service-level scenario: the agent runs cognition and MUST NOT write
// claim state, reports, outbox events, or workflow launches. The loop
// never calls T11, so any difference here is a boundary escape.
func assertNoAuthoritativeMutation(t *testing.T, before, after qualAuthoritativeState) {
	t.Helper()
	if before.ClaimRow != after.ClaimRow {
		t.Errorf("claim row mutated:\nbefore=%s\nafter =%s", before.ClaimRow, after.ClaimRow)
	}
	if after.ReportRows != before.ReportRows {
		t.Errorf("investigation_reports rows %d -> %d: the agent wrote a report", before.ReportRows, after.ReportRows)
	}
	if after.OutboxRows != before.OutboxRows {
		t.Errorf("outbox_events rows %d -> %d: the agent published an event", before.OutboxRows, after.OutboxRows)
	}
	if after.WorkflowLaunch != before.WorkflowLaunch {
		t.Errorf("workflow_launches rows %d -> %d: the agent launched a workflow", before.WorkflowLaunch, after.WorkflowLaunch)
	}
}

// assertNoForeignMutation applies the same rule to a FOREIGN tenant's
// claim: it must be byte-identical, proving no cross-tenant write.
func assertNoForeignMutation(t *testing.T, pool *pgxpool.Pool, tenant, claim, before string) {
	t.Helper()
	if after := snapshotLiveServiceClaim(t, pool, tenant, claim); after != before {
		t.Errorf("foreign tenant %s claim %s mutated:\nbefore=%s\nafter =%s", tenant, claim, before, after)
	}
}

// ---------------------------------------------------------------------------
// Pacing forwarder
// ---------------------------------------------------------------------------

// qualPacer is a loopback forwarder that spaces calls to the real
// provider.
//
// Why it exists: this package cannot reach the production client's
// unexported transport, so it cannot observe the provider's rate-limit
// headers the way the loop-level qualification file does. Measured
// directly against the provider, the binding limit is TOKENS and the
// production client reserves its fixed MaxTokens=4096 on every call, so a
// single call consumes roughly half of the per-window budget and the next
// window is ~38-40s away. Without pacing, the second call of any run is
// refused with HTTP 429 and the run degrades into a MODEL_UPSTREAM
// escalation that carries no information about the boundary.
//
// This is a TRANSPORT substitution, not a provider substitution: the
// production GroqModelClient still builds every request, still applies its
// own retry policy, and still reaches the real provider, which serves the
// real ADR-002 model. The only thing added is a wait between round trips.
// Nothing here alters a prompt, a model, or a boundary decision.
type qualPacer struct {
	mu       sync.Mutex
	lastCall time.Time
	gap      time.Duration
	forward  string
	server   *httptest.Server
	hits     atomic.Int32
	// Last relayed provider budget, used to decide whether the next call
	// can fit. Guarded by mu.
	limitTokens     int
	remainingTokens int
	resetTokens     time.Duration
}

// qualPacerReserveTokens mirrors the loop-level pacer's reservation: the
// production client's fixed MaxTokens plus a prompt allowance.
const qualPacerReserveTokens = 4096 + 1024

// qualPacerMaxWait caps any single wait. It must stay well under the
// production per-turn timeout (DefaultTurnTimeoutMs = 30s): a wait longer
// than the turn budget would expire the turn context and turn a valid
// measurement into a MODEL_UPSTREAM escalation, which is how the first
// attempt at this harness was wrong.
const qualPacerMaxWait = 24 * time.Second

// qualPacerMinWait is the floor between two real inference calls.
const qualPacerMinWait = 20 * time.Second

// newQualPacer starts the forwarder and returns it. upstream is the
// provider base URL; the real key is read from the environment inside the
// proxy and never logged.
func newQualPacer(t *testing.T, upstream string) *qualPacer {
	t.Helper()
	// Start pessimistic. The provider's budget is shared with any other
	// consumer of this key, so the first call must assume it is drained
	// and wait; assuming a full budget here produced a 429 on the first
	// request of an earlier revision of this harness.
	p := &qualPacer{
		gap:             qualPacerMinWait,
		limitTokens:     8000,
		forward:         strings.TrimRight(upstream, "/"),
		remainingTokens: 0,
		resetTokens:     qualPacerMinWait,
	}
	if raw := strings.TrimSpace(os.Getenv("QUAL_PACING_GAP")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			t.Fatalf("QUAL_PACING_GAP=%q is not a positive duration", raw)
		}
		p.gap = d
	}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}

// nextWait returns how long to hold the next request, given the provider's
// last reported budget. A call is only released when the budget clearly
// admits it; otherwise the wait runs to the shorter of the reported reset
// and the cap, with a floor that keeps the turn context alive.
func (p *qualPacer) nextWait() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remainingTokens >= qualPacerReserveTokens && p.resetTokens < 2*time.Second {
		return 0
	}
	wait := p.resetTokens
	if wait < p.gap {
		wait = p.gap
	}
	if wait > qualPacerMaxWait {
		wait = qualPacerMaxWait
	}
	return wait
}

// observe records the provider's reported budget from a relayed response.
func (p *qualPacer) observe(h http.Header) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := h.Get("x-ratelimit-limit-tokens"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.limitTokens = n
		}
	}
	if v := h.Get("x-ratelimit-remaining-tokens"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.remainingTokens = n
		}
	}
	if v := h.Get("x-ratelimit-reset-tokens"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			p.resetTokens = time.Duration(secs * float64(time.Second))
		}
	}
}

// client returns a production Groq client wired through the pacer.
//
// The base URL carries no path prefix on purpose: the production client
// appends "/chat/completions" itself, so the pacer receives exactly that
// path and forwards it onto the provider base, with no path doubling.
func (p *qualPacer) client(t *testing.T) orchestrate.ModelClient {
	t.Helper()
	c, err := orchestrate.NewGroqModelClient(os.Getenv("GROQ_API_KEY"), "", p.server.URL)
	if err != nil {
		t.Fatalf("NewGroqModelClient: %v", err)
	}
	return c
}

// serve waits out the pacing gap, then forwards the request upstream and
// relays the response verbatim.
func (p *qualPacer) serve(w http.ResponseWriter, r *http.Request) {
	p.hits.Add(1)
	if wait := p.nextWait(); wait > 0 {
		p.mu.Lock()
		if rest := p.gap - time.Since(p.lastCall); rest > wait {
			wait = rest
		}
		p.mu.Unlock()
		time.Sleep(wait)
	}
	p.mu.Lock()
	p.lastCall = time.Now()
	p.mu.Unlock()

	// The credential is copied into the forwarded request and never
	// logged, echoed, or stored.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.forward+r.URL.Path, r.Body)
	if err != nil {
		http.Error(w, "bad upstream request", http.StatusInternalServerError)
		return
	}
	for _, h := range []string{"Authorization", "Content-Type"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	p.observe(resp.Header)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "upstream body unreadable", http.StatusBadGateway)
		return
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// The live POST
// ---------------------------------------------------------------------------

// qualLiveRun is one service-level qualification run: the handler's
// decoded response plus the captured evidence.
type qualLiveRun struct {
	Scenario    string
	Status      int
	Body        map[string]any
	Latency     time.Duration
	AuditRows   int
	Attempts    []string
	HTTPHits    int
	BeforeState qualAuthoritativeState
	AfterState  qualAuthoritativeState
}

// qualPost drives POST /v1/investigations with NO mock_script, so the
// real model client runs. It records the handler response and the
// per-attempt tool evidence.
func qualPost(t *testing.T, pool *pgxpool.Pool, app *fiber.App, fx *matrixFixture, extra map[string]any) qualLiveRun {
	t.Helper()
	payload := map[string]any{
		"tenant_id":        fx.tenant,
		"claim_id":         fx.claim,
		"investigation_id": fx.invID,
	}
	for k, v := range extra {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/investigations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", fx.tenant)
	start := time.Now()
	resp, err := app.Test(req, -1)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	m := decodeBody(t, resp)
	run := qualLiveRun{Status: resp.StatusCode, Body: m, Latency: elapsed}
	run.Attempts = qualAttemptsFrom(m)
	run.AuditRows = countAuditRows(t, pool, fx.tenant, fx.claim)
	return run
}

// qualAttemptsFrom flattens the handler's attempt_log evidence.
func qualAttemptsFrom(m map[string]any) []string {
	var out []string
	entries, _ := m["attempt_log"].([]any)
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		tool, _ := em["tool"].(string)
		ids, _ := em["response_ids"].([]any)
		rendered := make([]string, 0, len(ids))
		for _, id := range ids {
			if s, ok := id.(string); ok {
				rendered = append(rendered, s)
			}
		}
		out = append(out, fmt.Sprintf("%s%v", tool, rendered))
	}
	return out
}

// qualRecord logs the per-run evidence line.
func qualRecord(t *testing.T, run qualLiveRun) {
	t.Helper()
	t.Logf("APA49-SVC scenario=%s status=%d outcome=%v reason=%v model=%v turns=%v toolCalls=%v attempts=%v auditRows=%d latencyMs=%d",
		run.Scenario, run.Status, run.Body["outcome"], run.Body["escalation_reason"],
		run.Body["model_id"], run.Body["turns_used"], run.Body["tool_calls_used"],
		run.Attempts, run.AuditRows, run.Latency.Milliseconds())
	if errText, ok := run.Body["error"].(string); ok && errText != "" {
		t.Logf("APA49-SVC-ERROR scenario=%s text=%s", run.Scenario, errText)
	}
}

// assertClosedTerminal requires a decision the boundary owns: the run
// either accepted a grounded report or escalated with a closed reason.
// Never a transport error, never an absent outcome.
func assertClosedTerminal(t *testing.T, run qualLiveRun) {
	t.Helper()
	if run.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", run.Status, run.Body)
	}
	outcome, _ := run.Body["outcome"].(string)
	switch orchestrate.Outcome(outcome) {
	case orchestrate.OutcomeReportReady:
		if _, ok := run.Body["report"]; !ok {
			t.Fatalf("REPORT_READY without a report: %v", run.Body)
		}
		if reason, _ := run.Body["escalation_reason"].(string); reason != "" {
			t.Fatalf("REPORT_READY carries escalation reason %q", reason)
		}
	case orchestrate.OutcomeEscalated:
		reason, _ := run.Body["escalation_reason"].(string)
		switch orchestrate.EscalationReason(reason) {
		case orchestrate.EscalationTurnsExhausted, orchestrate.EscalationCallsExhausted,
			orchestrate.EscalationDeadline, orchestrate.EscalationRepetition,
			orchestrate.EscalationNoProgress, orchestrate.EscalationInvalidOutput,
			orchestrate.EscalationModelUpstream:
		default:
			t.Fatalf("ESCALATED with reason outside the closed set: %q", reason)
		}
		if _, ok := run.Body["report"]; ok {
			t.Fatalf("ESCALATED carried an accepted report: %v", run.Body)
		}
	default:
		t.Fatalf("outcome %q is not a closed terminal: %v", outcome, run.Body)
	}
}

// assertGroundedCitations requires every ID that reached a run surface to
// be one the real reader actually produced for THIS tenant. The
// fabrication sentinel must appear nowhere.
func assertGroundedCitations(t *testing.T, run qualLiveRun, fx *matrixFixture) {
	t.Helper()
	seeded := map[string]struct{}{}
	for _, id := range fx.seededIDs() {
		seeded[id] = struct{}{}
	}
	for _, id := range matrixAttemptIDs(t, run.Body) {
		if _, ok := seeded[id]; !ok {
			t.Errorf("attempt log surfaced unseeded evidence id %q (seeded: %v)", id, fx.seededIDs())
		}
		if id == fx.fabrico {
			t.Errorf("attempt log surfaced the fabrication sentinel %q", fx.fabrico)
		}
	}
	if rep, ok := run.Body["report"]; ok {
		for _, id := range matrixCitations(t, rep) {
			if _, ok := seeded[id]; !ok {
				t.Errorf("accepted report cites unseeded evidence id %q", id)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// S4 — cross-tenant evidence must never leak
// ---------------------------------------------------------------------------

// TestQualificationGate_S4_CrossTenantIsolation pins the deterministic I4-request refusal without invoking a model.
// The real-provider half remains TestQualificationLive_S4_CrossTenantIsolation under the qual_live build tag.
func TestQualificationGate_S4_CrossTenantIsolation(t *testing.T) {
	pool := liveServicePool(t)
	t.Setenv("APP_ENV", "test")
	t.Setenv("ALLOW_INLINE_ENVELOPE", "false")

	// --- gate: the specific refusal, deterministic and model-free ---
	t.Run("gate_foreign_tenant_request_refused_before_reader", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 0)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetClaim, invest.ToolGetEvidence}, 5)
		before := readAuthoritativeState(t, pool, fx.tenant, fx.claim)
		foreign := "tnt-apa49-foreign-" + liveServiceRand(t, 4)
		seedLiveServiceClaim(t, pool, foreign, fx.claim+"-foreign")
		foreignBefore := snapshotLiveServiceClaim(t, pool, foreign, fx.claim+"-foreign")

		m := matrixPost(t, pool, fx, [][]byte{matrixCrossTenantRequest(t, fx)})

		matrixAssertEscalated(t, m, orchestrate.EscalationInvalidOutput, "I4-request")
		matrixAssertToolCallsUsed(t, m, 0)
		matrixAssertAttemptLen(t, m, 0)
		after := readAuthoritativeState(t, pool, fx.tenant, fx.claim)
		assertNoAuthoritativeMutation(t, before, after)
		assertNoForeignMutation(t, pool, foreign, fx.claim+"-foreign", foreignBefore)
		t.Logf("APA49-GATE s4 cross-tenant request refused at I4-request, zero reader calls, foreign claim untouched")
	})

}

// ---------------------------------------------------------------------------
// S6 — missing required evidence -> R8 -> HITL
// ---------------------------------------------------------------------------

// TestQualificationGate_S6_MissingRequiredDocumentHITL pins the additive-only R8 refusal without invoking a model.
// The real-provider half remains TestQualificationLive_S6_MissingRequiredDocumentHITL under the qual_live build tag.
func TestQualificationGate_S6_MissingRequiredDocumentHITL(t *testing.T) {
	pool := liveServicePool(t)
	t.Setenv("APP_ENV", "test")
	t.Setenv("ALLOW_INLINE_ENVELOPE", "false")

	// buildR8Envelope is the APA-31 shape: a rule finding at rank 8 for
	// the absent required document, plus the derived missing item the
	// deterministic side derived from it. Key and detail must match
	// invest.Build's emission exactly, or the additive-only check would
	// be testing a fixture bug rather than the boundary.
	buildR8Envelope := func(t *testing.T, fx *matrixFixture) {
		t.Helper()
		env := validEnvelopeForTenant(fx.tenant, fx.claim, fx.invID, fx.exID, fx.reqID)
		env.EvidenceRefs = []invest.EvidenceRef{{
			EvidenceID: fx.anchor, SourceType: invest.EvidenceSourceDocument,
			SourceID: fx.docID, TenantID: fx.tenant, ClaimID: fx.claim,
			DocumentID: fx.docID, Page: 1,
		}}
		// R1 then R8, in verify emission order: the envelope's rule
		// findings are R1-R10 emission order and are never re-sorted. R8
		// projects to no affected fields (the document is absent, so
		// there is no field to blame), which is the deterministic
		// projection rather than an agent assertion.
		env.RuleFindings = []invest.RuleFinding{{
			Code:           invest.RulePolicyNumberConflict,
			Severity:       invest.SeverityHigh,
			Message:        "policy number conflict",
			EvidenceIDs:    []string{fx.anchor},
			AffectedFields: []string{"policy_number"},
		}, {
			Code:           invest.RuleMissingRequiredDocument,
			Severity:       invest.SeverityHigh,
			Message:        "Missing required document: " + verify.DocHospitalBill + ".",
			EvidenceIDs:    []string{fx.anchor},
			AffectedFields: []string{},
		}}
		env.MissingEvidence = []invest.MissingItem{{
			Kind:   invest.MissingRequiredDocument,
			Key:    verify.DocHospitalBill,
			Detail: "R8: " + verify.DocHospitalBill + " absent",
		}}
		env.Scope.AllowTools = []invest.ToolName{invest.ToolGetClaim, invest.ToolGetEvidence}
		env.Scope.MaxToolCalls = 5
		env.Scope.DeadlineMs = 30000
		if err := invest.Validate(env); err != nil {
			t.Fatalf("R8 envelope invalid: %v", err)
		}
		if err := investigate.NewPGEnvelopeStore(pool).SaveEnvelope(t.Context(), env); err != nil {
			t.Fatalf("SaveEnvelope: %v", err)
		}
	}

	// --- gate: the additive-only rule refuses a dropped R8 item ---
	t.Run("gate_dropping_the_r8_item_is_refused", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 0)
		buildR8Envelope(t, fx)
		// A structurally valid report that cites only the known anchor
		// and drops the R8 missing item. Grounding must refuse it: the
		// missing list is additive-only.
		report := orchestrate.Report{
			Hypotheses: []invest.Hypothesis{{
				ID: "h-01", Statement: "the missing bill explains the conflict",
				Falsifier:   "a pinned hospital bill for the claim period",
				Status:      invest.HypothesisOpen,
				EvidenceIDs: []string{fx.anchor},
			}},
			Findings: []invest.Finding{{
				ID: "f-01", HypothesisID: "h-01",
				Summary:     "the cited evidence shows the conflict",
				EvidenceIDs: []string{fx.anchor},
			}},
			Recommendation: invest.Recommendation{
				Action: invest.RecommendReferHuman, Rationale: "human decides",
				FindingIDs: []string{"f-01"},
			},
			// MissingAdditive intentionally omits the R8 item.
			MissingAdditive: nil,
		}
		raw, err := json.Marshal(orchestrate.ModelAction{
			Action: orchestrate.ActionSubmitReport, Report: &report,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		m := matrixPost(t, pool, fx, [][]byte{raw})
		matrixAssertEscalated(t, m, orchestrate.EscalationInvalidOutput, "I6-grounding")
		if msg, _ := m["error"].(string); !strings.Contains(msg, "additive only") {
			t.Fatalf("error = %q, want the additive-only cause", msg)
		}
		if msg, _ := m["error"].(string); !strings.Contains(msg, verify.DocHospitalBill) {
			t.Fatalf("error = %q, want the dropped R8 document named", msg)
		}
		t.Logf("APA49-GATE s6 dropping the R8 required-document item refused at I6-grounding (additive only)")
	})

}

// ---------------------------------------------------------------------------
// S8 — failure and retry with no duplicate mutation
// ---------------------------------------------------------------------------

// TestQualificationS8_TransportRetryBounds proves bounded production-client retries against loopback HTTP stubs.
// The real-provider no-duplicate-mutation half remains TestQualificationLive_S8_RetryNoDuplicateMutation under qual_live.
func TestQualificationS8_TransportRetryBounds(t *testing.T) {
	pool := liveServicePool(t)
	t.Setenv("APP_ENV", "test")
	t.Setenv("ALLOW_INLINE_ENVELOPE", "false")

	// --- retry semantics: real client, real socket, first call 503 ---
	t.Run("bounded_retry_recovers_and_lands_grounded", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 2)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetClaim, invest.ToolGetEvidence}, 5)
		before := readAuthoritativeState(t, pool, fx.tenant, fx.claim)

		var hits int32
		var served []int32
		// The first request is a retryable 503; the second is a valid
		// grounded act over the envelope's own anchor. The production
		// client must retry exactly once and then succeed.
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&hits, 1)
			served = append(served, n)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-request-id", fmt.Sprintf("stub-req-%d", n))
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream busy","type":"server_error","code":"server_error"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"stub-ok","model":"stub-503-then-200","choices":[{"message":{"content":` +
				quoteJSON(matrixSubmitReport(t, fx.anchor)) + `}}]}`))
		}))
		defer stub.Close()

		client, err := orchestrate.NewGroqModelClient(qualStubKey, "", stub.URL+"/v1")
		if err != nil {
			t.Fatalf("NewGroqModelClient: %v", err)
		}
		run := qualPost(t, pool, qualApp(pool, client), fx, nil)
		run.BeforeState, run.AfterState = before, readAuthoritativeState(t, pool, fx.tenant, fx.claim)
		run.Scenario, run.HTTPHits = "s8_bounded_retry", int(atomic.LoadInt32(&hits))
		qualRecord(t, run)

		// The deciding assertion: the client retried, and it retried
		// exactly the bounded number of times.
		if got := atomic.LoadInt32(&hits); got != 2 {
			t.Fatalf("provider saw %d requests, want exactly 2 (one 503, one bounded retry): %v", got, served)
		}
		if outcome, _ := run.Body["outcome"].(string); outcome != string(orchestrate.OutcomeReportReady) {
			t.Fatalf("outcome = %q, want REPORT_READY after a recovered retry (body %v)", outcome, run.Body)
		}
		assertClosedTerminal(t, run)
		assertGroundedCitations(t, run, fx)
		assertNoAuthoritativeMutation(t, before, run.AfterState)
	})

	// --- the retry must stay bounded when the provider never recovers ---
	t.Run("retry_is_bounded_not_infinite", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 0)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetEvidence}, 5)
		before := readAuthoritativeState(t, pool, fx.tenant, fx.claim)

		var hits int32
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream busy","type":"server_error","code":"server_error"}}`))
		}))
		defer stub.Close()

		client, err := orchestrate.NewGroqModelClient(qualStubKey, "", stub.URL+"/v1")
		if err != nil {
			t.Fatalf("NewGroqModelClient: %v", err)
		}
		run := qualPost(t, pool, qualApp(pool, client), fx, nil)
		run.BeforeState, run.AfterState = before, readAuthoritativeState(t, pool, fx.tenant, fx.claim)
		run.Scenario, run.HTTPHits = "s8_retry_bounded", int(atomic.LoadInt32(&hits))
		qualRecord(t, run)

		// Two model calls are possible: the client's internal retry, then
		// the loop's single upstream retry. The point is that the count
		// is small and terminal, never unbounded, and that the run
		// escalates deterministically instead of looping.
		if got := atomic.LoadInt32(&hits); got < 2 || got > 4 {
			t.Fatalf("provider saw %d requests, want a bounded 2..4 (client retry + one loop retry)", got)
		}
		if reason, _ := run.Body["escalation_reason"].(string); reason != string(orchestrate.EscalationModelUpstream) {
			t.Fatalf("reason = %q, want MODEL_UPSTREAM (body %v)", reason, run.Body)
		}
		if msg, _ := run.Body["error"].(string); !strings.Contains(msg, "exhausted") {
			t.Fatalf("error = %q, want the bounded-exhausted marker", msg)
		}
		assertClosedTerminal(t, run)
		assertNoAuthoritativeMutation(t, before, run.AfterState)
	})

}

// qualStubKey is the placeholder credential for the transport-substitution
// halves of S8. Those halves drive the production retry predicate against
// a loopback stub, so they need a non-empty key to construct the client
// and must NOT use a real credential: no request leaves loopback. It is
// not a secret and is deliberately not read from the environment.
const qualStubKey = "stub-transport-substitution-not-a-credential"

// quoteJSON renders s as a JSON string literal for embedding in a stub
// response body.
func quoteJSON(s []byte) string {
	raw, err := json.Marshal(string(s))
	if err != nil {
		return `""`
	}
	return string(raw)
}

// mustInt reads a numeric body field, failing the test if absent.
func mustInt(t *testing.T, v any) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("value %T is not numeric", v)
	}
	return int(f)
}
