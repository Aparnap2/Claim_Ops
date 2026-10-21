# ADR 011 — Claim Expiry Contract (HITL-wait timeout)

* Status: Accepted 2026-10-21 (S5 / APA-26)
* Base: `main@7ad446c3235d6b77a672270763f70470a87225da` (rebased after #180 and #179)
* Context: `EXPIRED` has been implemented, wired and tested in the authoritative Go edge for a long time, but **no document in this repository ratified it before this ADR**. Its semantics were attested only by code comments citing a Linear id this repo does not record. Meanwhile the file that calls itself the specification of this lifecycle did not contain the status at all, and the cross-language fixture did not cover it.

## Context

`EXPIRED` is the terminal-for-round state reached when a HITL wait times out without a human decision. It exists in the Go edge as follows:

* `apps/api/internal/claims/claim.go:117-121` defines `ClaimStatusExpired` and states the intent: *"indicates a HITL wait timed out without a human decision (S5/APA-26). Terminal-for-round: no outgoing transitions. Re-drive is an explicit new decision/round, never an implicit edge. Timeout expiry can never approve or otherwise decide the claim."*
* `apps/api/internal/claims/statemachine.go:53-60` admits exactly two inbound edges, `HITL → EXPIRED` and `ACTION_PENDING → EXPIRED`. `EXPIRED` has no entry in `Transitions`, and terminality in this graph is enforced by absence from that map, so `EXPIRED` has no out-edges.
* `apps/api/internal/handlers/decision.go:95-100` wires the `EXPIRE` action into the HMAC-authenticated decision endpoint, with the same HMAC, version and idempotency controls as every other decision, and notes that *"the state machine admits only HITL/ACTION_PENDING → EXPIRED, and EXPIRED has no out-edges, so expiry can never approve or decide."*
* `workflows/claim-investigation.yaml` already posts `EXPIRE` on the HITL-timeout path (the `expire` and `expire_escalated` steps). The deployed workflow depends on the transition existing.

Tests that already pin this behaviour:

* `apps/api/internal/claims/expiry_red_test.go` — the S5/APA-26 contract: only the two legal expiry edges; `EXPIRED` has zero out-edges; post-expiry `APPROVE`/`REJECT`/`ESCALATE`/`HOLD` are illegal; and `APPROVE` from `HITL` was already illegal because `HITL` has no `VERIFIED` edge, so no timeout path can smuggle an approval.
* `apps/api/internal/handlers/decision_expiry_test.go` — 4 PostgreSQL-gated tests: expire from HITL, replay leaves the claim unchanged, post-expiry decisions rejected, expire from a non-HITL state rejected.
* `apps/api/internal/repository/postgres/hitl_expiry_test.go` — `TestHITL_ExpiredExcludedFromPending`: an expired claim is excluded from the pending-HITL set, preserving the canonical pending-state invariant (APA-10).

Two documentation facts made the gap dangerous:

* `apps/api/internal/claims/statemachine.go` described the file (before this ADR) as *"the Go port of the Python specification in packages/domain/state_machine.py"*. That specification has 13 statuses and **no `EXPIRED`** (`packages/domain/state_machine.py:28-63`), so the file claimed parity it did not have.
* `fixtures/golden_cases.json` — the cross-language behavioral contract designated by ADR-001 — contains nineteen cases and **none of them covers `EXPIRED`**. The one status on which the two implementations disagree was exactly the status the contract did not cover.

## Decision

**`EXPIRED` is an approved state of the authoritative Go claim lifecycle (S5/APA-26). Its semantics are as follows, and are now recorded rather than inferred from code.**

* **Semantics.** `EXPIRED` means: a HITL wait timed out without a human decision. It is a business state of the claim, distinct from a transport or infrastructure failure.
* **Terminal-for-round.** `EXPIRED` has **no outgoing transitions**. Terminality is enforced by absence from `claims.Transitions`; the status is listed in `isKnownStatus` so it is a recognised lifecycle state, and is otherwise unreachable.
* **Permitted inbound transitions — exactly two.** `HITL → EXPIRED` and `ACTION_PENDING → EXPIRED`. No other edge into `EXPIRED` is legal.
* **Timeout expiry can never decide a claim.** Expiry must never approve, reject, close, escalate, or otherwise determine an outcome. `APPROVE`/`REJECT`/`ESCALATE`/`HOLD` from `EXPIRED` are illegal, and `APPROVE` from `HITL` is already illegal because `HITL` has no `VERIFIED` edge. The `EXPIRE` decision carries the same HMAC, version and idempotency controls as every other decision.
* **Re-drive is a new round, never an implicit edge.** Re-driving an expired claim requires an explicit new decision or round; expiry never implies a successor state.
* **The Go edge is authoritative for lifecycle semantics.** `apps/api/internal/claims` is authoritative under ADR-001 and, for `EXPIRED` specifically, under this ADR.
* **`packages/domain/state_machine.py` is superseded for lifecycle semantics and is not a mirror.** It remains a demoted specification artefact under ADR-001 — it is not granted a mirror role, and it does not define `EXPIRED`. The divergence is registered as a limitation rather than silently reconciled.
* **The cross-language behavioural contract remains `fixtures/golden_cases.json`,** which as of this ADR contains no `EXPIRED` case. That hole is recorded as a limitation and is tracked separately; it is not closed by this ADR.

## Consequences

* The expiry contract is no longer attested only by code comments. It is recorded in a dated, accepted ADR, and the decision log and progress tracker carry the same entry.
* **No lifecycle behaviour changed.** This ADR records what the implementation already does; the existing tests already pin it. `apps/api/internal/claims/statemachine.go` changes only in its header comment, which described a parity that did not exist.
* `docs/AGENT_OPERATING_CONTRACT.md` item 5 now lists `EXPIRED` in the transition chain, so the repository's own statement of the contract agrees with the code.
* The narrower Python specification graph is now a **registered** limitation (`KL-18`) instead of an undocumented divergence, so a reader looking for it finds it in the register.
* The cross-language fixture still does not cover `EXPIRED`. Until it does, the contract is authoritative in the Go edge and in this ADR; the fixture gap is tracked, not closed here.
* The `statemachine.go` comment describing golden case 08 as witnessing "stale-version wins over legality" is inaccurate — case 08's edge is legal, so the case witnesses only that a stale version is rejected. That correction is a separate, focused change and is deliberately **not** bundled here.

## Alternatives

* **Resync the Python graph to fourteen statuses and keep it a true mirror.** Rejected: ADR-001 demotes `packages/domain` to specification artefacts and never grants it a mirror role; the divergence disappears with the artefact's retirement rather than by making a second implementation of the lifecycle permanent. It would also require a fixture change, which this ADR does not authorise.
* **Remove the Go `EXPIRED` edges and the status.** Rejected: the deployed GCW workflow posts `EXPIRE` on the HITL-timeout path. Removing the transitions would leave that branch with no business-state outcome, and would delete tests that already pin the safety property.
* **Leave expiry undocumented (status quo).** Rejected: this is precisely the gap — an implemented, wired, deployed state of a money-affecting lifecycle that no document ratified.
