package claims_test

// Terminal-status contract for the authoritative Go lifecycle.
//
// Scope: apps/api/internal/claims only. Per ADR-001
// (docs/adr/001-go-first-edge.md) this package is authoritative for claim
// lifecycle, and packages/domain (Python) is a demoted specification
// artifact -- deliberately not a mirror of this graph. This file therefore
// pins the Go contract and makes no cross-language parity claim.
//
// Terminality is enforced by absence from claims.Transitions: a missing key
// (or an empty inner map) means no outbound transition is legal. The
// terminal set is consequently a contract in its own right, and it is
// asserted here against hardcoded literals so that an edit to the
// transition map cannot silently move the expectation.
//
// Coverage gap this closes: EXPIRED terminality is pinned by
// expiry_red_test.go and CLOSED is pinned by golden case 07
// (fixtures/golden_cases.json, CLOSED -> DOCUMENTS_RECEIVED), but
// READY_FOR_REVIEW -- also terminal -- was referenced by no test at all.
//
// Determinism: no randomness, no network, no time.Now. Dates are fixed
// literals parsed with time.Parse; money is exact int64 paise.

import (
	"errors"
	"testing"
	"time"

	"claimops-api/internal/claims"
)

// terminalStatuses is the contract: the only lifecycle states with no legal
// outgoing transition. Hardcoded so the expectation cannot be derived from
// the implementation under test.
var terminalStatuses = []claims.ClaimStatus{
	claims.ClaimStatusReadyForReview,
	claims.ClaimStatusClosed,
	claims.ClaimStatusExpired,
}

// allStatuses is the full recognized lifecycle, hardcoded for the same
// reason: a test that enumerates the implementation cannot detect a status
// the implementation has dropped.
var allStatuses = []claims.ClaimStatus{
	claims.ClaimStatusReceived,
	claims.ClaimStatusRegistered,
	claims.ClaimStatusDocumentsReceived,
	claims.ClaimStatusValidating,
	claims.ClaimStatusReadyForReview,
	claims.ClaimStatusException,
	claims.ClaimStatusInvestigationRequired,
	claims.ClaimStatusWaitingForEvidence,
	claims.ClaimStatusHITL,
	claims.ClaimStatusActionPending,
	claims.ClaimStatusActioned,
	claims.ClaimStatusVerified,
	claims.ClaimStatusClosed,
	claims.ClaimStatusExpired,
}

func terminalClaim(t *testing.T, id string, status claims.ClaimStatus) claims.Claim {
	t.Helper()
	day, err := time.Parse("2006-01-02", "2026-01-15")
	if err != nil {
		t.Fatalf("parse fixed date: %v", err)
	}
	c, err := claims.NewClaim(
		claims.ClaimID(id), claims.TenantID("tnt-terminal"),
		claims.PolicyID("pol-terminal-01"), "REF-TERMINAL-01",
		claims.MustPaise(1000, 0), status, 1, day, day, time.Time{},
	)
	if err != nil {
		t.Fatalf("new claim at %q: %v", status, err)
	}
	return *c
}

func terminalTransitionCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var terr *claims.TransitionError
	if !errors.As(err, &terr) {
		t.Fatalf("err type = %T, want *claims.TransitionError", err)
	}
	return terr.Code
}

// TestTerminalStatuses_NoLegalOutgoingTransition asserts that every
// terminal status rejects a transition to every recognized status,
// including itself. This is the property the transition map encodes by
// omission, asserted directly against the public Transition API.
func TestTerminalStatuses_NoLegalOutgoingTransition(t *testing.T) {
	for _, from := range terminalStatuses {
		for _, to := range allStatuses {
			c := terminalClaim(t, "clm-term-"+string(from), from)
			_, err := claims.Transition(c, to, "ev-term-"+string(from)+"-"+string(to), 1, "tnt-terminal")
			if code := terminalTransitionCode(t, err); code != claims.CodeIllegalTransition {
				t.Fatalf("%q -> %q code = %q, want ILLEGAL_TRANSITION (terminal status has no out-edges)",
					from, to, code)
			}
		}
	}
}

// TestTerminalStatuses_ExactlyTheDeclaredSet asserts the declared terminal
// set is exactly the set of statuses with no outgoing edge. It fails if a
// status is added to the transition map without also being added to the
// contract here, and it fails if a declared terminal is given an out-edge.
func TestTerminalStatuses_ExactlyTheDeclaredSet(t *testing.T) {
	declared := make(map[claims.ClaimStatus]bool, len(terminalStatuses))
	for _, s := range terminalStatuses {
		declared[s] = true
	}
	for _, s := range allStatuses {
		hasOutEdge := false
		for _, target := range allStatuses {
			if claims.CanTransition(s, target) {
				hasOutEdge = true
				break
			}
		}
		if declared[s] && hasOutEdge {
			t.Fatalf("%q is declared terminal but has a legal outgoing transition", s)
		}
		if !declared[s] && !hasOutEdge {
			t.Fatalf("%q is not declared terminal but has no legal outgoing transition", s)
		}
	}
}

// TestNonTerminalStatuses_HaveAtLeastOneOutgoingEdge is the negative
// control for the test above: every non-terminal status must actually be
// able to move, so a terminal can never be declared by accident and the
// terminality loop cannot pass vacuously.
func TestNonTerminalStatuses_HaveAtLeastOneOutgoingEdge(t *testing.T) {
	declared := make(map[claims.ClaimStatus]bool, len(terminalStatuses))
	for _, s := range terminalStatuses {
		declared[s] = true
	}
	for _, s := range allStatuses {
		if declared[s] {
			continue
		}
		found := false
		for _, target := range allStatuses {
			if claims.CanTransition(s, target) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%q is not terminal but cannot transition anywhere", s)
		}
	}
}

// TestReadyForReview_IsReachableThenTerminal pins READY_FOR_REVIEW
// specifically: it is a reachable outcome of validation, and once reached
// it is terminal. Before this file it was referenced by no test.
func TestReadyForReview_IsReachableThenTerminal(t *testing.T) {
	c := terminalClaim(t, "clm-term-rfr-01", claims.ClaimStatusValidating)
	next, err := claims.Transition(c, claims.ClaimStatusReadyForReview, "ev-term-rfr-ready", 1, "tnt-terminal")
	if err != nil {
		t.Fatalf("VALIDATING -> READY_FOR_REVIEW: %v (want legal edge)", err)
	}
	if next.Status != claims.ClaimStatusReadyForReview {
		t.Fatalf("got %q, want READY_FOR_REVIEW", next.Status)
	}
	if next.Version != 2 {
		t.Fatalf("got version %d, want 2", next.Version)
	}
	for _, to := range allStatuses {
		_, err := claims.Transition(next, to, "ev-term-rfr-"+string(to), 2, "tnt-terminal")
		if code := terminalTransitionCode(t, err); code != claims.CodeIllegalTransition {
			t.Fatalf("READY_FOR_REVIEW -> %q code = %q, want ILLEGAL_TRANSITION (terminal)", to, code)
		}
	}
}
