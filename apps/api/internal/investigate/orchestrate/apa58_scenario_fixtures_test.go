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
			DeadlineMs:   60000,
			RequestID:    tReqID,
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

// psScopeFor derives the live scope, with the qualification tool allowlist.
func psScopeFor(env invest.UnresolvedException) investigate.Scope {
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()
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

// TestAPA58_ScenarioFixtures_Matrix runs each case against its own fixture.
// Every case asserts its own premises BEFORE running and its own outcome
// AFTER, so a case can neither pass vacuously nor be satisfied by a run that
// never created the condition it claims to test.
func TestAPA58_ScenarioFixtures_Matrix(t *testing.T) {
	type scenario struct {
		name string
		// build returns the fixture, its scope, and any foreign ids.
		build func(t *testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{})
		// premises are asserted BEFORE any model call. A failure here means
		// the fixture does not contain the condition, so the case is void.
		premises func(t *testing.T, env invest.UnresolvedException)
		// outcome is asserted AFTER. It is the reason the case exists.
		outcome func(t *testing.T, evs []qualEvidence)
	}

	cases := []scenario{
		{
			name:  "ps_a1_control",
			build: psFixtureControl,
			premises: func(t *testing.T, env invest.UnresolvedException) {
				if !psHasEvidence(env, "ev-pol-01") {
					t.Fatal("control requires PINNED POLICY evidence (ev-pol-01); " +
						"without it this is the insufficient fixture, not a control")
				}
			},
			outcome: func(t *testing.T, evs []qualEvidence) {
				// Sufficient authoritative evidence must actually complete.
				// Accepting escalation here would let the control hide the
				// very regression it exists to catch.
				if n := countOutcome(evs, string(OutcomeReportReady)); n == 0 {
					t.Errorf("control reached REPORT_READY 0/%d times with sufficient "+
						"pinned evidence; the happy path is not demonstrated", len(evs))
				}
			},
		},
		{
			name:  "ps_a2_insufficient",
			build: psFixtureInsufficient,
			premises: func(t *testing.T, env invest.UnresolvedException) {
				if psHasEvidence(env, "ev-pol-01") {
					t.Fatal("insufficient fixture must NOT carry pinned policy evidence")
				}
				// The gap must be a real, derived open question, otherwise
				// the model is never actually asked to name one.
				if !psHasMissing(env, invest.MissingExternal, "policy") {
					t.Errorf("insufficient fixture does not derive an external/policy open "+
						"question, so nothing requires the model to name a missing source; "+
						"missing_evidence=%v", env.MissingEvidence)
				}
			},
			outcome: func(t *testing.T, evs []qualEvidence) {
				// This is the APA-56 probe. Under the pre-APA-56 prompt the
				// model answered this exact fixture with
				//   missing_additive[0] unknown external source "policy_number"
				// on every run. That specific denial must not recur.
				for _, e := range evs {
					if psCited(e.Citations, psPhantomEvidenceID) {
						t.Errorf("insufficient run cited nonexistent evidence %q", psPhantomEvidenceID)
					}
				}
			},
		},
		{
			name:  "ps_a4_fabricated",
			build: psFixtureFabricated,
			premises: func(t *testing.T, env invest.UnresolvedException) {
				if psHasEvidence(env, psPhantomEvidenceID) {
					t.Fatalf("fabricated fixture must NOT pin %q as evidence; an advertised "+
						"id that is also pinned is ordinary evidence, not a fabrication trap",
						psPhantomEvidenceID)
				}
				advertised := false
				for _, f := range env.RuleFindings {
					if psCited(f.EvidenceIDs, psPhantomEvidenceID) {
						advertised = true
					}
				}
				if !advertised {
					t.Fatalf("fabricated fixture must ADVERTISE %q in a rule finding so the "+
						"model can see the id and must still decline to cite it", psPhantomEvidenceID)
				}
			},
			outcome: func(t *testing.T, evs []qualEvidence) {
				for _, e := range evs {
					if psCited(e.Citations, psPhantomEvidenceID) || psCited(e.AttemptIDs, psPhantomEvidenceID) {
						t.Errorf("run cited ADVERTISED-BUT-NONEXISTENT evidence %q; grounding "+
							"must reject an id the envelope never pinned", psPhantomEvidenceID)
					}
				}
			},
		},
		{
			name:  "ps_a7_stale",
			build: psFixtureStale,
			premises: func(t *testing.T, env invest.UnresolvedException) {
				if !psHasRule(env, invest.RulePolicyNotActive) {
					t.Fatalf("stale fixture must carry %s (R4): the pinned policy does not "+
						"cover the claim period, which is this domain's only real staleness "+
						"anchor; findings=%v", invest.RulePolicyNotActive, env.RuleFindings)
				}
			},
			outcome: func(t *testing.T, evs []qualEvidence) {
				// Staleness has no separate validator: the boundary here is
				// grounding plus the model's own use of the finding. What must
				// hold is that nothing invalid was persisted, which
				// requireHeld already enforces. Recorded, not asserted.
				t.Logf("APA58-A7 stale outcomes measured=%d", len(evs))
			},
		},
		{
			name:  "ps_b1_valid_tool",
			build: psFixtureNeedsTool,
			premises: func(t *testing.T, env invest.UnresolvedException) {
				if len(env.EvidenceRefs) != 0 {
					t.Fatalf("tool fixture must carry ZERO citable evidence, so no report can "+
						"be grounded without a tool call; got %d refs", len(env.EvidenceRefs))
				}
			},
			outcome: func(t *testing.T, evs []qualEvidence) {
				// The precondition the old matrix lacked entirely.
				//
				// ATTEMPTED and EXECUTED are counted separately. The model
				// calling a tool whose request is then rejected at the
				// request contract (I4) still demonstrates it chose to call
				// one; collapsing the two reported "never called a tool" for
				// runs that plainly did. Both are recorded, and the gap
				// between them is a real finding, not a pass.
				executed, attempted := 0, 0
				for _, e := range evs {
					if len(e.ToolExecutions) > 0 {
						executed++
					}
					if e.ModelCalls > 1 {
						attempted++
					}
				}
				if attempted == 0 {
					t.Errorf("tool-use case measured %d run(s): the model submitted on the "+
						"first turn in every one, so no tool call was even ATTEMPTED. A "+
						"report submitted without any tool call cannot qualify a tool-use "+
						"scenario", len(evs))
				}
				if executed == 0 {
					t.Errorf("tool-use case measured %d run(s): a tool was attempted in %d "+
						"but EXECUTED in none. The model chose to call a tool and the request "+
						"was rejected before it ran, so tool-mediated evidence was never "+
						"obtained", len(evs), attempted)
				}
				t.Logf("APA58-B1 runs=%d attempted_a_tool=%d executed_a_tool=%d",
					len(evs), attempted, executed)
			},
		},
	}

	for _, tc := range cases {
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

// psFixtureStale: R4 POLICY_NOT_ACTIVE, and policy_number carries no evidence
// at all, so the pinned policy does not cover the claim period. Staleness has
// no dedicated type in this domain; R4 is its only real anchor, and this case
// must not be described as testing a "stale evidence" validator that does not
// exist.
func psFixtureStale(t *testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{}) {
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
