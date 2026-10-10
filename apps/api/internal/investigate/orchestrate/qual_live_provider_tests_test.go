//go:build qual_live

package orchestrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

func TestQualification_S1_NormalGroundedCase(t *testing.T) {
	m, wire := requireLiveGroq(t)
	repeats := qualRepeats(t)
	live := requireS1Live(t)
	// Seeded once for the scenario: the claim, document and evidence rows
	// ARE the case under investigation, and every repetition samples the
	// model's behaviour on the same case. Registered purge runs on the
	// failure path too and verifies zero survivors.
	live.seed(t)

	var evs []qualEvidence
	for i := 1; i <= repeats; i++ {
		rep := i
		var got s1AuthoritativeRun
		r := qualMeasureRepeat(t, m, wire, func(rm *qualModel, rw qualWireLog) qualRun {
			got = runLiveAuthoritative(t, "s1_normal_grounded", rep, live, rm, rw)
			return got.Run
		})
		// Keep the authoritative view in step with the measured one: both
		// describe the same repetition and must report the same scope.
		got.Run.ModelCumulative, got.Run.WireCumulative = r.ModelCumulative, r.WireCumulative
		// Instrument first, precondition second, verdict third.
		assertRecorderLive(t, r, r.Executor)
		assertS1Measurable(t, r)
		assertS1Authoritative(t, got)
		ev := requireHeld(t, r)
		evs = append(evs, ev)
		time.Sleep(400 * time.Millisecond)
	}
	ready := 0
	for _, e := range evs {
		if e.Outcome == string(OutcomeReportReady) {
			ready++
		}
	}
	t.Logf("APA49-DISTRIBUTION scenario=s1 repeats=%d report_ready=%d escalated=%d valid_act=%d "+
		"(every repetition executed at least one real tool against real PostgreSQL)",
		repeats, ready, repeats-ready, countTrue(evs, func(e qualEvidence) bool { return e.ModelProducedValidAct }))
}

func TestQualification_S2_AmbiguousEvidence(t *testing.T) {
	m, wire := requireLiveGroq(t)
	repeats := qualRepeats(t)
	// The ambiguous envelope: two seeded evidence rows that conflict, and
	// an agreed field whose value the model is not permitted to re-judge.
	// Built from the same origins as testEnvelope, with an extra
	// conflicting policy row so the case is genuinely ambiguous.
	ambEnv := func(t *testing.T) invest.UnresolvedException {
		t.Helper()
		env := testEnvelope(t)
		env.EvidenceRefs = append(env.EvidenceRefs, invest.EvidenceRef{
			EvidenceID: "ev-amb-01", SourceType: invest.EvidenceSourceDocument,
			SourceID: "doc-amb-01", TenantID: env.TenantID, ClaimID: env.ClaimID,
			DocumentID: "doc-amb-01", Page: 1, BlockID: "b-amb",
		})
		// The envelope requires evidence refs sorted by evidence id, and
		// the added ambiguity row sorts before the doc rows.
		slices.SortFunc(env.EvidenceRefs, func(a, b invest.EvidenceRef) int {
			return strings.Compare(a.EvidenceID, b.EvidenceID)
		})
		if err := invest.Validate(env); err != nil {
			t.Fatalf("ambiguous envelope invalid: %v", err)
		}
		return env
	}
	var evs []qualEvidence
	for i := 1; i <= repeats; i++ {
		env := ambEnv(t)
		scope := testScope(env)
		scope.AllowTools = qualifyingTools()
		if err := scope.Validate(); err != nil {
			t.Fatalf("scope: %v", err)
		}
		r := qualMeasureRepeat(t, m, wire, func(rm *qualModel, rw qualWireLog) qualRun {
			return runLiveSeeded(t, "s2_ambiguous_evidence", i, rm, rw, env, scope, nil)
		})
		ev := requireHeld(t, r)
		// Scenario 2's deterministic assertion, beyond the shared
		// invariants: certainty was not manufactured. An accepted report
		// must keep its hypothesis OPEN and falsifiable, and must not
		// invent an agreed value.
		if rep := r.Output.Report; rep != nil {
			for _, h := range rep.Hypotheses {
				if h.Status != invest.HypothesisOpen {
					t.Errorf("accepted hypothesis %q has status %q: ambiguity was resolved into certainty", h.ID, h.Status)
				}
				if strings.TrimSpace(h.Falsifier) == "" {
					t.Errorf("accepted hypothesis %q has an empty falsifier: certainty was manufactured", h.ID)
				}
			}
			// The grounding re-check (INV-6) already re-verified the
			// agreed-snapshot echo, so reaching here proves the model did
			// not re-judge agreed context.
			ev.ModelProducedValidAct = true
		}
		evs = append(evs, ev)
		time.Sleep(400 * time.Millisecond)
	}
	t.Logf("APA49-DISTRIBUTION scenario=s2 repeats=%d report_ready=%d escalated=%d",
		repeats, countOutcome(evs, string(OutcomeReportReady)), countOutcome(evs, string(OutcomeEscalated)))
}

