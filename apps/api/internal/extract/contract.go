// Extractor contract and conformance runner: every canonical extractor
// must pass RunConformance against its Extractor implementation. The
// runner exercises the observation guarantees only — extractor identity,
// status-rule shape, provenance pinning, and the no-fabrication rule.
// Observation quality (did it find the right fields) is measured by
// later benchmark work, not here.
package extract

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"claimops-api/internal/parser"
)

// Extractor converts one canonical artifact into canonical observations.
// ctx carries cancellation/deadline only. Implementations report what the
// artifact says per the FieldStatus rules; they never judge validity —
// that is verify's job. The returned facts must satisfy the conformance
// checks below.
type Extractor interface {
	// Extract converts doc into canonical facts. doc is a validated
	// parser.ParsedDocument; implementations must not reach behind it
	// to vendor types or untrusted bytes.
	Extract(ctx context.Context, doc parser.ParsedDocument) (DocumentFacts, error)
	// Name is the stable extractor name recorded on every
	// ExtractedField (e.g. "regex-extract").
	Name() string
	// Version is the extractor version for reproducibility
	// (e.g. "0.4.2"). It must change whenever extraction behavior
	// changes.
	Version() string
}

// Mapping note: DocumentFacts -> verify.Input. The full mapper is #47's
// job; this note pins the field correspondence so the contract stays
// constructible (compile-level proof lives in extract_test.go,
// TestVerifyInputConstructible):
//
//	policy_number / claim_number PRESENT Normalized (NormalizeID trims;
//	  verify R1 casefolds both sides at compare time)
//	  -> Input.ClaimPolicyNumber / Input.PolicyNumber (R1)
//	patient_name PRESENT Normalized (NormalizeName mirrors
//	  verify.normalizeName, so the fact arrives pre-folded)
//	  -> Input.ClaimPatientName / Input.PolicyPatient, and per-document
//	  DocEvidence[docType]["patient_name"] (R2)
//	admission_date / discharge_date PRESENT Normalized (strict YYYY-MM-DD)
//	  parsed to time.Time -> Input.Admission/Discharge with
//	  HasAdmission/HasDischarge (R3); the same normalized strings under
//	  DocEvidence[*]["admission_date"] feed R10 cross-source agreement
//	total_bill PRESENT via NormalizePaise (exact int64 paise)
//	  -> Input.BillTotalPaise + HasBillTotal (R5/R6); bill line keys ->
//	  Input.BillLinePaise; the claimed-amount key -> Input.ClaimedPaise
//	facts.DocType ("CLAIM_FORM" / "DISCHARGE_SUMMARY" / "HOSPITAL_BILL")
//	  -> Input.DocsPresent[docType] = true (R8; see verify.DocClaimForm
//	  and kin for the required keys)
//	MISSING / AMBIGUOUS / MULTI_CANDIDATE observations NEVER feed typed
//	  Input fields directly: no bare value crosses into judgment.
//	  MULTI_CANDIDATE keys are held for #47's conflict path (exception
//	  input or HITL); AMBIGUOUS raw values are preserved for HITL display
//	  only.
//	EvidenceRef (document + page + block) lets #47 attach EvidenceIDs to
//	  verify exceptions, paralleling evidence.FieldEvidence provenance.

// RunConformance asserts the Extractor contract. Extractor tests call it
// as:
//
//	func TestMyExtractorConforms(t *testing.T) {
//		extract.RunConformance(t, NewMyExtractor(), fixtureDoc)
//	}
//
// Failures mean the extractor violates the observation boundary (bad
// status shape, missing provenance, fabricated values), not that
// observation quality is poor (quality is later benchmark work's job).
func RunConformance(t *testing.T, ext Extractor, fixture parser.ParsedDocument) {
	t.Helper()
	for _, v := range checkConformance(ext, fixture) {
		t.Error("contract: " + v)
	}
}

