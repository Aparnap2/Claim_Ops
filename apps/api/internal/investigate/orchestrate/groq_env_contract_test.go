package orchestrate

// APA-45 provider contract qualification (test-only, no live Groq key).
//
// Companion to cmd/agent/groq_provider_selection_test.go, which proves the
// service-level routing. This file pins the provider contract underneath it:
//
//   - the ADR-002 canonical model string, agreed across the code default, the
//     README, and the ADR itself, so the three cannot drift apart silently;
//   - env resolution for GROQ_MODEL / GROQ_BASE_URL / GROQ_API_KEY;
//   - fail-closed behavior when no key is present (ADR-002 requires unit/CI
//     to never need a live key);
//   - that key material reaches the Authorization header and never appears in
//     an error, a response payload, or a log line.
//
// No real Groq call is made anywhere: every origin in this file is an
// httptest server on loopback, and defaultGroqBaseURL is asserted rather
// than dialled.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	// canonicalModelString is the ADR-002 adjudicated canonical model
	// string (Option A, 2026-09-26; re-adjudicated to a servable production
	// model in APA-47, 2026-09-28). It is duplicated here as a literal on
	// purpose: if the code default ever changes, this test fails and forces
	// an explicit decision rather than letting code and docs drift.
	canonicalModelString = "qwen/qwen3.8-27b"

	// sentinelKey is fake key material used as a leak canary. It is not a
	// real credential and must never be committed as one.
	sentinelKey = "gsk-SENTINEL-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// requireLoopback fails when rawURL is not a loopback httptest origin, so no
// assertion in this file can reach the real Groq API.
func requireLoopback(t *testing.T, rawURL string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse origin %q: %v", rawURL, err)
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("origin %q has no port: %v", rawURL, err)
	}
	if host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	t.Fatalf("origin %q is not loopback; this suite must never leave the host", rawURL)
}

// capture records the inbound request at a stub Groq origin.
type capture struct {
	base   string
	hits   int
	method string
	path   string
	auth   string
	header http.Header
	body   []byte
}

// runStub starts a loopback httptest origin that records every request and
// then delegates the response to handler. The base URL is exposed on the
// capture so the test can construct a client pointed at it.
func runStub(t *testing.T, handler http.HandlerFunc) *capture {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
		if err != nil {
			body = nil
		}
		cap.hits++
		cap.method, cap.path = r.Method, r.URL.Path
		cap.auth = r.Header.Get("Authorization")
		cap.header = r.Header.Clone()
		cap.body = body
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	requireLoopback(t, srv.URL)
	cap.base = srv.URL
	return cap
}

// respondOK answers with a chat completion whose content is a valid model act
// and whose model field is model.
func respondOK(model string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		act := `{"action":"call_tool","tool":"get_claim"}`
		payload, _ := json.Marshal(map[string]any{
			"id":      "stub",
			"model":   model,
			"choices": []any{map[string]any{"message": map[string]string{"content": act}}},
		})
		_, _ = w.Write(payload)
	}
}

// groqEnvelopeRequest is the minimal valid model request used for direct
// client calls. It touches no database and no tools.
func groqEnvelopeRequest(t *testing.T) ModelRequest {
	t.Helper()
	env := testEnvelope(t)
	return ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{env.EvidenceRefs[0].EvidenceID},
		Turn:             1,
		RequestID:        env.Scope.RequestID,
	}
}

// assertNoKey fails when value carries the sentinel key.
func assertNoKey(t *testing.T, where, value string) {
	t.Helper()
	if strings.Contains(value, sentinelKey) {
		t.Fatalf("sentinel Groq key leaked into %s", where)
	}
}

// ---------------------------------------------------------------------------
// Canonical model string: code default == README == ADR-002
// ---------------------------------------------------------------------------

func TestGroqContract_DefaultModelIsCanonicalString(t *testing.T) {
	if defaultGroqModel != canonicalModelString {
		t.Fatalf("code default = %q, want the ADR-002 canonical %q; ADR/README/code must move together",
			defaultGroqModel, canonicalModelString)
	}
	// The default base URL is the Groq OpenAI-compatible endpoint. Asserted,
	// never dialled: this suite performs zero egress.
	if defaultGroqBaseURL != "https://api.groq.com/openai/v1" {
		t.Fatalf("default base URL = %q, want the Groq OpenAI-compatible endpoint", defaultGroqBaseURL)
	}
}

