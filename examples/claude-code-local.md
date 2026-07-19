# Route Claude Code to a local model

Claude Code sends Anthropic-dialect requests naming Claude models. relay can
statically remap those names to anything — including an Ollama model running
on your machine. Zero tokens leave your laptop.

## 1. Pull a capable local model

```bash
ollama pull gpt-oss:20b     # or any strong local model you can run
```

## 2. relay.yaml

```yaml
providers:
  ollama:
    type: ollama

routing:
  static:
    # remap the model names Claude Code asks for
    claude-sonnet-5: "ollama/gpt-oss:20b"
    claude-haiku-4-5: "ollama/gpt-oss:20b"
```

## 3. Point Claude Code at relay

```bash
export ANTHROPIC_BASE_URL=http://localhost:4000
claude
```

Claude Code asks for `claude-sonnet-5`; relay serves `ollama/gpt-oss:20b` and
translates the Anthropic dialect to Ollama's, streaming included. The
decisions log shows every remap: `static: configured route "claude-sonnet-5"
→ "ollama/gpt-oss:20b"`.

## Honest expectations

A local 20B model is not a frontier Claude model — tool-heavy agentic flows
will degrade. This recipe shines for: air-gapped/sensitive codebases, flights,
rate-limit outages, and CI-ish chores. Mix modes with an alias instead of a
static remap if you only want *some* traffic local — see
[sensitive-local-bulk-cheap.md](sensitive-local-bulk-cheap.md).
