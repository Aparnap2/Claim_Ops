// Package ports defines the outbound provider port boundary.
package ports

import (
	"context"
	"time"
)

// Diagnosis is a coded diagnosis on an encounter.
type Diagnosis struct {
	// Code is the diagnosis code.
	Code string

	// Description is the human-readable diagnosis text.
	Description string
}

// ProviderDoc is a document reference attached to an encounter.
type ProviderDoc struct {
	// DocumentID is the upstream document identifier.
	DocumentID string

	// Type is the upstream document type string.
	Type string

	// SHA256 is the hex-encoded SHA-256 digest of the document bytes.
	SHA256 string
}

// Encounter is the boundary DTO for a provider encounter fetched from an
// upstream provider source (a future NHCX implementation).
//
// All identifiers are plain strings: ports are boundary DTOs, not domain
// value objects, so they intentionally do not use claims value objects.
type Encounter struct {
	// EncounterID is the upstream encounter identifier.
	EncounterID string

	// PatientRef is the upstream patient reference.
	PatientRef string

	// HospitalID is the upstream hospital identifier.
	HospitalID string

	// AdmissionAt is the admission timestamp.
	AdmissionAt time.Time

	// DischargeAt is the discharge timestamp.
	DischargeAt time.Time

	// Diagnosis lists coded diagnoses for the encounter.
	Diagnosis []Diagnosis

	// Documents lists document references for the encounter.
	Documents []ProviderDoc
}

// ProviderPort fetches provider encounters from an upstream source.
type ProviderPort interface {
	// GetEncounter returns the encounter for encounterID scoped to tenant.
	//
	// Tenant-consistency expectation: implementations must return
	// ErrTenantMismatch when the upstream record's tenant differs from
	// tenant, ErrNotFound when no record exists, ErrContract when the
	// upstream payload fails schema or validation checks, and ErrUpstream
	// for 5xx, timeout, or network failures.
	GetEncounter(ctx context.Context, tenant string, encounterID string) (Encounter, error)
}
