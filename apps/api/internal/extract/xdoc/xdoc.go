// Package xdoc implements the deterministic document-type extractors for
// ClaimOps (issue #45) on top of the #44 extraction contract.
//
// Package placement (why a subpackage, not per-type files in package
// extract): extract.go/contract.go are contract-only and frozen by the #45
// constraints — real extractors must land without touching them. A dedicated
// subpackage keeps the contract pristine and mirrors the established
// parser/liteparse adapter split (vendor/parser-type isolation via package
// boundaries). xdoc imports stdlib plus the parser and extract packages
// only: no LLM, no I/O, no clock reads, no GCP/vendor/storage imports. All
// functions are pure over the canonical artifact.
//
// Phase 0 architecture-compatibility record (#45 gate): options
// considered were (a) per-type files in package extract, (b) this
// subpackage, (c) a single table-driven file, (d) reusing Tier-1
// documents.ExtractFields. (a) was rejected: it would co-mingle
// implementation with the frozen contract files. (c) was rejected:
// per-type key sets and table handling differ enough that one table
// would obscure the rules. (d) was rejected: Tier-1 is regex over raw
// text without evidence association or status semantics. (b) was
// chosen: contract files untouched, package boundary mirrors
// parser/liteparse, engine shared via run(). Compatibility verdict:
// the Extractor interface consumes only parser.ParsedDocument, so
// extraction is parser-agnostic (LiteParse, Go PDF, Document AI
// fallback all converge on the same artifact); no cloud, vendor,
// storage, or agent dependencies exist in this package. Proceeded
// without redesigning the architecture.
package xdoc

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"claimops-api/internal/extract"
	"claimops-api/internal/parser"
)

// Version is the shared extractor version recorded on every ExtractedField.
// It must change whenever extraction behavior changes (per the Extractor
// contract: Version documents reproducibility).
const Version = "1.0.0"

// Document types emitted in DocumentFacts.DocType. The first three match the
// verify package constants (verify.DocClaimForm and kin); the remaining
// three are new extraction-side types the #47 mapper will consume. They are
// declared here — not in verify — because the #45 constraints forbid
// modifying verify/.
const (
	DocClaimForm        = "CLAIM_FORM"
	DocDischargeSummary = "DISCHARGE_SUMMARY"
	DocHospitalBill     = "HOSPITAL_BILL"
	DocPolicySchedule   = "POLICY_SCHEDULE"
	DocPreauthForm      = "PREAUTH_FORM"
	DocLabReport        = "LAB_REPORT"
)

// Canonical field keys. Extractors emit EXACTLY these names (plus the
// bill_lines_N family for line items): no synonyms, no renames. The mapping
// note in contract.go uses the older "total_bill" illustration; the binding
// key for this issue is total_amount_paise.
const (
	KeyClaimNumber      = "claim_number"
	KeyPolicyNumber     = "policy_number"
	KeyPatientName      = "patient_name"
	KeyHospitalName     = "hospital_name"
	KeyAdmissionDate    = "admission_date"
	KeyDischargeDate    = "discharge_date"
	KeyTotalAmountPaise = "total_amount_paise"
	KeyBillLines        = "bill_lines"
	KeyDiagnosis        = "diagnosis"
	KeyProcedure        = "procedure"
)

// billLineKey returns the Nth line-item key of the bill_lines[] family
// (zero-based, table reading order).
func billLineKey(i int) string {
	return KeyBillLines + "_" + strconv.Itoa(i)
}

// fieldKind selects the normalizer for a key.
type fieldKind uint8

const (
	kindID fieldKind = iota
	kindName
	kindDate
	kindAmount
)

// keyKind binds every scalar key to its normalizer kind. Bill line items
// have no entry: descriptions normalize by trimming (NormalizeID).
var keyKind = map[string]fieldKind{
	KeyClaimNumber:      kindID,
	KeyPolicyNumber:     kindID,
	KeyPatientName:      kindName,
	KeyHospitalName:     kindName,
	KeyAdmissionDate:    kindDate,
	KeyDischargeDate:    kindDate,
	KeyTotalAmountPaise: kindAmount,
	KeyDiagnosis:        kindName,
	KeyProcedure:        kindName,
}

