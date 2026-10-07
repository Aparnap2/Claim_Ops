package orchestrate

// APA-58: alternate-provider experiment against Poolside/Laguna S-2.1.
//
// WHY THIS EXISTS
// The APA-55 causal re-measurement is blocked on Groq capacity: the provider
// meters max_tokens as reserved against an 8000-token window, so the
// production shape (a ~3k-token prompt plus MaxTokens=4096) cannot be served
// reliably. Poolside is available and exposes an OpenAI-compatible surface,
// so the SAME experiment can be run against a second conforming provider
// without touching production configuration.
//
// WHAT IS AND IS NOT CHANGED
// Nothing in production. This file is test-only and adds no production type.
// The prompt is the identical RenderPrompt output, the decoder is the same
// DecodeModelAction, the validator is the same ValidateModelAction, the tools
// and budgets are the same, and the invariants are the same requireHeld
// assertions. The ONLY differences are the endpoint, the model id, and
// chat_template_kwargs.enable_thinking=false.
//
// ADR-002 is untouched and the production Groq selection is untouched. This
// is an EXPERIMENTAL measurement, recorded separately from every Groq
// verdict, never a substitute for one.
//
// EVIDENCE SEPARATION
// Scenario names are prefixed "ps_" so the APA-57 gate can declare this
// matrix independently of the Groq one. That is deliberate: filing a Poolside
// result under a Groq scenario name would make the qualification ledger
// ambiguous about which provider produced which measurement.
//
// READING THE OUTCOME
//   control reaches REPORT_READY -> the APA-56 correction is supported on a
//                                  second provider, which strengthens it
//   same additive denial        -> a ClaimOps contract issue, not a Qwen
//                                  quirk: the fix would be insufficient
//   cannot satisfy the contract -> the adapter boundary or the model-facing
//                                  contract needs examination
//
// Provider condition is recorded in every record, and a provider 429 is never
// counted as a measurement (qualMeasure discards and re-measures it).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	poolsideBaseURL = "https://inference.poolside.ai/v1"
	poolsideModel   = "poolside/laguna-s-2.1"
	// poolsideThinkingDisabled mirrors the user-supplied configuration:
	// Poolside enables thinking by DEFAULT, so the experiment must disable it
	// explicitly or it is not measuring the same runtime shape ClaimOps asks
	// for. Verified live: reasoning_tokens is 0 when this is sent.
	poolsideThinkingDisabled = false
	// poolsideMaxTokens mirrors the production client's fixed reservation.
	poolsideMaxTokens = 4096
)

// poolsideChatRequest is the Poolside wire shape.
//
// max_completion_tokens is deliberately ABSENT. Poolside's OpenAI-compatible
// layer rejects the two together with HTTP 400 "Extra inputs are not
// permitted"; max_tokens alone is accepted, so the adapter keeps ClaimOps'
// production parameter name and adds nothing.
type poolsideChatRequest struct {
	Model       string             `json:"model"`
	Messages    []poolsideMessage  `json:"messages"`
	Temperature float64            `json:"temperature"`
	MaxTokens   int                `json:"max_tokens"`
	Stream      bool               `json:"stream"`
	ChatTmpl    poolsideChatTmplKw `json:"chat_template_kwargs"`
}

type poolsideChatTmplKw struct {
	EnableThinking bool `json:"enable_thinking"`
}

type poolsideMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type poolsideChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// poolsideClient is a test-only ModelClient. It is deliberately NOT placed in
// production code: provider selection is ADR-002's decision, and an adapter
// that exists only to run one experiment must not become a de facto second
// production provider.
type poolsideClient struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// Complete renders the identical prompt and decodes the identical response
// shape as the production Groq client, with one added Poolside field.
// Error classification mirrors production so the harness classifies outcomes
// the same way for both providers: retryable status -> ErrModelUpstream,
// terminal 4xx -> ErrModelContract, no choices/empty content -> ErrModelEmpty.
func (p *poolsideClient) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return ModelResponse{}, err
	}
	prompt, err := RenderPrompt(req)
	if err != nil {
		return ModelResponse{}, err
	}
	body, err := json.Marshal(poolsideChatRequest{
		Model:       p.model,
		Messages:    []poolsideMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		MaxTokens:   poolsideMaxTokens,
		ChatTmpl:    poolsideChatTmplKw{EnableThinking: poolsideThinkingDisabled},
	})
	if err != nil {
		return ModelResponse{}, fmt.Errorf("poolside: marshal: %w: %w", err, ErrModelContract)
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ModelResponse{}, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			p.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return ModelResponse{}, fmt.Errorf("poolside: new request: %w: %w", err, ErrModelContract)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

		resp, err := p.client.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return ModelResponse{}, ctx.Err()
			}
			lastErr = fmt.Errorf("poolside: do: %w: %w", err, ErrModelUpstream)
			continue
		}
		func() {
			defer resp.Body.Close()
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			var pr poolsideChatResponse
			decErr := json.Unmarshal(raw, &pr)
			if isRetryableStatus(resp.StatusCode) {
				if decErr != nil || readErr != nil {
					lastErr = fmt.Errorf("poolside: status %d: %w", resp.StatusCode, ErrModelUpstream)
					return
				}
				lastErr = fmt.Errorf("poolside: status %d: %w", resp.StatusCode, ErrModelUpstream)
				return
			}
			if decErr != nil {
				lastErr = fmt.Errorf("poolside: decode: %v: %w", decErr, ErrModelContract)
				return
			}
			if resp.StatusCode >= 400 {
				msg := "unknown"
				if pr.Error != nil && pr.Error.Message != "" {
					msg = pr.Error.Message
				}
				lastErr = fmt.Errorf("poolside: status %d: %s: %w", resp.StatusCode, msg, ErrModelContract)
				return
			}
			if len(pr.Choices) == 0 {
				lastErr = fmt.Errorf("poolside: no choices: %w", ErrModelEmpty)
				return
			}
			content := pr.Choices[0].Message.Content
			if strings.TrimSpace(content) == "" {
				lastErr = fmt.Errorf("poolside: empty content: %w", ErrModelEmpty)
				return
			}
			modelID := pr.Model
			if modelID == "" {
				modelID = p.model
			}
			lastErr = nil
			respPayload = ModelResponse{Payload: []byte(content), ModelID: modelID}
		}()
		if lastErr == nil {
			return respPayload, nil
		}
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return ModelResponse{}, lastErr
		}
	}
	return ModelResponse{}, fmt.Errorf("%w: exhausted", lastErr)
}

// respPayload carries the decoded result out of the closure without letting a
// partially-decoded response escape.
var respPayload ModelResponse

// requireLivePoolside builds the experiment seam: the test-only adapter, the
// shared wire recorder, and one fail-fast probe so a bad credential or an
// unreachable provider is reported as infrastructure rather than as model
// variability. The probe records the model the provider ACTUALLY served, so
// the evidence cannot be misattributed.
// psLiveOptIn is the single switch that decides whether the Poolside matrix
// runs at all.
//
// An earlier revision skipped per-CASE when POOLSIDE_API_KEY was unset, so a
// full-suite run without that key left an empty TestAPA58_ScenarioFixtures_Matrix
// parent that still FAILED. A subtest group with no subtests does not pass
// vacuously; it fails as malformed. The consequence was the worst possible
// shape: the deterministic PG-only gate, which must be independently valid,
// reported exit 1 for a reason that had nothing to do with Postgres.
//
// Skipping now happens once, at the parent, so a run without credentials
// self-skips as a unit and PG-only qualification stays valid on its own terms.
var psLiveOptIn = func() bool {
	return strings.TrimSpace(os.Getenv("POOLSIDE_API_KEY")) != ""
}

