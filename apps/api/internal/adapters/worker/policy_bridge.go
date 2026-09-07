package workeradapter

import (
	"context"
	"strings"

	httpadapter "claimops-api/internal/adapters/http"
	"claimops-api/internal/worker"
)

// PolicyBridge implements worker.PolicyChecker over the Mockoon-backed
// policy client. The upstream record carries no patient field, so
// Patient maps empty (documented): identity rules R1/R2 evaluate the
// policy number/active flag, patient comparison stays claim-side.
// Transport/timeout failures pass through as transient worker errors.
type PolicyBridge struct {
	Client *httpadapter.PolicyClient
}

// NewPolicyBridge builds a PolicyBridge over client.
func NewPolicyBridge(client *httpadapter.PolicyClient) *PolicyBridge {
	return &PolicyBridge{Client: client}
}

// CheckPolicy fetches the upstream policy and adapts it to the worker's
// read model. Active is an exact case-insensitive "ACTIVE" match.
func (b *PolicyBridge) CheckPolicy(ctx context.Context, tenant, policyID string) (worker.PolicyData, error) {
	info, err := b.Client.GetPolicy(ctx, tenant, policyID)
	if err != nil {
		return worker.PolicyData{}, err
	}
	return worker.PolicyData{
		Number:  info.PolicyID,
		Patient: "",
		Active:  strings.EqualFold(strings.TrimSpace(info.Status), "ACTIVE"),
	}, nil
}
