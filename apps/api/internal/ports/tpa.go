// Package ports defines the outbound prior-claims port boundary.
package ports

import (
	"context"
	"time"
)

// PriorClaim is the boundary DTO for a historical claim fetched from an
// upstream TPA source (a future NHCX implementation).
//
// All identifiers and statuses are plain strings: ports are boundary
// DTOs, not domain value objects, so they intentionally do not use
// claims.ClaimID or claims.ClaimStatus. Money is in INR minor units
// (paise) as int64, mirroring the MoneyPaise idiom.
type PriorClaim struct {
	// ClaimID is the upstream claim identifier.
	ClaimID string

	// TenantID is the upstream tenant marker. Every item carries it so
	// per-item tenant drift maps to ErrTenantMismatch, never silent use.
	TenantID string

	// Status is the upstream claim status string.
	Status string

	// Admission is the admission date of the prior claim.
	Admission time.Time

	// Discharge is the discharge date of the prior claim.
	Discharge time.Time

	// ApprovedPaise is the approved amount in paise.
	ApprovedPaise int64
}

// ClaimsPort lists historical claims from an upstream TPA source.
type ClaimsPort interface {
	// ListClaims returns prior claims for policyID scoped to tenant.
	//
	// Tenant-consistency expectation: implementations must return
	// ErrTenantMismatch when upstream records belong to a different
	// tenant, ErrContract when the upstream payload fails schema or
	// validation checks, and ErrUpstream for 5xx, timeout, or network
	// failures.
	ListClaims(ctx context.Context, tenant string, policyID string) ([]PriorClaim, error)
}