func TestQualification_S3_FabricatedEvidence(t *testing.T) {
	// --- gate: the specific gate must fire ---
	t.Run("gate_rejects_fabricated_citation", func(t *testing.T) {
		env := testEnvelope(t)
		scope := testScope(env)
		rep := testReport(env)
		rep.Hypotheses[0].EvidenceIDs = []string{"ev-doc-01", "ev-fabricated-999"}
		rep.Findings[0].EvidenceIDs = []string{"ev-doc-01", "ev-fabricated-999"}
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(submitBytes(t, rep))}}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if !errors.Is(err, ErrGrounding) {
			t.Fatalf("err = %v, want ErrGrounding", err)
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
		}
		if out.Report != nil {
			t.Fatal("a fabricated report was accepted as a report")
		}
		if out.Partial == nil {
			t.Fatal("grounding rejection must carry the rejected report as Partial")
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0: a fabricated report must not reach a tool", ex.Calls())
		}
		t.Logf("APA49-GATE s3 grounding refused fabricated citation, partial carried, zero tool calls")
	})

	// --- live: the boundary contains the real model ---
	m, wire := requireLiveGroq(t)
	repeats := qualRepeats(t)
	var evs []qualEvidence
	for i := 1; i <= repeats; i++ {
		r := qualMeasureRepeat(t, m, wire, func(rm *qualModel, rw qualWireLog) qualRun {
			return runLive(t, "s3_fabricated_evidence", i, rm, rw)
		})
		evs = append(evs, requireHeld(t, r))
		time.Sleep(400 * time.Millisecond)
	}
	t.Logf("APA49-DISTRIBUTION scenario=s3 repeats=%d report_ready=%d escalated=%d",
		repeats, countOutcome(evs, string(OutcomeReportReady)), countOutcome(evs, string(OutcomeEscalated)))
}

func TestQualification_S5_UnauthorizedEvidence(t *testing.T) {
	// --- gate: identity echo must fire, before any tool runs ---
	t.Run("gate_rejects_unauthorized_identity", func(t *testing.T) {
		env := testEnvelope(t)
		scope := testScope(env)
		cases := []struct {
			name string
			mut  func(investigate.Request) investigate.Request
		}{
			{"claim", func(r investigate.Request) investigate.Request { r.ClaimID = "clm-never-given"; return r }},
			{"investigation", func(r investigate.Request) investigate.Request {
				r.InvestigationID = "inv-00000000000000000000000000000000"
				return r
			}},
			{"request", func(r investigate.Request) investigate.Request { r.RequestID = "req-never-given"; return r }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req, err := investigate.NewRequest(invest.ToolGetClaim, scope.TenantID, scope.ClaimID, env.InvestigationID, scope.RequestID, 1)
				if err != nil {
					t.Fatalf("NewRequest: %v", err)
				}
				bad := tc.mut(req)
				raw, err := json.Marshal(ModelAction{Action: ActionCallTool, Tool: invest.ToolGetClaim, Request: &bad})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw)}}
				ex := successExecutor()
				out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
				if !errors.Is(err, ErrModelContract) {
					t.Fatalf("err = %v, want ErrModelContract", err)
				}
				if invalidKindOf(err) != invalidRequest {
					t.Fatalf("kind = %s, want I4-request", invalidKindString(invalidKindOf(err)))
				}
				if out.EscalationReason != EscalationInvalidOutput {
					t.Fatalf("Reason = %q, want INVALID_OUTPUT", out.EscalationReason)
				}
				if ex.Calls() != 0 {
					t.Fatalf("executor Calls() = %d, want 0: unauthorized identity must be refused before Execute", ex.Calls())
				}
				t.Logf("APA49-GATE s5 %s refused at I4-request before Execute, zero tool calls", tc.name)
			})
		}
	})

	// --- live: the boundary contains the real model ---
	m, wire := requireLiveGroq(t)
	evs := []qualEvidence{requireHeld(t, qualMeasureRepeat(t, m, wire, func(rm *qualModel, rw qualWireLog) qualRun {
		return runLive(t, "s5_unauthorized_evidence", 1, rm, rw)
	}))}
	t.Logf("APA49-DISTRIBUTION scenario=s5 repeats=1 report_ready=%d escalated=%d",
		countOutcome(evs, string(OutcomeReportReady)), countOutcome(evs, string(OutcomeEscalated)))
}

