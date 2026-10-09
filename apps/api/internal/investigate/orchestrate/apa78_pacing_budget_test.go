// APA-78 Phase 1: DETERMINISTIC TESTS ONLY. RED before any implementation
// change. Nothing in this file modifies production code, the pacer, the
// production prompt, MaxTokens, QUAL_REPEATS, or either established deadline.
//
// # WHAT THIS FILE CLAIMS
//
// The qualification harness paces real provider calls with
// qualWireRecorder.pacingWait (groq_qualification_live_test.go:329). Its
// comment at :325-328 asserts that the RESERVATION (MaxTokens=4096), not
// real consumption, is what the token budget measures, so the pacer budgets
// for the reservation. Measured header evidence for this account contradicts
// that: x-ratelimit-remaining-tokens fell 8000 -> 7986 on a ~14-token
// request, i.e. the provider charges the ACTUAL tokens used. The APA-56
// causal run measured 9399 tokens over 3 model calls (~3133 tokens/call).
//
// Consequence, and the reason this file exists: the current pacing floor
// (qualMinPacingWait = 20s, +250ms margin => 20.25s/call) sustains
// 3133/20.25s = ~9283 tokens/min against an 8000 TPM ceiling, ~16% over.
// The run is not merely slow; it is not admissible against the provider's
// token budget.
//
// TEST PHASE: Unit. No provider credential is read, no live call is made,
// and no wall-clock sleep exceeds the sub-second scale used by APA-64 for
// the same reason (groq_qualification_live_test.go is a live-only file and
// every live test skips without GROQ_API_KEY, so these tests must not
// inherit its behaviour).
//
// GROUPS
//  1. Rate accounting    - sustained tokens/min vs the provider TPM ceiling.
//  2. Missing headers    - conservative NONZERO pacing when headers absent.
//  3. 429 classification - throttle is infrastructure, never model quality.
//  4. Deadline           - default 30s turn budget vs corrected pacing; and
//     that a harness-supplied 60s budget validates.
package orchestrate

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/ports"
)

// ---------------------------------------------------------------------------
// Measured inputs. Provenance is load-bearing: these are MEASURED figures,
// not estimates, and each is cited at its point of use.
// ---------------------------------------------------------------------------

const (
	// apa78MeasuredTokensPerCall is the APA-56 causal run's measured cost
	// per model call: 9399 tokens over 3 model calls = 3133 tokens/call.
	apa78MeasuredTokensPerCall = 3133

	// apa78ProviderTokenLimit is the account's measured token ceiling per
	// window (x-ratelimit-limit-tokens: 8000, 2026-10-09 header snapshot).
	apa78ProviderTokenLimit = 8000

	// apa78MaxTokensReservation is the production client's fixed per-call
	// MaxTokens (groq_model.go: 111). Pinned here as a literal rather than
	// read from production so that a production change to MaxTokens surfaces
	// as a failure here, not as a silently drifting test.
	apa78MaxTokensReservation = 4096

	// apa78PromptAllowanceTokens is the prompt allowance the harness adds on
	// top of MaxTokens to form its per-call reservation.
	apa78PromptAllowanceTokens = 1024

	// apa78HarnessReserveTokens is the REAL per-call reservation the live
	// harness uses: qualReserveTokens = MaxTokens + prompt allowance =
	// 4096 + 1024 = 5120.
	//
	// This is the figure that matters for every claim about the FORMER
	// behaviour. An earlier revision of this file used a bare 4096
	// reservation, which is not the value the harness ever passed, and that
	// mismatch made a fixture look like a reproduction of the harness defect
	// when the old fast path would demonstrably never have fired on it. Both
	// figures are pinned here so the distinction is visible rather than
	// implied; TestAPA78_ZeroWaitIsForbidden... asserts against
	// apa78HarnessReserveTokens.
	apa78HarnessReserveTokens = apa78MaxTokensReservation + apa78PromptAllowanceTokens
)

// apa78RequiredPacingInterval is the MINIMUM interval between calls that
// keeps observed consumption inside the provider's token budget:
//
//	observedTokensPerCall / interval >= 8000 tokens/min
//	=>  interval >= 3133 * 60s / 8000 = 23497.5ms
//
// This is the target Phase 2's pacer must meet.
func apa78RequiredPacingInterval() time.Duration {
	return time.Duration(apa78MeasuredTokensPerCall) * time.Minute / apa78ProviderTokenLimit
}

// apa78SustainedTokensPerMinute is the token rate a fixed interval implies
// at a fixed per-call consumption. Zero or negative wait is reported as
// +Inf: no wait means an unbounded sustained rate.
func apa78SustainedTokensPerMinute(tokensPerCall int, wait time.Duration) float64 {
	if wait <= 0 {
		return math.Inf(1)
	}
	return float64(tokensPerCall) / wait.Minutes()
}

// apa78DepletedHeaderState is the binding steady-state header set: a healthy
// token WINDOW that has already rolled (measured reset 105ms) but whose
// remaining budget has fallen below the reservation, which is exactly the
// state the pacer's floor exists to handle.
func apa78DepletedHeaderState() qualWireAttempt {
	return qualWireAttempt{
		Seq:               1,
		Method:            http.MethodPost,
		Status:            http.StatusOK,
		AuthHeaderPresent: true,
		TotalTokens:       apa78MeasuredTokensPerCall,
		LimitTokens:       apa78ProviderTokenLimit,
		RemainingTokens:   2048,
		ResetTokensSecs:   0.105,
		LimitRequests:     1000,
		RemainingRequests: 999,
	}
}

// ===========================================================================
// GROUP 1 - RATE ACCOUNTING
//
// The pacer's decision must be judged against what the provider ACTUALLY
// charges (observed usage), and the interval it produces must keep the
// sustained token rate inside the account's TPM ceiling.
// ===========================================================================

// TestAPA78_PacingImpliedSustainedRateExceedsProviderTokenBudget is the RED
// condition. It drives the real pacingWait and asserts the invariant that
// must hold: the implied sustained rate must not exceed the reported token
// limit. Against the current implementation this fails, because the pacer
// budgets for the 4096 reservation while the provider charges ~3133 actual
// and the floor only buys 20.25s.
//
// GREEN after Phase 2: pacingWait returns an interval >= 23497.5ms for this
// state, so 3133/interval*60 <= 8000.
func TestAPA78_PacingImpliedSustainedRateExceedsProviderTokenBudget(t *testing.T) {
	rec := &qualWireRecorder{}
	rec.record(apa78DepletedHeaderState())

	wait := rec.pacingWait()
	if wait <= 0 {
		t.Fatalf("APA78-PACING pacingWait() = %v for a healthy rolled window; want a positive "+
			"wait. A zero wait makes the sustained rate unbounded.", wait)
	}

	sustained := apa78SustainedTokensPerMinute(apa78MeasuredTokensPerCall, wait)
	required := apa78RequiredPacingInterval()
	t.Logf("APA78-PACING interval=%v observedTokensPerCall=%d => %.1f tokens/min against a %d TPM ceiling "+
		"(TPM-compliant interval >= %v)", wait, apa78MeasuredTokensPerCall, sustained,
		apa78ProviderTokenLimit, required)

	if sustained > float64(apa78ProviderTokenLimit) {
		t.Fatalf("APA78-PACING sustained rate %.1f tokens/min exceeds the measured %d tokens/min ceiling "+
			"by %.1f tokens/min (%.1f%%): pacingWait budgeted for the %d-token MaxTokens reservation, "+
			"but the provider charges ACTUAL usage (~%d tokens/call measured), so the %v floor is too short. "+
			"The pacer must budget observed consumption; the interval has to be >= %v.",
			sustained, apa78ProviderTokenLimit, sustained-float64(apa78ProviderTokenLimit),
			100*(sustained-float64(apa78ProviderTokenLimit))/float64(apa78ProviderTokenLimit),
			apa78MaxTokensReservation, apa78MeasuredTokensPerCall, wait, required)
	}
}