// repoRoot walks up from this test file until it finds the ADR-002 marker, so
// the lookup cannot silently drift if the package moves deeper in the tree.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed")
	}
	marker := filepath.Join("docs", "adr", "002-groq-llm-provider.md")
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (no %s above %s)", marker, thisFile)
		}
		dir = parent
	}
}

func TestGroqContract_DocsAgreeOnCanonicalModelString(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range []string{"README.md", filepath.Join("docs", "adr", "002-groq-llm-provider.md")} {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if !strings.Contains(string(raw), canonicalModelString) {
			t.Fatalf("%s does not mention the canonical model %q; ADR-002 Option A requires docs and code to agree",
				rel, canonicalModelString)
		}
	}

	// ADR-002 must record the adjudicated decision on its Decision line, not
	// the stale string it originally contracted. The history mention inside
	// the adjudication paragraph is allowed, so check the Decision line only.
	adr, err := os.ReadFile(filepath.Join(root, "docs", "adr", "002-groq-llm-provider.md"))
	if err != nil {
		t.Fatalf("read ADR-002: %v", err)
	}
	decisionLine := ""
	for _, line := range strings.Split(string(adr), "\n") {
		if strings.Contains(line, "- Provider: Groq. Model:") {
			decisionLine = line
			break
		}
	}
	if decisionLine == "" {
		t.Fatalf("ADR-002 no longer has a '- Provider: Groq. Model:' decision line")
	}
	if !strings.Contains(decisionLine, canonicalModelString) {
		t.Fatalf("ADR-002 decision line = %q, want it to contract %q",
			strings.TrimSpace(decisionLine), canonicalModelString)
	}
}

