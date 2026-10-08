package orchestrate

// APA-56: the CAUSAL qualification fixture.
//
// WHAT IS ALREADY PROVEN, AND WHERE
// apa56_additive_contract_test.go is the deterministic contract owner. It
// proves, offline and without inference, that:
//
//	{Kind:"field",   Key:"policy_number"}  survives decode -> validate -> grounding
//	{Kind:"external", Key:"policy_number"}  is still rejected, with the exact
//	                                         message the 17/17 live runs produced
//
// Those are properties of a REPORT, exercised through hand-built inputs. They
// say nothing about the MODEL. The unproven claim — the one this file exists to
// measure — is causal:
//
//	Does the real model, shown an envelope that genuinely carries
//	missing_evidence, now emit the CORRECT missing_additive action?
//
// The historical answer was no: 17 of 17 Qwen runs answered
// missing_additive[0] with kind "external" / key "policy_number" and were
// denied by the validator. The Poolside arm cannot isolate APA-56 because it
// changed the model and the prompt at the same time.
//
// WHY A FIXTURE IS NEEDED AT ALL
// The pre-APA-56 live envelope derived NO missing_evidence whatsoever, so the
// model was never shown the item it got wrong. A fixture whose obligation the
// model can satisfy by ignoring it measures nothing: the model would pass
// whether or not it read the correction. So the missing condition here must be
// NECESSARY, not merely present. Two deterministic boundaries make it so:
//
//	loop.go:610  ValidateModelAction  -> validateMissingAdditive (action.go:200)
//	    rejects a WRONG kind/key. The historical shape dies here.
//	loop.go:630  CheckReportGrounding -> checkMissingAdditive (grounding.go:183)
//	    requires the submitted list to be a VERBATIM SUPERSET of the envelope's.
//	    An OMITTED item dies here.
//
// Both are TERMINAL in the same turn: repromptable (errors.go:108) admits only
// I1/I2/I7, and neither of these failures is one of those classes, so loop.go:615
// and loop.go:632 escalate INVALID_OUTPUT immediately. There is no retry path
// on which the model can recover by dropping the obligation. The ONLY route to
// REPORT_READY is to carry the envelope item across with its kind, key and
// detail unchanged.
//
// The vocabulary is not guessed. It is read from the authorities:
//
//	kind "field"                     invest.MissingField            (exception.go:200)
//	the closed external vocabulary   invest.ExternalSourceKeys()   (exception.go:768)
//	the derivation rule that emits   deriveMissing                  (exception.go:807)
//	the field key itself             invest.AffectedFields(R1)      (exception.go:86)
//
// Every one of those assertions is repeated below, offline, so a drift in any
// of them fails here rather than being discovered by a paid run.
//
// SCOPE. This file adds a fixture and a measurement contract. It changes no
// production code, no validator, no prompt, no decoder and no loop, and it
// weakens no APA-56 assertion. Nothing here is gated against the APA-59
// production-fidelity prompt hash: that hash belongs to a DIFFERENT fixture
// (a production-shaped envelope rendered at the production deadline), and this
// file's distinctness proof asserts exactly that the two are not the same bytes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/assemble"
	"claimops-api/internal/extract"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/verify"
)

// ---------------------------------------------------------------------------
// The deadline
// ---------------------------------------------------------------------------

// apa56CausalDeadlineMs is the investigation budget this fixture runs under,
// on BOTH the envelope and the scope. It is a separate declaration from the
// APA-58 fixture's named qualification constant on purpose: the Phase-2
// reconciliation proves by byte scan that the APA-58 identifier is referenced
// by that fixture's two build sites and by NO other file in this package, so
// naming it here would trip that containment proof for a mention that changes
// no deadline. The REASONING is mirrored instead, in prose and in numbers.
//
// WHY 600000. Three measured facts bound it, and only the middle one picks the
// value. This is the same derivation the APA-58 fixture records, restated
// rather than re-derived, and it is repeated here so this fixture is
// self-justifying rather than dependent on a comment in another file:
//
//  1. It must ABSORB the pacing the harness charges inside the run. That pacing
//     is CUMULATIVE per repetition: the measured repetitions reported
//     pacedMs = 60770 / 60794 / 60778 over modelCalls = 3 each, so one turn
//     costs ~20.26s and ~60.78s is the THREE-TURN TOTAL. A paced multi-turn
//     read-then-report trajectory needs more than ~60.78s of headroom; 60000
//     did not have it, which is why the earlier live run measured
//     ESCALATED(DEADLINE) 3/3 with zero REPORT_READY.
//  2. It is the value the S1 qualification envelope already uses and that
//     measured 3/3 REPORT_READY under this same harness and this same model.
//     Nothing is invented here: the number already proven to work on this
//     harness is adopted rather than a fresh figure chosen to make one run
//     pass.
//  3. It must stay STRICTLY BELOW the harness's own context ceiling.
//     runLiveSeeded wraps lp.Run in context.WithTimeout(..., 12*time.Minute)
//     = 720000ms (groq_qualification_live_test.go:2251). At or above that
//     ceiling the context preempts the loop and the run yields an EMPTY
//     InvestigationOutput with a raw context.DeadlineExceeded — strictly worse
//     than a well-formed ESCALATED(DEADLINE), because no structured escalation
//     survives to be read. 600000 keeps ~120s of margin under it.
//
// WHY BOTH SITES. The envelope deadline and the scope deadline are different
// values with different readers, and only the scope one governs:
// loop.go:502 derives the effective window from the SCOPE and never reads the
// envelope's, while loop.go:295-296 (checkLoopAuthority, reached from NewLoop)
// rejects a scope whose DeadlineMs EXCEEDS the envelope's — the scope may
// tighten, never extend. Setting only one leaves the other at testScope's own
// hardcoded 60000, NewLoop still passes checkLoopAuthority trivially, nothing
// logs, and the run still escalates at 60s while appearing to have been fixed.
// That silent no-op is why both are assigned from this one constant.
//
// NO BUDGET VALUE CHANGES WITH IT. DefaultBudgets reads only scope.MaxCalls;
// Budgets carries no deadline field at all. The deadline is a window, not a
// budget.
const apa56CausalDeadlineMs = int64(600000)

// apa56CausalLiveContextCeilingMs is runLiveSeeded's own context ceiling,
// asserted rather than remembered, so the deadline's upper-bound rationale
// cannot rot into a comment that no longer describes the harness.
const apa56CausalLiveContextCeilingMs = int64(720000)

// apa56CausalMeasuredPacingPerTurnMs is the MEASURED per-turn pacing from the
// live run, not the pacer's floor and not an estimate: ~60.77s over three
// turns. Used to assert the deadline actually clears a paced trajectory.
const apa56CausalMeasuredPacingPerTurnMs = int64(20260)

// ---------------------------------------------------------------------------
// The seeded missing condition
// ---------------------------------------------------------------------------

// apa56CausalKey is the claim field the fixture's missing item names. It is
// asserted against invest's own projection rather than trusted: R1's
// affected-field is exactly this key (AffectedFields, exception.go:86), and the
// whole experiment is about this key.
const apa56CausalKey = "policy_number"

// apa56CausalExpectedItem is the MissingItem the fixture is REQUIRED to derive.
// It is spelled out literally so a change in deriveMissing, in the rule-code
// spelling, or in the detail format surfaces as a diff here rather than as a
// silent shift in what the model is being asked to carry across.
//
// The detail is derived by deriveMissing as string(code) + ": " + key +
// " missing" (exception.go:807), and R1's code is verify.CodePolicyNumberConflict
// (verify.go:28).
var apa56CausalExpectedItem = invest.MissingItem{
	Kind:   invest.MissingField,
	Key:    apa56CausalKey,
	Detail: "POLICY_NUMBER_CONFLICT: policy_number missing",
}

