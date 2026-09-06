// Package ports defines the outbound risk port boundary.
package ports

import "context"

// RiskSignal is the boundary DTO for a risk signal fetched from an
// upstream risk source (a future NHCX implementation).
//
// All fields are plain strings: ports are boundary DTOs, not domain
// value objects.
type RiskSignal struct {
	// SignalType identifies the kind of risk signal.
	SignalType string

	// TenantID is the upstream tenant marker, enforced per item.
	TenantID string

	// Severity is the upstream severity string.
	Severity string

	// SourceRef is the upstream correlation reference.
	SourceRef string
}

// RiskPort fetches risk signals from an upstream source.
type RiskPort interface {
	// GetSignals returns risk signals for subjectID scoped to tenant.
	//
	// Tenant-consistency expectation: implementations must return
	// ErrTenantMismatch when upstream records belong to a different
	// tenant, ErrContract when the upstream payload fails schema or
	// validation checks, and ErrUpstream for 5xx, timeout, or network
	// failures.
	GetSignals(ctx context.Context, tenant string, subjectID string) ([]RiskSignal, error)
}