// labelEntry is one known field label anchored to a canonical key.
type labelEntry struct {
	key   string
	alias string
}

// labelEntries is the known-label registry. Matching is line-anchored
// (^alias SEPARATOR value) and case-insensitive; the value must follow on
// the same line or on the next unlabeled line of the same block. Lines
// without an explicit separator never match: free prose is MISSING, never
// a guessed PRESENT.
var labelEntries = []labelEntry{
	{KeyClaimNumber, "claim number"},
	{KeyClaimNumber, "claim no."},
	{KeyClaimNumber, "claim no"},
	{KeyClaimNumber, "claim #"},
	{KeyClaimNumber, "claim id"},
	{KeyPolicyNumber, "policy number"},
	{KeyPolicyNumber, "policy no."},
	{KeyPolicyNumber, "policy no"},
	{KeyPolicyNumber, "policy #"},
	{KeyPolicyNumber, "policy id"},
	{KeyPatientName, "name of patient"},
	{KeyPatientName, "patient name"},
	{KeyPatientName, "name of insured"},
	{KeyPatientName, "insured name"},
	{KeyPatientName, "policy holder"},
	{KeyPatientName, "holder name"},
	{KeyPatientName, "patient"},
	{KeyHospitalName, "name of hospital"},
	{KeyHospitalName, "treating hospital"},
	{KeyHospitalName, "hospital name"},
	{KeyHospitalName, "provider name"},
	{KeyHospitalName, "facility name"},
	{KeyHospitalName, "hospital"},
	{KeyHospitalName, "facility"},
	{KeyAdmissionDate, "date of admission"},
	{KeyAdmissionDate, "admission date"},
	{KeyAdmissionDate, "date admitted"},
	{KeyAdmissionDate, "admitted on"},
	{KeyAdmissionDate, "doa"},
	{KeyDischargeDate, "date of discharge"},
	{KeyDischargeDate, "discharge date"},
	{KeyDischargeDate, "date discharged"},
	{KeyDischargeDate, "discharged on"},
	{KeyDischargeDate, "dod"},
	{KeyTotalAmountPaise, "total amount payable"},
	{KeyTotalAmountPaise, "net amount payable"},
	{KeyTotalAmountPaise, "estimated amount"},
	{KeyTotalAmountPaise, "expected amount"},
	{KeyTotalAmountPaise, "claimed amount"},
	{KeyTotalAmountPaise, "amount claimed"},
	{KeyTotalAmountPaise, "amount payable"},
	{KeyTotalAmountPaise, "total amount"},
	{KeyTotalAmountPaise, "grand total"},
	{KeyTotalAmountPaise, "bill amount"},
	{KeyTotalAmountPaise, "net amount"},
	{KeyTotalAmountPaise, "net payable"},
	{KeyTotalAmountPaise, "total bill"},
	{KeyTotalAmountPaise, "total"},
	{KeyDiagnosis, "provisional diagnosis"},
	{KeyDiagnosis, "clinical diagnosis"},
	{KeyDiagnosis, "primary diagnosis"},
	{KeyDiagnosis, "final diagnosis"},
	{KeyDiagnosis, "diagnosed with"},
	{KeyDiagnosis, "impression"},
	{KeyDiagnosis, "diagnosis"},
	{KeyDiagnosis, "ailment"},
	{KeyDiagnosis, "disease"},
	{KeyProcedure, "procedure performed"},
	{KeyProcedure, "treatment given"},
	{KeyProcedure, "procedure"},
	{KeyProcedure, "treatment"},
	{KeyProcedure, "surgery"},
	{KeyProcedure, "operation"},
	{KeyProcedure, "intervention"},
}

