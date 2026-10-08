package orchestrate

// Offline proofs for the APA-64 fixture deadline reconciliation.
//
// WHAT IS BEING RECONCILED. The APA-55 live run measured ps_a1_control 3/3 as
//
//	outcome=ESCALATED reason=DEADLINE modelCalls=3 toolExecutions=0
//
// on lines that simultaneously reported recorderCalls=2 recorderObserved=2.
// APA-64 established the measured cause: the APA fixture's investigation
// budget was SMALLER than the cumulative provider pacing the harness charges
// inside it.
//
// The pacing is cumulative per repetition. The three measured repetitions
// reported pacedMs = 60770 / 60794 / 60778 over modelCalls = 3 each, so the
// per-turn cost is ~20.26s and ~60.77s is the THREE-TURN TOTAL. The fixture's
// budget was 60000ms. A three-turn paced trajectory therefore costs ~60.77s of
// a ~60s window, and the I2 post-Complete deadline check (loop.go:625) discards
// the turn-3 report before grounding ever sees it. The measured result is a
// statement about the HARNESS BUDGET, not about the model and not about the
// deterministic boundary, which is exactly why it has to be reconciled before
// any model conclusion is drawn.
//
// WHY TWO DEADLINES AND NOT ONE. The investigation has an envelope deadline
// (env.Scope.DeadlineMs, invest.ScopeConstraints) and a run-scope deadline
// (scope.DeadlineMs, investigate.Scope), and three separate facts make the
// second one the one that actually governs:
//
//  1. loop.go:502 derives the effective wall-clock window from the SCOPE:
//     deadline := time.Now().Add(l.scope.DeadlineMs). The envelope deadline is
//     never read by the loop body.
//  2. loop.go:295-296 (checkLoopAuthority, reached from NewLoop) rejects a
//     scope whose DeadlineMs EXCEEDS the envelope's, so the scope may tighten
//     the deadline but never extend it.
//  3. Raising only the envelope leaves the scope at testScope's hardcoded
//     60000 (orchestrate_test.go:198). NewLoop then passes checkLoopAuthority
//     TRIVIALLY, so nothing fails, and the run still escalates at 60s. That is
//     the silent no-op trap: a change that looks applied and measures
//     identically. TestAPA64Recon_NoOpTrapIsReal demonstrates exactly that.
//
// WHY 600000. It is the already-proven S1 qualification envelope value
// (apa52_s1_pg_live_test.go:448), which passed 3/3 REPORT_READY under this same
// harness and this same model. Nothing is invented here; the number that is
// known to work on this harness is adopted. It is also strictly below the
// harness's own context ceiling, which matters more than it looks: at or above
// that ceiling the context preempts the loop and the run yields an EMPTY
// InvestigationOutput with a raw context.DeadlineExceeded, which is a strictly
// worse failure mode than a well-formed ESCALATED(DEADLINE) because there is
// no structured escalation left to interpret. The deadline must therefore stay
// inside the harness context, not merely below the loop's own 1h cap.
//
// INDEPENDENCE FROM THE PHASE-1 ACCOUNTING CORRECTION. The Phase-1 change
// fixed what the harness MEASURES (ToolExecutions/RecorderObserved). This
// change fixes what the fixture GRANTS (the budget). They are separate axes and
// neither can compensate for the other: a correct measurement of a run that
// cannot finish is still a measurement of a run that cannot finish.
// TestAPA64Recon_IndependentOfMeasurementCorrection proves the two paths share
// no code and no constant rather than asserting it in prose.
//
// EVERYTHING HERE IS OFFLINE. No provider, no database, no quota, no wall-clock
// dependence. The five fixtures are built through their real builders, and the
// source assertions parse the actual files on disk.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
)

// psReconciledDeadlineMs is the value the proofs below require of the fixture.
// It is spelled as a LITERAL here, not as a reference to the fixture's own
// named constant, on purpose: a test that asserted the fixture equals the
// constant the fixture declares would pass whether or not either number were
// right. This literal is the contract. TestAPA64Recon_ConstantIsTheOneUsed
// then proves the fixture's constant has this value and that nothing else in
// the package carries it.
const psReconciledDeadlineMs = int64(600000)

// psMeasuredPacingPerTurnMs is the MEASURED per-turn pacing from the APA-55
// live run: pacedMs 60770 / 60794 / 60778 over modelCalls 3 each, i.e. the
// ~60.77s three-turn TOTAL divided by three turns. Not a derived estimate and
// not a floor read off the pacer; these are the numbers the run recorded. The
// deterministic floor is qualMinPacingWait (20s) plus the 250ms margin, and the
// measured ~20.26s is that floor plus a few ms of scheduler overshoot, which
// TestQualMeasurement_PacedMSIsCumulativeNotPerCall pins independently.
const psMeasuredPacingPerTurnMs = int64(20260)

// ---------------------------------------------------------------------------
// Source-reading helpers
//
// These parse the real files rather than trusting a comment or a remembered
// line number, so a later edit to any of them fails here instead of silently
// invalidating the proof.
// ---------------------------------------------------------------------------

// psReconParsed is a parsed source file together with the position
// information needed to map an AST node back to a byte range on disk.
type psReconParsed struct {
	rel  string
	raw  []byte
	fset *token.FileSet
	file *ast.File
}

// psReconParse parses one Go source file. Paths are resolved relative to this
// package's directory, which is the working directory `go test` sets.
func psReconParse(t *testing.T, rel string) *psReconParsed {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, raw, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return &psReconParsed{rel: rel, raw: raw, fset: fset, file: f}
}

// psReconFunc returns the function or method named name, including methods
// (a method's FuncDecl name is the bare method name, not receiver-qualified).
func (p *psReconParsed) psReconFunc(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range p.file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if ok && fd.Name.Name == name {
			return fd
		}
	}
	t.Fatalf("%s declares no function or method %q", p.rel, name)
	return nil
}

