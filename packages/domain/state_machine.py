"""Deterministic claim-case state machine. Pure functions, no I/O, no LLM."""

from __future__ import annotations

import dataclasses
from collections.abc import Mapping
from enum import StrEnum
from typing import Any, TypeVar

try:
    from pydantic import BaseModel as _PydanticBaseModel
except Exception:
    _PydanticBaseModel = None  # type: ignore[assignment, misc]

__all__ = [
    "ClaimStatus",
    "TRANSITIONS",
    "INITIAL_STATUS",
    "TERMINAL_STATUSES",
    "TransitionError",
    "can_transition",
    "transition",
]

T = TypeVar("T")


class ClaimStatus(StrEnum):
    RECEIVED = "RECEIVED"
    REGISTERED = "REGISTERED"
    DOCUMENTS_RECEIVED = "DOCUMENTS_RECEIVED"
    VALIDATING = "VALIDATING"
    READY_FOR_REVIEW = "READY_FOR_REVIEW"
    EXCEPTION = "EXCEPTION"
    INVESTIGATION_REQUIRED = "INVESTIGATION_REQUIRED"
    WAITING_FOR_EVIDENCE = "WAITING_FOR_EVIDENCE"
    HITL = "HITL"
    ACTION_PENDING = "ACTION_PENDING"
    ACTIONED = "ACTIONED"
    VERIFIED = "VERIFIED"
    CLOSED = "CLOSED"


TRANSITIONS: dict[ClaimStatus, frozenset[ClaimStatus]] = {
    ClaimStatus.RECEIVED: frozenset({ClaimStatus.REGISTERED}),
    ClaimStatus.REGISTERED: frozenset({ClaimStatus.DOCUMENTS_RECEIVED}),
    ClaimStatus.DOCUMENTS_RECEIVED: frozenset({ClaimStatus.VALIDATING}),
    ClaimStatus.VALIDATING: frozenset({ClaimStatus.READY_FOR_REVIEW, ClaimStatus.EXCEPTION}),
    ClaimStatus.READY_FOR_REVIEW: frozenset(),
    ClaimStatus.EXCEPTION: frozenset({ClaimStatus.INVESTIGATION_REQUIRED}),
    ClaimStatus.INVESTIGATION_REQUIRED: frozenset({ClaimStatus.WAITING_FOR_EVIDENCE}),
    ClaimStatus.WAITING_FOR_EVIDENCE: frozenset({ClaimStatus.HITL}),
    ClaimStatus.HITL: frozenset({ClaimStatus.ACTION_PENDING}),
    ClaimStatus.ACTION_PENDING: frozenset({ClaimStatus.ACTIONED}),
    ClaimStatus.ACTIONED: frozenset({ClaimStatus.VERIFIED}),
    ClaimStatus.VERIFIED: frozenset({ClaimStatus.CLOSED}),
    ClaimStatus.CLOSED: frozenset(),
}

INITIAL_STATUS: ClaimStatus = ClaimStatus.RECEIVED
TERMINAL_STATUSES: frozenset[ClaimStatus] = frozenset(
    {ClaimStatus.READY_FOR_REVIEW, ClaimStatus.CLOSED}
)


class TransitionError(Exception):
    def __init__(
        self,
        message: str,
        *,
        code: str,
        claim_id: Any = None,
        from_status: Any = None,
        to_status: Any = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.claim_id = claim_id
        self.from_status = from_status
        self.to_status = to_status

    def __str__(self) -> str:
        return f"[{self.code}] {super().__str__()}"


def _coerce_status(value: ClaimStatus | str) -> ClaimStatus | None:
    if isinstance(value, ClaimStatus):
        return value
    if isinstance(value, str):
        try:
            return ClaimStatus(value)
        except ValueError:
            return None
    return None


def can_transition(frm: ClaimStatus | str, to: ClaimStatus | str) -> bool:
    from_status = _coerce_status(frm)
    to_status = _coerce_status(to)
    if from_status is None or to_status is None:
        return False
    return to_status in TRANSITIONS.get(from_status, frozenset())


_STATUS_FIELDS: tuple[str, ...] = ("status",)
_VERSION_FIELDS: tuple[str, ...] = ("version",)
_TENANT_FIELDS: tuple[str, ...] = ("tenant_id",)
_CLAIM_ID_FIELDS: tuple[str, ...] = ("claim_id", "id", "claim_reference")
_IDEMPOTENCY_FIELDS: tuple[str, ...] = ("processed_event_ids", "seen_event_ids")


def _read_field(claim: Any, names: tuple[str, ...], *, default: Any = None) -> Any:
    if isinstance(claim, Mapping):
        for name in names:
            if name in claim:
                return claim[name]
        return default
    for name in names:
        if hasattr(claim, name):
            return getattr(claim, name)
    return default


def _copy_with[T](claim: T, **updates: Any) -> T:
    if _PydanticBaseModel is not None and isinstance(claim, _PydanticBaseModel):
        return claim.model_copy(update=updates)
    if dataclasses.is_dataclass(claim) and not isinstance(claim, type):
        return dataclasses.replace(claim, **updates)
    if isinstance(claim, Mapping):
        return {**claim, **updates}  # type: ignore[return-value]
    import copy as _copy

    clone = _copy.copy(claim)
    for key, value in updates.items():
        setattr(clone, key, value)
    return clone


def transition[T](
    claim: T, to: ClaimStatus | str, event_id: str, expected_version: int, tenant_id: Any = None
) -> T:
    claim_id = _read_field(claim, _CLAIM_ID_FIELDS)
    raw_status = _read_field(claim, _STATUS_FIELDS, default=None)
    raw_version = _read_field(claim, _VERSION_FIELDS, default=None)
    raw_tenant = _read_field(claim, _TENANT_FIELDS, default=None)
    raw_seen = _read_field(claim, _IDEMPOTENCY_FIELDS, default=set())

    from_status = _coerce_status(raw_status) if raw_status is not None else None
    to_status = _coerce_status(to)

    if raw_status is None or raw_version is None or raw_tenant is None:
        raise TransitionError(
            "claim must carry status, version, tenant_id", code="MALFORMED_CLAIM", claim_id=claim_id
        )
    if tenant_id is not None and tenant_id != raw_tenant:
        raise TransitionError(
            "tenant mismatch",
            code="TENANT_MISMATCH",
            claim_id=claim_id,
            from_status=raw_status,
            to_status=to,
        )
    if not isinstance(event_id, str) or not event_id.strip():
        raise TransitionError("event_id required", code="INVALID_EVENT_ID", claim_id=claim_id)
    seen: set[str] = set(raw_seen) if raw_seen is not None else set()
    if event_id in seen:
        return claim
    if raw_version != expected_version:
        raise TransitionError(
            f"stale version: expected {expected_version}, current {raw_version}",
            code="STALE_VERSION",
            claim_id=claim_id,
        )
    if from_status is None or to_status is None:
        raise TransitionError(
            f"unknown status {raw_status!r} -> {to!r}", code="UNKNOWN_STATUS", claim_id=claim_id
        )
    if not can_transition(from_status, to_status):
        raise TransitionError(
            f"illegal transition {from_status.value} -> {to_status.value}",
            code="ILLEGAL_TRANSITION",
            claim_id=claim_id,
            from_status=from_status.value,
            to_status=to_status.value,
        )
    next_seen = set(seen)
    next_seen.add(event_id)
    return _copy_with(
        claim,
        status=to_status.value if isinstance(claim, Mapping) else to_status,
        version=raw_version + 1,
        processed_event_ids=next_seen,
    )
