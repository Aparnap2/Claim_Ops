package orchestrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// Production prompt fidelity for APA-59.
//
// APA-59 was measured with a treatment prompt built by injecting one clause
// into the production prompt (PR #127, arm B). That evidence is only a
// statement about production if the prompt that was measured is the prompt
// production renders. This file pins that, so the recorded causal evidence
// cannot silently decay into a claim about a prompt that no longer ships.

// frozenAPA59BPromptSHA256 is the sha256 of the arm-B prompt bytes that were
// actually sent to the provider during the APA-59 live A/B measurement. It was
// logged by TestAPA59ABDecisiveToolTrajectory before any provider call:
//
//	APA59_B_PROMPT_SHA256=f57cf7ad58d360331e2cae9a914da3dea7df557751a41e2946667a3a1a1cdd5b
//
// This is the load-bearing constant for the whole APA-59 conclusion. Changing
// it requires a fresh measurement, not a rerun.
const frozenAPA59BPromptSHA256 = "f57cf7ad58d360331e2cae9a914da3dea7df557751a41e2946667a3a1a1cdd5b"

// apa59ProductionDeadlineContractMs is the PRODUCTION contract value for one
// investigation's wall clock, asserted independently of where it is declared.
//
// It exists to be asserted AGAINST workeradapter.DefaultScopeDeadlineMs, never
// to be used as the source of the anchored value below. Typing 60000 into the
// helper would let the two copies drift apart silently: production could move
// to 90000, the helper would keep rendering the 60000 prompt, and the hash gate
// would stay green while describing a prompt production no longer ships. The
// whole point of anchoring to the constant is that a production change breaks
// this gate LOUDLY.
const apa59ProductionDeadlineContractMs = int64(60000)

// The two production deadline defaults this file must agree on. Both declare
// 60000 and they are asserted against each other by
// TestAPA59_ProcessorDeadlineDefaultHasNotDiverged, because only one of the two
// is reachable as a Go identifier from this package.
const (
	// apa59FullchainSource declares the EXPORTED default,
	// workeradapter.DefaultScopeDeadlineMs, which this file anchors to. The Go
	// package is named workeradapter (internal/adapters/worker/fullchain.go:13),
	// so the constant is spelled workeradapter.DefaultScopeDeadlineMs; it has no
	// file-scoped namespace in Go.
	apa59FullchainSource = "../../adapters/worker/fullchain.go"
	// apa59ProcessorSource declares defaultScopeDeadlineMs (processor.go:1077),
	// which is UNEXPORTED and lives in package worker. It is not importable from
	// here, so it is read from source. See
	// TestAPA59_ProcessorDeadlineDefaultHasNotDiverged for why a source read is
	// the honest option rather than a workaround.
	apa59ProcessorSource = "../../worker/processor.go"
)

