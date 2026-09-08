package workeradapter_test

import (
	"context"
	"strings"
	"testing"

	workeradapter "claimops-api/internal/adapters/worker"
	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/ports"
	"claimops-api/internal/worker"

	"claimops-api/internal/adapters/memblob"
)

type stubLister struct {
	docs []documents.Document
	err  error
}

func (s stubLister) ListDocuments(_ context.Context, _ claims.ClaimID) ([]documents.Document, error) {
	return s.docs, s.err
}

func seedMem(t *testing.T, blobs *memblob.Store, tenant, claim, doc, content string) {
	t.Helper()
	if err := blobs.Put(context.Background(), ports.ObjectRef{
		Key: ports.DocumentObjectKey(tenant, claim, doc),
	}, strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
}

func TestBlobFetchBridgeHit(t *testing.T) {
	blobs := memblob.New()
	seedMem(t, blobs, "t1", "c1", "doc-1", "hello-bytes")
	docs := stubLister{docs: []documents.Document{{
		ID: "doc-1", Tenant: "t1", ClaimID: "c1",
		FileName: "bill.pdf", MIME: "application/pdf",
		SHA256: "dd1fb82ed53df87c98fa9397b0ceba6b166374eab810ab351e5d955652613f37",
	}}}
	f := workeradapter.NewBlobFetchBridge(blobs, docs)
	fn, mime, content, err := f.Fetch(context.Background(), "t1", "c1", "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if fn != "bill.pdf" || mime != "application/pdf" || content != "hello-bytes" {
		t.Fatalf("mismatch: %q %q %q", fn, mime, content)
	}
	var _ worker.ContentFetcher = f
}

func TestBlobFetchBridgeUnknownDoc(t *testing.T) {
	f := workeradapter.NewBlobFetchBridge(memblob.New(), stubLister{})
	if _, _, _, err := f.Fetch(context.Background(), "t1", "c1", "nope"); err == nil {
		t.Fatal("expected unknown-document error")
	}
}

func TestBlobFetchBridgeMissingObject(t *testing.T) {
	blobs := memblob.New()
	docs := stubLister{docs: []documents.Document{{
		ID: "doc-9", Tenant: "t1", ClaimID: "c1",
		FileName: "x.pdf", MIME: "application/pdf",
	}}}
	f := workeradapter.NewBlobFetchBridge(blobs, docs)
	if _, _, _, err := f.Fetch(context.Background(), "t1", "c1", "doc-9"); err == nil {
		t.Fatal("expected missing-object error")
	}
}
