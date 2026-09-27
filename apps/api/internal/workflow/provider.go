// Package workflow provides the WorkflowProvider boundary for GCW orchestration.
//
// Production code depends only on the WorkflowProvider interface.
// GCWProvider is the REST implementation for the GCW emulator.
// NoopProvider is for tests without an emulator.
package workflow

import (
	"context"
	"encoding/json"
	"errors"
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

	// ExecutionResourceName maps a BARE execution id to the fully-qualified
	// resource name
	// projects/{project}/locations/{location}/workflows/{workflowID}/executions/{executionID}.
	//
	// It is the only form GetExecution can resolve: a bare execution id is
	// NOT a valid input there. The implementation owns project and location,
	// so callers must never assemble the prefix themselves — restating it
	// lets the two drift, which makes reconciliation probe a name the
	// provider never minted and silently duplicates the execution (APA-41).
	//
	// Present so the launch reconciler can address an existing execution by
	// the same identity StartExecution uses, whether it is asking or
	// answering.
	ExecutionResourceName(workflowID, executionID string) string
}

// ErrExecutionNotFound marks a typed execution absence: the provider has
// no execution under the requested name. The launch reconciler treats
// exactly this as "safe to start"; any other GetExecution error fails
// closed (no start) so a provider outage can never read as absence and
// cause a duplicate launch.
var ErrExecutionNotFound = errors.New("workflow: execution not found")

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

func (n *NoopProvider) GetExecution(_ context.Context, executionName string) (string, json.RawMessage, error) {
	return "", nil, fmt.Errorf("noop provider: execution %q absent: %w", executionName, ErrExecutionNotFound)
}

// ExecutionResourceName returns a noop-scoped resource name. NoopProvider
// resolves no execution (GetExecution is always typed-absent), so the value
// is inert and is never resolvable; it exists only to satisfy the interface
// so the launch reconciler's addressability requirement is uniform across
// implementations (APA-41).
func (n *NoopProvider) ExecutionResourceName(workflowID, executionID string) string {
	return fmt.Sprintf("projects/noop/locations/noop/workflows/%s/executions/%s", workflowID, executionID)
}

func (n *NoopProvider) SendCallback(_ context.Context, _ string, _ any) error {
	return fmt.Errorf("noop provider: SendCallback not implemented")
}

func (n *NoopProvider) DeployWorkflow(_ context.Context, _ string, _ string) error {
	return fmt.Errorf("noop provider: DeployWorkflow not implemented")
}
