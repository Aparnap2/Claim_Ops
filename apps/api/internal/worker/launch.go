package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"claimops-api/internal/invest"
	"claimops-api/internal/investigate"
	"claimops-api/internal/ports"
)

// defaultWorkflowID is the workflow launched for worker-built exception
// envelopes. Processor.WorkflowID overrides it when set.
const defaultWorkflowID = "claim-investigation"

// InvestigationLauncher is the worker's narrow launch boundary: persist the
// authoritative envelope, then start the workflow exactly once per
// investigation ID. *investigate.Launcher is the production implementation;
// tests substitute fakes. A nil Launcher disables launching (current
// behavior for constructions that do not set it).
type InvestigationLauncher interface {
	EnsureLaunched(ctx context.Context, tenantID, claimID, investigationID string, env invest.UnresolvedException, workflowID string) (executionName string, launched bool, err error)
}

// investigationIDForDocument derives a STABLE investigation ID from
// (tenant, claim, document) (S6/Q3/Q6): redelivery of the same document
// yields the same ID, so envelope persistence (ON CONFLICT DO NOTHING,
// first wins) and launch dedupe (keyed by investigation ID) converge
// instead of minting a duplicate workflow execution per attempt.
// Blank inputs fail closed: IDs are never minted from partial identity.
func investigationIDForDocument(tenant, claimID, docID string) (string, error) {
	if strings.TrimSpace(tenant) == "" || strings.TrimSpace(claimID) == "" || strings.TrimSpace(docID) == "" {
		return "", fmt.Errorf("worker: investigation identity needs tenant, claim, document: %w", ports.ErrContract)
	}
	sum := sha256.Sum256([]byte("claimops-investigation-v1\x00" + tenant + "\x00" + claimID + "\x00" + docID))
	return invest.InvestigationIDPrefix + hex.EncodeToString(sum[:16]), nil
}

var _ InvestigationLauncher = (*investigate.Launcher)(nil)
