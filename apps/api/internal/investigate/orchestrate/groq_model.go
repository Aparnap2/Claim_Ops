// GroqModelClient — OpenAI-compatible HTTP client for Groq.
// Orchestrator never imports this file's types; only ModelClient seam.
package orchestrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Groq defaults.
const (
	defaultGroqModel   = "llama-3.1-8b-instant"
	defaultGroqBaseURL = "https://api.groq.com/openai/v1"
	groqTimeout        = 30 * time.Second
)

// GroqModelClient implements ModelClient via Groq's OpenAI-compatible API.
type GroqModelClient struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

// NewGroqModelClient builds a Groq client. apiKey must be non-empty.
func NewGroqModelClient(apiKey, model, baseURL string) (*GroqModelClient, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("groq: API key is required: %w", ErrModelContract)
	}
	if strings.TrimSpace(model) == "" {
		model = defaultGroqModel
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultGroqBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &GroqModelClient{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		client:  &http.Client{Timeout: groqTimeout},
	}, nil
}

// NewGroqModelClientFromEnv reads GROQ_API_KEY, GROQ_MODEL, GROQ_BASE_URL.
func NewGroqModelClientFromEnv() (*GroqModelClient, error) {
	key := os.Getenv("GROQ_API_KEY")
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("groq: GROQ_API_KEY not set: %w", ErrModelContract)
	}
	return NewGroqModelClient(key, os.Getenv("GROQ_MODEL"), os.Getenv("GROQ_BASE_URL"))
}

type groqChatRequest struct {
	Model       string        `json:"model"`
	Messages    []groqMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream"`
}

type groqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type groqChatResponse struct {
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

// Complete renders the prompt and calls Groq. Never logs key or prompt.
// Retry policy: exactly one retry on transient transport errors, 408, 429, and 5xx; 4xx (400,401,403,404) and empty are terminal; cancellation returns raw context error.
func (g *GroqModelClient) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return ModelResponse{}, err
	}
	prompt, err := RenderPrompt(req)
	if err != nil {
		return ModelResponse{}, err
	}
	chatReq := groqChatRequest{
		Model:       g.model,
		Messages:    []groqMessage{{Role: "user", Content: prompt}},
		Temperature: 0,
		MaxTokens:   4096,
	}
	body, err := json.Marshal(chatReq)
	if err != nil {
		return ModelResponse{}, fmt.Errorf("groq: marshal: %w", err)
	}
	url := g.baseURL + "/chat/completions"
	var lastErr error
	var lastStatus int
	var result ModelResponse
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return ModelResponse{}, ctx.Err()
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return ModelResponse{}, fmt.Errorf("groq: request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)
		resp, err := g.client.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return ModelResponse{}, ctx.Err()
			}
			lastErr = fmt.Errorf("groq: do: %w: %w", err, ErrModelUpstream)
			lastStatus = 0
			if attempt == 0 && isGroqRetryable(lastErr, lastStatus) {
				select {
				case <-ctx.Done():
					return ModelResponse{}, ctx.Err()
				case <-time.After(groqBackoff(attempt)):
				}
				continue
			}
			return ModelResponse{}, lastErr
		}
		func() {
			defer resp.Body.Close()
			lastStatus = resp.StatusCode
			var gr groqChatResponse
			if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
				// Decode of 5xx body is retryable only if status was 5xx; otherwise contract.
				if isRetryableStatus(lastStatus) {
					lastErr = fmt.Errorf("groq: decode: %w: %w", err, ErrModelUpstream)
				} else {
					lastErr = fmt.Errorf("groq: decode: %w: %w", err, ErrModelContract)
				}
				return
			}
			if isRetryableStatus(lastStatus) {
				lastErr = fmt.Errorf("groq: status %d: %w", resp.StatusCode, ErrModelUpstream)
				return
			}
			if resp.StatusCode >= 400 {
				msg := "unknown"
				if gr.Error != nil && gr.Error.Message != "" {
					msg = gr.Error.Message
				}
				// 4xx is terminal contract error, not upstream.
				lastErr = fmt.Errorf("groq: status %d: %s: %w", resp.StatusCode, msg, ErrModelContract)
				return
			}
			if len(gr.Choices) == 0 {
				lastErr = fmt.Errorf("groq: no choices: %w", ErrModelEmpty)
				return
			}
			content := gr.Choices[0].Message.Content
			if strings.TrimSpace(content) == "" {
				lastErr = fmt.Errorf("groq: empty content: %w", ErrModelEmpty)
				return
			}
			modelID := gr.Model
			if modelID == "" {
				modelID = g.model
			}
			result = ModelResponse{Payload: []byte(content), ModelID: modelID}
			lastErr = nil
		}()
		if lastErr == nil {
			return result, nil
		}
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return ModelResponse{}, lastErr
		}
		if attempt == 0 && isGroqRetryable(lastErr, lastStatus) {
			select {
			case <-ctx.Done():
				return ModelResponse{}, ctx.Err()
			case <-time.After(groqBackoff(attempt)):
			}
			continue
		}
		// Exhausted retryable error: mark as exhausted so Loop does not retry again (bounded total 2).
		if isGroqRetryable(lastErr, lastStatus) {
			return ModelResponse{}, fmt.Errorf("%w: exhausted", lastErr)
		}
		return ModelResponse{}, lastErr
	}
	// Final fallback if loop exhausted (should not reach here, but handle).
	if lastErr != nil && isGroqRetryable(lastErr, lastStatus) {
		return ModelResponse{}, fmt.Errorf("%w: exhausted", lastErr)
	}
	return ModelResponse{}, lastErr
}

// isRetryableStatus reports whether an HTTP status is retryable at the provider layer.
func isRetryableStatus(code int) bool {
	if code == 408 || code == 429 {
		return true
	}
	if code >= 500 && code <= 599 {
		return true
	}
	return false
}

func isGroqRetryable(err error, status int) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "exhausted") {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrModelEmpty) || errors.Is(err, ErrModelContract) {
		return false
	}
	if errors.Is(err, ErrModelUpstream) {
		// If we have a status, use it; otherwise transport error is retryable.
		if status != 0 {
			return isRetryableStatus(status)
		}
		// No status (transport) -> retryable
		return true
	}
	// Fallback string check for wrapped errors without status
	if strings.Contains(msg, "status 5") || strings.Contains(msg, "status 408") || strings.Contains(msg, "status 429") {
		return true
	}
	return false
}

// groqBackoff returns a deterministic bounded backoff for attempt.
// Attempt 0 -> 10ms, 1 -> 20ms, capped at 50ms. No real long sleeps.
func groqBackoff(attempt int) time.Duration {
	if attempt < 0 {
		return 0
	}
	d := time.Duration(10*(attempt+1)) * time.Millisecond
	if d > 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	return d
}