// keyOrder is the deterministic match priority across keys. Aliases are
// disjoint in practice, but a fixed order keeps multi-key lines (none
// expected) reproducible.
var keyOrder = []string{
	KeyClaimNumber,
	KeyPolicyNumber,
	KeyPatientName,
	KeyHospitalName,
	KeyAdmissionDate,
	KeyDischargeDate,
	KeyTotalAmountPaise,
	KeyDiagnosis,
	KeyProcedure,
}

// keyPatterns compiles one anchored pattern per key. Aliases sort
// longest-first so "patient name" wins over "patient" and "total amount
// payable" wins over "total amount" on the same line.
var keyPatterns = func() map[string]*regexp.Regexp {
	grouped := make(map[string][]string, len(keyOrder))
	for _, e := range labelEntries {
		grouped[e.key] = append(grouped[e.key], e.alias)
	}
	out := make(map[string]*regexp.Regexp, len(grouped))
	for key, aliases := range grouped {
		sort.SliceStable(aliases, func(i, j int) bool {
			if len(aliases[i]) != len(aliases[j]) {
				return len(aliases[i]) > len(aliases[j])
			}
			return aliases[i] < aliases[j]
		})
		alts := make([]string, len(aliases))
		for i, a := range aliases {
			alts[i] = regexp.QuoteMeta(a)
		}
		out[key] = regexp.MustCompile(`(?i)^\s*(?:` + strings.Join(alts, "|") + `)\s*[:=\-]\s*(.*)$`)
	}
	return out
}()

// matchLine matches one text line against the global label registry,
// returning the key and trimmed raw value. labeled is true even when the
// value is blank (label present, value on the next line or absent).
func matchLine(line string) (key, raw string, labeled bool) {
	for _, k := range keyOrder {
		if m := keyPatterns[k].FindStringSubmatch(line); m != nil {
			return k, strings.TrimSpace(m[1]), true
		}
	}
	return "", "", false
}

// nextUnlabeledValue returns the value on one of the two physical lines
// immediately after a blank-valued label (the adjacent line, or one blank
// line then the value) unless that line is itself labeled (never steal
// another field's label). The two-line window is deliberate: anything
// further is not a continuation, and letting distant boilerplate prose
// become a PRESENT value in coarse multi-line blocks would be
// fabrication-adjacent.
func nextUnlabeledValue(lines []string, from int) string {
	end := from + 2
	if end > len(lines) {
		end = len(lines)
	}
	for _, ln := range lines[from:end] {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if _, _, labeled := matchLine(t); labeled {
			return ""
		}
		return t
	}
	return ""
}

// spec declares one document-type extractor: its DocType, its recorded
// extractor name, the scalar keys it observes, and whether it reads bill
// line items from canonical tables.
type spec struct {
	docType   string
	name      string
	keys      []string
	billLines bool
}

// hit is one raw label-anchored observation before status resolution.
type hit struct {
	raw string
	ev  extract.EvidenceRef
}

// run is the shared deterministic engine behind all six extractors: scan
// canonical blocks for known labels, dedupe exact repeats (agreement, not
// conflict), resolve statuses per the #44 contract, and optionally read
// bill lines from canonical tables.
func run(ctx context.Context, doc parser.ParsedDocument, s spec) (extract.DocumentFacts, error) {
	if err := ctx.Err(); err != nil {
		return extract.DocumentFacts{}, err
	}
	declared := make(map[string]bool, len(s.keys))
	for _, k := range s.keys {
		declared[k] = true
	}
	hits := make(map[string][]hit)
	for _, page := range doc.Pages {
		for _, blk := range page.Blocks {
			lines := strings.Split(blk.Text, "\n")
			for i, line := range lines {
				key, raw, labeled := matchLine(line)
				if !labeled || !declared[key] {
					continue
				}
				if raw == "" {
					raw = nextUnlabeledValue(lines, i+1)
					if raw == "" {
						continue
					}
				}
				dup := false
				for _, h := range hits[key] {
					if h.raw == raw {
						dup = true
						break
					}
				}
				if !dup {
					hits[key] = append(hits[key], hit{raw: raw, ev: extract.RefFrom(blk.Evidence)})
				}
			}
		}
	}
	fields := make(map[string]extract.ExtractedField, len(s.keys)+4)
	for _, key := range s.keys {
		fields[key] = assemble(key, hits[key], s.name)
	}
	if s.billLines {
		for k, f := range billLineFields(doc, s.name) {
			fields[k] = f
		}
	}
	return extract.DocumentFacts{
		DocumentID: doc.DocumentID,
		DocType:    s.docType,
		Fields:     fields,
	}, nil
}