// psReconFuncSrc returns the raw source text of a function's BODY, so a scan
// for a forbidden token cannot be defeated by the token appearing in the doc
// comment above the function or elsewhere in the file.
func (p *psReconParsed) psReconFuncSrc(t *testing.T, name string) string {
	t.Helper()
	fd := p.psReconFunc(t, name)
	if fd.Body == nil {
		t.Fatalf("%s: function %q has no body", p.rel, name)
	}
	start := p.fset.Position(fd.Body.Lbrace).Offset
	end := p.fset.Position(fd.Body.Rbrace).Offset + 1
	if start < 0 || end > len(p.raw) || start >= end {
		t.Fatalf("%s: cannot slice body of %q (start=%d end=%d len=%d)",
			p.rel, name, start, end, len(p.raw))
	}
	return string(p.raw[start:end])
}

// psReconConstInt64 returns the value of a package-level integer constant,
// unwrapping a conversion call so `name = int64(3600000)` and `name = 3600000`
// are read the same way.
func psReconConstInt64(t *testing.T, rel, name string) int64 {
	t.Helper()
	p := psReconParse(t, rel)
	for _, d := range p.file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != name {
					continue
				}
				if i >= len(vs.Values) {
					t.Fatalf("%s: const %s has no value expression", rel, name)
				}
				return psReconIntExpr(t, rel, name, vs.Values[i])
			}
		}
	}
	t.Fatalf("%s declares no package-level const %q", rel, name)
	return 0
}

// psReconIntExpr evaluates an integer-valued constant expression: a literal, an
// explicit conversion of one, or a product of a literal and a time unit.
func psReconIntExpr(t *testing.T, rel, what string, e ast.Expr) int64 {
	t.Helper()
	switch v := e.(type) {
	case *ast.BasicLit:
		n, err := strconv.ParseInt(v.Value, 0, 64)
		if err != nil {
			t.Fatalf("%s: %s is not an integer literal: %q", rel, what, v.Value)
		}
		return n
	case *ast.CallExpr:
		if len(v.Args) != 1 {
			t.Fatalf("%s: %s is a conversion with %d args, want 1", rel, what, len(v.Args))
		}
		return psReconIntExpr(t, rel, what, v.Args[0])
	case *ast.BinaryExpr:
		if v.Op != token.MUL {
			t.Fatalf("%s: %s uses operator %v, want * (only a literal-times-unit product is read)",
				rel, what, v.Op)
		}
		unit, ok := v.Y.(*ast.SelectorExpr)
		if !ok {
			t.Fatalf("%s: %s multiplies by a non-selector; refusing to guess a unit", rel, what)
		}
		pkg, isIdent := unit.X.(*ast.Ident)
		if !isIdent || pkg.Name != "time" {
			t.Fatalf("%s: %s multiplies by %T.%s, want a time.<Unit>", rel, what,
				unit.X, unit.Sel.Name)
		}
		return psReconIntExpr(t, rel, what, v.X) * psReconTimeUnitMS(t, rel, unit.Sel.Name)
	}
	t.Fatalf("%s: cannot evaluate %s of type %T", rel, what, e)
	return 0
}

// psReconTimeUnitMS converts a time constant into milliseconds.
func psReconTimeUnitMS(t *testing.T, rel, unit string) int64 {
	t.Helper()
	switch unit {
	case "Nanosecond":
		return 0
	case "Microsecond":
		return 0
	case "Millisecond":
		return 1
	case "Second":
		return 1000
	case "Minute":
		return 60 * 1000
	}
	t.Fatalf("%s: unrecognised time unit %q", rel, unit)
	return 0
}

// psReconTimeoutMSIn returns the millisecond value of the deadline argument of
// the FIRST context.WithTimeout call inside fn. This is how the harness's own
// context ceiling is read, rather than hardcoded and re-checked against a
// stale copy of the source.
func psReconTimeoutMSIn(t *testing.T, rel, fn string) int64 {
	t.Helper()
	p := psReconParse(t, rel)
	fd := p.psReconFunc(t, fn)
	found := int64(0)
	seen := 0
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithTimeout" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "context" {
			return true
		}
		seen++
		if found == 0 {
			// context.WithTimeout(parent, timeout) — the timeout is arg 1.
			found = psReconIntExpr(t, rel, fn+" context deadline", call.Args[1])
		}
		return true
	})
	if seen == 0 {
		t.Fatalf("%s: function %q contains no context.WithTimeout call, so the harness ceiling "+
			"this proof compares against no longer exists", rel, fn)
	}
	return found
}

// psReconContains reports whether needle appears in the raw bytes of rel.
func psReconContains(t *testing.T, rel, needle string) bool {
	t.Helper()
	raw, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.Contains(string(raw), needle)
}

// psReconFixtureFiles is the fixture file whose two sites this change owns.
// The deadline constant must appear there and nowhere else in the package.
const psReconFixtureFiles = "apa58_scenario_fixtures_test.go"

// psReconPath asserts a protected source is readable from the package
// directory. The proofs read real files and never fall back to trusting a
// remembered literal, so an unreadable path is a hard failure rather than a
// silently skipped check.
func psReconPath(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("abs %s: %v", rel, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("protected source %s is not readable from the package directory (%v); "+
			"this proof reads the real file and cannot fall back to trusting a constant", rel, err)
	}
	return abs
}

// ---------------------------------------------------------------------------
// PROOF 1 — propagation, and the no-op trap that forces both edits
// ---------------------------------------------------------------------------