func requireLivePoolside(t *testing.T) (*qualModel, *qualWireRecorder) {
	t.Helper()
	// Defensive: the matrix opts in at the parent, so reaching here without a
	// key means a new call site bypassed the switch.
	if !psLiveOptIn() {
		t.Skip("POOLSIDE_API_KEY unset; the alternate-provider experiment requires live inference")
	}
	pc := &poolsideClient{
		apiKey:  os.Getenv("POOLSIDE_API_KEY"),
		model:   poolsideModel,
		baseURL: poolsideBaseURL,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
	rec := &qualWireRecorder{}
	pc.client.Transport = &qualTransport{base: pc.client.Transport, rec: rec}

	m := &qualModel{inner: pc, rec: rec, provider: "poolside"}
	t.Cleanup(func() { pc.client.CloseIdleConnections() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	env := testEnvelope(t)
	scope := testScope(env)
	probeResp, err := m.Complete(ctx, ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{"ev-doc-01"},
		Turn:             1,
		RequestID:        scope.RequestID,
	})
	if err != nil {
		if rec.throttledSince(rec.mark()) {
			t.Skipf("provider rate-limited the alternate-provider probe (HTTP 429); "+
				"this is provider availability, not a model result: %v", err)
		}
		t.Fatalf("poolside probe failed (credential/provider/infrastructure, not model quality): %v", err)
	}
	// Record the model the provider ACTUALLY served, read from its own
	// response, so the evidence cannot be attributed to the requested id.
	// requireLiveGroq does this for Groq; without it qualModelID() returns
	// empty and the ledger loses the served-model fact entirely.
	if strings.TrimSpace(probeResp.ModelID) == "" {
		t.Fatal("alternate-provider probe returned an empty served model id")
	}
	qualProbeMu.Lock()
	qualProbeVal = qualProbe{ModelID: probeResp.ModelID, Provider: "poolside"}
	qualProbeMu.Unlock()
	t.Logf("APA58-PROBE provider=poolside model_requested=%s model_served=%s thinking=disabled",
		poolsideModel, probeResp.ModelID)
	return m, rec
}

// The unseeded six-case reproduction that used to live here was REMOVED.
//
// It called runLive for every case, and runLive always builds the identical
// testEnvelope, so it reintroduced precisely the label-only duplication APA-58
// exists to eliminate — while appearing to run the same six scenarios the
// seeded matrix runs. Two matrices, one of them vacuous, is worse than one:
// the vacuous one reports green and looks like evidence.
//
// The seeded matrix in apa58_scenario_fixtures_test.go is the single canonical
// path: builder -> premise assertion -> runLiveSeeded -> outcome assertion.

// TestAPA58_Poolside_ServesTheProductionShape proves the adapter can serve
// the production request shape at all, and records the served model id. It is
// the capacity precondition: without it, a matrix result would be
// indistinguishable from a quota problem.
func TestAPA58_Poolside_ServesTheProductionShape(t *testing.T) {
	m, _ := requireLivePoolside(t)
	env := testEnvelope(t)
	scope := testScope(env)
	resp, err := m.Complete(context.Background(), ModelRequest{
		Exception:        env,
		KnownEvidenceIDs: []string{"ev-doc-01"},
		Turn:             1,
		RequestID:        scope.RequestID,
	})
	if err != nil {
		t.Fatalf("production shape not served: %v", err)
	}
	t.Logf("APA58-SHAPE model_served=%s bytes=%d", resp.ModelID, len(resp.Payload))

	// The act must survive the SAME authoritative decoder and validator that
	// Groq output must. ValidateModelAction is what enforces tool ownership
	// against the scope (APA-54), so no separate tool lookup is needed.
	a, err := DecodeModelAction(resp.Payload, 1<<20)
	if err != nil {
		t.Fatalf("poolside output rejected by the shared decoder: %v", err)
	}
	if err := ValidateModelAction(a, scope, env.InvestigationID); err != nil {
		t.Fatalf("poolside output rejected by the shared validator: %v", err)
	}
	t.Logf("APA58-SHAPE action=%s tool=%q survives the shared decoder and validator", a.Action, a.Tool)
}
