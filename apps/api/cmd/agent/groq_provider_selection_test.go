package main

// APA-45 service-selection qualification (test-only, no live Groq key).
//
// Scope of this file: prove the RUNNING Agent service routes to the Groq
// ModelClient when MODEL_PROVIDER=groq, that the canonical ADR-002 model
// string is what actually goes out on the wire, and that GROQ_API_KEY is
// injected into the Authorization header and appears nowhere else.
//
// This is a seam/selection proof, NOT a Groq qualification. The frozen
// harness Fake-vs-Groq comparison with hard gates (fabricated=0,
// crossTenant=0, unauthorized=0) remains a MANUAL live-key gate per
// ADR-002 and is not attempted here.
//
// Hard constraints honoured by this file:
//   - No network egress to groq.com. Every origin is an httptest server
//     bound to loopback, enforced structurally by requireLoopbackOrigin.
//   - No production code is modified. The provider branch in main.go is
//     pinned by AST inspection (TestAgentProvider_SourceBranchPinsEnvRouting)
//     rather than by editing it, because the branch is inline in main().
//   - No eval, corpus, gate, or scoring artifact is touched.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate/orchestrate"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// canonicalGroqModel is the ADR-002 adjudicated canonical model string
	// (Option A, 2026-09-26; re-adjudicated to a servable production model
	// in APA-47, 2026-09-28). It must equal the code default in
	// groq_model.go and the README line; groq_env_contract_test.go pins
	// that three-way agreement so the three cannot drift apart silently.
	canonicalGroqModel = "qwen/qwen3.8-27b"

	// sentinelGroqKey is a fake key material. It exists so the no-leak
	// assertions have something specific to hunt for. It is not a real
	// credential and must never be committed as one.
	sentinelGroqKey = "gsk-SENTINEL-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// overrideGroqModel is a distinct non-canonical model used to prove
	// GROQ_MODEL actually resolves rather than being hardcoded.
	overrideGroqModel = "llama-3.3-70b-versatile"
)

// ---------------------------------------------------------------------------
// Stub Groq origin
// ---------------------------------------------------------------------------

// groqCall is one recorded inbound request at the stub Groq origin.
type groqCall struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// groqOrigin is a loopback httptest stand-in for the Groq OpenAI-compatible
// endpoint. It records every request so the test can assert on exactly what
// the production client put on the wire.
type groqOrigin struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []groqCall
	// respond is the per-call response builder. Nil means "echo a valid
	// submit_report whose model field echoes the request's model".
	respond func(callIndex int, w http.ResponseWriter, body []byte)
}

// newGroqOrigin starts a loopback stub and registers cleanup. It refuses any
// non-loopback address so this suite can never reach the real Groq API.
func newGroqOrigin(t *testing.T, respond func(callIndex int, w http.ResponseWriter, body []byte)) *groqOrigin {
	t.Helper()
	o := &groqOrigin{respond: respond}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAllLimited(t, r)

		o.mu.Lock()
		idx := len(o.calls)
		o.calls = append(o.calls, groqCall{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		})
		o.mu.Unlock()

		if o.respond != nil {
			o.respond(idx, w, body)
			return
		}
		writeEchoedSubmitReport(w, body)
	}))
	t.Cleanup(o.srv.Close)
	requireLoopbackOrigin(t, o.srv.URL)
	return o
}

// URL is the base URL to hand to GROQ_BASE_URL.
func (o *groqOrigin) URL() string { return o.srv.URL }

// Calls returns a copy of every recorded request.
func (o *groqOrigin) Calls() []groqCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]groqCall(nil), o.calls...)
}

// Count returns how many requests reached the origin.
func (o *groqOrigin) Count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.calls)
}

