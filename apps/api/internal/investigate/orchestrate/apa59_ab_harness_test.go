package orchestrate

// APA-59: the controlled A/B seam for the per-tool limit-bound contract.
//
// WHAT THIS IS
// Variant B adds one clause to the model-facing prompt: the authoritative
// per-tool maximum for "limit". Variant A is the production prompt byte for
// byte. Both arms then run through ONE shared harness, so the only independent
// variable is whether the model was told the bound.
//
// WHY A TEST-ONLY SEAM
// The experiment needs two prompt states. Threading a PromptVariant through
// RenderPrompt would put an experiment-only parameter into the production
// contract, which is architectural pollution for a temporary causal question.
// So RenderPrompt is untouched and the variant is applied to its output:
//
//	production
//	  RenderPrompt(req) ──────────────> A  (exact production prompt)
//	          │
//	          └─ + APA-59 clause ─────> B  (production prompt + one clause)
//
// It is NOT:
//
//	production prompt ──copy/paste──> A
//	                            └───> B
//
// A is never reconstructed. It IS RenderPrompt(req).
//
// ANTI-DRIFT CONTRACT
// The risk of a test-only seam is that the arms drift apart for reasons
// unrelated to the experiment. Three tests below make that impossible to miss:
//
//	TestAPA59_ArmAIsExactlyProductionPrompt  A == RenderPrompt(req), byte for byte
//	TestAPA59_ArmsDifferOnlyByTheClause       normalize(B - A) == the clause
//	TestAPA59_ClauseMatchesAuthoritativeBounds  every stated bound == investigate.MaxRows
//
// The third is the one that matters most: the clause is GENERATED from the
// authoritative MaxRows function rather than restated, so the prompt cannot
// teach a bound the validator would reject. That is the exact failure this
// issue exists to fix, and restating the numbers in a prompt string would
// reproduce it one level up.
//
// HARD QUALIFICATION RULE
// B passing is NOT evidence that APA-59 solved anything if B still executes
// zero tools. TestAPA59_DecisiveToolTrajectory encodes that: A must attempt
// and fail to execute, B must attempt AND execute. Anything else is a
// null result and must be reported as such.

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// PromptVariant selects which model-facing contract a scenario is measured
// against. It lives in the test harness, never in production.
type PromptVariant int

const (
	// PromptVariantAPA56 is the control: the production prompt, unchanged.
	PromptVariantAPA56 PromptVariant = iota
	// PromptVariantAPA59 adds the per-tool limit-bound clause.
	PromptVariantAPA59
)

func (v PromptVariant) String() string {
	switch v {
	case PromptVariantAPA56:
		return "A:APA-56"
	case PromptVariantAPA59:
		return "B:APA-56+APA-59"
	default:
		return fmt.Sprintf("unknown(%d)", int(v))
	}
}

// apa59ClauseAnchor is the exact production line the clause is inserted
// BEFORE. It is chosen because it is unique in the rendered prompt and is the
// sentence that currently fails to say anything about "limit": the prompt
// defines the property and shows an example, but never states the bound.
//
// Uniqueness is asserted by TestAPA59_ClauseInsertionPointIsUnique, so if a
// future prompt edit makes this ambiguous the experiment fails loudly instead
// of silently injecting in the wrong place.
const apa59ClauseAnchor = `- A "request" selects and bounds a read.`

// apa59BoundTools is every tool that carries a "limit", read from the
// authoritative MaxRows function rather than restated. Rendering from this is
// what keeps the clause and the validator from drifting apart.
func apa59BoundTools() []invest.ToolName {
	return []invest.ToolName{
		invest.ToolGetClaim,
		invest.ToolGetPolicyContext,
		invest.ToolGetDocuments,
		invest.ToolGetEvidence,
		invest.ToolSearchEvidence,
		invest.ToolGetVerificationFindings,
		invest.ToolGetExternalPolicyStatus,
		invest.ToolGetTPACase,
		invest.ToolGetProviderEncounter,
		invest.ToolGetRiskSignals,
		invest.ToolCreateInvestigationReport,
	}
}

