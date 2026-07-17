# Relay — Design Document

> Self-hosted LLM gateway + intelligent model router. One static Go binary that speaks
> both the OpenAI and Anthropic API dialects inbound, routes across any provider
> outbound (BYOK), with pluggable routing policies from `static` to learned `smart`
> routing. Zero telemetry. Apache-2.0.

Status: **Phases 1–2 complete; Phase 3 in progress (Step 0 corpus audit done
2026-07-16 — findings in `docs/quirks.md`, one decision pending in §0.7).** This
document is kept current as the code evolves.

**Standing convention:** any decision where the implementation disagrees with the spec
or the reviewer — or wants to — gets argued *in writing* in §0 before code is written.
Review conditions are marked **(binding)** inline where they apply.

---

## 0. Pushback / decisions that need your sign-off

These are the places where I either disagree with the spec, or the spec is internally
inconsistent and I picked a resolution. Everything else in this doc I'm confident in.

### 0.1 Go — agreed, no pushback

Go is the right call and I'm not arguing against it. Single static binary, best-in-class
HTTP/SSE plumbing, goroutines map perfectly onto "hold two streams open and transform
events between them," `go:embed` for the dashboard/pricing/classifier weights, trivial
cross-compilation for goreleaser. Rust would buy ~nothing here (the workload is I/O-bound
proxying, not compute) and cost contributor accessibility. Python (LiteLLM's choice) is
exactly the incumbent weakness we're positioning against.

### 0.2 Naming inconsistency in the spec: `relay.yaml` but `router init` / `router stats` / `router train`

The spec names the config `relay.yaml` but names the CLI subcommands `router ...`.
**Decision: the binary is `relay`; all subcommands live under it:**

```
relay serve            # run the gateway (default when no subcommand)
relay init             # interactive config scaffold
relay stats            # spend/latency/routing summaries from the local SQLite log
relay train            # (phase 4) train the smart-routing classifier from logged traffic
relay eval             # (phase 4) eval harness: smart vs static on cost + quality
```

**Review decision (approved):** flat `relay train` / `relay eval` — no `router`
subcommand group for the headline feature.

### 0.3 Real tension: "single static binary, no runtime deps" vs. "embedding classifier with ONNX"

Requirement 1 (pure static binary, `CGO_ENABLED=0`) and requirement 6 ("embed the query
via local embedding model via Ollama **or ONNX**") conflict: every production-quality
ONNX runtime binding for Go is cgo (onnxruntime shared lib), which breaks static builds,
distroless images, and painless cross-compilation. Pure-Go ONNX interpreters exist but
are immature and slow. **Decision — tiered smart routing, static binary preserved:**

1. **Default tier (always available, zero deps):** a pure-Go lexical classifier —
   logistic regression / GBDT-lite over hashed character n-grams plus structural
   features (prompt length, code-fence presence, tool count, language, json-mode,
   message depth). Weights pre-trained offline and shipped via `go:embed` (~100s of KB).
   This is unreasonably effective for the actual routing decision ("is this trivial
   chat or hard reasoning?") and it's what makes `smart` work out of the box.
2. **Embedding tier (opt-in, uses Ollama you already run):** if the config points at an
   Ollama instance with an embedding model (e.g. `nomic-embed-text`), `smart` upgrades
   to embedding + KNN over your own logged traffic — the LLMRouter approach. Ollama is
   an *optional peer service*, not a runtime dependency of the binary.
3. ONNX-in-process is **out** for v1. Documented extension point.

If you want ONNX badly enough to accept a cgo build variant, that's a build-tag decision
we can take in Phase 4 — flagging it now so it's not a surprise then.

**LAUNCH GATE (review condition, binding):** the pure-Go lexical classifier ships as
the *default* `smart` tier **only if** the Phase 4 eval harness proves it beats static
routing on **cost at equal quality** on the eval set. Lexical classifiers are usually
embarrassing, and a "smart" router dumber than aliases torches credibility on launch
day. If it fails the bar, it ships **off by default** and Ollama-embedding-KNN becomes
the documented smart tier (with the lexical tier available behind an explicit config
flag, clearly labeled experimental). The Phase 4 eval report must state which side of
this gate we landed on; Phase 5 README copy depends on it.

### 0.4 "Train from your own traffic" has a labeling problem — be honest about the loop

The gateway logs (prompt features, model served, latency, cost) but **not response
quality**. KNN/MLP routers need quality labels. The spec's product loop is right, but it
needs an explicit labeling strategy. **Decision — `relay router train` supports three
label sources, all opt-in and local:**

- **Implicit signals** (free): conversation continued vs. user immediately retried the
  same prompt against a bigger model; response truncation; tool-call validity (did the
  arguments parse and match the schema). Weak but free.
- **Replay + judge** (costs the user tokens, explicitly confirmed): sample N logged
  prompts, replay against candidate models, judge with a user-chosen strong model.
  This is the honest version of LLMRouter's offline training, run on *your* traffic.
  **Review condition (binding): estimate-before-run.** `relay train --dry-run` prints
  projected spend (sampled prompts × candidate models × judge, priced from the
  registry) and the plain `relay train` run shows the same estimate and requires
  confirmation before any API call is made. It never spends silently.
- **Thumbs endpoint**: `POST /v1/feedback {request_id, score}` for users who wire up
  their own signal. Trivial to implement, ignored by default.

Phase 4 detail, but the SQLite schema (§9) reserves the columns now so Phase 1 logs are
usable as Phase 4 training data.

### 0.5 Additions to the API surface the spec missed

- **`POST /v1/messages/count_tokens`** — Claude Code calls this. Without it, "Claude
  Code runs through this gateway" (Phase 2 deliverable) degrades. We implement it with
  a local tokenizer approximation for non-Anthropic targets and passthrough for
  Anthropic targets. In scope for Phase 2.
- **`POST /v1/embeddings`** (OpenAI dialect) — cheap to support via the same adapters,
  needed anyway for the smart router's Ollama path. Small; Phase 3.
- **OpenAI `POST /v1/responses`** — the Responses API is what Codex and newer OpenAI
  SDKs default to. **Not in v1** (it's a large surface), but the design keeps the
  inbound-dialect layer pluggable so it can be added without touching the core.
  **Review conditions (binding):** (a) the README compatibility table must state
  explicitly "Chat Completions: full · Responses API: not yet" — we never claim
  "any OpenAI SDK works" anywhere in README/docs/marketing copy; (b) Responses is a
  **fast-follow milestone (v1.1)** with a tracking issue opened at launch, not
  roadmap vapor. The ecosystem is actively migrating; this gets an issue filed in
  week one if we're vague about it.

### 0.6 Security default worth stating now

If `relay` binds to a non-loopback address with no admin API key configured, it
**refuses to start** (override: `--insecure-no-auth`). An open proxy that spends your
provider keys is the one foot-gun this product must not ship. Loopback with no key is
allowed (the zero-config path).

### 0.7 Gemini 3 thought signatures vs. a stateless gateway — needs your sign-off

Found by the Phase 3 Step 0 corpus audit (2026-07-16, see `docs/quirks.md`):
Gemini 3 models attach `thoughtSignature` to response parts and require them
back on function-call replay. Anthropic's equivalent we already solved — but
only because the Anthropic dialect exists *inbound*, so signatures ride through
same-dialect hops. Gemini is outbound-only: an OpenAI-dialect client replays
history in OpenAI shapes, which have no field for a Gemini signature. Options:

1. **Document the limitation (recommended for v1).** Consistent with the existing
   policy "thinking is passthrough same-dialect only, dropped cross-dialect."
   Consequence: multi-turn tool use against Gemini 3 models may be rejected for
   missing signatures; single-turn and non-tool traffic unaffected. README's
   compatibility table gets a row, same honesty rule as the Responses API (§0.5).
2. **Smuggle the signature inside the synthesized tool-call id** (some gateways
   do this): clients treat ids as opaque and echo them back, so the adapter can
   decode on replay. Covers only functionCall signatures (text-part signatures
   have no echo channel), bloats ids (signatures can reach KBs on pro models),
   and quietly couples us to clients never truncating ids. Clever, fragile.
3. **Server-side signature cache** keyed by a conversation-ish hash — state in a
   product whose reliability story is statelessness. Not for v1.

**Resolved 2026-07-17: option 1 approved as scoped — document, don't smuggle —
with two binding conditions and one investigation (done):**

- **Condition 1 (binding) — fail loudly, not mysteriously.** Relay detects the
  case itself: Gemini's signature-validation 400 maps to a typed error
  (`gemini_missing_thought_signature`) that names the limitation, links to the
  docs section, and states the workarounds (alias/fallback multi-turn tool
  traffic elsewhere, or keep tool use single-turn). A structured warning is
  logged the first time a conversation crosses the line and is appended to the
  route reason, so the dashboard's recent-decisions view surfaces it. The
  mapping is backed by a recorded fixture of the real validation error
  (`testdata/gemini/recorded/missing_thought_signature/`).
- **Condition 2 (binding) — routing-level escape hatch.** The model catalog
  carries per-provider/model capability metadata (`multi_turn_tools: degraded`
  for Gemini 3 targets), and fallback-chain resolution considers it, so one
  alias can steer multi-turn tool traffic away from Gemini 3 without the user
  thinking about signatures. Capability metadata is needed for Phase 4 anyway —
  the field is built now and populated for this case.
- **Investigation (done 2026-07-16, recorded):** Google's own OpenAI-compat
  endpoint (`/v1beta/openai/`) does NOT absorb the problem — it surfaces the
  signature in a nonstandard `tool_calls[].extra_content.google.thought_signature`
  field and rejects replay without it with the identical 400. Routing Gemini
  traffic through the compat endpoint is therefore not a fix; conditions 1+2
  ship as designed. Findings in `docs/quirks.md`.

**Fast-follow (post-v1, door stays architecturally open):** (a) an opt-in
in-memory LRU signature cache keyed by tool-call id — the known future fix for
transparent multi-turn tool use on Gemini 3; (b) adopting Google's own
`extra_content.google.thought_signature` convention in relay's OpenAI inbound
dialect as a passthrough echo channel for clients that replay message objects
faithfully.

Everything below assumes the seven decisions above.

---

## 1. Architecture

```
   OpenAI SDKs · Anthropic SDKs · Claude Code · curl · Cursor · anything
        │
        ▼  HTTP (localhost:4000 default)
 ┌────────────────────────────── relay (one binary) ──────────────────────────────┐
 │                                                                                │
 │  inbound API layer                                                             │
 │   /v1/chat/completions  /v1/messages  /v1/models  /v1/messages/count_tokens    │
 │   /health  /metrics  /dashboard  /v1/feedback                                  │
 │        │  parse + validate inbound dialect (fuzzed, zero-panic)                │
 │        ▼                                                                       │
 │  translate: dialect ──► core IR (canonical request)          [package translate]│
 │        │                                                                       │
 │        ▼                                                                       │
 │  Router.Route(ctx, req) ──► ordered []ModelCandidate         [package router]  │
 │        │            ▲                                                          │
 │        │            └── live stats (latency EWMA), pricing registry, config    │
 │        ▼                                                                       │
 │  executor: walks the candidate chain                    [package reliability]  │
 │   retries · backoff · circuit breakers · key pool · timeout budgets ·          │
 │   pre-first-token streaming failover                                           │
 │        │                                                                       │
 │        ▼                                                                       │
 │  Provider adapter: core IR ──► provider wire format ──► core events            │
 │   openai · anthropic · gemini · openai-compat · ollama       [package provider]│
 │        │                                                                       │
 │  side effects (async, never block the response path):                          │
 │   SQLite request log · Prometheus metrics · exact-match cache                  │
 └────────────────────────────────────────────────────────────────────────────────┘
        │
        ▼
  api.openai.com · api.anthropic.com · generativelanguage.googleapis.com ·
  localhost:11434 (Ollama) · any OpenAI-compatible base_url · ...
```

**The load-bearing decision: a canonical intermediate representation (IR).**
With 2 inbound dialects and N outbound providers, pairwise translation is 2×N
hand-written paths that drift. Instead: every inbound request parses into `core.Request`
(a superset IR), every adapter serializes IR → its wire format, and streaming runs
through `core.Event` the same way. Translation correctness then lives in exactly two
places per dialect (in + out), tested against each other by round-trip fixtures.

Requests and responses flow **through** relay; the IR is in-memory only. Nothing is
mutated destructively: unknown/dialect-specific fields ride along in an `Ext` bag so a
same-dialect hop (OpenAI-in → OpenAI-out) is passthrough-faithful even for fields the IR
doesn't model.

---

## 2. Repository / package layout

```
relay/
├── cmd/relay/                  main; subcommands via stdlib flag + tiny dispatcher (no cobra unless it earns it)
├── internal/
│   ├── core/                   the IR: Request, Response, Event, Message, ToolCall, Usage. Zero deps.
│   ├── api/
│   │   ├── openai/             inbound OpenAI dialect: wire types, parser, SSE writer
│   │   └── anthropic/          inbound Anthropic dialect: wire types, parser, SSE writer
│   ├── translate/              dialect wire types ⇄ core IR, both directions, incl. streaming
│   │                           state machines. THE package with exhaustive fixture tests.
│   ├── provider/
│   │   ├── provider.go         Provider interface + registry + capability flags
│   │   ├── openaiprov/         first-party OpenAI (named to avoid colliding with api/openai);
│   │   │                       Azure OpenAI joins here later (same wire, different URL/auth)
│   │   ├── anthropic/
│   │   ├── gemini/             AI Studio + Vertex (same wire, different auth/endpoint)
│   │   ├── openaicompat/       the escape hatch: configurable base_url; vLLM/SGLang/LM Studio/
│   │   │                       llama.cpp/LocalAI/Groq/Together/Fireworks/DeepSeek/xAI/... are
│   │   │                       preset profiles over this adapter (quirk table, not new code)
│   │   └── ollama/             native /api/chat + /api/tags auto-discovery + /api/embed
│   ├── router/
│   │   ├── router.go           Router interface, ModelCandidate, policy registry
│   │   ├── static.go alias.go cheapest.go fastest.go weighted.go fallback.go
│   │   └── smart/              (phase 4) feature extraction, lexical classifier, KNN, training
│   ├── reliability/            executor, retry/backoff, circuit breaker, key pool, hedging point
│   ├── store/                  SQLite (modernc.org/sqlite — pure Go, keeps CGO_ENABLED=0)
│   ├── pricing/                embedded pricing.json + refresh from local file/URL on demand
│   ├── cache/                  exact-match response cache (memory LRU + optional SQLite persistence)
│   ├── config/                 relay.yaml schema, env interpolation, zero-config sniffing, hot reload
│   ├── stats/                  rolling latency/TTFT windows feeding fastest-router + dashboard
│   ├── metrics/                Prometheus registry
│   ├── dashboard/              one embedded HTML page (go:embed), server-rendered JSON + <100 lines vanilla JS
│   └── server/                 HTTP wiring, auth middleware, SSE plumbing, graceful shutdown
├── assets/pricing.json         bundled pricing registry (versioned, user-overridable)
│                               (fixtures live package-local, e.g. internal/translate/testdata/,
│                               per Go convention — not in a root testdata/ tree)
├── DESIGN.md  README.md  LICENSE (Apache-2.0)
└── examples/  .github/  Dockerfile  .goreleaser.yaml        (phase 5)
```

Dependency policy: stdlib-first. go.mod today: `modernc.org/sqlite`, `gopkg.in/yaml.v3`;
`prometheus/client_golang` joins in phase 3. Hot reload is 2s mtime polling, not fsnotify —
one fewer dependency, identical behavior at this scale, and reliable on Windows.

---

## 3. The core IR (`package core`)

Superset of both dialects' semantics. Sketch (fields elided, shapes final):

```go
// Request is the canonical, dialect-neutral form of a completion request.
type Request struct {
    Model        string        // as requested by the client (may be an alias)
    System       []SystemPart  // Anthropic: system param; OpenAI: system/developer messages
    Messages     []Message
    Tools        []Tool        // normalized JSON-Schema tools
    ToolChoice   ToolChoice    // auto | none | required | specific tool
    MaxTokens    *int
    Temperature  *float64      // stored in native scale + origin dialect; see §5 quirks
    TopP         *float64
    Stop         []string
    Stream       bool
    ResponseFormat *ResponseFormat // text | json_object | json_schema{...}
    Metadata     map[string]string
    Ext          Ext           // dialect-specific passthrough (see below)
    Inbound      Dialect       // openai | anthropic — drives lossiness policy
}

type Message struct {
    Role  Role     // system | user | assistant | tool
    Parts []Part   // ordered content blocks
}

// Part is a closed union: Text, Image (url|base64+mime), ToolCall (id, name,
// arguments as raw JSON), ToolResult (for id, content parts, isError), Thinking
// (Anthropic reasoning blocks — passthrough-only in v1).
```

`Ext` carries fields the IR doesn't model (e.g. OpenAI `logit_bias`, Anthropic
`anthropic_beta` headers), keyed by dialect. Rule: **Ext is applied only when outbound
dialect == the Ext's dialect**; cross-dialect it's dropped or errored per the strictness
config (§5.4).

Streaming is unified as an event sequence:

```go
// Event is the canonical streaming unit. Adapters produce these; inbound SSE
// writers consume them and emit dialect-correct wire events.
type Event struct {
    Kind  EventKind // MessageStart, TextDelta, ToolCallStart, ToolCallDelta,
                    // ToolCallEnd, ThinkingDelta, Usage, MessageEnd, Error
    // ... per-kind payload
}
```

Both dialects stream tool arguments as incremental JSON *strings*, so
`ToolCallDelta{ArgsFragment string}` translates token-by-token in both directions
without buffering. `MessageEnd` carries the normalized stop reason
(`end_turn | max_tokens | tool_use | stop_sequence | content_filter`), mapped per
dialect by a single table in `translate`.

---

## 4. The two interfaces

### 4.1 Provider

```go
// Provider is one upstream. Adapters translate core IR to the wire and back;
// they do NOT retry, break circuits, or pick keys — that's the executor's job.
type Provider interface {
    Name() string                                   // "openai", "ollama", "my-vllm"
    Models(ctx context.Context) ([]Model, error)    // for /v1/models; cached w/ TTL
    Complete(ctx context.Context, req *core.Request) (*core.Response, error)
    Stream(ctx context.Context, req *core.Request) (core.Stream, error)
}

// Stream is a pull iterator; Next blocks until an event, io.EOF, or error.
// Close aborts the upstream request.
type Stream interface {
    Next() (core.Event, error)
    Close() error
}

// Capabilities lets the executor and translate layer fail fast instead of
// discovering mid-flight that a provider can't do vision or tools.
type Capabilities struct {
    Tools, Vision, JSONSchema, Streaming, ParallelToolCalls bool
    MaxContext int
}
```

Errors are normalized to `provider.Error{Status, Code, Retryable, RetryAfter, Raw}` so
the executor's retry/breaker logic is provider-agnostic and the client always sees a
clean, dialect-correct error body (with the provider's raw error preserved in a detail
field — debugging beats purity).

### 4.2 Router

```go
// Router chooses an ordered fallback chain for a request. It must be fast
// (<1ms) and side-effect free; the executor owns actually walking the chain.
type Router interface {
    Name() string
    Route(ctx context.Context, req *core.Request) ([]ModelCandidate, error)
}

type ModelCandidate struct {
    Provider string  // registry key
    Model    string  // provider-native model id
    Reason   string  // human-readable: "alias 'fast' → groq/llama-3.3-70b (rank 1)"
                     // logged verbatim; this is the explainability story
    Params   Overrides // optional per-candidate overrides (e.g. alias pins temperature)
}
```

Policies compose: `alias` resolves a virtual model then delegates to an inner policy;
`weighted` wraps children; `fallback` is just a literal chain. `cheapest`/`fastest`
read the pricing registry / `stats` windows. Deterministic policies are pure functions
of (config, request); only `fastest` and `smart` consume live state, injected at
construction — no globals, easy tests.

---

## 5. Translation matrix (`package translate`) — the hard part

Four paths, all first-class: OpenAI-in→{OpenAI-out, Anthropic-out}, Anthropic-in→{same
two}, plus Gemini-out from either. Non-streaming and streaming each direction.

### 5.1 Known-hairy mappings (each gets dedicated fixtures)

| Concern | OpenAI dialect | Anthropic dialect | Strategy |
|---|---|---|---|
| System prompt | `system`/`developer` role messages, any position | top-level `system` param | Hoist+concatenate on →Anthropic; synthesize leading system message on →OpenAI |
| Tool definitions | `function.parameters` JSON Schema | `input_schema` JSON Schema | Direct; strip OpenAI `strict` flag for Anthropic, note in Ext |
| Tool calls (resp) | `tool_calls[]`, arguments = JSON **string** | `tool_use` block, input = JSON **object** | Parse/serialize at the boundary; invalid JSON from provider preserved as-is with error surfaced per strictness |
| Tool results | `role:"tool"` message, string content | `user` message w/ `tool_result` block | Merge/split adjacent messages; Anthropic requires tool_result immediately after tool_use — reorder pass with test coverage |
| Streaming tool args | `delta.tool_calls[i].function.arguments` fragments | `input_json_delta.partial_json` fragments | Both are string fragments → direct relay, no buffering |
| Stop reasons | `stop`,`length`,`tool_calls`,`content_filter` | `end_turn`,`max_tokens`,`tool_use`,`stop_sequence`,`refusal` | Single bidirectional table; unmapped → closest + `Ext` note |
| Images | `image_url` (https or data:) | `source: base64|url` + media_type | data: URIs decoded once into IR; https passed through where provider supports URL sources, else fetched **only if** `translate.fetch_images: true` (default off — privacy) |
| `response_format: json_schema` | native | none | Emulate via forced single-tool call + unwrap; documented; disable via strictness=error |
| `max_tokens` | optional | **required** | Inject configurable default (per-model ceiling from pricing registry) on →Anthropic |
| Temperature scale | 0–2 | 0–1 | **Clamp, never rescale** (rescaling silently changes behavior); warn via response header `x-relay-adjusted` |
| `n > 1` | supported | no | Reject with clear 400 on →Anthropic (no silent fan-out in v1) |
| logprobs, seed, etc. | supported | no | Strictness-dependent: drop+warn (default) or 400 |
| Usage in stream | `stream_options.include_usage` final chunk | `message_delta.usage` | Always request usage upstream when available; emit per inbound dialect's convention |
| Thinking blocks | reasoning_content (varies by compat provider) | `thinking` blocks | Passthrough same-dialect; cross-dialect: surface as Ext, drop from content (v1) |
| `role:"system"` in messages | normal | undocumented, but Claude Code sends it and the live API tolerates it (recorded 2026-07-14) | Accept on parse; hoist into the `system` parameter outbound |
| block `cache_control` | none | on system/tools/content blocks; silently dropping it breaks prompt caching | `BlockExt` on IR parts; same-dialect passthrough, dropped cross-dialect |

### 5.2 Streaming translation = two small state machines

Inbound writer per dialect consumes `core.Event`s and maintains the dialect's protocol
invariants (Anthropic's `message_start → content_block_start → deltas → content_block_stop
→ message_delta → message_stop` envelope; OpenAI's chunk/finish_reason/`[DONE]`
conventions). Adapters do the reverse. Each state machine is a pure
`func(state, wireEvent) (state, []core.Event, error)` — trivially table-testable.

### 5.3 Fixture-based testing

`testdata/fixtures/<provider>/<case>/` holds `request.json`, `response.json` (or
`stream.txt` — raw SSE bytes), recorded once against real APIs by a maintainer-run
`go run ./tools/record` tool (never in CI). Tests are table-driven over the fixture
tree: parse → IR → serialize → compare golden; plus cross-dialect round-trips
(OpenAI fixture → IR → Anthropic wire → golden). When a provider's live behavior
contradicts its docs, the fixture wins and gets a dated comment. Target: >80% coverage
on `translate` enforced in CI; fuzzers on both inbound parsers (zero-panic bar).

**Review conditions (binding) — two mandatory test classes:**

1. **Same-dialect round-trip identity.** The `Ext` passthrough bag is where lossless
   claims go to die. CI enforces a dedicated test class: for every recorded fixture,
   same-dialect hops (OpenAI-in → IR → OpenAI-out, Anthropic-in → IR → Anthropic-out)
   must be **semantically identical** to the original (key-order-insensitive JSON
   comparison, defaults-normalized), streaming included. Any new fixture automatically
   joins this suite, so the bag can't silently rot as the dialects evolve.
2. **`json_schema`-via-forced-tool re-synthesis.** Emulating OpenAI structured outputs
   on Anthropic via a forced tool call changes the *streaming shape*: the client
   expects `content` text deltas but Anthropic emits `input_json_delta` tool-input
   fragments. The outbound codec must re-synthesize those tool-input deltas as content
   deltas on the way back out (and the final message as assistant text content, with
   `finish_reason: stop` — not `tool_calls`). There is a dedicated streaming fixture
   for this path; it is not optional.

### 5.4 Strictness policy

`translate.strictness: warn | strict | silent` (default `warn`). `warn`: drop
untranslatable field, add `x-relay-dropped: logprobs` header + log. `strict`: 400 with
an explicit message. `silent`: drop quietly. Per-request override via
`x-relay-strictness` header.

---

## 6. Reliability (`package reliability`)

The executor walks the router's candidate chain:

- **Retries**: per-candidate, only on `Retryable` errors (429/5xx/transport), exponential
  backoff + full jitter, honoring `Retry-After`. Non-retryable (400/401/413) skip
  immediately to the next candidate or fail.
- **Key pool**: N keys per provider, round-robin; a 429 puts that key in cooldown
  (Retry-After or default 30s) and rotates. All keys cooling → candidate is down.
- **Circuit breaker**: per (provider, model). Closed → open after failure-rate threshold
  over a sliding window; half-open probes one request. Open circuit = candidate skipped
  instantly (the chain is the fallback).
- **Timeout budgets**: one overall deadline per request (`timeouts.request`, default
  5m streaming / 2m not), per-attempt connect + TTFT timeouts (`timeouts.ttft`, default
  30s). Budget is shared across the whole chain — a slow first candidate can't starve
  the fallback.
- **Streaming failover**: the inbound SSE response isn't started until the first
  upstream content event arrives. Fail before first token → next candidate,
  invisible to the client. Fail after first token → emit dialect-correct error event
  and terminate; **no mid-stream model switching in v1** (silently splicing two models'
  outputs is a correctness lie). The one-candidate-behind-buffering alternative is a
  documented non-goal.

---

## 7. Routing policies (`package router`)

| Policy | Behavior | State needed |
|---|---|---|
| `static` | `model` string → (provider, model) via config map; unknown model + exactly one provider serving it → direct | none |
| `alias` | virtual names (`fast`, `smart`, `cheap`) → child chain or nested policy | none |
| `cheapest` | rank eligible candidates by blended $/tok from pricing registry (input-weighted by request size estimate) | pricing |
| `fastest` | rank by rolling TTFT/latency EWMA per (provider, model); cold candidates get optimistic prior so they're explored | stats |
| `weighted` | deterministic hash-based traffic split (stable per request id) across children — canaries | none |
| `fallback` | explicit ordered chain | none |
| `smart` | classify difficulty/domain → route easy→cheap chain, hard→frontier chain (§0.3 tiers) | classifier + logs |

Eligibility filter runs first for all policies: capability check (tools? vision? context
fits?) so `cheapest` never routes an image to a text-only model.

---

## 8. Config (`package config`)

One `relay.yaml`, env interpolation `${VAR}`, hot reload via fsnotify → parse → validate
→ atomic swap of an immutable snapshot (in-flight requests keep the old one; broken
edits are rejected and logged, last-good config stays live).

```yaml
server:
  listen: "127.0.0.1:4000"
  api_keys: ["${RELAY_API_KEY}"]      # inbound auth; empty allowed on loopback only

providers:
  openai:
    api_key: ["${OPENAI_API_KEY}", "${OPENAI_API_KEY_2}"]   # list = key pool
  anthropic:
    api_key: ["${ANTHROPIC_API_KEY}"]
  gemini:
    api_key: ["${GEMINI_API_KEY}"]
  local:
    type: openai-compat                # the escape hatch
    base_url: "http://localhost:8000/v1"
  groq:
    type: openai-compat
    profile: groq                      # preset: base_url + quirk table
    api_key: ["${GROQ_API_KEY}"]
  ollama:
    type: ollama                       # auto-discovers models via /api/tags
    base_url: "http://localhost:11434"

routing:
  default: smart                        # policy for unaliased model names
  aliases:
    fast:  [groq/llama-3.3-70b, openai/gpt-4o-mini]
    cheap: { policy: cheapest, candidates: [ollama/*, openai/gpt-4o-mini] }
    best:  [anthropic/claude-opus-4-8, openai/gpt-5]
  smart:
    easy:  cheap                        # alias refs
    hard:  best
    embeddings: ollama/nomic-embed-text # optional: enables tier-2 KNN

reliability: { retries: 3, ttft_timeout: 30s, request_timeout: 5m }
cache:       { enabled: false, ttl: 10m }
logging:     { db: "~/.relay/relay.db", retain: 90d, log_prompts: off }   # off | embeddings | full
translate:   { strictness: warn }
```

**Zero-config mode**: no `relay.yaml` found → sniff well-known env vars
(`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `GROQ_API_KEY`, …), probe
`localhost:11434` for Ollama, register whatever answers, `static` routing, loopback
listen. `relay serve` with just env keys exported → working gateway. `relay init`
scaffolds the yaml interactively.

`log_prompts` is a three-tier setting (review condition, binding):

- `off` (default) — request *metadata* only. Privacy-by-default even from yourself.
- `embeddings` — store the query **embedding vector** (which the smart router computes
  anyway when the Ollama tier is enabled) but **never the raw text**. This captures
  most of the KNN training value with none of the "my prompts are sitting in a SQLite
  file" discomfort. Requires an embedding source; falls back to `off` with a logged
  warning if none is configured.
- `full` — raw prompt/response bodies, for maximum training fidelity. `relay init`
  explains the trade-off explicitly.

---

## 9. Observability

**SQLite** (`modernc.org/sqlite`, pure Go; WAL mode; async writer goroutine with a
bounded queue — logging never blocks or fails a request):

```sql
CREATE TABLE requests (
  id TEXT PRIMARY KEY, ts INTEGER, api TEXT,            -- inbound dialect
  model_requested TEXT, model_served TEXT, provider TEXT,
  route_policy TEXT, route_reason TEXT, attempts INTEGER, candidates_json TEXT,
  status INTEGER, error_code TEXT,
  tokens_in INTEGER, tokens_out INTEGER, cost_usd REAL,
  latency_ms INTEGER, ttft_ms INTEGER, stream INTEGER, cached INTEGER,
  prompt_hash TEXT,                                     -- always
  prompt_embedding BLOB,                                -- log_prompts: embeddings|full (float32 LE vector)
  prompt_body TEXT, response_body TEXT,                 -- log_prompts: full only
  feedback_score REAL                                   -- §0.4, reserved now
);
```

**Prometheus** `/metrics`: request counts/latency/TTFT histograms by (provider, model,
policy, status), tokens + cost counters, breaker state, key-pool cooldowns, cache hits.

**Dashboard** `/dashboard`: one `go:embed` HTML page, minimal vanilla JS fetching two
JSON endpoints — spend by model/provider/day, latency percentiles, recent routing
decisions with reasons. No framework, no build step.

**Pricing registry**: `assets/pricing.json` embedded at build ($/Mtok in/out, context,
capabilities per model), overridable by a local file, refreshable from the project's
GitHub raw URL **only on explicit `relay pricing update`** — never automatic (that
would be phone-home).

---

## 10. Caching

Exact-match only, opt-in. Key = SHA-256 of canonicalized IR (model resolved, key-order
normalized). Only cacheable when `temperature == 0` or the client sends
`x-relay-cache: allow`. Memory LRU, optional SQLite spill. Streams replay as synthetic
events. Semantic cache: documented extension point (interface exists, one impl: exact).

---

## 11. Quality bars (restated as CI gates)

- `go vet` + `golangci-lint` + `-race` clean; coverage gate >80% on `translate`, `router`
- **Race gate (review condition, binding):** `-race` is unsupported on windows/arm64
  dev machines, so every phase gate runs it in a container before review:
  `docker run --rm -v .:/app -w /app golang:1.25 go test -race ./...`
- Fuzz targets on both inbound parsers; zero panics on malformed input
- Fixture-only provider tests — CI never touches a real API
- Benchmark suite proving p50 overhead <5ms non-streaming, <2ms added TTFT (httptest
  upstream, measured deltas, run in CI with generous thresholds + locally with strict)
- Conventional commits; every exported symbol documented

## 12. Phase map (unchanged from your spec)

| Phase | Scope | Deliverable gate |
|---|---|---|
| 1 | OpenAI-in → openai/openaicompat/ollama adapters, streaming, static routing, config, SQLite | any OpenAI SDK at `localhost:4000` works vs Ollama + OpenAI |
| 2 | `/v1/messages` (+count_tokens), Anthropic+Gemini adapters, full cross-dialect translation, fixtures | Claude Code runs through relay via `ANTHROPIC_BASE_URL` |
| 3 | all routers except smart, reliability suite, aliases, pricing+cost, dashboard, embeddings endpoint, cache | |
| 4 | smart routing tiers, `relay train` / `relay eval` | eval report: smart vs static on cost+quality; §0.3 launch gate verdict |
| 5 | OSS polish: README w/ comparison table, CI, goreleaser, Dockerfile (distroless, static), brew tap, examples | |

## 13. Non-goals (v1)

No billing/accounts/multi-tenancy (single admin key), no hosted service, no semantic
cache impl, no mid-stream model switching, no `/v1/responses` (roadmap), no ONNX-in-
process (roadmap behind build tag), no web UI beyond the one dashboard page, **no
telemetry of any kind — ever, not even opt-in version pings in v1.**
