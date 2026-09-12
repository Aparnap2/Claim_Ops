// T9 (get_provider_encounter) backed by ports.ProviderPort.
//
// Tenant split (read carefully): the Encounter DTO is unattributed — it
// carries no tenant field — so the tool can never attest tenancy from the
// DTO itself and always reports TenantAttested=false. Envelope-present
// upstreams are handled one layer down: the HTTP adapter's peekTenant
// rejects envelope tenant drift with ErrTenantMismatch before the DTO
// reaches this tool. The tool's own gates are the scope allowlist
// (AllowedEncounters), the encounter echo, and the decoded-shape
// re-checks below.
//
// Logging: none (see policy.go). patient_ref/diagnosis content must never
// reach logs; there is deliberately no logger in this package.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"claimops-api/internal/claims"
	"claimops-api/internal/invest"
	"claimops-api/internal/ports"
)

// MaxProviderDocs caps T9 document-ref output. Longer upstream lists are
// truncated with Truncated set; the tool never fails closed on count and
// preserves verbatim upstream order in the prefix it returns.
const MaxProviderDocs = 50

// ProviderRequest is the T9 input: investigation envelope identity plus
// the upstream encounter key plus the scope allowlist of encounter IDs
// this investigation may read.
type ProviderRequest struct {
	TenantID          string
	ClaimID           string
	InvestigationID   string
	RequestID         string
	EncounterID       string
	AllowedEncounters []string
}

// validate rejects blank identity, malformed investigation IDs, blank
// params, and out-of-scope encounters. Every failure wraps
// ports.ErrContract (fail closed, permanent), including the scope gate:
// an empty allowlist or an unlisted encounter is "encounter not in
// scope", never a silent empty result.
func (r ProviderRequest) validate() error {
	if err := claims.TenantID(r.TenantID).Validate(); err != nil {
		return fmt.Errorf("%w: provider: %v", ports.ErrContract, err)
	}
	if err := claims.ClaimID(r.ClaimID).Validate(); err != nil {
		return fmt.Errorf("%w: provider: %v", ports.ErrContract, err)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, r.InvestigationID); err != nil {
		return fmt.Errorf("%w: provider: %v", ports.ErrContract, err)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: provider: blank request id", ports.ErrContract)
	}
	if strings.TrimSpace(r.EncounterID) == "" {
		return fmt.Errorf("%w: provider: blank encounter id", ports.ErrContract)
	}
	for i := range r.AllowedEncounters {
		if strings.TrimSpace(r.AllowedEncounters[i]) == "" {
			return fmt.Errorf("%w: provider: allowlist has blank encounter id at index %d", ports.ErrContract, i)
		}
	}
	allowed := false
	for _, id := range r.AllowedEncounters {
		if id == r.EncounterID {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: provider: encounter not in scope", ports.ErrContract)
	}
	return nil
}

// ProviderResponse is the T9 output: the encounter projection (documents
// truncated to MaxProviderDocs, diagnosis verbatim) plus the pinned
// evidence row. TenantAttested is always false: the DTO is
// unattributed, so the tool attests nothing about tenancy (see the file
// doc above for the adapter-side half of the split).
type ProviderResponse struct {
	Encounter      ports.Encounter
	EvidenceID     string
	ContentHash    string
	Truncated      bool
	TenantAttested bool
}

// checkEncounterProjection enforces the typed equivalent of the
// adapter's requireKeys plus post-decode invariants, so fake ports face
// the same gate as the HTTP adapter: the encounter echo must match the
// request key, patient_ref/hospital_id/admission must be present, and
// discharge must not precede admission. Every failure wraps
// ports.ErrContract. There is deliberately no tenant check here: the
// DTO carries no tenant marker (see TenantAttested).
func checkEncounterProjection(wantEncounterID string, e ports.Encounter) error {
	if e.EncounterID != wantEncounterID {
		return fmt.Errorf("%w: provider: encounter echo %q != requested %q", ports.ErrContract, e.EncounterID, wantEncounterID)
	}
	if strings.TrimSpace(e.PatientRef) == "" {
		return fmt.Errorf("%w: provider: blank patient ref in projection", ports.ErrContract)
	}
	if strings.TrimSpace(e.HospitalID) == "" {
		return fmt.Errorf("%w: provider: blank hospital id in projection", ports.ErrContract)
	}
	if e.AdmissionAt.IsZero() {
		return fmt.Errorf("%w: provider: zero admission in projection", ports.ErrContract)
	}
	if !e.DischargeAt.IsZero() && e.DischargeAt.Before(e.AdmissionAt) {
		return fmt.Errorf("%w: provider: discharge before admission in projection", ports.ErrContract)
	}
	return nil
}

// canonicalEncounter renders the FULL upstream projection
// deterministically for pinning. encoding/json v1 over a struct is
// field-order stable; no maps, no floats, no timestamps minted here.
func canonicalEncounter(e ports.Encounter) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("%w: provider: canonicalize: %v", ports.ErrContract, err)
	}
	return raw, nil
}

// NewProviderTool returns the T9 registry func: validate (including the
// encounter scope gate), GET via the port, encounter echo plus
// decoded-shape re-checks, pin the FULL upstream payload as the
// evidence row, and respond with the projection (documents truncated at
// MaxProviderDocs with the flag, diagnosis verbatim) plus evidence
// identity. Port errors (NotFound/Upstream/Contract/TenantMismatch)
// propagate untouched so errors.Is classification survives.
//
// The pinned bytes are the full pre-truncation encounter: the evidence
// row records what the upstream said, while Truncated records that the
// response carries a prefix of its document refs.
func NewProviderTool(port ports.ProviderPort, pin Pinner) func(ctx context.Context, req ProviderRequest) (ProviderResponse, error) {
	return func(ctx context.Context, req ProviderRequest) (ProviderResponse, error) {
		if err := req.validate(); err != nil {
			return ProviderResponse{}, err
		}
		if port == nil {
			return ProviderResponse{}, fmt.Errorf("%w: provider: nil port", ports.ErrContract)
		}
		if pin == nil {
			return ProviderResponse{}, fmt.Errorf("%w: provider: nil pinner", ports.ErrContract)
		}
		got, err := port.GetEncounter(ctx, req.TenantID, req.EncounterID)
		if err != nil {
			return ProviderResponse{}, err
		}
		if err := checkEncounterProjection(req.EncounterID, got); err != nil {
			return ProviderResponse{}, err
		}
		canonical, err := canonicalEncounter(got)
		if err != nil {
			return ProviderResponse{}, err
		}
		evID, err := pin.Pin(ctx, req.TenantID, req.ClaimID, "provider", got.EncounterID, canonical)
		if err != nil {
			return ProviderResponse{}, err
		}
		if strings.TrimSpace(evID) == "" {
			return ProviderResponse{}, fmt.Errorf("%w: provider: pinner returned blank evidence id", ports.ErrContract)
		}
		out := got
		truncated := false
		if len(out.Documents) > MaxProviderDocs {
			out.Documents = append([]ports.ProviderDoc(nil), out.Documents[:MaxProviderDocs]...)
			truncated = true
		}
		if out.Documents == nil {
			out.Documents = []ports.ProviderDoc{}
		}
		if out.Diagnosis == nil {
			out.Diagnosis = []ports.Diagnosis{}
		}
		sum := sha256.Sum256(canonical)
		return ProviderResponse{
			Encounter:      out,
			EvidenceID:     evID,
			ContentHash:    hex.EncodeToString(sum[:]),
			Truncated:      truncated,
			TenantAttested: false,
		}, nil
	}
}
