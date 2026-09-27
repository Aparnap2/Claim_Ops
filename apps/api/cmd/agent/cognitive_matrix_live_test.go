// APA-38: cognitive failure matrix over real PGReaders at the Agent-service
// level (Phase-3 plan step 10).
//
// Slice A (mock_report_ready_live_test.go) proved the happy path end to end:
// POST /v1/investigations -> real Loop + real tools + real PGReaders ->
// REPORT_READY, grounded, claim row unchanged. This file qualifies the
// FAILURE side of the same seam: the seven loop-level guardrails that were
// only ever proven against stub ToolFuncs (orchestrate_test.go,
// guardrail_test.go) are re-driven here through the production registry
// (buildAgentRegistry) and the real investigate.PGReaders against live PG.
//
// Only the ModelClient is scripted. Every seam below the model is real:
//
//   - real Fiber handler (cmd/agent investigationHandler)
//   - real PGEnvelopeStore load (no inline envelope)
//   - real buildAgentRegistry tool map
//   - real investigate.PGReaders over the live pool
//   - real audit hooks (tool-call + loop lifecycle rows)
//
// Live-PG gated: every test skips unless TEST_POSTGRES_DSN is set and
// pingable (liveServicePool, same gate as Slice A).
//
// What each case proves, and the honest limit of each:
//
//  1. malformed      I1-malformed terminal. Real registry wired, but the
//     act is refused at the decode boundary, so no reader
//     is consulted — proven by an empty attempt log.
//  2. undeclared     I3-denied (ErrToolDenied) terminal. Refused at Gate A
//     before Execute, so no reader is consulted.
//  3. fabricated     I6-grounding (ErrGrounding) terminal with the rejected
//     report carried as Partial. No reader consulted.
//  4. cross-tenant   I4-request terminal. THE POINT is that the real reader
//     is never asked for a foreign tenant, so the foreign
//     tenant's claim row is provably untouched.
//  5. repetition     REPETITION (ErrRepetition) terminal AFTER one real
//     get_evidence read: the attempt log must carry the live
//     row's real evidence ID.
//  6. budget/tools   CALLS_EXHAUSTED (ErrBudgetExceeded) at the envelope's
//     MaxToolCalls, after two real reads.
//  7. budget/turns   TURNS_EXHAUSTED (ErrBudgetExceeded) after all 12
//     DefaultMaxTurns, each a real widening read.
//  8. budget/model   CALLS_EXHAUSTED (ErrBudgetExceeded) at
//     DefaultMaxModelCalls, via two I1 re-prompts.
//
// NOT qualified here: the DEADLINE terminal. It is not drivable at this
// service boundary, and forcing it would produce a timing-dependent test.
// The precise reason is documented at TestAgentService_CognitiveMatrixLive.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/investigate/orchestrate"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// matrixFixture is one isolated live-PG investigation: its own tenant,
// claim, seeded evidence, and identifiers. Every field is suffixed per
// test run so repeated local runs never collide.
type matrixFixture struct {
	pool    *pgxpool.Pool
	tenant  string
	claim   string
	docID   string
	invID   string
	exID    string
	reqID   string
	evPre   string   // evidence-ID prefix for this run
	anchor  string   // seeded evidence referenced by the envelope
	paged   []string // seeded evidence NOT in the envelope: only the reader surfaces these
	fabrico string   // never seeded, never known: the fabrication sentinel
}

// newMatrixFixture seeds one claim plus anchor+paged evidence rows.
func newMatrixFixture(t *testing.T, pool *pgxpool.Pool, pagedCount int) *matrixFixture {
	t.Helper()
	suffix := liveServiceRand(t, 6)
	fx := &matrixFixture{
		pool:    pool,
		tenant:  "tnt-apa38-" + suffix,
		claim:   "clm-apa38-" + suffix,
		docID:   "doc-apa38-" + suffix,
		invID:   "inv-" + liveServiceRand(t, 16),
		exID:    "ex-" + liveServiceRand(t, 16),
		reqID:   "req-apa38-" + suffix,
		evPre:   "ev-apa38-" + suffix + "-",
		anchor:  "ev-apa38-" + suffix + "-a-anchor",
		fabrico: "ev-apa38-" + suffix + "-z-fabricated",
	}
	for i := 1; i <= pagedCount; i++ {
		fx.paged = append(fx.paged, fmt.Sprintf("%sp%02d", fx.evPre, i))
	}
	seedLiveServiceClaim(t, pool, fx.tenant, fx.claim)
	seedMatrixEvidence(t, pool, fx)
	return fx
}

