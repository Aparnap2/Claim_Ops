// Package external holds wire DTOs for upstream provider payloads.
//
// Each DTO is a type alias of its ports counterpart so adapters decode
// directly into the contract shape and return ports types with no
// conversion layer. A future NHCX adapter will implement
// ports.ProviderPort by decoding this contract.
package external

import "claimops-api/internal/ports"

// EncounterResponse is the wire DTO for an upstream encounter payload.
// Alias of ports.Encounter.
type EncounterResponse = ports.Encounter

// Diagnosis is the wire DTO for a coded diagnosis.
// Alias of ports.Diagnosis.
type Diagnosis = ports.Diagnosis

// ProviderDoc is the wire DTO for an encounter document reference.
// Alias of ports.ProviderDoc.
type ProviderDoc = ports.ProviderDoc