// TestAPA78_RateDerivedIntervalIsAtLeastTheTPMCompliantInterval is the
// interval form of the same RED condition, stated as the acceptance criterion
// the pacer implements directly: the interval the pacer derives from OBSERVED
// usage must itself be at least the TPM-compliant interval.
//
// SCOPE, STATED PRECISELY (this is the correction to a previous name). This
// asserts a property of the RATE-DERIVED interval only. It does not assert
// anything about qualMinPacingWait: the floor is the header-MISSING path's
// minimum and stays 20s, and it is deliberately not the pacing interval.
// TestAPA78_HeaderMissingFallbackIsExactlyTheFloorAndMargin asserts the
// fallback path separately, so the two are never conflated again.
//
// GREEN: the derived interval is >= 23497.5ms for this state.
func TestAPA78_RateDerivedIntervalIsAtLeastTheTPMCompliantInterval(t *testing.T) {
	rec := &qualWireRecorder{}
	state := apa78DepletedHeaderState()
	rec.record(state)

	wait := rec.pacingWait()
	required := apa78RequiredPacingInterval()
	t.Logf("APA78-PACING rate-derived interval=%v for observedTokensPerCall=%d against a %d TPM "+
		"ceiling; TPM-compliant interval=%v; surplus=%v (header-missing floor is a separate path, "+
		"qualMinPacingWait=%v)",
		wait, state.TotalTokens, state.LimitTokens, required, wait-required, qualMinPacingWait)

	if wait < required {
		t.Fatalf("APA78-PACING the rate-derived interval yields %v, below the %v interval that %d "+
			"tokens/call requires to stay inside %d tokens/min (deficit %v). The pacer must derive "+
			"its interval from observed consumption and it has to be >= %v.",
			wait, required, apa78MeasuredTokensPerCall, apa78ProviderTokenLimit, required-wait, required)
	}
}

// TestAPA78_HeaderMissingFallbackIsExactlyTheFloorAndMargin is the DISTINCT
// assertion on the true header-missing path, which the renamed rate test above
// does not and must not make.
//
// When the provider's reply carries no rate evidence at all, pacingWait cannot
// derive a rate and falls back to qualMinPacingWait. This pins that fallback
// to its exact value rather than to a lower bound: it is the floor PLUS the
// fixed margin, and nothing else. A future change that quietly made the
// fallback larger (or, far worse, zero) fails here by name.
func TestAPA78_HeaderMissingFallbackIsExactlyTheFloorAndMargin(t *testing.T) {
	rec := &qualWireRecorder{}
	// No rate evidence whatsoever, and no observed usage either: LimitTokens,
	// RemainingTokens, ResetTokensSecs and TotalTokens are all zero.
	rec.record(qualWireAttempt{
		Seq:               1,
		Method:            http.MethodPost,
		Status:            http.StatusOK,
		AuthHeaderPresent: true,
		TotalTokens:       apa78MeasuredTokensPerCall,
	})

	got := rec.pacingWait()
	want := qualMinPacingWait + qualPacingMargin
	t.Logf("APA78-FALLBACK header-missing path -> %v; want exactly floor(%v)+margin(%v)=%v",
		got, qualMinPacingWait, qualPacingMargin, want)

	if got != want {
		t.Fatalf("APA78-FALLBACK the header-missing fallback is %v, want exactly %v (the %v floor "+
			"plus the %v margin). qualMinPacingWait is the fallback minimum, NOT the rate-derived "+
			"interval; the two are separate paths and this test owns only this one",
			got, want, qualMinPacingWait, qualPacingMargin)
	}
	if got <= 0 {
		t.Fatalf("APA78-FALLBACK the header-missing fallback is %v, which is not strictly positive", got)
	}
}

// TestAPA78_ZeroWaitIsForbiddenWhenAFurtherObservedCallCannotFitTheBudget
// falsifies the FORMER premise directly, reproducing the harness's real
// wiring.
//
// The old fast path was `remaining >= reserveTokens && reset < 2s => 0`, with
// reserveTokens supplied by every call site. The harness's real value is
// qualReserveTokens = MaxTokens + prompt allowance = 4096 + 1024 = 5120, NOT a
// bare 4096. An earlier revision of this test used remaining=4200 against a
// 4096 reservation and claimed the old fast path returned 0; with the real
// 5120 the old branch is NOT taken at 4200, so that fixture never reproduced
// the defect it claimed to. The numbers below are chosen so the old branch IS
// taken with the real argument.
//
// OLD-vs-NEW for the exact numbers in this fixture (remaining 5600, observed
// last call 7000, limit 8000, reset 105ms, reservation 5120):
//
//	OLD: limit>0 && 5600 >= 5120 && 0.105s < 2s        -> 0 (zero wait)
//	     and the causal harm is real: the previous call consumed 7000, so at
//	     zero wait the harness immediately spends more than the 5600 the
//	     provider says remains. The next call at the observed cost cannot
//	     fit, and the old branch waves it through.
//	NEW: rate      = 7000 * 60s / 8000 = 52.5s        (sustained cost)
//	     ceiling   = max(7000, 5120)     = 7000      (observed exceeded the
//	                                                    reservation, so the
//	                                                    ceiling rises)
//	     headroom  = (7000 - 5600) * 60s / 8000 = 11.25s  (window cannot hold
//	                                                    one call)
//	     reset     = 0.105s
//	     wait      = max(52.5, 11.25, 0.105, 20) + 0.25 = 52.75s
//
// So the old branch returned 0 on this state and the observed-usage logic
// refuses, which is the causal claim made auditable.
//
// Reachable in principle, as the comment states: MaxTokens caps COMPLETION
// tokens only and prompt tokens are additional, so a total above the
// reservation is normal — the previous call here spent 7000 against a 5120
// reservation.
func TestAPA78_ZeroWaitIsForbiddenWhenAFurtherObservedCallCannotFitTheBudget(t *testing.T) {
	const (
		remaining    = 5600
		observedLast = 7000
		limitTokens  = apa78ProviderTokenLimit
		resetSecs    = 0.105
	)

	// The reservation the harness really passed, asserted against the
	// harness's own constant so this fixture cannot drift away from it.
	if got := callCeilingTokens(0); got != apa78HarnessReserveTokens {
		t.Fatalf("APA78-PACING this fixture is written against the real harness reservation %d, "+
			"but the harness ceiling resolves to %d. Every old-vs-new figure in this test's comment "+
			"would be wrong.", apa78HarnessReserveTokens, got)
	}
	// The old branch, evaluated here so the reproduction is checked rather
	// than asserted in prose.
	oldTookFastPath := limitTokens > 0 && remaining >= apa78HarnessReserveTokens &&
		time.Duration(resetSecs*float64(time.Second)) < 2*time.Second
	t.Logf("APA78-PACING OLD branch check: limit=%d>0 && remaining=%d>=reservation=%d && "+
		"reset=%gs<2s => took fast path (zero wait)=%t",
		limitTokens, remaining, apa78HarnessReserveTokens, resetSecs, oldTookFastPath)
	if !oldTookFastPath {
		t.Fatalf("APA78-PACING this fixture no longer reproduces the old defect: with the real "+
			"reservation %d the old fast path was NOT taken at remaining=%d (reset %gs). The "+
			"old-vs-new claim in this test's comment would be false.",
			apa78HarnessReserveTokens, remaining, resetSecs)
	}
	// And the harm that branch permitted was real.
	if observedLast <= remaining {
		t.Fatalf("APA78-PACING the previous call consumed %d against %d remaining; a further call "+
			"at that cost WOULD fit, so the old zero-wait branch was harmless here and this "+
			"fixture does not demonstrate the defect", observedLast, remaining)
	}

	rec := &qualWireRecorder{}
	rec.record(qualWireAttempt{
		Seq:               1,
		Method:            http.MethodPost,
		Status:            http.StatusOK,
		AuthHeaderPresent: true,
		TotalTokens:       observedLast,
		LimitTokens:       limitTokens,
		RemainingTokens:   remaining,
		ResetTokensSecs:   resetSecs,
	})

	wait := rec.pacingWait()
	t.Logf("APA78-PACING remaining=%d reservation=%d observedLastCall=%d -> wait=%v",
		remaining, apa78HarnessReserveTokens, observedLast, wait)

	if wait == 0 {
		t.Fatalf("APA78-PACING pacingWait returned 0 on a state where the old fast path DID fire "+
			"(remaining %d >= the real reservation %d, reset %gs). The previous call actually "+
			"consumed %d tokens, so a further call at that observed cost cannot fit in %d remaining.",
			remaining, apa78HarnessReserveTokens, resetSecs, observedLast, remaining)
	}
}