// seedMatrixEvidence inserts the anchor followed by the paged rows with a
// strictly increasing retrieved_at, so the reader's (retrieved_at, id)
// keyset order is exactly anchor, paged[0], paged[1], ...
//
// Each row carries its own source_id: the authoritative evidence table
// holds a UNIQUE (tenant_id, claim_id, source_type, source_id), so N
// distinct rows for one claim means N distinct source documents. Sharing
// one doc ID is not a legal way to stage a multi-row claim.
func seedMatrixEvidence(t *testing.T, pool *pgxpool.Pool, fx *matrixFixture) {
	t.Helper()
	ids := append([]string{fx.anchor}, fx.paged...)
	ctx := context.Background()
	tctx := postgres.WithTenant(ctx, claims.TenantID(fx.tenant))
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("begin evidence tx: %v", err)
	}
	defer func() { _ = tx.Rollback(tctx) }()
	base := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range ids {
		sourceID := fmt.Sprintf("%ssrc%02d", fx.docID+"-", i)
		if _, err := tx.Exec(tctx, `INSERT INTO evidence (id, tenant_id, claim_id, source_type, source_id, retrieved_at, content_hash, status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (id) DO NOTHING`,
			id, fx.tenant, fx.claim, string(invest.EvidenceSourceDocument), sourceID,
			base.Add(time.Duration(i)*time.Minute), "sha256-apa38-seed", "RETRIEVED"); err != nil {
			t.Fatalf("seed evidence %s: %v", id, err)
		}
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit evidence: %v", err)
	}
}

// seededIDs is the closed set of evidence IDs that legitimately exist in
// live PG for this fixture. Any attempt-log ID outside this set is a
// fabrication.
func (fx *matrixFixture) seededIDs() []string {
	out := append([]string{fx.anchor}, fx.paged...)
	return out
}

// saveMatrixEnvelope validates and persists the envelope the handler will
// load. allowTools and maxToolCalls come from the case under test; the
// deadline is the service-legal floor-plus so the run is never time-bound.
func (fx *matrixFixture) saveMatrixEnvelope(t *testing.T, allowTools []invest.ToolName, maxToolCalls int) invest.UnresolvedException {
	t.Helper()
	slices.Sort(allowTools)
	env := validEnvelopeForTenant(fx.tenant, fx.claim, fx.invID, fx.exID, fx.reqID)
	env.EvidenceRefs = []invest.EvidenceRef{{
		EvidenceID: fx.anchor,
		SourceType: invest.EvidenceSourceDocument,
		SourceID:   fx.docID,
		TenantID:   fx.tenant,
		ClaimID:    fx.claim,
		DocumentID: fx.docID,
		Page:       1,
	}}
	env.RuleFindings[0].EvidenceIDs = []string{fx.anchor}
	env.Scope.AllowTools = allowTools
	env.Scope.MaxToolCalls = maxToolCalls
	env.Scope.DeadlineMs = 30000
	if err := invest.Validate(env); err != nil {
		t.Fatalf("Validate envelope: %v", err)
	}
	if err := investigate.NewPGEnvelopeStore(fx.pool).SaveEnvelope(t.Context(), env); err != nil {
		t.Fatalf("SaveEnvelope: %v", err)
	}
	return env
}

// ---------------------------------------------------------------------------
// Scripted-model payload builders (the only scripted seam)
// ---------------------------------------------------------------------------

// matrixCallTool renders one CALL_TOOL act for the fixture's own scope.
func matrixCallTool(t *testing.T, fx *matrixFixture, tool invest.ToolName, limit int) []byte {
	t.Helper()
	req, err := investigate.NewRequest(tool, fx.tenant, fx.claim, fx.invID, fx.reqID, limit)
	if err != nil {
		t.Fatalf("NewRequest(%s, limit=%d): %v", tool, limit, err)
	}
	return matrixMarshalAct(t, orchestrate.ModelAction{
		Action:  orchestrate.ActionCallTool,
		Tool:    tool,
		Request: &req,
	})
}

// matrixMalformed returns act with one unknown top-level field added, so
// DecodeModelAction classifies it I1-malformed (re-promptable).
func matrixMalformed(t *testing.T, act []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(act, &m); err != nil {
		t.Fatalf("unmarshal act for malformed mutation: %v", err)
	}
	m["apa38_bogus_field"] = 1
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal malformed act: %v", err)
	}
	return raw
}

