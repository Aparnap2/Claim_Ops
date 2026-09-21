// GroqModelClient — OpenAI-compatible HTTP client for Groq.
// Orchestrator never imports this file's types; only ModelClient seam.
package orchestrate

import (
	"bytes"
	"context"
	"encoding/json"
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
// Retry policy: exactly one retry on transient transport errors and 5xx; 4xx, empty, and decode of 4xx body are not retried.
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
	var result ModelResponse
	for attempt := 0; attempt < 2; attempt++ {
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
			if attempt == 0 {
				continue
			}
			return ModelResponse{}, lastErr
		}
		func() {
			defer resp.Body.Close()
			var gr groqChatResponse
			if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
				lastErr = fmt.Errorf("groq: decode: %w: %w", err, ErrModelUpstream)
				return
			}
			if resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("groq: status %d: %w", resp.StatusCode, ErrModelUpstream)
				return
			}
			if resp.StatusCode >= 400 {
				msg := "unknown"
				if gr.Error != nil && gr.Error.Message != "" {
					msg = gr.Error.Message
				}
				lastErr = fmt.Errorf("groq: status %d: %s: %w", resp.StatusCode, msg, ErrModelUpstream)
				return
			}
			if len(gr.Choices) == 0 {
				lastErr = fmt.Errorf("groq: no choices: %w", ErrModelUpstream)
				return
			}
			content := gr.Choices[0].Message.Content
			if strings.TrimSpace(content) == "" {
				lastErr = fmt.Errorf("groq: empty content: %w", ErrModelUpstream)
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
		if attempt == 0 && isGroqRetryable(lastErr, nil) {
			// Only retry on 5xx / transport / decode; not on 4xx/empty.
			// We already know 4xx produced lastErr with status 4, so isGroqRetryable will be false.
			continue
		}
		return ModelResponse{}, lastErr
	}
	return ModelResponse{}, lastErr
}

func isGroqRetryable(err error, _ *http.Response) bool {
	msg := err.Error()
	if strings.Contains(msg, "status 5") {
		return true
	}
	if strings.Contains(msg, "groq: do:") || strings.Contains(msg, "groq: decode:") {
		// Transport or decode of 5xx body is retryable; but 4xx decode is not.
		// We already excluded 4xx via status string, so treat transport as retryable.
		if strings.Contains(msg, "status 4") {
			return false
		}
		return true
	}
	return false
}
