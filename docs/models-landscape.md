# Models landscape — which model for what

Working notes in the spirit of `quirks.md`: factual, dated, no hype. Each entry
says what a model family is actually good for through relay, and what to watch.
Prices live in `assets/pricing.json` (cited there); this file is about fit.
Dates mark when an entry was last reviewed — trust nothing undated.

> Maintainer note: seeded 2026-07-19 from the providers relay presets. Entries
> are starting points, expected to be edited by humans with production
> experience per provider. `relay compare` is the fastest way to check any
> claim here against your own prompts.

## First-party (native dialects)

### Anthropic (`anthropic`) — reviewed 2026-07-19
- **claude-fable-5** — frontier tier; strongest reasoning/coding in the family.
  The §0.3 eval's frontier baseline. Priced accordingly; route only hard
  traffic here.
- **claude-opus-4-8 / claude-sonnet-5** — mid-frontier work; opus-4-8 is
  relay's default eval judge (strong, and not an eval candidate).
- **claude-haiku-4-5** — fast/cheap tier with tool use good enough for real
  agent loops; a solid `hard`-chain budget alternative.
- Watch: no embeddings API (relay answers an honest 404); `/v1/messages`
  passthrough is relay's native path, so block-level `cache_control` survives.

### Google Gemini (`gemini`) — reviewed 2026-07-19
- **gemini-3.5-flash / gemini-3.1-pro-preview** — strong mid/frontier tiers;
  huge context (1,048,576 tokens).
- **gemini-3.1-flash-lite** — the cheap workhorse of relay's own eval runs;
  excellent $/quality on easy traffic.
- **gemini-embedding-001** — embeddings via the same key.
- Watch: free-tier quotas are *per model per day*; Gemini 3 thought signatures
  degrade multi-turn tool use through a cross-dialect gateway (documented:
  `quirks.md`, DESIGN §0.7 — relay flags affected models and warns).

### OpenAI (`openai`) — reviewed 2026-07-19
- **gpt-5 family** — frontier/mid tiers; `gpt-5-nano`/`gpt-5-mini` are
  competitive cheap tiers. **gpt-4.1 family** for long context.
- Watch: newer SDKs default to the Responses API, which relay does not speak
  yet (Chat Completions: full; Responses: tracked fast-follow).

### Ollama (`ollama`) — reviewed 2026-07-19
- Local, free, private. `nomic-embed-text` powers relay's tier-2 routing and
  the embeddings log tier. Good chat models for the `sensitive-local` pattern;
  quality tracks the open-weight state of the art at the size you can run.

## OpenAI-compatible presets

### Groq (`groq`) — reviewed 2026-07-19
Open-weight models (Llama family and friends) served unusually fast — often
the best TTFT on the board. Good `fast`/`cheap` alias material. Watch: model
list rotates; pin exact ids in aliases.

### DeepSeek (`deepseek`) — reviewed 2026-07-19
`deepseek-chat` and reasoning variants: strong quality per dollar, especially
code/math. Watch: prepaid balance required; API is mainland-hosted — check
your data-residency requirements before routing sensitive traffic.

### Mistral (`mistral`) — reviewed 2026-07-19
Solid European option (data residency); mid-tier models plus capable small
ones. Free experimentation tier exists.

### Together / Fireworks (`together`, `fireworks`) — reviewed 2026-07-19
Broad open-weight catalogs (Llama, Qwen, DeepSeek re-hosted) with per-token
pricing; useful to A/B the same open model across hosts with `relay compare`.

### xAI (`xai`) — reviewed 2026-07-19
Grok models; competitive frontier/mid tiers with live-data flavor. Watch:
pricing and ids move quickly; keep the registry override fresh.

### Cerebras (`cerebras`) — reviewed 2026-07-19
Extreme tokens/sec on a small open-weight catalog — TTFT/throughput demos and
latency-sensitive easy traffic.

### Moonshot / Kimi (`moonshot`) — reviewed 2026-07-19
Kimi models, strong long-context work. Watch: .ai vs .cn consoles are separate
accounts with separate keys.

### Alibaba Qwen via DashScope (`qwen`) — reviewed 2026-07-19
Qwen models: broad size range, strong multilingual and coding. Watch: needs an
Alibaba Cloud account; mainland vs international endpoints differ (set
`base_url` if your account is on `dashscope-intl`).

### Zhipu GLM (`zhipu`) — reviewed 2026-07-19
GLM models via bigmodel.cn; competitive Chinese-language and coding tiers.
Watch: phone-first mainland signup flow.

### MiniMax (`minimax`) — reviewed 2026-07-19
MiniMax models incl. long-context and voice-adjacent lines. Watch: .io vs
mainland platforms are separate accounts.

### OpenRouter (`openrouter`) — reviewed 2026-07-19
Not a model vendor — an aggregator. One key fronts many providers. Useful for
discovery before committing to direct keys (direct is cheaper at volume).
Routing through relay *and* OpenRouter stacks two gateways; do it consciously.

## Patterns that keep showing up

- **Easy/hard split beats model loyalty.** The eval harness keeps showing the
  same shape: cheap models are fine for most traffic and collapse on the hard
  tail. That is the entire premise of `routing.default: smart`.
- **Sensitive-local / bulk-cheap:** alias sensitive traffic to `ollama/*`,
  bulk traffic to a cheap hosted tier; see `examples/`.
- **Verify locally, always:** `relay compare --models a,b,c "your real prompt"`
  answers most "which model" questions in thirty seconds, on your data, with
  real latency and cost attached.