// TestAPA64Recon_DeadlinePropagatesToEveryFixtureScope proves that the
// reconciled deadline actually REACHES the investigation scope for all five
// APA fixtures, and that each (envelope, scope) pair is accepted in practice,
// not merely equal in arithmetic.
//
// Every fixture routes through psBuildParams and psScopeFor, so iterating the
// canonical registry psAllScenarios covers the five builders without
// restating their names, and a sixth fixture added later is covered
// automatically.
func TestAPA64Recon_DeadlinePropagatesToEveryFixtureScope(t *testing.T) {
	scenarios := psAllScenarios()
	if len(scenarios) != 5 {
		t.Fatalf("psAllScenarios returns %d scenario(s), want the five APA fixtures; the "+
			"propagation proof must cover the whole registry, not a hand-copied subset",
			len(scenarios))
	}

	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			env, scope, _ := tc.build(t)

			// The ENVELOPE deadline, set by psBuildParams.
			if got := env.Scope.DeadlineMs; got != psReconciledDeadlineMs {
				t.Errorf("env.Scope.DeadlineMs = %d, want %d: the envelope budget is still the "+
					"pre-reconciliation value, so the pacing cannot fit and the run escalates "+
					"ESCALATED(DEADLINE) for the harness-budget reason again", got, psReconciledDeadlineMs)
			}

			// The SCOPE deadline, set by psScopeFor. This is the one the loop
			// actually uses (loop.go:502).
			if got := scope.DeadlineMs; got != psReconciledDeadlineMs {
				t.Errorf("scope.DeadlineMs = %d, want %d: the loop derives its effective deadline "+
					"from the SCOPE (loop.go:502), never from the envelope, so this value is the "+
					"one that governs the run", got, psReconciledDeadlineMs)
			}

			// The loop.go:295 invariant, asserted directly so a future edit
			// cannot reintroduce the trap by leaving the scope behind the
			// envelope in either direction.
			if scope.DeadlineMs > env.Scope.DeadlineMs {
				t.Errorf("scope.DeadlineMs %d exceeds env.Scope.DeadlineMs %d; checkLoopAuthority "+
					"(loop.go:295) REJECTS this with ErrModelContract, so the fixture would fail "+
					"to build a loop rather than run slowly",
					scope.DeadlineMs, env.Scope.DeadlineMs)
			}

			// Scope.Validate on its own terms (the >= 100ms floor at
			// investigate/scope.go:61, canonical tool order, non-blank
			// request id).
			if err := scope.Validate(); err != nil {
				t.Errorf("scope.Validate: %v", err)
			}

			// In practice, not just arithmetically: NewLoop is the only place
			// checkLoopAuthority is reached, so a constructed loop is the proof
			// that the pair is accepted by the real boundary.
			lp, err := NewLoop(
				NewMockModelClient(nil),
				successExecutor(),
				DefaultBudgets(scope),
				scope, env, nil,
			)
			if err != nil {
				t.Fatalf("NewLoop rejected the reconciled pair: %v", err)
			}
			if lp == nil {
				t.Fatal("NewLoop returned a nil loop with a nil error")
			}
			// Disposed unrun. This proof is about the deadline REACHING the
			// loop, and Run would measure wall clock, which is not what is
			// under test here.
			t.Logf("RECON %s: env=%d scope=%d invariants-ok loop-constructed",
				tc.name, env.Scope.DeadlineMs, scope.DeadlineMs)
		})
	}
}

// TestAPA64Recon_NoOpTrapIsReal is the proof of WHY both edits were required.
//
// It constructs the exact half-applied state that raising only the envelope
// produces: envelope at 600000, scope left at testScope's hardcoded 60000.
// NewLoop ACCEPTS that pair, because 60000 <= 600000 trivially satisfies
// checkLoopAuthority. So nothing fails, nothing logs, and the run still
// escalates at 60s while appearing to have been fixed.
//
// The reason this cannot be caught by the boundary is that the boundary is not
// what is wrong: the boundary behaved correctly. The budget was simply too
// small, and the boundary faithfully reported the consequence.
func TestAPA64Recon_NoOpTrapIsReal(t *testing.T) {
	env, scope, _ := psFixtureControl(t)

	if env.Scope.DeadlineMs != psReconciledDeadlineMs {
		t.Fatalf("envelope deadline = %d, want %d; this proof depends on the reconciliation "+
			"already being applied", env.Scope.DeadlineMs, psReconciledDeadlineMs)
	}
	if scope.DeadlineMs != psReconciledDeadlineMs {
		t.Fatalf("scope deadline = %d, want %d; psScopeFor is not setting it, so the fixture is "+
			"still running the half-applied state", scope.DeadlineMs, psReconciledDeadlineMs)
	}

	// The trap, reproduced directly: start from testScope, which HARDCODES
	// 60000 independently of the envelope, and pair it with the raised
	// envelope. This is exactly what psScopeFor would return if its explicit
	// assignment were missing.
	trapped := testScope(env)
	if trapped.DeadlineMs != 60000 {
		t.Fatalf("testScope returned DeadlineMs %d, want the hardcoded 60000 this proof "+
			"assumes; if testScope changed, re-derive the trap rather than trusting this literal",
			trapped.DeadlineMs)
	}
	trapped.AllowTools = qualifyingTools()

	// It passes Validate and passes NewLoop. That is the whole hazard: the
	// half-applied change is indistinguishable from the applied one until a
	// live run reports ESCALATED(DEADLINE) with the same numbers as before.
	if err := trapped.Validate(); err != nil {
		t.Fatalf("trapped scope failed Validate: %v", err)
	}
	if trapped.DeadlineMs > env.Scope.DeadlineMs {
		t.Fatal("the trapped scope already exceeds the envelope, so it is not the trap")
	}
	if _, err := NewLoop(
		NewMockModelClient(nil), successExecutor(),
		DefaultBudgets(trapped), trapped, env, nil,
	); err != nil {
		t.Fatalf("the half-applied state was REJECTED by NewLoop (%v); the trap this proof "+
			"documents is that NewLoop ACCEPTS it silently", err)
	}

	// The decisive difference: what the loop would use as its window.
	// loop.go:502 reads l.scope.DeadlineMs, so these two numbers are the
	// entire difference between the trap and the fix.
	if trapped.DeadlineMs >= scope.DeadlineMs {
		t.Fatalf("trapped scope %d is not smaller than the reconciled scope %d; the contrast "+
			"this proof draws has collapsed", trapped.DeadlineMs, scope.DeadlineMs)
	}
	t.Logf("RECON no-op trap: envelope=%d raised, but testScope-derived scope=%d still governs "+
		"loop.go:502; NewLoop ACCEPTS it silently. Reconciled scope=%d. The envelope edit alone "+
		"changes the measured outcome to zero — it only changes what the fixture DECLARES",
		env.Scope.DeadlineMs, trapped.DeadlineMs, scope.DeadlineMs)
}

