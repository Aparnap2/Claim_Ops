// Package extract defines the application-owned canonical extraction
// boundary for ClaimOps (issue #44).
//
// An extractor reports what a canonical artifact SAYS — never whether it
// is right. It consumes parser.ParsedDocument (never untrusted bytes,
// never vendor types) and emits DocumentFacts: one status-tagged
// observation per canonical field key. Judging validity — cross-source
// agreement, policy matching, amount reconciliation — stays downstream in
// verify, which remains the sole judge (R1-R10).
//
// Position in the pipeline:
//
//	parser.ParsedDocument (canonical artifact, #28)
//	  -> extract.DocumentFacts (this package: observation, #44)
//	    -> verify.Input/Result (#47 maps facts to judgment inputs)
//
// Relation to neighbouring contracts (read these before extending this
// package):
//
//   - evidence.Evidence (evidence.go) pins upstream API captures (policy,
//     TPA, provider, risk) by content hash. Different concern: transport
//     provenance, not document content. Do not conflate.
//   - evidence.FieldEvidence (field.go) pins one Tier-1 regex field value
//     to its source line, WITH a [0,1] confidence float and a
//     NewFieldEvidence constructor enforcing trim/page/confidence
//     invariants. Tier-1 observes raw text lines; extract observes the
//     canonical artifact. Constructor conventions here (trimmed strings,
//     1-based pages, named extractors) mirror field.go.
//   - documents.Classify/ExtractFields is the Tier-1 regex layer this
//     contract sits ABOVE: extractors consume parser.ParsedDocument, they
//     do not replace Tier-1 line matching.
//   - claims.TenantID/ClaimID are the tenancy value objects; extract
//     intentionally carries only DocumentID strings because claim binding
//     happens upstream of extraction.
//
// Confidence: ExtractedField carries NO confidence float. This is
// deliberate. The evaluation phase taught us that vendor-silent 1.0 is
// uncalibrated noise (decision log #32; the bench scorer never rewards
// confidence), and a fabricated mid-range float would launder guesswork
// as measurement. Evidence presence IS the confidence signal: a PRESENT
// observation must pin page-level provenance, and the absence of evidence
// is itself the statement (MISSING). If a future extractor earns a
// calibrated score, that arrives as a new explicit field with its own
// calibration proof — never by reusing a bare float.
//
// The package is stdlib-only plus the parser types (the allowed set is
// stdlib + claims/parser/evidence; this file needs only parser): no HTTP,
// no I/O, no logging, no clock reads. All functions are pure.
package extract

import (
	"errors"
	"strings"
	"time"

	"claimops-api/internal/parser"
)

// FieldStatus is the observation state of one canonical field key.
//
// Status rules (enforced by RunConformance in contract.go):
//
//   - value absent from the artifact -> MISSING (never an empty-string
//     PRESENT);
//   - two conflicting values for one key -> MULTI_CANDIDATE (never a
//     silent winner);
//   - value present but unparseable -> AMBIGUOUS with the raw value
//     preserved.
type FieldStatus string

const (
	// StatusPresent means the artifact states one value for the key.
	StatusPresent FieldStatus = "PRESENT"
	// StatusMissing means the artifact states no value for the key.
	StatusMissing FieldStatus = "MISSING"
	// StatusAmbiguous means a value is present but cannot be normalized;
	// the raw text is preserved for human review.
	StatusAmbiguous FieldStatus = "AMBIGUOUS"
	// StatusMultiCandidate means the artifact states two or more
	// conflicting values for one key; all are preserved, none wins.
	StatusMultiCandidate FieldStatus = "MULTI_CANDIDATE"
)

// EvidenceRef pins an observation to its source position in the canonical
// artifact. It is a strict subset of parser.EvidenceLocation: document +
// page + block only. Float bounding boxes stay at the parser layer —
// page+block pinning suffices for field provenance, and adapters must
// never fabricate boxes this layer cannot verify. BlockID may be empty
// where the parser contract allows it (table cells carry no block-id
// by design, not by failure).
type EvidenceRef struct {
	DocumentID string
	Page       int
	BlockID    string
}

