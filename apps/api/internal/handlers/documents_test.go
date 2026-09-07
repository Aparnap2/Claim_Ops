package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claimops-api/internal/claims"
	"claimops-api/internal/documents"
	"claimops-api/internal/handlers"
	"claimops-api/internal/ingest"
	"claimops-api/internal/middleware"

	"github.com/gofiber/fiber/v2"
)

// stubUploader is a handlers.Uploader fake: it records the call and replays
// a canned (doc, created, err). No DB, no blob, no event bus — outbox
// behavior is owned by the ingest service tests, not the HTTP edge.
type stubUploader struct {
	doc                       documents.Document
	created                   bool
	err                       error
	calls                     int
	tenant, claim, file, mime string
	content                   []byte
}

func (s *stubUploader) Upload(_ context.Context, tenant, claimID, fileName, mime string, content []byte) (documents.Document, bool, error) {
	s.calls++
	s.tenant, s.claim, s.file, s.mime = tenant, claimID, fileName, mime
	s.content = append([]byte(nil), content...)
	return s.doc, s.created, s.err
}

// newUploadApp builds a local Fiber app wiring the tenant middleware and
// the document route only. It deliberately does NOT import the app package:
// handler tests must not depend on the full stack or its dependencies.
func newUploadApp(u handlers.Uploader) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(middleware.TenantContext())
	app.Post("/claims/:id/documents", handlers.PostDocument(u))
	return app
}

func postUpload(t *testing.T, app *fiber.App, claimID, tenant, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/claims/"+claimID+"/documents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func decodeUpload(t *testing.T, resp *http.Response) (ok, duplicate bool, docID string, raw string) {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	raw = string(body)
	var got struct {
		OK        bool `json:"ok"`
		Duplicate bool `json:"duplicate"`
		Document  struct {
			ID string `json:"ID"`
		} `json:"document"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode failed: %v body=%s", err, body)
	}
	return got.OK, got.Duplicate, got.Document.ID, raw
}

func stubDoc() documents.Document {
	return documents.Document{
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
}

func TestPostDocumentCreated(t *testing.T) {
	stub := &stubUploader{doc: stubDoc(), created: true}
	app := newUploadApp(stub)
	resp := postUpload(t, app, "CLM-1", "t-apollo",
		`{"file_name":"bill.pdf","mime":"application/pdf","content_text":"hello"}`)
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, raw)
	}
	ok, dup, docID, _ := decodeUpload(t, resp)
	if !ok || dup {
		t.Fatalf("expected ok:true duplicate:false, got ok=%v dup=%v", ok, dup)
	}
	if docID != "doc-abc123" {
		t.Fatalf("document id = %q, want doc-abc123", docID)
	}
	if stub.calls != 1 {
		t.Fatalf("uploader calls = %d, want 1", stub.calls)
	}
	if stub.tenant != "t-apollo" || stub.claim != "CLM-1" {
		t.Fatalf("uploader tenant/claim = %q/%q, want t-apollo/CLM-1", stub.tenant, stub.claim)
	}
	if stub.file != "bill.pdf" || stub.mime != "application/pdf" || string(stub.content) != "hello" {
		t.Fatalf("uploader payload = %q %q %q, want bill.pdf application/pdf hello",
			stub.file, stub.mime, stub.content)
	}
}

func TestPostDocumentDuplicate(t *testing.T) {
	stub := &stubUploader{doc: stubDoc(), created: false}
	app := newUploadApp(stub)
	resp := postUpload(t, app, "CLM-1", "t-apollo",
		`{"file_name":"bill.pdf","mime":"application/pdf","content_text":"hello"}`)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	ok, dup, docID, _ := decodeUpload(t, resp)
	if !ok || !dup {
		t.Fatalf("expected ok:true duplicate:true, got ok=%v dup=%v", ok, dup)
	}
	if docID != "doc-abc123" {
		t.Fatalf("document id = %q, want doc-abc123", docID)
	}
}

// validationErr returns a REAL ingest validation error (blank-tenant path
// on a pool-less service: validation runs before any blob/DB touch, so nil
// deps are safe) to prove the handler maps the exact sentinel to 400.
func validationErr(t *testing.T) error {
	t.Helper()
	probe := ingest.NewService(nil, nil)
	_, _, err := probe.Upload(context.Background(), "  ", "CLM-1", "bill.pdf", "application/pdf", []byte("hello"))
	if err == nil {
		t.Fatal("probe Upload with blank tenant must fail")
	}
	if !ingest.IsValidation(err) {
		t.Fatalf("probe error must satisfy IsValidation, got %v", err)
	}
	return err
}

func TestPostDocumentValidationError(t *testing.T) {
	stub := &stubUploader{doc: stubDoc(), err: validationErr(t)}
	app := newUploadApp(stub)
	resp := postUpload(t, app, "CLM-1", "t-apollo",
		`{"file_name":"bill.pdf","mime":"application/pdf","content_text":"hello"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "VALIDATION_ERROR") {
		t.Fatalf("expected VALIDATION_ERROR, got %s", raw)
	}
}

func TestPostDocumentBackendError(t *testing.T) {
	stub := &stubUploader{err: errors.New("blob unavailable")}
	app := newUploadApp(stub)
	resp := postUpload(t, app, "CLM-1", "t-apollo",
		`{"file_name":"bill.pdf","mime":"application/pdf","content_text":"hello"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "INTERNAL_ERROR") {
		t.Fatalf("expected INTERNAL_ERROR, got %s", raw)
	}
}

func TestPostDocumentMalformed(t *testing.T) {
	stub := &stubUploader{doc: stubDoc(), created: true}
	app := newUploadApp(stub)
	resp := postUpload(t, app, "CLM-1", "t-apollo", "{bad json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "VALIDATION_ERROR") {
		t.Fatalf("expected VALIDATION_ERROR, got %s", raw)
	}
	if stub.calls != 0 {
		t.Fatalf("uploader must not be called on malformed body, calls=%d", stub.calls)
	}
}

func TestPostDocumentNoTenant(t *testing.T) {
	stub := &stubUploader{doc: stubDoc(), created: true}
	app := newUploadApp(stub)
	resp := postUpload(t, app, "CLM-1", "",
		`{"file_name":"bill.pdf","mime":"application/pdf","content_text":"hello"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "TENANT_MISSING") {
		t.Fatalf("expected TENANT_MISSING, got %s", raw)
	}
	if stub.calls != 0 {
		t.Fatalf("uploader must not be called without tenant, calls=%d", stub.calls)
	}
}
