# Provider quirks — recorded reality vs. documentation

Institutional memory: every place a provider's live behavior differs from its
docs (or from the "obvious" reading of them), with the date observed and how
we handle it. Rule of the repo: **recorded reality beats documentation**
(DESIGN §5.3). Each entry says how it was verified:

- **recorded** — captured by `tools/record` from a live API; fixture in
  `internal/translate/testdata/*/recorded/`
- **live-observed** — seen in real traffic through the gateway (fixture
  synthesized afterwards)
- **docs-derived** — believed from documentation, not yet verified against a
  recording; treat as provisional

## Anthropic (Messages API, version 2023-06-01)

### `role:"system"` inside the messages array — live-observed 2026-07-14; **reversed by the API 2026-07-16**
The docs say `messages[].role` is `user | assistant` only. Claude Code sends
`role:"system"` entries inside `messages` on some code paths, and on
2026-07-14 the live API tolerated it. Discovered when Claude Code's first
request through the gateway was rejected by our then-strict parser.

**2026-07-16 probe:** the live API now **rejects** it — 400
`"messages.0: use the top-level 'system' parameter for the initial system
prompt; the directive-only form (content: [] with output_config) is accepted
at any position"`. Provider behavior changed under us within 48 hours, which
is exactly why this file exists.
**Handling (unchanged, now load-bearing):** parser accepts it; outbound
codecs hoist it into the `system` parameter — so the gateway *repairs*
requests the live API would now reject. (`internal/api/anthropic/types.go`)

### Thinking signatures are validated on replay — recorded 2026-07-16
`signature_delta` stream events are not decorative: clients replay thinking
blocks *with* their signature on the next conversation turn and the API
validates them. A gateway that strips signatures breaks any multi-turn
extended-thinking conversation on turn 2 — invisibly, until someone uses
tools + thinking together.
**Handling:** dedicated stream event kinds carry `signature_delta` and
`redacted_thinking` through same-dialect hops; pinned by the binding
round-trip suite. Verified against recordings: `thinking` and
`thinking_stream` fixtures (claude-haiku-4-5) carry real signatures and pass
the round-trip identity suite.

### `529 overloaded_error` — docs-derived
Anthropic sheds load with HTTP 529, outside the usual retryable-status
heuristics. **Handling:** always retryable. (`anthropicprov.apiError`)

### Streaming usage is split across the envelope — recorded 2026-07-16, with a correction
`input_tokens` arrives in `message_start`; the final `message_delta` carries
`output_tokens` — and, per the recordings, now **repeats** `input_tokens`
and the cache counters too, so the final delta is a complete usage object
(the "split" is no longer strict). Thinking runs add
`output_tokens_details.thinking_tokens`. **Handling:** the stream adapter
stitches both into one Usage event after `message_stop`; still correct
either way.

### `max_tokens` is required — live-verified 2026-07-16
Omitting it is a 400: `"max_tokens: Field required"`. OpenAI-dialect clients
routinely omit it. **Handling:** inject a default of 4096 on translation
(`translate.DefaultAnthropicMaxTokens`).

### New/undocumented response fields — recorded 2026-07-16
The corpus (claude-haiku-4-5, version header 2023-06-01) shows fields absent
from the published schema: `stop_details` (null in all recordings) on
messages and `message_delta`; `caller: {"type":"direct"}` on `tool_use`
blocks; usage gained `cache_creation` (`ephemeral_5m_input_tokens` /
`ephemeral_1h_input_tokens` breakdown), `service_tier`, and `inference_geo`.
**Handling:** unknown-field passthrough (`Ext` / `BlockExt`) carries all of
them; the same-dialect round-trip identity suite passes over the corpus, so
none of these are silently dropped.

### SSE cosmetics: padded data lines, spaced ping — recorded 2026-07-16
Anthropic pads `data:` lines with trailing whitespace after the JSON
(variable width; likely buffer-timing related) and formats ping as
`{"type": "ping"}` (space after colon, unlike every other event).
**Handling:** the SSE reader trims; comparisons are on parsed JSON. Noted so
nobody ever writes a byte-exact stream assertion against live traffic.

## Ollama

### OpenAI-compat endpoint: `content:""` instead of `null` — recorded 2026-07-14
For pure tool-call assistant messages, OpenAI proper emits `"content": null`;
Ollama's `/v1` endpoint emits `"content": ""`. Semantically identical.
**Handling:** the identity comparator folds them together; the gateway emits
the OpenAI-proper form.

### OpenAI-compat: streaming-style `index` in non-streaming tool_calls — recorded 2026-07-14
Non-streaming responses include `"index": 0` inside `tool_calls` entries — a
field that only means something in streaming deltas. **Handling:** parsed and
ignored; comparator treats it as positional noise.

### OpenAI-compat streaming: whole tool call in one delta — recorded 2026-07-14
OpenAI fragments tool-call arguments across many chunks; Ollama sends the
complete call (id + name + full arguments) in a single delta, repeats
`"role":"assistant"` on every chunk, and sends `content:""` alongside
tool_calls. **Handling:** the stream parser is shape-agnostic — first sight
of a tool index opens the call, so one-shot and fragmented forms both work.

### Native API: no tool-call ids; `done_reason` ignores tool calls — recorded 2026-07-14
`/api/chat` tool calls carry no id (the gateway synthesizes stable ones), and
`done_reason` is `"stop"` even when the model called tools. **Handling:**
adapter forces stop reason `tool_use` whenever tool calls were seen.

## OpenAI

### `max_tokens` vs `max_completion_tokens` — docs-derived
Newer models reject the legacy `max_tokens` key. A gateway that silently
rewrites one to the other breaks either old servers (compat providers that
only know `max_tokens`) or new models. **Handling:** the IR remembers which
key the client used (`LegacyMaxTokens`) and re-emits the same one.