// apa56CausalHistoricalDetail is the exact validator message the 17/17 measured
// live runs produced. It is asserted in the outcome gate so a recurrence is
// named in the failure rather than left for a reader to recognise.
const apa56CausalHistoricalDetail = `unknown external source "policy_number"`

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// apa56CausalClaim returns the canonical claim. policyNumberMissing selects the
// two arms of the offline mutation: TRUE derives the missing_evidence item,
// FALSE (a CONFLICT field) derives nothing. Every other input is held constant,
// so the pair differs in exactly the one fact under test.
func apa56CausalClaim(policyNumberMissing bool) assemble.CanonicalClaim {
	field := assemble.AssembledField{Key: apa56CausalKey, Status: assemble.StatusConflict}
	if policyNumberMissing {
		field = assemble.AssembledField{Key: apa56CausalKey, Status: assemble.StatusMissing}
	}
	return assemble.CanonicalClaim{
		Fields: map[string]assemble.AssembledField{
			apa56CausalKey: field,
			// One agreed field, so the envelope carries an AgreedSnapshot entry
			// and a report may carry a read-only fact_ref against it. Without it
			// no hypothesis could cite anything and the fixture would measure a
			// second, unrelated failure.
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
		// All three required documents present, so deriveMissing raises NO
		// required_document item. The fixture therefore isolates ONE missing
		// condition: the claim field. A second kind in the envelope would make
		// "did the model carry the field item across" ambiguous.
		DocsPresent: map[string]bool{
			verify.DocClaimForm: true, verify.DocDischargeSummary: true, verify.DocHospitalBill: true,
		},
		DocTypes: []string{verify.DocClaimForm, verify.DocDischargeSummary, verify.DocHospitalBill},
		DocIDs:   []string{"doc-01", "doc-02"},
	}
}

// apa56CausalResult is the R1 rule finding. R1 projects to AffectedFields
// ["policy_number"] (exception.go:86), which is what makes deriveMissing emit a
// MissingField item for that key. No policy-source evidence row is pinned:
// R1 does not consult it, and pinning one would not add a missing item either,
// so its absence keeps the evidence set minimal without changing the
// derivation.
func apa56CausalResult() verify.Result {
	return verify.Result{
		Passed: false,
		Exceptions: []verify.Exception{{
			Code:        verify.CodePolicyNumberConflict,
			Severity:    verify.SeverityHigh,
			Message:     "Claim policy number does not match policy number.",
			EvidenceIDs: []string{"ev-doc-01"},
		}},
	}
}

// apa56CausalBuild returns the envelope for the selected arm. It reuses the
// APA-58 document evidence rows and unresolved-conflict shape so the fixture
// differs from the sibling scenarios in the ONE respect that matters here, and
// builds through psBuildParams so the envelope is validated by the same
// constructor, with the same coupled deadline, as every other APA fixture.
func apa56CausalBuild(t *testing.T, policyNumberMissing bool) invest.UnresolvedException {
	t.Helper()
	env := psBuildParams(t,
		apa56CausalClaim(policyNumberMissing),
		psConflictUnresolved(),
		psDocEvidence(),
		apa56CausalResult(),
	)
	if policyNumberMissing {
		return env
	}
	// The mutation arm asserts its OWN premise here rather than in a premise
	// callback, because its whole purpose is to be the negative control: it
	// must derive NO missing evidence, or the differential proof below proves
	// nothing.
	if n := len(env.MissingEvidence); n != 0 {
		t.Fatalf("the mutation arm must derive ZERO missing_evidence items so the differential "+
			"proof has a negative control; got %d: %v", n, env.MissingEvidence)
	}
	return env
}

// apa56CausalFixture is the measured fixture: the arm that derives the item.
func apa56CausalFixture(t *testing.T) (invest.UnresolvedException, investigate.Scope) {
	t.Helper()
	env := apa56CausalBuild(t, true)
	return env, psScopeFor(env)
}

// ---------------------------------------------------------------------------
// Premise — asserted BEFORE any provider call
// ---------------------------------------------------------------------------

// apa56CausalPremise is the pre-flight gate, in the style of the APA-58
// psPremise* callbacks. It is deliberately a pure function of the envelope: it
// performs no inference, opens no socket and reads no credential, so a fixture
// that has drifted fails offline and free instead of costing a run.
//
// Every clause is an assertion about an AUTHORITY, not about this fixture's own
// preferences. If an authority moves, this fails and names which one.
func apa56CausalPremise(t *testing.T, env invest.UnresolvedException, scope investigate.Scope) {
	t.Helper()

	// (1) THE ITEM EXISTS, AND IT IS THE ONE ITEM. "At minimum a policy_number
	// entry" would be satisfied by an envelope carrying ten unrelated gaps and
	// the field among them; then a model that carried only the field would
	// still be denied and the run would measure the wrong thing. Exactly one is
	// what makes the causal question single-valued.
	if len(env.MissingEvidence) != 1 {
		t.Fatalf("causal fixture must derive EXACTLY ONE missing_evidence item, so the causal "+
			"question is single-valued; got %d: %v", len(env.MissingEvidence), env.MissingEvidence)
	}
	got := env.MissingEvidence[0]

	// (2) IT MATCHES THE SPELLED-OUT EXPECTATION, verbatim. A drift in
	// deriveMissing, in the rule-code spelling, or in the detail format shows up
	// here as a field-level diff rather than as a mysterious live result.
	if got != apa56CausalExpectedItem {
		t.Fatalf("derived missing_evidence item\n  got  %#v\n  want %#v\nThe detail string is part "+
			"of the obligation: checkMissingAdditive matches the whole struct, so a different "+
			"detail is a different item and would be rejected as a dropped envelope item",
			got, apa56CausalExpectedItem)
	}

	// (3) THE KIND IS IN THE CLOSED SET, and is the field kind.
	switch got.Kind {
	case invest.MissingRequiredDocument, invest.MissingField, invest.MissingExternal:
	default:
		t.Fatalf("derived kind %q is outside the closed missing-kind set; the contract this "+
			"fixture exercises does not exist for it", string(got.Kind))
	}
	if got.Kind != invest.MissingField {
		t.Fatalf("derived kind is %q, want %q. The correction under test is precisely that this "+
			"claim FIELD is kind \"field\"; if it derives as anything else the fixture no longer "+
			"exercises APA-56", string(got.Kind), string(invest.MissingField))
	}

	// (4) THE KEY IS A CLAIM FIELD KEY, and provably NOT an external source.
	// This is the exact distinction APA-56 turns on, asserted against the
	// authority rather than against prose.
	if got.Key != apa56CausalKey {
		t.Fatalf("derived key %q, want %q", got.Key, apa56CausalKey)
	}
	for _, ext := range invest.ExternalSourceKeys() {
		if got.Key == ext {
			t.Fatalf("derived key %q is in the closed EXTERNAL vocabulary %v; that is the defect "+
				"APA-56 fixed, and it is the shape the model historically produced",
				got.Key, invest.ExternalSourceKeys())
		}
	}

	// (5) THE KEY IS THE ONE R1 PROJECTS TO. Read from invest rather than
	// retyped, so the fixture cannot drift from the authority that decides which
	// field an R1 finding concerns.
	aff, err := invest.AffectedFields(invest.RulePolicyNumberConflict)
	if err != nil {
		t.Fatalf("invest.AffectedFields(R1): %v", err)
	}
	found := false
	for _, k := range aff {
		if k == got.Key {
			found = true
		}
	}
	if !found {
		t.Fatalf("derived key %q is not in R1's projected affected fields %v; the fixture is no "+
			"longer deriving its item from the rule finding it declares", got.Key, aff)
	}

	// (6) TENANT / CLAIM / REQUEST ECHO. Every identity in the scope matches the
	// envelope, so no read can cross the tenant line and the request id
	// propagated into tool requests is the envelope's.
	if scope.TenantID != env.TenantID || scope.ClaimID != env.ClaimID {
		t.Fatalf("scope identity does not echo the envelope: scope=(%q,%q) envelope=(%q,%q)",
			scope.TenantID, scope.ClaimID, env.TenantID, env.ClaimID)
	}
	if scope.RequestID != env.Scope.RequestID {
		t.Fatalf("scope request id %q does not echo the envelope's %q",
			scope.RequestID, env.Scope.RequestID)
	}
	if env.Scope.TenantID != env.TenantID || env.Scope.ClaimID != env.ClaimID {
		t.Fatalf("envelope scope constraints do not echo the envelope identity: "+
			"scope=(%q,%q) envelope=(%q,%q)",
			env.Scope.TenantID, env.Scope.ClaimID, env.TenantID, env.ClaimID)
	}

	// (7) LEAST PRIVILEGE: the allowlist is a non-empty subset the loop will
	// enforce, and it is present on BOTH sides of the coupling.
	if len(scope.AllowTools) == 0 || len(env.Scope.AllowTools) == 0 {
		t.Fatalf("allow-tools must be a non-empty subset on both sides: scope=%v envelope=%v",
			scope.AllowTools, env.Scope.AllowTools)
	}
	if len(scope.AllowTools) != len(env.Scope.AllowTools) {
		t.Fatalf("scope and envelope allow-tools differ in size: scope=%v envelope=%v",
			scope.AllowTools, env.Scope.AllowTools)
	}

	// (8) EVIDENCE PROVENANCE IS VALID AND TENANT-SCOPED. Every pinned row must
	// echo the envelope's tenant and claim, so nothing a report cites can have
	// come from another tenant, and every row must carry a trimmed id (the
	// property SeedKnownEvidence enforces, asserted here so a drift fails before
	// a run rather than inside it).
	if len(env.EvidenceRefs) == 0 {
		t.Fatal("causal fixture pins NO evidence; a report could then not be grounded at all and " +
			"the fixture would measure an unrelated failure")
	}
	for i := range env.EvidenceRefs {
		row := &env.EvidenceRefs[i]
		if row.TenantID != env.TenantID || row.ClaimID != env.ClaimID {
			t.Fatalf("evidence_refs[%d] (%q) does not echo the envelope identity (%q,%q); "+
				"foreign provenance must not reach the model context",
				i, row.EvidenceID, env.TenantID, env.ClaimID)
		}
		if strings.TrimSpace(row.EvidenceID) == "" || row.EvidenceID != strings.TrimSpace(row.EvidenceID) {
			t.Fatalf("evidence_refs[%d] needs a trimmed non-blank evidence id, which is what "+
				"SeedKnownEvidence requires: %q", i, row.EvidenceID)
		}
	}

	// (9) THE DEADLINE IS THE COUPLED PAIR, and it is in the right regime.
	// Only the scope governs the run (loop.go:502), and only the envelope bounds
	// the scope (loop.go:295), so both must carry the value or the run silently
	// measures at testScope's 60000.
	if env.Scope.DeadlineMs != apa56CausalDeadlineMs || scope.DeadlineMs != apa56CausalDeadlineMs {
		t.Fatalf("deadline coupling broken: envelope=%d scope=%d, want both %d. Raising only the "+
			"envelope would leave the run at testScope's 60000 and nothing would fail",
			env.Scope.DeadlineMs, scope.DeadlineMs, apa56CausalDeadlineMs)
	}
	apa56CausalPremiseDeadlineHolds(t)

	// (10) The envelope itself validates. A fixture that only works by accident
	// makes every downstream assertion meaningless.
	if err := invest.Validate(env); err != nil {
		t.Fatalf("causal fixture envelope is not valid: %v", err)
	}
	if err := scope.Validate(); err != nil {
		t.Fatalf("causal fixture scope is not valid: %v", err)
	}
	t.Logf("APA56-PREMISE ok missing_evidence=[{%s,%q,%q}] deadline=%d scope=%d "+
		"allowTools=%v evidenceRefs=%d",
		string(got.Kind), got.Key, got.Detail,
		env.Scope.DeadlineMs, scope.DeadlineMs, scope.AllowTools, len(env.EvidenceRefs))
}

// apa56CausalPremiseDeadlineHolds asserts the deadline is in the regime the
// rationale claims, numerically, against the measured pacing figure and against
// the harness's own context ceiling. It is a function of constants only, so it
// runs offline.
func apa56CausalPremiseDeadlineHolds(t *testing.T) {
	t.Helper()
	threeTurns := 3 * apa56CausalMeasuredPacingPerTurnMs
	if apa56CausalDeadlineMs <= threeTurns {
		t.Fatalf("deadline %dms does not exceed the measured three-turn pacing total %dms "+
			"(%dms per turn x 3); the turn-3 report would be discarded by the I2 post-Complete "+
			"deadline check and the run would measure ESCALATED(DEADLINE)",
			apa56CausalDeadlineMs, threeTurns, apa56CausalMeasuredPacingPerTurnMs)
	}
	if apa56CausalDeadlineMs >= apa56CausalLiveContextCeilingMs {
		t.Fatalf("deadline %dms is at or above the harness context ceiling %dms; the context "+
			"preempts the loop and the run yields an EMPTY output with a raw context error, "+
			"which is strictly worse than a well-formed ESCALATED(DEADLINE)",
			apa56CausalDeadlineMs, apa56CausalLiveContextCeilingMs)
	}
	headroom := apa56CausalLiveContextCeilingMs - apa56CausalDeadlineMs
	t.Logf("APA56-DEADLINE %dms: clears the measured three-turn paced total %dms by %dms and "+
		"stays %dms under the %dms context ceiling",
		apa56CausalDeadlineMs, threeTurns, apa56CausalDeadlineMs-threeTurns,
		headroom, apa56CausalLiveContextCeilingMs)
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

// apa56CausalPrompt renders the model-facing prompt for an envelope exactly as
// the loop would on turn 1, and returns it with its sha256.
func apa56CausalPrompt(t *testing.T, env invest.UnresolvedException, scope investigate.Scope) (string, string) {
	t.Helper()
	p, err := RenderPrompt(ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: KnownIDs(mustSeed(t, env)),
		Turn:             1,
		RequestID:        scope.RequestID,
	})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	sum := sha256.Sum256([]byte(p))
	return p, hex.EncodeToString(sum[:])
}

// apa56CausalDataBlock returns the DATA section of a rendered prompt: the
// canonical request bytes the model actually reads. Everything the fixture
// seeds is asserted against THIS, never against the Go struct, because the
// model cannot see a struct.
func apa56CausalDataBlock(t *testing.T, prompt string) string {
	t.Helper()
	const marker = "\nDATA:\n"
	i := strings.LastIndex(prompt, marker)
	if i < 0 {
		t.Fatalf("rendered prompt carries no DATA block; the model-facing input this fixture " +
			"exists to shape is the DATA block, so its absence invalidates every premise here")
	}
	return prompt[i+len(marker):]
}

// apa56CausalItemJSON renders one MissingItem exactly as the DATA block carries
// it, by marshalling the authoritative struct with the same encoder the request
// canonicalisation uses.
func apa56CausalItemJSON(t *testing.T, m invest.MissingItem) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal MissingItem: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Offline anti-vacuity proofs
// ---------------------------------------------------------------------------

// TestAPA56Causal_PremiseHoldsBeforeAnyInference is the offline form of the
// premise gate. The live matrix calls apa56CausalPremise before it spends a
// provider call; this test calls the same function with no credential present,
// so the guarantee is proven on every commit rather than only when somebody
// remembers to run the matrix.
//
// It also asserts the premise is pure: calling it twice yields the same verdict
// and touches nothing outside the envelope and scope it is handed.
func TestAPA56Causal_PremiseHoldsBeforeAnyInference(t *testing.T) {
	env, scope := apa56CausalFixture(t)

	apa56CausalPremise(t, env, scope)
	// Idempotent: a premise that mutated its input could pass once and fail the
	// second time, and the live matrix calls it once per scenario.
	before := env
	apa56CausalPremise(t, env, scope)
	if len(env.MissingEvidence) != len(before.MissingEvidence) {
		t.Fatalf("the premise mutated the envelope it was handed: missing_evidence went from "+
			"%d to %d item(s)", len(before.MissingEvidence), len(env.MissingEvidence))
	}

	// The authority, re-read here so a drift in the closed external vocabulary
	// is reported against this fixture too.
	t.Logf("APA56-OFFLINE premise holds; external vocabulary is %v and the seeded key %q is "+
		"not among it", invest.ExternalSourceKeys(), apa56CausalKey)
}

// TestAPA56Causal_MissingEvidenceReachesTheModelFacingDataBlock is the first
// anti-vacuity proof, and it is the one the pre-APA-56 envelope failed.
//
// The claim under test is that the model is actually SHOWN the item. Asserting
// `len(env.MissingEvidence) == 1` would prove only that a Go slice is
// populated. The model never sees a Go slice; it sees rendered bytes. So the
// item's exact JSON is located inside the rendered DATA block, and the location
// is the assertion.
//
// The NEGATIVE case is proved in the same test, from the mutation arm: the one
// input this fixture changes (policy_number MISSING -> CONFLICT) removes the
// item from the envelope AND from the rendered DATA block. Without that pair,
// "the item appears in the prompt" could be true because it is always in the
// prompt — as prose, as an example — and the fixture would prove nothing.
func TestAPA56Causal_MissingEvidenceReachesTheModelFacingDataBlock(t *testing.T) {
	env, scope := apa56CausalFixture(t)
	prompt, sum := apa56CausalPrompt(t, env, scope)
	data := apa56CausalDataBlock(t, prompt)
	item := apa56CausalItemJSON(t, apa56CausalExpectedItem)

	// GREEN: the exact item, byte-for-byte, inside the DATA the model reads.
	if !strings.Contains(data, item) {
		t.Fatalf("the seeded missing_evidence item is NOT in the model-facing DATA block.\n"+
			"item JSON: %s\nrendered prompt sha256: %s\nThe model is never shown this obligation "+
			"if it does not reach the prompt, and the pre-APA-56 live envelope derived no items "+
			"at all — which is precisely why the model was never given the fact it got wrong",
			item, sum)
	}
	t.Logf("APA56-OFFLINE the seeded item is present in the rendered DATA block: %s", item)

	// It must appear inside the "missing_evidence" ARRAY specifically, not merely
	// somewhere in the DATA block. An item quoted under some other key would
	// satisfy the Contains above while leaving the envelope's own array empty.
	const arrayKey = `"missing_evidence":[`
	ai := strings.Index(data, arrayKey)
	if ai < 0 {
		t.Fatalf("rendered DATA block carries no %s array; the envelope's open questions are "+
			"carried to the model under that key", arrayKey)
	}
	end := strings.Index(data[ai:], "]")
	if end < 0 {
		t.Fatal(`rendered DATA block has an unterminated "missing_evidence" array`)
	}
	array := data[ai : ai+end+1]
	if !strings.Contains(array, item) {
		t.Fatalf("the seeded item is in the DATA block but NOT inside its missing_evidence "+
			"array.\narray: %s\nitem: %s", array, item)
	}
	t.Logf("APA56-OFFLINE missing_evidence array as the model reads it: %s", array)

	// NEGATIVE: the mutation arm derives nothing, so the same JSON must be
	// absent from ITS DATA block. This is what makes the positive assertion
	// mean something.
	mutEnv := apa56CausalBuild(t, false)
	mutPrompt, mutSum := apa56CausalPrompt(t, mutEnv, psScopeFor(mutEnv))
	mutData := apa56CausalDataBlock(t, mutPrompt)
	if strings.Contains(mutData, item) {
		t.Fatalf("the mutation arm (policy_number CONFLICT, which derives nothing) still renders "+
			"the item in its DATA block.\nitem: %s\nmutation prompt sha256: %s\nIf both arms render "+
			"the same item, the positive assertion above proves nothing about the derivation",
			item, mutSum)
	}
	// And the arms must differ in the prompt as a whole, not only in the item.
	if mutSum == sum {
		t.Fatalf("the two arms render a byte-identical prompt (sha256 %s); the mutation did not "+
			"change the model-facing input at all", sum)
	}
	t.Logf("APA56-OFFLINE mutation arm (no derived item) renders sha256 %s and does NOT carry %s; "+
		"measured arm renders sha256 %s and does", mutSum, item, sum)
}

// TestAPA56Causal_OmissionIsRejectedAtGrounding is the necessity proof for the
// OMITTED case, exercised through the real loop with a scripted model.
//
// The claim: a report that IGNORES the envelope's missing_evidence cannot reach
// REPORT_READY. Without this the fixture measures nothing — the model would
// pass whether or not it read the correction.
//
// The mechanism, read rather than assumed:
// grounding.go:183-199 checkMissingAdditive builds a set from the submitted
// list and requires every envelope item to be a member. Because MissingItem is
// compared as a whole struct, membership means (kind, key, detail) all match.
// loop.go:630 calls it, and loop.go:632 turns the failure into a terminal
// ESCALATED(INVALID_OUTPUT): repromptable (errors.go:108) admits only I1/I2/I7,
// and invalidGrounding is none of them.
//
// The GREEN half runs first, so a broken assertion below cannot be mistaken for
// containment: on the SAME fixture, a report that DOES carry the item reaches
// REPORT_READY.
func TestAPA56Causal_OmissionIsRejectedAtGrounding(t *testing.T) {
	env, scope := apa56CausalFixture(t)
	apa56CausalPremise(t, env, scope)

	t.Run("carrying_the_item_is_GREEN", func(t *testing.T) {
		out, runErr := apa56CausalRunLoop(t, env, scope, submitBytes(t, apa56CausalCarriedReport(t, env)))
		if out.Outcome != OutcomeReportReady {
			t.Fatalf("a report that CARRIES the envelope item did not reach REPORT_READY: "+
				"outcome=%q reason=%q. Without this GREEN half the RED half below would prove "+
				"nothing — it could just mean the fixture cannot be satisfied at all",
				out.Outcome, out.EscalationReason)
		}
		if runErr != nil {
			t.Fatalf("a REPORT_READY run must carry no classification error; got %v", runErr)
		}
		if len(out.Report.MissingAdditive) != 1 {
			t.Fatalf("accepted report carries %d missing_additive item(s), want 1",
				len(out.Report.MissingAdditive))
		}
		if out.Report.MissingAdditive[0] != apa56CausalExpectedItem {
			t.Fatalf("accepted report did not carry the envelope item verbatim: got %#v want %#v",
				out.Report.MissingAdditive[0], apa56CausalExpectedItem)
		}
		t.Logf("APA56-OFFLINE GREEN: carried report accepted, missing_additive=%v",
			out.Report.MissingAdditive)
	})

	t.Run("omitting_the_item_is_RED", func(t *testing.T) {
		rep := testReport(env)
		rep.MissingAdditive = []invest.MissingItem{} // the model ignores the obligation
		out, runErr := apa56CausalRunLoop(t, env, scope, submitBytes(t, rep))

		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("a report that OMITS the envelope's missing_evidence was not rejected: "+
				"outcome=%q reason=%q. The fixture's obligation is then not necessary and the "+
				"model could pass by ignoring it, which is exactly what this fixture must not allow",
				out.Outcome, out.EscalationReason)
		}
		if out.Report != nil {
			t.Fatal("an escalated run must carry no report")
		}
		// WHICH boundary rejected it. The loop reports INVALID_OUTPUT for both a
		// validator failure and a grounding failure, so the escalation alone does
		// not identify the gate; the I-class and the message do. This is what makes
		// the omission claim a statement about grounding.go:183 rather than a
		// statement about "the loop said no".
		if k := invalidKindOf(runErr); k != invalidGrounding {
			t.Fatalf("omission was not rejected by the grounding boundary: I-class %s (%v). If a "+
				"future change let omission through the validator and stopped it somewhere else, "+
				"this claim would silently become about a different boundary",
				invalidKindString(k), runErr)
		}
		if !strings.Contains(runErr.Error(), `drops envelope item "policy_number" (additive only)`) {
			t.Fatalf("omission was rejected, but not by the additive-superset rule: %v\nwant a "+
				"message naming the dropped envelope item", runErr)
		}
		t.Logf("APA56-OFFLINE RED: omission contained as ESCALATED(%s) at %s: %v",
			out.EscalationReason, invalidKindString(invalidKindOf(runErr)), runErr)
	})

	// The differential, which is what makes both halves above informative. The
	// SAME item-less report is ACCEPTED on the mutation arm, whose envelope
	// derives no open questions. So the rejection above is caused by the
	// envelope's missing evidence and by nothing else in the fixture.
	t.Run("the_same_item_less_report_is_accepted_where_no_item_is_owed", func(t *testing.T) {
		mutEnv := apa56CausalBuild(t, false)
		mutScope := psScopeFor(mutEnv)
		rep := testReport(mutEnv)
		if len(rep.MissingAdditive) != 0 {
			t.Fatalf("the mutation arm's report should owe nothing; got %v", rep.MissingAdditive)
		}
		out, runErr := apa56CausalRunLoop(t, mutEnv, mutScope, submitBytes(t, rep))
		if runErr != nil {
			t.Fatalf("an accepted run must carry no classification error; got %v", runErr)
		}
		if out.Outcome != OutcomeReportReady {
			t.Fatalf("on the envelope that derives NO missing evidence, an item-less report was "+
				"still rejected (outcome=%q reason=%q). The differential proof requires that this "+
				"be accepted, or the RED half above is not attributable to the envelope's item",
				out.Outcome, out.EscalationReason)
		}
		t.Logf("APA56-OFFLINE differential: identical item-less report ACCEPTED where " +
			"missing_evidence is empty, so the obligation is created by the envelope alone")
	})
}

