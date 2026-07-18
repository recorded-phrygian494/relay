---
name: verify
description: Build, launch, and drive relay locally to verify changes end-to-end (server surface, both API dialects, dashboard/metrics).
---

# Verifying relay changes

Relay is a single Go HTTP server. Verify at the socket: launch it, send real
requests, read responses / scrape endpoints.

## Build & launch

```powershell
# Keys are User-level Windows env vars, NOT inherited by the session — load explicitly.
$env:GEMINI_API_KEY = [Environment]::GetEnvironmentVariable('GEMINI_API_KEY','User')
go build -o $scratch\relay.exe ./cmd/relay
& $scratch\relay.exe serve --config $scratch\relay.yaml   # run in background
```

Minimal scratch config (port 4100 avoids the default 4000):

```yaml
server: { listen: "127.0.0.1:4100" }
providers:
  gemini: { api_key: ["${GEMINI_API_KEY}"] }
routing:
  aliases:
    fast: [gemini/gemini-3.1-flash-lite]
logging: { db: "<scratchpad>/relay.db" }
```

Gotchas:
- `routing:` has no `default:` field until smart routing (phase 4) — setting it
  fails config load.
- Model for live calls: `gemini-3.1-flash-lite` (2.0-flash quota-exhausted,
  2.5-flash account-gated; see docs/quirks.md).
- Stop with `Stop-Process -Name relay -Force`.

## Drive

- OpenAI dialect: `POST /v1/chat/completions` with `{"model":"fast",...}`.
- Anthropic dialect: `POST /v1/messages` (add `"max_tokens"`, `"stream":true`
  to exercise SSE/TTFT).
- Embeddings: `POST /v1/embeddings` `{"model":"gemini/gemini-embedding-001","input":["a"]}`.
- Cache (needs `cache: {enabled: true}` in config): same body twice with
  `"temperature":0` → second decision logs `policy=cache`; counters in /metrics.
- Failure path: request a nonexistent model → 404 routed decision.
- `GET /dashboard/data` (JSON), `GET /metrics` (Prometheus), `GET /dashboard`.
- Overhead gate: `go test ./internal/server/ -run TestOverheadBudget -v`.
- Dashboard screenshot: headless Edge works —
  `& "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" --headless
  --disable-gpu --window-size=1200,900 --virtual-time-budget=6000
  --screenshot=$scratch\dash.png http://127.0.0.1:4100/dashboard`
- Piping JSON through `python -m json.tool` mojibakes UTF-8 (cp1252 stdin) —
  check raw bytes before blaming the server.
