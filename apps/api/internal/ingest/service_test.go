// Package ingest_test covers the Postgres-backed upload service.
//
// Transport note (parser-agnostic): blobs are opaque bytes and outbox
// events stay IDs-only. The upload payload carries exactly the envelope
// keys {schema_version, event_id, occurred_at, tenant, claim, document_id,
// sha256} — no other keys, ever (see TestUploadEventHasNoParserFields).
package ingest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claimops-api/internal/adapters/memblob"
	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/ingest"
	"claimops-api/internal/ports"
	"claimops-api/internal/repository/postgres"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// allowedUploadKeys is the closed key set for the document.ingested.v1
// payload. Mirrors ingest.BuildUploadPayload's contract; any drift fails
// the key-set assertions below.
var allowedUploadKeys = map[string]bool{
	"schema_version": true,
	"event_id":       true,
	"occurred_at":    true,
	"tenant":         true,
	"claim":          true,
	"document_id":    true,
	"sha256":         true,
}

// forbiddenPayloadSubstrings guards the parser-agnostic contract: no
// parser/OCR/extraction vocabulary may appear anywhere in the serialized
// event payload.
var forbiddenPayloadSubstrings = []string{
	"pars",
	"ocr",
	"tesseract",
	"textract",
	"vision",
	"llm",
	"prompt",
	"classif",
}

var svcSeq atomic.Int64

func svcSuffix(prefix string) string {
	n := svcSeq.Add(1)
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), n)
}

func svcDSN() string {
	if v := os.Getenv("TEST_POSTGRES_DSN"); v != "" {
		return v
	}
	return "postgres://claimops_app:claimops_app@localhost:5433/claimops"
}

// requireSvcPool dials the live DB as the NON-superuser app role (RLS
// enforced). If the DB is unreachable the test is skipped so unit CI stays
// green.
func requireSvcPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, svcDSN())
	if err != nil {
		t.Skipf("postgres unavailable (dial): %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres unavailable (ping): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// beginSvcAs scopes a fresh transaction to tenant via BeginTenantTx.
func beginSvcAs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenant claims.TenantID) (context.Context, pgx.Tx) {
	t.Helper()
	tctx := postgres.WithTenant(ctx, tenant)
	tx, err := postgres.BeginTenantTx(tctx, pool)
	if err != nil {
		t.Fatalf("BeginTenantTx(%s): %v", tenant, err)
	}
	return tctx, tx
}

// mustSaveSvcClaim inserts the parent claim row the documents.claim_id FK
// requires. Documents cannot exist without their claim.
func mustSaveSvcClaim(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID) {
	t.Helper()
	c, err := claims.NewClaim(
		claimID,
		tenant,
		claims.PolicyID("pol-"+string(claimID)),
		"ref-"+string(claimID),
		claims.MustPaise(100, 0),
		claims.ClaimStatusReceived,
		1,
		time.Time{},
		time.Time{},
		time.Time{},
	)
	if err != nil {
		t.Fatalf("NewClaim: %v", err)
	}
	ctx := context.Background()
	tctx, tx := beginSvcAs(t, ctx, pool, tenant)
	if err := postgres.New(pool).SaveClaim(tctx, tx, *c); err != nil {
		_ = tx.Rollback(tctx)
		t.Fatalf("SaveClaim: %v", err)
	}
	if err := tx.Commit(tctx); err != nil {
		t.Fatalf("commit claim: %v", err)
	}
}

// countOutboxByTenant counts outbox rows visible to tenant (RLS scope).
func countOutboxByTenant(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID) int {
	t.Helper()
	ctx := context.Background()
	tctx, tx := beginSvcAs(t, ctx, pool, tenant)
	defer func() { _ = tx.Rollback(tctx) }()
	var n int
	if err := tx.QueryRow(tctx, `SELECT COUNT(*) FROM outbox_events WHERE tenant_id = $1`, string(tenant)).Scan(&n); err != nil {
		t.Fatalf("count outbox_events: %v", err)
	}
	return n
}

// getOutboxPayload fetches the raw payload for eventID in tenant scope.
func getOutboxPayload(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, eventID string) []byte {
	t.Helper()
	ctx := context.Background()
	tctx, tx := beginSvcAs(t, ctx, pool, tenant)
	defer func() { _ = tx.Rollback(tctx) }()
	var payload []byte
	if err := tx.QueryRow(tctx, `SELECT payload FROM outbox_events WHERE event_id = $1`, eventID).Scan(&payload); err != nil {
		t.Fatalf("read outbox payload %s: %v", eventID, err)
	}
	return payload
}

// listSvcDocs returns every document for claimID in tenant scope.
func listSvcDocs(t *testing.T, pool *pgxpool.Pool, tenant claims.TenantID, claimID claims.ClaimID) []documents.Document {
	t.Helper()
	ctx := context.Background()
	tctx, tx := beginSvcAs(t, ctx, pool, tenant)
	defer func() { _ = tx.Rollback(tctx) }()
	got, err := postgres.New(pool).ListDocumentsByClaim(tctx, tx, claimID)
	if err != nil {
		t.Fatalf("ListDocumentsByClaim: %v", err)
	}
	return got
}

// assertUploadPayloadKeys unmarshals payload and asserts the key set is
// exactly the 7 allowed envelope keys with sane values, and that no
// parser/OCR vocabulary appears anywhere in the serialized form.
func assertUploadPayloadKeys(t *testing.T, payload []byte, tenant, claimID string, doc documents.Document) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("payload unmarshal: %v body=%s", err, payload)
	}
	if len(m) != len(allowedUploadKeys) {
		t.Fatalf("payload keys = %v, want exactly %d allowed keys", m, len(allowedUploadKeys))
	}
	for k := range m {
		if !allowedUploadKeys[k] {
			t.Fatalf("payload has forbidden key %q (full=%s)", k, payload)
		}
	}
	if m["schema_version"] != ports.DocumentIngestedSchemaVersion {
		t.Fatalf("schema_version = %v, want %q", m["schema_version"], ports.DocumentIngestedSchemaVersion)
	}
	if m["event_id"] != "evt-"+doc.ID {
		t.Fatalf("event_id = %v, want %q", m["event_id"], "evt-"+doc.ID)
	}
	if m["tenant"] != tenant {
		t.Fatalf("tenant = %v, want %q", m["tenant"], tenant)
	}
	if m["claim"] != claimID {
		t.Fatalf("claim = %v, want %q", m["claim"], claimID)
	}
	if m["document_id"] != doc.ID {
		t.Fatalf("document_id = %v, want %q", m["document_id"], doc.ID)
	}
	if m["sha256"] != doc.SHA256 {
		t.Fatalf("sha256 = %v, want %q", m["sha256"], doc.SHA256)
	}
	occurred, _ := m["occurred_at"].(string)
	if _, err := time.Parse(time.RFC3339, occurred); err != nil {
		t.Fatalf("occurred_at not RFC3339: %q (%v)", occurred, err)
	}
	lowered := strings.ToLower(string(payload))
	for _, bad := range forbiddenPayloadSubstrings {
		if strings.Contains(lowered, bad) {
			t.Fatalf("payload contains forbidden %q (full=%s)", bad, payload)
		}
	}
}