// checkConformance is the pure core behind RunConformance: it returns one
// message per violation, empty when conformant. It is unexported so the
// public entry keeps the exact (t, Extractor, fixture) shape, while tests
// can still assert the REJECT path (a fabricating stub must yield
// violations) without failing the suite.
func checkConformance(ext Extractor, fixture parser.ParsedDocument) []string {
	var out []string
	fail := func(format string, args ...any) {
		// Sprintf-style single site keeps message construction uniform.
		msg := fmt.Sprintf(format, args...)
		out = append(out, msg)
	}
	if strings.TrimSpace(ext.Name()) == "" {
		fail("extractor Name must be non-blank")
	}
	if strings.TrimSpace(ext.Version()) == "" {
		fail("extractor Version must be non-blank")
	}
	if err := fixture.Validate(); err != nil {
		return append(out, "fixture invalid: "+err.Error())
	}
	facts, err := ext.Extract(context.Background(), fixture)
	if err != nil {
		return append(out, "Extract(valid fixture) = "+err.Error()+", want facts")
	}
	if facts.DocumentID != fixture.DocumentID {
		fail("facts id = %q, want artifact %q", facts.DocumentID, fixture.DocumentID)
	}
	artifact := artifactText(fixture)
	for _, key := range slices.Sorted(maps.Keys(facts.Fields)) {
		f := facts.Fields[key]
		where := "field " + strconv.Quote(key)
		if strings.TrimSpace(f.Key) == "" {
			fail("%s: Key must be non-blank", where)
		} else if f.Key != key {
			fail("%s: Key = %q, want map key", where, f.Key)
		}
		if f.Extractor != ext.Name() {
			fail("%s: Extractor = %q, want %q", where, f.Extractor, ext.Name())
		}
		if f.ExtractorVersion != ext.Version() {
			fail("%s: ExtractorVersion = %q, want %q", where, f.ExtractorVersion, ext.Version())
		}
		switch f.Status {
		case StatusPresent:
			if strings.TrimSpace(f.Value) == "" {
				fail("%s: PRESENT with blank value (absent values must be MISSING)", where)
			}
			checkPinned(fail, where, facts.DocumentID, f.Evidence)
			checkGrounded(fail, where, artifact, f.Value)
		case StatusMissing:
			if f.Value != "" || f.Normalized != "" {
				fail("%s: MISSING carries a value (must carry none)", where)
			}
			if len(f.Candidates) != 0 {
				fail("%s: MISSING with %d candidates (must carry none)", where, len(f.Candidates))
			}
			if f.Evidence != (EvidenceRef{}) {
				fail("%s: MISSING with evidence pin (pins nothing)", where)
			}
		case StatusAmbiguous:
			if strings.TrimSpace(f.Value) == "" {
				fail("%s: AMBIGUOUS with blank value (raw must be preserved)", where)
			}
			if f.Normalized != "" {
				fail("%s: AMBIGUOUS with normalized form (unparseable values normalize to nothing)", where)
			}
			checkPinned(fail, where, facts.DocumentID, f.Evidence)
			checkGrounded(fail, where, artifact, f.Value)
		case StatusMultiCandidate:
			if f.Value != "" || f.Normalized != "" {
				fail("%s: MULTI_CANDIDATE with top-level value (never a silent winner)", where)
			}
			if len(f.Candidates) < 2 {
				fail("%s: MULTI_CANDIDATE with %d candidates (want >= 2)", where, len(f.Candidates))
			}
			for i, c := range f.Candidates {
				cwhere := fmt.Sprintf("%s candidate %d", where, i)
				if strings.TrimSpace(c.Value) == "" {
					fail("%s: blank value", cwhere)
				}
				checkPinned(fail, cwhere, facts.DocumentID, c.Evidence)
				checkGrounded(fail, cwhere, artifact, c.Value)
			}
		default:
			fail("%s: unknown status %q", where, string(f.Status))
		}
	}
	return out
}

// checkPinned asserts page-level provenance: the pin must reference this
// document on a 1-based page. BlockID may be empty (table cells carry no
// block-id by parser contract); boxes do not exist at this layer.
func checkPinned(fail func(string, ...any), where, docID string, ev EvidenceRef) {
	if ev.DocumentID != docID {
		fail("%s: evidence doc = %q, want %q", where, ev.DocumentID, docID)
	}
	if ev.Page < 1 {
		fail("%s: evidence page = %d, want >= 1", where, ev.Page)
	}
}

// checkGrounded enforces the no-fabrication rule: every observed raw
// value must be traceable to the artifact text, either exactly or under
// case/whitespace folding. Semantic equivalence (unit conversion,
// paraphrase) does NOT count as grounding — the raw string must appear.
// Blank values are skipped (the status-shape checks own them).
func checkGrounded(fail func(string, ...any), where, artifact, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	if !grounded(artifact, value) {
		fail("%s: value %q not grounded in artifact text (no fabrication)", where, value)
	}
}

// artifactText concatenates every block text and table cell text of the
// artifact. It is the grounding corpus for the no-fabrication check.
func artifactText(doc parser.ParsedDocument) string {
	var b strings.Builder
	for _, p := range doc.Pages {
		for _, blk := range p.Blocks {
			b.WriteString(blk.Text)
			b.WriteByte('\n')
		}
		for _, tbl := range p.Tables {
			for _, row := range tbl.Rows {
				for _, cell := range row.Cells {
					b.WriteString(cell.Text)
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}

// grounded reports whether value appears in artifact exactly or under
// case/whitespace folding.
func grounded(artifact, value string) bool {
	if strings.Contains(artifact, value) {
		return true
	}
	folded := foldText(value)
	return folded != "" && strings.Contains(foldText(artifact), folded)
}

// foldText lowercases s and collapses all whitespace runs to single
// spaces, so "Aparna   PRADHAN" folds to "aparna pradhan".
func foldText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
