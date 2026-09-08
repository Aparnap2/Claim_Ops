// Package documents implements the Tier-1 deterministic document
// intelligence foundation for the ClaimOps application edge.
//
// It is stdlib-only by design (plus the claims value objects): no HTTP,
// no I/O, no logging, no clock reads. Classification is filename-keyword
// driven; field extraction is a line-regex pass over OCR/plain text.
// Real OCR arrives later as a Port implementation of OCRProvider; until
// then NoopOCR forces the Tier-1-or-fail path.
package documents

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"

	"claimops-api/internal/claims"
)

// ErrInvalidDate is returned when a date string matches none of the
// accepted layouts.
var ErrInvalidDate = errors.New("documents: invalid date (want YYYY-MM-DD or DD/MM/YYYY)")

// ErrInvalidAmount is returned when an amount string is not a
// non-negative decimal with at most 2 fraction digits.
var ErrInvalidAmount = errors.New("documents: invalid amount (want non-negative decimal, max 2 places)")

// ErrOCRUnavailable is returned by NoopOCR for every call. It forces the
// Tier-1-or-fail path until a real OCR Port implementation lands.
var ErrOCRUnavailable = errors.New("documents: OCR unavailable (Tier-1-or-fail)")

// DocType is the deterministic document category.
type DocType string

const (
	// DocClaimForm is a filled claim intimation form.
	DocClaimForm DocType = "CLAIM_FORM"
	// DocDischargeSummary is a hospital discharge summary.
	DocDischargeSummary DocType = "DISCHARGE_SUMMARY"
	// DocHospitalBill is a hospital bill or invoice.
	DocHospitalBill DocType = "HOSPITAL_BILL"
	// DocPolicyDocument is a policy schedule or wording document.
	DocPolicyDocument DocType = "POLICY_DOCUMENT"
	// DocIdentityDocument is a KYC identity document.
	DocIdentityDocument DocType = "IDENTITY_DOCUMENT"
	// DocUnknown is the fallback when no classifier rule fires.
	DocUnknown DocType = "UNKNOWN"
)

// Document lifecycle states. Status is a plain string (not a dedicated
// type) so storage layers can persist it without conversion.
//
// Lifecycle and extraction quality are deliberately separate vocabularies:
// Status tracks where the document is in the pipeline (RECEIVED ->
// PROCESSED | FAILED; CLASSIFIED marks the classify step), while
// ExtractionOutcome reports what the extractor found. A PROCESSED status
// must never be read as an AI/data-quality verdict — quality travels in
// ExtractionOutcome (see worker.Outcome.Extraction), logs, and metrics.
const (
	// StReceived is the initial state at upload.
	StReceived = "RECEIVED"
	// StClassified follows successful Classify.
	StClassified = "CLASSIFIED"
	// StProcessed is the success-terminal lifecycle state: the pipeline
	// ran to completion and persisted the document, regardless of how
	// much (if anything) was extracted. See ExtractionOutcome for
	// data-quality.
	StProcessed = "PROCESSED"
	// StFailed marks a document that failed classification or extraction.
	StFailed = "FAILED"
)

// ExtractionOutcome is the extraction-quality verdict for one document.
// It is orthogonal to the lifecycle Status: a PROCESSED document may
// carry any outcome except NOT_ATTEMPTED, and Failed documents never
// carry a quality verdict beyond NOT_ATTEMPTED/NO_CONTENT.
type ExtractionOutcome string

const (
	// ExtractionNotAttempted means extraction never ran (fetch failure,
	// schema mismatch, malformed event).
	ExtractionNotAttempted ExtractionOutcome = "NOT_ATTEMPTED"
	// ExtractionNoContent means extraction ran but found nothing (empty
	// content, or no key rule matched).
	ExtractionNoContent ExtractionOutcome = "NO_CONTENT"
	// ExtractionPartial means some but not all key rules matched.
	ExtractionPartial ExtractionOutcome = "PARTIAL"
	// ExtractionComplete means every key rule matched.
	ExtractionComplete ExtractionOutcome = "COMPLETE"
)

// Document is the deterministic document entity. Tenant and ClaimID reuse
// the claims value objects so blank-ID semantics match the claim core.
type Document struct {
	ID        string
	Tenant    claims.TenantID
	ClaimID   claims.ClaimID
	Type      DocType
	FileName  string
	MIME      string
	SHA256    string
	SizeBytes int64
	Status    string
}

