package ingest_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"claimops-api/internal/adapters/memblob"
	"claimops-api/internal/claims"
	"claimops-api/internal/ingest"
	"claimops-api/internal/ports"
)

var corpusSeq int64

// corpusSvc builds a Service over the memory blob port and the live
// database with a fresh tenant+claim (parent claim row included for the
// documents.claim_id FK); skips when TEST_POSTGRES_DSN is absent.
func corpusSvc(t *testing.T) (*ingest.Service, *memblob.Store, string, string) {
	t.Helper()
	pool := requireSvcPool(t)
	n := atomic.AddInt64(&corpusSeq, 1)
	tenant := claims.TenantID(fmt.Sprintf("eng-t-%d-%d", os.Getpid(), n))
	claim := claims.ClaimID(fmt.Sprintf("ENG-C-%d-%d", os.Getpid(), n))
	mustSaveSvcClaim(t, pool, tenant, claim)
	blobs := memblob.New()
	return ingest.NewService(blobs, pool), blobs, string(tenant), string(claim)
}

func TestEngineeringEmptyRejected(t *testing.T) {
	svc, _, tenant, claim := corpusSvc(t)
	if _, _, err := svc.Upload(context.Background(), tenant, claim, "empty.pdf", "application/pdf", nil); err == nil {
		t.Fatal("expected validation error for empty content")
	} else if !ingest.IsValidation(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestEngineeringGarbageAcceptedOpaque(t *testing.T) {
	svc, blobs, tenant, claim := corpusSvc(t)
	doc, created, err := svc.Upload(context.Background(), tenant, claim,
		"scan.pdf", "application/pdf", []byte("this is not a pdf, just text"))
	if err != nil || !created {
		t.Fatalf("garbage bytes must be accepted opaquely: %v created=%v", err, created)
	}
	rc, err := blobs.Get(context.Background(), ports.ObjectRef{
		Key: ports.DocumentObjectKey(tenant, claim, doc.ID),
	})
	if err != nil {
		t.Fatalf("staged blob missing: %v", err)
	}
	defer rc.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "this is not a pdf, just text" {
		t.Fatalf("staged bytes mismatch: %q", buf.String())
	}
}

func TestEngineeringLargeBlob(t *testing.T) {
	svc, _, tenant, claim := corpusSvc(t)
	big := bytes.Repeat([]byte{0}, 5<<20)
	_, created, err := svc.Upload(context.Background(), tenant, claim, "big.bin", "application/octet-stream", big)
	if err != nil || !created {
		t.Fatalf("5MiB blob must be accepted (no cap in #15): %v", err)
	}
}

func TestEngineeringDuplicateConverges(t *testing.T) {
	svc, _, tenant, claim := corpusSvc(t)
	content := []byte("same-bytes-twice")
	if _, c1, err := svc.Upload(context.Background(), tenant, claim, "a.pdf", "application/pdf", content); err != nil || !c1 {
		t.Fatalf("first upload: %v created=%v", err, c1)
	}
	if _, c2, err := svc.Upload(context.Background(), tenant, claim, "b.pdf", "application/pdf", content); err != nil || c2 {
		t.Fatalf("duplicate upload must converge: %v created=%v", err, c2)
	}
}
