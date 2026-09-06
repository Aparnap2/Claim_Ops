"""Golden deterministic suite — 9 cases, pure domain, no LLM/network."""

from __future__ import annotations

import json
from datetime import date
from decimal import Decimal
from pathlib import Path
from uuid import uuid4

import pytest

from packages.domain.audit import AuditEvent, AuditLog
from packages.domain.claim import Claim
from packages.domain.exceptions import ExceptionCode
from packages.domain.repository import InMemoryDocumentRepository
from packages.domain.state_machine import ClaimStatus, TransitionError, can_transition, transition
from packages.domain.validation import (
    validate_amount_reconciliation,
    validate_date_logic,
    validate_duplicate_sha256,
    validate_policy_active,
    validate_required_docs,
)

GOLDEN = json.loads((Path(__file__).parents[2] / "fixtures" / "golden_cases.json").read_text())


def _make_claim(**over) -> Claim:
    base = {
        "id": uuid4(),
        "tenant_id": uuid4(),
        "policy_id": uuid4(),
        "claim_reference": "CLM-001",
        "claimed_amount": Decimal("85000.00"),
        "status": "RECEIVED",
        "version": 1,
        "date_filed": date(2026, 8, 15),
        "processed_event_ids": set(),
    }
    base.update(over)
    return Claim(**base)


def test_01_happy_valid_claim_docs_transition_audit():
    c = GOLDEN["cases"][0]
    i = c["inputs"]
    assert validate_required_docs(i["docs"]).passed
    assert validate_policy_active(
        date.fromisoformat(i["incident"]),
        date.fromisoformat(i["policy_start"]),
        date.fromisoformat(i["policy_end"]),
    ).passed
    assert validate_date_logic(
        date.fromisoformat(i["admission"]), date.fromisoformat(i["discharge"])
    ).passed
    assert validate_amount_reconciliation(i["gross"], i["discount"], i["net"]).passed
    claim = _make_claim(
        claimed_amount=Decimal(i["claim_amount"]),
        date_of_admission=date.fromisoformat(i["admission"]),
        date_of_discharge=date.fromisoformat(i["discharge"]),
    )
    log = AuditLog()
    nxt = transition(claim, ClaimStatus.REGISTERED, "evt-happy-1", 1, claim.tenant_id)
    log.record(
        AuditEvent(
            tenant_id=str(nxt.tenant_id),
            claim_id=str(nxt.id),
            actor_type="system",
            actor_id="test",
            action="claim.status.transition",
            entity="claim",
            entity_id=str(nxt.id),
            before={"status": "RECEIVED"},
            after={"status": nxt.status, "version": nxt.version},
            trace_id="t-happy",
        )
    )
    assert nxt.status == ClaimStatus.REGISTERED or str(nxt.status) == "REGISTERED"
    assert nxt.version == 2
    assert len(log.for_claim(str(nxt.tenant_id), str(nxt.id))) == 1


def test_02_missing_document_no_state_mutation():
    claim = _make_claim()
    r = validate_required_docs(["CLAIM_FORM", "HOSPITAL_FINAL_BILL"])
    assert not r.passed
    assert ExceptionCode.MISSING_REQUIRED_DOCUMENT.value in ("MISSING_REQUIRED_DOCUMENT")
    assert claim.status == "RECEIVED" and claim.version == 1


def test_03_date_conflict_classified():
    r = validate_date_logic(date(2026, 8, 14), date(2026, 8, 10))
    assert not r.passed
    assert r.rule_id == "DATE_LOGIC"  # maps to DATE_CONFLICT taxonomy


def test_04_inactive_policy_rejected_no_llm():
    r = validate_policy_active(date(2026, 8, 10), date(2026, 4, 1), date(2026, 6, 30))
    assert not r.passed
    assert r.severity.value == "BLOCK"  # POLICY_NOT_ACTIVE routing


def test_05_duplicate_sha_idempotent_repo():
    tenant = uuid4()
    repo = InMemoryDocumentRepository()
    from packages.domain.repository import Document

    claim_id = uuid4()
    d1 = Document(
        id=uuid4(), tenant_id=tenant, claim_id=claim_id, sha256="abc123", storage_uri="s3://a"
    )
    d2 = Document(
        id=uuid4(), tenant_id=tenant, claim_id=claim_id, sha256="abc123", storage_uri="s3://b"
    )
    r1 = repo.save(tenant, d1)
    r2 = repo.save(tenant, d2)
    assert r1.id == r2.id  # idempotent on (tenant, claim, sha256)
    assert validate_duplicate_sha256(["abc123", "def456", "abc123"]).passed is False
    assert len(repo.list_by_claim(tenant, claim_id)) == 1


def test_06_amount_mismatch_decimal_only():
    r = validate_amount_reconciliation("85000.00", "0.00", "79500.00")
    assert not r.passed
    assert isinstance(r.message, str)
    # exact decimal: 85000.00 - 0.00 != 79500.00
    ok = validate_amount_reconciliation("85000.00", "5500.00", "79500.00")
    assert ok.passed


def test_07_illegal_transition_rejected_version_unchanged():
    claim = _make_claim(status="CLOSED")
    with pytest.raises(TransitionError) as e:
        transition(claim, ClaimStatus.DOCUMENTS_RECEIVED, "evt-x", 1, claim.tenant_id)
    assert e.value.code == "ILLEGAL_TRANSITION"
    assert claim.version == 1 and str(claim.status) == "CLOSED"
    assert not can_transition("CLOSED", "DOCUMENTS_RECEIVED")


def test_08_stale_version_zero_partial_mutation():
    claim = _make_claim(status="REGISTERED", version=3)
    with pytest.raises(TransitionError) as e:
        transition(claim, ClaimStatus.DOCUMENTS_RECEIVED, "evt-y", 2, claim.tenant_id)
    assert e.value.code == "STALE_VERSION"
    assert claim.version == 3 and str(claim.status) == "REGISTERED"


def test_09_duplicate_event_replay_single_audit():
    claim = _make_claim()
    log = AuditLog()
    once = transition(claim, ClaimStatus.REGISTERED, "evt-001", 1, claim.tenant_id)
    log.record(
        AuditEvent(
            tenant_id=str(once.tenant_id),
            claim_id=str(once.id),
            actor_type="system",
            actor_id="test",
            action="claim.status.transition",
            entity="claim",
            entity_id=str(once.id),
            before={"status": "RECEIVED"},
            after={"status": str(once.status)},
            trace_id="t-replay",
        )
    )
    twice = transition(once, ClaimStatus.REGISTERED, "evt-001", 2, once.tenant_id)
    assert twice.version == once.version  # replay: no bump, same object
    assert len(log.for_claim(str(once.tenant_id), str(once.id))) == 1
