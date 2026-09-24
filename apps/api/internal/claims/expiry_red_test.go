package claims_test

// S5 RED: HITL-expiry state contract (APA-26).
//
//   - HITL -> EXPIRED and ACTION_PENDING -> EXPIRED are the only legal
//     expiry edges (timeout/system actor, idempotent hitl-expiry key).
//   - EXPIRED has zero out-edges: terminal-for-round. Post-expiry
//     APPROVE/REJECT/ESCALATE/HOLD are ILLEGAL (timeout never approves).
//   - Pre-existing pin: APPROVE from HITL is ALREADY illegal (HITL has no
//     VERIFIED edge), so no timeout path can smuggle approval.

import (
	"errors"
	"testing"
	"time"

	"claimops-api/internal/claims"
)

func expiryClaim(t *testing.T, id, tenant string, status claims.ClaimStatus) claims.Claim {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	c, err := claims.NewClaim(
		claims.ClaimID(id), claims.TenantID(tenant),
		claims.PolicyID("pol-s5-01"), "REF-S5-01",
		claims.MustPaise(1000, 0), status, 1, now, now, time.Time{},
	)
	if err != nil {
		t.Fatalf("new claim: %v", err)
	}
	return *c
}

func expiryTransitionCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var terr *claims.TransitionError
	if !errors.As(err, &terr) {
		t.Fatalf("err type = %T, want *TransitionError", err)
	}
	return terr.Code
}

func TestExpiry_HITLToExpired_Legal(t *testing.T) {
	c := expiryClaim(t, "clm-s5-exp-01", "tnt-s5", claims.ClaimStatusHITL)
	next, err := claims.Transition(c, claims.ClaimStatusExpired, "hitl-expiry:inv-01", 1, "tnt-s5")
	if err != nil {
		t.Fatalf("HITL->EXPIRED: %v (want legal expiry edge)", err)
	}
	if next.Status != claims.ClaimStatusExpired || next.Version != 2 {
		t.Fatalf("got %q v%d, want EXPIRED v2", next.Status, next.Version)
	}
}

func TestExpiry_ActionPendingToExpired_Legal(t *testing.T) {
	c := expiryClaim(t, "clm-s5-exp-02", "tnt-s5", claims.ClaimStatusActionPending)
	next, err := claims.Transition(c, claims.ClaimStatusExpired, "hitl-expiry:inv-02", 1, "tnt-s5")
	if err != nil {
		t.Fatalf("ACTION_PENDING->EXPIRED: %v (want legal expiry edge)", err)
	}
	if next.Status != claims.ClaimStatusExpired {
		t.Fatalf("got %q, want EXPIRED", next.Status)
	}
}

func TestExpiry_NoOutEdges(t *testing.T) {
	for _, to := range []claims.ClaimStatus{
		claims.ClaimStatusVerified, claims.ClaimStatusClosed,
		claims.ClaimStatusException, claims.ClaimStatusHITL,
		claims.ClaimStatusActionPending, claims.ClaimStatusReceived,
	} {
		c := expiryClaim(t, "clm-s5-exp-03", "tnt-s5", claims.ClaimStatusExpired)
		err := func() error {
			_, err := claims.Transition(c, to, "ev-s5-"+string(to), 1, "tnt-s5")
			return err
		}()
		if code := expiryTransitionCode(t, err); code != claims.CodeIllegalTransition {
			t.Fatalf("EXPIRED->%q code = %q, want ILLEGAL_TRANSITION (terminal-for-round)", to, code)
		}
	}
}

func TestExpiry_ApproveFromHITL_Illegal(t *testing.T) {
	c := expiryClaim(t, "clm-s5-exp-04", "tnt-s5", claims.ClaimStatusHITL)
	_, err := claims.Transition(c, claims.ClaimStatusVerified, "ev-s5-approve", 1, "tnt-s5")
	if code := expiryTransitionCode(t, err); code != claims.CodeIllegalTransition {
		t.Fatalf("HITL->VERIFIED code = %q, want ILLEGAL_TRANSITION (no approval path from HITL)", code)
	}
}
