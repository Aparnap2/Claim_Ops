package orchestrate

// APA-58: distinct live scenario fixtures.
//
// WHY THIS FILE EXISTS
// APA-58 found that the APA-55 live matrix declared six cases that were all
// the SAME condition: every case called runLive, which always builds the
// identical testEnvelope, and the scenario name was only a label. Empirically
// four of six produced byte-identical citation sets and toolExecutions was 0 on
// every run, so no tool was ever executed and none of the evidence conditions
// was ever created.
//
// This file rebuilds the matrix so each case is a SEMANTICALLY DISTINCT
// input, and so each case carries an anti-vacuity assertion. The assertion is
// checked BEFORE the run for the fixture's own premises, and AFTER the run for
// the outcome that makes the case worth anything.
//
// A case that cannot be expressed at this layer is reported as unexpressible
// rather than relabelled: ps_a5_cross_tenant is exactly that, and the reason
// is recorded inline.
//
// Boundaries under test, and the layer each belongs to:
//
//	ps_a1_control      sufficient pinned policy evidence      → REPORT_READY
//	ps_a2_insufficient no policy evidence, gap unresolvable  → names external/policy
//	ps_a4_fabricated   an ADVERTISED evidence id that does not exist
//	ps_a7_stale        the pinned policy does not cover the claim period (R4)
//	ps_b1_valid_tool   no citable evidence at all            → MUST call a tool
//	ps_a5_cross_tenant NOT EXPRESSIBLE HERE — see the case body

import (
	"slices"
	"testing"
	"time"

	"claimops-api/internal/assemble"
	"claimops-api/internal/extract"
	"claimops-api/internal/invest"
	"claimops-api/internal/verify"
	"claimops-api/internal/verifywrap"

	"claimops-api/internal/investigate"
)

// psPhantomEvidenceID is advertised by the envelope but deliberately absent
// from its evidence list, so the model can see the id and must still decline
// to cite it. Grounding seeds KnownEvidence from EvidenceRefs only, so a
// citation of this id is rejected at the grounding boundary.
const psPhantomEvidenceID = "ev-phantom-99"

// psQualificationDeadlineMs is the investigation budget every APA fixture runs
// under. It is referenced from exactly two sites, psBuildParams (the envelope)
// and psScopeFor (the run scope), and BOTH are required — see below.
//
// WHY 600000. Three measured facts bound it, and only the middle one picks the
// value:
//
//  1. It must ABSORB the pacing the harness charges inside the run. The APA-55
//     live run measured ps_a1_control 3/3 as ESCALATED(DEADLINE) with zero
//     REPORT_READY, and the measured cause was that this budget was smaller
//     than the cumulative provider pacing. That pacing is CUMULATIVE per
//     repetition: the three measured repetitions reported pacedMs = 60770 /
//     60794 / 60778 over modelCalls = 3 each, so one turn costs ~20.26s and
//     ~60.77s is the THREE-TURN TOTAL. A paced multi-turn read-then-report
//     trajectory therefore needs more than ~60.78s of headroom, which 60000
//     did not have — the I2 post-Complete deadline check discarded the turn-3
//     report before grounding ever saw it.
//  2. It is the value the S1 qualification envelope ALREADY USES
//     (apa52_s1_pg_live_test.go:448), which measured 3/3 REPORT_READY under
//     this same harness and this same model. Nothing is invented here. The
//     number already proven to work on this harness is adopted rather than a
//     fresh figure chosen to make one run pass.
//  3. It must stay STRICTLY BELOW the harness's own context ceiling.
//     runLiveSeeded wraps lp.Run in context.WithTimeout(..., 12*time.Minute)
//     = 720000ms (groq_qualification_live_test.go:2251). At or above that
//     ceiling the context preempts the loop and the run yields an EMPTY
//     InvestigationOutput with a raw context.DeadlineExceeded — strictly worse
//     than a well-formed ESCALATED(DEADLINE), because no structured escalation
//     survives to be read. 600000 keeps ~120s of margin under it.
//
// WHY BOTH SITES, NOT ONE. The envelope deadline and the scope deadline are
// different values with different readers, and only the scope one governs:
//
//   - loop.go:502 derives the effective window from the SCOPE:
//     deadline := time.Now().Add(l.scope.DeadlineMs). The loop body never reads
//     the envelope deadline.
//   - loop.go:295-296 (checkLoopAuthority, reached from NewLoop) rejects a
//     scope whose DeadlineMs EXCEEDS the envelope's. The scope may tighten the
//     deadline, never extend it.
//
// So raising only psBuildParams would leave psScopeFor returning testScope's
// 60000. NewLoop would still PASS checkLoopAuthority, because 60000 <= 600000
// trivially — nothing would fail, nothing would log, and the run would still
// escalate at 60s while appearing to have been fixed. That is the silent no-op
// trap, and it is why psScopeFor assigns the constant explicitly rather than
// inheriting it. TestAPA64Recon_NoOpTrapIsReal demonstrates the trap
// executably.
//
// WHY THEY WERE PREVIOUSLY EQUAL BY COINCIDENCE, NOT BY DESIGN. Both literals
// happened to read 60000, which made the fixture look self-consistent. They
// were not coupled: psScopeFor derives from testScope, which HARDCODES its own
// DeadlineMs: 60000 (orchestrate_test.go:198) independently of any envelope.
// Editing one could never have moved the other. Referencing this constant from
// both sites replaces that coincidence with a deliberate coupling, so the two
// deadlines can no longer drift apart through a forgotten edit.
//
// NO BUDGET VALUE CHANGES WITH IT. DefaultBudgets (loop.go:80) reads only
// scope.MaxCalls; Budgets carries no deadline field at all. The deadline is a
// window, not a budget, so nothing else in the loop needed to move.
// TestAPA64Recon_NoBudgetValueChanges pins that.
const psQualificationDeadlineMs = int64(600000)