// RefFrom projects a parser evidence location down to an EvidenceRef,
// dropping the bounding box. It performs no validation.
func RefFrom(loc parser.EvidenceLocation) EvidenceRef {
	return EvidenceRef{
		DocumentID: loc.DocumentID,
		Page:       loc.Page,
		BlockID:    loc.BlockID,
	}
}

// Candidate is one conflicting value of a MULTI_CANDIDATE observation.
// Each candidate carries its own value and provenance so downstream
// (#47) can surface the conflict without re-reading the artifact.
type Candidate struct {
	Value      string
	Normalized string
	Evidence   EvidenceRef
}

// ExtractedField is one observation: what the artifact says about a
// canonical field key. Key is the canonical name (e.g. "policy_number",
// "admission_date", "total_bill"); Value is the raw artifact text;
// Normalized is the canonical form per the normalizers below (empty when
// there is nothing valid to normalize: MISSING, AMBIGUOUS,
// MULTI_CANDIDATE). Evidence pins the observation; Candidates carries
// the conflict set for MULTI_CANDIDATE. Extractor/ExtractorVersion name
// the producing extractor (mirroring the parser metadata convention).
//
// There is deliberately no confidence float; see the package comment.
type ExtractedField struct {
	Key              string
	Value            string
	Normalized       string
	Evidence         EvidenceRef
	Status           FieldStatus
	Candidates       []Candidate
	Extractor        string
	ExtractorVersion string
}

// DocumentFacts is THE extractor output type: one document's canonical
// observations. DocumentID ties back to the source artifact; DocType is
// the classified document type string (documents.DocClaimForm and kin);
// Fields maps canonical key -> observation.
type DocumentFacts struct {
	DocumentID string
	DocType    string
	Fields     map[string]ExtractedField
}

// Normalizer errors. Callers map blank-input errors to MISSING and
// invalid-input errors to AMBIGUOUS (raw preserved); see FieldStatus.
var (
	// ErrBlankDate is returned when a date string is empty or blank.
	ErrBlankDate = errors.New("extract: blank date (maps to MISSING)")
	// ErrInvalidDate is returned when a date string is not strict
	// YYYY-MM-DD or not a real calendar date.
	ErrInvalidDate = errors.New("extract: invalid date (want YYYY-MM-DD)")
	// ErrBlankAmount is returned when an amount string carries no value.
	ErrBlankAmount = errors.New("extract: blank amount (maps to MISSING)")
	// ErrInvalidAmount is returned when an amount string is not a
	// non-negative decimal with at most 2 fraction digits.
	ErrInvalidAmount = errors.New("extract: invalid amount (want non-negative decimal, max 2 places)")
)

// NormalizeDate normalizes s to strict YYYY-MM-DD, returning the
// canonical string. It accepts ONLY the ISO layout: DD/MM/YYYY and other
// locale renderings are rejected even when unambiguous, because verify
// assumes admission_date values are already YYYY-MM-DD and compares them
// as strings across sources (R10). Blank input yields ErrBlankDate
// (caller maps to MISSING); anything else unparseable yields
// ErrInvalidDate (caller maps to AMBIGUOUS with the raw value preserved).
func NormalizeDate(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", ErrBlankDate
	}
	// Strict shape check first: time.Parse alone accepts non-padded
	// "2026-1-5", which would fork the canonical form.
	if len(trimmed) != 10 || trimmed[4] != '-' || trimmed[7] != '-' {
		return "", ErrInvalidDate
	}
	for i := 0; i < 10; i++ {
		if i == 4 || i == 7 {
			continue
		}
		if trimmed[i] < '0' || trimmed[i] > '9' {
			return "", ErrInvalidDate
		}
	}
	t, err := time.Parse("2006-01-02", trimmed)
	if err != nil {
		return "", ErrInvalidDate
	}
	return t.Format("2006-01-02"), nil
}

