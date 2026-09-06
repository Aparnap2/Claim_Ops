package handlers_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claimops-api/internal/app"
	"claimops-api/internal/ingest"
	"claimops-api/internal/ports"
)

func newDocApp() (*ingest.Store, *ports.InMemoryBus, interface {
	Test(*http.Request, ...int) (*http.Response, error)
}) {
	store := ingest.New()
	bus := ports.NewInMemoryBus()
	return store, bus, app.NewWithDeps(store, bus)
}

func postDoc(t *testing.T, a interface {
	Test(*http.Request, ...int) (*http.Response, error)
}, claimID, tenant, body string, contentType string,
) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/claims/"+claimID+"/documents", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	if tenant != "" {
		req.Header.Set("X-Tenant-ID", tenant)
	}
	resp, err := a.Test(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func TestPostDocumentCreated(t *testing.T) {
	_, bus, a := newDocApp()
	resp := postDoc(t, a, "CLM-1", "t-apollo",
		`{"file_name":"bill.pdf","mime":"application/pdf","content_text":"hello"}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, raw)
	}
	var got struct {
		OK        bool `json:"ok"`
		Duplicate bool `json:"duplicate"`
		Document  struct {
			ID     string `json:"ID"`
			SHA256 string `json:"SHA256"`
		} `json:"document"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode failed: %v body=%s", err, raw)
	}
	if !got.OK || got.Duplicate {
		t.Fatalf("expected ok:true duplicate:false, got %s", raw)
	}
	if len(bus.Events[ports.TopicDocumentUploaded]) != 1 {
		t.Fatalf("expected 1 document.uploaded event, got %d", len(bus.Events[ports.TopicDocumentUploaded]))
	}
	var evt struct {
		SchemaVersion string `json:"schema_version"`
		Tenant        string `json:"tenant"`
		Claim         string `json:"claim"`
		DocumentID    string `json:"document_id"`
		SHA256        string `json:"sha256"`
	}
	if err := json.Unmarshal(bus.Events[ports.TopicDocumentUploaded][0], &evt); err != nil {
		t.Fatalf("event decode failed: %v", err)
	}
	if evt.SchemaVersion != ports.DocumentUploadedSchemaVersion {
		t.Fatalf("expected schema_version %q, got %q",
			ports.DocumentUploadedSchemaVersion, evt.SchemaVersion)
	}
	if evt.SchemaVersion != "document-uploaded.v1" {
		t.Fatalf("expected schema_version %q, got %q", "document-uploaded.v1", evt.SchemaVersion)
	}
	if evt.Tenant == "" || evt.Claim == "" || evt.DocumentID == "" || evt.SHA256 == "" {
		t.Fatalf("event missing required fields: %+v", evt)
	}
}

func TestPostDocumentDuplicate(t *testing.T) {
	_, bus, a := newDocApp()
	body := `{"file_name":"bill.pdf","mime":"application/pdf","content_text":"hello"}`
	r1 := postDoc(t, a, "CLM-1", "t-apollo", body, "")
	r1.Body.Close()
	if r1.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 first, got %d", r1.StatusCode)
	}
	resp := postDoc(t, a, "CLM-1", "t-apollo", body, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 duplicate, got %d: %s", resp.StatusCode, raw)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"duplicate":true`) {
		t.Fatalf("expected duplicate:true, got %s", raw)
	}
	if len(bus.Events[ports.TopicDocumentUploaded]) != 1 {
		t.Fatalf("duplicate must not publish again, events=%d", len(bus.Events[ports.TopicDocumentUploaded]))
	}
}

func TestPostDocumentMalformed(t *testing.T) {
	_, _, a := newDocApp()
	resp := postDoc(t, a, "CLM-1", "t-apollo", "{bad json", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "VALIDATION_ERROR") {
		t.Fatalf("expected VALIDATION_ERROR, got %s", raw)
	}
}

func TestPostDocumentNoTenant(t *testing.T) {
	_, _, a := newDocApp()
	resp := postDoc(t, a, "CLM-1", "",
		`{"file_name":"bill.pdf","mime":"application/pdf","content_text":"hello"}`, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "TENANT_MISSING") {
		t.Fatalf("expected TENANT_MISSING, got %s", raw)
	}
}
