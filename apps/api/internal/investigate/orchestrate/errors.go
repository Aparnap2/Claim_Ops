package orchestrate

import (
	"errors"
	"fmt"

	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

// Error taxonomy for the bounded investigation orchestrator (issue #66).
//
// Ten sentinels, each wrapping a lower-layer taxonomy entry with %w so
// callers classify with errors.Is against either layer without importing
// adapter internals:
//
//	ErrModelContract            ports.ErrContract
//	ErrModelEmpty               ports.ErrContract
//	ErrModelUpstream            ports.ErrUpstream
//	ErrGrounding                ports.ErrContract
//	ErrToolDenied               investigate.ErrToolNotAllowed
//	ErrRepetition               ports.ErrContract
//	ErrBudgetExceeded           investigate.ErrBudgetExceeded
//	ErrDeadlineExceeded         investigate.ErrDeadlineExceeded
//	ErrTenantMismatch           investigate.ErrTenantMismatch
//	ErrToolUpstream             investigate.ErrUpstream
//
// ctx cancellation is never wrapped and never retried: Run returns the
// context error itself.
var (
	// ErrModelContract rejects model acts that violate the 2-act shape.
	ErrModelContract = fmt.Errorf("orchestrate: model output violates contract: %w", ports.ErrContract)
	// ErrModelEmpty rejects empty model output.
	ErrModelEmpty = fmt.Errorf("orchestrate: model output empty: %w", ports.ErrContract)
	// ErrModelUpstream marks model transport failure (retried once, then escalated).
	ErrModelUpstream = fmt.Errorf("orchestrate: model transport failure: %w", ports.ErrUpstream)
	// ErrGrounding rejects citations outside the KnownEvidence set.
	ErrGrounding = fmt.Errorf("orchestrate: ungrounded citation: %w", ports.ErrContract)
	// ErrToolDenied rejects tools outside the allowlist/scope, and T11 always.
	ErrToolDenied = fmt.Errorf("orchestrate: tool denied: %w", investigate.ErrToolNotAllowed)
	// ErrRepetition rejects an exact-repeat tool call.
	ErrRepetition = fmt.Errorf("orchestrate: repeated tool call: %w", ports.ErrContract)
	// ErrBudgetExceeded marks spent turn/call budgets.
	ErrBudgetExceeded = fmt.Errorf("orchestrate: budget exhausted: %w", investigate.ErrBudgetExceeded)
	// ErrDeadlineExceeded marks a passed scope deadline.
	ErrDeadlineExceeded = fmt.Errorf("orchestrate: deadline exceeded: %w", investigate.ErrDeadlineExceeded)
	// ErrTenantMismatch aborts on scope/envelope tenant split (F6 class).
	ErrTenantMismatch = fmt.Errorf("orchestrate: tenant mismatch: %w", investigate.ErrTenantMismatch)
	// ErrToolUpstream marks tool backend transients surfaced to the loop.
	ErrToolUpstream = fmt.Errorf("orchestrate: tool backend failure: %w", investigate.ErrUpstream)
)

// invalidKind is the I1-I8 invalid-output class for one model act.
// Only I1/I2/I7 earn a single same-turn re-prompt; every other class
// escalates immediately, and a second consecutive invalid escalates.
type invalidKind int

const (
	invalidNone invalidKind = iota
	// invalidMalformed (I1) covers undecodable bytes: syntax errors,
	// unknown fields, trailing data.
	invalidMalformed
	// invalidAction (I2) covers a missing or unknown act discriminator.
	invalidAction
	// invalidDenied (I3) covers a denied tool: not allowlisted, not in
	// scope, or the T11 writer.
	invalidDenied
	// invalidRequest (I4) covers a malformed tool request: failed
	// Request.Validate, tool echo split, or identity not echoing scope.
	invalidRequest
	// invalidReport (I5) covers a malformed submitted report shape.
	invalidReport
	// invalidGrounding (I6) covers citations outside KnownEvidence and
	// unresolvable finding/hypothesis links.
	invalidGrounding
	// invalidEmpty (I7) covers empty model output.
	invalidEmpty
	// invalidOversize (I8) covers output past the byte cap.
	invalidOversize
)

// invalidKindString names the class for messages and attempt codes.
func invalidKindString(k invalidKind) string {
	switch k {
	case invalidMalformed:
		return "I1-malformed"
	case invalidAction:
		return "I2-action"
	case invalidDenied:
		return "I3-denied"
	case invalidRequest:
		return "I4-request"
	case invalidReport:
		return "I5-report"
	case invalidGrounding:
		return "I6-grounding"
	case invalidEmpty:
		return "I7-empty"
	case invalidOversize:
		return "I8-oversize"
	default:
		return "I0-unknown"
	}
}

// repromptable reports whether the class earns the single same-turn
// re-prompt (I1/I2/I7 only, per the reviewed design).
func repromptable(k invalidKind) bool {
	return k == invalidMalformed || k == invalidAction || k == invalidEmpty
}

// invalidError carries the I-class plus the sentinel chain. Unwrap
// exposes both the orchestrate sentinel and the underlying cause so
// errors.Is classifies at either layer.
type invalidError struct {
	kind     invalidKind
	sentinel error
	cause    error
}

func (e *invalidError) Error() string {
	if e.cause != nil {
		return "orchestrate: invalid model output (" + invalidKindString(e.kind) + "): " + e.cause.Error()
	}
	return "orchestrate: invalid model output (" + invalidKindString(e.kind) + ")"
}

// Unwrap exposes the sentinel and, when present, the cause.
func (e *invalidError) Unwrap() []error {
	if e.cause != nil {
		return []error{e.sentinel, e.cause}
	}
	return []error{e.sentinel}
}

// newInvalidError builds the classified error for one invalid act.
func newInvalidError(kind invalidKind, sentinel error, cause error) error {
	return &invalidError{kind: kind, sentinel: sentinel, cause: cause}
}

// invalidKindOf recovers the I-class from a classified error.
func invalidKindOf(err error) invalidKind {
	var ie *invalidError
	if errors.As(err, &ie) {
		return ie.kind
	}
	return invalidNone
}
