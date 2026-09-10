package bench

import (
	"strconv"
	"strings"

	"claimops-api/internal/eval/corpus"
	"claimops-api/internal/parser"
)

// ScoreCase deterministically scores one canonical artifact against one
// golden. It is a pure function: no I/O, no clock, stdlib only. It imports
// the corpus package for the Golden TYPE only and never calls the loader.
//
// Fields the scorer fills: CaseID, DocumentType, Difficulty, ParseOK,
// ArtifactValid, Fields, Tables, Provenance, Failures. LatencyMs and
// InputBytes are owned by the runner and are always set to 0 here.
//
// Failure-code contract (scorer's subset of the bench.go taxonomy):
//
//	Emitted:  PAGE_STRUCTURE_MISMATCH, EMPTY_ARTIFACT, FIELD_MISSING,
//	          TABLE_MISSING, TABLE_PARTIAL, PROVENANCE_MISSING,
//	          READING_ORDER_MISMATCH.
//	Never:    PARSE_FAILURE, UNSUPPORTED_MEDIA (runner owns parse outcomes);
//	          MISSING_TEXT, TEXT_CONTENT_MISMATCH (no text ground truth is
//	          compared — only key presence); FIELD_INCORRECT,
//	          PROVENANCE_INCORRECT (see MatchIncorrect note below);
//	          TABLE_STRUCTURE_MISMATCH (partial/missed cover the signal).
//
// Documented limitations encoded below:
//
//  1. MatchIncorrect is NEVER emitted. Text search can prove presence but
//     cannot distinguish "value absent" from "value present but wrong"
//     without field-level alignment, so every non-match is MatchMissing.
//     The class is reserved for future alignment scoring.
//  2. Reading order is a Y0-monotonicity sanity check over block boxes per
//     page, not true order scoring: goldens carry no order ground truth.
//     Pages whose blocks carry no boxes are skipped silently.
//  3. Provenance asymmetry: a matched hit without a block id raises
//     PROVENANCE_MISSING; a nil box is only counted in the tally, never a
//     failure (nil box is the honest vendor-silent representation).
//  4. Expected-table derivation: Golden carries no expected_tables (that
//     flag lives on corpus.Case metadata, which is NOT passed to ScoreCase),
//     so expected=1 when the golden has any line item OR the docType is a
//     bill ({hospital_bill, bill}); otherwise expected=0.
//  5. Page-count mismatch is skipped: Golden carries no page counts.
//  6. Line-item amounts are NOT scored as fields: they duplicate the bill
//     total signal, so only each item description is scored (as
//     "line_item[i]") and amounts are covered by total_amount_paise.
func ScoreCase(caseID, docType, difficulty string, doc parser.ParsedDocument, golden corpus.Golden) CaseResult {
	res := CaseResult{
		CaseID:       caseID,
		DocumentType: docType,
		Difficulty:   difficulty,
		// ParseOK is always true here: the scorer only ever sees a produced
		// artifact. Classifying vendor parse failures (PARSE_FAILURE,
		// UNSUPPORTED_MEDIA) belongs to the runner, which may skip scoring
		// or override this field.
		ParseOK:       true,
		ArtifactValid: doc.Validate() == nil,
		LatencyMs:     0,
		InputBytes:    0,
	}

	cands := collectCandidates(doc)

	// -- Field scoring -------------------------------------------------
	type expectation struct {
		key     string
		text    string // trimmed golden text; unused when numeric
		numeric bool
		amount  int64
	}
	var wants []expectation
	addText := func(key, v string) {
		if strings.TrimSpace(v) == "" {
			return // skip null/empty goldens
		}
		wants = append(wants, expectation{key: key, text: strings.TrimSpace(v)})
	}
	// Allowlist only: structural keys (expected_conflict, expected_empty,
	// bundle_id, case_id, document_type, lab_results, ...) are never
	// text-searched. DATE_CONFLICT goldens need no special casing: each
	// date value is scored independently for presence in THIS artifact.
	addText("claim_number", golden.ClaimNumber)
	addText("policy_number", golden.PolicyNumber)
	addText("patient_name", golden.PatientName)
	addText("hospital", golden.Hospital)
	addText("admission_date", golden.AdmissionDate)
	addText("discharge_date", golden.DischargeDate)
	if golden.TotalAmountPaise != nil {
		wants = append(wants, expectation{key: "total_amount_paise", numeric: true, amount: *golden.TotalAmountPaise})
	}
	for i, li := range golden.LineItems {
		if strings.TrimSpace(li.Description) == "" {
			continue
		}
		wants = append(wants, expectation{
			key:  "line_item[" + strconv.Itoa(i) + "]",
			text: strings.TrimSpace(li.Description),
		})
	}

	prov := ProvenanceScore{}
	provMissing := false
	for _, w := range wants {
		var h matchHit
		if w.numeric {
			h = matchNumeric(w.amount, cands)
		} else {
			h = matchText(w.text, cands)
		}
		if !h.found {
			res.Fields = append(res.Fields, FieldScore{Key: w.key, Match: MatchMissing})
			res.Failures = append(res.Failures, FailFieldMissing)
			continue
		}
		hasProv := h.ev.Page >= 1 && strings.TrimSpace(h.ev.BlockID) != ""
		res.Fields = append(res.Fields, FieldScore{Key: w.key, Match: h.class, HasProvenance: hasProv})
		prov.HitsTotal++
		if h.ev.Page >= 1 {
			prov.HitsWithPage++
		}
		if strings.TrimSpace(h.ev.BlockID) != "" {
			prov.HitsWithBlock++
		} else {
			provMissing = true
		}
		if h.ev.Box != nil {
			prov.HitsWithBox++
		}
	}
	res.Provenance = prov

	// -- Table scoring --------------------------------------------------
	res.Tables = scoreTables(docType, doc, golden)
	switch res.Tables.Verdict {
	case "TABLE_MISSED":
		res.Failures = append(res.Failures, FailTableMissing)
	case "TABLE_PARTIAL":
		res.Failures = append(res.Failures, FailTablePartial)
	}

	// -- Page structure --------------------------------------------------
	// Goldens carry no page counts, so only the degenerate case (zero
	// pages) is classified.
	if len(doc.Pages) == 0 {
		res.Failures = append(res.Failures, FailPageStructure)
	}

	// -- Empty artifact ---------------------------------------------------
	// Generalizes "zero blocks AND zero tables AND zero text": any artifact
	// whose blocks+cells carry no text at all is content-absent (blank /
	// raster fixtures land here BY DESIGN). ParseOK stays true: parsing
	// succeeded, content is absent.
	if totalTextLen(doc) == 0 {
		res.Failures = append(res.Failures, FailEmptyArtifact)
	}

	// -- Provenance failure (once; box absence is tally-only, see note 3) --
	if provMissing {
		res.Failures = append(res.Failures, FailProvenanceMissing)
	}

	// -- Reading order (Y0 monotonicity sanity check, see note 2) ---------
	if readingOrderViolated(doc) {
		res.Failures = append(res.Failures, FailReadingOrder)
	}

	return res
}