## Gemini (AI Studio v1beta)

Recorded corpus: gemini-3.1-flash-lite, 2026-07-16
(`internal/translate/testdata/gemini/recorded/`, replayed through the
adapter by `provider/gemini.TestRecordedFixtures`).

### Rejects `$schema` / `additionalProperties` in tool schemas — live-verified 2026-07-16
Standard JSON-Schema keywords in `functionDeclarations.parameters` are a 400
`INVALID_ARGUMENT`: `Unknown name "$schema" ... Cannot find field`.
**Handling:** stripped recursively (`gemini.sanitizeSchema`).

### Tool results reference function NAME, not call id — docs-derived, half-obsolete 2026-07-16
`functionResponse` was documented with no id field, and the adapter builds
an id→name map from prior assistant `functionCall`s. **Recorded reality:**
gemini-3-era models now DO emit an `id` on `functionCall`
(`"id":"Ru4BlAgl"`). The adapter currently ignores it and synthesizes its
own ids; the name-keyed mapping still works. Whether `functionResponse`
accepts/validates an id on replay is unverified — revisit when tool-loop
scenarios get recorded.

### `thoughtSignature` on parts — recorded 2026-07-16; enforcement recorded 2026-07-16
Gemini 3 models attach a `thoughtSignature` to text and `functionCall`
parts (including a signature-only empty `text:""` part at stream end), and
signatures MUST be returned on function-call replay in multi-turn
conversations — verified live, not just docs: replaying a recorded
`functionCall` without its signature is a 400 `INVALID_ARGUMENT`
("Function call is missing a thought_signature in functionCall parts...",
fixture: `recorded/missing_thought_signature/`); the identical replay with
the signature attached succeeds (fixture: `recorded/tool_replay/`). Same
failure class as Anthropic's thinking signatures, but harder: the client
replays history in *its* dialect, which has nowhere to carry a Gemini
signature through a stateless gateway.
**Handling (DESIGN §0.7, resolved 2026-07-17):** documented v1 limitation.
The adapter maps the validation 400 to the typed error
`gemini_missing_thought_signature` naming the limitation and workarounds,
and the gateway logs a structured warning (also appended to the route
reason) when it routes function-call replay to a Gemini 3 target. The
`multi_turn_tools: degraded` capability flag lets routing steer such
traffic elsewhere.

### OpenAI-compat endpoint (`/v1beta/openai/`) — recorded 2026-07-16
Google's own OpenAI-compatibility layer does not absorb the
thought-signature problem: it surfaces the signature in a nonstandard
`tool_calls[].extra_content.google.thought_signature` field (and
`message.extra_content` for text turns), and replaying an assistant
tool-call message without that field fails with the identical
missing-thought-signature 400. Standard OpenAI SDK object models drop
unknown fields, so typical clients hit the wall even through Google's own
compat layer. Two extra quirks: error bodies from the compat endpoint are
wrapped in a JSON **array** (`[{"error": {...}}]` — an `{"error":...}`
parser must unwrap it), and tool-call ids are 8-char opaque strings, not
OpenAI-style `call_*`. Fixtures: `internal/translate/testdata/googlecompat/`.
**Handling:** none yet — no google preset profile exists for the
openaicompat adapter; these recordings are its ground truth when it lands.

### Streaming shape — recorded 2026-07-16
Cumulative `usageMetadata` arrives on **every** chunk, not just the last;
the final chunk repeats the aggregate, carries `finishReason`, and may
contain an empty `text:""` part (sometimes signature-only). Function calls
arrive whole in a single chunk. No `[DONE]` sentinel — EOF ends the stream.
**Handling:** adapter overwrites usage per chunk, skips empty text parts,
synthesizes tool start/delta/end triples; replay-verified against the corpus.

### New metadata fields — recorded 2026-07-16
`finishMessage` ("Model generated function call(s).") alongside
`finishReason`, `promptTokensDetails` (per-modality breakdown),
`serviceTier`, `responseId`, `modelVersion`. Non-streaming responses are
pretty-printed (indented) JSON. **Handling:** ignored — Gemini is
outbound-only, so there is no round-trip obligation; usage extraction
doesn't need them.

### Model availability is per-account; gating uses 404 — live-observed 2026-07-16
`gemini-2.5-flash` returns 404 `NOT_FOUND` "no longer available to *new
users*" — the model exists, the account is gated. Free-tier quotas are
per-model (`gemini-2.0-flash` 429s on quota while `gemini-3.1-flash-lite`
works on the same key). **Handling:** none needed in the adapter; relevant
to router/pricing docs — a 404 from Gemini doesn't mean the model id is
wrong, and quota exhaustion on one model says nothing about siblings.

## Cross-dialect policy decisions (not quirks, but adjacent)

- **Temperature is clamped, never rescaled** (OpenAI 0–2 vs Anthropic 0–1):
  rescaling silently changes sampling behavior; clamping is predictable.
- **`n > 1` → Anthropic is rejected** with a clear 400, not silently degraded.
- **URL-only images are never fetched** by the gateway (privacy default);
  providers that need inline data (Ollama, Gemini) get a clear error instead.

---

*The recorded-corpus mismatch audit (Step 0 of Phase 3) ran 2026-07-16:
`tools/record` against Anthropic (claude-haiku-4-5-20251001, all 10
scenarios) and Gemini (gemini-3.1-flash-lite, all 4 scenarios), plus three
targeted live probes (schema keywords, missing max_tokens, role:"system").
Findings are folded in above. Open item from the audit: Gemini
`thoughtSignature` replay (DESIGN §0.7).*