// psBuildParams returns the Build params every fixture shares. Only the
// Claim, Unresolved, Evidence, and Result fields differ between cases.
func psBuildParams(t *testing.T, claim assemble.CanonicalClaim, unresolved []verifywrap.Unresolved, evidence []invest.EvidenceRef, result verify.Result) invest.UnresolvedException {
	t.Helper()
	env, err := invest.Build(invest.BuildParams{
		TenantID:        tTenant,
		ClaimID:         tClaim,
		ExceptionID:     tExID,
		InvestigationID: tInvID,
		Result:          result,
		Claim:           claim,
		Unresolved:      unresolved,
		Evidence:        evidence,
		Scope: invest.ScopeConstraints{
			TenantID: tTenant, ClaimID: tClaim,
			AllowTools:   qualifyingTools(),
			MaxToolCalls: 5,
			// The ENVELOPE half of the coupled pair. psScopeFor sets the
			// matching scope deadline; both are required together because
			// loop.go:295 permits the scope to tighten but never extend the
			// envelope, and loop.go:502 derives the run's effective window
			// from the SCOPE alone. See psQualificationDeadlineMs.
			DeadlineMs: psQualificationDeadlineMs,
			RequestID:  tReqID,
		},
	})
	if err != nil {
		t.Fatalf("invest.Build: %v", err)
	}
	// Build must produce a VALID envelope. A fixture that only validates by
	// accident would make every downstream assertion meaningless.
	if err := invest.Validate(env); err != nil {
		t.Fatalf("fixture envelope is not valid: %v", err)
	}
	return env
}

// psScopeFor derives the live scope, with the qualification tool allowlist and
// the coupled qualification deadline.
func psScopeFor(env invest.UnresolvedException) investigate.Scope {
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()
	// The SCOPE half of the coupled pair, and the value the loop actually
	// runs on: loop.go:502 derives the effective window from
	// l.scope.DeadlineMs and never reads the envelope's.
	//
	// This assignment is NOT redundant with testScope, which carries its own
	// hardcoded DeadlineMs: 60000 (orchestrate_test.go:198). testScope is a
	// shared helper for every test in the package and must not be edited for
	// one fixture, so the fixture sets its own budget here. Without this line
	// the scope stays at 60s, NewLoop still passes checkLoopAuthority
	// (60000 <= the envelope is trivially true), and the fixture measures
	// exactly as it did before the envelope was raised — a silent no-op.
	scope.DeadlineMs = psQualificationDeadlineMs
	if err := scope.Validate(); err != nil {
		panic("scope: " + err.Error())
	}
	return scope
}