// apa59MeasuredRequest is the exact request the A/B rendered and measured.
//
// It matches TestAPA59ABDecisiveToolTrajectory, which renders
// ModelRequest{Exception: env, KnownEvidenceIDs: []string{}, Turn: 1,
// RequestID: scope.RequestID} from psFixtureNeedsTool. KnownEvidenceIDs is
// empty on purpose: that is the zero-citable-evidence condition that forces
// real tool mediation, which is what the experiment needs.
//
// THE ENVELOPE DEADLINE IS ANCHORED TO PRODUCTION, NOT TO THE FIXTURE.
//
// deadline_ms is a JSON-serialized field of invest.ScopeConstraints
// (invest/exception.go:261), the envelope is embedded in ModelRequest.Exception,
// and CanonicalModelRequest marshals it into the prompt body. So the envelope's
// deadline is PROMPT BYTES, not metadata, and byte equality is the entire claim
// this file makes. A production-fidelity gate that takes its supposed production
// bytes from a mutable qualification fixture is not measuring production.
//
// The shared fixture builds that envelope at its own named QUALIFICATION
// deadline constant, 600000, declared in apa58_scenario_fixtures_test.go. That
// is a DIFFERENT number answering a DIFFERENT question, and the two are
// intentionally unequal. Neither may be reconciled by editing one to match the
// other, and this file deliberately does not name that constant: the Phase-2
// reconciliation proves by byte scan that the fixture's qualification constant
// is referenced by the two fixture sites and NO other file in this package, and
// an APA-59 comment that spelled the identifier would trip that containment
// proof for a mention that changes no deadline. The contract being claimed here
// is stated instead, in prose and in numbers:
//
//   - APA-59 asks "is the production-shaped prompt still the measured prompt?"
//     Production renders with workeradapter.DefaultScopeDeadlineMs = 60000
//     (fullchain.go:33). 60000 is therefore the value THIS file needs.
//   - APA-55 asks "can the scenario exercise multi-turn behaviour?" Its budget
//     must absorb cumulative provider pacing, measured at ~60.78s over three
//     turns, so it is 600000. The derivation, and the measured numbers behind
//     it, are documented at that constant in apa58_scenario_fixtures_test.go.
//
// The value is TAKEN FROM the production constant rather than typed as a literal
// here, so that if production ever moves the anchored prompt moves with it and
// the hash assertion fails loudly instead of quietly comparing production
// against a stale 60000 prompt. Two tests make that failure a diagnosis rather
// than a mystery: TestAPA59_ProductionDeadlineDefaultIsTheAnchoredContractValue
// and TestAPA59_ProcessorDeadlineDefaultHasNotDiverged.
//
// ONE SCOPE-ORDERING TRAP, STATED SO NOBODY FALLS IN IT. psFixtureNeedsTool
// still returns a scope whose deadline is 600000, and it is used here only for
// scope.RequestID. This file never constructs a loop, so loop.go's
// checkLoopAuthority rule (scope.DeadlineMs may tighten but never exceed the
// envelope's) is never evaluated, and RenderPrompt reads the ENVELOPE alone.
// Building a NewLoop from this request would be rejected, correctly: the two
// deadlines here are not a pair, they belong to two different questions.
func apa59MeasuredRequest(t *testing.T) ModelRequest {
	t.Helper()
	env, scope, _ := psFixtureNeedsTool(t)
	env.Scope.DeadlineMs = workeradapter.DefaultScopeDeadlineMs
	// Precondition: re-anchoring must leave a VALID envelope. invest.Validate
	// requires deadline_ms > 0, and the loop caps scope deadlines at
	// MaxDeadlineMs (loop.go:36). If the production default ever became illegal
	// the three tests below would silently grade a malformed prompt, so the
	// fixture refuses to hand one over.
	if err := invest.Validate(env); err != nil {
		t.Fatalf("anchored envelope is not valid at the production deadline %d: %v",
			env.Scope.DeadlineMs, err)
	}
	return ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{},
		Turn:             1,
		RequestID:        scope.RequestID,
	}
}

// TestAPA59_ProductionDeadlineDefaultIsTheAnchoredContractValue pins the
// production default itself.
//
// apa59MeasuredRequest deliberately derives its deadline from production rather
// than typing 60000, so the hash gate would survive a production change by
// comparing production against a prompt production no longer renders. This test
// closes that hole: it asserts the constant still equals the value the frozen
// prompt was MEASURED under, so the first thing to break when production moves
// is a named assertion about the default, not a bare digest mismatch.
//
// Note the direction of the failure. This does not mean "update the hash to the
// new default". The recorded APA-59 evidence describes the prompt measured
// under 60000; a different production default means that evidence must be
// RE-MEASURED, exactly as frozenAPA59BPromptSHA256 already states.
func TestAPA59_ProductionDeadlineDefaultIsTheAnchoredContractValue(t *testing.T) {
	got := workeradapter.DefaultScopeDeadlineMs
	// Fatalf, not Errorf: the confirmation line below asserts equality, so
	// running it after a failure would print a passing-shaped claim about a
	// value this test just rejected. There is nothing further to run here.
	if got != apa59ProductionDeadlineContractMs {
		t.Fatalf("PRODUCTION DEFAULT MOVED: workeradapter.DefaultScopeDeadlineMs (%s) = %d, "+
			"but the frozen APA-59 prompt was measured under %d.\n"+
			"The recorded causal evidence describes the %d-deadline prompt. Do NOT re-freeze "+
			"the hash and do NOT edit the constant to make this pass: a production default change "+
			"invalidates the measurement and requires re-running APA-59, then re-freezing "+
			"frozenAPA59BPromptSHA256 from the fresh measurement.",
			apa59FullchainSource, got, apa59ProductionDeadlineContractMs, apa59ProductionDeadlineContractMs)
	}
	t.Logf("PRODUCTION DEADLINE %s: DefaultScopeDeadlineMs = %d (contract %d)",
		apa59FullchainSource, got, apa59ProductionDeadlineContractMs)
}

