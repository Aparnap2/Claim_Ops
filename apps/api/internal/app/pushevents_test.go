package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"claimops-api/internal/repository/postgres"

	"github.com/gofiber/fiber/v2"
)

func pushApp(handle func(ctx context.Context, event []byte) error, verify TokenVerifier, auth PushAuth) *fiber.App {
	a := fiber.New()
	a.Post("/events/document-ingested", DocumentPushHandler(handle, verify, auth))
	return a
}

func pushBody(t *testing.T, event map[string]string, attrs map[string]string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{
		"schema_version": "document-ingested.v1",
		"tenant":         event["tenant"],
		"claim":          event["claim"],
		"document_id":    event["document_id"],
		"sha256":         event["sha256"],
	})
	body, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"data":       base64.StdEncoding.EncodeToString(raw),
			"attributes": attrs,
			"messageId":  "msg-1",
		},
		"subscription": "projects/p/subscriptions/s",
	})
	return string(body)
}

func TestPushHappyAck(t *testing.T) {
	var got []byte
	var tenantSeen string
	a := pushApp(func(ctx context.Context, event []byte) error {
		got = append([]byte(nil), event...)
		if tenant, err := postgres.TenantFrom(ctx); err == nil {
			tenantSeen = string(tenant)
		}
		return nil
	}, nil, PushAuth{Mode: "none"})
	req := httptest.NewRequest(http.MethodPost, "/events/document-ingested",
		strings.NewReader(pushBody(t,
			map[string]string{"tenant": "t1", "claim": "c1", "document_id": "d1", "sha256": "ab"},
			map[string]string{"tenant_id": "t1", "event_id": "e1"})))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(got), `"document_id":"d1"`) {
		t.Fatalf("handler got wrong bytes: %s", got)
	}
	if tenantSeen != "t1" {
		t.Fatalf("tenant ctx = %q, want t1", tenantSeen)
	}
}

func TestPushReplayAcksTwice(t *testing.T) {
	calls := 0
	a := pushApp(func(_ context.Context, _ []byte) error { calls++; return nil },
		nil, PushAuth{Mode: "none"})
	body := pushBody(t,
		map[string]string{"tenant": "t1", "claim": "c1", "document_id": "d1", "sha256": "ab"},
		map[string]string{"tenant_id": "t1"})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/events/document-ingested", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("replay %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (ack each; idempotency is the processor's job)", calls)
	}
}

func TestPushTerminalStillAcks(t *testing.T) {
	a := pushApp(func(_ context.Context, _ []byte) error { return errors.New("boom") },
		nil, PushAuth{Mode: "none"})
	req := httptest.NewRequest(http.MethodPost, "/events/document-ingested",
		strings.NewReader(pushBody(t,
			map[string]string{"tenant": "t1", "claim": "c1", "document_id": "d1", "sha256": "ab"},
			map[string]string{"tenant_id": "t1"})))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("terminal outcome must still ack: status = %d", resp.StatusCode)
	}
}

func TestPushMalformed(t *testing.T) {
	a := pushApp(nil, nil, PushAuth{Mode: "none"})
	for _, body := range []string{"{bad json", `{"message":{}}`, `{"message":{"data":""}}`} {
		req := httptest.NewRequest(http.MethodPost, "/events/document-ingested", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestPushOIDC(t *testing.T) {
	verify := func(_ context.Context, token, _ string) (string, error) {
		if token == "good-token" {
			return "push@proj.iam.gserviceaccount.com", nil
		}
		return "", errors.New("bad token")
	}
	auth := PushAuth{Mode: "oidc", Audience: "https://worker.example", ServiceAccount: "push@proj.iam.gserviceaccount.com"}
	newReq := func(token string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/events/document-ingested",
			strings.NewReader(pushBody(t,
				map[string]string{"tenant": "t1", "claim": "c1", "document_id": "d1", "sha256": "ab"},
				map[string]string{"tenant_id": "t1"})))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return req
	}
	cases := []struct {
		name   string
		token  string
		auth   PushAuth
		status int
	}{
		{"valid", "good-token", auth, 200},
		{"bad token", "bad-token", auth, 401},
		{"missing", "", auth, 401},
		{"wrong sender", "good-token", PushAuth{Mode: "oidc", Audience: "https://worker.example", ServiceAccount: "other@proj.iam.gserviceaccount.com"}, 403},
	}
	for _, tc := range cases {
		a := pushApp(func(_ context.Context, _ []byte) error { return nil }, verify, tc.auth)
		resp, err := a.Test(newReq(tc.token))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: status = %d, want %d", tc.name, resp.StatusCode, tc.status)
		}
	}
}

func TestPushUnknownModeFailsClosed(t *testing.T) {
	a := pushApp(nil, nil, PushAuth{Mode: "mystery"})
	req := httptest.NewRequest(http.MethodPost, "/events/document-ingested",
		strings.NewReader(pushBody(t, map[string]string{}, map[string]string{})))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}