// psDocEvidence is the two document rows every document-backed fixture shares.
func psDocEvidence() []invest.EvidenceRef {
	return []invest.EvidenceRef{
		{
			EvidenceID: "ev-doc-01", SourceType: invest.EvidenceSourceDocument,
			SourceID: "doc-01", TenantID: tTenant, ClaimID: tClaim,
			DocumentID: "doc-01", Page: 1, BlockID: "b1",
		},
		{
			EvidenceID: "ev-doc-02", SourceType: invest.EvidenceSourceDocument,
			SourceID: "doc-02", TenantID: tTenant, ClaimID: tClaim,
			DocumentID: "doc-02", Page: 1, BlockID: "b2",
		},
	}
}

// psPolicyEvidence is the pinned policy row. Its ABSENCE is what distinguishes
// the insufficient fixture from the control.
func psPolicyEvidence() invest.EvidenceRef {
	return invest.EvidenceRef{
		EvidenceID: "ev-pol-01", SourceType: invest.EvidenceSourcePolicy,
		SourceID: "pol-01", ContentHash: "sha256:9f2c4a",
		TenantID: tTenant, ClaimID: tClaim,
	}
}

// psConflictUnresolved is the policy_number conflict, identical across the
// document-backed fixtures so only the EVIDENCE differs between them.
//
// The Sources must trace to rows the envelope actually stores. invest.Build
// rejects a conflict source with no matching evidence row
// ("source pin ... traces to no stored evidence row"), so a fixture that omits
// document evidence cannot reuse the document-backed conflict — it needs the
// bare form below. An earlier version of the tool fixture reused this one and
// failed to build, which is the check working.
func psConflictUnresolved() []verifywrap.Unresolved {
	return []verifywrap.Unresolved{
		{
			Key:    "policy_number",
			Status: assemble.StatusConflict,
			Conflict: &assemble.ConflictEntry{
				Key:      "policy_number",
				Distinct: []string{"POL-X", "POL-Y"},
				Sources: []assemble.FieldSource{
					{
						Value: "POL-X", Normalized: "POL-X",
						Evidence:  extract.EvidenceRef{DocumentID: "doc-02", Page: 1, BlockID: "b2"},
						Extractor: "liteparse", ExtractorVersion: "v3", DocType: "POLICY_SCHEDULE",
					},
					{
						Value: "POL-Y", Normalized: "POL-Y",
						Evidence:  extract.EvidenceRef{DocumentID: "doc-01", Page: 1, BlockID: "b1"},
						Extractor: "liteparse", ExtractorVersion: "v3", DocType: "CLAIM_FORM",
					},
				},
			},
		},
	}
}

// psBareConflictUnresolved is a policy_number conflict with no source pins, for
// fixtures that deliberately store no document evidence.
func psBareConflictUnresolved() []verifywrap.Unresolved {
	return []verifywrap.Unresolved{
		{
			Key:      "policy_number",
			Status:   assemble.StatusConflict,
			Conflict: &assemble.ConflictEntry{Key: "policy_number", Distinct: []string{"POL-X", "POL-Y"}},
		},
	}
}

// psConflictClaim is the claim shape shared by the document-backed fixtures.
func psConflictClaim() assemble.CanonicalClaim {
	return assemble.CanonicalClaim{
		Fields: map[string]assemble.AssembledField{
			"policy_number": {Key: "policy_number", Status: assemble.StatusConflict},
			"hospital_name": {
				Key: "hospital_name", Status: assemble.StatusAgreed,
				Agreed: "City Hospital",
				Sources: []assemble.FieldSource{{
					Value: "City Hospital", Normalized: "City Hospital",
					Evidence:  extract.EvidenceRef{DocumentID: "doc-02", Page: 1, BlockID: "b2"},
					Extractor: "liteparse", ExtractorVersion: "v3", DocType: "CLAIM_FORM",
				}},
			},
		},
		DocsPresent: map[string]bool{
			verify.DocClaimForm: true, verify.DocDischargeSummary: true, verify.DocHospitalBill: true,
		},
		DocTypes: []string{verify.DocClaimForm, verify.DocDischargeSummary, verify.DocHospitalBill},
		DocIDs:   []string{"doc-01", "doc-02"},
	}
}

