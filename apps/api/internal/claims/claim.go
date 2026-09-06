// Package claims implements the deterministic claim core for the ClaimOps
// application edge.
//
// It is stdlib-only by design: no HTTP, no I/O, no logging, no clock reads.
// All money is represented in INR minor units (paise) as int64 to keep
// 2-decimal-place semantics exact. Date invariants mirror
// packages/domain/claim.py: discharge, when set, must be >= admission.
package claims

import (
	"errors"
	"strings"
	"time"
)

// ErrBlankTenant is returned when a tenant identifier is empty or blank.
var ErrBlankTenant = errors.New("claims: blank tenant id")

// ErrBlankPolicy is returned when a policy identifier is empty or blank.
var ErrBlankPolicy = errors.New("claims: blank policy id")

// ErrBlankReference is returned when a claim reference is empty or blank.
var ErrBlankReference = errors.New("claims: blank reference")

// ErrInvalidVersion is returned when a claim version is less than 1.
var ErrInvalidVersion = errors.New("claims: invalid version")

// ErrInvalidDates is returned when discharge is set before admission.
var ErrInvalidDates = errors.New("claims: discharge must be zero or >= admission")

// TenantID identifies the owning tenant.
type TenantID string

// Validate reports whether the tenant identifier is non-empty after trimming
// surrounding whitespace.
func (id TenantID) Validate() error {
	if strings.TrimSpace(string(id)) == "" {
		return ErrBlankTenant
	}
	return nil
}

// ClaimID identifies a claim.
type ClaimID string

// Validate reports whether the claim identifier is non-empty after trimming
// surrounding whitespace.
func (id ClaimID) Validate() error {
	if strings.TrimSpace(string(id)) == "" {
		return errors.New("claims: blank claim id")
	}
	return nil
}

// PolicyID identifies the policy a claim is filed against.
type PolicyID string

// Validate reports whether the policy identifier is non-empty after trimming
// surrounding whitespace.
func (id PolicyID) Validate() error {
	if strings.TrimSpace(string(id)) == "" {
		return ErrBlankPolicy
	}
	return nil
}

// MoneyPaise is an INR amount in minor units (paise). 1 rupee = 100 paise.
//
// The type is intentionally permissive: it can hold negative values.
// Negative-amount rejection lives in the validation package, not here.
type MoneyPaise int64

// NewMoneyPaise converts rupees and paise components into minor units.
//
// It performs no validation: negatives are allowed here and rejected by the
// caller (validation package) where the contract requires non-negative
// amounts.
func NewMoneyPaise(rupees int64, paise int64) MoneyPaise {
	return MoneyPaise(rupees*100 + paise)
}

// MustPaise is a total-conversion helper for call sites and tests that
// construct a MoneyPaise inline. It never fails because the conversion is
// total; negativity checks remain the caller's responsibility.
func MustPaise(rupees int64, paise int64) MoneyPaise {
	return NewMoneyPaise(rupees, paise)
}

// ClaimStatus is the lifecycle state of a claim.
type ClaimStatus string