// TestAPA59_ProcessorDeadlineDefaultHasNotDiverged is the second half of the
// production-default guard, and it exists because of a language limit rather
// than a testing preference.
//
// WHY A SOURCE READ IS NECESSARY, NOT A SHORTCUT. There are two production
// deadline defaults and they must agree:
//
//	apps/api/internal/adapters/worker/fullchain.go:33  DefaultScopeDeadlineMs
//	apps/api/internal/worker/processor.go:1077        defaultScopeDeadlineMs
//
// The first is exported and is what apa59MeasuredRequest anchors to. The second
// is UNEXPORTED and is declared in a different package, so Go gives this test no
// way to name it: there is no import, no accessor, and no build-time link to it.
// Its comment states it "mirrors" the adapter default, and a comment is not a
// constraint. So the only way to actually CHECK the mirror is to read the
// declaration. The alternative — re-typing 60000 and comparing that to a second
// re-typed 60000 — asserts nothing about processor.go at all and is precisely
// the silent-decay failure this guard exists to prevent.
//
// The value is read through the package's existing AST helper psReconConstInt64
// (apa64_fixture_deadline_reconciliation_test.go), which parses the declaration
// rather than substring-matching it, so a mention in a comment or in an
// unrelated expression cannot satisfy it. Reusing that parser instead of adding a
// second one is deliberate: two parsers for one constant could disagree, and the
// disagreement would be invisible.
func TestAPA59_ProcessorDeadlineDefaultHasNotDiverged(t *testing.T) {
	// Both files are named in the failure below, so both are proven readable
	// first. An unreadable source must fail hard: a skipped check would leave the
	// divergence unguarded while reporting success.
	psReconPath(t, apa59FullchainSource)
	psReconPath(t, apa59ProcessorSource)

	processor := psReconConstInt64(t, apa59ProcessorSource, "defaultScopeDeadlineMs")
	// Fatalf for the same reason as the contract check above: the line after
	// this reports the two values as equal and must not be reached when they
	// are not.
	if processor != workeradapter.DefaultScopeDeadlineMs {
		t.Fatalf("PRODUCTION DEFAULTS DIVERGED: %s declares defaultScopeDeadlineMs = %d, but %s "+
			"exports DefaultScopeDeadlineMs = %d.\n"+
			"processor.go states it mirrors the adapter default, and an investigation's wall clock "+
			"would then be %d on one wiring path and %d on the other. Because the processor constant "+
			"is unexported and in another package it cannot be imported, so this divergence is "+
			"detectable ONLY by reading the declaration: fix the production defaults, not this test.",
			apa59ProcessorSource, processor, apa59FullchainSource,
			workeradapter.DefaultScopeDeadlineMs, processor, workeradapter.DefaultScopeDeadlineMs)
	}
	t.Logf("PRODUCTION DEADLINE %s: defaultScopeDeadlineMs = %d == %s DefaultScopeDeadlineMs = %d",
		apa59ProcessorSource, processor, apa59FullchainSource, workeradapter.DefaultScopeDeadlineMs)
}