// TestServiceUploadHappyPath is the live-gated happy path: memblob as the
// Blobs fake plus live PG. Asserts bytes staged under the provider-neutral
// key, a RECEIVED doc row, and an outbox row for the event ID with the
// exact payload key set.
func TestServiceUploadHappyPath(t *testing.T) {
	pool := requireSvcPool(t)
	blobs := memblob.New()
	svc := ingest.NewService(blobs, pool)
	ctx := context.Background()

	tenant := claims.TenantID(svcSuffix("svc-happy-tenant"))
	claimID := claims.ClaimID(svcSuffix("svc-happy-claim"))
	mustSaveSvcClaim(t, pool, tenant, claimID)

	content := []byte("%PDF-1.4\nopaque-bytes-" + svcSuffix("svc-happy-body") + strings.Repeat(" ", 600))
	doc, created, err := svc.Upload(ctx, string(tenant), string(claimID), "bill.pdf", "application/pdf", content)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on first Upload")
	}
	if !strings.HasPrefix(doc.ID, "doc-") {
		t.Fatalf("doc ID = %q, want doc- prefix", doc.ID)
	}
	if doc.Status != documents.StReceived {
		t.Fatalf("doc status = %q, want %q", doc.Status, documents.StReceived)
	}
	sum := sha256.Sum256(content)
	if want := hex.EncodeToString(sum[:]); doc.SHA256 != want {
		t.Fatalf("doc sha256 = %q, want %q", doc.SHA256, want)
	}
	if doc.SizeBytes != int64(len(content)) {
		t.Fatalf("doc size = %d, want %d", doc.SizeBytes, len(content))
	}

	// Bytes staged under the provider-neutral key, verbatim.
	key := ports.DocumentObjectKey(string(tenant), string(claimID), doc.ID)
	rc, err := blobs.Get(ctx, ports.ObjectRef{Key: key})
	if err != nil {
		t.Fatalf("staged blob missing for key %q: %v", key, err)
	}
	staged, _ := io.ReadAll(rc)
	rc.Close()
	if string(staged) != string(content) {
		t.Fatalf("staged bytes = %q, want %q", staged, content)
	}

	// One RECEIVED doc row.
	rows := listSvcDocs(t, pool, tenant, claimID)
	if len(rows) != 1 {
		t.Fatalf("document rows = %d, want 1", len(rows))
	}
	if rows[0].ID != doc.ID || rows[0].Status != documents.StReceived || rows[0].SHA256 != doc.SHA256 {
		t.Fatalf("document row = %+v, want ID/status/sha of %+v", rows[0], doc)
	}

	// One outbox row for the event ID with the exact payload key set.
	eventID := "evt-" + doc.ID
	payload := getOutboxPayload(t, pool, tenant, eventID)
	assertUploadPayloadKeys(t, payload, string(tenant), string(claimID), doc)
}

