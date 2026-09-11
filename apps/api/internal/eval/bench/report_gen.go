package bench

import (
	"fmt"
	"sort"
	"strings"
)

// ReportVersion is the version of the deterministic markdown renderer.
// It is recorded in the report footer so a human reader can tell which
// renderer produced the text from a given benchmark.json.
//
// Single-source rule: benchmark.json stays the ONLY machine artifact. No
// second JSON bundle is emitted because a second artifact could drift from
// the first (stale copies, divergent fingerprints). The markdown report is
// a pure function of Report (cases + meta) plus this renderer version, so
// any consumer can regenerate it byte-identically with RenderMarkdown.
func ReportVersion() string { return "v1" }

// triageTable is the fixed deterministic mapping from failure code to
// triage class plus a one-line rationale. It is a lookup table, not a
// heuristic: the same code always maps to the same class. Codes that the
// current scorer never emits still have entries documenting WHY they are
// absent (contract reservation), so the exhaustiveness test can iterate
// the full bench.go taxonomy. Codes with no entry default to investigate.
var triageTable = map[FailureCode]string{
	FailParseFailure:        "corpus-golden: fixture violated the harness trust boundary or the vendor failed; correct behavior on corrupted fixtures — flag for #33 if seen on clean PDFs.",
	FailUnsupportedMedia:    "corpus-golden: fixture media outside the LiteParse adapter declared scope; image/raster escalation is a #33 question, not a parse bug.",
	FailEmptyArtifact:       "corpus-golden: expected-by-design on raster/blank fixtures (OCR is off); ParseOK stays true and content is absent — flag for #33 if seen on text PDFs.",
	FailMissingText:         "contract-design: not emitted by the current scorer (no text ground truth is compared, only key presence); reserved for future text scoring.",
	FailTextMismatch:        "contract-design: not emitted by the current scorer (no text ground truth is compared, only key presence); reserved for future text scoring.",
	FailPageStructure:       "adapter-mapping: adapter emitted an artifact failing canonical Validate (mapping bug class) or the degenerate zero-page case.",
	FailReadingOrder:        "contract-design: Y0-monotonicity sanity check only — goldens carry no order ground truth and boxless pages skip silently; beyond-Y0 order is unscored by design.",
	FailFieldMissing:        "investigate: do not guess cause from counts alone — on D0 clean PDFs flag for #33 investigation; on raster/blank tiers expected alongside EMPTY_ARTIFACT.",
	FailFieldIncorrect:      "contract-design: never emitted by the current scorer — text search cannot distinguish incorrect from missing, so every non-match is MatchMissing; reserved for future alignment scoring.",
	FailTableMissing:        "parser-limitation: bill flattened to paragraphs or an ungridded layout the parser did not reconstruct as a table; never silently a pass.",
	FailTablePartial:        "parser-limitation: table detected but fewer line-item rows matched than expected; row/cell reconstruction gap.",
	FailTableStructure:      "contract-design: not emitted by the current scorer — partial/missed cover the signal; reserved.",
	FailProvenanceMissing:   "contract-design: table cells carry no block-id field by contract (adapter convertRow sets page and box only) — cell-matched hits are unavailable-by-design, not parser failure.",
	FailProvenanceIncorrect: "contract-design: never emitted — the scorer records provenance present/absent only, never correctness.",
}

// triageCode maps one failure code through the fixed table. Unknown codes
// default to investigate: no guessing.
func triageCode(code FailureCode) string {
	if v, ok := triageTable[code]; ok {
		return v
	}
	return "investigate: unrecognized failure code with no fixed mapping — flag for #33; do not guess cause."
}

// Triage classifies each DISTINCT failure code present in the report into
// parser-limitation | adapter-mapping | corpus-golden | contract-design
// (or investigate) with a one-line rationale. Output depends only on which
// codes are present, never on counts, order, or values.
func Triage(r Report) map[FailureCode]string {
	out := map[FailureCode]string{}
	for _, c := range r.Cases {
		for _, f := range c.Failures {
			if _, ok := out[f]; !ok {
				out[f] = triageCode(f)
			}
		}
	}
	return out
}

