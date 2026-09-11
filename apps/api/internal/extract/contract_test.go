// In-package conformance tests: the reject path asserts on the pure
// checkConformance core (a fabricating stub must yield violations)
// without failing the suite, which the public RunConformance(t, ...)
// entry cannot do. The accept path is covered here and again through the
// public entry in extract_test.go. No network, no I/O, no clock reads:
// the fixture artifact is hand-built and deterministic.
package extract

import (
	"context"
	"strings"
	"testing"

	"claimops-api/internal/parser"
)

// stubExtractor is the in-package fake proving the runner accepts a
// conforming extractor and rejects broken ones (real extractors land in
// later issues; this package is contract-only per #44).
type stubExtractor struct {
	name    string
	version string
	facts   DocumentFacts
	err     error
}

func (s stubExtractor) Name() string    { return s.name }
func (s stubExtractor) Version() string { return s.version }
func (s stubExtractor) Extract(ctx context.Context, doc parser.ParsedDocument) (DocumentFacts, error) {
	if s.err != nil {
		return DocumentFacts{}, s.err
	}
	if err := ctx.Err(); err != nil {
		return DocumentFacts{}, err
	}
	return s.facts, nil
}

// conformanceFixture returns a hand-built canonical artifact covering all
// four statuses: claim_number/patient_name/admission_date present,
// discharge_date present-but-unparseable, policy_number absent, and two
// conflicting totals.
func conformanceFixture() parser.ParsedDocument {
	const docID = "doc-conform-1"
	blk := func(id, text string) parser.ContentBlock {
		return parser.ContentBlock{
			ID:         id,
			Type:       parser.BlockText,
			Text:       text,
			Evidence:   parser.EvidenceLocation{DocumentID: docID, Page: 1, BlockID: id},
			Confidence: 0.9,
		}
	}
	return parser.ParsedDocument{
		DocumentID: docID,
		Pages: []parser.ParsedPage{{Number: 1, Blocks: []parser.ContentBlock{
			blk("b1", "Claim Number: CLM-1001"),
			blk("b2", "Patient Name: Aparna Pradhan"),
			blk("b3", "Admission Date: 2026-01-05"),
			blk("b4", "Discharge Date: not yet known"),
			blk("b5", "Total Bill: Rs. 1,45,465.00"),
			blk("b6", "Net Payable: Rs. 1,45,400.00"),
		}}},
		Metadata: parser.DocumentMetadata{
			ParserName:    "stub-parser",
			ParserVersion: "0.0.1",
			SourceSHA256:  "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a8",
			SourceMedia:   "text/plain",
			PageCount:     1,
		},
	}
}

// conformingFacts returns grounded facts over conformanceFixture: three
// PRESENT, one AMBIGUOUS (raw preserved), one MISSING (carries nothing),
// one MULTI_CANDIDATE (no top-level value, two pinned candidates).
func conformingFacts() DocumentFacts {
	const docID = "doc-conform-1"
	ref := func(block string) EvidenceRef { return EvidenceRef{DocumentID: docID, Page: 1, BlockID: block} }
	field := func(key, value, normalized string, ev EvidenceRef, status FieldStatus) ExtractedField {
		return ExtractedField{
			Key: key, Value: value, Normalized: normalized,
			Evidence: ev, Status: status,
			Extractor: "stub-extract", ExtractorVersion: "0.0.1",
		}
	}
	return DocumentFacts{
		DocumentID: docID,
		DocType:    "CLAIM_FORM",
		Fields: map[string]ExtractedField{
			"claim_number":   field("claim_number", "CLM-1001", "CLM-1001", ref("b1"), StatusPresent),
			"patient_name":   field("patient_name", "Aparna Pradhan", "aparna pradhan", ref("b2"), StatusPresent),
			"admission_date": field("admission_date", "2026-01-05", "2026-01-05", ref("b3"), StatusPresent),
			"discharge_date": field("discharge_date", "not yet known", "", ref("b4"), StatusAmbiguous),
			"policy_number":  field("policy_number", "", "", EvidenceRef{}, StatusMissing),
			"total_bill": {
				Key: "total_bill", Status: StatusMultiCandidate,
				Candidates: []Candidate{
					{Value: "Rs. 1,45,465.00", Normalized: "14546500", Evidence: ref("b5")},
					{Value: "Rs. 1,45,400.00", Normalized: "14540000", Evidence: ref("b6")},
				},
				Extractor: "stub-extract", ExtractorVersion: "0.0.1",
			},
		},
	}
}