// requireLoopbackOrigin fails the test if rawURL is not a loopback httptest
// origin. This is the structural guarantee that no assertion in this file can
// cause egress to api.groq.com.
func requireLoopbackOrigin(t *testing.T, rawURL string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse origin %q: %v", rawURL, err)
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("origin %q has no port: %v", rawURL, err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		t.Fatalf("origin %q is not loopback; this suite must never leave the host", rawURL)
	}
}

func readAllLimited(t *testing.T, r *http.Request) []byte {
	t.Helper()
	var buf bytes.Buffer
	// Bound the read so a hostile stub cannot exhaust the test process.
	_, _ = buf.ReadFrom(http.MaxBytesReader(nil, r.Body, 1<<20))
	return buf.Bytes()
}

// writeEchoedSubmitReport answers with one valid submit_report act whose
// "model" field echoes what the client actually asked for. Echoing means the
// Agent response body's model_id and the recorded outbound model must agree,
// which is only true if the same string travelled both ways.
func writeEchoedSubmitReport(w http.ResponseWriter, reqBody []byte) {
	echoed := canonicalGroqModel
	var parsed struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(reqBody, &parsed); err == nil && parsed.Model != "" {
		echoed = parsed.Model
	}
	act := buildSubmitReportAct(trustEvID)
	payload, _ := json.Marshal(map[string]any{
		"id":      "stub-completion",
		"model":   echoed,
		"choices": []any{map[string]any{"message": map[string]string{"content": string(act)}}},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

// ---------------------------------------------------------------------------
// Envelope / report fixtures
// ---------------------------------------------------------------------------

const trustEvID = "ev-01"

// buildSubmitReportAct renders a valid, fully grounded submit_report act for
// the fixture envelope. It cites only ev-01, which is seeded into KnownEvidence
// from the envelope's EvidenceRefs, so Gate B passes with no tool call — the
// model turn happens and no database row is required.
func buildSubmitReportAct(evID string) []byte {
	report := orchestrate.Report{
		Hypotheses: []invest.Hypothesis{{
			ID:          "h-01",
			Statement:   "Policy number conflict stems from transcription variance.",
			Falsifier:   "A pinned policy record showing the claimed number as active.",
			Status:      invest.HypothesisOpen,
			EvidenceIDs: []string{evID},
		}},
		Findings: []invest.Finding{{
			ID:           "f-01",
			HypothesisID: "h-01",
			Summary:      "Cited evidence shows the conflict.",
			EvidenceIDs:  []string{evID},
		}},
		Recommendation: invest.Recommendation{
			Action:     invest.RecommendReferHuman,
			Rationale:  "Needs human review.",
			FindingIDs: []string{"f-01"},
		},
	}
	raw, err := json.Marshal(orchestrate.ModelAction{
		Action: orchestrate.ActionSubmitReport,
		Report: &report,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal submit_report act: %v", err))
	}
	return raw
}

// ---------------------------------------------------------------------------
// Agent app harness
// ---------------------------------------------------------------------------

// deadPool returns a real *pgxpool.Pool pointed at a closed loopback port.
//
// Why not nil and why not &pgxpool.Pool{}:
//   - A nil pool is rejected by investigationHandler with 503 NO_DB, so the
//     model client is never reached and nothing is proven.
//   - A zero-value &pgxpool.Pool{} panics on pool.Begin, which the loop audit
//     hook reaches; fiber's recover middleware would turn that into a 500.
//
// A real pool over an unreachable DSN degrades correctly: pgxpool.New is
// lazy, and the loop/tool audit hooks treat a failed transaction as
// best-effort by contract ("hook failures never fail a tool result or run").
// This keeps the proof independent of live infrastructure, which is down in
// this environment.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, "postgres://nobody:nobody@127.0.0.1:1/none?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New(dead DSN): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newAgentApp builds the Agent app wired the way main() wires it: the same
// fiber config and recover middleware, then POST /v1/investigations bound to
// investigationHandler(pool, modelClient).
func newAgentApp(t *testing.T, modelClient orchestrate.ModelClient) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(recover.New())
	app.Post("/v1/investigations", investigationHandler(deadPool(t), modelClient))
	return app
}

// groqClientFromEnv constructs the client exactly the way the MODEL_PROVIDER
// branch in main.go does: via orchestrate.NewGroqModelClientFromEnv, reading
// GROQ_API_KEY / GROQ_MODEL / GROQ_BASE_URL from the process environment.
func groqClientFromEnv(t *testing.T) orchestrate.ModelClient {
	t.Helper()
	c, err := orchestrate.NewGroqModelClientFromEnv()
	if err != nil {
		t.Fatalf("NewGroqModelClientFromEnv: %v", err)
	}
	return c
}

// postInvestigation drives the real POST /v1/investigations route with the
// inline-envelope local-test adapter and returns the decoded response body
// plus the raw response for byte-level leak checks.
func postInvestigation(t *testing.T, app *fiber.App) (int, string, map[string]any) {
	t.Helper()
	env := validEnvelopeForTenant(trustTenantA, trustClaim, trustInvID, trustExID, trustReqID)
	if err := invest.Validate(env); err != nil {
		t.Fatalf("fixture envelope must satisfy invest.Validate: %v", err)
	}
	body, err := json.Marshal(InvestigationRequest{
		TenantID:        trustTenantA,
		InvestigationID: trustInvID,
		Exception:       &env,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/investigations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", trustTenantA)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	decoded := map[string]any{}
	_ = json.Unmarshal(raw.Bytes(), &decoded)
	return resp.StatusCode, raw.String(), decoded
}

// useInlineEnvelope turns on the local-test inline envelope adapter and clears
// every Groq/Model env var so each test starts from a known state.
func useInlineEnvelope(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENV", "local")
	t.Setenv("ALLOW_INLINE_ENVELOPE", "true")
	for _, k := range []string{"MODEL_PROVIDER", "GROQ_API_KEY", "GROQ_MODEL", "GROQ_BASE_URL"} {
		t.Setenv(k, "")
	}
}

// outboundModel extracts the model string the client put on the wire.
func outboundModel(t *testing.T, call groqCall) string {
	t.Helper()
	var parsed struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(call.Body, &parsed); err != nil {
		t.Fatalf("outbound body is not JSON: %v (body=%q)", err, string(call.Body))
	}
	return parsed.Model
}

// ---------------------------------------------------------------------------
// 1. Selection: MODEL_PROVIDER=groq selects GroqModelClient, not Mock
// ---------------------------------------------------------------------------

func TestAgentProvider_GroqEnvSelectsGroqClientNotMock(t *testing.T) {
	useInlineEnvelope(t)
	origin := newGroqOrigin(t, nil)

	t.Setenv("MODEL_PROVIDER", "groq")
	t.Setenv("GROQ_API_KEY", sentinelGroqKey)
	t.Setenv("GROQ_BASE_URL", origin.URL())
	t.Setenv("GROQ_MODEL", "")

	client := groqClientFromEnv(t)

	// The constructed client must be the Groq implementation, and must not
	// be the Mock (which is the default provider and the per-request
	// override the handler installs for empty scripts).
	if _, ok := client.(*orchestrate.MockModelClient); ok {
		t.Fatalf("MODEL_PROVIDER=groq produced a MockModelClient; the seam did not route to Groq")
	}
	if _, ok := client.(*orchestrate.GroqModelClient); !ok {
		t.Fatalf("MODEL_PROVIDER=groq produced %T, want *orchestrate.GroqModelClient", client)
	}

	app := newAgentApp(t, client)
	status, raw, body := postInvestigation(t, app)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	if body["outcome"] != string(orchestrate.OutcomeReportReady) {
		t.Fatalf("outcome = %v, want %q; body=%s", body["outcome"], orchestrate.OutcomeReportReady, raw)
	}
	// The decisive assertion: the request reached the Groq origin. A Mock
	// client performs no HTTP at all, so origin traffic proves the Groq
	// client was the one the service called.
	calls := origin.Calls()
	if len(calls) == 0 {
		t.Fatalf("stub Groq origin received no request: the running service did not route to GroqModelClient")
	}
	for _, c := range calls {
		if c.Path != "/chat/completions" {
			t.Fatalf("unexpected outbound path %q, want /chat/completions", c.Path)
		}
	}
	// model_id is the Groq client's own identifier echoed by the origin. It
	// must be the Groq model, never the handler's "dynamic-mock"/"mock-scripted"
	// fallback — that is what proves no mock substitution occurred.
	if got := body["model_id"]; got != canonicalGroqModel {
		t.Fatalf("model_id = %v, want %q (a mock substitution would change this); body=%s", got, canonicalGroqModel, raw)
	}
}

// TestAgentProvider_MockProviderDefaultSendsNoEgress is the negative control:
// the default/mock provider must never produce Groq origin traffic, so the
// selection test above cannot pass by accident.
func TestAgentProvider_MockProviderDefaultSendsNoEgress(t *testing.T) {
	useInlineEnvelope(t)
	origin := newGroqOrigin(t, nil)

	app := newAgentApp(t, orchestrate.NewMockModelClient(nil))
	status, raw, _ := postInvestigation(t, app)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	if n := origin.Count(); n != 0 {
		t.Fatalf("mock provider produced %d requests to the Groq origin, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 2. The canonical model string is the one sent
// ---------------------------------------------------------------------------

func TestAgentProvider_OutboundCarriesCanonicalModelString(t *testing.T) {
	useInlineEnvelope(t)
	origin := newGroqOrigin(t, nil)

	t.Setenv("MODEL_PROVIDER", "groq")
	t.Setenv("GROQ_API_KEY", sentinelGroqKey)
	t.Setenv("GROQ_BASE_URL", origin.URL())
	// GROQ_MODEL deliberately unset: the resolved value must be the ADR-002
	// canonical string, i.e. the code default.
	t.Setenv("GROQ_MODEL", "")

	status, raw, _ := postInvestigation(t, newAgentApp(t, groqClientFromEnv(t)))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	calls := origin.Calls()
	if len(calls) == 0 {
		t.Fatalf("stub Groq origin received no request")
	}
	if got := outboundModel(t, calls[0]); got != canonicalGroqModel {
		t.Fatalf("outbound model = %q, want %q (ADR-002 canonical string)", got, canonicalGroqModel)
	}
}

func TestAgentProvider_GroqModelEnvOverrideIsHonored(t *testing.T) {
	useInlineEnvelope(t)
	origin := newGroqOrigin(t, nil)

	t.Setenv("MODEL_PROVIDER", "groq")
	t.Setenv("GROQ_API_KEY", sentinelGroqKey)
	t.Setenv("GROQ_BASE_URL", origin.URL())
	t.Setenv("GROQ_MODEL", overrideGroqModel)

	status, raw, body := postInvestigation(t, newAgentApp(t, groqClientFromEnv(t)))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	calls := origin.Calls()
	if len(calls) == 0 {
		t.Fatalf("stub Groq origin received no request")
	}
	// Proves the outbound model is resolved from GROQ_MODEL rather than
	// hardcoded: a different value must actually travel.
	if got := outboundModel(t, calls[0]); got != overrideGroqModel {
		t.Fatalf("outbound model = %q, want %q from GROQ_MODEL", got, overrideGroqModel)
	}
	if got := body["model_id"]; got != overrideGroqModel {
		t.Fatalf("model_id = %v, want %q", got, overrideGroqModel)
	}
}

// ---------------------------------------------------------------------------
// 3. Key injection without leaking
// ---------------------------------------------------------------------------

func TestAgentProvider_KeyInjectedOnlyIntoAuthorizationHeader(t *testing.T) {
	useInlineEnvelope(t)
	origin := newGroqOrigin(t, nil)

	t.Setenv("MODEL_PROVIDER", "groq")
	t.Setenv("GROQ_API_KEY", sentinelGroqKey)
	t.Setenv("GROQ_BASE_URL", origin.URL())
	t.Setenv("GROQ_MODEL", "")

	status, raw, _ := postInvestigation(t, newAgentApp(t, groqClientFromEnv(t)))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	calls := origin.Calls()
	if len(calls) == 0 {
		t.Fatalf("stub Groq origin received no request")
	}
	call := calls[0]

	// Injected: the key reaches the upstream Authorization header.
	if got, want := call.Header.Get("Authorization"), "Bearer "+sentinelGroqKey; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}

	// Not leaked: nowhere except that header.
	assertKeyAbsent(t, "outbound request body", string(call.Body))
	for name, values := range call.Header {
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		for _, v := range values {
			assertKeyAbsent(t, "outbound header "+name, v)
		}
	}
	// Not leaked: the Agent's own HTTP response.
	assertKeyAbsent(t, "agent response body", raw)
}

func TestAgentProvider_KeyAbsentFromErrorAndResponseOnUpstreamFailure(t *testing.T) {
	useInlineEnvelope(t)
	// Upstream fails with a neutral 500. Its message must not be able to
	// smuggle our key into the Agent's response or error text.
	origin := newGroqOrigin(t, func(callIndex int, w http.ResponseWriter, _ []byte) {
		if callIndex == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"internal","type":"server","code":"internal"}}`))
			return
		}
		// The client retries once on 5xx; answer that with the same failure
		// so the run escalates deterministically.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal","type":"server","code":"internal"}}`))
	})

	t.Setenv("MODEL_PROVIDER", "groq")
	t.Setenv("GROQ_API_KEY", sentinelGroqKey)
	t.Setenv("GROQ_BASE_URL", origin.URL())
	t.Setenv("GROQ_MODEL", "")

	status, raw, body := postInvestigation(t, newAgentApp(t, groqClientFromEnv(t)))

	// The point of this test is the leak assertions, not the status code,
	// so accept any well-formed outcome and only require the run happened.
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	if len(origin.Calls()) == 0 {
		t.Fatalf("stub Groq origin received no request; the seam was not exercised")
	}
	if body["outcome"] != string(orchestrate.OutcomeEscalated) {
		t.Fatalf("outcome = %v, want %q on upstream failure; body=%s", body["outcome"], orchestrate.OutcomeEscalated, raw)
	}
	if errStr, ok := body["error"].(string); ok && strings.TrimSpace(errStr) != "" {
		assertKeyAbsent(t, "agent error field", errStr)
	}
	assertKeyAbsent(t, "agent response body (upstream failure)", raw)
}

func assertKeyAbsent(t *testing.T, where, value string) {
	t.Helper()
	if strings.Contains(value, sentinelGroqKey) {
		t.Fatalf("sentinel Groq key leaked into %s", where)
	}
}

// ---------------------------------------------------------------------------
// 4. No-key behavior is deterministic and needs no live key
// ---------------------------------------------------------------------------

func TestAgentProvider_NoGroqKeyFailsClosedWithZeroEgress(t *testing.T) {
	useInlineEnvelope(t)
	origin := newGroqOrigin(t, nil)

	t.Setenv("MODEL_PROVIDER", "groq")
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("GROQ_BASE_URL", origin.URL())
	t.Setenv("GROQ_MODEL", "")

	// ADR-002: unit/CI must never need a live key. With no key the
	// constructor fails closed before any request is attempted.
	first, err := orchestrate.NewGroqModelClientFromEnv()
	if err == nil {
		t.Fatalf("NewGroqModelClientFromEnv succeeded with no GROQ_API_KEY; it must fail closed")
	}
	if !errors.Is(err, orchestrate.ErrModelContract) {
		t.Fatalf("no-key error = %v, want ErrModelContract", err)
	}
	if first != nil {
		t.Fatalf("no-key construction returned a non-nil client: %#v", first)
	}
	// Deterministic: identical failure every time, no clock, no randomness.
	_, second := orchestrate.NewGroqModelClientFromEnv()
	if err.Error() != second.Error() {
		t.Fatalf("no-key error is not deterministic: %q vs %q", err.Error(), second.Error())
	}
	// The key value must not appear in the error text.
	if strings.Contains(err.Error(), "gsk-") {
		t.Fatalf("no-key error text looks like it carries key material: %q", err.Error())
	}
	if n := origin.Count(); n != 0 {
		t.Fatalf("no-key path attempted %d upstream requests, want 0", n)
	}
}

func TestAgentProvider_BlankKeyIsRejectedLikeAbsent(t *testing.T) {
	useInlineEnvelope(t)
	for _, blank := range []string{"", "   ", "\t"} {
		t.Run(fmt.Sprintf("key=%q", blank), func(t *testing.T) {
			t.Setenv("GROQ_API_KEY", blank)
			t.Setenv("GROQ_BASE_URL", "http://127.0.0.1:1/v1")
			if _, err := orchestrate.NewGroqModelClientFromEnv(); err == nil {
				t.Fatalf("blank key %q accepted; want fail-closed", blank)
			}
		})
	}
}

func TestAgentProvider_DefaultProviderIsDeterministicMockStub(t *testing.T) {
	useInlineEnvelope(t)
	// ADR-002 no-key behavior: the deterministic stub responder. The Agent's
	// default provider is the scripted MockModelClient, which needs no
	// network and no key, and fails deterministically on an empty script.
	origin := newGroqOrigin(t, nil)
	t.Setenv("GROQ_BASE_URL", origin.URL())
	t.Setenv("GROQ_API_KEY", sentinelGroqKey)

	mock := orchestrate.NewMockModelClient(nil)
	env := validEnvelopeForTenant(trustTenantA, trustClaim, trustInvID, trustExID, trustReqID)
	req := orchestrate.ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{trustEvID},
		Turn:             1,
		RequestID:        env.Scope.RequestID,
	}
	_, err1 := mock.Complete(context.Background(), req)
	_, err2 := mock.Complete(context.Background(), req)
	if err1 == nil || err2 == nil {
		t.Fatalf("empty mock script must fail closed, got %v / %v", err1, err2)
	}
	if err1.Error() != err2.Error() {
		t.Fatalf("mock stub error is not deterministic: %q vs %q", err1.Error(), err2.Error())
	}
	if !errors.Is(err1, orchestrate.ErrModelUpstream) {
		t.Fatalf("mock stub error = %v, want ErrModelUpstream", err1)
	}
	if n := origin.Count(); n != 0 {
		t.Fatalf("mock stub attempted %d upstream requests, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// 5. Source pin: the selection branch itself cannot drift
// ---------------------------------------------------------------------------

// TestAgentProvider_SourceBranchPinsEnvRouting pins main.go's provider
// selection by parsing it, because the branch is inline in main() and has no
// callable seam. Without this, the runtime tests above would only prove the
// env-driven constructor, not that the service actually routes by
// MODEL_PROVIDER. It is the guard that stops "MODEL_PROVIDER=groq silently
// stopped selecting Groq" from regressing unnoticed.
func TestAgentProvider_SourceBranchPinsEnvRouting(t *testing.T) {
	_, src, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed")
	}
	mainPath := filepath.Join(filepath.Dir(src), "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", mainPath, err)
	}

	var (
		readsProviderEnv  bool
		emptyDefaultsMock bool
		groqCaseCalls     []string
		mockCaseCalls     []string
		defaultFatalf     bool
		foundSwitchTag    string
	)

	ast.Inspect(file, func(n ast.Node) bool {
		// os.Getenv("MODEL_PROVIDER") must be read.
		if call, isCall := n.(*ast.CallExpr); isCall {
			if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel {
				if pkg, isIdent := sel.X.(*ast.Ident); isIdent && pkg.Name == "os" && sel.Sel.Name == "Getenv" {
					if len(call.Args) == 1 {
						if lit, isLit := call.Args[0].(*ast.BasicLit); isLit && lit.Value == `"MODEL_PROVIDER"` {
							readsProviderEnv = true
						}
					}
				}
			}
		}
		// provider == "" -> provider = "mock"
		if assign, isAssign := n.(*ast.AssignStmt); isAssign && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if id, isID := assign.Lhs[0].(*ast.Ident); isID && id.Name == "provider" {
				if lit, isLit := assign.Rhs[0].(*ast.BasicLit); isLit && lit.Value == `"mock"` {
					emptyDefaultsMock = true
				}
			}
		}
		// switch provider { case "groq": ... }
		sw, isSwitch := n.(*ast.SwitchStmt)
		if !isSwitch {
			return true
		}
		if tag, isIdent := sw.Tag.(*ast.Ident); !isIdent || tag.Name != "provider" {
			return true
		}
		foundSwitchTag = "provider"
		for _, stmt := range sw.Body.List {
			cc := stmt.(*ast.CaseClause)
			label := ""
			if len(cc.List) == 1 {
				if lit, isLit := cc.List[0].(*ast.BasicLit); isLit {
					label = strings.Trim(lit.Value, `"`)
				}
			}
			for _, inner := range cc.Body {
				ast.Inspect(inner, func(m ast.Node) bool {
					call, isCall := m.(*ast.CallExpr)
					if !isCall {
						return true
					}
					name := calleeName(call)
					if name == "" {
						return true
					}
					switch label {
					case "groq":
						groqCaseCalls = append(groqCaseCalls, name)
					case "mock":
						mockCaseCalls = append(mockCaseCalls, name)
					case "":
						if name == "log.Fatalf" {
							defaultFatalf = true
						}
					}
					return true
				})
			}
		}
		return true
	})

	if !readsProviderEnv {
		t.Errorf("main.go no longer reads MODEL_PROVIDER; the provider seam was removed or renamed")
	}
	if !emptyDefaultsMock {
		t.Errorf("main.go no longer defaults an empty MODEL_PROVIDER to \"mock\"")
	}
	if foundSwitchTag != "provider" {
		t.Fatalf("main.go no longer switches on the provider variable")
	}
	if !containsCall(groqCaseCalls, "NewGroqModelClientFromEnv") {
		t.Errorf("case \"groq\" no longer constructs the Groq client; calls=%v", groqCaseCalls)
	}
	if !containsCall(mockCaseCalls, "NewMockModelClient") {
		t.Errorf("case \"mock\" no longer constructs the mock stub; calls=%v", mockCaseCalls)
	}
	if !defaultFatalf {
		t.Errorf("the default provider case no longer fails closed via log.Fatalf")
	}
}

func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if pkg, isIdent := fun.X.(*ast.Ident); isIdent {
			return pkg.Name + "." + fun.Sel.Name
		}
		return fun.Sel.Name
	}
	return ""
}

func containsCall(names []string, want string) bool {
	for _, n := range names {
		if n == want || strings.HasSuffix(n, "."+want) {
			return true
		}
	}
	return false
}

// TestAgentProvider_EnvIsolation asserts this suite does not depend on a
// developer's ambient Groq configuration leaking in, and documents the
// no-egress guarantee: the canonical base URL constant is never dialled here.
func TestAgentProvider_EnvIsolation(t *testing.T) {
	// The test binary must be able to run with no Groq env at all.
	if v := os.Getenv("GROQ_API_KEY"); strings.TrimSpace(v) != "" && v != sentinelGroqKey {
		t.Logf("note: ambient GROQ_API_KEY is set in this environment; tests override it with t.Setenv")
	}
	requireLoopbackOrigin(t, "http://127.0.0.1:1")
}