// matrixUndeclaredTool renders a CALL_TOOL act naming a tool outside the
// closed allowlist, carrying an otherwise-valid request.
func matrixUndeclaredTool(t *testing.T, fx *matrixFixture) []byte {
	t.Helper()
	req, err := investigate.NewRequest(invest.ToolGetClaim, fx.tenant, fx.claim, fx.invID, fx.reqID, 1)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return matrixMarshalAct(t, orchestrate.ModelAction{
		Action:  orchestrate.ActionCallTool,
		Tool:    invest.ToolName("apa38_evil_tool"),
		Request: &req,
	})
}

// matrixCrossTenantRequest renders a CALL_TOOL act whose request names a
// different tenant than the run scope. Well-formed, but outside scope.
func matrixCrossTenantRequest(t *testing.T, fx *matrixFixture) []byte {
	t.Helper()
	req, err := investigate.NewRequest(invest.ToolGetClaim, fx.tenant, fx.claim, fx.invID, fx.reqID, 1)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.TenantID = fx.tenant + "-other"
	return matrixMarshalAct(t, orchestrate.ModelAction{
		Action:  orchestrate.ActionCallTool,
		Tool:    invest.ToolGetClaim,
		Request: &req,
	})
}

// matrixSubmitReport renders a SUBMIT_REPORT act citing exactly the given
// evidence IDs, in the sorted unique order invest validation requires.
func matrixSubmitReport(t *testing.T, evIDs ...string) []byte {
	t.Helper()
	ids := append([]string(nil), evIDs...)
	slices.Sort(ids)
	report := orchestrate.Report{
		Hypotheses: []invest.Hypothesis{{
			ID:          "h-01",
			Statement:   "the conflict traces to transcription variance in the pinned record",
			Falsifier:   "a pinned policy record showing the claimed number active",
			Status:      invest.HypothesisOpen,
			EvidenceIDs: ids,
		}},
		Findings: []invest.Finding{{
			ID:           "f-01",
			HypothesisID: "h-01",
			Summary:      "the cited evidence shows the policy-number conflict",
			EvidenceIDs:  ids,
		}},
		Recommendation: invest.Recommendation{
			Action:     invest.RecommendReferHuman,
			Rationale:  "consequence-bearing; human decides",
			FindingIDs: []string{"f-01"},
		},
	}
	return matrixMarshalAct(t, orchestrate.ModelAction{
		Action: orchestrate.ActionSubmitReport,
		Report: &report,
	})
}

func matrixMarshalAct(t *testing.T, a orchestrate.ModelAction) []byte {
	t.Helper()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal act: %v", err)
	}
	return raw
}

// ---------------------------------------------------------------------------
// HTTP harness
// ---------------------------------------------------------------------------

// matrixPost drives POST /v1/investigations with the scripted model
// responses and returns the decoded body. Production-shaped: the envelope
// is loaded from the store, never inlined.
func matrixPost(t *testing.T, pool *pgxpool.Pool, fx *matrixFixture, script [][]byte) map[string]any {
	t.Helper()
	responses := make([]orchestrate.ModelResponse, 0, len(script))
	for _, payload := range script {
		responses = append(responses, orchestrate.ModelResponse{
			Payload: payload,
			ModelID: "apa38-matrix",
		})
	}
	body, err := json.Marshal(map[string]any{
		"tenant_id":        fx.tenant,
		"claim_id":         fx.claim,
		"investigation_id": fx.invID,
		"mock_script":      responses,
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	app := newInvestigationApp(t, pool)
	req := httptest.NewRequest(http.MethodPost, "/v1/investigations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", fx.tenant)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	m := decodeBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, m)
	}
	return m
}

// ---------------------------------------------------------------------------
// Shared assertions
// ---------------------------------------------------------------------------

// matrixAssertEscalated pins the terminal: ESCALATED, the exact exit
// class, the classified error text, and no accepted report.
func matrixAssertEscalated(t *testing.T, m map[string]any, wantReason orchestrate.EscalationReason, wantErrSubstring string) {
	t.Helper()
	if got := m["outcome"]; got != string(orchestrate.OutcomeEscalated) {
		t.Fatalf("outcome = %v, want ESCALATED (body %v)", got, m)
	}
	if got := m["escalation_reason"]; got != string(wantReason) {
		t.Fatalf("escalation_reason = %v, want %v (body %v)", got, wantReason, m)
	}
	msg, _ := m["error"].(string)
	if !strings.Contains(msg, wantErrSubstring) {
		t.Fatalf("error = %q, want substring %q (body %v)", msg, wantErrSubstring, m)
	}
	if _, ok := m["report"]; ok {
		t.Fatalf("escalation carried an accepted report: %v", m)
	}
}