// TestAPA56Causal_WrongKindIsRejectedByTheValidator is the necessity proof for
// the WRONG-KIND case — the historically-failing shape.
//
// Two layers, deliberately:
//
//	direct  DecodeModelAction + ValidateModelAction, asserting the exact
//	        historical message. This pins WHICH boundary rejects it, which the
//	        loop alone cannot tell you: the loop surfaces the same
//	        ESCALATED(INVALID_OUTPUT) for a grounding failure and a validator
//	        failure, so a loop-only assertion would let a future refactor move
//	        the rejection from the validator to grounding without this test
//	        noticing.
//	loop    the real loop with a scripted model, proving the run cannot succeed
//	        on that shape either.
//
// The wrong-kind item deliberately uses the correct KEY. A test that got the
// key wrong too would pass for the wrong reason: the key is not what the
// validator objects to, the KIND paired with a non-external key is.
func TestAPA56Causal_WrongKindIsRejectedByTheValidator(t *testing.T) {
	env, scope := apa56CausalFixture(t)
	apa56CausalPremise(t, env, scope)

	historical := invest.MissingItem{
		Kind:   invest.MissingExternal,
		Key:    apa56CausalKey,
		Detail: "R1: no pinned policy evidence",
	}

	t.Run("validator_names_the_historical_message", func(t *testing.T) {
		rep := testReport(env)
		rep.MissingAdditive = []invest.MissingItem{historical}
		raw := submitBytes(t, rep)
		a, err := DecodeModelAction(raw, 1<<20)
		if err != nil {
			t.Fatal("rejected at the decoder, which is also containment: ", err)
		}
		verr := ValidateModelAction(a, scope, env.InvestigationID)
		if verr == nil {
			t.Fatalf("kind %q key %q was ACCEPTED; the validator is the containment boundary for "+
				"this shape and must never be weakened into accepting it",
				string(historical.Kind), historical.Key)
		}
		if !strings.Contains(verr.Error(), apa56CausalHistoricalDetail) {
			t.Fatalf("rejected, but for the wrong reason: %v\nwant a message containing %s — that "+
				"exact string is the evidence in APA-55, so a different message means this test is "+
				"no longer measuring the historical failure",
				verr, apa56CausalHistoricalDetail)
		}
		t.Logf("APA56-OFFLINE wrong kind rejected by the validator: %v", verr)
	})

	t.Run("the_loop_cannot_succeed_on_it", func(t *testing.T) {
		rep := testReport(env)
		rep.MissingAdditive = []invest.MissingItem{historical}
		out, runErr := apa56CausalRunLoop(t, env, scope, submitBytes(t, rep))
		if out.Outcome != OutcomeEscalated || out.EscalationReason != EscalationInvalidOutput {
			t.Fatalf("the historically-failing shape was not contained by the loop: outcome=%q "+
				"reason=%q. Either the item was silently corrected or the boundary let it through",
				out.Outcome, out.EscalationReason)
		}
		if !strings.Contains(runErr.Error(), apa56CausalHistoricalDetail) {
			t.Fatalf("the loop rejected the shape for a different reason: %v\nwant a message "+
				"containing %s", runErr, apa56CausalHistoricalDetail)
		}

		// A MEASUREMENT CAVEAT, recorded here because it is the failure mode
		// this whole scenario exists to detect.
		//
		// The loop does NOT preserve the rejected report for a VALIDATOR
		// rejection. loop.go:615 calls submitPartial, and submitPartial
		// (loop.go:343-345) yields nil whenever ValidateReport fails — which is
		// exactly what a wrong external key does, since validateMissingAdditive is
		// part of ValidateReport. Contrast a GROUNDING rejection (loop.go:630),
		// which passes the report straight through and does populate Partial.
		//
		// So for the historically-failing shape the only in-band evidence of WHAT
		// the model emitted is the error message and the verbatim model payload.
		// The live measurement reads both, and this log records the asymmetry
		// rather than leaving a reader to assume Partial is always populated. If
		// this ever starts reporting a Partial, the containment did not change and
		// the measurement simply got easier.
		if out.Partial == nil {
			t.Logf("APA56-OFFLINE caveat: a validator rejection carries NO Partial, so the rejected "+
				"shape is observable only through the error message and the verbatim model payload. "+
				"The causal measurement reads both: %v", runErr)
		} else {
			if got := out.Partial.MissingAdditive; len(got) != 1 || got[0] != historical {
				t.Fatalf("Partial carries something other than the rejected item: %v", got)
			}
			t.Logf("APA56-OFFLINE the rejected shape was additionally preserved as Partial: %v",
				out.Partial.MissingAdditive)
		}
	})

	// The converse, so the test cannot pass because the KEY is also wrong: the
	// same key with the CORRECT kind survives the same three layers. This is
	// already pinned by apa56_additive_contract_test.go; it is repeated here as
	// the differential that gives the rejection above its meaning.
	t.Run("the_correct_kind_on_the_same_key_survives", func(t *testing.T) {
		rep := testReport(env)
		rep.MissingAdditive = []invest.MissingItem{apa56CausalExpectedItem}
		raw := submitBytes(t, rep)
		a, err := DecodeModelAction(raw, 1<<20)
		if err != nil {
			t.Fatalf("decode rejected the corrected additive: %v", err)
		}
		if err := ValidateModelAction(a, scope, env.InvestigationID); err != nil {
			t.Fatalf("ValidateModelAction rejected kind %q key %q: %v",
				string(apa56CausalExpectedItem.Kind), apa56CausalExpectedItem.Key, err)
		}
		known, err := SeedKnownEvidence(env)
		if err != nil {
			t.Fatalf("SeedKnownEvidence: %v", err)
		}
		if err := CheckReportGrounding(*a.Report, known, env); err != nil {
			t.Fatalf("grounding rejected kind %q key %q: %v",
				string(apa56CausalExpectedItem.Kind), apa56CausalExpectedItem.Key, err)
		}
		t.Logf("APA56-OFFLINE same key + correct kind survives decode, validate and grounding")
	})
}