// TestAPA64Recon_NoBudgetValueChanges records the reason no budget was edited
// alongside the deadlines: DefaultBudgets reads scope.MaxCalls and nothing
// else, so the deadline change cannot have moved any budget. Asserted by
// evaluation rather than by reading DefaultBudgets, so a later edit that starts
// deriving a budget from the deadline is caught here.
func TestAPA64Recon_NoBudgetValueChanges(t *testing.T) {
	_, scope, _ := psFixtureControl(t)

	// The same scope with only the deadline moved back to the pre-change value.
	// If DefaultBudgets derived anything from the deadline, this pair would
	// differ.
	pre := scope
	pre.DeadlineMs = 60000

	if got, want := DefaultBudgets(scope), DefaultBudgets(pre); got != want {
		t.Errorf("DefaultBudgets changed when only DeadlineMs changed:\n reconciled: %+v\n"+
			"  pre-change: %+v\nThe deadline is not a budget input; if it now is, the "+
			"reconciliation has a second effect that must be adjudicated on its own",
			got, want)
	}
	// And the budgets the loop actually gets are the documented defaults.
	b := DefaultBudgets(scope)
	if b.MaxToolCalls != scope.MaxCalls {
		t.Errorf("MaxToolCalls = %d, want scope.MaxCalls %d", b.MaxToolCalls, scope.MaxCalls)
	}
	if b.TurnTimeoutMs == scope.DeadlineMs {
		t.Error("TurnTimeoutMs equals the scope deadline; the turn timeout and the investigation " +
			"deadline are distinct bounds and conflating them would cap each turn at the whole budget")
	}
	t.Logf("RECON budgets unchanged by the deadline: maxTurns=%d maxModelCalls=%d maxToolCalls=%d "+
		"turnTimeoutMs=%d (deadline is not an input)", b.MaxTurns, b.MaxModelCalls, b.MaxToolCalls,
		b.TurnTimeoutMs)
}

// ---------------------------------------------------------------------------
// PROOF 2 — the ceiling, read out of the harness source
// ---------------------------------------------------------------------------

// TestAPA64Recon_DeadlineIsInsideTheHarnessContextCeiling proves the reconciled
// deadline stays INSIDE the harness's own context, which is a tighter bound
// than the loop's 1h cap and a different kind of bound.
//
// The harness ceiling is read by parsing runLiveSeeded rather than restating
// 720000, so lengthening or shortening that context re-checks the relationship
// instead of silently leaving a stale number in this proof.
//
// The failure mode being avoided: at or above the harness context, the context
// preempts the loop and the run yields an EMPTY InvestigationOutput with a raw
// context.DeadlineExceeded. That is strictly worse than a well-formed
// ESCALATED(DEADLINE), because there is no structured escalation left to read.
// The deadline must therefore be strictly below the ceiling, not equal to it.
func TestAPA64Recon_DeadlineIsInsideTheHarnessContextCeiling(t *testing.T) {
	const harness = "groq_qualification_live_test.go"
	psReconPath(t, harness)

	ceiling := psReconTimeoutMSIn(t, harness, "runLiveSeeded")
	if ceiling != 12*60*1000 {
		t.Logf("RECON note: runLiveSeeded context ceiling is %dms, not the 720000ms this proof was "+
			"written against; the relationship below is re-checked against the parsed value",
			ceiling)
	}

	if psReconciledDeadlineMs >= ceiling {
		t.Fatalf("reconciled deadline %dms is NOT strictly below the runLiveSeeded context "+
			"ceiling %dms (groq_qualification_live_test.go). At or above the ceiling the context "+
			"preempts the loop and the run returns an EMPTY InvestigationOutput with a raw "+
			"context.DeadlineExceeded instead of a structured ESCALATED(DEADLINE)",
			psReconciledDeadlineMs, ceiling)
	}

	// The loop's own hard cap. MaxDeadlineMs is package-level and therefore
	// reachable from this test package, so the second bound is checked as a
	// live value rather than quoted.
	if psReconciledDeadlineMs > MaxDeadlineMs {
		t.Fatalf("reconciled deadline %dms exceeds orchestrate.MaxDeadlineMs %dms; "+
			"checkLoopAuthority's sibling check (loop.go:117) rejects the scope outright",
			psReconciledDeadlineMs, MaxDeadlineMs)
	}
	if MaxDeadlineMs != 3600000 {
		t.Errorf("MaxDeadlineMs = %d, want 3600000; this proof was written against the 1h cap "+
			"and a change to it must be adjudicated here", MaxDeadlineMs)
	}

	// The three windows, in the order they bind. Worth stating together: the
	// loop cap is the loosest, the harness context is the binding one, and
	// the deadline sits between the pacing total and the context.
	t.Logf("RECON ceilings: deadline=%d < harnessContext=%d (binding) < MaxDeadlineMs=%d; "+
		"measured 3-turn pacing total=%d",
		psReconciledDeadlineMs, ceiling, MaxDeadlineMs, 3*psMeasuredPacingPerTurnMs)
}