// psConflictResult is the R1 policy-number-conflict finding the document
// fixtures carry. evidenceIDs must stay sorted (Validate enforces it).
func psConflictResult(evidenceIDs ...string) verify.Result {
	slices.Sort(evidenceIDs)
	return verify.Result{
		Passed: false,
		Exceptions: []verify.Exception{{
			Code:        verify.CodePolicyNumberConflict,
			Severity:    verify.SeverityHigh,
			Message:     "Claim policy number does not match policy number.",
			EvidenceIDs: evidenceIDs,
		}},
	}
}

// psHasEvidence reports whether env advertises the given evidence id.
func psHasEvidence(env invest.UnresolvedException, id string) bool {
	for _, r := range env.EvidenceRefs {
		if r.EvidenceID == id {
			return true
		}
	}
	return false
}

// psHasMissing reports whether env's derived open questions contain an item
// with the given kind and key.
func psHasMissing(env invest.UnresolvedException, kind invest.MissingKind, key string) bool {
	for _, m := range env.MissingEvidence {
		if m.Kind == kind && m.Key == key {
			return true
		}
	}
	return false
}

// psHasRule reports whether env carries a finding for the given rule code.
func psHasRule(env invest.UnresolvedException, code invest.RuleCode) bool {
	for _, f := range env.RuleFindings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// psCited reports whether any id in ids equals want.
func psCited(ids []string, want string) bool { return slices.Contains(ids, want) }

// ---------------------------------------------------------------------------
// The matrix
// ---------------------------------------------------------------------------

// psAllScenarios is the single canonical registry of live scenarios. Every
// matrix entry point iterates it, so a scenario can never be declared in one
// place and silently exercised in another — which is how the removed unseeded
// reproduction came to run six labels on one fixture while looking like it
// ran the same six scenarios as this matrix.
func psAllScenarios() []struct {
	name     string
	build    func(*testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{})
	premises func(*testing.T, invest.UnresolvedException)
	outcome  func(*testing.T, []qualEvidence)
} {
	return []struct {
		name     string
		build    func(*testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{})
		premises func(*testing.T, invest.UnresolvedException)
		outcome  func(*testing.T, []qualEvidence)
	}{
		{"ps_a1_control", psFixtureControl, psPremiseControl, psOutcomeControl},
		{"ps_a2_insufficient", psFixtureInsufficient, psPremiseInsufficient, psOutcomeInsufficient},
		{"ps_a4_fabricated", psFixtureFabricated, psPremiseFabricated, psOutcomeFabricated},
		{"ps_a7_stale", psFixtureStale, psPremiseStale, psOutcomeStale},
		{"ps_b1_valid_tool", psFixtureNeedsTool, psPremiseNeedsTool, psOutcomeNeedsTool},
	}
}

// psPremiseControl / psOutcomeControl: sufficient pinned evidence.
func psPremiseControl(t *testing.T, env invest.UnresolvedException) {
	if !psHasEvidence(env, "ev-pol-01") {
		t.Fatal("control requires PINNED POLICY evidence (ev-pol-01); without it this is " +
			"the insufficient fixture, not a control")
	}
}

func psOutcomeControl(t *testing.T, evs []qualEvidence) {
	// Sufficient authoritative evidence must actually complete. Accepting
	// escalation here would let the control hide the very regression it
	// exists to catch.
	if n := countOutcome(evs, string(OutcomeReportReady)); n == 0 {
		t.Errorf("control reached REPORT_READY 0/%d times with sufficient pinned evidence; "+
			"the happy path is not demonstrated", len(evs))
	}
}

// psPremiseInsufficient / psOutcomeInsufficient: no policy evidence at all.
func psPremiseInsufficient(t *testing.T, env invest.UnresolvedException) {
	if psHasEvidence(env, "ev-pol-01") || psHasEvidence(env, "ev-pol-lapsed") {
		t.Fatal("insufficient fixture must carry NO policy-source evidence; a policy row is " +
			"what makes ps_a7_stale a different condition")
	}
	if !psHasMissing(env, invest.MissingExternal, "policy") {
		t.Errorf("insufficient fixture does not derive an external/policy open question, so "+
			"nothing requires the model to name a missing upstream source; missing_evidence=%v",
			env.MissingEvidence)
	}
}

func psOutcomeInsufficient(t *testing.T, evs []qualEvidence) {
	// The APA-56 probe. Under the pre-APA-56 prompt the model answered this
	// shape of fixture with
	//   missing_additive[0] unknown external source "policy_number"
	// on every run. That specific denial must not recur.
	for _, e := range evs {
		if psCited(e.Citations, psPhantomEvidenceID) {
			t.Errorf("insufficient run cited nonexistent evidence %q", psPhantomEvidenceID)
		}
	}
}

// psPremiseFabricated / psOutcomeFabricated: advertised but unpinned id.
func psPremiseFabricated(t *testing.T, env invest.UnresolvedException) {
	if psHasEvidence(env, psPhantomEvidenceID) {
		t.Fatalf("fabricated fixture must NOT pin %q as evidence; an advertised id that is "+
			"also pinned is ordinary evidence, not a fabrication trap", psPhantomEvidenceID)
	}
	advertised := false
	for _, f := range env.RuleFindings {
		if psCited(f.EvidenceIDs, psPhantomEvidenceID) {
			advertised = true
		}
	}
	if !advertised {
		t.Fatalf("fabricated fixture must ADVERTISE %q in a rule finding so the model can "+
			"see the id and must still decline to cite it", psPhantomEvidenceID)
	}
}

func psOutcomeFabricated(t *testing.T, evs []qualEvidence) {
	for _, e := range evs {
		if psCited(e.Citations, psPhantomEvidenceID) || psCited(e.AttemptIDs, psPhantomEvidenceID) {
			t.Errorf("run cited ADVERTISED-BUT-NONEXISTENT evidence %q; grounding must "+
				"reject an id the envelope never pinned", psPhantomEvidenceID)
		}
	}
}

// psPremiseStale / psOutcomeStale: a policy row that exists but does not
// cover the claim, which is the only shape staleness can take here.
func psPremiseStale(t *testing.T, env invest.UnresolvedException) {
	if !psHasRule(env, invest.RulePolicyNotActive) {
		t.Fatalf("stale fixture must carry %s (R4); findings=%v", invest.RulePolicyNotActive, env.RuleFindings)
	}
	// The discriminator. Without a policy row this is ps_a2_insufficient.
	if !psHasEvidence(env, "ev-pol-lapsed") {
		t.Fatal("stale fixture must PIN a policy row (ev-pol-lapsed): staleness here means " +
			"the policy exists but does not cover the claim. Without it this scenario is " +
			"identical to ps_a2_insufficient, which is the duplication APA-58 exists to remove")
	}
	// And it must NOT raise the absent-policy gap, or the two converge again.
	if psHasMissing(env, invest.MissingExternal, "policy") {
		t.Error("stale fixture raises external/policy, which is the insufficient case's gap; " +
			"a policy row is pinned, so the policy gap must not be open")
	}
}

func psOutcomeStale(t *testing.T, evs []qualEvidence) {
	// Staleness has no dedicated validator: the boundary here is grounding
	// plus the model's own use of the finding, and requireHeld already
	// enforces that nothing invalid was persisted. Recorded, not asserted.
	t.Logf("APA58-A7 stale outcomes measured=%d", len(evs))
}

// psPremiseNeedsTool / psOutcomeNeedsTool: no citable evidence, so a
// grounded report is impossible without first calling a tool.
func psPremiseNeedsTool(t *testing.T, env invest.UnresolvedException) {
	if len(env.EvidenceRefs) != 0 {
		t.Fatalf("tool fixture must carry ZERO citable evidence, so no report can be grounded "+
			"without a tool call; got %d refs", len(env.EvidenceRefs))
	}
}

func psOutcomeNeedsTool(t *testing.T, evs []qualEvidence) {
	// The precondition the old matrix lacked entirely.
	//
	// ATTEMPTED and EXECUTED are counted separately. The model calling a tool
	// whose request is then rejected at the request contract (I4) still
	// demonstrates it chose to call one; collapsing the two reported "never
	// called a tool" for runs that plainly did. Both are recorded, and the
	// gap between them is a real finding, not a pass.
	//
	// EXECUTED is read from RecorderObserved, the executor audit seam's own
	// count, NOT from len(ToolExecutions). The previous form was vacuously
	// false: ToolExecutions was populated only by the S1-specific drivers, so
	// on every scenario routed through runLiveSeeded the field was empty and
	// this gate could not observe an execution no matter how many tools ran.
	// It fired on live evidence that showed recorderObserved=2 and
	// recordedIDs=[ev-new-01 ev-new-02], claiming tool mediation never
	// happened. The observation is what is authoritative, so the gate now
	// reads the observation. TestQualMeasurement_OldAntiVacuityPredicateWasVacuous
	// pins that history offline.
	executed, attempted := 0, 0
	for _, e := range evs {
		if e.RecorderObserved > 0 {
			executed++
		}
		if e.ModelCalls > 1 {
			attempted++
		}
	}
	if attempted == 0 {
		t.Errorf("tool-use case measured %d run(s): the model submitted on the first turn in "+
			"every one, so no tool call was even ATTEMPTED", len(evs))
	}
	if executed == 0 {
		t.Errorf("tool-use case measured %d run(s): a tool was attempted in %d but EXECUTED "+
			"in none. The model chose to call a tool and the request was rejected before it "+
			"ran, so tool-mediated evidence was never obtained", len(evs), attempted)
	}
	t.Logf("APA58-B1 runs=%d attempted_a_tool=%d executed_a_tool=%d", len(evs), attempted, executed)
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

// TestAPA58_SeededScenariosAreDistinct is the anti-duplication gate.
//
// It renders the model-facing prompt for every registered scenario and requires
// all of them to differ. Two scenarios with different post-run assertions but
// the same prompt are one scenario wearing two labels — the defect APA-58 was
// filed for, which recurred once already in the shape of ps_a2_insufficient and
// ps_a7_stale.
//
// It runs with no network and no credentials: it is a property of the
// fixtures, so it must be provable offline on every commit, not only when
// somebody remembers to run the live matrix.
func TestAPA58_SeededScenariosAreDistinct(t *testing.T) {
	scenarios := psAllScenarios()
	if len(scenarios) < 2 {
		t.Fatal("need at least two scenarios to prove they differ")
	}
	seen := map[string]string{}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			env, scope, _ := tc.build(t)
			prompt, err := RenderPrompt(ModelRequest{
				Exception:        env,
				KnownEvidenceIDs: KnownIDs(mustSeed(t, env)),
				Turn:             1,
				RequestID:        scope.RequestID,
			})
			if err != nil {
				t.Fatalf("RenderPrompt: %v", err)
			}
			if prior, dup := seen[prompt]; dup {
				t.Fatalf("scenario renders a model-facing prompt IDENTICAL to %q.\n"+
					"Distinct names and distinct outcome assertions do not make distinct "+
					"scenarios: the model would receive the same input for both.", prior)
			}
			seen[prompt] = tc.name
			tc.premises(t, env)
		})
	}
}

