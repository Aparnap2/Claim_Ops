// Package claims implements the deterministic claim lifecycle core.
//
// This file is the Go port of the Python specification in
// packages/domain/state_machine.py, bound by the behavioral contract in
// fixtures/golden_cases.json and ADR-001 (docs/adr/001-go-first-edge.md):
// the Go edge is authoritative for claim lifecycle, idempotency, and
// concurrency control.
//
// Design rules: pure value semantics (Transition copies in, copies out and
// never mutates its input), STDLIB ONLY (no web-framework imports in domain
// code), and a fixed check order — malformed, tenant, event ID, idempotent
// replay, version, known statuses, legality — so that golden cases 07
// (illegal-transition), 08 (stale-version wins over legality) and 09
// (idempotent replay returns the claim unchanged) hold.
//
// Canonical entity types (Claim, ClaimStatus and its ClaimStatus* constants,
// TenantID, ClaimID) and the idempotency helpers (Claim.HasEvent,
// Claim.MarkEvent over the ProcessedEvents field) live in claim.go; this
// file only adds the transition graph and the Transition function.
package claims

import (
	"fmt"
	"strings"
)

// Transitions is the allowed contract chain. A missing key (or an empty
// inner map) means terminal: no outbound transition is legal.
// READY_FOR_REVIEW and CLOSED are terminal and therefore absent.
var Transitions = map[ClaimStatus]map[ClaimStatus]bool{
	ClaimStatusReceived: {
		ClaimStatusRegistered: true,
	},
	ClaimStatusRegistered: {
		ClaimStatusDocumentsReceived: true,
	},
	ClaimStatusDocumentsReceived: {
		ClaimStatusValidating: true,
	},
	ClaimStatusValidating: {
		ClaimStatusReadyForReview: true,
		ClaimStatusException:      true,
	},
	ClaimStatusException: {
		ClaimStatusInvestigationRequired: true,
	},
	ClaimStatusInvestigationRequired: {
		ClaimStatusWaitingForEvidence: true,
	},
	ClaimStatusWaitingForEvidence: {
		ClaimStatusHITL: true,
	},
	ClaimStatusHITL: {
		ClaimStatusActionPending: true,
	},
	ClaimStatusActionPending: {
		ClaimStatusActioned: true,
	},
	ClaimStatusActioned: {
		ClaimStatusVerified: true,
	},
	ClaimStatusVerified: {
		ClaimStatusClosed: true,
	},
}

// CanTransition reports whether moving from one status to another is legal
// per the contract chain. Unknown or terminal statuses return false.
func CanTransition(from, to ClaimStatus) bool {
	next, ok := Transitions[from]
	if !ok {
		return false
	}
	return next[to]
}

// Transition error codes. They mirror the Python TransitionError codes and
// the golden_cases.json contract (cases 07-09).
const (
	CodeTenantMismatch    = "TENANT_MISMATCH"
	CodeInvalidEventID    = "INVALID_EVENT_ID"
	CodeStaleVersion      = "STALE_VERSION"
	CodeUnknownStatus     = "UNKNOWN_STATUS"
	CodeIllegalTransition = "ILLEGAL_TRANSITION"
	CodeMalformedClaim    = "MALFORMED_CLAIM"
)

// TransitionError describes a rejected lifecycle transition.
type TransitionError struct {
	Code    string
	ClaimID string
	From    string
	To      string
}

// Error implements the error interface.
func (e TransitionError) Error() string {
	return fmt.Sprintf("[%s] claim %q transition %q -> %q rejected", e.Code, e.ClaimID, e.From, e.To)
}

// isKnownStatus reports whether s is a recognized lifecycle state.
func isKnownStatus(s ClaimStatus) bool {
	switch s {
	case ClaimStatusReceived,
		ClaimStatusRegistered,
		ClaimStatusDocumentsReceived,
		ClaimStatusValidating,
		ClaimStatusReadyForReview,
		ClaimStatusException,
		ClaimStatusInvestigationRequired,
		ClaimStatusWaitingForEvidence,
		ClaimStatusHITL,
		ClaimStatusActionPending,
		ClaimStatusActioned,
		ClaimStatusVerified,
		ClaimStatusClosed:
		return true
	default:
		return false
	}
}

// Transition advances a claim to a new status with pure value semantics:
// the input is never mutated; a copy carrying the new status, an
// incremented version, and the recorded event ID is returned.
//
// Check order (contract): malformed claim, tenant match, event ID present,
// idempotent replay (already-seen event returns the claim unchanged),
// version equality, known statuses, then legality.
func Transition(c Claim, to ClaimStatus, eventID string, expectedVersion int, actingTenant TenantID) (Claim, error) {
	// 1. Malformed: a claim must carry a status, a version, and a tenant.
	if c.Status == "" || c.Version < 1 || c.Tenant == "" {
		return Claim{}, &TransitionError{
			Code:    CodeMalformedClaim,
			ClaimID: string(c.ID),
			From:    string(c.Status),
			To:      string(to),
		}
	}
	// 2. Tenant isolation wins over all other checks.
	if c.Tenant != actingTenant {
		return Claim{}, &TransitionError{
			Code:    CodeTenantMismatch,
			ClaimID: string(c.ID),
			From:    string(c.Status),
			To:      string(to),
		}
	}
	// 3. Every transition must carry a non-blank idempotency key.
	if strings.TrimSpace(eventID) == "" {
		return Claim{}, &TransitionError{
			Code:    CodeInvalidEventID,
			ClaimID: string(c.ID),
			From:    string(c.Status),
			To:      string(to),
		}
	}
	// 4. Idempotent replay: an already-seen event returns the claim
	// unchanged (golden case 09) before any version or legality check.
	// The idempotency set is deep-copied so callers cannot mutate the
	// stored claim through the returned map.
	if c.HasEvent(eventID) {
		replayed := c
		if c.ProcessedEvents != nil {
			copied := make(map[string]bool, len(c.ProcessedEvents))
			for k, v := range c.ProcessedEvents {
				copied[k] = v
			}
			replayed.ProcessedEvents = copied
		}
		return replayed, nil
	}
	// 5. Optimistic concurrency: the caller must present the current
	// version (golden case 08; stale wins over legality).
	if c.Version != expectedVersion {
		return Claim{}, &TransitionError{
			Code:    CodeStaleVersion,
			ClaimID: string(c.ID),
			From:    string(c.Status),
			To:      string(to),
		}
	}
	// 6. Both endpoints must be recognized lifecycle states.
	if !isKnownStatus(c.Status) || !isKnownStatus(to) {
		return Claim{}, &TransitionError{
			Code:    CodeUnknownStatus,
			ClaimID: string(c.ID),
			From:    string(c.Status),
			To:      string(to),
		}
	}
	// 7. The edge must exist in the contract chain (golden case 07).
	if !CanTransition(c.Status, to) {
		return Claim{}, &TransitionError{
			Code:    CodeIllegalTransition,
			ClaimID: string(c.ID),
			From:    string(c.Status),
			To:      string(to),
		}
	}
	// 8. Apply on a deep copy so the input (including its event set) is
	// never mutated through the shared map reference.
	next := c
	copied := make(map[string]bool, len(c.ProcessedEvents)+1)
	for k, v := range c.ProcessedEvents {
		copied[k] = v
	}
	next.ProcessedEvents = copied
	next.Status = to
	next.Version = c.Version + 1
	next.MarkEvent(eventID)
	return next, nil
}
