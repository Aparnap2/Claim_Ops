# ClaimOps — UI Rules

## Scope
ClaimOps is an internal health-insurance claims operations system. UI is an operational console, not a consumer healthcare application.

There is currently no reason to invent a large frontend design system while the backend/evaluation chain is being built. These rules define the future UI boundary so agents do not invent inconsistent behavior.

## UX principles
- Optimize for claims operators and investigators.
- Show evidence before conclusions.
- Make uncertainty visible.
- Distinguish fact, extracted value, rule finding, AI hypothesis, recommendation, and human decision.
- Never present an AI recommendation as an authoritative decision.
- Every material extracted value should be traceable to evidence where available.
- Never hide validation failures behind a generic "AI insight."
- Destructive or consequential actions require explicit human confirmation where policy requires HITL.

## Case view
A future claim investigation view should separate:
1. Claim facts
2. Documents
3. Evidence
4. Deterministic validation findings
5. Investigation findings/hypotheses
6. Recommended next action
7. Human decision
8. Audit trail

## Evidence interaction
- Selecting an extracted value should reveal its source document/page/location when provenance exists.
- Missing provenance must be shown as unavailable, not approximated.
- Do not allow UI labels to imply certainty that the backend does not provide.
- AI-generated text must be visibly distinguished from authoritative facts.

## Loading/error states
Every async operation needs:
- loading state,
- success state,
- empty state,
- terminal error state,
- retry state when retry is safe.

Do not show a spinner indefinitely.
Do not retry destructive actions automatically.

## Security
- Never render secrets.
- Never render raw sensitive data unless the authenticated operator is authorized.
- Avoid exposing unnecessary PII/PHI.
- Respect tenant boundaries.
- Do not put sensitive document contents in URLs, analytics, client logs, or error telemetry.

## Accessibility
- Keyboard navigation must work for primary workflows.
- Do not encode meaning using color alone.
- Use semantic labels and accessible names.
- Tables must remain readable and navigable.

## Status language
Prefer precise states:
- RECEIVED
- PROCESSING
- PROCESSED
- EXCEPTION
- INVESTIGATION_REQUIRED
- RECOMMENDATION_READY
- HUMAN_REVIEW
- VERIFIED
- FAILED

Avoid vague labels such as "AI solved" or "looks good."