// TestServiceUploadDuplicate replays the same bytes: created=false, no
// second outbox row, no second document row, and the blob store keeps only
// the original staged key (the replay's staged key is best-effort
// deleted on the dedup rollback path).
func TestServiceUploadDuplicate(t *testing.T) {
	pool := requireSvcPool(t)
	blobs := memblob.New()
	svc := ingest.NewService(blobs, pool)
	ctx := context.Background()

	tenant := claims.TenantID(svcSuffix("svc-dup-tenant"))
	claimID := claims.ClaimID(svcSuffix("svc-dup-claim"))
	mustSaveSvcClaim(t, pool, tenant, claimID)

	content := []byte("%PDF-1.4\nopaque-bytes-" + svcSuffix("svc-dup-body") + strings.Repeat(" ", 600))
	first, created, err := svc.Upload(ctx, string(tenant), string(claimID), "bill.pdf", "application/pdf", content)
	if err != nil {
		t.Fatalf("first Upload: %v", err)
	}
	if !created {
		t.Fatal("expected created=true on first Upload")
	}

	second, created, err := svc.Upload(ctx, string(tenant), string(claimID), "bill.pdf", "application/pdf", content)
	if err != nil {
		t.Fatalf("duplicate Upload: %v", err)
	}
	if created {
		t.Fatal("expected created=false on duplicate Upload")
	}
	if second != (documents.Document{}) {
		t.Fatalf("duplicate must return zero Document, got %+v", second)
	}

	if n := countOutboxByTenant(t, pool, tenant); n != 1 {
		t.Fatalf("outbox rows for tenant = %d, want 1 (no second row)", n)
	}
	if rows := listSvcDocs(t, pool, tenant, claimID); len(rows) != 1 || rows[0].ID != first.ID {
		t.Fatalf("document rows = %+v, want exactly the first row", rows)
	}
	if n := blobs.Len(); n != 1 {
		t.Fatalf("blob entries = %d, want 1 (replay key cleaned up)", n)
	}
}

// failBlob is a BlobStore whose Put always fails, proving Upload writes
// zero DB rows when staging fails.
type failBlob struct{ err error }

func (f failBlob) Put(_ context.Context, _ ports.ObjectRef, _ io.Reader) error {
	return f.err
}

func (f failBlob) Get(_ context.Context, _ ports.ObjectRef) (io.ReadCloser, error) {
	return nil, io.ErrUnexpectedEOF
}

func (f failBlob) Delete(_ context.Context, _ ports.ObjectRef) error { return nil }

// TestServiceUploadBlobFailure stages nothing on blob failure: Upload
// errors and leaves zero document and zero outbox rows.
func TestServiceUploadBlobFailure(t *testing.T) {
	pool := requireSvcPool(t)
	svc := ingest.NewService(failBlob{err: fmt.Errorf("gcs: connection refused")}, pool)
	ctx := context.Background()

	tenant := claims.TenantID(svcSuffix("svc-fail-tenant"))
	claimID := claims.ClaimID(svcSuffix("svc-fail-claim"))
	mustSaveSvcClaim(t, pool, tenant, claimID)

	if _, _, err := svc.Upload(ctx, string(tenant), string(claimID), "bill.pdf", "application/pdf", []byte("%PDF-1.4\nopaque"+strings.Repeat(" ", 600))); err == nil {
		t.Fatal("expected error on blob failure, got nil")
	}
	if rows := listSvcDocs(t, pool, tenant, claimID); len(rows) != 0 {
		t.Fatalf("document rows = %d, want 0 after blob failure", len(rows))
	}
	if n := countOutboxByTenant(t, pool, tenant); n != 0 {
		t.Fatalf("outbox rows for tenant = %d, want 0 after blob failure", n)
	}
}

// TestUploadEventHasNoParserFields is a pure unit test (no DB, no blob):
// the marshaled BuildUploadPayload output carries exactly the 7 allowed
// envelope keys and no parser/OCR vocabulary anywhere.
func TestUploadEventHasNoParserFields(t *testing.T) {
	doc := documents.Document{
		ID:        "doc-abc123",
		Tenant:    claims.TenantID("t-apollo"),
		ClaimID:   claims.ClaimID("CLM-1"),
		Type:      documents.DocHospitalBill,
		FileName:  "bill.pdf",
		MIME:      "application/pdf",
		SHA256:    "deadbeef",
		SizeBytes: 5,
		Status:    documents.StReceived,
	}
	raw, err := json.Marshal(ingest.BuildUploadPayload(doc, time.Now().UTC()))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	assertUploadPayloadKeys(t, raw, "t-apollo", "CLM-1", doc)
}
