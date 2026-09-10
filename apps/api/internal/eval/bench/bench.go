// Package bench is the deterministic parser benchmark harness (#31).
//
// It consumes ONLY the public application-owned contract
// (parser.Parser -> parser.ParsedDocument) plus corpus goldens, so any
// future adapter (GCP, Docling, OCR-escalated) can be scored without
// changing the scorer. No LLM, no network, no vendor types.
//
// Determinism rule: same corpus version + parser version + configuration
// -> same scores, taxonomy, and failure sets. Wall-clock latency is
// MEASURED but EXCLUDED from repeatability: the repeatability test
// compares everything except latency fields.
//
// PII rule: benchmark output carries field KEYS, match classes, counts,
// and taxonomy codes — never field VALUES (goldens hold synthetic
// identities; scores must not echo them into CI artifacts).
//
// Confidence rule: canonical Confidence 1.0 from a vendor-silent source
// is NOT evidence of certainty. The scorer NEVER rewards confidence
// values; confidence is recorded as present/absent only, and the report
// must state that vendor-silent 1.0 is uncalibrated.
package bench

// FailureCode is the benchmark failure taxonomy. A benchmark failure is
// NOT automatically a parser bug: PARSE_FAILURE on a corrupted fixture
// is correct behavior; TABLE_MISSED may be a parser limit, a mapping
// loss, or a golden/contract gap — #32 triages, #31 only classifies.
type FailureCode string

const (
	// Parse-level outcomes.
	FailParseFailure     FailureCode = "PARSE_FAILURE"
	FailUnsupportedMedia FailureCode = "UNSUPPORTED_MEDIA"
	FailEmptyArtifact    FailureCode = "EMPTY_ARTIFACT"
	FailMissingText      FailureCode = "MISSING_TEXT"
	FailTextMismatch     FailureCode = "TEXT_CONTENT_MISMATCH"
	FailPageStructure    FailureCode = "PAGE_STRUCTURE_MISMATCH"
	FailReadingOrder     FailureCode = "READING_ORDER_MISMATCH"
	// Field-level outcomes.
	FailFieldMissing   FailureCode = "FIELD_MISSING"
	FailFieldIncorrect FailureCode = "FIELD_INCORRECT"
	// Table-level outcomes.
	FailTableMissing   FailureCode = "TABLE_MISSING"
	FailTablePartial   FailureCode = "TABLE_PARTIAL"
	FailTableStructure FailureCode = "TABLE_STRUCTURE_MISMATCH"
	// Provenance outcomes.
	FailProvenanceMissing   FailureCode = "PROVENANCE_MISSING"
	FailProvenanceIncorrect FailureCode = "PROVENANCE_INCORRECT"
)

// MatchClass is a field-level verdict. Values are never echoed.
type MatchClass string

const (
	MatchExact      MatchClass = "exact_match"
	MatchNormalized MatchClass = "normalized_match"
	MatchMissing    MatchClass = "missing"
	MatchIncorrect  MatchClass = "incorrect"
)

// FieldScore scores one expected field key.
type FieldScore struct {
	Key   string     `json:"key"`
	Match MatchClass `json:"match"`
	// HasProvenance reports whether the matched hit carried a page +
	// block id (box presence tracked separately in ProvenanceScore).
	HasProvenance bool `json:"has_provenance"`
}

// TableScore scores table reconstruction for one case. Expected tables
// come from the golden (expected_tables + line_items); detected tables
// from the artifact. A bill flattened to paragraphs is TABLE_MISSED,
// never silently equivalent to a pass.
type TableScore struct {
	ExpectedTables int    `json:"expected_tables"`
	DetectedTables int    `json:"detected_tables"`
	RowsMatched    int    `json:"rows_matched"`
	CellsMatched   int    `json:"cells_matched"`
	Verdict        string `json:"verdict"` // TABLE_PASS, TABLE_PARTIAL, TABLE_MISSED
}

// ProvenanceScore aggregates provenance availability over matched hits.
type ProvenanceScore struct {
	HitsTotal     int `json:"hits_total"`
	HitsWithPage  int `json:"hits_with_page"`
	HitsWithBlock int `json:"hits_with_block"`
	HitsWithBox   int `json:"hits_with_box"`
}

// CaseResult is the per-case machine-readable record.
type CaseResult struct {
	CaseID        string          `json:"case_id"`
	DocumentType  string          `json:"document_type"`
	Difficulty    string          `json:"difficulty"`
	ParseOK       bool            `json:"parse_ok"`
	ArtifactValid bool            `json:"artifact_valid"`
	Fields        []FieldScore    `json:"fields"`
	Tables        TableScore      `json:"tables"`
	Provenance    ProvenanceScore `json:"provenance"`
	Failures      []FailureCode   `json:"failures"`
	// Latency excluded from repeatability comparison.
	LatencyMs  int64 `json:"latency_ms"`
	InputBytes int64 `json:"input_bytes"`
}

// RunMeta fingerprints the benchmark conditions for reproducibility.
type RunMeta struct {
	CorpusVersion string `json:"corpus_version"`
	CorpusCases   int    `json:"corpus_cases"`
	ParserName    string `json:"parser_name"`
	ParserVersion string `json:"parser_version"`
	AdapterName   string `json:"adapter_name"`
}

// Report is the top-level machine-readable CI artifact.
type Report struct {
	Meta         RunMeta              `json:"meta"`
	Cases        []CaseResult         `json:"cases"`
	ByDifficulty map[string]Aggregate `json:"by_difficulty"`
	ByType       map[string]Aggregate `json:"by_type"`
	Overall      Aggregate            `json:"overall"`
	WorstCases   []string             `json:"worst_cases"`
}

// Aggregate holds counts only — no values, no averages that hide tails.
type Aggregate struct {
	Cases            int            `json:"cases"`
	ParseOK          int            `json:"parse_ok"`
	FieldsExact      int            `json:"fields_exact"`
	FieldsNormalized int            `json:"fields_normalized"`
	FieldsMissing    int            `json:"fields_missing"`
	FieldsIncorrect  int            `json:"fields_incorrect"`
	TablesPass       int            `json:"tables_pass"`
	TablesPartial    int            `json:"tables_partial"`
	TablesMissed     int            `json:"tables_missed"`
	Failures         map[string]int `json:"failures"`
}

// RepeatKey strips latency for the repeatability comparison.
func (r CaseResult) RepeatKey() CaseResult {
	r.LatencyMs = 0
	return r
}
