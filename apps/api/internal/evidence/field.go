// FieldEvidence pins one Tier-1 extracted field value to its source
// document line, mirroring the evidence package conventions: stdlib-only
// (plus the claims value objects), no HTTP, no I/O, no logging, no clock
// reads. Tenant and ClaimID reuse the claims value objects so blank-ID
// semantics match the claim core; confidence is a [0,1] probability and
// page is 1-based.
package evidence

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"claimops-api/internal/claims"
)

// ErrBlankDocumentID is returned when a document identifier is empty or
// blank.
var ErrBlankDocumentID = errors.New("evidence: blank document id")

// ErrBlankField is returned when a field name is empty or blank.
var ErrBlankField = errors.New("evidence: blank field name")

// ErrBlankExtractor is returned when an extractor name is empty or blank.
var ErrBlankExtractor = errors.New("evidence: blank extractor")

// ErrInvalidConfidence is returned when a confidence is NaN or outside
// [0,1].
var ErrInvalidConfidence = errors.New("evidence: invalid confidence (want [0,1])")

// ErrInvalidPage is returned when a page number is less than 1.
var ErrInvalidPage = errors.New("evidence: invalid page (want >= 1)")

// FieldEvidence pins one extracted field value to a claim via its source
// document. DocumentID is the originating document, Field/Value carry the
// canonical name and extracted text, Anchor is the trimmed source line,
// Extractor names the Tier (e.g. tier1-regex), DocType is the classified
// document type string, Page is 1-based, and Confidence is a [0,1]
// probability.
type FieldEvidence struct {
	ID         string
	Tenant     claims.TenantID
	ClaimID    claims.ClaimID
	DocumentID string
	Field      string
	Value      string
	Anchor     string
	Extractor  string
	DocType    string
	Page       int
	Confidence float64
}

// NewFieldEvidence builds a FieldEvidence, enforcing the deterministic
// invariants:
//
//   - tenant and claim identifiers must be non-empty after trim
//     (delegated to claims.TenantID/claims.ClaimID validation),
//   - document id, field name, and extractor must be non-empty after trim
//     (stored trimmed),
//   - confidence must be non-NaN and within [0,1],
//   - page must be >= 1.
//
// The returned record carries a generated ID ("evf-" + 16 hex chars).
func NewFieldEvidence(
	tenant, claimID, documentID, docType, field, value, anchor, extractor string,
	page int,
	confidence float64,
) (FieldEvidence, error) {
	t := claims.TenantID(tenant)
	if err := t.Validate(); err != nil {
		return FieldEvidence{}, err
	}
	c := claims.ClaimID(claimID)
	if err := c.Validate(); err != nil {
		return FieldEvidence{}, err
	}
	trimmedDoc := strings.TrimSpace(documentID)
	if trimmedDoc == "" {
		return FieldEvidence{}, ErrBlankDocumentID
	}
	trimmedField := strings.TrimSpace(field)
	if trimmedField == "" {
		return FieldEvidence{}, ErrBlankField
	}
	trimmedExtractor := strings.TrimSpace(extractor)
	if trimmedExtractor == "" {
		return FieldEvidence{}, ErrBlankExtractor
	}
	if math.IsNaN(confidence) || confidence < 0 || confidence > 1 {
		return FieldEvidence{}, ErrInvalidConfidence
	}
	if page < 1 {
		return FieldEvidence{}, ErrInvalidPage
	}
	id, err := newFieldID()
	if err != nil {
		return FieldEvidence{}, err
	}
	return FieldEvidence{
		ID:         id,
		Tenant:     t,
		ClaimID:    c,
		DocumentID: trimmedDoc,
		Field:      trimmedField,
		Value:      strings.TrimSpace(value),
		Anchor:     strings.TrimSpace(anchor),
		Extractor:  trimmedExtractor,
		DocType:    strings.TrimSpace(docType),
		Page:       page,
		Confidence: confidence,
	}, nil
}

// newFieldID generates a random field-evidence identifier ("evf-" + 16
// hex chars), mirroring the evidence newID idiom. Unlike newID there is no
// content hash to fall back on, so an entropy failure is returned.
func newFieldID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("evidence: field id entropy: %w", err)
	}
	return "evf-" + hex.EncodeToString(b[:]), nil
}
