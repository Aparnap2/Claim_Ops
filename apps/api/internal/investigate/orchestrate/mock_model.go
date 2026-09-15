// Package orchestrate — MockModelClient for local E2E qualification.
// Scripted deterministic model that returns canned ModelResponse values in order.
// Real orchestrator, real executor, real tools — only the model is scripted.
// Never bypasses capability, tenant, grounding, budgets, or repetition checks.
package orchestrate

import (
	"context"
	"fmt"
	"sync"
)

// MockModelClient serves scripted responses in order.
type MockModelClient struct {
	mu        sync.Mutex
	responses []ModelResponse
	index     int
	modelID   string
}

// NewMockModelClient builds a scripted client. Each Complete call returns
// the next response. Exhaustion returns ErrModelUpstream (transient).
func NewMockModelClient(responses []ModelResponse) *MockModelClient {
	return &MockModelClient{responses: append([]ModelResponse(nil), responses...), modelID: "mock-scripted"}
}

// NewMockModelClientWithID builds a scripted client with a custom model ID.
func NewMockModelClientWithID(responses []ModelResponse, modelID string) *MockModelClient {
	if modelID == "" {
		modelID = "mock-scripted"
	}
	return &MockModelClient{responses: append([]ModelResponse(nil), responses...), modelID: modelID}
}

// Complete returns the next scripted response. Respects ctx cancellation.
func (m *MockModelClient) Complete(ctx context.Context, _ ModelRequest) (ModelResponse, error) {
	if err := ctx.Err(); err != nil {
		return ModelResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.index >= len(m.responses) {
		return ModelResponse{}, fmt.Errorf("mock model: script exhausted at call %d: %w", m.index+1, ErrModelUpstream)
	}
	resp := m.responses[m.index]
	m.index++
	if resp.ModelID == "" {
		resp.ModelID = m.modelID
	}
	return resp, nil
}

// Calls returns how many Complete calls have been served.
func (m *MockModelClient) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.index
}

// Remaining returns how many scripted responses are left.
func (m *MockModelClient) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.responses) - m.index
}

// Reset rewinds the script to the beginning.
func (m *MockModelClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.index = 0
}