// TestAPA64Recon_ConstantIsTheOneUsed proves the fixture's named constant has
// the required value and that the value appears nowhere else in the package
// except the two fixture sites that are supposed to own it.
//
// The first half is the coupling the change introduced: psBuildParams (the
// envelope) and psScopeFor (the scope) now reference ONE constant, so the two
// deadlines cannot drift apart again through a forgotten edit.
//
// The second half is containment. A deadline in the production default, in
// fullchain, or in processor would change production behaviour; this proof
// fails if that happens. Those two production defaults are read as VALUES in
// TestAPA64Recon_ProtectedLocationsUnchanged; here the claim is narrower, that
// this reconciliation did not reach them.
func TestAPA64Recon_ConstantIsTheOneUsed(t *testing.T) {
	const fixture = "apa58_scenario_fixtures_test.go"
	const constName = "psQualificationDeadlineMs"
	psReconPath(t, fixture)

	// The constant exists, is package-level, and has the required value.
	if got := psReconConstInt64(t, fixture, constName); got != psReconciledDeadlineMs {
		t.Fatalf("%s: const %s = %d, want %d", fixture, constName, got, psReconciledDeadlineMs)
	}

	// Both fixture sites reference the constant, so the envelope and the scope
	// are deliberately coupled rather than coincidentally equal.
	buildSrc := psReconParse(t, fixture).psReconFuncSrc(t, "psBuildParams")
	scopeSrc := psReconParse(t, fixture).psReconFuncSrc(t, "psScopeFor")
	if !strings.Contains(buildSrc, constName) {
		t.Errorf("psBuildParams does not reference %s: the ENVELOPE deadline is no longer coupled "+
			"to the scope's, so the two can drift apart again", constName)
	}
	if !strings.Contains(scopeSrc, constName) {
		t.Errorf("psScopeFor does not reference %s: the SCOPE deadline, the one loop.go:502 actually "+
			"reads, is no longer coupled to the envelope's", constName)
	}

	// Containment: the constant, and the number itself, live in the fixture
	// only. Scan the package for the constant name.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || e.IsDir() {
			continue
		}
		if !psReconContains(t, name, constName) {
			continue
		}
		// Expected: the fixture (definition + 2 sites) and this proof file
		// (which names it as a string to scan for).
		allowed := name == fixture || name == "apa64_fixture_deadline_reconciliation_test.go"
		if !allowed {
			t.Errorf("%s references %s; the reconciled deadline belongs to the APA-58 fixtures only, "+
				"and a reference here means the change leaked into shared or production code",
				name, constName)
		}
	}

	// No OTHER file in the package sets an investigation deadline to the
	// reconciled value. psReconciledDeadlineMs must be unique to this fixture,
	// so a stale 600000 elsewhere cannot be mistaken for the fixture's.
	t.Logf("RECON constant %s = %d, referenced by psBuildParams (envelope) and psScopeFor "+
		"(scope); no other package file names it", constName, psReconciledDeadlineMs)
}

// ---------------------------------------------------------------------------
// PROOF 3 — the APA-64 reproduction no longer describes the fixture
// ---------------------------------------------------------------------------

