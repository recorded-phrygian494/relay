# relay

Self-hosted LLM gateway + model router. One static Go binary that speaks both
the **OpenAI and Anthropic API dialects inbound**, routes across any provider
outbound (bring your own keys), with routing policies from static aliases to a
learned smart tier — gated by an eval harness that ships in the box. Zero
telemetry — not even opt-in pings. Apache-2.0.

```
any OpenAI SDK ─┐                                  ┌─ OpenAI / Anthropic / Gemini
Claude Code ────┤→  relay (one binary, :4000)  →  ├─ Groq / DeepSeek / Mistral / xAI / …
plain curl ─────┘   route · failover · log · cache └─ your local Ollama
```

## 60-second quickstart

```bash
# 1. install (or download a release binary; a tested Dockerfile is included to build yourself)
brew install llmrelay/tap/relay

# 2. keys you already have, zero config
export GEMINI_API_KEY=...        # and/or OPENAI_API_KEY, ANTHROPIC_API_KEY, GROQ_API_KEY, ...
relay serve

# 3. point anything at it
curl http://localhost:4000/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"gemini/gemini-3.1-flash-lite","messages":[{"role":"user","content":"hello"}]}'
export ANTHROPIC_BASE_URL=http://localhost:4000   # Claude Code now runs through relay
```

`relay init` scaffolds a full relay.yaml — including where to get every
provider's key. See [examples/](examples/) for Claude Code, Cursor, SDK
snippets, and the sensitive-local / bulk-cheap alias recipe.

## Why relay (and honest alternatives)

| | **relay** | LiteLLM | OpenRouter | RouteLLM |
|---|---|---|---|---|
| What it is | self-hosted gateway + router | Python proxy/SDK gateway | hosted aggregator API | research routing framework |
| Deploy | one static binary / distroless image | Python service + deps | nothing (their cloud) | Python library |
| Inbound dialects | OpenAI **and** Anthropic, full cross-translation | OpenAI (+ passthroughs) | OpenAI-compatible | n/a |
| Learned routing | tiered, local, trained on **your** logs, eval-gated | manual routing strategies | their routing | the prior art¹ |
| Eval harness in the box | yes (`relay eval`, committed sets, live-judge) | no | no | offline research harness |
| Your prompts transit a third party | never (self-hosted; remote-embedder routing requires explicit opt-in) | self-hosted | yes — that's the product | n/a |
| Telemetry | none, ever | none per their docs | hosted service | n/a |
| Ecosystem breadth | 4 native + 12 presets, adapter guide | broadest provider matrix, teams/budgets/virtual keys | very broad | n/a |
| License / runtime | Apache-2.0, Go | mixed (OSS + enterprise), Python | proprietary service | Apache-2.0, Python |