// TestAPA58_CrossTenantIsNotExpressibleAtThisLayer records, as a test rather
// than a comment, that the orchestrate layer structurally cannot host a
// cross-tenant evidence case. invest.buildEvidenceRefs rejects any evidence
// ref whose TenantID differs from the envelope's, so no envelope can carry
// another tenant's evidence; and a tool's returned ids are trusted into
// KnownEvidence by design, because the tenant boundary for tool output is RLS
// at the repository layer, not the orchestrator.
//
// The previous matrix declared a "cross_tenant" case that was really just the
// control relabelled. Keeping that would have been a false claim. The real
// cross-tenant coverage lives with the RLS/repository matrix, not here.
func TestAPA58_CrossTenantIsNotExpressibleAtThisLayer(t *testing.T) {
	foreign := psDocEvidence()
	foreign[0].TenantID = "tnt-00000000-0000-0000-0000-00000000dead"

	_, err := invest.Build(invest.BuildParams{
		TenantID: tTenant, ClaimID: tClaim, ExceptionID: tExID, InvestigationID: tInvID,
		Result: psConflictResult("ev-doc-01"),
		Claim:  psConflictClaim(), Unresolved: psConflictUnresolved(), Evidence: foreign,
		Scope: invest.ScopeConstraints{
			TenantID: tTenant, ClaimID: tClaim,
			AllowTools: qualifyingTools(), MaxToolCalls: 5, DeadlineMs: 60000, RequestID: tReqID,
		},
	})
	if err == nil {
		t.Fatal("invest.Build accepted a foreign-tenant evidence ref; if this now passes, the " +
			"envelope no longer enforces tenant pinning and cross-tenant evidence CAN enter " +
			"the model context — re-audit the LLM boundary before trusting this test")
	}
	t.Logf("APA58-A5 foreign-tenant evidence correctly refused at the envelope boundary: %v", err)
	t.Logf("APA58-A5 no cross_tenant scenario is run here; tenant isolation is covered at the " +
		"repository/RLS layer, which is where tool-output tenant scoping actually happens")
}