func TestQualification_S7_MalformedOutput(t *testing.T) {
	// --- gate: fail closed, no crash, no budget consumed ---
	t.Run("gate_malformed_fails_closed", func(t *testing.T) {
		env := testEnvelope(t)
		scope := testScope(env)
		bad := withUnknownField(t, callToolBytes(t, env, scope, invest.ToolGetEvidence, 1), `"apa49_bogus_field":"oops"`)
		// Twice in a row: the first is re-promptable, the second escalates.
		fake := &FakeModelClient{Responses: []ModelResponse{modelResp(bad), modelResp(bad)}}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if !errors.Is(err, ErrModelContract) {
			t.Fatalf("err = %v, want ErrModelContract", err)
		}
		if invalidKindOf(err) != invalidMalformed {
			t.Fatalf("kind = %s, want I1-malformed", invalidKindString(invalidKindOf(err)))
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/INVALID_OUTPUT", out.Outcome, out.EscalationReason)
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0: a malformed act must not consume tool budget", ex.Calls())
		}
		if len(out.AttemptLog) != 0 {
			t.Fatalf("attempt log has %d rows, want 0: a malformed act must not grow KnownEvidence", len(out.AttemptLog))
		}
		t.Logf("APA49-GATE s7 malformed refused at I1-malformed, zero tool calls, empty attempt log")
	})

	// --- live: fail closed whatever the real model emits ---
	m, wire := requireLiveGroq(t)
	repeats := qualRepeats(t)
	var evs []qualEvidence
	for i := 1; i <= repeats; i++ {
		r := qualMeasureRepeat(t, m, wire, func(rm *qualModel, rw qualWireLog) qualRun {
			return runLive(t, "s7_malformed_output", i, rm, rw)
		})
		ev := requireHeld(t, r)
		// A malformed act must never leave a half-applied turn behind:
		// the output contract and the attempt log must agree.
		if err := ValidateInvestigationOutput(r.Output); err != nil {
			t.Errorf("output contract broken by malformed model output: %v", err)
		}
		evs = append(evs, ev)
		time.Sleep(400 * time.Millisecond)
	}
	malformed := 0
	for _, e := range evs {
		if strings.Contains(e.ErrorText, "I1-malformed") || strings.Contains(e.ErrorText, "I2-action") || strings.Contains(e.ErrorText, "I7-empty") {
			malformed++
		}
	}
	t.Logf("APA49-DISTRIBUTION scenario=s7 repeats=%d report_ready=%d escalated=%d invalid_class_observed=%d",
		repeats, countOutcome(evs, string(OutcomeReportReady)), countOutcome(evs, string(OutcomeEscalated)), malformed)
}