// candidate is one searchable text unit with its evidence.
type candidate struct {
	text string
	ev   parser.EvidenceLocation
}

// collectCandidates pools all block texts plus all table cell texts.
func collectCandidates(doc parser.ParsedDocument) []candidate {
	var out []candidate
	for _, p := range doc.Pages {
		for _, b := range p.Blocks {
			out = append(out, candidate{text: b.Text, ev: b.Evidence})
		}
		for _, t := range p.Tables {
			for _, r := range t.Rows {
				for _, c := range r.Cells {
					out = append(out, candidate{text: c.Text, ev: c.Evidence})
				}
			}
		}
	}
	return out
}

type matchHit struct {
	class MatchClass
	ev    parser.EvidenceLocation
	found bool
}

// matchText implements exact-then-normalized text search. Exact is a
// case-sensitive verbatim substring ("golden value appears verbatim in any
// candidate"). Normalized lowercases and keeps ASCII alphanumerics only,
// so dates match across separators ("2026-03-21" vs "2026/03/21") and
// names match case-insensitively. MatchIncorrect is never returned (see
// ScoreCase note 1).
func matchText(golden string, cands []candidate) matchHit {
	for _, c := range cands {
		if strings.Contains(c.text, golden) {
			return matchHit{class: MatchExact, ev: c.ev, found: true}
		}
	}
	ng := normAlnum(golden)
	if ng == "" {
		return matchHit{}
	}
	for _, c := range cands {
		if strings.Contains(normAlnum(c.text), ng) {
			return matchHit{class: MatchNormalized, ev: c.ev, found: true}
		}
	}
	return matchHit{}
}

// matchNumeric scores total_amount_paise (an int64 in PAISE) against
// rendered text. Exact is the verbatim decimal rendering appearing in a
// candidate; otherwise the rupees-vs-paise digit rule applies and any hit
// is MatchNormalized. MatchIncorrect is never returned (see ScoreCase
// note 1).
func matchNumeric(amountPaise int64, cands []candidate) matchHit {
	dec := strconv.FormatInt(amountPaise, 10)
	for _, c := range cands {
		if strings.Contains(c.text, dec) {
			return matchHit{class: MatchExact, ev: c.ev, found: true}
		}
	}
	for _, c := range cands {
		if numericAmountMatch(amountPaise, c.text) {
			return matchHit{class: MatchNormalized, ev: c.ev, found: true}
		}
	}
	return matchHit{}
}