// TestAPA59_ProductionPromptIsTheFrozenMeasuredPrompt is the fidelity gate:
// production RenderPrompt must render byte-identically to the prompt whose
// behaviour was measured.
//
// This is a hash assertion, deliberately. A substring or "contains the clause"
// check would pass for any prompt that merely mentions a limit bound, including
// one that says the wrong numbers or places the rule where the model does not
// read it. Only whole-prompt byte equality proves the recorded evidence
// describes production.
func TestAPA59_ProductionPromptIsTheFrozenMeasuredPrompt(t *testing.T) {
	req := apa59MeasuredRequest(t)

	production, err := RenderPrompt(req)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	sum := sha256.Sum256([]byte(production))
	got := hex.EncodeToString(sum[:])
	// Emit the digest on PASS as well as on FAIL. A byte-equality gate that
	// prints nothing when green is indistinguishable from a gate that was
	// skipped, and this digest is the evidence the whole APA-59 conclusion rests
	// on. The line is deliberately the same APA59_B_PROMPT_SHA256= form the live
	// A/B measurement logged, so the offline reproduction and the original
	// measurement can be compared by eye.
	t.Logf("APA59_B_PROMPT_SHA256=%s (envelope deadline_ms=%d from %s, frozen %s)",
		got, req.Exception.Scope.DeadlineMs, apa59FullchainSource, frozenAPA59BPromptSHA256)
	if got != frozenAPA59BPromptSHA256 {
		t.Errorf("PROMPT DRIFT: production RenderPrompt no longer renders the prompt that "+
			"APA-59 measured.\n  frozen measured sha256: %s\n  production   sha256: %s\n"+
			"The recorded APA-59 causal evidence describes the frozen prompt above, not this one. "+
			"Either the production prompt changed, or the measurement must be repeated.",
			frozenAPA59BPromptSHA256, got)
	}
}

// apa59BoundsLinePrefix opens the production bounds clause line. Parsing starts
// from the rendered prompt rather than from the clause function, so the gate
// reads what the model is actually shown rather than what the renderer would
// return if asked.
const apa59BoundsLinePrefix = `- "limit" is bounded per tool`

// apa59StatedBoundsInPrompt parses the per-tool bounds out of a rendered prompt
// into whole tool-to-integer units.
func apa59StatedBoundsInPrompt(t *testing.T, prompt string) map[string]int {
	t.Helper()
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, apa59BoundsLinePrefix) {
			return apa59StatedBounds(line)
		}
	}
	t.Fatalf("rendered prompt has no line beginning %q; the production bounds clause "+
		"has drifted or been removed", apa59BoundsLinePrefix)
	return nil
}

// boundDefect is one tool whose prompt-stated bound disagrees with the
// authority that rejects an over-bound request.
type boundDefect struct {
	Tool      string
	Stated    int
	Authority int
}

func (d boundDefect) String() string {
	return fmt.Sprintf("%s: prompt states %d, authority allows %d", d.Tool, d.Stated, d.Authority)
}

// apa59BoundDefects compares parsed, whole-unit bounds against investigate.MaxRows
// as exact integers.
//
// This is deliberately integer equality and never a substring or prefix test.
// The prompt and the authority are rendered as text ("get_claim at most 1"),
// and "get_claim at most 10" CONTAINS "get_claim at most 1" as a substring, so
// a Contains-based gate passes exactly the 1 -> 10 defect it exists to reject.
// That collision is not hypothetical: it is the bug this whole chain was filed
// against. TestAPA59_BoundGateRejectsThePrefixCollision proves it.
func apa59BoundDefects(stated map[string]int) []boundDefect {
	var defects []boundDefect
	for _, tool := range limitBoundsTools() {
		name := string(tool)
		max, err := investigate.MaxRows(tool)
		if err != nil {
			defects = append(defects, boundDefect{Tool: name, Stated: -1, Authority: -1})
			continue
		}
		got, ok := stated[name]
		if !ok {
			defects = append(defects, boundDefect{Tool: name, Stated: -1, Authority: max})
			continue
		}
		if got != max {
			defects = append(defects, boundDefect{Tool: name, Stated: got, Authority: max})
		}
	}
	return defects
}