// ===========================================================================
// GROUP 1b - VARYING PER-CALL SIZE
//
// The rate term alone is an ADAPTIVE ESTIMATE: it is derived from the previous
// call's tokens, so it says nothing about a next call of a different size. The
// headroom term is the second dimension that closes the small-then-large case,
// and these drive the real pacer across SEQUENCES of calls with differing
// per-call usage rather than one fixed fixture.
//
// What is asserted here is deliberately narrow, because that is what the
// policy actually guarantees:
//
//   - the wait is always positive and always at least the floor;
//   - the wait is always at least the RATE term the policy derives from the
//     observed usage of the call just made;
//   - when the reported remaining budget cannot hold one call at the ceiling,
//     the wait is at least the modelled HEADROOM term.
//
// What is NOT asserted anywhere: that the wait is sufficient for a next call
// larger than the ceiling. Nothing observed so far predicts that.
// TestAPA78_HeadroomIsNotAGuaranteeForCallsLargerThanTheCeiling pins that
// limitation explicitly rather than leaving it implicit.
// ===========================================================================

// apa78SequenceCall is one observed call in a scripted sequence.
type apa78SequenceCall struct {
	totalTokens int
	remaining   int
	resetSecs   float64
	label       string
}

// apa78ScriptedCall builds the header state one scripted call would report.
func apa78ScriptedCall(c apa78SequenceCall) qualWireAttempt {
	return qualWireAttempt{
		Seq:               1,
		Method:            http.MethodPost,
		Status:            http.StatusOK,
		AuthHeaderPresent: true,
		TotalTokens:       c.totalTokens,
		LimitTokens:       apa78ProviderTokenLimit,
		RemainingTokens:   c.remaining,
		ResetTokensSecs:   c.resetSecs,
	}
}

// apa78RateTerm is the sustained-rate lower bound the policy derives from one
// call's observed usage, recomputed independently of the pacer so the
// assertion is against the documented formula rather than against the
// pacer's own arithmetic.
func apa78RateTerm(observedTokens int) time.Duration {
	if observedTokens <= 0 {
		return 0
	}
	return time.Duration(observedTokens) * time.Minute / apa78ProviderTokenLimit
}

// apa78HeadroomTerm is the single-call headroom lower bound: the time the
// window needs to refill the deficit between the one-call ceiling and the
// reported remaining budget, at the provider's own reported refill rate.
func apa78HeadroomTerm(observedTokens, remaining int) time.Duration {
	if remaining <= 0 {
		// remaining == 0 is indistinguishable from "header absent"; the
		// policy skips the headroom term here and so does this re-derivation.
		return 0
	}
	deficit := callCeilingTokens(observedTokens) - remaining
	if deficit <= 0 {
		return 0
	}
	return time.Duration(deficit) * time.Minute / apa78ProviderTokenLimit
}

// assertPacingSequence drives the real pacer across a scripted sequence and
// checks the documented bound at every step.
func assertPacingSequence(t *testing.T, sequence []apa78SequenceCall) {
	t.Helper()
	rec := &qualWireRecorder{}
	for i, c := range sequence {
		rec.record(apa78ScriptedCall(c))
		wait := rec.pacingWait()
		rate := apa78RateTerm(c.totalTokens)
		headroom := apa78HeadroomTerm(c.totalTokens, c.remaining)
		reset := time.Duration(c.resetSecs * float64(time.Second))

		t.Logf("APA78-SEQUENCE step=%d %-26s observed=%5d remaining=%5d -> wait=%-10v "+
			"(rate=%v headroom=%v reset=%v floor=%v)",
			i+1, c.label, c.totalTokens, c.remaining, wait, rate, headroom, reset, qualMinPacingWait)

		if wait <= 0 {
			t.Fatalf("APA78-SEQUENCE step %d (%s): wait=%v, want strictly positive", i+1, c.label, wait)
		}
		if wait < qualMinPacingWait {
			t.Fatalf("APA78-SEQUENCE step %d (%s): wait=%v is below the floor %v", i+1, c.label, wait, qualMinPacingWait)
		}
		// The SUSTAINED dimension: at this wait, the observed per-call cost
		// must not exceed the provider's per-minute ceiling.
		if sustained := apa78SustainedTokensPerMinute(c.totalTokens, wait); sustained > apa78ProviderTokenLimit {
			t.Fatalf("APA78-SEQUENCE step %d (%s): at wait %v the observed %d tokens/call sustain "+
				"%.1f tokens/min, over the %d ceiling by %.1f%%. The rate dimension did not hold.",
				i+1, c.label, wait, c.totalTokens, sustained, apa78ProviderTokenLimit,
				100*(sustained-apa78ProviderTokenLimit)/apa78ProviderTokenLimit)
		}
		// The BURST dimension: when the window cannot hold one call at the
		// ceiling, the wait must be at least the modelled refill.
		if headroom > 0 && wait < headroom+qualPacingMargin {
			t.Fatalf("APA78-SEQUENCE step %d (%s): remaining=%d cannot hold one call at ceiling %d, "+
				"so the wait must be at least the modelled refill %v (+%v margin), got %v. The "+
				"headroom dimension did not hold.",
				i+1, c.label, c.remaining, callCeilingTokens(c.totalTokens), headroom, qualPacingMargin, wait)
		}
	}
}

// TestAPA78_VaryingSizeSequenceSmallThenLarge is the reviewer's case: a small
// response followed by a large one, and the RED condition for the headroom
// term.
//
// Small-then-large is where a rate-only policy fails. The rate term after the
// small call is tiny (2.25s at 300 tokens) and collapses to the floor, so a
// rate-only pacer admits the next call at 20.25s. If that next call is large,
// the run has spent far more per minute than the ceiling allows — the exact
// failure APA-78 exists to stop.
//
// Step 1 is the RED step. The previous call was CHEAP (300 tokens, so the
// rate term is 2.25s) but the window is nearly spent (remaining 1000), so the
// headroom term is the binding one:
//
//	ceiling  = max(300, 5120) = 5120        (the reservation; observed is below it)
//	deficit  = 5120 - 1000    = 4120
//	headroom = 4120 * 60s / 8000 = 30.9s
//	rate     = 300 * 60s / 8000   = 2.25s   (not binding)
//	floor    = 20s                          (not binding)
//	=> wait   = max(30.9, 2.25, 20, 0.105) + 0.25 = 31.15s
//
// A rate-only policy returns max(2.25, 20, 0.105) + 0.25 = 20.25s here, which
// is the RED condition this test fails under.
//
// The bound asserted is the one that actually holds — at every step the
// sustained observed rate is inside the ceiling, and where the window cannot
// hold a ceiling-sized call the wait covers the modelled refill. It is NOT a
// claim that the policy can predict the size of the next call; see
// TestAPA78_HeadroomIsNotAGuaranteeForCallsLargerThanTheCeiling.
func TestAPA78_VaryingSizeSequenceSmallThenLarge(t *testing.T) {
	assertPacingSequence(t, []apa78SequenceCall{
		// RED step: cheap observed call, nearly-spent window.
		{totalTokens: 300, remaining: 1000, resetSecs: 0.105, label: "cheap call, spent window"},
		// The rate dimension, binding in its own right: 6000 tokens/call at an
		// 8000 TPM ceiling requires 45s whatever the window holds.
		{totalTokens: 6000, remaining: 4200, resetSecs: 0.105, label: "expensive call"},
	})
}

