# Models landscape — which model for what

Working notes in the spirit of `quirks.md`: only claims backed by relay's
presets, its recorded corpus, or a cited dated source. Prices live in
`assets/pricing.json` (sources cited there); this file is about fit. Dates mark
when an entry was last reviewed — trust nothing undated. For anything not
listed here, `relay compare --models a,b,c "your prompt"` answers the question
on your own data in thirty seconds, with real latency and cost attached.

> Maintainer note: seeded 2026-07-20 from what the presets and recorded
> fixtures actually establish. Expected to grow through PRs from people with
> production experience per provider.

## First-party (native dialects)

### Anthropic (`anthropic`) — reviewed 2026-07-20
- Model tiers in relay's registry: `claude-fable-5` (frontier; the eval
  harness's frontier baseline), `claude-opus-4-8` (relay's default eval judge
  — strong, and not an eval candidate), `claude-sonnet-5`, `claude-haiku-4-5`
  (the cheap tier; served relay's own smart-routing hard chain in phase-4
  live verification).
- No embeddings API — relay answers `/v1/embeddings` for it with an honest
  404 (recorded behavior).
- Anthropic-dialect passthrough is native, so block-level `cache_control`
  survives (DESIGN §5 corpus).

### Google Gemini (`gemini`) — reviewed 2026-07-20
- Registry tiers: `gemini-3.5-flash`, `gemini-3.1-pro-preview` (both
  1,048,576-token input windows per Google's model pages, fetched
  2026-07-18), `gemini-3.1-flash-lite` (the cheap band in relay's committed
  eval runs), `gemini-embedding-001` for embeddings.
- Free-tier quotas are per model per day and can be small — observed
  2026-07-19: `gemini-3.5-flash` capped at 20 requests/day, which aborted a
  49-request eval run; `flash-lite` limits are far higher (observed).
- Gemini 3 thought signatures degrade multi-turn tool use through a
  cross-dialect gateway — recorded fixture, `quirks.md`, DESIGN §0.7; relay
  flags affected models and warns.

### OpenAI (`openai`) — reviewed 2026-07-20
- Registry tiers: `gpt-5` family (incl. `-mini`, `-nano`), `gpt-4.1` family,
  `gpt-4o` family, `o3`/`o4-mini`.
- Newer OpenAI SDKs default to the Responses API, which relay does not speak
  yet (Chat Completions: full; Responses: tracked v1.1 fast-follow — the
  compatibility table is binding, DESIGN §0.5).

### Ollama (`ollama`) — reviewed 2026-07-20
- Local, free, no key; relay auto-discovers models via `/api/tags`.
- `nomic-embed-text` powers relay's tier-2 routing and the embeddings log
  tier in relay's own verification runs.
- The `sensitive-local` pattern (see `examples/`) exists because tokens that
  never leave the machine is a property no hosted tier can match.

## OpenAI-compatible presets

What each preset factually is (base URL, key env, gotchas) lives in
`internal/provider/openaicompat/profiles.go` and prints via `relay init`.
Notes below only where relay has something verifiable to add.

### Groq (`groq`) — reviewed 2026-07-20
Serves open-weight models (Llama family and others) on custom hardware.

### DeepSeek (`deepseek`) — reviewed 2026-07-20
Prepaid balance required before requests succeed (onboarding note, profiles).
API is mainland-hosted; check data-residency requirements before routing
sensitive traffic there. The bundled registry records comparatively low
per-token pricing as of 2026-07-18; evaluate quality on your workload with
`relay compare`.

### Mistral (`mistral`) — reviewed 2026-07-20
EU-based provider (data-residency relevant); free experimentation tier with
phone verification (onboarding note).

### Together / Fireworks (`together`, `fireworks`) — reviewed 2026-07-20
Broad open-weight catalogs; both host many of the same models, which makes
them natural `relay compare` A/B pairs and fallback-chain partners.

### xAI (`xai`), Cerebras (`cerebras`) — reviewed 2026-07-20
Presets verified for base URL/key convention. Cerebras serves an
open-weight catalog on wafer-scale hardware.

### Moonshot / Kimi (`moonshot`) — reviewed 2026-07-20
`.ai` (international) vs `.cn` (mainland) consoles are separate accounts with
non-interchangeable keys (onboarding note).

### Alibaba Qwen via DashScope (`qwen`) — reviewed 2026-07-20
Requires an Alibaba Cloud account. Mainland vs international endpoints differ
(`dashscope-intl.aliyuncs.com` — set `base_url` explicitly if your account is
international). Qwen models span a wide size range.

### Zhipu GLM (`zhipu`) — reviewed 2026-07-20
GLM models via bigmodel.cn; account flow is phone-number-first (mainland).

### MiniMax (`minimax`) — reviewed 2026-07-20
`.io` (international) vs mainland platforms are separate accounts.

### OpenRouter (`openrouter`) — reviewed 2026-07-20
An aggregator, not a model vendor: one key fronts many providers. Routing
through relay *and* OpenRouter stacks two gateways — do it consciously (e.g.
for discovery before getting direct keys).

## What relay's own harness has established

- On both committed eval sets, cheap models track the frontier on trivial
  traffic and fall off steeply with difficulty — the premise of routing at
  all. The magnitude is synthetic-label-dependent; see `assets/eval/README.md`
  for the honest caveats and `relay eval --live-judge` for measured quality.
- No smart tier has yet proven cost-at-equal-quality on held-out data, which
  is why smart routing ships off-by-default (DESIGN §0.3, gate outcome v2).
