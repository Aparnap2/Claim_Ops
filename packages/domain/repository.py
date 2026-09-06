"""Repository contracts + in-memory implementations (Phase 1).

Postgres later: same Protocols backed by SQLAlchemy/psycopg with RLS
SET app.tenant_id. In-memory dicts enforce identical tenant scoping now.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from decimal import Decimal
from typing import Protocol
from uuid import UUID, uuid4


# ---------------------------------------------------------------- entities
@dataclass(frozen=True, slots=True)
class Claim:
    id: UUID
    tenant_id: UUID
    claim_reference: str
    policy_id: UUID
    member_id: UUID
    claimed_amount: Decimal
    currency: str = "INR"
    status: str = "RECEIVED"
    version: int = 1


@dataclass(frozen=True, slots=True)
class Document:
    id: UUID
    tenant_id: UUID
    claim_id: UUID
    sha256: str
    document_type: str = "UNKNOWN"
    storage_uri: str = ""
    mime_type: str = "application/pdf"
    status: str = "RECEIVED"


@dataclass(frozen=True, slots=True)
class Evidence:
    id: UUID
    tenant_id: UUID
    claim_id: UUID
    document_id: UUID
    field_name: str
    value: str
    page: int = 1
    source_span: str = ""


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


# ---------------------------------------------------------------- protocols
class ClaimRepository(Protocol):
    def get(self, tenant_id: UUID, claim_id: UUID) -> Claim | None: ...
    def list_by_policy(self, tenant_id: UUID, policy_id: UUID) -> list[Claim]: ...
    def save(self, tenant_id: UUID, claim: Claim) -> Claim: ...


class DocumentRepository(Protocol):
    def get(self, tenant_id: UUID, document_id: UUID) -> Document | None: ...
    def list_by_claim(self, tenant_id: UUID, claim_id: UUID) -> list[Document]: ...
    def find_by_hash(self, tenant_id: UUID, claim_id: UUID, sha256: str) -> Document | None: ...
    def save(self, tenant_id: UUID, doc: Document) -> Document:
        """Idempotent on (tenant, claim, sha256): returns existing on dup."""
        ...


class EvidenceRepository(Protocol):
    def get(self, tenant_id: UUID, evidence_id: UUID) -> Evidence | None: ...
    def list_by_claim(self, tenant_id: UUID, claim_id: UUID) -> list[Evidence]: ...
    def list_by_document(self, tenant_id: UUID, document_id: UUID) -> list[Evidence]: ...
    def save(self, tenant_id: UUID, evidence: Evidence) -> Evidence: ...


def _assert_tenant(tenant_id: UUID, entity_tenant: UUID, what: str) -> None:
    if tenant_id != entity_tenant:
        raise PermissionError(f"cross-tenant {what} denied")


# ---------------------------------------------------------------- in-memory
class InMemoryClaimRepository:
    def __init__(self) -> None:
        self._store: dict[UUID, Claim] = {}

    def get(self, tenant_id: UUID, claim_id: UUID) -> Claim | None:
        c = self._store.get(claim_id)
        return c if c is not None and c.tenant_id == tenant_id else None

    def list_by_policy(self, tenant_id: UUID, policy_id: UUID) -> list[Claim]:
        return [
            c for c in self._store.values() if c.tenant_id == tenant_id and c.policy_id == policy_id
        ]

    def save(self, tenant_id: UUID, claim: Claim) -> Claim:
        _assert_tenant(tenant_id, claim.tenant_id, "claim.save")
        self._store[claim.id] = claim
        return claim


class InMemoryDocumentRepository:
    def __init__(self) -> None:
        self._store: dict[UUID, Document] = {}
        self._hash_idx: dict[tuple[UUID, UUID, str], UUID] = {}

    def get(self, tenant_id: UUID, document_id: UUID) -> Document | None:
        d = self._store.get(document_id)
        return d if d is not None and d.tenant_id == tenant_id else None

    def list_by_claim(self, tenant_id: UUID, claim_id: UUID) -> list[Document]:
        return [
            d for d in self._store.values() if d.tenant_id == tenant_id and d.claim_id == claim_id
        ]

    def find_by_hash(self, tenant_id: UUID, claim_id: UUID, sha256: str) -> Document | None:
        doc_id = self._hash_idx.get((tenant_id, claim_id, sha256))
        return self._store.get(doc_id) if doc_id else None

    def save(self, tenant_id: UUID, doc: Document) -> Document:
        _assert_tenant(tenant_id, doc.tenant_id, "document.save")
        key = (tenant_id, doc.claim_id, doc.sha256)
        if key in self._hash_idx:  # sha256 dedup → idempotent
            return self._store[self._hash_idx[key]]
        self._store[doc.id] = doc
        self._hash_idx[key] = doc.id
        return doc


class InMemoryEvidenceRepository:
    def __init__(self) -> None:
        self._store: dict[UUID, Evidence] = {}

    def get(self, tenant_id: UUID, evidence_id: UUID) -> Evidence | None:
        e = self._store.get(evidence_id)
        return e if e is not None and e.tenant_id == tenant_id else None

    def list_by_claim(self, tenant_id: UUID, claim_id: UUID) -> list[Evidence]:
        return [
            e for e in self._store.values() if e.tenant_id == tenant_id and e.claim_id == claim_id
        ]

    def list_by_document(self, tenant_id: UUID, document_id: UUID) -> list[Evidence]:
        return [
            e
            for e in self._store.values()
            if e.tenant_id == tenant_id and e.document_id == document_id
        ]

    def save(self, tenant_id: UUID, evidence: Evidence) -> Evidence:
        _assert_tenant(tenant_id, evidence.tenant_id, "evidence.save")
        self._store[evidence.id] = evidence
        return evidence


def new_id() -> UUID:
    return uuid4()