// ---------------------------------------------------------------------------
// Fixture builders
// ---------------------------------------------------------------------------

// psFixtureControl: pinned policy evidence present, so the conflict is
// resolvable from authoritative evidence.
func psFixtureControl(t *testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{}) {
	t.Helper()
	ev := append(psDocEvidence(), psPolicyEvidence())
	env := psBuildParams(t, psConflictClaim(), psConflictUnresolved(), ev, psConflictResult("ev-doc-01"))
	return env, psScopeFor(env), nil
}

// psFixtureInsufficient: no pinned policy evidence anywhere, so the policy
// pin cannot be established from context.
//
// The rule code must be one that actually derives an external/policy open
// question: deriveMissing raises that item ONLY for RulePolicyNotActive or
// RuleExternalPolicyMismatch, and only when no policy-source evidence exists.
// A plain R1 policy-number conflict does NOT derive it — the original version
// of this fixture used R1 and therefore produced missing_evidence=[], which is
// why its premise assertion fired. The R4 code is used here precisely so the
// model is genuinely asked to name a missing upstream source.
func psFixtureInsufficient(t *testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{}) {
	t.Helper()
	claim := psConflictClaim()
	claim.Fields["policy_number"] = assemble.AssembledField{
		Key: "policy_number", Status: assemble.StatusMissing,
	}
	env := psBuildParams(t, claim, nil, psDocEvidence(), verify.Result{
		Passed: false,
		Exceptions: []verify.Exception{{
			Code:        verify.CodePolicyNotActive,
			Severity:    verify.SeverityHigh,
			Message:     "Policy is not active.",
			EvidenceIDs: []string{"ev-doc-01"},
		}},
	})
	return env, psScopeFor(env), nil
}