// NormalizePaise parses a rupee amount string into exact int64 paise,
// mirroring documents.NormalizePaise. It cannot import documents (this
// package is constrained to stdlib + claims/parser/evidence, and the
// contract sits ABOVE Tier-1), so the logic is duplicated here by design;
// keep the two in sync. It strips rupee markers (Rs, Rs., INR, ₹),
// commas, and spaces, then requires a non-negative decimal with at most
// 2 fraction digits, using exact integer math throughout. Blank input
// yields ErrBlankAmount (caller maps to MISSING); garbage, negatives,
// and >2 fraction digits yield ErrInvalidAmount (caller maps to
// AMBIGUOUS).
//
// Relation to the bench scorer convention (eval/bench/score.go
// numericAmountMatch): the scorer matches digit-stripped candidate text
// against a golden paise value with a rupees-vs-paise rule
// (c == g, c+"00" == g, g+"00" == c). That rule is golden-RELATIVE — it
// needs the expected value to disambiguate units — so it cannot serve as
// a total normalizer. This function is its absolute counterpart: a bare
// digit string with no fraction is read as rupees ("145465" -> 14546500,
// "1,45,465.00" -> 14546500), which is exactly the scorer's c+"00" == g
// arm. Known divergence, documented here rather than hidden: a verbatim
// paise rendering ("14546500" meaning paise) normalizes as rupees
// (x100), while the scorer's Exact arm would match it same-unit. The
// scorer arm only has meaning against a golden; extraction has no golden,
// so the absolute reading wins. Downstream verify consumes int64 paise,
// never re-matching raw text, so the divergence cannot leak into
// judgment.
func NormalizePaise(s string) (int64, error) {
	t := strings.TrimSpace(s)
	for {
		trimmed := strings.TrimSpace(t)
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(trimmed, "₹"):
			t = strings.TrimSpace(strings.TrimPrefix(trimmed, "₹"))
		case strings.HasPrefix(upper, "RS."):
			t = strings.TrimSpace(trimmed[3:])
		case strings.HasPrefix(upper, "RS"):
			t = strings.TrimSpace(trimmed[2:])
		case strings.HasPrefix(upper, "INR"):
			t = strings.TrimSpace(trimmed[3:])
		default:
			t = trimmed
		}
		if t == trimmed {
			break
		}
	}
	t = strings.ReplaceAll(t, ",", "")
	t = strings.ReplaceAll(t, " ", "")
	if t == "" {
		return 0, ErrBlankAmount
	}
	if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "+") {
		return 0, ErrInvalidAmount
	}
	intPart := t
	fracPart := ""
	if i := strings.IndexByte(t, '.'); i >= 0 {
		intPart, fracPart = t[:i], t[i+1:]
	}
	if intPart == "" || (fracPart == "" && strings.Contains(t, ".")) {
		return 0, ErrInvalidAmount
	}
	if !allDigits(intPart) || len(fracPart) > 2 || !allDigits(fracPart) {
		return 0, ErrInvalidAmount
	}
	var rupees int64
	for i := 0; i < len(intPart); i++ {
		rupees = rupees*10 + int64(intPart[i]-'0')
	}
	paise := rupees * 100
	switch len(fracPart) {
	case 1:
		paise += int64(fracPart[0]-'0') * 10
	case 2:
		paise += int64(fracPart[0]-'0')*10 + int64(fracPart[1]-'0')
	}
	return paise, nil
}

// allDigits reports whether s is all ASCII digits. The empty string
// returns true so a missing fraction part passes; callers check
// emptiness where it matters. Mirrors documents.allDigits; see
// NormalizePaise for why the logic is duplicated.
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// NormalizeName trims, casefolds, and collapses interior whitespace,
// mirroring verify.normalizeName. It cannot import verify's helper (it
// is unexported), so the one-line logic is duplicated here by design.
// This deliberately differs from documents.NormalizeName (Title-case for
// display): extract mirrors verify because facts feed verify R2 patient
// identity comparison, where both sides are casefolded before compare.
// Empty input yields "" (caller maps to MISSING).
func NormalizeName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

// NormalizeID trims surrounding whitespace and nothing else. Case
// handling stays with verify at judgment time (R1 upper-cases both sides
// via normalizePolicy), so extraction never destroys case information
// the judge may need. Empty input yields "" (caller maps to MISSING).
func NormalizeID(s string) string {
	return strings.TrimSpace(s)
}