// TestAPA59_ProductionPromptStatesEveryAuthoritativeBound pins the substance
// independently of the prompt hash, so that a failure names what drifted rather
// than only reporting two digests.
func TestAPA59_ProductionPromptStatesEveryAuthoritativeBound(t *testing.T) {
	production, err := RenderPrompt(apa59MeasuredRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	stated := apa59StatedBoundsInPrompt(t, production)
	if n, want := len(stated), len(limitBoundsTools()); n != want {
		t.Errorf("parsed %d bound statements out of the production prompt, want %d; "+
			"every tool carrying a \"limit\" must be stated or the model guesses", n, want)
	}
	for _, d := range apa59BoundDefects(stated) {
		t.Errorf("BOUND MISMATCH %s; the model would emit a request the validator rejects", d)
	}
}

// TestAPA59_BoundGateRejectsThePrefixCollision is the adversarial RED proof for
// the gate above. It injects the exact historical defect -- get_claim widened
// from 1 to 10 -- into the rendered production prompt and requires the gate to
// catch it.
//
// Without this test the gate could silently regress to a Contains check and go
// green on the very defect it was written to reject.
func TestAPA59_BoundGateRejectsThePrefixCollision(t *testing.T) {
	production, err := RenderPrompt(apa59MeasuredRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	// Precondition: the real prompt states the authoritative single-row bound.
	if !strings.Contains(production, "get_claim at most 1") {
		t.Fatal("precondition changed: production does not state \"get_claim at most 1\"; " +
			"this proof encodes the 1 -> 10 collision and must be revisited, not weakened")
	}

	// The trap, stated explicitly: after widening 1 -> 10, the correct string is
	// still a substring of the prompt. A Contains-based gate cannot see it.
	faulty := strings.Replace(production, "get_claim at most 1", "get_claim at most 10", 1)
	if faulty == production {
		t.Fatal("injection failed: the authoritative bound string was not present to widen")
	}
	if !strings.Contains(faulty, "get_claim at most 1") {
		t.Fatal("precondition changed: expected the widened \"at most 10\" text to still " +
			"contain \"at most 1\" as a substring; that collision is what makes a " +
			"substring gate unsound and this proof is no longer testing it")
	}

	// Drive the real path the model reads: text -> parse -> compare.
	stated := apa59StatedBoundsInPrompt(t, faulty)
	if got := stated["get_claim"]; got != 10 {
		t.Fatalf("parser did not read the injected fault: get_claim parsed as %d, want 10", got)
	}

	var found bool
	for _, d := range apa59BoundDefects(stated) {
		if d.Tool == string(invest.ToolGetClaim) && d.Stated == 10 && d.Authority == 1 {
			found = true
		}
	}
	if !found {
		t.Error("GATE IS BROKEN: the prompt states get_claim at most 10 while the authority " +
			"allows 1, but the gate reported no defect. A substring comparison would have " +
			"passed this; the gate must compare parsed integers")
	}

	// The same gate must be clean on the real prompt, or "always reports a
	// defect" would pass the test above for the wrong reason.
	if defects := apa59BoundDefects(apa59StatedBoundsInPrompt(t, production)); len(defects) != 0 {
		t.Errorf("production prompt is not clean under the gate that just proved itself: %v", defects)
	}
}

// TestAPA59_ProductionPromptClosesTheSingleRowTrap guards the specific
// inference the example teaches by omission: get_documents shows limit 10,
// which is legal, while get_claim is capped at 1. Without an explicit rule the
// example value leaks across tools, which is the defect APA-59 measured.
func TestAPA59_ProductionPromptClosesTheSingleRowTrap(t *testing.T) {
	production, err := RenderPrompt(apa59MeasuredRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	claimMax, err := investigate.MaxRows(invest.ToolGetClaim)
	if err != nil {
		t.Fatalf("MaxRows(get_claim): %v", err)
	}
	if claimMax != 1 {
		t.Fatalf("precondition changed: get_claim MaxRows is %d, not 1; "+
			"this test encodes the single-row case and must be revisited, not silently weakened", claimMax)
	}

	for _, want := range []string{
		`a single-row tool must send "limit" 1`,
		`is rejected before the tool runs`,
	} {
		if !strings.Contains(production, want) {
			t.Errorf("production prompt is missing the guard %q; the model is left to infer the "+
				"per-tool bound from an example that only shows a multi-row tool", want)
		}
	}
}
