package postgres_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/evidence"
	"claimops-api/internal/repository/postgres"
)

// uniqueDocSuffix isolates document/sha/evidence IDs across reruns: the app
// role cannot delete rows, so every test mints fresh identifiers (pid +
// atomic counter, mirroring uniqueClaimID).
func uniqueDocSuffix(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), idSeq.Add(1))
}

// newTestDocument builds a Document with minted ID/SHA so reruns never
// collide on the (tenant_id, claim_id, sha256) unique key.
func newTestDocument(tenant claims.TenantID, claimID claims.ClaimID, docType documents.DocType, status string) documents.Document {
	suffix := uniqueDocSuffix("doct")
	return documents.Document{
		ID:        "doc-" + suffix,
		Tenant:    tenant,
		ClaimID:   claimID,
		Type:      docType,
		FileName:  "discharge_summary.pdf",
		MIME:      "application/pdf",
		SHA256:    "sha256-" + suffix,
		SizeBytes: 1234,
		Status:    status,
	}
}

// newTestEvidence builds a FieldEvidence pinning field to doc, with a
// minted ID so reruns never collide on the primary key.
func newTestEvidence(tenant claims.TenantID, claimID claims.ClaimID, doc documents.Document) evidence.FieldEvidence {
	return evidence.FieldEvidence{
		ID:         "evf-" + uniqueDocSuffix("evf"),
		Tenant:     tenant,
		ClaimID:    claimID,
		DocumentID: doc.ID,
		Field:      "total_bill",
		Value:      "Rs. 12,345.00",
		Anchor:     "Total Bill: Rs. 12,345.00",
		Extractor:  "tier1-regex",
		DocType:    string(doc.Type),
		Page:       1,
		Confidence: 0.9,
	}
}

// 1. Insert + conflict-dedup + list round-trip: the first insert reports
// inserted=true; re-inserting the same (tenant, claim, sha256) with a new
// id and a different status reports inserted=false and changes nothing
// (append-only: no UPDATE path exists for the app role).
func TestDocumentInsert_DedupAndListRoundTrip(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := claims.TenantID("doct-tenant-a")
	claimID := uniqueClaimID("doct-claim")

	tctx, tx := beginAs(t, ctx, pool, tenant)
	if err := repo.SaveClaim(tctx, tx, mustNewClaim(t, claimID, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim: %v", err)
	}
	commit(t, tctx, tx)

	doc := newTestDocument(tenant, claimID, documents.DocDischargeSummary, documents.StReceived)

	tctx, tx = beginAs(t, ctx, pool, tenant)
	inserted, err := repo.InsertDocument(tctx, tx, doc)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("InsertDocument #1: %v", err)
	}
	if !inserted {
		rollback(t, tctx, tx)
		t.Fatalf("InsertDocument #1 inserted = false, want true")
	}
	commit(t, tctx, tx)

	// Duplicate content under a new id and escalated status: dedup wins,
	// the stored row keeps the original status.
	dup := doc
	dup.ID = "doc-" + uniqueDocSuffix("doct-dup")
	dup.Status = documents.StProcessed
	tctx, tx = beginAs(t, ctx, pool, tenant)
	inserted, err = repo.InsertDocument(tctx, tx, dup)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("InsertDocument #2 (replay): %v", err)
	}
	if inserted {
		rollback(t, tctx, tx)
		t.Fatalf("InsertDocument #2 inserted = true, want false (conflict dedup)")
	}
	commit(t, tctx, tx)

	tctx, tx = beginAs(t, ctx, pool, tenant)
	got, err := repo.ListDocumentsByClaim(tctx, tx, claimID)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("ListDocumentsByClaim: %v", err)
	}
	if len(got) != 1 {
		rollback(t, tctx, tx)
		t.Fatalf("ListDocumentsByClaim rows = %d, want 1", len(got))
	}
	g := got[0]
	if g.ID != doc.ID ||
		g.Tenant != doc.Tenant ||
		g.ClaimID != doc.ClaimID ||
		g.Type != doc.Type ||
		g.FileName != doc.FileName ||
		g.MIME != doc.MIME ||
		g.SHA256 != doc.SHA256 ||
		g.SizeBytes != doc.SizeBytes ||
		g.Status != documents.StReceived {
		rollback(t, tctx, tx)
		t.Fatalf("ListDocumentsByClaim row = %+v, want %+v (original status, replay ignored)", g, doc)
	}
	rollback(t, tctx, tx)
}

