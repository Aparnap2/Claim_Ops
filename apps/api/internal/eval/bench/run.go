package bench

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"claimops-api/internal/eval/corpus"
	"claimops-api/internal/parser"
)

// Run executes the deterministic end-to-end benchmark slice for issue #31:
//
//	corpus -> loader -> parser.Parser -> ScoreCase -> Report + benchmark.json.
//
// There is no LLM, no network, and no value echoing anywhere on this path.
// Cases run sequentially in manifest order so output order is deterministic.
//
// Behavior contract (decisions encoded):
//   - VerifyCorpus runs FIRST (fail fast on a tampered corpus).
//   - An INVALID golden aborts the whole run with a hard error carrying the
//     case id. A golden is ground truth authored alongside the corpus; an
//     invalid one is a CORPUS bug, not a parser verdict, so recording it as
//     PARSE_FAILURE would launder bad fixtures into parser signal. Fail
//     loud, fix the corpus. (An unreadable expected.json aborts the same
//     way for the same reason.)
//   - A LoadTrustedDocument rejection is recorded as FailParseFailure. The
//     loader is the harness trust boundary (sha check, then admission): a
//     rejection means the fixture violates that boundary, i.e. the corpus
//     bug class, surfaced here as a parse-level failure so the report still
//     attributes the case instead of aborting sibling cases.
//   - A ParsedDocument that fails artifact.Validate() is recorded as
//     FailPageStructure. The canonical schema is owned by ClaimOps, so an
//     adapter emitting an invalid artifact is a MAPPING bug class, and page
//     structure is the taxonomy code closest to "not a valid canonical
//     artifact". (#32 triages root cause; #31 only classifies.)
//   - LatencyMs (wall ms, monotonic clock) and InputBytes (len of the
//     trusted content captured BEFORE Parse consumes it) are set on every
//     path that reaches the parser, including parse/artifact failures.
//     ScoreCase itself returns LatencyMs=0, InputBytes=0 per the sibling
//     contract; Run fills them from measurement.
//
// CLI note: this slice is library-first by design. No cmd binary is
// included here; wiring an adapter into Run (e.g. a liteparse main) belongs
// to #32 or a follow-up.
func Run(ctx context.Context, fixturesRoot string, p parser.Parser, tenant, claimPrefix string) (Report, error) {
	if err := corpus.VerifyCorpus(fixturesRoot); err != nil {
		return Report{}, fmt.Errorf("bench: verify corpus: %w", err)
	}
	m, err := corpus.LoadManifest(fixturesRoot)
	if err != nil {
		return Report{}, fmt.Errorf("bench: load manifest: %w", err)
	}
	rep := Report{
		Meta: RunMeta{
			CorpusVersion: m.Version,
			CorpusCases:   len(m.Cases),
			ParserName:    p.Name(),
			ParserVersion: p.Version(),
			AdapterName:   p.Name(),
		},
		Cases:        make([]CaseResult, 0, len(m.Cases)),
		ByDifficulty: map[string]Aggregate{},
		ByType:       map[string]Aggregate{},
	}
	for _, c := range m.Cases {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		cr, err := runCase(ctx, c, p, tenant, claimPrefix+"-"+c.ID)
		if err != nil {
			return Report{}, err
		}
		rep.Cases = append(rep.Cases, cr)
	}
	aggregate(&rep)
	return rep, nil
}

// runCase executes one manifest case. A non-nil error is always a fatal
// corpus bug (unreadable or invalid golden) that aborts the run; every
// parser/harness-level outcome is returned as a CaseResult.
func runCase(ctx context.Context, c corpus.Case, p parser.Parser, tenant, claimID string) (CaseResult, error) {
	base := CaseResult{
		CaseID:       c.ID,
		DocumentType: c.DocumentType,
		Difficulty:   c.Difficulty,
	}
	golden, err := corpus.LoadGolden(c.Dir())
	if err != nil {
		return CaseResult{}, fmt.Errorf("bench: case %s: load golden: %w", c.ID, err)
	}
	if err := golden.Validate(c.DocumentType); err != nil {
		return CaseResult{}, fmt.Errorf("bench: case %s: invalid golden: %w", c.ID, err)
	}
	doc, err := corpus.LoadTrustedDocument(c, tenant, claimID)
	if err != nil {
		base.Failures = classifyLoaderError(err)
		return base, nil
	}
	inputBytes := int64(len(doc.Content))
	start := time.Now()
	artifact, err := p.Parse(ctx, doc)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		base.LatencyMs = latency
		base.InputBytes = inputBytes
		base.Failures = classifyParseError(err)
		return base, nil
	}
	if err := artifact.Validate(); err != nil {
		base.LatencyMs = latency
		base.InputBytes = inputBytes
		base.Failures = []FailureCode{FailPageStructure}
		return base, nil
	}
	scored := ScoreCase(c.ID, c.DocumentType, c.Difficulty, artifact, golden)
	scored.CaseID = c.ID
	scored.DocumentType = c.DocumentType
	scored.Difficulty = c.Difficulty
	scored.LatencyMs = latency
	scored.InputBytes = inputBytes
	return scored, nil
}

