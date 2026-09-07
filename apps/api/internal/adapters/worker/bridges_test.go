package workeradapter_test

import (
	"context"
	"testing"
	"time"

	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/claims"
	"claimops-api/internal/ingest"
)

func TestToClaimViewMapping(t *testing.T) {
	adm := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	c := claims.Claim{
		ID:            "CLM-1",
		Tenant:        "t-acme",
		Policy:        "POL-9",
		AmountPaise:   150000,
		AdmissionDate: adm,
	}
	got := workeradapter.ToClaimView(c)
	if got.PolicyNumber != "POL-9" || got.ClaimedPaise != 150000 {
		t.Fatalf("policy/amount mismatch: %+v", got)
	}
	if !got.HasAdmission || got.Admission != adm {
		t.Fatalf("admission mismatch: %+v", got)
	}
	if got.HasDischarge {
		t.Fatalf("discharge should be unset: %+v", got)
	}
	if got.PatientName != "" || got.HospitalName != "" {
		t.Fatalf("patient/hospital must be empty (no such columns): %+v", got)
	}
}

func TestFetchBridgeHitAndMiss(t *testing.T) {
	blob := ingest.NewBlobStore()
	if err := blob.Put("doc-1", "bill.pdf", "application/pdf", "hello"); err != nil {
		t.Fatal(err)
	}
	f := workeradapter.NewFetchBridge(blob)
	fn, mime, content, err := f.Fetch(context.Background(), "t", "c", "doc-1")
	if err != nil || fn != "bill.pdf" || mime != "application/pdf" || content != "hello" {
		t.Fatalf("hit mismatch: %q %q %q %v", fn, mime, content, err)
	}
	if _, _, _, err := f.Fetch(context.Background(), "t", "c", "nope"); err == nil {
		t.Fatal("expected miss error")
	} else if err != ingest.ErrBlobNotFound {
		t.Fatalf("expected ErrBlobNotFound, got %v", err)
	}
}