// apa59LimitBoundsClause renders the clause model-facing. Every number comes
// from investigate.MaxRows, so the prompt can never state a bound the request
// validator would reject.
func apa59LimitBoundsClause() string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(`"limit" is bounded per tool, and the bound is not the same for every tool.`)
	b.WriteString(` Send "limit" no greater than the tool's own maximum: `)
	tools := apa59BoundTools()
	parts := make([]string, 0, len(tools))
	for _, t := range tools {
		max, err := investigate.MaxRows(t)
		if err != nil {
			// Unreachable for the fixed list above; failing loudly beats
			// rendering a bound nobody can verify.
			panic("apa59: authoritative bound unavailable: " + err.Error())
		}
		parts = append(parts, fmt.Sprintf("%s at most %d", string(t), max))
	}
	b.WriteString(strings.Join(parts, ", "))
	b.WriteString(".")
	b.WriteString(` A "limit" above a tool's maximum is rejected before the tool runs, so a single-row tool must send "limit" 1.`)
	return b.String()
}

// renderAPA59Variant returns the model-facing prompt for one arm.
//
// A is RenderPrompt(req) verbatim — no reconstruction, no transformation.
// B is that same string with exactly one clause inserted, at a deterministic
// point. Everything else about the prompt is untouched, which is what makes
// the A→B difference attributable to the limit-bound contract alone.
func renderAPA59Variant(req ModelRequest, v PromptVariant) (string, error) {
	base, err := RenderPrompt(req)
	if err != nil {
		return "", err
	}
	if v == PromptVariantAPA56 {
		return base, nil
	}
	if v != PromptVariantAPA59 {
		return "", fmt.Errorf("apa59: unknown prompt variant %d", int(v))
	}
	if strings.Count(base, apa59ClauseAnchor) != 1 {
		return "", fmt.Errorf("apa59: insertion point is not unique in the rendered prompt "+
			"(%d occurrences); refusing to inject at an ambiguous location",
			strings.Count(base, apa59ClauseAnchor))
	}
	clause := apa59LimitBoundsClause()
	return strings.Replace(base, apa59ClauseAnchor, clause+"\n"+apa59ClauseAnchor, 1), nil
}

// apa59SampleRequest builds the canonical request the A/B renders. One
// builder for both arms, so the arms cannot differ because one of them was
// handed a different request.
func apa59SampleRequest(t *testing.T) ModelRequest {
	t.Helper()
	env := testEnvelope(t)
	return ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: KnownIDs(mustSeed(t, env)),
		Turn:             1,
		RequestID:        testScope(env).RequestID,
	}
}

// ---------------------------------------------------------------------------
// Anti-drift
// ---------------------------------------------------------------------------

// TestAPA59_ArmAIsExactlyProductionPrompt is the load-bearing anti-drift test.
// Arm A must BE the production renderer output, not a reconstruction of it.
func TestAPA59_ArmAIsExactlyProductionPrompt(t *testing.T) {
	req := apa59SampleRequest(t)
	production, err := RenderPrompt(req)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	armA, err := renderAPA59Variant(req, PromptVariantAPA56)
	if err != nil {
		t.Fatalf("render arm A: %v", err)
	}
	if armA != production {
		t.Errorf("arm A is NOT the production prompt (armA=%d bytes, production=%d bytes).\n"+
			"A must be RenderPrompt(req) verbatim; a reconstructed control is weaker "+
			"evidence than the real historical implementation", len(armA), len(production))
	}
}

// TestAPA59_ClauseInsertionPointIsUnique guards the injection itself. If a
// future prompt edit makes the anchor ambiguous or removes it, the experiment
// must fail rather than inject somewhere arbitrary.
func TestAPA59_ClauseInsertionPointIsUnique(t *testing.T) {
	prompt, err := RenderPrompt(apa59SampleRequest(t))
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if n := strings.Count(prompt, apa59ClauseAnchor); n != 1 {
		t.Fatalf("insertion anchor appears %d times in the rendered prompt, want exactly 1; "+
			"pick a new anchor before running the A/B", n)
	}
	if strings.Contains(prompt, apa59LimitBoundsClause()) {
		t.Fatal("the production prompt ALREADY contains the APA-59 clause; arm A would no " +
			"longer be the pre-APA-59 control and the experiment would be meaningless")
	}
}

