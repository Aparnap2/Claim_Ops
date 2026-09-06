# Document Cases — Tier-1 extractor fixtures
Purpose: language-independent document-intelligence contract mirroring fixtures/golden_cases.json style.
Adapter rule (ADR-001): fixtures are the contract; Go edge owns validation, Python is spec-only.
Cases: happy_path, missing_document, duplicate_document, date_conflict, amount_mismatch, policy_inactive, identity_mismatch, unreadable_document, unknown_document.
Each case: documents.json (inputs) + expected.json (status + exceptions).