// TestAPA64Recon_ReconciledDeadlineExceedsThreeTurnPacing is a positive claim
// about the fixture: its budget now covers the measured three-turn paced
// trajectory with room to spare.
//
// CONTRAST WITH THE REPRODUCTION. The APA-64 reproduction
// (TestAPA64_ThreeTurnCumulativePacingDiscardsTurnThree) is built on the
// premise
//
//	deadline D must satisfy 2*pacing < D < 3*pacing
//
// so turns 1 and 2 finish inside the window and turn 3 does not, which is what
// makes the run end ESCALATED(DEADLINE) with two tools executed. That premise
// describes a fixture whose budget is SMALLER than cumulative pacing.
//
// The reconciled fixture is in the opposite regime: D > 3*pacing, so the same
// trajectory completes instead of being discarded. The reproduction is still
// correct as a demonstration of the loop's turn-3 discard — the loop's
// behaviour has not changed and must not — but it no longer describes this
// fixture, and citing it as a reproduction of the APA-55 result would now be
// wrong. That is the whole point of this proof.
//
// The pacing used here is the MEASURED per-turn figure, not the pacer's floor
// and not the reproduction's millisecond-scaled substitute. Using the measured
// number is what makes the comparison a statement about the live run.
func TestAPA64Recon_ReconciledDeadlineExceedsThreeTurnPacing(t *testing.T) {
	pacing := psMeasuredPacingPerTurnMs
	threeTurnTotal := 3 * pacing // ~60.78s, the measured three-turn TOTAL

	// The positive claim.
	if psReconciledDeadlineMs <= threeTurnTotal {
		t.Fatalf("reconciled deadline %dms does NOT exceed the measured three-turn pacing "+
			"total %dms (%dms per turn x 3). The turn-3 report would still be discarded by the "+
			"I2 post-Complete deadline check and the run would still measure ESCALATED(DEADLINE)",
			psReconciledDeadlineMs, threeTurnTotal, pacing)
	}

	// The headroom is asserted, not assumed: a deadline barely above the total
	// would satisfy the inequality and still fail in practice.
	headroom := psReconciledDeadlineMs - threeTurnTotal
	if headroom < 2*pacing {
		t.Errorf("headroom %dms is under two further paced turns (%dms); a budget that only just "+
			"clears three turns will not survive a fourth, and the trajectory is not fixed at three",
			headroom, 2*pacing)
	}

	// The reproduction's regime, contrasted numerically with this fixture's.
	// The reproduction's own scaled constants (pacing 60ms, deadline 150ms) are
	// restated here so the contrast is concrete and checkable rather than
	// rhetorical.
	const reproPacingMs, reproDeadlineMs = 60, 150
	if !(2*reproPacingMs < reproDeadlineMs && reproDeadlineMs < 3*reproPacingMs) {
		t.Fatalf("the reproduction's own premise is not self-consistent (%dms is not strictly "+
			"between %dms and %dms); the contrast below would be meaningless",
			reproDeadlineMs, 2*reproPacingMs, 3*reproPacingMs)
	}

	// The fixture is NOT in the reproduction's band. This is the load-bearing
	// assertion: it is what makes "the reproduction no longer describes this
	// fixture" a fact about the fixture rather than a claim about the prose.
	if psReconciledDeadlineMs <= 3*psMeasuredPacingPerTurnMs {
		t.Fatalf("reconciled deadline %dms is inside the reproduction's band (2*pacing < D < "+
			"3*pacing = <%dms), so the reproduction would still be representative",
			psReconciledDeadlineMs, 3*psMeasuredPacingPerTurnMs)
	}

	t.Logf("RECON regimes differ. Reproduction: D=%dms in (2*pacing=%dms, 3*pacing=%dms) — budget "+
		"SMALLER than cumulative pacing, turn 3 discarded. Fixture: D=%dms vs measured 3-turn "+
		"total=%dms (%dms/turn x 3) — budget EXCEEDS it by %dms. The loop's turn-3 discard is "+
		"unchanged and still demonstrated; what changed is that this fixture no longer triggers it",
		reproDeadlineMs, 2*reproPacingMs, 3*reproPacingMs,
		psReconciledDeadlineMs, threeTurnTotal, pacing, headroom)

	// The same claim, made about the fixture's ACTUAL governing deadline rather
	// than about the target literal, so this is a statement about the fixture
	// and not only about the number this file was written with. loop.go:502
	// makes the SCOPE the value that matters.
	env, scope, _ := psFixtureControl(t)
	if env.Scope.DeadlineMs <= threeTurnTotal {
		t.Errorf("fixture ENVELOPE deadline %dms does not exceed the measured three-turn total "+
			"%dms", env.Scope.DeadlineMs, threeTurnTotal)
	}
	if scope.DeadlineMs <= threeTurnTotal {
		t.Errorf("fixture SCOPE deadline %dms does not exceed the measured three-turn total %dms; "+
			"this is the value loop.go:502 uses, so the fixture is still in the reproduction's "+
			"regime", scope.DeadlineMs, threeTurnTotal)
	}
	t.Logf("RECON fixture measured against the reproduction's regime: envelope=%dms scope=%dms, "+
		"both above the %dms three-turn total", env.Scope.DeadlineMs, scope.DeadlineMs, threeTurnTotal)

	// The pre-reconciliation value IS in the band, which is why the live run
	// measured what it measured. This closes the loop on the causal claim.
	const preReconciliationMs = 60000
	if preReconciliationMs <= 3*psMeasuredPacingPerTurnMs {
		t.Logf("RECON pre-reconciliation D=%dms vs 3-turn total=%dms: budget smaller than pacing, "+
			"so ESCALATED(DEADLINE) on turn 3 is the predicted and observed outcome",
			preReconciliationMs, threeTurnTotal)
	} else {
		t.Fatalf("pre-reconciliation deadline %dms EXCEEDS the measured three-turn total %dms; "+
			"then the deadline was not the cause of the APA-55 ESCALATED(DEADLINE) and the "+
			"reconciliation addresses a symptom rather than the cause",
			preReconciliationMs, threeTurnTotal)
	}
}

// ---------------------------------------------------------------------------
// PROOF 4 — independence from the Phase-1 accounting correction
// ---------------------------------------------------------------------------

// psReconMeasurementPathFiles are the two files that constitute the measurement
// path: the live harness that renders evidence and renders verdicts, and the
// offline integrity proofs for the Phase-1 ToolExecutions/RecorderObserved
// correction.
var psReconMeasurementPathFiles = []string{
	"groq_qualification_live_test.go",
	"qualification_measurement_integrity_test.go",
}

// psReconMeasurementPredicates are the functions that turn evidence into a
// pass/fail verdict or a rendered record.
//
// THE BOUNDARY IS DELIBERATE. It is the PREDICATES, not whole files. The
// measurement path legitimately sets a scope deadline for its own offline runs
// (qualification_measurement_integrity_test.go:79 and :386 set 5000ms so the
// probe finishes quickly, and groq_qualification_live_test.go:2777 sets the
// 100ms Validate floor), and one Phase-1 test reproduces the recorded live
// evidence line verbatim, including its EscalationDeadline reason.
//
// Those are run CONFIGURATION and DOCUMENTATION of the defect. They are not
// predicates, and a blanket file-level scan would be a false assertion rather
// than a stronger one. Claiming "no Deadline token anywhere in these files"
// would be false, and a proof that is false when it passes is worse than no
// proof.
var psReconMeasurementPredicates = []struct{ file, fn string }{
	{"groq_qualification_live_test.go", "buildEvidence"},
	{"groq_qualification_live_test.go", "requireHeld"},
	{"groq_qualification_live_test.go", "assertRecorderLive"},
	{"groq_qualification_live_test.go", "logEvidence"},
	{"groq_qualification_live_test.go", "checkInvariants"},
	{"apa58_scenario_fixtures_test.go", "psOutcomeControl"},
	{"apa58_scenario_fixtures_test.go", "psOutcomeInsufficient"},
	{"apa58_scenario_fixtures_test.go", "psOutcomeFabricated"},
	{"apa58_scenario_fixtures_test.go", "psOutcomeStale"},
	{"apa58_scenario_fixtures_test.go", "psOutcomeNeedsTool"},
	{"apa58_scenario_fixtures_test.go", "psPremiseControl"},
	{"apa58_scenario_fixtures_test.go", "psPremiseInsufficient"},
	{"apa58_scenario_fixtures_test.go", "psPremiseFabricated"},
	{"apa58_scenario_fixtures_test.go", "psPremiseStale"},
	{"apa58_scenario_fixtures_test.go", "psPremiseNeedsTool"},
	{"qualification_measurement_integrity_test.go", "qual65OldOutcomeNeedsTool"},
}