// TestAPA59_ArmsDifferOnlyByTheClause is the strongest form of the diff
// assertion: removing the clause from B must reproduce A exactly. Anything
// else means the arms differ by more than the independent variable.
func TestAPA59_ArmsDifferOnlyByTheClause(t *testing.T) {
	req := apa59SampleRequest(t)
	armA, err := renderAPA59Variant(req, PromptVariantAPA56)
	if err != nil {
		t.Fatalf("render arm A: %v", err)
	}
	armB, err := renderAPA59Variant(req, PromptVariantAPA59)
	if err != nil {
		t.Fatalf("render arm B: %v", err)
	}
	clause := apa59LimitBoundsClause()

	if armA == armB {
		t.Fatal("arm A and arm B are identical; the experiment would compare nothing")
	}
	if strings.Contains(armA, clause) {
		t.Error("arm A already contains the APA-59 clause; it is not the control")
	}
	if !strings.Contains(armB, clause) {
		t.Error("arm B does not contain the APA-59 clause")
	}

	// Strip the clause (and the newline it introduced) and demand A back.
	stripped := strings.Replace(armB, clause+"\n", "", 1)
	if stripped != armA {
		t.Errorf("arms differ by more than the APA-59 clause.\n"+
			"After removing the clause, B (%d bytes) did not reproduce A (%d bytes).\n"+
			"The independent variable is the clause alone.", len(stripped), len(armA))
	}
}

// TestAPA59_ClauseMatchesAuthoritativeBounds is the divergence test that stops
// this defect class recurring. Every bound the clause states is read back from
// investigate.MaxRows, so the prompt cannot disagree with the validator.
func TestAPA59_ClauseMatchesAuthoritativeBounds(t *testing.T) {
	clause := apa59LimitBoundsClause()

	if strings.Count(clause, apa59ClauseAnchor) != 0 {
		t.Error("the clause must not contain its own insertion anchor; it would nest on re-render")
	}

	// Parse the clause into stated pairs and compare EXACTLY.
	//
	// A substring check is unsound here and this test already proved it: the
	// clause rendered "get_claim at most 10" while the authority said 1, and
	// strings.Contains(clause, "get_claim at most 1") returned TRUE because
	// "get_claim at most 10" has it as a prefix. A number is never safe to
	// match as a substring, so the clause is parsed and the pairs compared as
	// whole units.
	stated := apa59StatedBounds(clause)
	for _, tool := range apa59BoundTools() {
		max, err := investigate.MaxRows(tool)
		if err != nil {
			t.Fatalf("authoritative bound for %s: %v", tool, err)
		}
		got, ok := stated[string(tool)]
		if !ok {
			t.Errorf("clause does not state a bound for %s\nclause: %s", tool, clause)
			continue
		}
		if got != max {
			t.Errorf("clause states %s at most %d, but the authoritative bound is %d; "+
				"the prompt would teach a value Request.Validate rejects",
				tool, got, max)
		}
	}

	// Every bound-bearing tool must appear, and nothing else may. A newly
	// allowlisted tool that silently missed the clause would reintroduce this
	// exact gap; a tool named in the clause that does not exist would teach a
	// knob that does not exist.
	if len(stated) != len(apa59BoundTools()) {
		t.Errorf("clause states %d bounds but %d tools carry one; stated=%v",
			len(stated), len(apa59BoundTools()), stated)
	}
	for _, tool := range apa59BoundTools() {
		if _, err := investigate.MaxRows(tool); err != nil {
			t.Errorf("clause names tool %s, which is not a real tool: %v", tool, err)
		}
	}
}

// apa59StatedBounds parses "<tool> at most <n>" pairs out of the rendered
// clause into an exact map. Parsing rather than substring-matching is what
// makes the divergence test sound: see the comment in
// TestAPA59_ClauseMatchesAuthoritativeBounds.
func apa59StatedBounds(clause string) map[string]int {
	out := map[string]int{}
	// Parse only the BOUNDS REGION: everything between the clause's list
	// introducer and the sentence terminator. Scanning the whole clause
	// dropped the first pair, because the prose before "get_claim" left that
	// segment with more than two fields.
	const intro = "the tool's own maximum: "
	i := strings.Index(clause, intro)
	if i < 0 {
		return out
	}
	region := clause[i+len(intro):]
	if j := strings.Index(region, "."); j >= 0 {
		region = region[:j]
	}
	normalized := strings.NewReplacer(",", ".", " at most ", " ").Replace(region)
	for _, seg := range strings.Split(normalized, ".") {
		fields := strings.Fields(strings.TrimSpace(seg))
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		out[fields[0]] = n
	}
	return out
}