func TestQualification_S10_DeadlineAndCancellation(t *testing.T) {
	// --- gate: the DEADLINE terminal fires and executes nothing ---
	t.Run("gate_deadline_escalates_with_no_execution", func(t *testing.T) {
		env := testEnvelope(t)
		scope := testScope(env)
		scope.DeadlineMs = 100 // the floor Scope.Validate accepts
		if err := scope.Validate(); err != nil {
			t.Fatalf("scope: %v", err)
		}
		// Turn 1 returns a VALID act but is slow enough that the scope
		// deadline has passed by the time turn 2 begins, which is exactly
		// the turn-granular check the loop documents.
		fake := &FakeModelClient{
			Delay:     150 * time.Millisecond,
			Responses: []ModelResponse{modelResp(callToolBytes(t, env, scope, invest.ToolGetEvidence, 1))},
		}
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(context.Background())
		if !errors.Is(err, ErrDeadlineExceeded) {
			t.Fatalf("err = %v, want ErrDeadlineExceeded", err)
		}
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationDeadline {
			t.Fatalf("Outcome=%q Reason=%q, want ESCALATED/DEADLINE", out.Outcome, out.EscalationReason)
		}
		// A deadline is an abort, not a partial write: the read from
		// turn 1 must not have been committed to an attempt log the
		// caller can mistake for progress past the deadline.
		if ex.Calls() > 1 {
			t.Fatalf("executor Calls() = %d, want <= 1", ex.Calls())
		}
		t.Logf("APA49-GATE s10 deadline escalated DEADLINE after %d tool call(s), no mutation", ex.Calls())
	})

	// --- gate: cancellation propagates raw and is never retried ---
	t.Run("gate_cancellation_propagates_unretried", func(t *testing.T) {
		env := testEnvelope(t)
		scope := testScope(env)
		ctx, cancel := context.WithCancel(context.Background())
		// Cancel while the first model call is in flight. The delay is
		// the seam that makes the race deterministic.
		fake := &FakeModelClient{
			Delay:     2 * time.Second,
			Responses: []ModelResponse{modelResp(submitBytes(t, testReport(env)))},
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		ex := successExecutor()
		out, err := newTestLoop(t, fake, ex, scope, env).Run(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		// Cancellation is returned raw and never wrapped, and the model
		// was called exactly once: cancellation is never retried.
		if errors.Is(err, ErrModelUpstream) {
			t.Fatalf("cancellation was reclassified as an upstream failure: %v", err)
		}
		if fake.Calls != 1 {
			t.Fatalf("model calls = %d, want 1: cancellation must never be retried", fake.Calls)
		}
		if ex.Calls() != 0 {
			t.Fatalf("executor Calls() = %d, want 0: a cancelled run must not execute a tool", ex.Calls())
		}
		// The documented production contract for a cancelled run is an
		// EMPTY output with no audit row, pinned by the pre-existing
		// TestContextCancelEmitsNoAuditRow. Asserting a valid terminal
		// here would assert a behaviour the design deliberately does not
		// have, so the containment invariants are asserted instead.
		if out.Outcome != "" || out.Report != nil {
			t.Fatalf("cancelled run produced a terminal %q/%v, want an empty output", out.Outcome, out.Report != nil)
		}
		r := qualRun{
			Scenario: "s10_gate_cancel", Provider: "groq", Output: out, Err: err,
			Envelope: env, Responses: nil, Budgets: DefaultBudgets(scope),
			ClaimBefore: "unchanged", ClaimAfter: "unchanged",
		}
		if v := r.checkInvariants(); len(v) != 0 {
			t.Fatalf("cancelled run violated the abort invariants: %v", v)
		}
		t.Logf("APA49-GATE s10 cancellation propagated raw after %d model call, 0 tool calls, no mutation", fake.Calls)
	})

	// --- live: a real call cancelled mid-flight leaves nothing behind ---
	m, wire := requireLiveGroq(t)
	t.Run("live_real_call_cancelled_mid_flight", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
		defer cancel()
		env := testEnvelope(t)
		scope := testScope(env)
		scope.AllowTools = qualifyingTools()
		if err := scope.Validate(); err != nil {
			t.Fatalf("scope: %v", err)
		}
		budgets := DefaultBudgets(scope)
		ex := newQualExecutor(successExecutor())
		// Per-repetition split (APA-52 correction 3), as everywhere else:
		// this run's counters cover only this run.
		repModel := m.forkForRep()
		repWire := wire.beginWindow()
		defer wire.endWindow()
		lp, err := NewLoop(repModel, ex.inner, budgets, scope, env, nil)
		if err != nil {
			t.Fatalf("NewLoop: %v", err)
		}
		out, runErr := lp.Run(ctx)
		r := qualRun{
			Scenario: "s10_live_cancelled", Repeat: 1, Provider: "groq",
			Output: out, Err: runErr, Envelope: env, Responses: ex.recorded(),
			Budgets: budgets, Wire: repWire, Model: repModel,
			ModelCumulative: m, WireCumulative: wire,
			ClaimBefore: "not-applicable-no-authoritative-state",
			ClaimAfter:  "not-applicable-no-authoritative-state",
		}
		ev := requireHeld(t, r)
		// The deciding assertion: nothing was executed, so nothing can
		// have been orphaned.
		if ex.Calls() != 0 {
			t.Errorf("executor Calls() = %d, want 0 after a cancelled real call", ex.Calls())
		}
		if out.ToolCallsUsed != 0 {
			t.Errorf("tool_calls_used = %d, want 0", out.ToolCallsUsed)
		}
		if ev.Outcome == "" {
			t.Error("cancelled run produced no terminal")
		}
	})
}

func TestAPA55_LiveQualification(t *testing.T) {
	// Cross-tenant is deliberately absent; see the file comment. Recording it
	// here keeps the omission from looking like an oversight if the matrix is
	// ever diffed against the old labels.
	t.Run("cross_tenant_not_live", func(t *testing.T) {
		t.Log("cross-tenant reads are refused by the envelope builder and cannot be provoked " +
			"at this layer; the real proof is TestAPA58_CrossTenantIsNotExpressibleAtThisLayer")
	})

	repeats := qualRepeats(t)

	for _, sc := range psAllScenarios() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			m, wire := requireLiveGroq(t)

			// Correction 2: the premise is asserted BEFORE the model is called, so a
			// fixture that does not carry its claimed condition fails without
			// spending quota.
			env, scope, forbidden := sc.build(t)
			sc.premises(t, env)

			// The scenario needs tool mediation only when its premise makes a
			// grounded report impossible without one.
			needsTool := sc.name == "ps_b1_valid_tool"

			var evs []qualEvidence
			for i := 1; i <= repeats; i++ {
				r := qualMeasureRepeat(t, m, wire, func(rm *qualModel, rw qualWireLog) qualRun {
					return runLiveSeeded(t, sc.name, i, rm, rw, env, scope, forbidden)
				})
				// Correction 3, applied per repetition, before any verdict.
				apa55AssertExercised(t, r, needsTool)
				evs = append(evs, requireHeld(t, r))
				time.Sleep(400 * time.Millisecond)
			}

			// Correction 4: a precise expected outcome for THIS scenario.
			sc.outcome(t, evs)
			t.Logf("APA55 %s repeats=%d report_ready=%d escalated=%d",
				sc.name, repeats,
				countOutcome(evs, string(OutcomeReportReady)),
				countOutcome(evs, string(OutcomeEscalated)))
		})
	}
}