// TestAPA56Causal_ExpectedActionCannotComeFromAMisconfiguredFixture is the
// explicit mutation RED/GREEN statement, separated from the boundary proofs so
// a reader can see at a glance what makes this fixture non-vacuous.
//
// The claim, in one line: the expected action is UNAVAILABLE to a model that is
// handed a fixture without the obligation. Three mutations, each isolating one
// link:
//
//	no MissingEvidence   the model is never told what to carry, and an
//	                     item-less report is then CORRECT — so a model that
//	                     submits one has not demonstrated anything.
//	omitted from report  rejected at grounding (I6).
//	wrong kind           rejected by the validator (ErrModelContract).
//
// The first is the anti-vacuity core; the other two are the containment the
// causal claim rests on.
func TestAPA56Causal_ExpectedActionCannotComeFromAMisconfiguredFixture(t *testing.T) {
	t.Run("a_fixture_with_no_missing_evidence_owes_nothing", func(t *testing.T) {
		mutEnv := apa56CausalBuild(t, false)
		mutScope := psScopeFor(mutEnv)

		// Nothing is owed, so nothing can be carried. If the model could
		// "produce the expected action" here, the expectation would be
		// available without the fixture, and the measured arm would be
		// indistinguishable from an empty one.
		if len(mutEnv.MissingEvidence) != 0 {
			t.Fatalf("mutation arm owes %d item(s): %v", len(mutEnv.MissingEvidence), mutEnv.MissingEvidence)
		}
		out, runErr := apa56CausalRunLoop(t, mutEnv, mutScope, submitBytes(t, testReport(mutEnv)))
		if runErr != nil {
			t.Fatalf("an accepted run must carry no classification error; got %v", runErr)
		}
		if out.Outcome != OutcomeReportReady {
			t.Fatalf("an item-less report was rejected where nothing is owed (outcome=%q reason=%q); "+
				"the mutation is not isolating the obligation", out.Outcome, out.EscalationReason)
		}
		if len(out.Report.MissingAdditive) != 0 {
			t.Fatalf("accepted report invented %d missing_additive item(s) where none was owed: %v",
				len(out.Report.MissingAdditive), out.Report.MissingAdditive)
		}
		t.Logf("APA56-OFFLINE mutation: a fixture owing nothing accepts an empty list, so the " +
			"measured arm's obligation is not available to a misconfigured fixture")
	})

	t.Run("the_measured_fixture_rejects_what_the_mutation_accepts", func(t *testing.T) {
		env, scope := apa56CausalFixture(t)
		out, _ := apa56CausalRunLoop(t, env, scope, submitBytes(t, apa56CausalEmptyAdditive(t, env)))
		if out.Outcome == OutcomeReportReady {
			t.Fatalf("the measured fixture ACCEPTED the very report the mutation fixture accepted; " +
				"the two envelopes are then indistinguishable on the model-facing obligation and the " +
				"fixture is vacuous")
		}
		t.Logf("APA56-OFFLINE the measured fixture rejects the mutation-accepted report: "+
			"outcome=%q reason=%q", out.Outcome, out.EscalationReason)
	})
}