// Full deterministic contract chain. Terminal states (EXCEPTION, CLOSED)
// and HITL-adjacent states are part of the chain, not ad-hoc strings.
const (
	// ClaimStatusReceived is the initial state at intake.
	ClaimStatusReceived ClaimStatus = "RECEIVED"
	// ClaimStatusRegistered follows intake once the claim is registered.
	ClaimStatusRegistered ClaimStatus = "REGISTERED"
	// ClaimStatusDocumentsReceived indicates required documents arrived.
	ClaimStatusDocumentsReceived ClaimStatus = "DOCUMENTS_RECEIVED"
	// ClaimStatusValidating indicates deterministic validation is running.
	ClaimStatusValidating ClaimStatus = "VALIDATING"
	// ClaimStatusReadyForReview indicates the claim is ready for review.
	ClaimStatusReadyForReview ClaimStatus = "READY_FOR_REVIEW"
	// ClaimStatusException indicates a deterministic exception was raised.
	ClaimStatusException ClaimStatus = "EXCEPTION"
	// ClaimStatusInvestigationRequired routes the claim to investigation.
	ClaimStatusInvestigationRequired ClaimStatus = "INVESTIGATION_REQUIRED"
	// ClaimStatusWaitingForEvidence waits on additional evidence.
	ClaimStatusWaitingForEvidence ClaimStatus = "WAITING_FOR_EVIDENCE"
	// ClaimStatusHITL parks the claim for human-in-the-loop action.
	ClaimStatusHITL ClaimStatus = "HITL"
	// ClaimStatusActionPending indicates a HITL action is pending.
	ClaimStatusActionPending ClaimStatus = "ACTION_PENDING"
	// ClaimStatusActioned indicates the HITL action was applied.
	ClaimStatusActioned ClaimStatus = "ACTIONED"
	// ClaimStatusVerified indicates the claim outcome was verified.
	ClaimStatusVerified ClaimStatus = "VERIFIED"
	// ClaimStatusClosed is the terminal closed state.
	ClaimStatusClosed ClaimStatus = "CLOSED"
)

// Claim is the deterministic claim entity. Zero time.Time values mean unset.
type Claim struct {
	ID              ClaimID
	Tenant          TenantID
	Policy          PolicyID
	Reference       string
	AmountPaise     MoneyPaise
	Status          ClaimStatus
	Version         int
	IncidentDate    time.Time
	AdmissionDate   time.Time
	DischargeDate   time.Time
	ProcessedEvents map[string]bool
}

// HasEvent reports whether eventID was already processed. It is nil-safe:
// a nil claim or nil map reports false.
func (c *Claim) HasEvent(eventID string) bool {
	if c == nil || c.ProcessedEvents == nil {
		return false
	}
	return c.ProcessedEvents[eventID]
}

// MarkEvent records eventID as processed. It is nil-safe on the map: a nil
// map is allocated first. It no-ops on a nil claim.
func (c *Claim) MarkEvent(eventID string) {
	if c == nil {
		return
	}
	if c.ProcessedEvents == nil {
		c.ProcessedEvents = make(map[string]bool)
	}
	c.ProcessedEvents[eventID] = true
}

// NewClaim builds a Claim, enforcing the deterministic invariants:
//
//   - claim id must be non-empty after trim (validated first),
//   - tenant and policy identifiers must be non-empty after trim,
//   - reference must be non-empty after trim (stored trimmed),
//   - version must be >= 1,
//   - discharge must be zero or >= admission (when both set;
//     a discharge with an unset admission is accepted, mirroring the
//     Python spec which only constrains the both-set case).
func NewClaim(
	id ClaimID,
	tenant TenantID,
	policy PolicyID,
	reference string,
	amount MoneyPaise,
	status ClaimStatus,
	version int,
	incidentDate time.Time,
	admissionDate time.Time,
	dischargeDate time.Time,
) (*Claim, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(tenant)) == "" {
		return nil, ErrBlankTenant
	}
	if strings.TrimSpace(string(policy)) == "" {
		return nil, ErrBlankPolicy
	}
	trimmed := strings.TrimSpace(reference)
	if trimmed == "" {
		return nil, ErrBlankReference
	}
	if version < 1 {
		return nil, ErrInvalidVersion
	}
	if !admissionDate.IsZero() && !dischargeDate.IsZero() && dischargeDate.Before(admissionDate) {
		return nil, ErrInvalidDates
	}
	return &Claim{
		ID:              id,
		Tenant:          tenant,
		Policy:          policy,
		Reference:       trimmed,
		AmountPaise:     amount,
		Status:          status,
		Version:         version,
		IncidentDate:    incidentDate,
		AdmissionDate:   admissionDate,
		DischargeDate:   dischargeDate,
		ProcessedEvents: make(map[string]bool),
	}, nil
}