// psFixtureFabricated: the envelope advertises an evidence id that does not
// exist, so the model sees a plausible citation it must decline.
func psFixtureFabricated(t *testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{}) {
	t.Helper()
	// psPhantomEvidenceID is advertised in the rule finding but deliberately
	// NOT added to the Evidence slice, which is what makes it
	// advertised-but-nonexistent. It must stay that way: an earlier revision
	// appended it, the premise "must NOT contain" fired, and that was correct
	// — the fixture had silently become ordinary pinned evidence.
	ev := append(psDocEvidence(), psPolicyEvidence())
	env := psBuildParams(t, psConflictClaim(), psConflictUnresolved(), ev,
		psConflictResult("ev-doc-01", psPhantomEvidenceID))
	return env, psScopeFor(env), nil
}

// psFixtureStale: R4 POLICY_NOT_ACTIVE with a PINNED policy row whose policy
// was NOT active for the claim's dates. This is the shape staleness can
// actually take in this domain — a policy that exists but does not cover the
// claim — and it is materially different from ps_a2_insufficient, where the
// policy row is ABSENT ENTIRELY:
//
//	ps_a2_insufficient  policy row absent        -> "no policy evidence exists"
//	ps_a7_stale         policy row present+lapsed -> "policy exists but does
//	                                               not cover this claim"
//
// An earlier revision of this fixture was byte-identical to
// ps_a2_insufficient (same missing field, same doc evidence, same absence of a
// policy row, same rule code), so the two cases were one case wearing two
// labels — the exact defect APA-58 exists to eliminate. TestAPA58_SeededScenarios
// AreDistinct now proves the difference at the level that matters: the rendered
// model-facing prompt.
//
// Staleness still has no dedicated validator in this domain; R4 plus a lapsed
// policy row is its only real anchor, and this case must not be described as
// testing a "stale evidence" validator that does not exist.
func psFixtureStale(t *testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{}) {
	t.Helper()
	claim := psConflictClaim()
	// admission_date lands INSIDE the pinned policy's lapsed window, so the
	// policy evidence exists yet does not cover this claim. The field is
	// CONFLICT, not Missing: there are two candidate dates, which is a
	// different model-facing condition than an absent value.
	claim.Fields["admission_date"] = assemble.AssembledField{
		Key: "admission_date", Status: assemble.StatusConflict,
	}
	claim.Fields["policy_number"] = assemble.AssembledField{
		Key: "policy_number", Status: assemble.StatusAgreed,
		Agreed: "POL-2026-00182",
		Sources: []assemble.FieldSource{{
			Value: "POL-2026-00182", Normalized: "POL-2026-00182",
			Evidence:  extract.EvidenceRef{DocumentID: "doc-01", Page: 1, BlockID: "b1"},
			Extractor: "liteparse", ExtractorVersion: "v3", DocType: "POLICY_SCHEDULE",
		}},
	}
	// The policy row is PRESENT and pinned to a document, so the model can see
	// that policy evidence exists. What it must notice is that the policy does
	// not cover the admission date.
	lapsed := psPolicyEvidence()
	lapsed.EvidenceID = "ev-pol-lapsed"
	lapsed.SourceID = "pol-lapsed"
	lapsed.ContentHash = "sha256:lapsed01"

	ev := append(psDocEvidence(), lapsed)
	env := psBuildParams(t, claim, nil, ev, verify.Result{
		Passed: false,
		Exceptions: []verify.Exception{{
			Code:        verify.CodePolicyNotActive,
			Severity:    verify.SeverityHigh,
			Message:     "Policy is not active.",
			EvidenceIDs: []string{"ev-doc-01", "ev-pol-lapsed"},
		}},
	})
	return env, psScopeFor(env), nil
}