func TestGroqContract_EmptyModelResolvesToCanonicalDefault(t *testing.T) {
	cap := runStub(t, respondOK(canonicalModelString))

	c, err := NewGroqModelClient(sentinelKey, "", cap.base)
	if err != nil {
		t.Fatalf("NewGroqModelClient: %v", err)
	}
	if c.model != canonicalModelString {
		t.Fatalf("resolved model = %q, want %q", c.model, canonicalModelString)
	}
	if _, err := c.Complete(context.Background(), groqEnvelopeRequest(t)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(cap.body, &sent); err != nil {
		t.Fatalf("outbound body not JSON: %v", err)
	}
	if sent.Model != canonicalModelString {
		t.Fatalf("outbound model = %q, want the canonical %q", sent.Model, canonicalModelString)
	}
}

func TestGroqContract_ModelEnvOverrideWins(t *testing.T) {
	cap := runStub(t, respondOK("some-other-model"))

	t.Setenv("GROQ_API_KEY", sentinelKey)
	t.Setenv("GROQ_MODEL", "some-other-model")
	t.Setenv("GROQ_BASE_URL", cap.base)

	c, err := NewGroqModelClientFromEnv()
	if err != nil {
		t.Fatalf("NewGroqModelClientFromEnv: %v", err)
	}
	if c.model != "some-other-model" {
		t.Fatalf("GROQ_MODEL did not win: model = %q", c.model)
	}
	if c.baseURL != cap.base {
		t.Fatalf("GROQ_BASE_URL did not win: baseURL = %q, want %q", c.baseURL, cap.base)
	}
	if _, err := c.Complete(context.Background(), groqEnvelopeRequest(t)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(cap.body, &sent); err != nil {
		t.Fatalf("outbound body not JSON: %v", err)
	}
	if sent.Model != "some-other-model" {
		t.Fatalf("outbound model = %q, want the GROQ_MODEL override", sent.Model)
	}
}

// ---------------------------------------------------------------------------
// No-key behavior: deterministic, fail-closed, zero egress
//
// TWO GUARDS, TWO OWNERS (APA-45 finding).
//
// groq_model.go rejects a blank key at two independent sites, with two
// different messages:
//
//	NewGroqModelClientFromEnv  ->  "groq: GROQ_API_KEY not set: %w"
//	NewGroqModelClient        ->  "groq: API key is required: %w"
//
// NewGroqModelClientFromEnv short-circuits before delegating, so on the env
// path the env guard always reports first and the constructor guard is never
// reached. The constructor guard is therefore NOT redundant: it owns every
// direct NewGroqModelClient call, and it is the only thing standing between a
// blank key and a live request if the env guard is ever removed or reordered.
//
// Consequence for this suite, learned from mutation MUT-5 (removing the env
// guard): with the env guard deleted the call still fails closed, but it fails
// closed *at the constructor*, with different wording. So a fail-closed-only
// assertion does NOT catch removal of either guard on its own, and a wording
// assertion that is not tied to its site can pass for the wrong reason. The
// tests below are split so each site is pinned where it actually owns
// behavior:
//
//	TestGroqContract_NoKeyFailsClosedDeterministically
//	    pins the INVARIANT (nil client, ErrModelContract, deterministic, zero
//	    egress, no key material) with no wording dependence, then positively
//	    identifies the env guard as the reporting site.
//	TestGroqContract_ConstructorGuardsBlankKeyIndependently
//	    pins the constructor guard, which no other test here exercises.
//
// Production wording is deliberately NOT changed to satisfy a test. Where
// wording is asserted it is asserted as site-ownership evidence and is labelled
// as such; the load-bearing assertions are the structural ones above it.
// ---------------------------------------------------------------------------

// Guard markers, duplicated as literals on purpose so the assertions pin the
// owning site rather than whatever the current implementation happens to say.
const (
	envGuardMarker  = "GROQ_API_KEY not set"
	ctorGuardMarker = "API key is required"
)

func TestGroqContract_NoKeyFailsClosedDeterministically(t *testing.T) {
	// Point the base URL at a loopback stub: if a no-key client ever tried to
	// call out, the hit count would be non-zero.
	cap := runStub(t, respondOK(canonicalModelString))

	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("GROQ_BASE_URL", cap.base)

	c, err := NewGroqModelClientFromEnv()

	// --- INVARIANT: the fail-closed guarantee, with no wording dependence. ---
	if err == nil {
		t.Fatalf("no-key construction succeeded; want fail-closed")
	}
	if c != nil {
		t.Fatalf("no-key construction returned a non-nil client: %#v", c)
	}
	if !errors.Is(err, ErrModelContract) {
		t.Fatalf("error = %v, want ErrModelContract", err)
	}
	// Deterministic and free of key-shaped material.
	_, err2 := NewGroqModelClientFromEnv()
	if err.Error() != err2.Error() {
		t.Fatalf("no-key error not deterministic: %q vs %q", err.Error(), err2.Error())
	}
	if strings.Contains(err.Error(), "gsk-") {
		t.Fatalf("no-key error carries key-shaped material: %q", err.Error())
	}
	if cap.hits != 0 {
		t.Fatalf("no-key path performed %d upstream calls, want 0", cap.hits)
	}

	// --- SITE OWNERSHIP (diagnostic, not the fail-closed proof above). -------
	// The env entry point must report from its own guard and must not delegate
	// a blank key down to the constructor guard. Asserted in both directions so
	// the check cannot pass on a marker shared by, or inherited from, another
	// site. This is the assertion that owns the MUT-5 mutation.
	msg := err.Error()
	if !strings.Contains(msg, envGuardMarker) {
		t.Errorf("env no-key error = %q, want the NewGroqModelClientFromEnv guard (%q)", msg, envGuardMarker)
	}
	if strings.Contains(msg, ctorGuardMarker) {
		t.Errorf("env no-key error = %q was reported by NewGroqModelClient's guard; the env guard did not short-circuit", msg)
	}
}

func TestGroqContract_ConstructorGuardsBlankKeyIndependently(t *testing.T) {
	// The direct-constructor guard. Nothing else in this file reaches it: the
	// env path short-circuits above it, so deleting this guard is invisible to
	// TestGroqContract_NoKeyFailsClosedDeterministically. It is pinned here so
	// both no-key sites are covered on their own terms.
	//
	// A blank model/base URL resolve to the production defaults here, so this
	// call is construction-only: no request is ever attempted, and the real
	// Groq base URL is never dialled by this suite.
	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"space", " "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"crlf", "\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewGroqModelClient(tc.key, "", "")
			if err == nil {
				t.Fatalf("blank key %q accepted; want fail-closed", tc.key)
			}
			// --- INVARIANT: fail-closed, no wording dependence. ---
			if c != nil {
				t.Fatalf("blank key %q returned a non-nil client: %#v", tc.key, c)
			}
			if !errors.Is(err, ErrModelContract) {
				t.Fatalf("error = %v, want ErrModelContract", err)
			}
			if strings.Contains(err.Error(), "gsk-") {
				t.Fatalf("error carries key-shaped material: %q", err.Error())
			}
			// --- SITE OWNERSHIP: a distinct site from the env guard. --------
			msg := err.Error()
			if !strings.Contains(msg, ctorGuardMarker) {
				t.Errorf("constructor error = %q, want the NewGroqModelClient guard (%q)", msg, ctorGuardMarker)
			}
			if strings.Contains(msg, envGuardMarker) {
				t.Errorf("constructor error = %q came from the env guard; the direct constructor did not guard the key itself", msg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Key injection and no-leak at the provider boundary
// ---------------------------------------------------------------------------

func TestGroqContract_KeySentOnlyInAuthorizationHeader(t *testing.T) {
	cap := runStub(t, respondOK(canonicalModelString))

	c, err := NewGroqModelClient(sentinelKey, canonicalModelString, cap.base)
	if err != nil {
		t.Fatalf("NewGroqModelClient: %v", err)
	}
	resp, err := c.Complete(context.Background(), groqEnvelopeRequest(t))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if cap.hits != 1 {
		t.Fatalf("stub hits = %d, want 1", cap.hits)
	}
	if cap.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", cap.method)
	}
	if cap.path != "/chat/completions" {
		t.Fatalf("path = %q, want /chat/completions", cap.path)
	}
	// Injected.
	if want := "Bearer " + sentinelKey; cap.auth != want {
		t.Fatalf("Authorization = %q, want %q", cap.auth, want)
	}
	// Not in the body, and not in any other header.
	assertNoKey(t, "outbound body", string(cap.body))
	for name, values := range cap.header {
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		for _, v := range values {
			assertNoKey(t, "outbound header "+name, v)
		}
	}
	// Not echoed into the decoded response.
	assertNoKey(t, "response payload", string(resp.Payload))
	assertNoKey(t, "response ModelID", resp.ModelID)
}

func TestGroqContract_KeyAbsentFromEveryErrorPath(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"terminal 4xx", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request_error","code":"invalid"}}`))
		}},
		{"exhausted 5xx", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server","code":"internal"}}`))
		}},
		{"empty choices", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"id":"x","model":"m","choices":[]}`))
		}},
		{"blank content", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"id":"x","model":"m","choices":[{"message":{"content":"   "}}]}`))
		}},
		{"malformed json", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{not json`))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := runStub(t, tc.handler)
			c, err := NewGroqModelClient(sentinelKey, canonicalModelString, cap.base)
			if err != nil {
				t.Fatalf("NewGroqModelClient: %v", err)
			}
			if _, err := c.Complete(context.Background(), groqEnvelopeRequest(t)); err == nil {
				t.Fatalf("expected an error from %s", tc.name)
			} else {
				assertNoKey(t, "error from "+tc.name, err.Error())
			}
		})
	}
}

func TestGroqContract_KeyAbsentOnTransportFailure(t *testing.T) {
	// A closed loopback port: the request never reaches any server.
	c, err := NewGroqModelClient(sentinelKey, canonicalModelString, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("NewGroqModelClient: %v", err)
	}
	_, err = c.Complete(context.Background(), groqEnvelopeRequest(t))
	if err == nil {
		t.Fatalf("expected a transport error against a closed port")
	}
	if !errors.Is(err, ErrModelUpstream) {
		t.Fatalf("transport error = %v, want ErrModelUpstream", err)
	}
	assertNoKey(t, "transport error", err.Error())
}

func TestGroqContract_CancellationIsRawAndKeyFree(t *testing.T) {
	cap := runStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server","code":"internal"}}`))
	})
	c, err := NewGroqModelClient(sentinelKey, canonicalModelString, cap.base)
	if err != nil {
		t.Fatalf("NewGroqModelClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Complete(ctx, groqEnvelopeRequest(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Complete = %v, want raw context.Canceled", err)
	} else {
		assertNoKey(t, "cancelled error", err.Error())
	}
}
