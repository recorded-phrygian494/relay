# Claude Code through relay

Claude Code speaks the Anthropic dialect; relay serves it fully, including
`count_tokens` and streaming.

```bash
export ANTHROPIC_BASE_URL=http://localhost:4000
claude
```

That's it. Claude Code now runs through your gateway: every request is in the
local log with its routing decision, spend shows on `/dashboard`, and your
aliases/failover apply.

Useful variations:

- **Failover Claude Code across keys/providers** — put a fallback alias in
  relay.yaml and keep using real model names for everything else.
- **Cap spend visibility** — `relay stats --since 24h` shows exactly what
  Claude Code cost you today, per model.
- If you set inbound `server.api_keys`, also export
  `ANTHROPIC_API_KEY=<your relay key>` for Claude Code to send.

To route Claude Code to a *local* model instead, see
[claude-code-local.md](claude-code-local.md).