// TestAPA56Causal_PromptIsDistinctFromEveryOtherFixture is the anti-duplication
// gate, in the shape TestAPA58_SeededScenariosAreDistinct established.
//
// Two fixtures with different names and different post-run assertions but one
// byte-identical model-facing prompt are ONE scenario wearing two labels. That
// is the defect APA-58 was filed for, and it recurred once already. This test
// runs offline on every commit, so the property does not depend on somebody
// remembering to run a live matrix.
//
// It compares against BOTH families: the five APA-58 scenarios AND the
// APA-59 production-shaped prompt. The second comparison is the reason this
// fixture's recorded prompt hash is a NEW measurement: that anchor is a
// DIFFERENT envelope (rendered at the production deadline, with an empty
// known-evidence set, to force tool mediation). Asserting distinctness here is
// what proves the two cannot be confused, rather than merely asserting it —
// and it leaves that anchor untouched and independent.
func TestAPA56Causal_PromptIsDistinctFromEveryOtherFixture(t *testing.T) {
	env, scope := apa56CausalFixture(t)
	apa56CausalPremise(t, env, scope)
	_, sum := apa56CausalPrompt(t, env, scope)

	seen := map[string]string{sum: "apa56_causal"}

	for _, sc := range psAllScenarios() {
		otherEnv, otherScope, _ := sc.build(t)
		_, otherSum := apa56CausalPrompt(t, otherEnv, otherScope)
		if prior, dup := seen[otherSum]; dup {
			t.Fatalf("APA-56 causal fixture renders a prompt IDENTICAL to %q (sha256 %s).\n"+
				"Distinct names and distinct assertions do not make distinct scenarios: the model "+
				"would receive the same bytes for both, and the causal claim would be measuring "+
				"another fixture's condition.", prior, otherSum)
		}
		seen[otherSum] = sc.name
		t.Logf("APA56-DISTINCT vs %-22s %s", sc.name, otherSum[:12])
	}

	// The APA-59 anchor, compared as bytes and stated as the reason the two
	// measurements are independent. Rendered from the SAME helper its own gate
	// renders from, so this comparison is against the bytes APA-59 measures and
	// not against a re-derivation of them.
	apa59Prompt, err := RenderPrompt(apa59MeasuredRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt (APA-59 production-shaped request): %v", err)
	}
	apa59Sum := sha256.Sum256([]byte(apa59Prompt))
	apa59Hex := hex.EncodeToString(apa59Sum[:])
	if prior, dup := seen[apa59Hex]; dup {
		t.Fatalf("APA-56 causal fixture renders a prompt IDENTICAL to %q (sha256 %s)", prior, apa59Hex)
	}
	seen[apa59Hex] = "apa59_production_shaped"
	if len(seen) != 2+len(psAllScenarios()) {
		t.Fatalf("collected %d distinct prompts for %d fixtures (APA-58 scenarios, the causal "+
			"fixture, and the APA-59 production-shaped prompt); the distinctness set is wrong",
			len(seen), 2+len(psAllScenarios()))
	}

	t.Logf("APA56-DISTINCT this fixture's prompt sha256=%s", sum)
	t.Logf("APA56-DISTINCT the APA-59 production-fidelity anchor %s belongs to a DIFFERENT "+
		"envelope (production deadline, empty known-evidence set) and is NOT a gate on this "+
		"measurement; distinctness here proves the two records cannot be confused",
		frozenAPA59BPromptSHA256)
}