// matrixAssertToolCallsUsed pins how much tool budget the run actually
// spent. Zero proves the reader was never consulted.
func matrixAssertToolCallsUsed(t *testing.T, m map[string]any, want int) {
	t.Helper()
	got, ok := m["tool_calls_used"].(float64)
	if !ok {
		t.Fatalf("tool_calls_used = %T, want number (body %v)", m["tool_calls_used"], m)
	}
	if int(got) != want {
		t.Fatalf("tool_calls_used = %d, want %d (body %v)", int(got), want, m)
	}
}

// matrixAttemptLog returns the decoded attempt_log entries.
func matrixAttemptLog(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["attempt_log"]
	if !ok {
		return nil
	}
	entries, ok := raw.([]any)
	if !ok {
		t.Fatalf("attempt_log = %T, want []any (body %v)", raw, m)
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("attempt_log entry = %T, want object (body %v)", e, m)
		}
		out = append(out, em)
	}
	return out
}

// matrixAssertAttemptLen pins the number of executed tool turns.
func matrixAssertAttemptLen(t *testing.T, m map[string]any, want int) {
	t.Helper()
	if got := len(matrixAttemptLog(t, m)); got != want {
		t.Fatalf("attempt_log has %d entries, want %d (body %v)", got, want, m)
	}
}

// matrixAttemptIDs flattens every response_ids entry in the attempt log:
// the set of evidence IDs a real reader actually returned.
func matrixAttemptIDs(t *testing.T, m map[string]any) []string {
	t.Helper()
	var out []string
	for _, e := range matrixAttemptLog(t, m) {
		ids, _ := e["response_ids"].([]any)
		for _, id := range ids {
			s, ok := id.(string)
			if !ok {
				t.Fatalf("response_ids entry = %T, want string", id)
			}
			out = append(out, s)
		}
	}
	return out
}

// matrixEntryIDs returns one attempt-log entry's response_ids as strings,
// preserving the reader's order.
func matrixEntryIDs(t *testing.T, entry map[string]any) []string {
	t.Helper()
	ids, _ := entry["response_ids"].([]any)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		s, ok := id.(string)
		if !ok {
			t.Fatalf("response_ids entry = %T, want string", id)
		}
		out = append(out, s)
	}
	return out
}

// matrixAssertGrounded asserts the no-fabrication property: every evidence
// ID the run surfaced came from a real seeded row, and the fabrication
// sentinel appears nowhere in the attempt log. This is the evidence-ids
// check for every case in the matrix.
func matrixAssertGrounded(t *testing.T, m map[string]any, seeded []string, forbidden string) {
	t.Helper()
	allowed := make(map[string]struct{}, len(seeded))
	for _, id := range seeded {
		allowed[id] = struct{}{}
	}
	for _, id := range matrixAttemptIDs(t, m) {
		if _, ok := allowed[id]; !ok {
			t.Fatalf("attempt log surfaced unseeded evidence id %q (seeded: %v)", id, seeded)
		}
		if id == forbidden {
			t.Fatalf("attempt log surfaced the fabrication sentinel %q", id)
		}
	}
	// A fabricated citation must never reach an accepted report. Reports
	// only exist on REPORT_READY, which no failure case reaches, so assert
	// the absence of any citation-bearing success surface instead.
	if rep, ok := m["report"]; ok {
		ids := matrixCitations(t, rep)
		for _, id := range ids {
			if _, ok := allowed[id]; !ok {
				t.Fatalf("report cites unseeded evidence id %q (seeded: %v)", id, seeded)
			}
		}
	}
}