func TestFixtureValid(t *testing.T) {
	if err := conformanceFixture().Validate(); err != nil {
		t.Fatalf("hand-built fixture invalid: %v", err)
	}
}

func TestCheckConformanceAccepts(t *testing.T) {
	fix := conformanceFixture()
	stub := stubExtractor{name: "stub-extract", version: "0.0.1", facts: conformingFacts()}
	if violations := checkConformance(stub, fix); len(violations) != 0 {
		t.Fatalf("conformant stub rejected: %v", violations)
	}
}

func TestCheckConformanceRejects(t *testing.T) {
	fix := conformanceFixture()
	good := conformingFacts()
	ref := EvidenceRef{DocumentID: fix.DocumentID, Page: 1, BlockID: "b1"}

	mutate := func(f DocumentFacts, key string, set func(*ExtractedField)) DocumentFacts {
		clone := DocumentFacts{
			DocumentID: f.DocumentID,
			DocType:    f.DocType,
			Fields:     make(map[string]ExtractedField, len(f.Fields)),
		}
		for k, v := range f.Fields {
			clone.Fields[k] = v
		}
		fld := clone.Fields[key]
		set(&fld)
		clone.Fields[key] = fld
		return clone
	}

	cases := []struct {
		name  string
		facts DocumentFacts
		want  string
	}{
		{
			name:  "fabricated value not in artifact",
			facts: mutate(good, "claim_number", func(f *ExtractedField) { f.Value = "CLM-99999" }),
			want:  "not grounded",
		},
		{
			name:  "folded fabrication still caught",
			facts: mutate(good, "patient_name", func(f *ExtractedField) { f.Value = "Nobody Here" }),
			want:  "not grounded",
		},
		{
			name:  "empty-string present",
			facts: mutate(good, "claim_number", func(f *ExtractedField) { f.Value = "  " }),
			want:  "blank value",
		},
		{
			name:  "missing carries value",
			facts: mutate(good, "policy_number", func(f *ExtractedField) { f.Value = "POL-1" }),
			want:  "MISSING carries a value",
		},
		{
			name: "missing with evidence pin",
			facts: mutate(good, "policy_number", func(f *ExtractedField) {
				f.Evidence = ref
			}),
			want: "MISSING with evidence",
		},
		{
			name:  "ambiguous drops raw",
			facts: mutate(good, "discharge_date", func(f *ExtractedField) { f.Value = "" }),
			want:  "raw must be preserved",
		},
		{
			name:  "ambiguous smuggles normalized",
			facts: mutate(good, "discharge_date", func(f *ExtractedField) { f.Normalized = "2026-01-09" }),
			want:  "normalize to nothing",
		},
		{
			name:  "silent winner on multi",
			facts: mutate(good, "total_bill", func(f *ExtractedField) { f.Value = "Rs. 1,45,465.00" }),
			want:  "silent winner",
		},
		{
			name: "single candidate multi",
			facts: mutate(good, "total_bill", func(f *ExtractedField) {
				f.Candidates = f.Candidates[:1]
			}),
			want: "want >= 2",
		},
		{
			name:  "present without page provenance",
			facts: mutate(good, "claim_number", func(f *ExtractedField) { f.Evidence.Page = 0 }),
			want:  "want >= 1",
		},
		{
			name:  "wrong extractor identity",
			facts: mutate(good, "claim_number", func(f *ExtractedField) { f.Extractor = "other" }),
			want:  "Extractor =",
		},
		{
			name:  "unknown status",
			facts: mutate(good, "claim_number", func(f *ExtractedField) { f.Status = "MAYBE" }),
			want:  "unknown status",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := stubExtractor{name: "stub-extract", version: "0.0.1", facts: tc.facts}
			violations := checkConformance(stub, fix)
			if len(violations) == 0 {
				t.Fatalf("breaking stub accepted, want rejection containing %q", tc.want)
			}
			joined := strings.Join(violations, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("violations %v do not mention %q", violations, tc.want)
			}
		})
	}
}

func TestCheckConformanceRejectsBadIdentity(t *testing.T) {
	fix := conformanceFixture()
	stub := stubExtractor{name: "  ", version: "0.0.1", facts: conformingFacts()}
	if violations := checkConformance(stub, fix); len(violations) == 0 {
		t.Fatal("blank extractor Name accepted, want rejection")
	}
}