// TestAPA59_ClauseStaysWithinEveryAuthoritativeBound guards the one thing the
// clause must not do: teach a value the validator rejects. Each bound stated
// is checked against the real Request.Validate.
// TestAPA59_ClauseStaysWithinEveryAuthoritativeBound is the end-to-end check:
// every bound the CLAUSE STATES is fed to the real request validator.
//
// It deliberately drives from the parsed clause rather than from
// investigate.MaxRows. An earlier revision read `max` from the authority and
// never touched the clause at all (it carried a literal `_ = clause`), so it
// stayed green under a fault that taught a wrong bound — it was testing the
// authority against itself. The path that matters is clause -> string ->
// validator, because that is the path the model reads.
func TestAPA59_ClauseStaysWithinEveryAuthoritativeBound(t *testing.T) {
	clause := apa59LimitBoundsClause()
	stated := apa59StatedBounds(clause)
	if len(stated) == 0 {
		t.Fatalf("clause yielded no parseable bounds; the parser and the clause "+
			"disagree, so nothing here is meaningful:\n%s", clause)
	}
	for toolName, statedMax := range stated {
		tool := invest.ToolName(toolName)
		newReq := func(limit int) investigate.Request {
			return investigate.Request{
				Tool: tool, TenantID: tTenant, ClaimID: tClaim,
				InvestigationID: tInvID, RequestID: tReqID, Limit: limit,
			}
		}
		// The number the clause names must be accepted by the real validator.
		if err := newReq(statedMax).Validate(); err != nil {
			t.Errorf("clause states %s accepts limit=%d, but the validator rejects it: %v",
				tool, statedMax, err)
		}
		// And the stated bound must be a real ceiling, not documentation:
		// one more must be refused, which is the behaviour the clause warns
		// the model about.
		if err := newReq(statedMax + 1).Validate(); err == nil {
			t.Errorf("clause implies %s caps limit at %d, but limit=%d was ACCEPTED; "+
				"the stated bound does not describe the real validator", tool, statedMax, statedMax+1)
		}
	}
}

// TestAPA59_PromptStillDeclaresLimitBelowItsBound confirms the specific
// contradiction this issue was filed against is present in arm A and absent in
// arm B. The get_documents example uses limit 10, which is legal (bound 50), so
// the check is that the example never exceeds its OWN tool's bound.
func TestAPA59_PromptStillDeclaresLimitBelowItsBound(t *testing.T) {
	req := apa59SampleRequest(t)
	armA, err := renderAPA59Variant(req, PromptVariantAPA56)
	if err != nil {
		t.Fatalf("render arm A: %v", err)
	}
	armB, err := renderAPA59Variant(req, PromptVariantAPA59)
	if err != nil {
		t.Fatalf("render arm B: %v", err)
	}

	// arm A states no per-tool bound at all. That absence is the defect.
	if strings.Contains(armA, "at most") {
		t.Error("arm A already states a per-tool bound; it is not the pre-APA-59 control")
	}
	if !strings.Contains(armB, "at most") {
		t.Error("arm B does not state any per-tool bound")
	}

	// The canonical call_tool example's limit must be legal for the tool IT
	// names, in both arms. An earlier revision checked
	// !strings.Contains(armA, `"limit":10`) && docsMax < 10, which can never
	// fire because get_documents bounds at 50 and it also had the condition
	// inverted: it complained when the example was ABSENT rather than when it
	// was illegal. The example is parsed instead.
	for arm, prompt := range map[string]string{"A": armA, "B": armB} {
		tool, limit, ok := apa59ExampleCallToolLimit(prompt)
		if !ok {
			t.Fatalf("arm %s: could not find a canonical call_tool example with a limit", arm)
		}
		max, err := investigate.MaxRows(tool)
		if err != nil {
			t.Fatalf("arm %s: example names unknown tool %q: %v", arm, tool, err)
		}
		if limit < 1 || limit > max {
			t.Errorf("arm %s: canonical example calls %s with limit=%d, outside 1..%d; "+
				"the example itself would be rejected by the validator", arm, tool, limit, max)
		}
	}
}