¹ relay's tier-2 KNN is the same family as [RouteLLM](https://arxiv.org/abs/2406.18665)
(Ong et al.), run locally over your own traffic; graph routers
([GraphRouter](https://arxiv.org/abs/2410.03834), ICLR 2025) are roadmap.
LiteLLM and OpenRouter are good products with different trade-offs — pick the
row that matters to you.

## API compatibility

| Inbound | Status |
|---|---|
| OpenAI Chat Completions (`/v1/chat/completions`) | full, incl. streaming, tools, vision |
| OpenAI Responses API (`/v1/responses`) | **not yet** — tracked v1.1 fast-follow |
| Anthropic Messages (`/v1/messages`, `count_tokens`) | full, incl. streaming, tools |
| Embeddings (`/v1/embeddings`) | OpenAI dialect; providers without an embeddings API answer an honest 404 |
| `/v1/models`, `/metrics` (Prometheus), `/dashboard`, `/v1/feedback` | yes |

## Performance

Gateway overhead measured against a loopback mock upstream (`go test
-run TestOverheadBudget ./internal/server/` — the budget is a hard CI gate,
methodology in [DESIGN.md §11](DESIGN.md); Windows 11 / Go 1.25, 2026-07-18):

| Metric | Budget (CI-gated) | Measured |
|---|---|---|
| Non-streaming p50 overhead | < 5 ms | **~0.18 ms** |
| Added time-to-first-token p50 (streaming) | < 2 ms | **~0.63 ms** |

Provider latency dominates end-to-end time; relay's job is to stay invisible.

## Routing

Static routes and aliases with four policies (fallback / cheapest / fastest /
weighted), plus reliability underneath every chain: retries with jittered
backoff, API-key pools with rate-limit cooldowns, circuit breakers, and
pre-first-token streaming failover.

### Smart routing — off by default, and that's the feature

relay ships a difficulty-based smart router (easy traffic → your cheap chain,
hard → your frontier chain) with an eval harness — and the harness's own
verdict is that **you should beat our baseline on your traffic before trusting
it**, so smart routing is off by default:

```bash
relay eval                          # dry-run: the committed sets, your candidates
relay eval --live-judge --dry-run   # real completions, judge-scored; prints spend first
```

What our harness measured on the held-out set v2, **live-judged** — real
completions from each routed model, quality scored by `claude-opus-4-8`
(2026-07-20; 49 prompts, valid N = 49 for every policy; completions capped at
700 output tokens; mid band is `claude-sonnet-5` because Gemini's free tier
caps `gemini-3.5-flash` at 20 requests/day; 6 of 147 judge replies buried the
verdict number in commentary and were re-parsed deterministically from the
preserved raw replies rather than scored 0 — the corrections log inside the
verdict JSON records each one):

| Policy | Cost vs always-frontier | Judged quality delta | Verdict at −0.0200 tolerance (inclusive) |
|---|---|---|---|
| static-cheap / cheapest | −98% | +0.0163 | passes |
| static-mid | −69% | +0.0041 | passes |
| tier 1 (lexical) | −50% | **−0.0204** | **fail** — strictly below the bound |
| tier 2 (embedding KNN, cold-start) | −17% | −0.0061 | passes within tolerance; opt-in for v0.1.0 |
| static-frontier (baseline) | — | — | — |

Values are shown rounded. Gate decisions use the unrounded values from the
committed verdict asset (a delta of exactly −0.0200 would pass — the rule is
inclusive; tier 1's unrounded delta is −0.020408…, exactly −1/49, strictly
below it). A CI test asserts this table matches the asset at full precision.

On this 49-prompt, 700-output-token, single-judge evaluation, the static-cheap
baseline received a higher judged score than the configured frontier baseline.
That result is specific to the tested models, prompts, token cap, and judge —
it is **not** a general model-quality claim. What it does establish is the bar
relay holds itself to: routing cleverness must earn its keep against strong
dumb baselines, *measured on your traffic*. Tier 1 failed that bar on held-out
data; tier 2 passed within tolerance but stays opt-in for v0.1.0; no smart
tier is on by default.

(History: on the v1 set tier 1 had "passed" synthetic at −0.017 — in-sample
flattery, since its weights were calibrated on v1. On v2 synthetic labels it
failed at −0.071. v1 is the calibration set; v2 live-judged is the standing
verdict. Full tables: `assets/eval/`.)

Enabling it is one config block — with an explicit tier, because nothing
routes your traffic by silent default:

```yaml
providers:
  gemini:
    api_key: ["${GEMINI_API_KEY}"]
  anthropic:
    api_key: ["${ANTHROPIC_API_KEY}"]
  ollama:
    type: ollama

routing:
  default: smart
  smart:
    easy: gemini/gemini-3.1-flash-lite
    hard: anthropic/claude-sonnet-5
    embeddings: ollama/nomic-embed-text   # selects the knn tier (documented path)
    # tier: lexical                       # experimental: failed the held-out gate
```

Tier 2 (KNN) **gets better on YOUR traffic via `relay train`** — implicit
signals, optional replay+judge (always estimates spend and asks first), and
`POST /v1/feedback` scores grow its reference set; then measure your own
crossover with `relay eval --refs ~/.relay/smart_refs.json`. Every smart
decision logs its evidence (`knn: 5 neighbors [seed-math-03 d=0.75 sim=0.95;
…]`) — "the model felt like it" is not an accepted routing reason. A remote
embedder requires `allow_remote_embeddings: true`; routing never silently
ships prompts anywhere.

## Compare models on your own prompts

```bash
relay compare --models gemini/gemini-3.1-flash-lite,anthropic/claude-haiku-4-5,groq/llama-3.3-70b \
  --html compare.html "Explain CRDTs to a backend engineer in five sentences."
```

One table (and a shareable HTML report): output, cost, latency, and TTFT side
by side, through the same adapters that serve your traffic.
[docs/models-landscape.md](docs/models-landscape.md) is the dated
"which model for what" companion.

## Observability

Every request logs to local SQLite with privacy tiers (`off` by default —
metadata only; `embeddings` stores query vectors, never text; `full` is an
explicit choice). `/dashboard` shows spend by day, latency percentiles, and
recent routing decisions with human-readable reasons. `/metrics` is
Prometheus. Models missing from the pricing registry surface as **unpriced**
— never a silent $0.

## Security

Loopback by default; a non-loopback bind without inbound API keys **refuses
to start**. No telemetry. See [SECURITY.md](SECURITY.md).

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) — the adapter-authoring guide is there;
an OpenAI-compatible provider is a one-entry preset. Design changes get
argued in [DESIGN.md](DESIGN.md) §0 first; it's how the doc stays true.