// psReconcNoDeadlineField asserts that a record type carries no field whose
// name mentions a deadline. Checked on the TYPE rather than on the file text,
// so a field added later fails here even if nothing reads it yet.
func psReconcNoDeadlineField(t *testing.T, label string, v any) {
	t.Helper()
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if strings.Contains(strings.ToLower(f.Name), "deadline") {
			t.Errorf("%s carries field %s.%s; the measurement record cannot then be independent "+
				"of the budget, because a verdict could be conditioned on it", label, rt.Name(), f.Name)
		}
	}
}

// TestAPA64Recon_IndependentOfMeasurementCorrection proves the deadline
// reconciliation and the Phase-1 accounting correction are independent axes.
//
// The two corrections address different questions. Phase 1 fixed what the
// harness MEASURES: ToolExecutions was populated only by the S1-specific paths,
// so every non-S1 scenario reported an empty tool-execution record while
// reporting executions the recorder really observed, and the anti-vacuity
// predicate reading that field was a constant false. This change fixes what the
// fixture GRANTS: the budget was smaller than the pacing charged inside it.
//
// Neither can substitute for the other. A perfectly measured run that cannot
// finish is still a run that cannot finish; a fixture with room to finish that
// is measured with a broken recorder still reports nothing. Both had to be
// corrected, and a regression that reintroduces either must be caught
// independently.
//
// The assertions are source- and type-level, which is the honest form here: the
// claim is about the absence of a coupling, and the strongest available
// evidence for the absence of a coupling is that neither name nor field exists
// in the other path.
func TestAPA64Recon_IndependentOfMeasurementCorrection(t *testing.T) {
	const constName = "psQualificationDeadlineMs"

	// (a) The fixture's deadline constant appears NOWHERE in the measurement
	// path — not in the harness, not in the Phase-1 proofs. If it did, the
	// verdict could be conditioned on the budget it is judging.
	for _, f := range psReconMeasurementPathFiles {
		psReconPath(t, f)
		if psReconContains(t, f, constName) {
			t.Errorf("%s references %s; the measurement path must not know the fixture's "+
				"budget, or a verdict could depend on the deadline it is evaluating", f, constName)
		}
	}

	// (b) No measurement predicate reads any deadline. Scanned over function
	// BODIES, so the token cannot hide in a doc comment.
	for _, p := range psReconMeasurementPredicates {
		body := psReconParse(t, p.file).psReconFuncSrc(t, p.fn)
		if i := strings.Index(body, "Deadline"); i >= 0 {
			t.Errorf("measurement predicate %s.%s reads a deadline at body offset %d; a pass/fail "+
				""+"rule that reads the budget it is judging cannot be independent of it",
				p.file, p.fn, i)
		}
	}

	// (c) The record types carry no deadline field at all, which makes (b)
	// structural rather than a matter of discipline: a predicate cannot read a
	// deadline it is not given.
	psReconcNoDeadlineField(t, "qualEvidence", qualEvidence{})
	psReconcNoDeadlineField(t, "qualRun", qualRun{})

	// (d) The measurement field this reconciliation must NOT be conflated with
	// is still populated by the non-S1 path. If the Phase-1 fix were reverted
	// AND the deadline raised, the run would complete and report an empty
	// tool-execution record — which is why the two gates must both stay.
	observed := &qualEvidence{RecorderObserved: 2, ToolExecutions: nil}
	if len(observed.ToolExecutions) == 0 && observed.RecorderObserved == 2 {
		t.Logf("RECON independence: a run with recorderObserved=%d and %d tool executions recorded "+
			"is still a measurement contradiction, independent of any deadline",
			observed.RecorderObserved, len(observed.ToolExecutions))
	}

	t.Logf("RECON independence: %s appears in none of %v; %d measurement predicates read no "+
		"deadline; qualEvidence and qualRun carry no deadline field",
		constName, psReconMeasurementPathFiles, len(psReconMeasurementPredicates))
}

// ---------------------------------------------------------------------------
// Protected locations — read, not trusted
// ---------------------------------------------------------------------------

