// Package ports defines the outbound policy port boundary.
package ports

import (
	"context"
	"time"
)

// WaitingPeriod is a condition-level waiting period on a policy.
type WaitingPeriod struct {
	// ConditionCode identifies the conditioned ailment or treatment.
	ConditionCode string

	// EndsOn is the date the waiting period lifts.
	EndsOn time.Time
}

// PolicyInfo is the boundary DTO for policy details fetched from an
// upstream policy source (a future NHCX implementation).
//
// All identifiers are plain strings: ports are boundary DTOs, not domain
// value objects, so they intentionally do not use claims.TenantID or
// claims.PolicyID. Money is in INR minor units (paise) as int64,
// mirroring the MoneyPaise idiom.
type PolicyInfo struct {
	// PolicyID is the upstream policy identifier.
	PolicyID string

	// TenantID echoes the owning tenant from the upstream record.
	TenantID string

	// Status is the upstream policy status string.
	Status string

	// EffectiveFrom is the policy coverage start.
	EffectiveFrom time.Time

	// EffectiveTo is the policy coverage end.
	EffectiveTo time.Time

	// SumInsuredPaise is the total cover in paise.
	SumInsuredPaise int64

	// AvailablePaise is the remaining cover in paise.
	AvailablePaise int64

	// WaitingPeriods lists condition-level waiting periods.
	WaitingPeriods []WaitingPeriod

	// SourceRef is the upstream correlation reference.
	SourceRef string
}

// PolicyPort fetches policy details from an upstream source.
type PolicyPort interface {
	// GetPolicy returns the policy for policyID scoped to tenant.
	//
	// Tenant-consistency expectation: implementations must return
	// ErrTenantMismatch when the upstream record's tenant differs from
	// tenant, ErrNotFound when no record exists, ErrContract when the
	// upstream payload fails schema or validation checks, and ErrUpstream
	// for 5xx, timeout, or network failures.
	GetPolicy(ctx context.Context, tenant string, policyID string) (PolicyInfo, error)
}