// matrixCitations flattens every evidence_ids list in a report body.
func matrixCitations(t *testing.T, rep any) []string {
	t.Helper()
	rm, ok := rep.(map[string]any)
	if !ok {
		t.Fatalf("report = %T, want object", rep)
	}
	var out []string
	for _, key := range []string{"hypotheses", "findings"} {
		items, _ := rm[key].([]any)
		for _, it := range items {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			ids, _ := im["evidence_ids"].([]any)
			for _, id := range ids {
				if s, ok := id.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// matrixAssertClaimUnchanged proves the agent wrote zero claim mutations.
func matrixAssertClaimUnchanged(t *testing.T, pool *pgxpool.Pool, tenant, claim, before string) {
	t.Helper()
	if after := snapshotLiveServiceClaim(t, pool, tenant, claim); after != before {
		t.Fatalf("claim row mutated by agent:\nbefore=%s\nafter =%s", before, after)
	}
}

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

// TestAgentService_CognitiveFailureMatrixLive re-drives the seven
// loop-level cognitive guardrails through the production Agent service
// with real PGReaders and a real Loop. Only the ModelClient is scripted.
//
// DEADLINE is deliberately absent. It cannot be driven at this boundary
// without a timing-dependent test:
//
//   - investigate.Scope.Validate rejects any scope deadline below 100ms,
//     and the handler builds the loop scope from the stored envelope, so
//     100ms is the hard floor for the run deadline.
//   - The loop fires DEADLINE only when wall-clock elapsed time crosses
//     that deadline (checked at each turn top and after each Complete).
//   - The only ModelClient reachable at this boundary is
//     orchestrate.NewMockModelClient, which serves scripted bytes with no
//     delay seam (unlike the loop-level FakeModelClient, which the
//     loop-level deadline test relies on). It returns no error, so the
//     loop's 10ms retry backoff is unreachable too, and script exhaustion
//     is terminal (contains "exhausted"), not retried.
//   - The only real work inside the run is a localhost PG read plus audit
//     writes, ~1-3ms per turn, and the run is capped at 12 turns
//     (DefaultMaxTurns). That cannot reliably cross 100ms.
//
// Reaching the deadline would require either a production-code delay seam
// (out of scope: no architecture change) or seeding enough rows to burn
// >100ms of reader CPU, which is machine-speed dependent — i.e. a flaky
// assertion. The DEADLINE terminal stays qualified at the loop level
// (orchestrate_test.go TestDeadlineEscalates), where the delay seam
// exists. This is a harness limitation, reported rather than forced.
func TestAgentService_CognitiveFailureMatrixLive(t *testing.T) {
	pool := liveServicePool(t)
	// Production-shaped: inline envelopes are refused, so the handler must
	// load the authoritative envelope from the store.
	t.Setenv("APP_ENV", "test")
	t.Setenv("ALLOW_INLINE_ENVELOPE", "false")

	// Case 1 — malformed model output. I1-malformed earns one same-turn
	// re-prompt; a second consecutive malformed act escalates. The terminal
	// is INVALID_OUTPUT with the I1 class. No tool runs, so no reader runs.
	t.Run("malformed_is_invalid_output", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 0)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetEvidence}, 5)
		before := snapshotLiveServiceClaim(t, pool, fx.tenant, fx.claim)

		bad := matrixMalformed(t, matrixCallTool(t, fx, invest.ToolGetEvidence, 1))
		m := matrixPost(t, pool, fx, [][]byte{bad, bad})

		matrixAssertEscalated(t, m, orchestrate.EscalationInvalidOutput, "I1-malformed")
		matrixAssertToolCallsUsed(t, m, 0)
		matrixAssertAttemptLen(t, m, 0)
		matrixAssertGrounded(t, m, fx.seededIDs(), fx.fabrico)
		matrixAssertClaimUnchanged(t, pool, fx.tenant, fx.claim, before)
	})

	// Case 2 — undeclared capability. A tool name outside the closed
	// allowlist is I3-denied, which is ErrToolDenied. Refused at Gate A
	// before Execute, so the real registry is never reached.
	t.Run("undeclared_capability_is_tool_denied", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 0)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetClaim, invest.ToolGetEvidence}, 5)
		before := snapshotLiveServiceClaim(t, pool, fx.tenant, fx.claim)

		m := matrixPost(t, pool, fx, [][]byte{matrixUndeclaredTool(t, fx)})

		matrixAssertEscalated(t, m, orchestrate.EscalationInvalidOutput, "I3-denied")
		if msg, _ := m["error"].(string); !strings.Contains(msg, "not in allowlist") {
			t.Fatalf("error = %q, want the allowlist cause", msg)
		}
		matrixAssertToolCallsUsed(t, m, 0)
		matrixAssertAttemptLen(t, m, 0)
		matrixAssertGrounded(t, m, fx.seededIDs(), fx.fabrico)
		matrixAssertClaimUnchanged(t, pool, fx.tenant, fx.claim, before)
	})

	// Case 3 — fabricated evidence. The report is structurally valid and
	// cites one seeded ID plus one invented ID. Gate B rejects it with
	// I6-grounding (ErrGrounding) and the rejected report comes back as
	// Partial, never as an accepted report.
	t.Run("fabricated_citation_is_grounding_rejection", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 0)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetClaim, invest.ToolGetEvidence}, 5)
		before := snapshotLiveServiceClaim(t, pool, fx.tenant, fx.claim)

		m := matrixPost(t, pool, fx, [][]byte{
			matrixSubmitReport(t, fx.anchor, fx.fabrico),
		})

		matrixAssertEscalated(t, m, orchestrate.EscalationInvalidOutput, "I6-grounding")
		msg, _ := m["error"].(string)
		if !strings.Contains(msg, "cites unknown evidence id") {
			t.Fatalf("error = %q, want the unknown-citation cause", msg)
		}
		if !strings.Contains(msg, fx.fabrico) {
			t.Fatalf("error = %q, want the rejected id %q named", msg, fx.fabrico)
		}
		partial, ok := m["partial"]
		if !ok || partial == nil {
			t.Fatalf("grounding rejection must carry the rejected report as partial: %v", m)
		}
		if !slices.Contains(matrixCitations(t, partial), fx.fabrico) {
			t.Fatalf("partial does not show the rejected citation: %v", partial)
		}
		matrixAssertToolCallsUsed(t, m, 0)
		matrixAssertAttemptLen(t, m, 0)
		matrixAssertGrounded(t, m, fx.seededIDs(), fx.fabrico)
		matrixAssertClaimUnchanged(t, pool, fx.tenant, fx.claim, before)
	})

	// Case 4 — cross-tenant request. The act names a foreign tenant. I4
	// is not re-promptable, so it escalates immediately. The security
	// property is the absence of a read: the real PGReaders is never asked
	// for another tenant's data, proven by the empty attempt log and by
	// the foreign tenant's claim row being byte-identical afterwards.
	t.Run("cross_tenant_request_never_reaches_reader", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 0)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetClaim, invest.ToolGetEvidence}, 5)
		before := snapshotLiveServiceClaim(t, pool, fx.tenant, fx.claim)

		foreign := "tnt-apa38-foreign-" + liveServiceRand(t, 4)
		seedLiveServiceClaim(t, pool, foreign, fx.claim+"-foreign")
		foreignBefore := snapshotLiveServiceClaim(t, pool, foreign, fx.claim+"-foreign")

		m := matrixPost(t, pool, fx, [][]byte{matrixCrossTenantRequest(t, fx)})

		matrixAssertEscalated(t, m, orchestrate.EscalationInvalidOutput, "I4-request")
		if msg, _ := m["error"].(string); !strings.Contains(msg, "must echo scope") {
			t.Fatalf("error = %q, want the identity-echo cause", msg)
		}
		matrixAssertToolCallsUsed(t, m, 0)
		matrixAssertAttemptLen(t, m, 0)
		matrixAssertGrounded(t, m, fx.seededIDs(), fx.fabrico)
		matrixAssertClaimUnchanged(t, pool, fx.tenant, fx.claim, before)
		matrixAssertClaimUnchanged(t, pool, foreign, fx.claim+"-foreign", foreignBefore)
	})

	// Case 5 — exact repetition. The FIRST call is a real get_evidence
	// read against live PG; the second is byte-identical, so the
	// repetition key hits before Execute. The terminal is REPETITION
	// (ErrRepetition) and the attempt log must carry exactly one turn
	// with the live row's real evidence ID.
	t.Run("exact_repeat_is_repetition", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 3)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetEvidence}, 5)
		before := snapshotLiveServiceClaim(t, pool, fx.tenant, fx.claim)

		call := matrixCallTool(t, fx, invest.ToolGetEvidence, 1)
		m := matrixPost(t, pool, fx, [][]byte{call, call})

		matrixAssertEscalated(t, m, orchestrate.EscalationRepetition, "repeated tool call")
		if msg, _ := m["error"].(string); !strings.Contains(msg, "exact repeat of get_evidence call") {
			t.Fatalf("error = %q, want the exact-repeat cause", msg)
		}
		matrixAssertToolCallsUsed(t, m, 1)
		matrixAssertAttemptLen(t, m, 1)
		log := matrixAttemptLog(t, m)
		if log[0]["tool"] != string(invest.ToolGetEvidence) {
			t.Fatalf("attempt_log tool = %v, want %q", log[0]["tool"], invest.ToolGetEvidence)
		}
		if code, _ := log[0]["error_code"].(string); code != "" {
			t.Fatalf("attempt_log error_code = %q, want empty (the real read succeeded)", code)
		}
		// The single executed turn must carry the real reader's row.
		ids := matrixAttemptIDs(t, m)
		if len(ids) != 1 || ids[0] != fx.anchor {
			t.Fatalf("attempt response_ids = %v, want exactly the live anchor row %q", ids, fx.anchor)
		}
		matrixAssertGrounded(t, m, fx.seededIDs(), fx.fabrico)
		matrixAssertClaimUnchanged(t, pool, fx.tenant, fx.claim, before)
	})

	// Case 6 — tool-call budget. The envelope grants MaxToolCalls=2, so
	// the loop-side tool budget follows it. Two real reads run; the third
	// act is refused before Execute. Terminal: CALLS_EXHAUSTED
	// (ErrBudgetExceeded).
	t.Run("tool_call_budget_is_calls_exhausted", func(t *testing.T) {
		fx := newMatrixFixture(t, pool, 3)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetEvidence}, 2)
		before := snapshotLiveServiceClaim(t, pool, fx.tenant, fx.claim)

		m := matrixPost(t, pool, fx, [][]byte{
			matrixCallTool(t, fx, invest.ToolGetEvidence, 1),
			matrixCallTool(t, fx, invest.ToolGetEvidence, 2),
			matrixCallTool(t, fx, invest.ToolGetEvidence, 3), // refused by budget
		})

		matrixAssertEscalated(t, m, orchestrate.EscalationCallsExhausted, "budget exhausted")
		matrixAssertToolCallsUsed(t, m, 2)
		matrixAssertAttemptLen(t, m, 2)
		// Both executed turns came from the real reader, and the second
		// widened KnownEvidence by the first paged row.
		ids := matrixAttemptIDs(t, m)
		if !slices.Contains(ids, fx.anchor) || !slices.Contains(ids, fx.paged[0]) {
			t.Fatalf("attempt response_ids = %v, want the live anchor and first paged row %v", ids, fx.paged[:1])
		}
		matrixAssertGrounded(t, m, fx.seededIDs(), fx.fabrico)
		matrixAssertClaimUnchanged(t, pool, fx.tenant, fx.claim, before)
	})

	// Case 7 — turn budget. DefaultMaxTurns is 12 at the service
	// boundary, so twelve distinct real get_evidence reads (limit 1..12,
	// each widening KnownEvidence by exactly one paged row) run before
	// the loop exits. Terminal: TURNS_EXHAUSTED (ErrBudgetExceeded).
	// This is the deepest real-reader proof in the matrix: every surfaced
	// ID is a live row, and the seeded row past the twelfth is absent
	// because it was never read — the log reflects reads, not seeds.
	t.Run("turn_budget_is_turns_exhausted", func(t *testing.T) {
		const turns = orchestrate.DefaultMaxTurns
		fx := newMatrixFixture(t, pool, turns)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetEvidence}, turns)
		before := snapshotLiveServiceClaim(t, pool, fx.tenant, fx.claim)

		script := make([][]byte, 0, turns)
		for i := 1; i <= turns; i++ {
			script = append(script, matrixCallTool(t, fx, invest.ToolGetEvidence, i))
		}
		m := matrixPost(t, pool, fx, script)

		matrixAssertEscalated(t, m, orchestrate.EscalationTurnsExhausted, "budget exhausted")
		if got, _ := m["turns_used"].(float64); int(got) != turns {
			t.Fatalf("turns_used = %v, want %d (body %v)", m["turns_used"], turns, m)
		}
		matrixAssertToolCallsUsed(t, m, turns)
		matrixAssertAttemptLen(t, m, turns)
		// Each turn reads with limit=i, so the real reader returns the
		// first i rows: turn i must log exactly the anchor plus the first
		// (i-1) paged rows, in the reader's sorted order, and each turn's
		// set must be a strict widening of the previous one. The seeded
		// row past the twelfth was never read, so it must not appear —
		// the log records reads, not seeds.
		log := matrixAttemptLog(t, m)
		for i, entry := range log {
			got := matrixEntryIDs(t, entry)
			want := append([]string{fx.anchor}, fx.paged[:i]...)
			if !slices.Equal(got, want) {
				t.Fatalf("attempt_log[%d] response_ids = %v, want the first %d live rows %v", i+1, got, i+1, want)
			}
		}
		ids := matrixAttemptIDs(t, m)
		if slices.Contains(ids, fx.paged[turns-1]) {
			t.Fatalf("attempt response_ids surfaced the never-read row %q: %v", fx.paged[turns-1], ids)
		}
		union := make(map[string]struct{}, turns)
		for _, id := range ids {
			union[id] = struct{}{}
		}
		if len(union) != turns {
			t.Fatalf("attempt log covers %d distinct rows, want %d: %v", len(union), turns, ids)
		}
		matrixAssertGrounded(t, m, fx.seededIDs(), fx.fabrico)
		matrixAssertClaimUnchanged(t, pool, fx.tenant, fx.claim, before)
	})

	// Case 8 — model-call budget. DefaultMaxModelCalls is 13, one above
	// DefaultMaxTurns. Two I1 re-prompts push the call count ahead of the
	// turn count, so the call budget is spent inside turn 12 rather than
	// the turn ceiling being reached first. Terminal: CALLS_EXHAUSTED
	// (ErrBudgetExceeded) after eleven real reads.
	t.Run("model_call_budget_is_calls_exhausted", func(t *testing.T) {
		const (
			turns       = orchestrate.DefaultMaxTurns
			maxModelCal = orchestrate.DefaultMaxModelCalls
		)
		fx := newMatrixFixture(t, pool, turns)
		fx.saveMatrixEnvelope(t, []invest.ToolName{invest.ToolGetEvidence}, turns)
		before := snapshotLiveServiceClaim(t, pool, fx.tenant, fx.claim)

		// Turns 1 and 2 each burn an extra call on an I1 re-prompt, so
		// 11 executed turns + 2 re-prompts = 13 calls. Turn 12 finds no
		// call budget left.
		script := [][]byte{
			matrixMalformed(t, matrixCallTool(t, fx, invest.ToolGetEvidence, 1)),
			matrixCallTool(t, fx, invest.ToolGetEvidence, 1),
			matrixMalformed(t, matrixCallTool(t, fx, invest.ToolGetEvidence, 2)),
			matrixCallTool(t, fx, invest.ToolGetEvidence, 2),
		}
		for i := 3; i <= 11; i++ {
			script = append(script, matrixCallTool(t, fx, invest.ToolGetEvidence, i))
		}
		if len(script) != maxModelCal {
			t.Fatalf("script has %d entries, want %d (the model-call budget)", len(script), maxModelCal)
		}
		m := matrixPost(t, pool, fx, script)

		matrixAssertEscalated(t, m, orchestrate.EscalationCallsExhausted, "budget exhausted")
		// The discriminator between "model calls spent" and "turns spent":
		// the run reached turn 12 and was refused there, so turns_used is
		// 12 while the attempt log holds only the 11 turns that actually
		// executed a read. The turn-ceiling case logs all 12.
		if got, _ := m["turns_used"].(float64); int(got) != turns {
			t.Fatalf("turns_used = %v, want %d: the call budget is spent on entry to turn %d (body %v)", m["turns_used"], turns, turns, m)
		}
		matrixAssertToolCallsUsed(t, m, 11)
		matrixAssertAttemptLen(t, m, 11)
		// Turn i read with limit=i, so the log widens one real row per turn.
		log := matrixAttemptLog(t, m)
		for i, entry := range log {
			got := matrixEntryIDs(t, entry)
			want := append([]string{fx.anchor}, fx.paged[:i]...)
			if !slices.Equal(got, want) {
				t.Fatalf("attempt_log[%d] response_ids = %v, want the first %d live rows %v", i+1, got, i+1, want)
			}
		}
		// Eleven executed turns read with limit 1..11, so the widest read
		// is the anchor plus the first ten paged rows.
		union := make(map[string]struct{}, 11)
		for _, id := range matrixAttemptIDs(t, m) {
			union[id] = struct{}{}
		}
		if len(union) != 11 {
			t.Fatalf("attempt log covers %d distinct rows, want 11", len(union))
		}
		if _, ok := union[fx.paged[9]]; !ok {
			t.Fatalf("attempt log missing the tenth paged live row %q", fx.paged[9])
		}
		matrixAssertGrounded(t, m, fx.seededIDs(), fx.fabrico)
		matrixAssertClaimUnchanged(t, pool, fx.tenant, fx.claim, before)
	})
}