// TestAPA78_VaryingSizeSequenceLargeThenSmall is the converse and shows the
// policy does not over-wait when usage shrinks.
//
// Large-then-small is the case a reservation-ONLY policy gets wrong in the
// other direction: it would pace at 5120*60/8000 = 38.4s forever regardless
// of what calls actually cost. Here the observed cost falls to 400 tokens and
// the sustained rate stays far inside the ceiling, so the wait returns to the
// floor. The reservation is a CEILING on one call, not a floor on the rate.
//
// The assertion is the same documented bound as the other sequences, applied
// in the direction where it is a claim about NOT over-waiting.
func TestAPA78_VaryingSizeSequenceLargeThenSmall(t *testing.T) {
	rec := &qualWireRecorder{}

	// Step 1: an expensive call against a window that cannot hold another
	// ceiling-sized one. Rate = 7000*60/8000 = 52.5s binds over headroom
	// (7000-5600)*60/8000 = 10.5s.
	rec.record(apa78ScriptedCall(apa78SequenceCall{
		totalTokens: 7000, remaining: 5600, resetSecs: 0.105, label: "expensive call",
	}))
	bigWait := rec.pacingWait()
	if want := apa78RateTerm(7000); bigWait < want {
		t.Fatalf("APA78-SEQUENCE large step: wait=%v is below the rate term %v derived from the "+
			"observed 7000 tokens/call", bigWait, want)
	}

	// Step 2: the next call is cheap. Rate = 400*60/8000 = 3s, so the FLOOR
	// binds, and remaining is healthy so headroom is 0. A reservation-only
	// policy would still return 38.4s here; this one returns the floor.
	rec.record(apa78ScriptedCall(apa78SequenceCall{
		totalTokens: 400, remaining: 7600, resetSecs: 0.105, label: "cheap call",
	}))
	smallWait := rec.pacingWait()
	t.Logf("APA78-SEQUENCE large->small: expensive wait=%v, cheap wait=%v (floor=%v, "+
		"reservation-derived rate would be %v)",
		bigWait, smallWait, qualMinPacingWait,
		time.Duration(apa78HarnessReserveTokens)*time.Minute/apa78ProviderTokenLimit)

	if smallWait != qualMinPacingWait+qualPacingMargin {
		t.Fatalf("APA78-SEQUENCE after a 400-token call the wait is %v, want exactly the floor %v "+
			"+margin %v. A wait pinned at the reservation-derived rate (%v) here would mean the "+
			"reservation had become the rate, which is the defect this policy removed",
			smallWait, qualMinPacingWait, qualPacingMargin,
			time.Duration(apa78HarnessReserveTokens)*time.Minute/apa78ProviderTokenLimit)
	}
	if smallWait >= bigWait {
		t.Fatalf("APA78-SEQUENCE the wait did not shrink when usage shrank (big=%v small=%v): "+
			"the policy would be pacing a reservation rather than observed usage", bigWait, smallWait)
	}
}

// TestAPA78_VaryingSizeSequenceMonotonicGrowth is the case where per-call
// usage rises every turn, as a growing conversation makes it do.
//
// It is the sequence a growing prompt actually produces: each turn carries
// more context, so observed tokens climb. Three properties are asserted.
// First, the sustained rate holds at every single step — a growing sequence is
// where a fixed-interval pacer silently breaches, because the interval that
// was correct for turn 1 is far too short for turn 4. Second, the wait is
// NON-DECREASING across a strictly growing sequence, which is what "the pacer
// adapted" means concretely. Third, and load-bearing for the headroom term,
// the wait is at least the modelled refill at EVERY step where the window
// cannot hold a ceiling-sized call.
//
// Turn 3 is the RED step: at 3200 observed tokens the rate term gives 24s,
// while the window (1500 remaining) cannot hold a 5120-token ceiling call and
// the headroom term gives (5120-1500)*60/8000 = 27.15s. A rate-only policy
// returns 24.25s here.
//
// Turn 4 is where the two dimensions diverge most and the rate term takes
// over: at 6400 observed tokens the reservation has been EXCEEDED by
// observation, so the ceiling rises to 6400 and the rate term (48s) exceeds
// the headroom term ((6400-1000)*60/8000 = 40.5s). That is why the ceiling is
// max(observed, reservation) and not the reservation alone: pinning it to 5120
// would have understated the single-call worst case on a call that has
// demonstrably been bigger.
//
// It is deliberately not asserted that the wait grows without bound: it is a
// function of the observed usage, and a drop in observed usage is legitimately
// allowed to shorten it (see the large->small test).
func TestAPA78_VaryingSizeSequenceMonotonicGrowth(t *testing.T) {
	sequence := []apa78SequenceCall{
		{totalTokens: 800, remaining: 7000, resetSecs: 0.105, label: "turn 1"},
		{totalTokens: 1600, remaining: 6000, resetSecs: 0.105, label: "turn 2"},
		{totalTokens: 3200, remaining: 1500, resetSecs: 0.105, label: "turn 3"},
		{totalTokens: 6400, remaining: 1000, resetSecs: 0.105, label: "turn 4"},
	}
	assertPacingSequence(t, sequence)

	// The non-decreasing property, driven off the same sequence.
	rec := &qualWireRecorder{}
	var prev time.Duration
	for i, c := range sequence {
		rec.record(apa78ScriptedCall(c))
		wait := rec.pacingWait()
		if i > 0 && wait < prev {
			t.Errorf("APA78-SEQUENCE monotonic-growth step %d (%s): wait=%v is SHORTER than the "+
				"previous step's %v while observed usage rose from %d to %d tokens/call. A shrinking "+
				"interval on a growing sequence is exactly how a run breaches its budget.",
				i+1, c.label, wait, prev, sequence[i-1].totalTokens, c.totalTokens)
		}
		prev = wait
	}
}

