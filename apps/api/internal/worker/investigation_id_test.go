package worker

// S6 RED: stable investigation identity per document delivery (Q3/Q6).
//
// buildExceptionEnvelope mints a random ID per call, so every redelivery
// would mint a fresh investigation — launch dedupe keyed by investigation
// ID could never converge and each redelivery would start a duplicate
// workflow execution. The worker must derive a STABLE investigation ID
// from (tenant, claim, document): same delivery => same ID, distinct
// documents => distinct IDs, tenant-bound, valid invest ID format.

import (
	"testing"

	"claimops-api/internal/invest"
)

func TestInvestigationID_StablePerDocument(t *testing.T) {
	a, err := investigationIDForDocument("tnt-s6", "clm-s6", "doc-01")
	if err != nil {
		t.Fatalf("investigationIDForDocument: %v", err)
	}
	b, err := investigationIDForDocument("tnt-s6", "clm-s6", "doc-01")
	if err != nil {
		t.Fatalf("investigationIDForDocument: %v", err)
	}
	if a != b {
		t.Fatalf("same delivery gave %q then %q: must be stable for launch dedupe", a, b)
	}
	if err := invest.ValidateID(invest.InvestigationIDPrefix, a); err != nil {
		t.Fatalf("stable ID %q fails invest validation: %v", a, err)
	}
}

func TestInvestigationID_DistinctDocumentsDistinctIDs(t *testing.T) {
	a, _ := investigationIDForDocument("tnt-s6", "clm-s6", "doc-01")
	b, _ := investigationIDForDocument("tnt-s6", "clm-s6", "doc-02")
	if a == b {
		t.Fatalf("distinct documents share ID %q: launches would collide", a)
	}
	c, _ := investigationIDForDocument("tnt-s6-OTHER", "clm-s6", "doc-01")
	if a == c {
		t.Fatalf("distinct tenants share ID %q: cross-tenant launch collision", a)
	}
}

func TestInvestigationID_BlankInputs_FailClosed(t *testing.T) {
	for name, tc := range map[string][3]string{
		"blank tenant": {"", "clm-s6", "doc-01"},
		"blank claim":  {"tnt-s6", "", "doc-01"},
		"blank doc":    {"tnt-s6", "clm-s6", ""},
	} {
		if _, err := investigationIDForDocument(tc[0], tc[1], tc[2]); err == nil {
			t.Fatalf("%s: want error, got success (never mint IDs from blank identity)", name)
		}
	}
}