// ---------------------------------------------------------------------------
// Offline loop helper
// ---------------------------------------------------------------------------

// apa56CausalRunLoop drives the REAL loop once against a scripted model that
// answers with raw on every turn, and returns BOTH the loop's output and the
// error it classified that output with.
//
// The error is NOT ignored. loop.go:519-525 fail() returns a well-formed
// ESCALATED output AND a non-nil classification error, so an escalation always
// carries one; treating a non-nil error as a fixture defect would hide every
// containment result this file exists to observe. The distinction that actually
// matters is made on the OUTPUT, which ValidateInvestigationOutput checks below:
// a raw abort (loop.go:648, :654, :705, :709) yields an EMPTY output, which
// fails validation, whereas a contained run yields a structured escalation.
//
// The real executor is attached so a tool call would be genuinely recorded, but
// the fixture's necessity is in the REPORT, not in a tool read, so the script
// submits directly. The context timeout never binds: the loop's own effective
// window is derived from scope.DeadlineMs, which is the value under test.
//
// Every turn answers identically, so a run that reaches REPORT_READY does so on
// turn 1 and a run that is contained is contained on the turn the contract says
// it should be. A looping run is therefore a fixture defect, not a tolerated
// outcome, and the turn count is logged either way.
func apa56CausalRunLoop(t *testing.T, env invest.UnresolvedException, scope investigate.Scope, raw []byte) (InvestigationOutput, error) {
	t.Helper()
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope: %v", err)
	}
	fake := &FakeModelClient{Responses: []ModelResponse{modelResp(raw), modelResp(raw)}}
	ex := newQualExecutor(successExecutor())
	lp, err := NewLoop(fake, ex.inner, DefaultBudgets(scope), scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, runErr := lp.Run(ctx)
	// A structured escalation is the expected shape of every contained run, so
	// the error is carried to the caller rather than fatal here. An EMPTY output
	// is the raw-abort shape and is a loop-construction or fixture bug, not a
	// result: fail loudly rather than reading empty fields as "contained".
	if err := ValidateInvestigationOutput(out); err != nil {
		t.Fatalf("loop produced an invalid output (%q / %q), so this is a raw abort rather than a "+
			"containment result: out=%#v runErr=%v", out.Outcome, out.EscalationReason, out, runErr)
	}
	if out.TurnsUsed != 1 {
		t.Errorf("run reached a decision on turn %d, not turn 1. The script answers identically on "+
			"every turn, so the fixture's necessity is settled by the first answer; a multi-turn "+
			"resolution means the model was given a chance to change its mind about the obligation",
			out.TurnsUsed)
	}
	t.Logf("APA56-LOOP outcome=%q reason=%q turns=%d modelCalls=%d recorderObserved=%d classified=%v",
		out.Outcome, out.EscalationReason, out.TurnsUsed, fake.Calls, ex.Observed(), runErr)
	return out, runErr
}