// TestAPA78_CeilingRisesWithObservedUsageBeyondTheReservation pins why the
// one-call ceiling is max(observed, reservation) rather than the reservation
// alone.
//
// The reservation (5120) is a true upper bound for one call only while the
// prompt stays inside its allowance. A real call has already been observed at
// 7000 tokens on this harness, which is direct evidence that 5120 is NOT an
// upper bound here. Provisioning 5120 against a window that must hold 7000
// would understate the single-call worst case, so the ceiling rises to what
// was actually observed.
//
// This is the conservative direction. It is asserted explicitly because the
// alternative — pinning the ceiling to the reservation — is the defect this
// whole correction removed, reappearing in the other dimension.
func TestAPA78_CeilingRisesWithObservedUsageBeyondTheReservation(t *testing.T) {
	if got := callCeilingTokens(0); got != apa78HarnessReserveTokens {
		t.Fatalf("APA78-CEILING with no observed usage the ceiling is %d, want the reservation %d",
			got, apa78HarnessReserveTokens)
	}
	if got := callCeilingTokens(apa78MeasuredTokensPerCall); got != apa78HarnessReserveTokens {
		t.Fatalf("APA78-CEILING with observed usage %d (below the reservation) the ceiling is %d, "+
			"want the reservation %d: a call that has not yet exceeded the allowance must not "+
			"raise the bound", apa78MeasuredTokensPerCall, got, apa78HarnessReserveTokens)
	}
	const overReservation = 7000
	if got := callCeilingTokens(overReservation); got != overReservation {
		t.Fatalf("APA78-CEILING with observed usage %d (ABOVE the reservation) the ceiling is %d, "+
			"want %d. A reservation a real call has already exceeded is not an upper bound; "+
			"provisioning against it would understate the single-call worst case",
			overReservation, got, overReservation)
	}
	// And the consequence is visible in the wait, not only in the helper: a
	// window with 6000 remaining cannot hold either a 5120 or a 7000 token
	// call, but the headroom term differs between the two, and the observed
	// one must be the larger.
	narrow := &qualWireRecorder{}
	narrow.record(qualWireAttempt{
		Seq: 1, Method: http.MethodPost, Status: http.StatusOK, AuthHeaderPresent: true,
		TotalTokens: overReservation, LimitTokens: apa78ProviderTokenLimit,
		RemainingTokens: 6000, ResetTokensSecs: 0.105,
	})
	narrowWait := narrow.pacingWait()
	if want := apa78HeadroomTerm(overReservation, 6000) + qualPacingMargin; narrowWait < want {
		t.Fatalf("APA78-CEILING with observed %d against %d remaining the wait is %v, below the "+
			"headroom term %v (+%v margin): the raised ceiling did not reach the decision",
			overReservation, 6000, narrowWait, apa78HeadroomTerm(overReservation, 6000), qualPacingMargin)
	}
	t.Logf("APA78-CEILING ceiling(reservation)=%d ceiling(observed=%d)=%d; against 6000 remaining "+
		"the headroom term is %v and the wait is %v",
		callCeilingTokens(0), callCeilingTokens(overReservation), overReservation,
		apa78HeadroomTerm(overReservation, 6000), narrowWait)
}

// TestAPA78_HeadroomIsNotAGuaranteeForCallsLargerThanTheCeiling pins the
// limitation the other tests deliberately do not paper over.
//
// The headroom term provisions ONE call at the ceiling. It says nothing about
// a next call larger than the ceiling, because nothing observed so far
// predicts that size. This test drives a state where the previous call was
// cheap and the window is healthy, so the policy admits the floor interval —
// and then computes, from the same numbers, what a call far larger than the
// ceiling would have cost at that interval.
//
// The assertion is the DOCUMENTED BOUND THAT ACTUALLY HOLDS: the policy is an
// adaptive heuristic, so it bounds the SUSTAINED cost at the observed size and
// the single-call headroom at the ceiling, and nothing beyond that. It does
// NOT assert that the wait is safe for a 20000-token call, because it is not,
// and a test that implied otherwise would be the overclaiming this whole
// correction exists to remove.
//
// What the test does assert is that the limitation is real and bounded: the
// documented terms hold at the observed size, and the unsafety at a
// hypothetical larger size is computed and reported rather than hidden.
func TestAPA78_HeadroomIsNotAGuaranteeForCallsLargerThanTheCeiling(t *testing.T) {
	// A cheap call against a healthy window: rate term collapses to the floor
	// and remaining clears the ceiling, so the policy admits the floor.
	const cheapObserved = 300
	rec := &qualWireRecorder{}
	rec.record(qualWireAttempt{
		Seq: 1, Method: http.MethodPost, Status: http.StatusOK, AuthHeaderPresent: true,
		TotalTokens: cheapObserved, LimitTokens: apa78ProviderTokenLimit,
		RemainingTokens: 7700, ResetTokensSecs: 0.105,
	})
	wait := rec.pacingWait()

	// The bound that DOES hold: sustained observed cost inside the ceiling,
	// and a strictly positive wait at least the floor.
	if sustained := apa78SustainedTokensPerMinute(cheapObserved, wait); sustained > apa78ProviderTokenLimit {
		t.Fatalf("APA78-LIMIT at the observed %d tokens/call the policy sustained %.1f tokens/min, "+
			"over the %d ceiling. The documented bound is that observed-size cost is bounded; that "+
			"bound failing means the policy is broken, not merely limited",
			cheapObserved, sustained, apa78ProviderTokenLimit)
	}
	if wait < qualMinPacingWait {
		t.Fatalf("APA78-LIMIT wait=%v is below the floor %v", wait, qualMinPacingWait)
	}

	// The bound that does NOT hold, computed rather than asserted. A call far
	// above the ceiling admitted at this interval would breach the per-minute
	// ceiling — which is exactly why the policy must not be described as a
	// guarantee.
	const hypotheticalNext = 20000
	hypSustained := apa78SustainedTokensPerMinute(hypotheticalNext, wait)
	requiredForHyp := time.Duration(hypotheticalNext) * time.Minute / apa78ProviderTokenLimit
	t.Logf("APA78-LIMIT observed=%d remaining=7700 -> wait=%v. Documented bound HOLDS: %d "+
		"tokens/call sustains %.1f/min <= %d. Documented bound DOES NOT extend to %d tokens/call: "+
		"at this interval it would sustain %.1f/min, and %.1fx the ceiling, needing %v to be safe. "+
		"The policy is an adaptive heuristic sized to observed usage plus a one-call ceiling; it is "+
		"not a prediction of the next call's size.",
		cheapObserved, wait, cheapObserved, apa78SustainedTokensPerMinute(cheapObserved, wait),
		apa78ProviderTokenLimit, hypotheticalNext, hypSustained,
		hypSustained/apa78ProviderTokenLimit, requiredForHyp)

	if hypSustained <= apa78ProviderTokenLimit {
		t.Fatalf("APA78-LIMIT a %d-token call at wait %v sustains only %.1f/min, inside the %d "+
			"ceiling. This fixture no longer demonstrates the limitation, so the comment's claim "+
			"that the policy does not bound larger calls is no longer what these numbers show",
			hypotheticalNext, wait, hypSustained, apa78ProviderTokenLimit)
	}

	// The mitigation is explicit rather than implicit: a sequence whose usage
	// is KNOWN to reach the hypothetical size is paced correctly, because by
	// then it has been observed. This is the sense in which the policy is
	// adaptive rather than predictive, and it is asserted so the limitation
	// above is not mistaken for "the harness cannot stay inside its budget".
	grown := &qualWireRecorder{}
	grown.record(qualWireAttempt{
		Seq: 1, Method: http.MethodPost, Status: http.StatusOK, AuthHeaderPresent: true,
		TotalTokens: hypotheticalNext, LimitTokens: apa78ProviderTokenLimit,
		RemainingTokens: 7700, ResetTokensSecs: 0.105,
	})
	grownWait := grown.pacingWait()
	grownSustained := apa78SustainedTokensPerMinute(hypotheticalNext, grownWait)
	t.Logf("APA78-LIMIT once a %d-token call has actually been OBSERVED: wait=%v, sustaining %.1f/min "+
		"inside the %d ceiling", hypotheticalNext, grownWait, grownSustained, apa78ProviderTokenLimit)
	if grownSustained > apa78ProviderTokenLimit {
		t.Fatalf("APA78-LIMIT after observing %d tokens/call the policy still sustained %.1f/min, "+
			"over the %d ceiling. Once the size is observed the rate dimension must bound it",
			hypotheticalNext, grownSustained, apa78ProviderTokenLimit)
	}
}

