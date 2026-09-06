# ADR-002: Groq-hosted LLM via OpenAI-compatible endpoint, no Ollama

Date: 2026-09-06
Status: accepted and live-verified (2026-09-06)

## Context

The roadmap assumed local inference via Ollama behind localgcp's Vertex
proxy. Decision: drop Ollama entirely and call Groq's OpenAI-compatible
API directly. Groq documents `base_url="https://api.groq.com/openai/v1"`
with `api_key` from the environment, working with stock OpenAI clients
and LangChain's `ChatOpenAI` (verified against Groq docs, 2026-09-06).

Three candidate keys were rejected by Groq with `invalid_api_key`
(2026-09-06 smoke tests). A fourth key verified live the same day:
`qwen/qwen3.6-27b` chat + LangChain `ChatGroq` JSON extraction both
succeed. No key material is stored anywhere; only the verification
outcome is recorded here.

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
- Qwen thinking control: `reasoning_effort="none"` (explicit
  `ChatGroq` field per LangChain/Groq docs) suppresses `<think>`
  blocks; `max_tokens` capped per Groq on_demand OTPM limits.

## Consequences

- Phase 6 builds the LangGraph investigation service against this
  contract; live-key evaluation is a manual gate, not a CI gate.
- A valid key must be confirmed in the Groq playground before any
  further key-based smoke test is attempted here.