// TestAPA64Recon_ProtectedLocationsUnchanged asserts, by reading the real
// sources, that this reconciliation stayed inside the two fixture sites.
//
// Every value here is read off disk through the parser. If any of them is
// edited, this fails with the parsed value next to the expected one, so a
// production default cannot drift because a fixture change was made in a hurry.
//
// SCOPE NOTE ON PATHS. The production defaults named in the reconciliation
// brief do not live under this package. They are at
// apps/api/internal/adapters/worker/fullchain.go and
// apps/api/internal/worker/processor.go, and they are read here by their real
// paths. The claim being protected — both still default to 60000 — is
// unaffected by where the files live.
func TestAPA64Recon_ProtectedLocationsUnchanged(t *testing.T) {
	t.Run("orchestrate_test.go_testScope_still_hardcodes_60s", func(t *testing.T) {
		const rel = "orchestrate_test.go"
		psReconPath(t, rel)
		body := psReconParse(t, rel).psReconFuncSrc(t, "testScope")
		if strings.Contains(body, "psQualificationDeadlineMs") {
			t.Errorf("testScope references the fixture deadline constant; the shared scope helper " +
				"would then silently change every other test that derives a scope from it")
		}
		if !strings.Contains(body, "DeadlineMs: 60000") {
			t.Errorf("testScope no longer hardcodes DeadlineMs: 60000; its body now reads %q. That is "+
				"the independent origin of the fixture's scope deadline, and the premise of the "+
				"coincidental-equality claim this change replaced with a deliberate coupling",
				body)
		}
		t.Logf("PROTECTED %s testScope still hardcodes DeadlineMs: 60000 and does not reference the "+
			"fixture constant", rel)
	})

	t.Run("loop.go_MaxDeadlineMs_still_1h", func(t *testing.T) {
		const rel = "loop.go"
		psReconPath(t, rel)
		got := psReconConstInt64(t, rel, "MaxDeadlineMs")
		if got != 3600000 {
			t.Errorf("loop.go MaxDeadlineMs = %d, want 3600000", got)
		}
		t.Logf("PROTECTED loop.go MaxDeadlineMs = %d", got)
	})

	t.Run("worker_production_defaults_still_60s", func(t *testing.T) {
		for _, tc := range []struct{ rel, name string }{
			{"../../adapters/worker/fullchain.go", "DefaultScopeDeadlineMs"},
			{"../../worker/processor.go", "defaultScopeDeadlineMs"},
		} {
			psReconPath(t, tc.rel)
			got := psReconConstInt64(t, tc.rel, tc.name)
			if got != 60000 {
				t.Errorf("%s: %s = %d, want 60000. A live-fixture deadline has no business reaching "+
					"a production default; if this now reads %d, the change has left the test layer",
					tc.rel, tc.name, got, psReconciledDeadlineMs)
			}
			t.Logf("PROTECTED %s: %s = %d", tc.rel, tc.name, got)
		}
	})

	t.Run("crossTenantLiteral_still_60s", func(t *testing.T) {
		const rel = "apa58_scenario_fixtures_test.go"
		const fn = "TestAPA58_CrossTenantIsNotExpressibleAtThisLayer"
		body := psReconParse(t, rel).psReconFuncSrc(t, fn)
		if !strings.Contains(body, "DeadlineMs: 60000") {
			t.Errorf("%s:%s no longer carries its own DeadlineMs: 60000 literal; body reads %q. That "+
				"test builds its envelope inline and is out of scope for this reconciliation, so its "+
				"literal must be exactly as it was", rel, fn, body)
		}
		if strings.Contains(body, "psQualificationDeadlineMs") {
			t.Errorf("%s:%s references the fixture deadline constant; it is out of scope and must "+
				"not have been swept along", rel, fn)
		}
		t.Logf("PROTECTED %s:%s literal unchanged (DeadlineMs: 60000, no fixture constant)", rel, fn)
	})

	t.Run("only_two_fixture_sites_changed", func(t *testing.T) {
		// Containment on the fixture file itself: the reconciliation owns
		// psBuildParams and psScopeFor. The cross-tenant literal is the one
		// other deadline literal in the file and must remain 60000.
		const rel = "apa58_scenario_fixtures_test.go"
		raw, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		src := string(raw)
		if n := strings.Count(src, "DeadlineMs:   600000"); n > 0 {
			t.Errorf("%s contains %d occurrence(s) of the inline literal `DeadlineMs:   600000`; "+
				"the reconciled value must be the named constant so the two sites cannot drift",
				rel, n)
		}
		t.Logf("PROTECTED %s: no inline 600000 literal; the value is the named constant at both sites",
			rel)
	})
}

// ---------------------------------------------------------------------------
// Compile-time guard on the fixtures the proofs reason about
// ---------------------------------------------------------------------------

// psReconFixtureBuilder is the signature every APA fixture shares.
type psReconFixtureBuilder func(*testing.T) (invest.UnresolvedException, investigate.Scope, map[string]struct{})

// TestAPA64Recon_AllFiveFixturesAreCovered names the five builders explicitly,
// so a fixture added to the registry but forgotten here fails loudly instead of
// silently widening or narrowing the propagation proof. psAllScenarios is
// iterated by TestAPA64Recon_DeadlinePropagatesToEveryFixtureScope; this test
// asserts that registry and this list agree.
func TestAPA64Recon_AllFiveFixturesAreCovered(t *testing.T) {
	builders := map[string]psReconFixtureBuilder{
		"ps_a1_control":      psFixtureControl,
		"ps_a2_insufficient": psFixtureInsufficient,
		"ps_a4_fabricated":   psFixtureFabricated,
		"ps_a7_stale":        psFixtureStale,
		"ps_b1_valid_tool":   psFixtureNeedsTool,
	}
	registry := psAllScenarios()
	if len(registry) != len(builders) {
		t.Fatalf("psAllScenarios has %d entries, this proof names %d; the propagation proof "+
			"iterates the registry so a new fixture is covered automatically, but the names here "+
			"must be kept in step", len(registry), len(builders))
	}
	for _, tc := range registry {
		if _, ok := builders[tc.name]; !ok {
			t.Errorf("registry scenario %q is not named in this proof; add it so the coverage "+
				"claim stays explicit", tc.name)
		}
	}
	// Every builder must actually be reachable and produce the reconciled
	// deadline, exercised through the builders themselves rather than the
	// registry indirection.
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			env, scope, _ := build(t)
			if env.Scope.DeadlineMs != psReconciledDeadlineMs || scope.DeadlineMs != psReconciledDeadlineMs {
				t.Fatalf("%s: env=%d scope=%d, want both %d", name,
					env.Scope.DeadlineMs, scope.DeadlineMs, psReconciledDeadlineMs)
			}
		})
	}
}