// 2. Evidence insert + conflict-dedup + full-column list round-trip: every
// persisted column (field, value, anchor, extractor, doc_type via the
// parent join, page, confidence) comes back exactly.
func TestFieldEvidenceInsert_DedupAndListRoundTrip(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenant := claims.TenantID("docev-tenant-a")
	claimID := uniqueClaimID("docev-claim")

	tctx, tx := beginAs(t, ctx, pool, tenant)
	if err := repo.SaveClaim(tctx, tx, mustNewClaim(t, claimID, tenant)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim: %v", err)
	}
	commit(t, tctx, tx)

	doc := newTestDocument(tenant, claimID, documents.DocHospitalBill, documents.StProcessed)
	tctx, tx = beginAs(t, ctx, pool, tenant)
	if _, err := repo.InsertDocument(tctx, tx, doc); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("InsertDocument: %v", err)
	}
	commit(t, tctx, tx)

	want := newTestEvidence(tenant, claimID, doc)

	tctx, tx = beginAs(t, ctx, pool, tenant)
	inserted, err := repo.InsertFieldEvidence(tctx, tx, want)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("InsertFieldEvidence #1: %v", err)
	}
	if !inserted {
		rollback(t, tctx, tx)
		t.Fatalf("InsertFieldEvidence #1 inserted = false, want true")
	}
	commit(t, tctx, tx)

	// Replay: same (tenant, claim, document, field) with a new id and a
	// different value must dedup to inserted=false.
	replay := want
	replay.ID = "evf-" + uniqueDocSuffix("evf-dup")
	replay.Value = "Rs. 99,999.00"
	tctx, tx = beginAs(t, ctx, pool, tenant)
	inserted, err = repo.InsertFieldEvidence(tctx, tx, replay)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("InsertFieldEvidence #2 (replay): %v", err)
	}
	if inserted {
		rollback(t, tctx, tx)
		t.Fatalf("InsertFieldEvidence #2 inserted = true, want false (conflict dedup)")
	}
	commit(t, tctx, tx)

	tctx, tx = beginAs(t, ctx, pool, tenant)
	got, err := repo.ListEvidenceByClaim(tctx, tx, claimID)
	if err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("ListEvidenceByClaim: %v", err)
	}
	if len(got) != 1 {
		rollback(t, tctx, tx)
		t.Fatalf("ListEvidenceByClaim rows = %d, want 1", len(got))
	}
	g := got[0]
	if g.ID != want.ID ||
		g.Tenant != want.Tenant ||
		g.ClaimID != want.ClaimID ||
		g.DocumentID != want.DocumentID ||
		g.Field != want.Field ||
		g.Value != want.Value ||
		g.Anchor != want.Anchor ||
		g.Extractor != want.Extractor ||
		g.DocType != want.DocType ||
		g.Page != want.Page ||
		g.Confidence != want.Confidence {
		rollback(t, tctx, tx)
		t.Fatalf("ListEvidenceByClaim row = %+v, want %+v (all columns incl. anchor/extractor/doc_type/page/confidence)", g, want)
	}
	rollback(t, tctx, tx)
}

// 3. Cross-tenant invisibility spot-check: a second tenant's RLS scope sees
// zero document and zero evidence rows for tenant A's claim, while A still
// sees its own rows (proves the miss is RLS, not absence).
func TestDocumentCrossTenant_Invisible(t *testing.T) {
	pool := requirePool(t)
	repo := postgres.New(pool)
	ctx := context.Background()
	tenantA := claims.TenantID("docx-tenant-a")
	tenantB := claims.TenantID("docx-tenant-b")
	claimID := uniqueClaimID("docx-claim")

	tctx, tx := beginAs(t, ctx, pool, tenantA)
	if err := repo.SaveClaim(tctx, tx, mustNewClaim(t, claimID, tenantA)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("SaveClaim A: %v", err)
	}
	commit(t, tctx, tx)

	doc := newTestDocument(tenantA, claimID, documents.DocClaimForm, documents.StReceived)
	tctx, tx = beginAs(t, ctx, pool, tenantA)
	if _, err := repo.InsertDocument(tctx, tx, doc); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("InsertDocument A: %v", err)
	}
	commit(t, tctx, tx)

	tctx, tx = beginAs(t, ctx, pool, tenantA)
	if _, err := repo.InsertFieldEvidence(tctx, tx, newTestEvidence(tenantA, claimID, doc)); err != nil {
		rollback(t, tctx, tx)
		t.Fatalf("InsertFieldEvidence A: %v", err)
	}
	commit(t, tctx, tx)

	bctx, btx := beginAs(t, ctx, pool, tenantB)
	bDocs, err := repo.ListDocumentsByClaim(bctx, btx, claimID)
	if err != nil {
		rollback(t, bctx, btx)
		t.Fatalf("B ListDocumentsByClaim: %v", err)
	}
	if len(bDocs) != 0 {
		rollback(t, bctx, btx)
		t.Fatalf("B ListDocumentsByClaim rows = %d, want 0", len(bDocs))
	}
	bEv, err := repo.ListEvidenceByClaim(bctx, btx, claimID)
	if err != nil {
		rollback(t, bctx, btx)
		t.Fatalf("B ListEvidenceByClaim: %v", err)
	}
	if len(bEv) != 0 {
		rollback(t, bctx, btx)
		t.Fatalf("B ListEvidenceByClaim rows = %d, want 0", len(bEv))
	}
	rollback(t, bctx, btx)

	// Sanity: A still sees its own rows.
	actx, atx := beginAs(t, ctx, pool, tenantA)
	aDocs, err := repo.ListDocumentsByClaim(actx, atx, claimID)
	if err != nil {
		rollback(t, actx, atx)
		t.Fatalf("A ListDocumentsByClaim: %v", err)
	}
	if len(aDocs) != 1 {
		rollback(t, actx, atx)
		t.Fatalf("A ListDocumentsByClaim rows = %d, want 1", len(aDocs))
	}
	aEv, err := repo.ListEvidenceByClaim(actx, atx, claimID)
	if err != nil {
		rollback(t, actx, atx)
		t.Fatalf("A ListEvidenceByClaim: %v", err)
	}
	if len(aEv) != 1 {
		rollback(t, actx, atx)
		t.Fatalf("A ListEvidenceByClaim rows = %d, want 1", len(aEv))
	}
	rollback(t, actx, atx)
}