// Classify maps a file name to a DocType with a confidence score using
// deterministic lowercase filename-keyword rules applied in priority
// order: CLAIM_FORM > DISCHARGE_SUMMARY > HOSPITAL_BILL > POLICY_DOCUMENT
// > IDENTITY_DOCUMENT. The mime argument is accepted for signature
// stability (future content-sniffing rules) and does not affect the
// Tier-1 outcome. Unknown files yield (DocUnknown, 0.0).
func Classify(fileName, mime string) (DocType, float64) {
	_ = mime
	lower := strings.ToLower(fileName)
	if strings.Contains(lower, "claim_form") || strings.Contains(lower, "claimform") {
		return DocClaimForm, 0.9
	}
	if strings.Contains(lower, "discharge") {
		return DocDischargeSummary, 0.9
	}
	if strings.Contains(lower, "bill") || strings.Contains(lower, "invoice") {
		return DocHospitalBill, 0.85
	}
	if strings.Contains(lower, "policy") {
		return DocPolicyDocument, 0.9
	}
	if strings.Contains(lower, "aadhaar") ||
		strings.Contains(lower, "passport") ||
		strings.Contains(lower, "identity") ||
		hasToken(lower, "pan") {
		return DocIdentityDocument, 0.8
	}
	return DocUnknown, 0.0
}

// hasToken reports whether lower contains word as a standalone token.
// Tokens are maximal runs of ASCII letters, so "pan_card" and "my-pan"
// match while "company" and "panel" do not.
func hasToken(lower, word string) bool {
	for _, tok := range strings.FieldsFunc(lower, func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		if tok == word {
			return true
		}
	}
	return false
}

// Field is one extracted key/value pair pinned to its source line.
type Field struct {
	Name       string
	Value      string
	Anchor     string
	Page       int
	Confidence float64
}

// keyRule pairs a canonical field name with its key alternation.
type keyRule struct {
	name string
	key  string
}

// keyRules is the Tier-1 key vocabulary in deterministic match order.
var keyRules = []keyRule{
	{name: "claim_number", key: `claim[\s._-]*?(?:number|no\.?|num|id)`},
	{name: "policy_number", key: `policy[\s._-]*?(?:number|no\.?|num|id)`},
	{name: "patient_name", key: `patient[\s._-]*?name`},
	{name: "admission_date", key: `(?:admission[\s._-]*?date|date[\s._-]*of[\s._-]*admission)`},
	{name: "discharge_date", key: `discharge[\s._-]*?date`},
	{name: "hospital_name", key: `hospital[\s._-]*?name`},
	{name: "total_bill", key: `(?:total[\s._-]*?bill|bill[\s._-]*?total|net[\s._-]*?payable)`},
}

// fullLine and partialLine are per-rule regexes built once at init:
// full-line `^\s*key\s*[:=]\s*value\s*$` yields confidence 0.9, while a
// key match with leading text on the line yields 0.6.
type linePatterns struct {
	name    string
	full    *regexp.Regexp
	partial *regexp.Regexp
}

var lineMatchers = func() []linePatterns {
	out := make([]linePatterns, 0, len(keyRules))
	for _, r := range keyRules {
		out = append(out, linePatterns{
			name:    r.name,
			full:    regexp.MustCompile(`(?i)^\s*(?:` + r.key + `)\s*[:=]\s*(.+?)\s*$`),
			partial: regexp.MustCompile(`(?i)(?:` + r.key + `)\s*[:=]\s*(.+?)\s*$`),
		})
	}
	return out
}()