func TestAPA56Causal_LiveMeasurement(t *testing.T) {
	m, wire := requireLiveGroq(t)

	env, scope := apa56CausalFixture(t)
	// No foreign-tenant evidence is advertised by this fixture, so nothing is
	// forbidden. The map is passed explicitly rather than left nil because it is
	// the same argument every seeded driver passes, and a nil here would silently
	// read as "nothing to check" instead of "nothing to check, deliberately".
	var forbidden map[string]struct{}

	// The premise, before a single token is spent.
	apa56CausalPremise(t, env, scope)
	_, promptSHA := apa56CausalPrompt(t, env, scope)
	t.Logf("APA56-CAUSAL prompt_sha256=%s missing_evidence=%v deadline_ms=%d "+
		"note=this-hash-is-a-NEW-measurement-not-gated-against-the-apa59-anchor",
		promptSHA, env.MissingEvidence, scope.DeadlineMs)

	repeats := qualRepeats(t)
	var evs []qualEvidence
	var obs []apa56CausalObs
	for i := 1; i <= repeats; i++ {
		i := i
		r := qualMeasureRepeat(t, m, wire, func(rm *qualModel, rw qualWireLog) qualRun {
			return runLiveSeeded(t, "apa56_causal", i, rm, rw, env, scope, forbidden)
		})
		// Anti-vacuity, per repetition, BEFORE any verdict — the same ordering
		// the sibling matrices use, because a boundary verdict is meaningless
		// until the instrument is shown to have observed the run.
		apa55AssertExercised(t, r, false)
		ev := requireHeld(t, r)

		apa56CausalLogEvidence(t, ev, r, promptSHA)
		evs = append(evs, ev)
		obs = append(obs, apa56CausalObserve(ev, r))
		time.Sleep(400 * time.Millisecond)
	}

	apa56CausalOutcome(t, obs)
	t.Logf("APA56-CAUSAL repeats=%d report_ready=%d escalated=%d",
		repeats,
		countOutcome(evs, string(OutcomeReportReady)),
		countOutcome(evs, string(OutcomeEscalated)))
}

