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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ModelResponse{}, fmt.Errorf("groq: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+g.apiKey)

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return ModelResponse{}, fmt.Errorf("groq: do: %w: %w", err, ErrModelUpstream)
	}
	defer resp.Body.Close()

	var gr groqChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return ModelResponse{}, fmt.Errorf("groq: decode: %w: %w", err, ErrModelUpstream)
	}
	if resp.StatusCode >= 500 {
		return ModelResponse{}, fmt.Errorf("groq: status %d: %w", resp.StatusCode, ErrModelUpstream)
	}
	if resp.StatusCode >= 400 {
		msg := "unknown"
		if gr.Error != nil && gr.Error.Message != "" {
			msg = gr.Error.Message
		}
		return ModelResponse{}, fmt.Errorf("groq: status %d: %s: %w", resp.StatusCode, msg, ErrModelUpstream)
	}
	if len(gr.Choices) == 0 {
		return ModelResponse{}, fmt.Errorf("groq: no choices: %w", ErrModelUpstream)
	}
	content := gr.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return ModelResponse{}, fmt.Errorf("groq: empty content: %w", ErrModelUpstream)
	}
	modelID := gr.Model
	if modelID == "" {
		modelID = g.model
	}
	return ModelResponse{Payload: []byte(content), ModelID: modelID}, nil
}
