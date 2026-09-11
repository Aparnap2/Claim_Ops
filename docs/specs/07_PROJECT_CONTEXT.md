# ClaimOps — Project Context

## One-sentence definition
ClaimOps is an India-focused internal health-insurance claims back-office cognitive operations system that uses deterministic software for truth/control and bounded AI for investigative reasoning over evidence.

## Problem
Claims operations are document-heavy and exception-heavy. Humans spend time gathering documents, extracting facts, reconciling contradictions, checking external records, investigating gaps, and preparing recommendations.

ClaimOps aims to reduce this investigative workload without delegating authoritative decisions to an LLM.

## Core model

Documents are evidence.

Evidence becomes structured facts.

Deterministic validation identifies exceptions.

Cognitive reasoning investigates ambiguous exceptions.

Humans retain authority over consequential decisions.

## Canonical flow

claim received/registered
-> document ingestion
-> admission/integrity
-> classification/parsing
-> extraction
-> evidence/provenance
-> canonical claim assembly
-> deterministic validation/reconciliation
-> exception
-> cognitive investigation
-> evidence retrieval
-> hypothesis/finding
-> deterministic verification
-> recommendation
-> HITL
-> workflow action
-> verification
-> audit

## Deterministic vs cognitive boundary

Deterministic:
- tenant isolation
- authorization
- state transitions
- required documents
- date arithmetic
- monetary arithmetic
- duplicate detection
- policy identity matching
- cross-document consistency
- bill reconciliation
- external contract validation
- retry/idempotency
- audit
- allowed workflow transitions

Cognitive:
- ambiguous document interpretation
- semantic retrieval
- hypothesis generation
- explanation synthesis
- identifying which evidence to inspect next
- interpreting unstructured contradictions
- recommendation drafting

## Agent safety boundary
The future Claim Investigation Agent may recommend and explain but cannot:
- approve a claim
- deny a claim
- authorize payment
- modify policy
- override deterministic validation
- directly mutate authoritative state

## Evidence model
Prefer:
Document -> Extraction -> Evidence -> ClaimFact

A ClaimFact should be traceable to its evidence where possible.

Evidence provenance should include, where available:
- document ID
- page
- source span
- coordinates/bounding box
- extractor/parser version
- confidence if honestly supplied

## External systems
The MVP uses adapter boundaries for:
- Policy Administration
- TPA/Claims Administration
- Hospital/Provider HIS
- Claims Risk Signals

NHCX is a future adapter/boundary, not an MVP dependency.

## Compliance/security posture
Treat health and identity information as sensitive.
Do not claim regulatory compliance unless the specific control has actually been implemented and verified.
Model business/regulatory turnaround rules as versioned configuration/policy rather than hard-coded universal assumptions.

## Design philosophy
The system should become more reliable by shrinking the nondeterministic surface area, not by adding more prompts.
