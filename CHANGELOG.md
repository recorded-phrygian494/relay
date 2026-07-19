# Changelog

All notable changes to relay. Format loosely follows Keep a Changelog;
versions follow SemVer once tagged.

## [Unreleased] — v0.1.0 (launch candidate)

### Added
- OpenAI (`/v1/chat/completions`) and Anthropic (`/v1/messages`,
  `count_tokens`) inbound dialects with full cross-dialect translation,
  streaming, tools, and vision, over a canonical IR.
- Provider adapters: OpenAI, Anthropic, Gemini, Ollama, and 12
  OpenAI-compatible presets (Groq, Together, Fireworks, DeepSeek, xAI,
  Mistral, Cerebras, Moonshot, OpenRouter, Qwen/DashScope, Zhipu, MiniMax).
- Routing: static routes, aliases with fallback / cheapest / fastest /
  weighted policies, and tiered smart routing (lexical + embedding-KNN)
  gated by a committed eval harness (`relay eval`).
- Reliability: retries with jittered backoff, API-key pools with rate-limit
  cooldowns, circuit breakers, pre-first-token streaming failover.
- Observability: SQLite request log with privacy tiers (off / embeddings /
  full), Prometheus `/metrics`, one-page `/dashboard`, explicit "unpriced"
  cost state, per-decision routing reasons.
- `relay train` (implicit signals, replay+judge with spend estimates,
  `/v1/feedback`), `relay compare`, `relay stats`, `relay init` with
  per-provider key onboarding, `relay pricing update` (explicit only).
- `/v1/embeddings` (OpenAI dialect), exact-match response cache (opt-in,
  canonical-IR keyed, streams replay as synthetic events).
- Zero telemetry, loopback-by-default with refuse-to-start on unauthenticated
  non-loopback binds.

### Known gaps (tracked)
- OpenAI Responses API (`/v1/responses`): not yet; fast-follow v1.1.
- Semantic cache, log retention, smart-routing `translate.strictness`:
  reserved config keys, documented as such.
