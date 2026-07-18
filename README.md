# relay

Self-hosted LLM gateway + model router. One static Go binary that speaks both the
OpenAI and Anthropic API dialects inbound, routes across any provider outbound
(bring your own keys), with pluggable routing policies. Zero telemetry — not even
opt-in pings. Apache-2.0.

> **Status:** pre-1.0, under active development (phase 4 of 5 complete: smart
> routing, `relay eval` / `relay train`, plus routing policies, reliability,
> pricing/cost, observability, embeddings, cache from earlier phases).
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

## Smart routing

Smart routing **gets better on YOUR traffic via `relay train`** — the shipped
router is a starting point, not a finished brain. `routing.default: smart`
classifies each unaliased request's difficulty and routes easy traffic to your
cheap chain and hard traffic to your frontier chain. Two tiers:

- **Tier 1 (default): pure-Go lexical classifier.** Deterministic, zero external
  calls, and every decision logs its evidence
  (`lexical: words=9 reason=1(prove) → difficulty 0.74 (hard, conf 0.91)`) into
  the decisions log — "the model felt like it" is not an accepted routing reason.
- **Tier 2 (opt-in): embedding KNN** over a reference set, using an embedding
  model you already run (`routing.smart.embeddings: ollama/nomic-embed-text`).
  Local-only by default: a remote embedder requires an explicit
  `allow_remote_embeddings: true`, because routing must never silently ship your
  prompts to an API. `relay train` grows the reference set from your own logs
  (implicit signals, optional replay+judge — which always prints its projected
  spend and asks before spending — and `POST /v1/feedback` scores).

**What the eval gate showed (and its limits):** on the committed synthetic eval
set (v1, 49 queries — see `assets/eval/README.md` for provenance and the stated
circularity caveats), `relay eval` measured tier 1 at −18% cost within 0.017
mean quality of always-frontier (tolerance 0.02) — that result is why tier 1
ships enabled. Tier 2 from the cold-start seed set alone was cheaper (−27%) but
missed the quality bar (−0.033); it is the upgrade path *after* `relay train`
has densified its reference set with your traffic. These are dry-run numbers
against a synthetic quality model, not live benchmark claims — run
`relay eval` yourself; the harness is in the box.

Prior art, credited: relay's tier-2 KNN is the same family as
[RouteLLM](https://arxiv.org/abs/2406.18665) (Ong et al.) run locally over your
own traffic; learned matrix-factorization and
[graph routers](https://arxiv.org/abs/2410.03834) (GraphRouter, Feng et al.,
ICLR 2025) are on the roadmap, not in v1.

## Observability

Every request is logged to a local SQLite database (metadata only by default —
prompt logging is opt-in, three-tier). `/dashboard` serves spend by day, latency
percentiles, and recent routing decisions with human-readable reasons.
`/metrics` exposes Prometheus counters and histograms, including cost and cache
hit rates. Models missing from the pricing registry are surfaced as **unpriced**
(never silently $0) in the dashboard, `relay stats`, and
`relay_unpriced_requests_total`.
