package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// WriteJSON marshals the report with canonical encoding/json and 2-space
// indent. The schema (bench.go) carries no timestamps, and nothing here
// adds any: output is a pure function of corpus + parser + scores, so
// identical inputs produce byte-identical artifacts.
func WriteJSON(report Report, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// RepeatEqual reports whether two reports are repeat-equal: same Meta plus
// per-case RepeatKey() equality (latency zeroed). Latency is measured wall
// time and therefore EXCLUDED by design; everything else — scores,
// taxonomy, failure sets — must be identical for the determinism proof.
func RepeatEqual(a, b Report) bool {
	if a.Meta != b.Meta {
		return false
	}
	if len(a.Cases) != len(b.Cases) {
		return false
	}
	for i := range a.Cases {
		if !reflect.DeepEqual(a.Cases[i].RepeatKey(), b.Cases[i].RepeatKey()) {
			return false
		}
	}
	return reflect.DeepEqual(a.Overall, b.Overall) &&
		reflect.DeepEqual(a.ByDifficulty, b.ByDifficulty) &&
		reflect.DeepEqual(a.ByType, b.ByType) &&
		reflect.DeepEqual(a.WorstCases, b.WorstCases)
}

// Summarize renders a human one-pager: overall counts, a per-difficulty
// table (cases / parse-ok / fields exact+normalized / missing), top failure
// codes, and worst cases. Keys, counts and codes only — field VALUES never
// appear (CaseResult carries none by construction, and this function prints
// none of the golden-derived content beyond match-class counts).
func Summarize(r Report) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "benchmark report: corpus=%s cases=%d parser=%s %s (adapter=%s)\n",
		r.Meta.CorpusVersion, r.Meta.CorpusCases,
		r.Meta.ParserName, r.Meta.ParserVersion, r.Meta.AdapterName)
	o := r.Overall
	fmt.Fprintf(&sb, "overall: cases=%d parse_ok=%d fields exact=%d normalized=%d missing=%d incorrect=%d tables pass=%d partial=%d missed=%d\n",
		o.Cases, o.ParseOK, o.FieldsExact, o.FieldsNormalized,
		o.FieldsMissing, o.FieldsIncorrect,
		o.TablesPass, o.TablesPartial, o.TablesMissed)
	sb.WriteString("by_difficulty:\n")
	for _, d := range sortedKeys(r.ByDifficulty) {
		a := r.ByDifficulty[d]
		fmt.Fprintf(&sb, "  %s: cases=%d parse_ok=%d fields exact=%d normalized=%d missing=%d incorrect=%d\n",
			d, a.Cases, a.ParseOK, a.FieldsExact, a.FieldsNormalized,
			a.FieldsMissing, a.FieldsIncorrect)
	}
	sb.WriteString("top_failures:\n")
	for _, kv := range topCodes(o.Failures, 5) {
		fmt.Fprintf(&sb, "  %s: %d\n", kv.code, kv.count)
	}
	if len(r.WorstCases) > 0 {
		fmt.Fprintf(&sb, "worst_cases: %s\n", strings.Join(r.WorstCases, ", "))
	} else {
		sb.WriteString("worst_cases: (none)\n")
	}
	return sb.String()
}

// sortedKeys returns map keys in ascending order for deterministic output.
func sortedKeys(m map[string]Aggregate) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type codeCount struct {
	code  string
	count int
}

// topCodes returns up to n failure codes ordered by count desc, then code
// asc for determinism.
func topCodes(m map[string]int, n int) []codeCount {
	out := make([]codeCount, 0, len(m))
	for code, count := range m {
		out = append(out, codeCount{code: code, count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].code < out[j].code
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
