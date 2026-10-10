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
	"claimops-api/internal/investigate/orchestrate"
	"claimops-api/internal/repository/postgres"
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

// TestQualificationLive_S4_CrossTenantIsolation covers scenario 4.
//
// The deciding deterministic assertion is tenant identity authority: the
// request identity must come from the scope, never from the model, and a
// foreign tenant's claim row is byte-identical afterwards. The live model
// is given a real investigation for its own tenant; whatever it emits,
// the invariant is that no foreign-tenant ID reaches any run surface and
// no foreign claim is written.
//
// The gate half reuses the APA-38 cross-tenant construction, so the
// specific I4-request refusal stays proven independently of the model.

// ---------------------------------------------------------------------------
// S6 — missing required evidence -> R8 -> HITL
// ---------------------------------------------------------------------------

// TestQualificationLive_S6_MissingRequiredDocumentHITL covers scenario 6
// on the APA-31 path: an absent required document raises R8
// (MISSING_REQUIRED_DOCUMENT), and the investigation routes to a human
// instead of resolving itself.
//
// The deciding deterministic assertion is additive-only: the R8 missing
// item is already known open by the deterministic side, so the boundary
// must never let a report drop it. Dropping it would be the investigation
// quietly forgetting a document it never saw, which is precisely the
// failure mode HITL exists to prevent.
//
// The gate half reuses the APA-31/APA-38 constructions; the live half
// runs the real model against an envelope that genuinely carries an R8
// finding and its derived missing item.

// ---------------------------------------------------------------------------
// S8 — failure and retry with no duplicate mutation
// ---------------------------------------------------------------------------

// TestQualificationLive_S8_RetryNoDuplicateMutation covers scenario 8.
//
// Two halves, because "tool failure/retry" and "no duplicate mutation"
// are two different claims:
//
//   - retry semantics: the production Groq client's bounded retry is
//     exercised for real over a real HTTP socket. The transport is
//     substituted (a local endpoint that answers 503 once, then a valid
//     completion) but the client, its retry predicate, and its backoff
//     are the production ones. This is a transport substitution, not a
//     provider substitution, and it is labelled as such in the log.
//   - no duplicate mutation: the same real client against live Groq, with
//     a real reader, where the decisive assertion is that the
//     authoritative state is byte-identical regardless of what the model
//     did or how many turns it burned.

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

// ---------------------------------------------------------------------------
// S9 — budget exhaustion is deterministic
// ---------------------------------------------------------------------------

// TestQualificationLive_S9_BudgetExhaustion covers scenario 9.
//
// The deciding deterministic assertion is that a spent budget is
// ESCALATED, never a silent stop and never a further turn. The tool
// budget is the one the envelope controls, so a MaxToolCalls of 1 makes
// the boundary's arithmetic the deciding factor: the run may execute at
// most one read, and the second attempt is refused before Execute.

// ---------------------------------------------------------------------------
// S10 — deadline propagates, nothing is orphaned
// ---------------------------------------------------------------------------

// TestQualificationLive_S10_DeadlinePropagates covers scenario 10.
//
// What is deterministic here, and is therefore what this test asserts:
// the run reaches a closed typed terminal, executes NO tool, and leaves
// the authoritative state byte-identical. A deadline is an abort, not a
// partial write, so "nothing happened" is the whole promise.
//
// The DEADLINE reason specifically is NOT asserted, and the reason is
// worth recording rather than papering over. The loop's deadline is
// turn-granular: it is checked at the top of every turn and re-checked
// after a tool executes, because one Complete call may legitimately
// outlive the deadline under the turn cap (loop.go package contract).
// Turn 1 therefore always starts inside the budget, and if the model
// fails validation on its first act the run escalates at the decode
// boundary before any deadline re-check is reached. Against the real
// ADR-002 model that is exactly what happens: the first act is refused as
// I1-malformed and INVALID_OUTPUT wins the race. So DEADLINE is
// unreachable through this boundary for this model, and the terminal
// itself is qualified at the loop level, where a delay seam exists, in
// TestQualification_S10_DeadlineAndCancellation and in the pre-existing
// TestDeadlineEscalates. Asserting DEADLINE here would mean forcing an
// outcome the production control flow does not produce, which is exactly
// the kind of manufactured evidence this qualification forbids.

// ---------------------------------------------------------------------------
// S11 — repeated execution is stable and non-duplicating
// ---------------------------------------------------------------------------

// TestQualificationLive_S11_IdempotentExecution covers scenario 11.
//
// Two identical POSTs for one investigation_id. The deciding assertions
// are: the authoritative state is byte-identical across both runs (no
// duplicate authoritative mutation), and the outcome is stable — same
// terminal class, and where both accept a report, byte-identical
// reports. Stability is asserted at the terminal class and the report
// bytes, never at the model's prose, which is allowed to vary.

// mustInt reads a numeric body field, failing the test if absent.
func mustInt(t *testing.T, v any) int {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("value %T is not numeric", v)
	}
	return int(f)
}