// normalize maps a raw observation to its canonical form. ok=false means
// present-but-unparseable (caller emits AMBIGUOUS with the raw preserved).
// ID/name kinds always normalize (trim / casefold-collapse); date/amount
// kinds reject anything outside their strict grammars.
func normalize(key, raw string) (norm string, ok bool) {
	switch keyKind[key] {
	case kindID:
		return extract.NormalizeID(raw), true
	case kindName:
		return extract.NormalizeName(raw), true
	case kindDate:
		n, err := extract.NormalizeDate(raw)
		if err != nil {
			return "", false
		}
		return n, true
	case kindAmount:
		p, err := extract.NormalizePaise(raw)
		if err != nil {
			return "", false
		}
		return strconv.FormatInt(p, 10), true
	}
	return raw, true
}

// assemble resolves one key's hits to its observation: no hits -> MISSING
// (carries nothing); one hit -> PRESENT or AMBIGUOUS; conflicting hits ->
// MULTI_CANDIDATE with no top-level value (never a silent winner).
func assemble(key string, hs []hit, extractor string) extract.ExtractedField {
	base := extract.ExtractedField{
		Key:              key,
		Status:           extract.StatusMissing,
		Extractor:        extractor,
		ExtractorVersion: Version,
	}
	if len(hs) == 0 {
		return base
	}
	if len(hs) > 1 {
		cands := make([]extract.Candidate, 0, len(hs))
		for _, h := range hs {
			n, _ := normalize(key, h.raw)
			cands = append(cands, extract.Candidate{Value: h.raw, Normalized: n, Evidence: h.ev})
		}
		base.Status = extract.StatusMultiCandidate
		base.Candidates = cands
		return base
	}
	h := hs[0]
	if n, ok := normalize(key, h.raw); ok {
		base.Status = extract.StatusPresent
		base.Value = h.raw
		base.Normalized = n
		base.Evidence = h.ev
		return base
	}
	base.Status = extract.StatusAmbiguous
	base.Value = h.raw
	base.Evidence = h.ev
	return base
}

// billHeaderWords marks a first table row as a header (folded exact match).
// Any single hit skips the row; data rows that merely resemble headers are
// still protected by the amount-cell qualification below.
var billHeaderWords = map[string]bool{
	"s.no": true, "sno": true, "sr no": true, "sr.no": true, "#": true, "no": true,
	"description": true, "descriptions": true, "particular": true, "particulars": true,
	"item": true, "items": true, "service": true, "services": true,
	"charge": true, "charges": true, "qty": true, "quantity": true,
	"rate": true, "amount": true, "price": true, "days": true,
	"detail": true, "details": true,
}

// billTotalsWords marks a data row as a totals/summary row rather than a
// billable line item (folded exact match on the description cell).
var billTotalsWords = map[string]bool{
	"total": true, "grand total": true, "total amount": true, "net payable": true,
	"net amount": true, "amount payable": true, "balance payable": true,
	"amount due": true, "balance": true, "subtotal": true, "sub total": true,
}

