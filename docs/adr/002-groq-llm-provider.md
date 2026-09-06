# ADR-002: Groq-hosted LLM via OpenAI-compatible endpoint, no Ollama

Date: 2026-09-06
Status: accepted (provider decision locked; live verification pending valid key)

## Context

The roadmap assumed local inference via Ollama behind localgcp's Vertex
proxy. Decision: drop Ollama entirely and call Groq's OpenAI-compatible
API directly. Groq documents `base_url="https://api.groq.com/openai/v1"`
with `api_key` from the environment, working with stock OpenAI clients
and LangChain's `ChatOpenAI` (verified against Groq docs, 2026-09-06).

Three candidate keys were rejected by Groq with `invalid_api_key`
(2026-09-06 smoke tests). No key material is stored anywhere as a
result; integration runs in stub mode until a valid key exists.

## Decision

- Provider: Groq. Model: `openai/gpt-oss-20b` (revisit only via ADR).
- Client pattern: OpenAI-compatible (`base_url` + env key). Works with
  `openai.OpenAI`, LangChain `ChatOpenAI`, LangGraph nodes unchanged.
- Ollama is out of scope. localgcp Vertex proxy is reserved for
  Phase 5 infra tests, not for model inference.
- Secrets: `GROQ_API_KEY` from environment only. Never committed,
  never logged. Production path is Secret Manager (Phase 7).
- No-key behavior: deterministic stub responder (same philosophy as
  localgcp stub mode) so unit/CI suites never need a live key.

## Consequences

- Phase 6 builds the LangGraph investigation service against this
  contract; live-key evaluation is a manual gate, not a CI gate.
- A valid key must be confirmed in the Groq playground before any
  further key-based smoke test is attempted here.