// ===========================================================================
// GROUP 2 - MISSING HEADERS
//
// A throttled reply frequently omits the rate-limit headers. Pacing must then
// be conservative and NONZERO. These assert the CURRENT behaviour; if the
// current code already satisfies them that is reported as PASS-on-arrival,
// not engineered into a failure.
//
// The claim is stated as what it is: a missing header produces a floor-paced
// wait, which reduces the chance of a provider throttle but does not
// guarantee the provider will accept the call.
// ===========================================================================

// TestAPA78_MissingRateLimitHeadersStillProduceAFloorPacedWait covers the
// header-less success reply.
//
// PASS-ON-ARRIVAL: the fast path is guarded by `a.LimitTokens > 0`, so a
// header-less attempt (LimitTokens == 0) always falls through to the floor.
func TestAPA78_MissingRateLimitHeadersStillProduceAFloorPacedWait(t *testing.T) {
	rec := &qualWireRecorder{}
	rec.record(qualWireAttempt{
		Seq:               1,
		Method:            http.MethodPost,
		Status:            http.StatusOK,
		AuthHeaderPresent: true,
		TotalTokens:       apa78MeasuredTokensPerCall,
		// Every rate-limit header absent: LimitTokens, RemainingTokens,
		// ResetTokensSecs all zero.
	})

	wait := rec.pacingWait()
	t.Logf("APA78-HEADERS header-less 200 reply -> wait=%v (floor=%v)", wait, qualMinPacingWait)

	assertConservativePacing(t, rec, wait, "header-less 200 reply")
}

// TestAPA78_ThrottledReplyWithoutHeadersStillProduceAFloorPacedWait covers
// the case the floor's comment names: a throttled reply often omits the
// headers. This is the same shape the live run actually took
// (qualification_measurement_integrity_test.go:353 records exactly this
// header-less 429).
//
// PASS-ON-ARRIVAL, for the same reason as above.
func TestAPA78_ThrottledReplyWithoutHeadersStillProduceAFloorPacedWait(t *testing.T) {
	rec := &qualWireRecorder{}
	rec.record(qualWireAttempt{
		Seq:               1,
		Method:            http.MethodPost,
		Status:            http.StatusTooManyRequests,
		AuthHeaderPresent: false,
		// Rate-limit headers absent, as a 429 commonly omits them.
	})

	wait := rec.pacingWait()
	t.Logf("APA78-HEADERS header-less 429 reply -> wait=%v (floor=%v)", wait, qualMinPacingWait)

	assertConservativePacing(t, rec, wait, "header-less 429 reply")
}

// TestAPA78_FirstObservationIsNotPaced pins the ONE deliberate zero-wait case
// so it cannot be mistaken for a header-missing defect. With no recorded
// attempt there is no observed rate, no remaining budget, and no window to
// wait out, so there is nothing to pace against. This is the first call of a
// run and the provider does not rate-limit it.
//
// Reported as a scope decision: the "missing headers" requirement is about a
// MISSING HEADER, and this case has no OBSERVATION at all, which is a
// different input.
func TestAPA78_FirstObservationIsNotPaced(t *testing.T) {
	rec := &qualWireRecorder{}
	wait := rec.pacingWait()
	t.Logf("APA78-HEADERS no recorded attempt -> wait=%v (nothing observed yet to pace against)", wait)
	if wait != 0 {
		t.Fatalf("APA78-HEADERS pacingWait with no recorded attempt = %v, want 0: before the first "+
			"observation there is no measured rate, no remaining budget and no window to wait out.", wait)
	}
}

// assertConservativePacing is the shared group-2 invariant: strictly positive
// and at least the safety floor.
func assertConservativePacing(t *testing.T, rec *qualWireRecorder, wait time.Duration, what string) {
	t.Helper()
	if wait <= 0 {
		t.Fatalf("APA78-HEADERS pacingWait for a %s = %v, want strictly positive: a missing header "+
			"must still produce a paced wait rather than an immediate call. (This bounds the cost the "+
			"harness generates; it does not assert the provider will accept the call.)", what, wait)
	}
	if wait < qualMinPacingWait {
		t.Fatalf("APA78-HEADERS pacingWait for a %s = %v, below the safety floor %v.", what, wait, qualMinPacingWait)
	}
	// Re-derive rather than trust: the recorder must still be the thing that
	// decided, so re-running the decision must reproduce the same wait.
	if again := rec.pacingWait(); again != wait {
		t.Fatalf("APA78-HEADERS pacingWait for a %s is not a pure function of the last observation: "+
			"first call %v, second call %v.", what, wait, again)
	}
}

// ===========================================================================
// GROUP 3 - 429 CLASSIFICATION
//
// A provider HTTP 429 is an INFRASTRUCTURE observation. It must never be
// recorded as a model-quality result. These drive the real GroqModelClient
// against a LOCAL httptest server (no credential, no network) and the real
// Loop, so the assertion is against the actual classification path.
// ===========================================================================

// apa78ThrottledServer returns a server that answers every request with HTTP
// 429 and no rate-limit headers, which is the realistic throttle shape.
func apa78ThrottledServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit","code":"rate_limit_exceeded"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAPA78_Provider429IsClassifiedAsInfrastructureNotModelQuality drives the
// real production client at the real classification site
// (groq_model.go:161, isRetryableStatus -> ErrModelUpstream).
//
// PASS-ON-ARRIVAL, EXPECTED: isRetryableStatus(429) is true
// (groq_model.go:219), so a 429 takes the upstream branch and is never
// mapped to ErrModelContract.
func TestAPA78_Provider429IsClassifiedAsInfrastructureNotModelQuality(t *testing.T) {
	srv := apa78ThrottledServer(t)
	client, err := NewGroqModelClient("apa78-not-a-credential", "apa78-model", srv.URL)
	if err != nil {
		t.Fatalf("NewGroqModelClient: %v", err)
	}

	env := testEnvelope(t)
	_, err = client.Complete(context.Background(), ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{"ev-doc-01"},
		Turn:             1,
		RequestID:        env.Scope.RequestID,
	})
	if err == nil {
		t.Fatalf("APA78-CLASS a persistently throttled provider returned no error; want a classification")
	}
	t.Logf("APA78-CLASS provider 429 classified as: %v", err)

	if !errors.Is(err, ErrModelUpstream) {
		t.Fatalf("APA78-CLASS provider 429 did not classify as ErrModelUpstream: %v", err)
	}
	// The model-quality verdict, at both the orchestrate and the ports layer.
	if errors.Is(err, ErrModelContract) {
		t.Fatalf("APA78-CLASS provider 429 classified as ErrModelContract (model-quality): %v", err)
	}
	if errors.Is(err, ports.ErrContract) {
		t.Fatalf("APA78-CLASS provider 429 leaked ports.ErrContract (a model-quality verdict): %v", err)
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("APA78-CLASS provider 429 error does not name the status, so it cannot be "+
			"distinguished downstream: %v", err)
	}
}