// ExtractFields runs the Tier-1 line-regex extractor over content for a
// known docType. Each line is tried against the key vocabulary in order;
// the first non-empty value per canonical name wins. Anchor is the matched
// line trimmed to 200 chars, Page is always 1, Confidence is 0.9 on a
// full-line `key [: =] value` match and 0.6 when the key is embedded in
// surrounding text. Unknown docTypes yield nil.
func ExtractFields(docType DocType, content string) []Field {
	switch docType {
	case DocClaimForm,
		DocDischargeSummary,
		DocHospitalBill,
		DocPolicyDocument,
		DocIdentityDocument:
	default:
		return nil
	}
	var out []Field
	seen := make(map[string]bool, len(lineMatchers))
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for _, m := range lineMatchers {
			if seen[m.name] {
				continue
			}
			var value string
			var confidence float64
			if sub := m.full.FindStringSubmatch(line); sub != nil {
				value = strings.TrimSpace(sub[1])
				confidence = 0.9
			} else if sub := m.partial.FindStringSubmatch(line); sub != nil {
				value = strings.TrimSpace(sub[1])
				confidence = 0.6
			} else {
				continue
			}
			if value == "" {
				continue
			}
			out = append(out, Field{
				Name:       m.name,
				Value:      value,
				Anchor:     truncateRunes(trimmed, 200),
				Page:       1,
				Confidence: confidence,
			})
			seen[m.name] = true
		}
	}
	return out
}

// ClassifyExtraction maps extractor output to an ExtractionOutcome with
// a deterministic rule: empty content or zero fields is NO_CONTENT;
// COMPLETE when every keyRules name appears in fields, PARTIAL
// otherwise. It never touches Status — lifecycle and quality stay
// separate.
func ClassifyExtraction(content string, fields []Field) ExtractionOutcome {
	if len(content) == 0 {
		return ExtractionNoContent
	}
	if len(fields) == 0 {
		return ExtractionNoContent
	}
	seen := make(map[string]bool, len(fields))
	for _, f := range fields {
		seen[f.Name] = true
	}
	for _, r := range keyRules {
		if !seen[r.name] {
			return ExtractionPartial
		}
	}
	return ExtractionComplete
}

// truncateRunes caps s at n runes.
func truncateRunes(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// NormalizeName trims, collapses inner whitespace, and Title-cases each
// word ("  aparna   PRADHAN " -> "Aparna Pradhan").
func NormalizeName(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		lowered := []rune(strings.ToLower(w))
		if len(lowered) > 0 {
			lowered[0] = unicode.ToUpper(lowered[0])
		}
		words[i] = string(lowered)
	}
	return strings.Join(words, " ")
}

// dateLayouts are the accepted date formats: ISO first, then DD/MM/YYYY
// with slash and dash separators.
var dateLayouts = []string{"2006-01-02", "02/01/2006", "02-01-2006"}

// NormalizeDate parses s in one of the accepted layouts (2006-01-02,
// 02/01/2006, 02-01-2006 with DD/MM/YYYY semantics) and returns the
// resulting time.Time, else ErrInvalidDate.
func NormalizeDate(s string) (time.Time, error) {
	trimmed := strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, trimmed); err == nil {
			return t, nil
		}
	}
	return time.Time{}, ErrInvalidDate
}

// NormalizePaise parses a rupee amount string into INR minor units
// (paise) using exact integer math: it strips rupee signs (Rs, Rs., INR),
// commas, and spaces, then requires a non-negative decimal with at most 2
// fraction digits. Garbage, empty input, and negatives yield
// ErrInvalidAmount.
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
		return 0, ErrInvalidAmount
	}
	if strings.HasPrefix(t, "-") || strings.HasPrefix(t, "+") {
		return 0, ErrInvalidAmount
	}
	intPart := t
	fracPart := ""
	if i := strings.IndexByte(t, '.'); i >= 0 {
		intPart, fracPart = t[:i], t[i+1:]
	}
	if intPart == "" || fracPart == "" && strings.Contains(t, ".") {
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

// allDigits reports whether s is non-empty and all ASCII digits. The empty
// string returns true so a missing fraction part passes; callers check
// emptiness where it matters.
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// OCRProvider is the Port for text extraction from document blobs. Real
// OCR implementations arrive in a later phase; the interface pins the
// contract now.
type OCRProvider interface {
	// ExtractText returns the plain text of blob for doc.
	ExtractText(ctx context.Context, doc Document, blob []byte) (string, error)
}

// NoopOCR is the Tier-1-or-fail placeholder: it always returns
// ErrOCRUnavailable so callers must succeed via the deterministic
// extractor or fail explicitly.
type NoopOCR struct{}

// ExtractText always returns ("", ErrOCRUnavailable).
func (NoopOCR) ExtractText(_ context.Context, _ Document, _ []byte) (string, error) {
	return "", ErrOCRUnavailable
}
