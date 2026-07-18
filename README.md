# relay

Self-hosted LLM gateway + model router. One static Go binary that speaks both the
OpenAI and Anthropic API dialects inbound, routes across any provider outbound
(bring your own keys), with pluggable routing policies. Zero telemetry — not even
opt-in pings. Apache-2.0.

> **Status:** pre-1.0, under active development (phase 3 of 5 complete: routing
> policies, reliability, pricing/cost, observability, embeddings, cache).
> See [DESIGN.md](DESIGN.md) for the full design and roadmap.

## Quick start

```
export GEMINI_API_KEY=...      # and/or OPENAI_API_KEY, ANTHROPIC_API_KEY, GROQ_API_KEY, ...
relay serve                    # zero-config: sniffs env keys, probes local Ollama, listens on 127.0.0.1:4000
```

Point any OpenAI SDK at `http://localhost:4000/v1`, or Claude Code at
`ANTHROPIC_BASE_URL=http://localhost:4000`. `relay init` scaffolds a `relay.yaml`
for aliases, fallback/cheapest/fastest/weighted routing, key pools, and the cache
(the full example lives in DESIGN.md §8 and always loads verbatim — CI enforces it).

## API compatibility

| Inbound | Status |
|---|---|
| OpenAI Chat Completions (`/v1/chat/completions`) | full, incl. streaming, tools, vision |
| OpenAI Responses API (`/v1/responses`) | **not yet** — fast-follow after v1 |
| Anthropic Messages (`/v1/messages`, `count_tokens`) | full, incl. streaming, tools |
| Embeddings (`/v1/embeddings`) | OpenAI dialect; providers without an embeddings API answer an honest 404 |
| `/v1/models`, `/metrics` (Prometheus), `/dashboard` | yes |

## Performance

Gateway overhead measured against a loopback mock upstream (`go test
-run TestOverheadBudget ./internal/server/`, p50 of 20×30-request batches,
Windows 11 / Go 1.25, 2026-07-18):

| Metric | Budget (CI-gated) | Measured |
|---|---|---|
| Non-streaming p50 overhead | < 5 ms | **~0.18 ms** |
| Added time-to-first-token p50 (streaming) | < 2 ms | **~0.63 ms** |

The budget is a hard test in CI; the numbers above are what the suite printed on
the dev machine. Provider latency dominates end-to-end time; relay's job is to
stay invisible.

## Observability

Every request is logged to a local SQLite database (metadata only by default —
prompt logging is opt-in, three-tier). `/dashboard` serves spend by day, latency
percentiles, and recent routing decisions with human-readable reasons.
`/metrics` exposes Prometheus counters and histograms, including cost and cache
hit rates. Models missing from the pricing registry are surfaced as **unpriced**
(never silently $0) in the dashboard, `relay stats`, and
`relay_unpriced_requests_total`.
