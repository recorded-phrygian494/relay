---
name: "tracking: OpenAI Responses API"
about: "Binding v1.1 fast-follow (DESIGN §0.5) — file this verbatim at launch"
labels: tracking, v1.1
---

# Support the OpenAI Responses API (`/v1/responses`)

**Status: not in v1 — this issue is the binding fast-follow commitment
(DESIGN §0.5), opened at launch, not roadmap vapor.**

Codex and newer OpenAI SDKs default to the Responses API. relay v1 speaks
Chat Completions fully (README compatibility table says exactly this) but
does not accept `/v1/responses` inbound.

Scope for v1.1:
- [ ] Inbound `/v1/responses` dialect package (`internal/api/responses`),
      translating to/from the core IR like the other two dialects
- [ ] Streaming event mapping (response.* event family → core events)
- [ ] Stateful features (`previous_response_id`) — design note first: relay
      is stateless by design; likely same-dialect passthrough + documented
      limitation cross-dialect, argued in DESIGN §0 before code
- [ ] Fixtures recorded against the real API; fuzz target on the parser
- [ ] README compatibility row flips to "Responses API: full"

Non-goals here: built-in tools (web search, file search) execution — those
are OpenAI-hosted server-side features, not gateway surface.
