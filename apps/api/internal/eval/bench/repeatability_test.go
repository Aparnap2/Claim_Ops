package bench

import (
	"context"
	"testing"
)

// TestRepeatDeterministic is the determinism proof: two full runs over the
// same temp corpus with the same stub parser must be repeat-equal
// (Meta + per-case RepeatKey + aggregates + worst cases; wall latency
// excluded by design).
func TestRepeatDeterministic(t *testing.T) {
	root, _ := seedBenchCorpus(t, []benchCaseSpec{
		{id: "CASE-001", docType: "discharge_summary", difficulty: "D0"},
		{id: "CASE-002", docType: "hospital_bill", difficulty: "D1"},
		{id: "CASE-003", docType: "discharge_summary", difficulty: "D1"},
	})
	a, err := Run(context.Background(), root, okStub(), testTenant, testClaimPrefix)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	b, err := Run(context.Background(), root, okStub(), testTenant, testClaimPrefix)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !RepeatEqual(a, b) {
		t.Fatalf("runs not repeat-equal:\nfirst  %+v\nsecond %+v", a, b)
	}
}
