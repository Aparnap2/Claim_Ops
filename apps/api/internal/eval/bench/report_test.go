package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func sampleReport() Report {
	rep := Report{
		Meta: RunMeta{CorpusVersion: "vtest", CorpusCases: 2, ParserName: "stub", ParserVersion: "0.0-test", AdapterName: "stub"},
		Cases: []CaseResult{
			{CaseID: "CASE-001", DocumentType: "discharge_summary", Difficulty: "D0",
				ParseOK: true, ArtifactValid: true,
				Fields:     []FieldScore{{Key: "claim_number", Match: MatchExact, HasProvenance: true}},
				Tables:     TableScore{Verdict: "TABLE_PASS"},
				Provenance: ProvenanceScore{HitsTotal: 1, HitsWithPage: 1, HitsWithBlock: 1},
				LatencyMs:  7, InputBytes: 700},
			{CaseID: "CASE-002", DocumentType: "hospital_bill", Difficulty: "D1",
				ParseOK: false, Failures: []FailureCode{FailParseFailure},
				LatencyMs: 3, InputBytes: 720},
		},
		ByDifficulty: map[string]Aggregate{},
		ByType:       map[string]Aggregate{},
	}
	aggregate(&rep)
	return rep
}

func TestReportWriteJSONRoundTrip(t *testing.T) {
	want := sampleReport()
	var buf bytes.Buffer
	if err := WriteJSON(want, &buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if !strings.HasPrefix(buf.String(), "{\n  \"meta\"") {
		t.Fatalf("expected 2-space indent, got head: %q", buf.String()[:40])
	}
	var got Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestReportRepeatEqualIgnoresLatency(t *testing.T) {
	a := sampleReport()
	b := sampleReport()
	b.Cases[0].LatencyMs = 9999
	b.Cases[1].LatencyMs = 1234
	if !RepeatEqual(a, b) {
		t.Fatal("latency-only difference must be repeat-equal")
	}
	c := sampleReport()
	c.Cases[1].Failures = []FailureCode{FailUnsupportedMedia}
	if RepeatEqual(a, c) {
		t.Fatal("failure-code difference must NOT be repeat-equal")
	}
	d := sampleReport()
	d.Meta.ParserVersion = "9.9-other"
	if RepeatEqual(a, d) {
		t.Fatal("meta difference must NOT be repeat-equal")
	}
	e := sampleReport()
	e.Cases = e.Cases[:1]
	if RepeatEqual(a, e) {
		t.Fatal("case-count difference must NOT be repeat-equal")
	}
}

func TestReportSummarizeNoValues(t *testing.T) {
	// End-to-end no-echo proof: the golden carries a distinctive sentinel
	// VALUE; neither the marshalled benchmark.json nor the human summary
	// may contain it (output is keys/counts/codes only by construction).
	const sentinel = "ZZZ_SENTINEL"
	root, _ := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-001", docType: "discharge_summary", difficulty: "D0",
			golden: `{"claim_number":"CLM-1","patient_name":"` + sentinel + `"}`},
	})
	rep, err := Run(context.Background(), root, okStub(), testTenant, testClaimPrefix)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteJSON(rep, &buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if strings.Contains(buf.String(), sentinel) {
		t.Fatal("benchmark.json echoes a golden value")
	}
	if s := Summarize(rep); strings.Contains(s, sentinel) {
		t.Fatalf("summary echoes a golden value:\n%s", s)
	}
}

func TestReportSummarizeShape(t *testing.T) {
	s := Summarize(sampleReport())
	for _, want := range []string{
		"corpus=vtest", "cases=2", "overall:", "by_difficulty:",
		"D0:", "D1:", "top_failures:", "PARSE_FAILURE", "worst_cases:",
		"CASE-001", "CASE-002",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary missing %q:\n%s", want, s)
		}
	}
}
