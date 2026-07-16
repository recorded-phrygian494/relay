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

### `role:"system"` inside the messages array — live-observed 2026-07-14
The docs say `messages[].role` is `user | assistant` only. Claude Code sends
`role:"system"` entries inside `messages` on some code paths, and the live
API tolerates it. Discovered when Claude Code's first request through the
gateway was rejected by our then-strict parser.
**Handling:** parser accepts it; outbound codecs hoist it into the `system`
parameter. (`internal/api/anthropic/types.go`)

### Thinking signatures are validated on replay — enforced 2026-07-15
`signature_delta` stream events are not decorative: clients replay thinking
blocks *with* their signature on the next conversation turn and the API
validates them. A gateway that strips signatures breaks any multi-turn
extended-thinking conversation on turn 2 — invisibly, until someone uses
tools + thinking together.
**Handling:** dedicated stream event kinds carry `signature_delta` and
`redacted_thinking` through same-dialect hops; pinned by the binding
round-trip suite. Pending recorded verification (`--scenario thinking_stream`).

### `529 overloaded_error` — docs-derived
Anthropic sheds load with HTTP 529, outside the usual retryable-status
heuristics. **Handling:** always retryable. (`anthropicprov.apiError`)

### Streaming usage is split across the envelope — docs-derived
`input_tokens` arrives in `message_start`, `output_tokens` (cumulative) in
the final `message_delta`. **Handling:** the stream adapter stitches both
into one Usage event after `message_stop`.

### `max_tokens` is required — docs-derived
OpenAI-dialect clients routinely omit it. **Handling:** inject a default of
4096 on translation (`translate.DefaultAnthropicMaxTokens`).

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

### Rejects `$schema` / `additionalProperties` in tool schemas — docs-derived
Standard JSON-Schema keywords in `functionDeclarations.parameters` cause API
errors. **Handling:** stripped recursively (`gemini.sanitizeSchema`).
Pending recorded verification.

### Tool results reference function NAME, not call id — docs-derived
`functionResponse` has no id field. **Handling:** the adapter builds an
id→name map from prior assistant `functionCall`s in the conversation.

## Cross-dialect policy decisions (not quirks, but adjacent)

- **Temperature is clamped, never rescaled** (OpenAI 0–2 vs Anthropic 0–1):
  rescaling silently changes sampling behavior; clamping is predictable.
- **`n > 1` → Anthropic is rejected** with a clear 400, not silently degraded.
- **URL-only images are never fetched** by the gateway (privacy default);
  providers that need inline data (Ollama, Gemini) get a clear error instead.

---

*The recorded-corpus mismatch audit (Step 0 of Phase 3) appends its findings
here once `tools/record` has been run against Anthropic and Gemini with real
keys.*