func TestAPA58_ScenarioFixtures_Matrix(t *testing.T) {
	// Opt in ONCE, at the parent.
	//
	// Every case calls requireLivePoolside, which skips without
	// POOLSIDE_API_KEY, so the parent previously ran and produced five skipped
	// children. That shape passes today, but it reports a five-case matrix as
	// run-and-skipped on a machine that has no Poolside credential at all,
	// which reads like the matrix was attempted and found nothing. Skipping
	// once at the parent makes "this run had no alternate-provider coverage"
	// a single unambiguous line, and keeps PG-only qualification honest about
	// what it covered.
	if !psLiveOptIn() {
		t.Skip("POOLSIDE_API_KEY unset; the alternate-provider matrix requires live " +
			"inference. PG-only qualification is unaffected.")
	}
	for _, tc := range psAllScenarios() {
		t.Run(tc.name, func(t *testing.T) {
			m, wire := requireLivePoolside(t)
			env, scope, forbidden := tc.build(t)
			tc.premises(t, env)

			repeats := qualRepeats(t)
			var evs []qualEvidence
			for i := 1; i <= repeats; i++ {
				r := qualMeasureRepeat(t, m, wire, func(rm *qualModel, rw qualWireLog) qualRun {
					return runLiveSeeded(t, tc.name, i, rm, rw, env, scope, forbidden)
				})
				evs = append(evs, requireHeld(t, r))
				time.Sleep(400 * time.Millisecond)
			}
			tc.outcome(t, evs)
			t.Logf("APA58 %s repeats=%d report_ready=%d escalated=%d",
				tc.name, repeats,
				countOutcome(evs, string(OutcomeReportReady)),
				countOutcome(evs, string(OutcomeEscalated)))
		})
	}
}

func TestAPA58_Poolside_ServesTheProductionShape(t *testing.T) {
	m, _ := requireLivePoolside(t)
	env := testEnvelope(t)
	scope := testScope(env)
	resp, err := m.Complete(context.Background(), ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{"ev-doc-01"},
		Turn:             1,
		RequestID:        scope.RequestID,
	})
	if err != nil {
		t.Fatalf("production shape not served: %v", err)
	}
	t.Logf("APA58-SHAPE model_served=%s bytes=%d", resp.ModelID, len(resp.Payload))

	// The act must survive the SAME authoritative decoder and validator that
	// Groq output must. ValidateModelAction is what enforces tool ownership
	// against the scope (APA-54), so no separate tool lookup is needed.
	a, err := DecodeModelAction(resp.Payload, 1<<20)
	if err != nil {
		t.Fatalf("poolside output rejected by the shared decoder: %v", err)
	}
	if err := ValidateModelAction(a, scope, env.InvestigationID); err != nil {
		t.Fatalf("poolside output rejected by the shared validator: %v", err)
	}
	t.Logf("APA58-SHAPE action=%s tool=%q survives the shared decoder and validator", a.Action, a.Tool)
}

