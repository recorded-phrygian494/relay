# Support the OpenAI Responses API (`/v1/responses`)

**Status: not in v0.1.0 — this issue is the binding fast-follow commitment
(DESIGN §0.5), filed at launch, not roadmap vapor.**

Codex and newer OpenAI SDKs default to the Responses API. relay v0.1.0 speaks
Chat Completions fully (the README compatibility table says exactly this) and
does not accept `/v1/responses` inbound.

## Scope for v1.1

- [ ] Inbound `/v1/responses` dialect package (`internal/api/responses`),
      translating to/from the core IR like the other two dialects
- [ ] Streaming event mapping (`response.*` event family → core events)
- [ ] Stateful features (`previous_response_id`): design note first — relay
      is stateless by design; likely same-dialect passthrough plus a
      documented cross-dialect limitation, argued in DESIGN §0 before code
- [ ] Recorded fixtures against the real API; fuzz target on the parser
- [ ] README compatibility row flips to "Responses API: full"

## Non-goals here

OpenAI-hosted built-in tools (web search, file search) execution — those are
server-side features of OpenAI's platform, not gateway surface.
