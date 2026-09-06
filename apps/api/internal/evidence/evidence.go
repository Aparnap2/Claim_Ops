// Package evidence implements the deterministic evidence core for the
// ClaimOps application edge.
//
// It is stdlib-only by design (plus the claims value objects): no HTTP,
// no I/O, no logging, no clock reads. Each Evidence row pins one upstream
// response (policy, TPA, provider, risk) to a claim via the sha256 hex of
// the canonical response bytes, so re-fetching the same bytes yields the
// same content hash. Status defaults to CAPTURED; SUPERSEDED is applied
// later when a fresher capture for the same source replaces it.
package evidence

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"claimops-api/internal/claims"
)

// ErrBlankSourceType is returned when a source type is empty or blank.
var ErrBlankSourceType = errors.New("evidence: blank source type")

// ErrInvalidSourceType is returned when a source type is not one of
// policy, tpa, provider, or risk.
var ErrInvalidSourceType = errors.New("evidence: invalid source type (want policy|tpa|provider|risk)")

// ErrBlankSourceID is returned when a source identifier is empty or blank.
var ErrBlankSourceID = errors.New("evidence: blank source id")

// ErrZeroRetrievedAt is returned when the retrieval timestamp is unset.
var ErrZeroRetrievedAt = errors.New("evidence: zero retrieved_at")

// EvidenceStatus is the lifecycle state of an evidence capture.
type EvidenceStatus string

const (
	// EvidenceStatusCaptured is the initial state at fetch time.
	EvidenceStatusCaptured EvidenceStatus = "CAPTURED"
	// EvidenceStatusSuperseded marks a capture replaced by a fresher one
	// for the same (tenant, claim, source type, source id).
	EvidenceStatusSuperseded EvidenceStatus = "SUPERSEDED"
)

// Evidence pins one upstream response to a claim. Tenant and ClaimID reuse
// the claims value objects so blank-ID semantics match the claim core;
// SourceType is one of policy|tpa|provider|risk. ContentHash is the sha256
// hex of the canonical response bytes. Zero time.Time values mean unset.
type Evidence struct {
	ID          string
	Tenant      claims.TenantID
	ClaimID     claims.ClaimID
	SourceType  string
	SourceID    string
	RetrievedAt time.Time
	ContentHash string
	Status      EvidenceStatus
}

// New builds an Evidence, enforcing the deterministic invariants:
//
//   - tenant and claim identifiers must be non-empty after trim
//     (delegated to claims.TenantID/claims.ClaimID validation),
//   - source type must be non-empty after trim and one of
//     policy|tpa|provider|risk (stored trimmed),
//   - source id must be non-empty after trim (stored trimmed),
//   - retrieved-at must be non-zero,
//   - content hash is the sha256 hex of body (body itself is not stored).
//
// The returned Evidence defaults to Status CAPTURED with a generated ID.
func New(
	tenant string,
	claimID string,
	sourceType string,
	sourceID string,
	body []byte,
	at time.Time,
) (Evidence, error) {
	t := claims.TenantID(tenant)
	if err := t.Validate(); err != nil {
		return Evidence{}, err
	}
	c := claims.ClaimID(claimID)
	if err := c.Validate(); err != nil {
		return Evidence{}, err
	}
	trimmedType := strings.TrimSpace(sourceType)
	if trimmedType == "" {
		return Evidence{}, ErrBlankSourceType
	}
	switch trimmedType {
	case "policy", "tpa", "provider", "risk":
	default:
		return Evidence{}, ErrInvalidSourceType
	}
	trimmedSource := strings.TrimSpace(sourceID)
	if trimmedSource == "" {
		return Evidence{}, ErrBlankSourceID
	}
	if at.IsZero() {
		return Evidence{}, ErrZeroRetrievedAt
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	return Evidence{
		ID:          newID(hash),
		Tenant:      t,
		ClaimID:     c,
		SourceType:  trimmedType,
		SourceID:    trimmedSource,
		RetrievedAt: at,
		ContentHash: hash,
		Status:      EvidenceStatusCaptured,
	}, nil
}

// newID generates a random evidence identifier ("ev-" + 32 hex chars),
// mirroring the middleware request-ID idiom. The content hash seeds a
// deterministic fallback so an entropy failure still yields a non-blank ID
// instead of an error.
func newID(contentHash string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		if len(contentHash) >= 32 {
			return "ev-" + contentHash[:32]
		}
		return "ev-" + contentHash
	}
	return "ev-" + hex.EncodeToString(b[:])
}