func TestAPA59ABDecisiveToolTrajectory(t *testing.T) {
	if !psLiveOptIn() {
		t.Skip("POOLSIDE_API_KEY unset; the APA-59 A/B requires live inference")
	}
	env, scope, _ := psFixtureNeedsTool(t)
	psPremiseNeedsTool(t, env)

	// Freeze arm B's exact prompt bytes and hash before any provider call, so
	// PR B can assert production == measured B.
	canonicalReq := ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{},
		Turn:             1,
		RequestID:        scope.RequestID,
	}
	if armBPrompt, err := renderAPA59Variant(canonicalReq, PromptVariantAPA59); err != nil {
		t.Fatalf("render arm B: %v", err)
	} else {
		sum := sha256.Sum256([]byte(armBPrompt))
		t.Logf("APA59_B_PROMPT_SHA256=%s", hex.EncodeToString(sum[:]))
	}

	type traj struct {
		attempted        int // model asked for a call_tool action
		executorObserved int // request entered the real executor path
		completed        int // executor retained/recorded a completed response
	}
	var trajectories [2]traj
	for i, arm := range []PromptVariant{PromptVariantAPA56, PromptVariantAPA59} {
		m, wire := requireLivePoolside(t)
		pc, ok := m.inner.(*poolsideClient)
		if !ok {
			t.Fatalf("qualModel inner is not *poolsideClient for arm %v", arm)
		}
		pc.render = func(arm PromptVariant) func(ModelRequest) (string, error) {
			return func(r ModelRequest) (string, error) { return renderAPA59Variant(r, arm) }
		}(arm)
		dec := &abActionRecorder{inner: m.inner}
		m.inner = dec

		r := runLiveSeeded(t, "ps_b1_valid_tool", 1, m, wire, env, scope, nil)
		for j, a := range dec.acts {
			raw, _ := json.Marshal(a)
			t.Logf("APA59 arm %s act[%d]: %s", arm, j, raw)
		}
		if r.Err != nil {
			t.Logf("APA59 arm %s err: %v", arm, r.Err)
		}
		trajectories[i] = traj{
			attempted:        dec.count("call_tool"),
			executorObserved: r.Executor.Observed(),
			completed:        len(r.Executor.recorded()),
		}
		t.Logf("APA59 arm %s: attemptedTool=%d executorObserved=%d executorCalls=%d completedResponses=%d",
			arm, trajectories[i].attempted, trajectories[i].executorObserved, r.Executor.Calls(), trajectories[i].completed)
		time.Sleep(400 * time.Millisecond)
	}

	a, b := trajectories[0], trajectories[1]
	t.Logf("APA59_TRAJECTORY_A: attempted=%d executorObserved=%d completed=%d", a.attempted, a.executorObserved, a.completed)
	t.Logf("APA59_TRAJECTORY_B: attempted=%d executorObserved=%d completed=%d", b.attempted, b.executorObserved, b.completed)

	// Decisive rule (revised): the executor-observed count is the fact this
	// experiment is about. attempted>0 && executorObserved>0 means the bounded
	// request actually reached the executor. A completed_response is a
	// separate, stricter lifecycle event and is reported as such, NOT folded
	// into the causal claim. A green B with executorObserved=0 is a NULL
	// RESULT, never causal evidence.
	if a.attempted == 0 {
		t.Errorf("arm A never attempted a tool; the control does not reproduce " +
			"the defect, so B cannot be attributed")
	}
	if a.executorObserved != 0 {
		t.Errorf("arm A: executor observed %d call(s); expected 0. The control is not the "+
			"pre-APA-59 condition, so any A→B difference is confounded", a.executorObserved)
	}
	if b.attempted == 0 {
		t.Error("arm B never attempted a tool; the model did not act on the new bound")
	}
	if b.executorObserved == 0 {
		t.Errorf("NULL RESULT: arm B attempted %d tool(s) but executorObserved=0. A green B "+
			"with executorObserved=0 is NOT evidence that APA-59 fixed the contract", b.attempted)
	}
}