// apa59ExampleCallToolLimit extracts the tool and limit from the prompt's
// canonical call_tool example, so the example can be validated against the
// bound of the tool it actually names rather than a hardcoded one.
func apa59ExampleCallToolLimit(prompt string) (invest.ToolName, int, bool) {
	const marker = `{"action":"call_tool"`
	i := strings.Index(prompt, marker)
	if i < 0 {
		return "", 0, false
	}
	line := prompt[i:]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	toolAt := strings.Index(line, `"tool":"`)
	if toolAt < 0 {
		return "", 0, false
	}
	rest := line[toolAt+len(`"tool":"`):]
	toolEnd := strings.IndexByte(rest, '"')
	if toolEnd < 0 {
		return "", 0, false
	}
	limitAt := strings.Index(line, `"limit":`)
	if limitAt < 0 {
		return "", 0, false
	}
	numAt := limitAt + len(`"limit":`)
	numEnd := numAt
	for numEnd < len(line) && line[numEnd] >= '0' && line[numEnd] <= '9' {
		numEnd++
	}
	n, err := strconv.Atoi(line[numAt:numEnd])
	if err != nil {
		return "", 0, false
	}
	return invest.ToolName(rest[:toolEnd]), n, true
}

// TestAPA59_DecisiveToolTrajectory is the qualification rule, encoded.
//
//	B green with zero executed tools is NOT evidence APA-59 worked.
//
// It requires BOTH arms to have been measured, A to attempt a tool and fail to
// execute one, and B to attempt AND execute. Any other shape is a null result,
// and the caller must report it as such rather than as a pass.
func TestAPA59_DecisiveToolTrajectory(t *testing.T) {
	trajectories := map[PromptVariant]struct{ attempted, executed int }{}
	if v := qualifyingTrajectoryFromEnv("A"); v != nil {
		trajectories[PromptVariantAPA56] = *v
	}
	if v := qualifyingTrajectoryFromEnv("B"); v != nil {
		trajectories[PromptVariantAPA59] = *v
	}
	if len(trajectories) < 2 {
		// A skip must never contribute to a qualification verdict (APA-57).
		// So the dormant state is explicit rather than silently absent, and a
		// run that CLAIMS to qualify APA-59 without trajectories fails hard
		// instead of reporting a green-looking skip.
		//
		//   unset / 0 -> structural work only; this rule stays dormant
		//   1        -> this run asserts APA-59 qualification, so an
		//               unmeasured trajectory is a FAILURE, not a skip
		msg := fmt.Sprintf("APA-59 decisive trajectory NOT MEASURED (arms supplied: %d/2); "+
			"the structural gates are sound but causal qualification requires a live A/B "+
			"via APA59_TRAJECTORY_A and APA59_TRAJECTORY_B", len(trajectories))
		if strings.TrimSpace(os.Getenv("APA59_REQUIRE_TRAJECTORY")) == "1" {
			t.Fatalf("%s. This run declares APA59_REQUIRE_TRAJECTORY=1, so it is claiming "+
				"to qualify APA-59; a qualification verdict requires the measured A/B", msg)
		}
		t.Skip(msg)
	}
	a, aOK := trajectories[PromptVariantAPA56]
	b, bOK := trajectories[PromptVariantAPA59]
	if !aOK || !bOK {
		t.Fatal("both arms must be measured before this rule can be applied")
	}

	if a.attempted == 0 {
		t.Errorf("arm A never ATTEMPTED a tool (attempted=%d); the control does not "+
			"reproduce the defect, so B could not be attributed", a.attempted)
	}
	if a.executed != 0 {
		t.Errorf("arm A EXECUTED %d tools; expected 0. The control is not the pre-APA-59 "+
			"condition, so any A→B difference is confounded", a.executed)
	}
	if b.attempted == 0 {
		t.Error("arm B never attempted a tool; the model did not act on the new bound")
	}
	if b.executed == 0 {
		t.Errorf("NULL RESULT: arm B attempted %d tool(s) but executed 0. "+
			"A green B with executed=0 is NOT evidence that APA-59 fixed the contract; "+
			"the requests are still being rejected before they run", b.attempted)
	}
}

