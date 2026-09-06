// Package ports defines the outbound port boundaries for the ClaimOps
// application edge.
//
// Dependency direction per docs/adr/001-go-first-edge.md is
// Domain <- Workflow <- Ports <- Adapters: workflow depends on these
// port interfaces, and concrete adapters (including a future NHCX
// adapter) implement them. Ports never import domain value objects,
// adapters, handlers, or HTTP packages; all identifiers crossing the boundary
// are plain strings and int64 paise amounts, mirroring the
// TenantID/ClaimID/MoneyPaise idioms in apps/api/internal/claims/claim.go
// without taking a dependency on them.
package ports

import "errors"

// Sentinel errors returned by port implementations. Adapters wrap these
// with %w so callers can classify failures with errors.Is without
// depending on adapter internals.
var (
	// ErrNotFound is returned when the upstream has no record for the key.
	ErrNotFound = errors.New("ports: not found")

	// ErrContract is returned when the upstream payload fails schema or
	// validation checks (malformed shape, missing required fields).
	ErrContract = errors.New("ports: contract failure")

	// ErrTenantMismatch is returned when the upstream record's tenant does
	// not match the requesting tenant.
	ErrTenantMismatch = errors.New("ports: tenant mismatch")

	// ErrUpstream is returned for transient upstream failures: HTTP 5xx,
	// timeouts, and network errors.
	ErrUpstream = errors.New("ports: upstream failure")
)
