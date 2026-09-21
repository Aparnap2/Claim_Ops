// Package workflow provides the WorkflowProvider boundary for GCW orchestration.
//
// Production code depends only on the WorkflowProvider interface.
// GCWProvider is the REST implementation for the GCW emulator.
// NoopProvider is for tests without an emulator.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
)

// WorkflowProvider abstracts durable workflow orchestration.
//
// It mirrors GCW responsibilities without leaking GCW types into domain code.
type WorkflowProvider interface {
	StartExecution(ctx context.Context, workflowID string, argument any) (executionName string, err error)
	GetExecution(ctx context.Context, executionName string) (state string, result json.RawMessage, execErr error)
	SendCallback(ctx context.Context, callbackID string, payload any) error
	DeployWorkflow(ctx context.Context, workflowID string, sourceContents string) error
}

// NoopProvider returns errors for all operations.
// Use in tests that do not require a GCW emulator.
type NoopProvider struct{}

var _ WorkflowProvider = (*NoopProvider)(nil)

// NewNoopProvider returns a NoopProvider.
func NewNoopProvider() *NoopProvider {
	return &NoopProvider{}
}

func (n *NoopProvider) StartExecution(_ context.Context, _ string, _ any) (string, error) {
	return "", fmt.Errorf("noop provider: StartExecution not implemented")
}

func (n *NoopProvider) GetExecution(_ context.Context, _ string) (string, json.RawMessage, error) {
	return "", nil, fmt.Errorf("noop provider: GetExecution not implemented")
}

func (n *NoopProvider) SendCallback(_ context.Context, _ string, _ any) error {
	return fmt.Errorf("noop provider: SendCallback not implemented")
}

func (n *NoopProvider) DeployWorkflow(_ context.Context, _ string, _ string) error {
	return fmt.Errorf("noop provider: DeployWorkflow not implemented")
}