// apa56CausalCarriedReport is the GREEN report: the envelope's own item carried
// across verbatim, which is what the prompt instructs and what
// checkMissingAdditive requires. It is built from the ENVELOPE, not from a
// literal, so the test cannot pass by asserting against a constant that happens
// to match a broken derivation.
func apa56CausalCarriedReport(t *testing.T, env invest.UnresolvedException) Report {
	t.Helper()
	rep := testReport(env)
	rep.MissingAdditive = append([]invest.MissingItem(nil), env.MissingEvidence...)
	if len(rep.MissingAdditive) != 1 || rep.MissingAdditive[0] != apa56CausalExpectedItem {
		t.Fatalf("carried report does not carry the seeded item: %v", rep.MissingAdditive)
	}
	return rep
}

// apa56CausalEmptyAdditive is the omission: the same grounded report with the
// obligation dropped.
func apa56CausalEmptyAdditive(t *testing.T, env invest.UnresolvedException) Report {
	t.Helper()
	rep := apa56CausalCarriedReport(t, env)
	rep.MissingAdditive = []invest.MissingItem{}
	return rep
}

// ---------------------------------------------------------------------------
// Step 3 — the live measurement contract
// ---------------------------------------------------------------------------

// TestAPA56Causal_LiveMeasurement is the gate. It SKIPS without a credential
// and costs no quota when one is absent, exactly like the sibling matrices.
//
// It asserts the premise BEFORE any provider call, so a fixture that has
// drifted fails offline and free.
//
// WHY TOOL MEDIATION IS NOT REQUIRED HERE. apa55AssertExercised takes a
// requireToolMediation flag, and this scenario passes false. The flag exists
// for scenarios whose PREMISE makes a grounded report impossible without a
// tool read (APA-58's ps_b1_valid_tool, which pins ZERO citable evidence). This
// fixture is not that: it pins two document rows, so a report can be grounded
// from the envelope alone, and the whole obligation under test lives in the
// REPORT's missing_additive, which is reachable without any tool call.
// Demanding tool mediation would measure a different property — whether the
// model goes and looks — and would fail this scenario for a reason unrelated to
// APA-56. The anti-vacuity reasoning that DOES apply is reused: the run must
// end in a classified outcome, never a silent no-op, and that is the false arm
// of apa55AssertExercised.
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

// apa56CausalObs pairs one repetition's harness record with what the model
// ACTUALLY submitted.
//
// The pairing is the point. qualEvidence deliberately carries no report — it is
// the boundary record, and a boundary record that also carried model output
// would blur the two questions this package keeps separable. The causal claim,
// though, is a question about model output, so the submitted items are read
// off the run alongside the evidence record and judged together.
type apa56CausalObs struct {
	ev        qualEvidence
	items     []invest.MissingItem
	hasReport bool
}

// apa56CausalObserve reads the submitted missing_additive off one run.
//
// IT READS TWO SOURCES, BECAUSE ONE IS NOT ENOUGH FOR THE FAILURE THIS
// EXPERIMENT EXISTS TO DETECT:
//
//	Output.Report   the accepted report. Absent unless the run reached
//	                REPORT_READY.
//	Output.Partial  the report carried into an escalation. Present for a
//	                GROUNDING rejection (loop.go:630 passes the report
//	                straight through) but NOT for a VALIDATOR rejection, because
//	                submitPartial (loop.go:343-345) yields nil whenever
//	                ValidateReport fails and a wrong external key fails it. That
//	                is precisely the historically-failing shape, so the denial
//	                most worth studying carries no report.
//	ev.ErrorText    the loop's own classification message, which names the
//	                rejected pair. This is the ONLY in-band record of the
//	                historical shape.
//	ev.Payloads     the model output verbatim, captured at the client seam.
//	                The ground truth of what was emitted.
//
// The outcome gate therefore judges on Report when there is one and on
// ErrorText/Payloads when there is not. Anything that read only Report would
// record a denial with no cause and would call that a measurement.
func apa56CausalObserve(ev qualEvidence, r qualRun) apa56CausalObs {
	o := apa56CausalObs{ev: ev}
	switch {
	case r.Output.Report != nil:
		o.items, o.hasReport = r.Output.Report.MissingAdditive, true
	case r.Output.Partial != nil:
		o.items, o.hasReport = r.Output.Partial.MissingAdditive, true
	}
	return o
}