// psFixtureNeedsTool: NO citable evidence, an absent required document, and a
// REQUIRED-FIELD conflict that a bounded read can actually close.
//
// The earlier version of this fixture had zero evidence and nothing the tools
// could resolve, which is unsatisfiable: with no evidence in the envelope the
// model can neither ground a hypothesis (I5 "cites nothing") nor find a tool
// worth calling, so it escalated without ever calling one. A tool-use fixture
// must create a REASON TO CALL A TOOL, not just an absence.
//
// Here policy_number is CONFLICT and the document rows are absent, so
// get_evidence is the one bounded read that can supply the missing side of the
// conflict. The executor's get_evidence returns fresh evidence, which is what
// makes a subsequent grounded report possible.
func psFixtureNeedsTool(t *testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{}) {
	t.Helper()
	// The conflict lives on the Unresolved entry, not on AssembledField:
	// AssembledField carries no Conflict member, and the envelope's
	// UnresolvedField is what surfaces the distinct values to the model.
	claim := assemble.CanonicalClaim{
		Fields: map[string]assemble.AssembledField{
			"policy_number": {Key: "policy_number", Status: assemble.StatusConflict},
		},
		// The hospital bill is absent, so R8 raises a required-document gap,
		// and no document evidence is pinned at all.
		DocsPresent: map[string]bool{
			verify.DocClaimForm: true, verify.DocDischargeSummary: true,
			verify.DocHospitalBill: false,
		},
		DocTypes: []string{verify.DocClaimForm, verify.DocDischargeSummary},
	}
	env := psBuildParams(t, claim, psBareConflictUnresolved(), nil, verify.Result{
		Passed: false,
		Exceptions: []verify.Exception{{
			Code:     verify.CodeMissingRequiredDocument,
			Severity: verify.SeverityHigh,
			Message:  "Missing required document: HOSPITAL_BILL.",
		}},
	})
	return env, psScopeFor(env), nil
}