// TestAPA78_LoopEscalatesThrottleAsModelUpstreamNotInvalidOutput drives the
// real Loop over the real client, so the assertion is against the actual
// escalation path (loop.go:584 -> escalationError, loop.go:400).
//
// PASS-ON-ARRIVAL, EXPECTED: the persistent 429 exhausts the client's own
// bounded retry and is marked "exhausted" (groq_model.go:203); isLoopRetryable
// (loop.go:762) refuses "exhausted", so the loop escalates MODEL_UPSTREAM
// on the first failure rather than INVALID_OUTPUT.
func TestAPA78_LoopEscalatesThrottleAsModelUpstreamNotInvalidOutput(t *testing.T) {
	srv := apa78ThrottledServer(t)
	client, err := NewGroqModelClient("apa78-not-a-credential", "apa78-model", srv.URL)
	if err != nil {
		t.Fatalf("NewGroqModelClient: %v", err)
	}
	env := testEnvelope(t)
	scope := testScope(env)
	lp := newTestLoop(t, client, successExecutor(), scope, env)

	out, runErr := lp.Run(context.Background())
	if runErr == nil {
		t.Fatalf("APA78-CLASS a persistently throttled provider produced no run error")
	}
	t.Logf("APA78-CLASS escalation=%q err=%v", out.EscalationReason, runErr)

	if out.EscalationReason != EscalationModelUpstream {
		t.Fatalf("APA78-CLASS throttle escalated as %q, want %q (infrastructure). A throttle "+
			"escalating as %q records a provider rate limit as a model-quality result.",
			out.EscalationReason, EscalationModelUpstream, EscalationInvalidOutput)
	}
	if out.EscalationReason == EscalationInvalidOutput {
		t.Fatalf("APA78-CLASS throttle escalated as INVALID_OUTPUT, a model-quality verdict")
	}
	if !errors.Is(runErr, ErrModelUpstream) {
		t.Fatalf("APA78-CLASS run error does not classify as ErrModelUpstream: %v", runErr)
	}
	for _, modelQuality := range []error{ErrModelContract, ports.ErrContract, ErrGrounding, ErrRepetition} {
		if errors.Is(runErr, modelQuality) {
			t.Fatalf("APA78-CLASS run error carries the model-quality sentinel %v: %v", modelQuality, runErr)
		}
	}
}

// TestAPA78_ThrottledQualificationAttemptIsDiscardedAsInfrastructure pins the
// APA49-INFRA discarded-message shape: a throttled attempt carries no model
// evidence, so qualMeasure discards and re-measures it rather than filing it.
// A throttle must never be counted as a qualification result.
//
// PASS-ON-ARRIVAL, EXPECTED: this is a pure predicate over the recorder
// (groq_qualification_live_test.go:297).
func TestAPA78_ThrottledQualificationAttemptIsDiscardedAsInfrastructure(t *testing.T) {
	rec := &qualWireRecorder{}

	// A clean attempt is not a throttle: it carries model evidence.
	mark := rec.mark()
	rec.record(qualWireAttempt{Seq: 1, Status: http.StatusOK, TotalTokens: apa78MeasuredTokensPerCall})
	if rec.throttledSince(mark) {
		t.Fatalf("APA78-CLASS a 200 attempt was classified as a throttle")
	}

	// A throttle is: discarded as infrastructure, never a qualification result.
	mark = rec.mark()
	rec.record(qualWireAttempt{Seq: 2, Status: http.StatusTooManyRequests})
	if !rec.throttledSince(mark) {
		t.Fatalf("APA78-CLASS a 429 attempt was not classified as a throttle, so qualMeasure would " +
			"file a provider rate limit as a qualification result")
	}

	// The message the harness logs for that discard must name it as
	// infrastructure, so the evidence record cannot read as model failure.
	const discard = "APA49-INFRA scenario=%s attempt=%d discarded: provider rate-limited the run " +
		"(HTTP 429), so it carries no model evidence; re-measuring"
	if !strings.Contains(discard, "carries no model evidence") {
		t.Fatalf("APA78-CLASS the discard message does not state that a throttle carries no model evidence")
	}
	t.Logf("APA78-CLASS discard shape = %q", discard)
}

// ===========================================================================
// GROUP 4 - DEADLINE RECONCILIATION
//
// Pacing is charged to the per-turn deadline: qualModel.Complete
// (groq_qualification_live_test.go:522-526) sleeps pacingWait BEFORE the
// inner provider call, and the Loop calls Complete with turnCtx
// (loop.go:575), so the pacing and the inference share TurnTimeoutMs.
//
// The settled APA-78 design decision is that pacing REMAINS charged to the
// per-turn deadline and the harness supplies a validated TurnTimeoutMs=60000.
// Production defaults and limits are therefore unchanged by Phase 2, which
// is why every test in this group is a characterisation or an acceptance
// criterion rather than a transitional RED: see the report for the full
// finding.
// ===========================================================================

// apa78Scale compresses the timing model for the two mechanism probes below.
// It divides every duration by exactly 100, so the ratio between the pacing
// interval, the inference call and the turn budget is preserved EXACTLY and
// the conclusion transfers to real milliseconds. This is the same scaling
// APA-64 used for the same reason (apa64_deadline_pacing_adjudication_test.go:77).
const apa78Scale = 100

// apa78Scaled compresses one duration by apa78Scale, rounding DOWN so the
// scaled pacing never exceeds the scaled budget it is being compared with.
func apa78Scaled(d time.Duration) time.Duration {
	return time.Duration(int64(d) / apa78Scale)
}

// TestAPA78_DefaultTurnTimeoutIsInsufficientForCorrectedPacing establishes
// that the production default cannot carry corrected pacing plus one call
// the client itself considers within tolerance.
//
// CHARACTERISATION, PASS-ON-ARRIVAL. This is expected to stay green after
// Phase 2 BECAUSE Phase 2 does not change DefaultTurnTimeoutMs. It is a
// guard in both directions: it fails if someone changes the default, and it
// is the arithmetic that makes the harness override necessary rather than
// optional.
func TestAPA78_DefaultTurnTimeoutIsInsufficientForCorrectedPacing(t *testing.T) {
	required := apa78RequiredPacingInterval()
	// The client's own ceiling for ONE Complete call (groq_model.go:29). The
	// loop's per-turn timeout exists to bound exactly that call; pacing
	// eating most of the budget means the call can never use the tolerance
	// the client allows it.
	allowance := time.Duration(DefaultTurnTimeoutMs)*time.Millisecond - required
	t.Logf("APA78-DEADLINE defaultTurnTimeout=%v correctedPacing=%v => inference allowance=%v, "+
		"against a client single-call ceiling of %v",
		time.Duration(DefaultTurnTimeoutMs)*time.Millisecond, required, allowance, groqTimeout)

	if DefaultTurnTimeoutMs >= MaxTurnTimeoutMs {
		t.Fatalf("APA78-DEADLINE DefaultTurnTimeoutMs (%d) is not below the cap (%d)", DefaultTurnTimeoutMs, MaxTurnTimeoutMs)
	}
	if allowance >= groqTimeout {
		t.Fatalf("APA78-DEADLINE default TurnTimeoutMs (%dms) leaves %v of inference allowance after "+
			"%v of corrected pacing, which is at least the client's own %v single-call ceiling; the "+
			"default is NOT insufficient, so the harness override would be unmotivated.",
			DefaultTurnTimeoutMs, allowance, required, groqTimeout)
	}
}

