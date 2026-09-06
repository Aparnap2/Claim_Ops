"""Append-only audit trail. Deterministic, no LLM."""

from __future__ import annotations

from datetime import UTC, datetime
from typing import Any, Literal
from uuid import uuid4

from pydantic import BaseModel, ConfigDict, Field

ActorType = Literal["system", "ops_user", "reviewer", "agent", "workflow"]
Entity = Literal[
    "claim",
    "document",
    "extraction",
    "evidence",
    "claim_fact",
    "validation",
    "exception",
    "investigation",
    "finding",
    "review_task",
    "human_decision",
]


class AuditEvent(BaseModel):
    model_config = ConfigDict(strict=True, extra="forbid", frozen=True)
    event_id: str = Field(default_factory=lambda: f"EVT-{uuid4().hex[:12].upper()}")
    tenant_id: str = Field(min_length=1)
    claim_id: str = Field(min_length=1)
    actor_type: ActorType
    actor_id: str = Field(min_length=1)
    action: str = Field(min_length=1)
    entity: Entity
    entity_id: str = Field(min_length=1)
    before: dict[str, Any] = Field(default_factory=dict)
    after: dict[str, Any] = Field(default_factory=dict)
    trace_id: str = Field(min_length=1)
    timestamp: datetime = Field(default_factory=lambda: datetime.now(UTC))


class AuditLog:
    def __init__(self) -> None:
        self._events: list[AuditEvent] = []

    def record(self, event: AuditEvent) -> AuditEvent:
        if not event.tenant_id or not event.claim_id:
            raise ValueError("tenant_id and claim_id required")
        frozen = AuditEvent(**event.model_dump())
        self._events.append(frozen)
        return frozen

    def for_claim(self, tenant_id: str, claim_id: str) -> list[AuditEvent]:
        return [e for e in self._events if e.tenant_id == tenant_id and e.claim_id == claim_id]

    def __len__(self) -> int:
        return len(self._events)
