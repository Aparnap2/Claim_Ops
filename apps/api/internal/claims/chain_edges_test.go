// Contract-chain edge witnesses for the five legal edges no other Go test
// drove at the claims seam, plus the stale-version precedence the state
// machine documents but no test observed.
//
// ADR-011 (docs/adr/011-claim-expiry-contract.md) makes claims.Transitions
// the sole authority for edge legality. Every listed edge is asserted
// positively: a missing edge leaves the transition silently impossible even
// though the fixture layer still passes, which is how the chain was allowed
// to hold unmatched edges before GOLDEN-14-19 were introduced.
//
// Spy-replay rule (docs/adr/001-go-first-edge.md): nil processor; no network.
package claims_test

import (
	"errors"
	"testing"
	"time"

	claims "claimops-api/internal/claims"
)

func TestChainEdgesRegisteredToDocumentsReceived(t *testing.T) {
	assertChainEdge(t, claims.ClaimStatusRegistered, claims.ClaimStatusDocumentsReceived)
}

func TestChainEdgesDocumentsReceivedToValidating(t *testing.T) {
	assertChainEdge(t, claims.ClaimStatusDocumentsReceived, claims.ClaimStatusValidating)
}

func TestChainEdgesValidatingToReadyForReview(t *testing.T) {
	assertChainEdge(t, claims.ClaimStatusValidating, claims.ClaimStatusReadyForReview)
}

func TestChainEdgesActionedToVerified(t *testing.T) {
	assertChainEdge(t, claims.ClaimStatusActioned, claims.ClaimStatusVerified)
}

func TestChainEdgesVerifiedToClosed(t *testing.T) {
	assertChainEdge(t, claims.ClaimStatusVerified, claims.ClaimStatusClosed)
}

// assertChainEdge witnesses one contract-chain edge: the claim is built at `from` with the
// version the caller must present, the transition must succeed, and the
// result must show exactly the new status with version bumped by one.
func assertChainEdge(t *testing.T, from, to claims.ClaimStatus) {
	t.Helper()
	c := mustClaim(t, from, 1, time.Time{}, time.Time{}, time.Time{})
	s, v, _ := snapshot(c)
	next, err := claims.Transition(c, to, "evt-chain-"+string(from)+"-"+string(to), 1, "tenant-01")
	if err != nil {
		t.Fatalf("Transition %s->%s: %v (want a legal edge)", s, to, err)
	}
	if next.Status != to {
		t.Fatalf("status = %s, want %s", next.Status, to)
	}
	if next.Version != v+1 {
		t.Fatalf("version = %d, want %d", next.Version, v+1)
	}
}

// The state machine documents that a stale version wins over edge legality
// (statemachine.go check 5 runs before checks 6/7). A stale version
// presented against an ILLEGAL edge must therefore come back STALE_VERSION,
// never ILLEGAL_TRANSITION: the caller must be told their view of the claim
// is out of date even when the transition they were attempting was
// worthless. ILLEGAL_TRANSITION would imply the claim view was current.
func TestChainEdgesStaleVersionWinsOverIllegalEdge(t *testing.T) {
	c := mustClaim(t, claims.ClaimStatusValidating, 3, time.Time{}, time.Time{}, time.Time{})
	_, err := claims.Transition(c, claims.ClaimStatusClosed, "evt-chain-stale-01", 2, "tenant-01")
	if err == nil {
		t.Fatal("Transition VALIDATING->CLOSED at a stale version succeeded, want an error")
	}
	var terr *claims.TransitionError
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v (%T), want *claims.TransitionError", err, err)
	}
	if terr.Code != claims.CodeStaleVersion {
		t.Fatalf("code = %q, want %q (stale version wins over an illegal edge)", terr.Code, claims.CodeStaleVersion)
	}
}