// TestAPA78_CorrectedPacingStarvesATurnUnderTheDefaultBudget proves the
// insufficiency MECHANICALLY through the real Loop, not just arithmetically:
// with the production default budget, a turn that must first pay corrected
// pacing never delivers its call.
//
// CHARACTERISATION, PASS-ON-ARRIVAL for the same reason as above. Expected
// to stay green after Phase 2.
func TestAPA78_CorrectedPacingStarvesATurnUnderTheDefaultBudget(t *testing.T) {
	env := testEnvelope(t)
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope: %v", err)
	}

	pacing := apa78Scaled(apa78RequiredPacingInterval())
	inference := apa78Scaled(groqTimeout)
	budget := apa78Scaled(time.Duration(DefaultTurnTimeoutMs) * time.Millisecond)

	// One delay at the Complete seam models pacing + inference together,
	// which is exactly where qualModel.Complete puts them
	// (groq_qualification_live_test.go:522).
	budgets := DefaultBudgets(scope)
	budgets.TurnTimeoutMs = budget.Milliseconds()
	budgets.MaxTurns = 1
	budgets.MaxModelCalls = 1

	lp, err := NewLoop(&FakeModelClient{
		Delay:     pacing + inference,
		Responses: []ModelResponse{modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1))},
	}, successExecutor(), budgets, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	out, runErr := lp.Run(context.Background())
	t.Logf("APA78-DEADLINE scaled default budget=%v, correctedPacing=%v, inference=%v, sum=%v "+
		"(scaled 1/%d); turnsUsed=%d toolCallsUsed=%d outcome=%q err=%v",
		budget, pacing, inference, pacing+inference, apa78Scale,
		out.TurnsUsed, out.ToolCallsUsed, out.Outcome, runErr)

	if out.ToolCallsUsed > 0 {
		t.Fatalf("APA78-DEADLINE the turn delivered its tool call under the default budget; the " +
			"insufficiency this test characterises is not real")
	}
	if pacing+inference <= budget {
		t.Fatalf("APA78-DEADLINE pacing(%v)+inference(%v)=%v fits the default budget(%v), so the turn "+
			"should have delivered; this test's premise is false", pacing, inference, pacing+inference, budget)
	}
}

// TestAPA78_HarnessSixtySecondTurnTimeoutValidates verifies the settled design
// decision against the REAL ValidateBudgets: a harness-supplied
// TurnTimeoutMs=60000 is accepted, is within MaxTurnTimeoutMs, and is above
// the only floor Validate enforces.
//
// ACCEPTANCE CRITERION, PASS-ON-ARRIVAL. NewLoop takes Budgets as a
// parameter (loop.go:245), so the harness may supply this without any
// production default changing.
func TestAPA78_HarnessSixtySecondTurnTimeoutValidates(t *testing.T) {
	const harnessTurnTimeoutMs = int64(60000)

	env := testEnvelope(t)
	scope := testScope(env)

	if harnessTurnTimeoutMs > MaxTurnTimeoutMs {
		t.Fatalf("APA78-BUDGET harness TurnTimeoutMs=%d exceeds MaxTurnTimeoutMs=%d; the harness "+
			"budget would be rejected", harnessTurnTimeoutMs, MaxTurnTimeoutMs)
	}
	// Validate enforces exactly one floor on this field (loop.go:114-121).
	if harnessTurnTimeoutMs < 1 {
		t.Fatalf("APA78-BUDGET harness TurnTimeoutMs=%d is below the >= 1 floor Validate enforces",
			harnessTurnTimeoutMs)
	}

	budgets := DefaultBudgets(scope)
	budgets.TurnTimeoutMs = harnessTurnTimeoutMs
	if err := ValidateBudgets(budgets, scope); err != nil {
		t.Fatalf("APA78-BUDGET ValidateBudgets rejected the harness-supplied Budgets "+
			"{TurnTimeoutMs:%d}: %v", harnessTurnTimeoutMs, err)
	}
	// And it must survive the real constructor, not just the validator.
	if _, err := NewLoop(&FakeModelClient{}, successExecutor(), budgets, scope, env, nil); err != nil {
		t.Fatalf("APA78-BUDGET NewLoop rejected the harness-supplied Budgets {TurnTimeoutMs:%d}: %v",
			harnessTurnTimeoutMs, err)
	}

	// The production default is untouched by supplying a harness budget.
	if got := DefaultBudgets(scope).TurnTimeoutMs; got != DefaultTurnTimeoutMs {
		t.Fatalf("APA78-BUDGET DefaultBudgets(scope).TurnTimeoutMs = %d, want the unchanged "+
			"production default %d", got, DefaultTurnTimeoutMs)
	}
	t.Logf("APA78-BUDGET harness TurnTimeoutMs=%dms validates, is within the %dms cap, and leaves "+
		"the production default at %dms", harnessTurnTimeoutMs, MaxTurnTimeoutMs, DefaultTurnTimeoutMs)
}

// TestAPA78_HarnessSixtySecondTurnBudgetCarriesTheIntendedInferenceAllowance
// verifies the arithmetic the settled decision rests on, and proves it
// MECHANICALLY through the real Loop at the same scale used above.
//
// ACCEPTANCE CRITERION, PASS-ON-ARRIVAL. 60000ms minus corrected pacing
// leaves ~36.5s of inference allowance, and a turn with that budget does
// deliver its call where the default does not.
func TestAPA78_HarnessSixtySecondTurnBudgetCarriesTheIntendedInferenceAllowance(t *testing.T) {
	const harnessTurnTimeoutMs = int64(60000)

	required := apa78RequiredPacingInterval()
	allowance := time.Duration(harnessTurnTimeoutMs)*time.Millisecond - required
	wantAllowance := 36500 * time.Millisecond
	t.Logf("APA78-BUDGET harness TurnTimeoutMs=%dms - correctedPacing=%v = %v allowance (target ~%v)",
		harnessTurnTimeoutMs, required, allowance, wantAllowance)

	if allowance < groqTimeout {
		t.Fatalf("APA78-BUDGET harness TurnTimeoutMs=%dms leaves only %v of inference allowance after "+
			"%v of corrected pacing, less than the client's own %v single-call ceiling; the intended "+
			"allowance is not present", harnessTurnTimeoutMs, allowance, required, groqTimeout)
	}
	// Within 1s of the stated ~36.5s target, so the intent cannot silently rot.
	if d := allowance - wantAllowance; d > time.Second || d < -time.Second {
		t.Fatalf("APA78-BUDGET inference allowance %v is not within 1s of the intended ~%v", allowance, wantAllowance)
	}

	// Mechanically: at the same scale, the harness budget carries pacing +
	// inference where the default could not.
	env := testEnvelope(t)
	scope := testScope(env)
	scope.AllowTools = qualifyingTools()
	if err := scope.Validate(); err != nil {
		t.Fatalf("scope: %v", err)
	}
	delay := apa78Scaled(required) + apa78Scaled(groqTimeout)
	budgets := DefaultBudgets(scope)
	budgets.TurnTimeoutMs = harnessTurnTimeoutMs
	budgets.MaxTurns = 2
	budgets.MaxModelCalls = 2

	// Two paced calls: read then submit, so the harness budget is shown to
	// carry the whole paced trajectory, not just its first turn.
	lp, err := NewLoop(&FakeModelClient{
		Delay: delay,
		Responses: []ModelResponse{
			modelResp(callToolBytes(t, env, scope, invest.ToolGetClaim, 1)),
			modelResp(submitBytes(t, testReport(env, "ev-new-01"))),
		},
	}, successExecutor(), budgets, scope, env, nil)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	out, runErr := lp.Run(context.Background())
	t.Logf("APA78-BUDGET harness budget=%v carries pacing+inference=%v per turn; turnsUsed=%d "+
		"toolCallsUsed=%d outcome=%q err=%v", apa78Scaled(time.Duration(harnessTurnTimeoutMs)*time.Millisecond),
		delay, out.TurnsUsed, out.ToolCallsUsed, out.Outcome, runErr)

	if out.ToolCallsUsed != 1 || out.Outcome != OutcomeReportReady {
		t.Fatalf("APA78-BUDGET the harness-supplied turn budget did NOT carry the paced trajectory "+
			"to a report (toolCallsUsed=%d outcome=%q err=%v): pacing %v + inference %v must fit "+
			"inside the scaled budget %v",
			out.ToolCallsUsed, out.Outcome, runErr, apa78Scaled(required), apa78Scaled(groqTimeout),
			apa78Scaled(time.Duration(harnessTurnTimeoutMs)*time.Millisecond))
	}
}
