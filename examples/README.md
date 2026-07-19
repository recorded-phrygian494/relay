# relay examples

Every example assumes relay is running locally (`relay serve`, default
`127.0.0.1:4000`). Set provider keys as environment variables first — with no
relay.yaml at all, zero-config mode sniffs `OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `GROQ_API_KEY`, … and probes a local
Ollama.

| Example | What it shows |
|---|---|
| [curl.md](curl.md) | both dialects, streaming, embeddings, feedback — no SDK |
| [python-openai.md](python-openai.md) | OpenAI Python SDK pointed at relay |
| [typescript-openai.md](typescript-openai.md) | OpenAI TS SDK pointed at relay |
| [claude-code.md](claude-code.md) | Claude Code through relay via `ANTHROPIC_BASE_URL` |
| [claude-code-local.md](claude-code-local.md) | route Claude Code to a **local** Ollama model |
| [cursor.md](cursor.md) | Cursor's OpenAI-compatible override → relay |
| [sensitive-local-bulk-cheap.md](sensitive-local-bulk-cheap.md) | the alias recipe: private traffic local, bulk traffic cheap |