// sortedCases returns a copy of cases ordered by case id ascending so the
// renderer is a pure function of the case SET, not of input order.
func sortedCases(cases []CaseResult) []CaseResult {
	out := append([]CaseResult(nil), cases...)
	sort.Slice(out, func(i, j int) bool { return out[i].CaseID < out[j].CaseID })
	return out
}

// rateStr formats part/whole as a one-decimal percent, or "n/a" when the
// denominator is zero (zero-case categories must not divide by zero).
func rateStr(part, whole int) string {
	if whole == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(part)/float64(whole))
}

// countRate formats "N (R%)" or "N (n/a)".
func countRate(part, whole int) string {
	return fmt.Sprintf("%d (%s)", part, rateStr(part, whole))
}

// FieldTable renders the per-field recovery table: for each golden key
// observed in r.Cases[].Fields, the exact/normalized/missing/incorrect
// counts across cases. Keys, counts and match classes only — values never
// appear (CaseResult carries none by construction).
func FieldTable(r Report) string {
	type counts struct {
		total, exact, normalized, missing, incorrect int
	}
	m := map[string]*counts{}
	for _, c := range sortedCases(r.Cases) {
		seen := map[string]bool{}
		for _, f := range c.Fields {
			cc := m[f.Key]
			if cc == nil {
				cc = &counts{}
				m[f.Key] = cc
			}
			if !seen[f.Key] {
				cc.total++
				seen[f.Key] = true
			}
			switch f.Match {
			case MatchExact:
				cc.exact++
			case MatchNormalized:
				cc.normalized++
			case MatchMissing:
				cc.missing++
			case MatchIncorrect:
				cc.incorrect++
			}
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("| field key | cases with key | exact | normalized | missing | incorrect |\n")
	sb.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	if len(keys) == 0 {
		sb.WriteString("| (no fields scored) | n/a | n/a | n/a | n/a | n/a |\n")
		return sb.String()
	}
	for _, k := range keys {
		cc := m[k]
		fmt.Fprintf(&sb, "| %s | %d | %s | %s | %s | %s |\n",
			k, cc.total,
			countRate(cc.exact, cc.total),
			countRate(cc.normalized, cc.total),
			countRate(cc.missing, cc.total),
			countRate(cc.incorrect, cc.total))
	}
	return sb.String()
}

// tableBuckets folds table verdicts over cases. Verdicts follow the
// TableScore vocabulary; cases that never reached scoring (empty verdict)
// land in unscored, and TABLE_NA (no table ground truth, none found) is
// reported separately so it can never be mistaken for a pass.
type tableBuckets struct {
	pass, partial, missed, na, unscored int
}

func foldTables(cases []CaseResult) tableBuckets {
	var b tableBuckets
	for _, c := range cases {
		switch c.Tables.Verdict {
		case "TABLE_PASS":
			b.pass++
		case "TABLE_PARTIAL":
			b.partial++
		case "TABLE_MISSED":
			b.missed++
		case "TABLE_NA":
			b.na++
		default:
			b.unscored++
		}
	}
	return b
}

// groupKeys returns the sorted distinct values of select over cases.
func groupKeys(cases []CaseResult, selectFn func(CaseResult) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cases {
		k := selectFn(c)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// RenderMarkdown renders the full human-readable benchmark report.
// Deterministic: cases are sorted by id internally, groups and codes are
// emitted in sorted order, and the output contains no timestamps and no
// field values (keys, counts, codes and case ids only).
//
// The report leads with dimensions (parse, fields, tables) and never with
// a single accuracy number: exact+normalized are reported side by side,
// never merged into one headline metric.
func RenderMarkdown(r Report) string {
	cases := sortedCases(r.Cases)
	overall := summarize(cases)
	tables := foldTables(cases)
	triage := Triage(r)

	var sb strings.Builder
	sb.WriteString("# Parser benchmark report (LiteParse-only evidence)\n\n")
	sb.WriteString("Scope: LiteParse adapter only. Docling was dropped from the current stage for " +
		"operational footprint reasons; this report contains no Docling data and makes no comparative claim.\n\n")
	fmt.Fprintf(&sb, "meta: corpus=%s cases=%d parser=%s %s adapter=%s report_version=%s\n\n",
		r.Meta.CorpusVersion, r.Meta.CorpusCases,
		r.Meta.ParserName, r.Meta.ParserVersion, r.Meta.AdapterName, ReportVersion())
	sb.WriteString("Single source: benchmark.json is the only machine artifact. This markdown is a " +
		"deterministic rendering of it (renderer " + ReportVersion() + "); no second artifact is emitted " +
		"because a second artifact could drift from the first.\n\n")

	// -- Evidence questions ------------------------------------------------
	sb.WriteString("## Evidence questions (14)\n\n")
	for _, q := range []string{
		"1. What fraction of cases parsed OK? (see Overall dimensions)",
		"2. How does field recovery split across exact/normalized/missing? (see Overall dimensions; no single accuracy number is reported)",
		"3. How does table reconstruction split across pass/partial/missed? (see Table behavior)",
		"4. How do results vary by difficulty tier? (see By difficulty)",
		"5. How do results vary by document type? (see By type)",
		"6. Which field keys fail most? (see Per-field recovery)",
		"7. Are bills flattened to paragraphs counted as passes? (No — see Table behavior: flattened-to-paragraphs is TABLE_MISSED)",
		"8. What provenance is available across page/block/box? (see Provenance)",
		"9. Are table-cell provenance gaps parser failures? (No — see Provenance: unavailable-by-design under the contract)",
		"10. Is reading order preserved, and where is it violated? (see Reading order)",
		"11. Which cases are worst, and why? (see Worst cases)",
		"12. What is the failure taxonomy distribution and triage? (see Failure taxonomy and triage)",
		"13. What are the scorer and contract limitations? (see Limitations)",
		"14. What does the evidence imply for OCR escalation and #33 routing? (see OCR implications and Input to #33; questions, not decisions)",
	} {
		sb.WriteString(q + "\n")
	}
	sb.WriteString("\n")

	// -- Overall ------------------------------------------------------------
	sb.WriteString("## Overall dimensions (no single accuracy number)\n\n")
	fieldTotal := overall.FieldsExact + overall.FieldsNormalized + overall.FieldsMissing + overall.FieldsIncorrect
	fmt.Fprintf(&sb, "parse_ok: %s\n\n", countRate(overall.ParseOK, overall.Cases))
	sb.WriteString("fields (counts and rates over all scored field keys; exact and normalized are reported side by side, never merged):\n\n")
	fmt.Fprintf(&sb, "- exact: %s\n- normalized: %s\n- missing: %s\n- incorrect: %s\n\n",
		countRate(overall.FieldsExact, fieldTotal),
		countRate(overall.FieldsNormalized, fieldTotal),
		countRate(overall.FieldsMissing, fieldTotal),
		countRate(overall.FieldsIncorrect, fieldTotal))
	sb.WriteString("tables (verdict counts over all cases):\n\n")
	fmt.Fprintf(&sb, "- TABLE_PASS: %d\n- TABLE_PARTIAL: %d\n- TABLE_MISSED: %d\n- TABLE_NA (no table ground truth, none found): %d\n- unscored (never reached scoring): %d\n\n",
		tables.pass, tables.partial, tables.missed, tables.na, tables.unscored)
	sb.WriteString("No single accuracy number is reported by design; the dimensions above are the headline.\n\n")

	// -- By difficulty -------------------------------------------------------
	sb.WriteString("## By difficulty\n\n")
	tiers := groupKeys(cases, func(c CaseResult) string { return c.Difficulty })
	if len(tiers) == 0 {
		sb.WriteString("(no cases)\n\n")
	} else {
		sb.WriteString("| difficulty | cases | parse_ok | exact | normalized | missing | incorrect | tables pass | partial | missed |\n")
		sb.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
		for _, t := range tiers {
			a := summarize(filterBy(cases, func(c CaseResult) bool { return c.Difficulty == t }))
			tb := foldTables(filterBy(cases, func(c CaseResult) bool { return c.Difficulty == t }))
			ft := a.FieldsExact + a.FieldsNormalized + a.FieldsMissing + a.FieldsIncorrect
			fmt.Fprintf(&sb, "| %s | %d | %s | %s | %s | %s | %s | %d | %d | %d |\n",
				t, a.Cases, countRate(a.ParseOK, a.Cases),
				countRate(a.FieldsExact, ft), countRate(a.FieldsNormalized, ft),
				countRate(a.FieldsMissing, ft), countRate(a.FieldsIncorrect, ft),
				tb.pass, tb.partial, tb.missed)
		}
		sb.WriteString("\n")
	}

	// -- By type --------------------------------------------------------------
	sb.WriteString("## By type\n\n")
	types := groupKeys(cases, func(c CaseResult) string { return c.DocumentType })
	if len(types) == 0 {
		sb.WriteString("(no cases)\n\n")
	} else {
		sb.WriteString("| document type | cases | parse_ok | exact | normalized | missing | incorrect | tables pass | partial | missed |\n")
		sb.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
		for _, t := range types {
			a := summarize(filterBy(cases, func(c CaseResult) bool { return c.DocumentType == t }))
			tb := foldTables(filterBy(cases, func(c CaseResult) bool { return c.DocumentType == t }))
			ft := a.FieldsExact + a.FieldsNormalized + a.FieldsMissing + a.FieldsIncorrect
			fmt.Fprintf(&sb, "| %s | %d | %s | %s | %s | %s | %s | %d | %d | %d |\n",
				t, a.Cases, countRate(a.ParseOK, a.Cases),
				countRate(a.FieldsExact, ft), countRate(a.FieldsNormalized, ft),
				countRate(a.FieldsMissing, ft), countRate(a.FieldsIncorrect, ft),
				tb.pass, tb.partial, tb.missed)
		}
		sb.WriteString("\n")
	}

	// -- Per field --------------------------------------------------------------
	sb.WriteString("## Per-field recovery (keys only, never values)\n\n")
	sb.WriteString(FieldTable(r))
	sb.WriteString("\n")

	// -- Table behavior -----------------------------------------------------------
	sb.WriteString("## Table behavior\n\n")
	fmt.Fprintf(&sb, "Verdicts observed: TABLE_PASS %d, TABLE_PARTIAL %d, TABLE_MISSED %d, TABLE_NA %d, unscored %d.\n\n",
		tables.pass, tables.partial, tables.missed, tables.na, tables.unscored)
	sb.WriteString("A bill flattened to paragraphs is TABLE_MISSED (failure code TABLE_MISSING), " +
		"never silently equivalent to a pass. TABLE_NA means the golden carried no table ground truth " +
		"and none was found; it is not a pass and not a failure.\n\n")

	// -- Provenance ------------------------------------------------------------------
	sb.WriteString("## Provenance\n\n")
	var hits, withPage, withBlock, withBox int
	for _, c := range cases {
		hits += c.Provenance.HitsTotal
		withPage += c.Provenance.HitsWithPage
		withBlock += c.Provenance.HitsWithBlock
		withBox += c.Provenance.HitsWithBox
	}
	fmt.Fprintf(&sb, "Matched hits: %d; with page %s; with block id %s; with box %s.\n\n",
		hits, countRate(withPage, hits), countRate(withBlock, hits), countRate(withBox, hits))
	sb.WriteString("Nil boxes are tally-only: a nil box is the honest vendor-silent representation of " +
		"absent geometry and never raises a failure on its own. Table cells carry no block-id field by " +
		"contract design (the adapter sets page and box only), so block-id gaps on cell-matched fields " +
		"are unavailable-by-design, not parser failure; see the triage table for the PROVENANCE_MISSING classification.\n\n")

	// -- Reading order ----------------------------------------------------------------
	sb.WriteString("## Reading order\n\n")
	var orderLines []string
	for _, c := range cases {
		if hasCode(c, FailReadingOrder) {
			orderLines = append(orderLines, c.CaseID+" ("+c.DocumentType+")")
		}
	}
	if len(orderLines) == 0 {
		sb.WriteString("No cases carry READING_ORDER_MISMATCH.\n\n")
	} else {
		sb.WriteString("Cases carrying READING_ORDER_MISMATCH (case id + document type):\n\n")
		for _, l := range orderLines {
			sb.WriteString("- " + l + "\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Order is judged by Y0-monotonicity over blocks that carry a box; boxless pages skip " +
		"silently and goldens carry no order ground truth (see Limitations).\n\n")

	// -- Worst cases ---------------------------------------------------------------------
	sb.WriteString("## Worst cases\n\n")
	byID := map[string]CaseResult{}
	for _, c := range cases {
		byID[c.CaseID] = c
	}
	worst := append([]string(nil), r.WorstCases...)
	if len(worst) == 0 && len(cases) > 0 {
		worst = worstCases(cases, 8)
	}
	sort.Strings(worst)
	if len(worst) == 0 {
		sb.WriteString("(none)\n\n")
	} else {
		sb.WriteString("Worst cases in case-id order with their failure codes (keys/counts/codes only):\n\n")
		for _, id := range worst {
			codes := "(no failures)"
			if c, ok := byID[id]; ok && len(c.Failures) > 0 {
				var parts []string
				for _, f := range c.Failures {
					parts = append(parts, string(f))
				}
				codes = strings.Join(parts, ", ")
			}
			fmt.Fprintf(&sb, "- %s: %s\n", id, codes)
		}
		sb.WriteString("\n")
	}

	// -- Failure taxonomy and triage ----------------------------------------------------------
	sb.WriteString("## Failure taxonomy and triage\n\n")
	taxCounts := map[string]int{}
	for _, c := range cases {
		for _, f := range c.Failures {
			taxCounts[string(f)]++
		}
	}
	ranked := topCodes(taxCounts, len(taxCounts))
	if len(ranked) == 0 {
		sb.WriteString("(no failures recorded)\n\n")
	} else {
		sb.WriteString("| failure code | count | triage |\n")
		sb.WriteString("| --- | --- | --- |\n")
		for _, kv := range ranked {
			fmt.Fprintf(&sb, "| %s | %d | %s |\n", kv.code, kv.count, triage[FailureCode(kv.code)])
		}
		sb.WriteString("\n")
	}

	// -- Limitations -------------------------------------------------------------------------------
	sb.WriteString("## Limitations\n\n")
	for _, l := range []string{
		"- FIELD_INCORRECT is never emitted: text search proves presence but cannot distinguish a wrong value from an absent one, so every non-match is MatchMissing; the class is reserved for future alignment scoring.",
		"- Reading order is a Y0-monotonicity sanity check over block boxes per page, not true order scoring: goldens carry no order ground truth and pages whose blocks carry no boxes are skipped silently.",
		"- A nil box is tally-only and never a failure: it is the honest vendor-silent representation of absent geometry.",
		"- Latency is measured but excluded from repeatability: RepeatEqual zeroes latency, so identical scores with different wall-clock times are repeat-equal by design.",
		"- Expected-table derivation: the golden carries no expected_tables flag, so one table is expected when the golden has any line item or the document type is a bill; otherwise zero.",
		"- Confidence note: the LiteParse adapter stamps vendor-silent 1.0 on every block because the vendor exposes no confidence signal. That 1.0 is uncalibrated, the scorer never rewards confidence values, and this report makes no certainty claim about the parser.",
	} {
		sb.WriteString(l + "\n")
	}
	sb.WriteString("\n")

	// -- OCR implications ------------------------------------------------------------------------------
	sb.WriteString("## OCR implications\n\n")
	sb.WriteString("OCR is off in the current LiteParse adapter. Tier evidence below is descriptive; " +
		"escalation is a #33 question, not a decision made here.\n\n")
	if len(tiers) == 0 {
		sb.WriteString("(no tiers)\n\n")
	} else {
		for _, t := range tiers {
			var n, ok, empty, unsup, pf, missFields, missed int
			for _, c := range cases {
				if c.Difficulty != t {
					continue
				}
				n++
				if c.ParseOK {
					ok++
				}
				for _, f := range c.Failures {
					switch f {
					case FailEmptyArtifact:
						empty++
					case FailUnsupportedMedia:
						unsup++
					case FailParseFailure:
						pf++
					}
				}
				for _, f := range c.Fields {
					if f.Match == MatchMissing || f.Match == MatchIncorrect {
						missFields++
					}
				}
				if c.Tables.Verdict == "TABLE_MISSED" {
					missed++
				}
			}
			absent := empty + unsup + pf
			var verdict string
			switch {
			case absent > 0:
				verdict = fmt.Sprintf("escalation candidate — content absent without OCR on %d of %d cases (parse failures/empty/unsupported); question for #33 whether managed OCR escalation is warranted for this tier.", absent, n)
			case ok == n && missFields == 0 && missed == 0:
				verdict = "no OCR justified — clean recovery without OCR on all cases in this tier."
			default:
				verdict = "mixed — partial recovery without OCR; question for #33: are the misses OCR-recoverable (raster content) or structural (layout/mapping)?"
			}
			fmt.Fprintf(&sb, "- %s: cases=%d parse_ok=%d field-misses=%d tables-missed=%d => %s\n", t, n, ok, missFields, missed, verdict)
		}
		sb.WriteString("\n")
	}

	// -- Input to #33 ------------------------------------------------------------------------------------------------
	sb.WriteString("## Input to #33 (questions, not decisions)\n\n")
	sb.WriteString("No production routing decision is made here; benchmark evidence only. Routing candidates below appear only if the evidence indicates them, framed as questions.\n\n")
	var questions []string
	var d0Missing []string
	for _, c := range cases {
		if c.Difficulty == "D0" && c.ParseOK && hasCode(c, FailFieldMissing) {
			d0Missing = append(d0Missing, c.CaseID)
		}
	}
	if len(d0Missing) > 0 {
		questions = append(questions, "- FIELD_MISSING on clean D0 parses ("+strings.Join(d0Missing, ", ")+"): what loses fields where content is present? Flagged for investigation; no cause is guessed here.")
	}
	var billMissed int
	for _, c := range cases {
		dt := strings.ToLower(strings.TrimSpace(c.DocumentType))
		if (dt == "hospital_bill" || dt == "bill") && hasCode(c, FailTableMissing) {
			billMissed++
		}
	}
	if billMissed > 0 {
		questions = append(questions, fmt.Sprintf("- TABLE_MISSING on %d bill cases: is a table-capable route or corpus requalification needed?", billMissed))
	}
	var emptyCases int
	for _, c := range cases {
		if hasCode(c, FailEmptyArtifact) || hasCode(c, FailUnsupportedMedia) {
			emptyCases++
		}
	}
	if emptyCases > 0 {
		questions = append(questions, fmt.Sprintf("- %d content-absent cases (EMPTY_ARTIFACT/UNSUPPORTED_MEDIA): should #33 route those tiers to managed OCR escalation?", emptyCases))
	}
	if _, ok := triage[FailProvenanceMissing]; ok {
		questions = append(questions, "- PROVENANCE_MISSING triages as contract-design for cells; should #33 require block ids on blocks (adapter-mapping) or accept cell gaps as unavailable-by-design?")
	}
	if _, ok := triage[FailReadingOrder]; ok {
		questions = append(questions, "- READING_ORDER_MISMATCH observed under a Y0-monotonicity check only; should #33 invest in true order ground truth before treating order as a routing signal?")
	}
	if len(questions) == 0 {
		sb.WriteString("No routing signal in this evidence: no clean-tier field loss, no bill table misses, no content-absent cases, no provenance or order findings.\n\n")
	} else {
		for _, q := range questions {
			sb.WriteString(q + "\n")
		}
		sb.WriteString("\n")
	}

	// -- Footer ---------------------------------------------------------------------------
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "report_version: %s | corpus=%s cases=%d parser=%s %s adapter=%s | deterministic (sorted by case id; no timestamps) | LiteParse-only evidence: no Docling comparison | keys/counts/codes only, no field values.\n",
		ReportVersion(), r.Meta.CorpusVersion, r.Meta.CorpusCases,
		r.Meta.ParserName, r.Meta.ParserVersion, r.Meta.AdapterName)
	return sb.String()
}

// hasCode reports whether the case carries the failure code.
func hasCode(c CaseResult, code FailureCode) bool {
	for _, f := range c.Failures {
		if f == code {
			return true
		}
	}
	return false
}