// classifyLoaderError maps a LoadTrustedDocument rejection onto the failure
// taxonomy. Every loader error — sha mismatch, admission rejection,
// oversized/empty content, bad extension, blank ids — becomes
// FailParseFailure: the fixture violated the harness trust boundary, which
// is the corpus bug class recorded at parse level so the case stays
// attributable in the report.
func classifyLoaderError(err error) []FailureCode {
	_ = err
	return []FailureCode{FailParseFailure}
}

// classifyParseError maps a Parser.Parse error onto the taxonomy. Only the
// sentinel ErrUnsupportedMediaType gets its own code; everything else
// (including wrapped ErrParseFailure) is FailParseFailure.
func classifyParseError(err error) []FailureCode {
	if errors.Is(err, parser.ErrUnsupportedMediaType) {
		return []FailureCode{FailUnsupportedMedia}
	}
	return []FailureCode{FailParseFailure}
}

// aggregate fills Overall, ByDifficulty, ByType and WorstCases from Cases.
func aggregate(rep *Report) {
	rep.Overall = summarize(rep.Cases)
	rep.ByDifficulty = map[string]Aggregate{}
	rep.ByType = map[string]Aggregate{}
	for _, cr := range rep.Cases {
		rep.ByDifficulty[cr.Difficulty] = summarize(filterBy(rep.Cases, func(o CaseResult) bool {
			return o.Difficulty == cr.Difficulty
		}))
		rep.ByType[cr.DocumentType] = summarize(filterBy(rep.Cases, func(o CaseResult) bool {
			return o.DocumentType == cr.DocumentType
		}))
	}
	rep.WorstCases = worstCases(rep.Cases, 8)
}

// filterBy returns the cases matching keep, preserving manifest order.
func filterBy(cases []CaseResult, keep func(CaseResult) bool) []CaseResult {
	out := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// summarize folds a case set into counts only — no values, no averages that
// hide tails. Table buckets follow the TableScore verdict strings; cases
// that never reached scoring (empty verdict) count in no table bucket.
func summarize(cases []CaseResult) Aggregate {
	agg := Aggregate{Cases: len(cases), Failures: map[string]int{}}
	for _, cr := range cases {
		if cr.ParseOK {
			agg.ParseOK++
		}
		for _, f := range cr.Fields {
			switch f.Match {
			case MatchExact:
				agg.FieldsExact++
			case MatchNormalized:
				agg.FieldsNormalized++
			case MatchMissing:
				agg.FieldsMissing++
			case MatchIncorrect:
				agg.FieldsIncorrect++
			}
		}
		switch cr.Tables.Verdict {
		case "TABLE_PASS":
			agg.TablesPass++
		case "TABLE_PARTIAL":
			agg.TablesPartial++
		case "TABLE_MISSED":
			agg.TablesMissed++
		}
		for _, code := range cr.Failures {
			agg.Failures[string(code)]++
		}
	}
	return agg
}

// worstCases returns up to n case IDs ordered worst-first: fields
// missing+incorrect desc, then tables missed desc, then parse failures
// (!ParseOK) desc, with a deterministic tiebreak on case id ascending.
func worstCases(cases []CaseResult, n int) []string {
	type scored struct {
		id       string
		badField int
		tblMiss  int
		parseBad int
	}
	rows := make([]scored, 0, len(cases))
	for _, cr := range cases {
		s := scored{id: cr.CaseID}
		for _, f := range cr.Fields {
			if f.Match == MatchMissing || f.Match == MatchIncorrect {
				s.badField++
			}
		}
		if cr.Tables.Verdict == "TABLE_MISSED" {
			s.tblMiss = 1
		}
		if !cr.ParseOK {
			s.parseBad = 1
		}
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].badField != rows[j].badField {
			return rows[i].badField > rows[j].badField
		}
		if rows[i].tblMiss != rows[j].tblMiss {
			return rows[i].tblMiss > rows[j].tblMiss
		}
		if rows[i].parseBad != rows[j].parseBad {
			return rows[i].parseBad > rows[j].parseBad
		}
		return rows[i].id < rows[j].id
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.id)
	}
	return out
}