// normAlnum lowercases s and keeps ASCII letters/digits only.
func normAlnum(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// digitsOnly strips every non-ASCII-digit rune.
func digitsOnly(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// numericAmountMatch implements the rupees-vs-paise rule with EXACT
// semantics: let g = digit-stripped decimal rendering of the golden paise
// value and c = digit-stripped candidate text. Match iff:
//
//	c == g,                                      (same unit)
//	c + "00" == g,                               (candidate in rupees, golden in paise)
//	g + "00" == c                                (symmetric guard: candidate carries a
//	                                             redundant minor-unit suffix)
//
// Examples: golden 14546500 (Rs 1,45,465.00) matches "Rs. 1,45,465.00"
// (c == g) and "Rs 145465" (c+"00" == g), but NOT "Rs. 1,45,466.00".
// Sign-blind by construction (FormatInt's '-' is stripped); goldens are
// never negative per corpus validation.
func numericAmountMatch(amountPaise int64, candText string) bool {
	g := digitsOnly(strconv.FormatInt(amountPaise, 10))
	c := digitsOnly(candText)
	if c == "" || g == "" {
		return false
	}
	if c == g {
		return true
	}
	if c+"00" == g {
		return true
	}
	if g+"00" == c {
		return true
	}
	return false
}

// scoreTables derives the expected table count (see ScoreCase note 4),
// counts detected tables, and matches rows/cells against line-item
// descriptions (normalized substring).
func scoreTables(docType string, doc parser.ParsedDocument, golden corpus.Golden) TableScore {
	var descs []string
	for _, li := range golden.LineItems {
		if d := strings.TrimSpace(li.Description); d != "" {
			descs = append(descs, d)
		}
	}
	normDescs := make([]string, 0, len(descs))
	for _, d := range descs {
		if n := normAlnum(d); n != "" {
			normDescs = append(normDescs, n)
		}
	}

	dt := strings.ToLower(strings.TrimSpace(docType))
	expected := 0
	if len(descs) > 0 || dt == "hospital_bill" || dt == "bill" {
		expected = 1
	}

	ts := TableScore{ExpectedTables: expected}
	for _, p := range doc.Pages {
		ts.DetectedTables += len(p.Tables)
		for _, t := range p.Tables {
			for _, r := range t.Rows {
				rowHit := false
				for _, c := range r.Cells {
					if cellMatchesDescs(c.Text, normDescs) {
						ts.CellsMatched++
						rowHit = true
					}
				}
				if rowHit {
					ts.RowsMatched++
				}
			}
		}
	}

	switch {
	case expected == 0 && ts.DetectedTables == 0:
		ts.Verdict = "TABLE_NA" // no table ground truth, none found: not a failure
	case expected == 0:
		// Extra structure without expectation is not a failure.
		ts.Verdict = "TABLE_PASS"
	case ts.DetectedTables == 0:
		// A bill flattened to paragraphs lands here: never silently a pass.
		ts.Verdict = "TABLE_MISSED"
	case len(normDescs) == 0:
		// Bill docType with no item descriptions: no row ground truth to
		// check, so a detected table passes on structure alone.
		ts.Verdict = "TABLE_PASS"
	case ts.RowsMatched >= len(normDescs):
		ts.Verdict = "TABLE_PASS"
	default:
		ts.Verdict = "TABLE_PARTIAL"
	}
	return ts
}

// cellMatchesDescs reports whether the cell contains any line-item
// description (normalized substring).
func cellMatchesDescs(cellText string, normDescs []string) bool {
	if len(normDescs) == 0 {
		return false
	}
	nc := normAlnum(cellText)
	if nc == "" {
		return false
	}
	for _, nd := range normDescs {
		if strings.Contains(nc, nd) {
			return true
		}
	}
	return false
}

// totalTextLen sums trimmed text over all blocks and cells.
func totalTextLen(doc parser.ParsedDocument) int {
	n := 0
	for _, p := range doc.Pages {
		for _, b := range p.Blocks {
			n += len(strings.TrimSpace(b.Text))
		}
		for _, t := range p.Tables {
			for _, r := range t.Rows {
				for _, c := range r.Cells {
					n += len(strings.TrimSpace(c.Text))
				}
			}
		}
	}
	return n
}

// readingOrderViolated verifies Y0 non-decreasing in block order per page
// over blocks that carry a box. Pages with no boxes are skipped silently:
// without boxes the order cannot be judged, so no failure is raised (see
// ScoreCase note 2). Table cell boxes are excluded: only block order is
// checked.
func readingOrderViolated(doc parser.ParsedDocument) bool {
	for _, p := range doc.Pages {
		first := true
		prev := 0.0
		for _, b := range p.Blocks {
			if b.Evidence.Box == nil {
				continue
			}
			y := b.Evidence.Box.Y0
			if !first && y < prev {
				return true
			}
			prev = y
			first = false
		}
	}
	return false
}