// qualifyingTrajectoryFromEnv reads an arm's measured trajectory so the rule
// above can be applied against real numbers rather than asserted in prose.
// Format: "attempted,executed".
func qualifyingTrajectoryFromEnv(arm string) *struct{ attempted, executed int } {
	raw := strings.TrimSpace(os.Getenv("APA59_TRAJECTORY_" + arm))
	if raw == "" {
		return nil
	}
	var got struct{ attempted, executed int }
	if _, err := fmt.Sscanf(raw, "%d,%d", &got.attempted, &got.executed); err != nil {
		return nil
	}
	return &got
}

// TestAPA59_VariantCoverage proves both arms stay wired to the same renderer,
// so a future refactor cannot quietly point one arm somewhere else.
func TestAPA59_VariantCoverage(t *testing.T) {
	variants := []PromptVariant{PromptVariantAPA56, PromptVariantAPA59}
	seen := map[string]bool{}
	for _, v := range variants {
		if seen[v.String()] {
			t.Errorf("duplicate variant label %q; two arms would be indistinguishable in the "+
				"qualification record", v.String())
		}
		seen[v.String()] = true
	}
	if !slices.Contains(variants, PromptVariantAPA56) || !slices.Contains(variants, PromptVariantAPA59) {
		t.Fatal("the A/B pair must always include both the control and the variant")
	}
	// The zero value must be the control, so an accidental invocation
	// reproduces the historical condition rather than the experimental one.
	var zero PromptVariant
	if zero != PromptVariantAPA56 {
		t.Errorf("PromptVariant zero value is %v; it must be PromptVariantAPA56 so an "+
			"accidental or defaulted invocation reproduces the control condition", zero)
	}
}

// TestAPA59_QualificationStatus makes the harness's own standing explicit in the
// qualification output, so a reviewer never has to infer it from a skip.
//
// It reports two separate facts that must never be conflated:
//
//	APA-59 harness: structural gates PASS
//	APA-59 decisive trajectory: NOT MEASURED  (or MEASURED)
//
// The first says the contract machinery is internally sound: the clause is
// derived from the authoritative bounds, both arms share one renderer, and the
// canonical example is legal for the tool it names. The second says whether any
// real model has yet been observed changing behaviour because of it. Only a
// live A/B can establish that, and only this test can report it.
func TestAPA59_QualificationStatus(t *testing.T) {
	// Prove the structural half here rather than asserting it, so the status
	// line cannot claim PASS for gates that were never executed.
	clauses := map[PromptVariant]string{}
	req := apa59SampleRequest(t)
	for _, v := range []PromptVariant{PromptVariantAPA56, PromptVariantAPA59} {
		prompt, err := renderAPA59Variant(req, v)
		if err != nil {
			t.Fatalf("render %v: %v", v, err)
		}
		clauses[v] = prompt
	}
	armA, armB := clauses[PromptVariantAPA56], clauses[PromptVariantAPA59]

	stated := apa59StatedBounds(apa59LimitBoundsClause())
	structuralOK := len(stated) == len(apa59BoundTools())
	for _, tool := range apa59BoundTools() {
		max, err := investigate.MaxRows(tool)
		if err != nil || stated[string(tool)] != max {
			structuralOK = false
			break
		}
	}
	structuralOK = structuralOK &&
		strings.Contains(armB, apa59LimitBoundsClause()) &&
		!strings.Contains(armA, apa59LimitBoundsClause())

	a, b := qualifyingTrajectoryFromEnv("A"), qualifyingTrajectoryFromEnv("B")
	t.Logf("APA-59 harness: structural gates %s",
		map[bool]string{true: "PASS", false: "FAIL"}[structuralOK])
	if !structuralOK {
		t.Error("APA-59 harness: structural gates FAIL; the status line above must not be " +
			"read as PASS, and no qualification may rely on this harness")
	}

	switch {
	case a != nil && b != nil:
		t.Logf("APA-59 decisive trajectory: MEASURED A(attempted=%d executed=%d) B(attempted=%d executed=%d)",
			a.attempted, a.executed, b.attempted, b.executed)
	default:
		t.Logf("APA-59 decisive trajectory: NOT MEASURED — causal qualification is " +
			"PENDING. This is NOT a qualified result and must not be reported as one.")
	}
}