// foldCell lowercases a cell and collapses whitespace for vocabulary checks.
// Parentheticals are kept verbatim: a header like "Amount (Rs.)" will not
// folded-match "amount", so header detection relies on the OTHER header
// words in the row plus the money-shape amount gate below (defense in
// depth, documented rather than widened — widening the vocab risks
// skipping real data rows).
func foldCell(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// billLineFields reads bill line items from canonical tables only. A row
// qualifies when it carries at least one parseable amount cell and a
// distinct non-amount description cell; the description is the first
// non-empty cell that is not itself an amount. Header first-rows and totals
// rows are skipped. With zero qualifying rows the extractor emits the bare
// KeyBillLines key as MISSING — line-like block text never becomes rows
// (that flattening gap is TABLE_MISSED territory, not extraction's to fix
// by hallucinating structure).
func billLineFields(doc parser.ParsedDocument, extractor string) map[string]extract.ExtractedField {
	type line struct {
		desc string
		page int
	}
	var lines []line
	for _, page := range doc.Pages {
		for _, tbl := range page.Tables {
			for ri, row := range tbl.Rows {
				if ri == 0 && isHeaderRow(row) {
					continue
				}
				desc, ok := rowDescription(row)
				if !ok {
					continue
				}
				if billTotalsWords[foldCell(desc)] {
					continue
				}
				lines = append(lines, line{desc: desc, page: tbl.Page})
			}
		}
	}
	out := make(map[string]extract.ExtractedField, len(lines)+1)
	mk := func(key string) extract.ExtractedField {
		return extract.ExtractedField{Key: key, Extractor: extractor, ExtractorVersion: Version}
	}
	if len(lines) == 0 {
		f := mk(KeyBillLines)
		f.Status = extract.StatusMissing
		out[KeyBillLines] = f
		return out
	}
	for i, ln := range lines {
		f := mk(billLineKey(i))
		f.Status = extract.StatusPresent
		f.Value = ln.desc
		f.Normalized = extract.NormalizeID(ln.desc)
		f.Evidence = extract.EvidenceRef{DocumentID: doc.DocumentID, Page: ln.page}
		out[f.Key] = f
	}
	return out
}

// isHeaderRow reports whether any cell of the row is a known header word.
func isHeaderRow(row parser.TableRow) bool {
	for _, c := range row.Cells {
		if billHeaderWords[foldCell(c.Text)] {
			return true
		}
	}
	return false
}

// hasMoneyShape reports whether a cell looks like a monetary amount
// rather than a serial number or code: it must contain '.', ',', '₹',
// or an rs/inr/paise token (case-insensitive). Bare integers ("1",
// "100") are valid NormalizePaise inputs but are serial/code-shaped,
// and treating them as amounts lets amount-less rows (e.g. ["4",
// "Stationery"]) fabricate bill lines. Tradeoff, documented: a table
// amount written as a bare integer will not qualify as a row amount
// here; such amounts still extract via the scalar label path.
func hasMoneyShape(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	folded := strings.ToLower(t)
	if strings.ContainsAny(folded, ".,₹") {
		return true
	}
	for _, tok := range []string{"rs", "inr", "paise", "rupees"} {
		if strings.Contains(folded, tok) {
			return true
		}
	}
	return false
}

// rowDescription returns the row's description cell and whether the row
// qualifies as a bill line: some cell must be a money-shaped amount
// (exact integer math via NormalizePaise) and a different non-empty cell
// must carry the description.
func rowDescription(row parser.TableRow) (string, bool) {
	hasAmount := false
	for _, c := range row.Cells {
		t := strings.TrimSpace(c.Text)
		if t == "" || !hasMoneyShape(t) {
			continue
		}
		if _, err := extract.NormalizePaise(t); err == nil {
			hasAmount = true
			break
		}
	}
	if !hasAmount {
		return "", false
	}
	for _, c := range row.Cells {
		t := strings.TrimSpace(c.Text)
		if t == "" {
			continue
		}
		// Any number-like cell (serials included) cannot be a
		// description; the amount gate above already required a
		// money-shaped amount elsewhere in the row.
		if _, err := extract.NormalizePaise(t); err == nil {
			continue
		}
		return t, true
	}
	return "", false
}