// apa56CausalLogEvidence writes the ONE evidence line for a repetition.
//
// It is the causal measurement, so it carries the observation that actually
// decides the claim — the model's own missing_additive — and not only the
// harness bookkeeping the sibling lines already record. requestedModelSource is
// printed beside requestedModel because a record that names the harness's
// intent next to a field called "model" is how a served-by-a-different-model
// run gets misread; the source says how strong the observation is.
//
// prompt_sha256 is recorded, not gated. This is a NEW measurement of a NEW
// envelope; the existing production-fidelity anchor belongs to a different
// fixture and comparing against it would be a category error. Recording it is
// what lets a later reader detect drift in THIS fixture.
func apa56CausalLogEvidence(t *testing.T, ev qualEvidence, r qualRun, promptSHA string) {
	t.Helper()

	// The decoded action class, and the item triple the causal claim is about.
	// Item-level detail is the whole point: "the run passed" cannot distinguish
	// the corrected shape from the historical one.
	//
	// source names WHICH slot the items came from, because the slots are not
	// equally populated: a validator rejection leaves Partial nil (see
	// apa56CausalObserve), and on exactly the historical failure that means
	// missing_additive reads "not-preserved" rather than "none-submitted". Those
	// two are different facts — one is a real submission, the other is a lost
	// record — and conflating them would let a denial hide behind an empty list.
	additive, source := "none-preserved", "no-report-no-partial"
	switch {
	case r.Output.Report != nil:
		additive, source = apa56CausalRenderItems(r.Output.Report.MissingAdditive), "accepted-report"
	case r.Output.Partial != nil:
		additive, source = apa56CausalRenderItems(r.Output.Partial.MissingAdditive), "escalation-partial"
	}

	// The model's own output, verbatim, bounded. This is the ground truth for a
	// run that was denied before any report survived, and it is the last field
	// on the line because it is long.
	payload := "none"
	if n := len(ev.Payloads); n > 0 {
		payload = ev.PayloadPrefixes[n-1]
	}

	t.Logf("APA56-CAUSAL scenario=%s repeat=%d requestedModel=%s requestedModelSource=%s "+
		"codeDefaultModel=%s provider=%s modelCalls=%d recorderObserved=%d actClass=%s "+
		"missing_additive=%s missing_additive_source=%s outcome=%q escalationReason=%q "+
		"verdict=%s violations=%d prompt_sha256=%s error=%q lastModelPayload=%q",
		ev.Scenario, ev.Repeat, ev.RequestedModel, ev.RequestedModelSource,
		ev.CodeDefaultModel, ev.Provider, ev.ModelCalls, ev.RecorderObserved,
		ev.ModelActClass, additive, source, ev.Outcome, ev.EscalationReason,
		ev.Verdict, len(ev.Violations), promptSHA, ev.ErrorText, payload)
}

// apa56CausalRenderItems renders a missing_additive list for the log, one item
// per entry with its kind, key and detail, so a reader can adjudicate the
// causal claim without opening the evidence file.
func apa56CausalRenderItems(items []invest.MissingItem) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(items))
	for _, m := range items {
		parts = append(parts, "{kind="+string(m.Kind)+",key="+m.Key+",detail="+m.Detail+"}")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// apa56CausalOutcome is the per-scenario gate, and it states the causal claim
// HONESTLY in both directions.
//
// The claim under test is one sentence: the real model, shown an envelope that
// genuinely carries MissingEvidence, now emits the CORRECT missing_additive
// action. This gate fails in three distinct ways, each named:
//
//  1. No run reached REPORT_READY. The demonstration did not happen. This is
//     NOT downgraded to a note: "the boundary held" on an escalating run says
//     nothing about whether the model was fixed, which is the question.
//  2. A run failed with the historical message. The model reproduced the exact
//     shape the 17/17 runs produced. Named explicitly, because this is the
//     failure the whole fixture exists to detect and it deserves to be
//     unmistakable in the log.
//  3. A submitted report carried the wrong kind for the seeded key, or omitted
//     it while still being accepted. Either means the report is not the action
//     under test.
//
// The prohibition is asymmetric on purpose: kind "external" with key
// "policy_number" is the historically-failing pair and is always a failure,
// whatever else the report contains.
func apa56CausalOutcome(t *testing.T, obs []apa56CausalObs) {
	t.Helper()
	if len(obs) == 0 {
		t.Fatal("no APA-56 causal runs were measured; the causal claim is unproven either way")
	}

	ready, corrected, historical, unreadable := 0, 0, 0, 0
	for _, o := range obs {
		ev := o.ev

		// (2) The historical shape, named wherever it appears.
		if strings.Contains(ev.ErrorText, apa56CausalHistoricalDetail) {
			historical++
			t.Errorf("APA-56 CAUSAL CLAIM NOT DEMONSTRATED (repeat %d): the model reproduced the "+
				"historically-failing shape and was denied by the validator — %s.\n"+
				"The prompt correction did not move the model on this envelope.",
				ev.Repeat, apa56CausalHistoricalDetail)
		}

		if ev.Outcome != string(OutcomeReportReady) {
			continue
		}
		ready++
		if !o.hasReport {
			unreadable++
			t.Errorf("repeat %d reached REPORT_READY with no readable report; the measurement "+
				"cannot judge what the model submitted", ev.Repeat)
			continue
		}

		correctKind := false
		badKind := false
		for _, m := range o.items {
			if m.Key != apa56CausalKey {
				continue
			}
			if m.Kind == invest.MissingField {
				correctKind = true
				continue
			}
			badKind = true
		}
		if badKind {
			t.Errorf("repeat %d: the submitted report pairs key %q with a kind that is not %q. "+
				"That is the historical failure, and the validator rejects it.", ev.Repeat,
				apa56CausalKey, string(invest.MissingField))
		}
		if !correctKind {
			t.Errorf("repeat %d: the submitted report does not carry kind %q key %q. The envelope "+
				"seeds that item and grounding requires it to survive verbatim, so this report "+
				"should not have been accepted.", ev.Repeat,
				string(invest.MissingField), apa56CausalKey)
		}
		if correctKind && !badKind {
			corrected++
		}
	}

	// (1) The demonstration itself.
	if ready == 0 {
		t.Errorf("APA-56 CAUSAL CLAIM NOT DEMONSTRATED: %d run(s) measured and 0 reached "+
			"REPORT_READY. The boundary contained every run, which is a containment result and not "+
			"a demonstration that the model now emits the correct missing_additive action.",
			len(obs))
	}
	if ready > 0 && corrected == 0 {
		t.Errorf("APA-56 CAUSAL CLAIM NOT DEMONSTRATED: %d run(s) reached REPORT_READY and none "+
			"submitted the corrected shape {%q,%q}.", ready, string(invest.MissingField), apa56CausalKey)
	}

	t.Logf("APA56-CAUSAL outcome gate: measured=%d report_ready=%d corrected_shape=%d "+
		"historical_denial=%d unreadable=%d",
		len(obs), ready, corrected, historical, unreadable)
}
