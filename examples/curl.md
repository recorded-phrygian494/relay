# curl — both dialects, no SDK

## OpenAI dialect

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini/gemini-3.1-flash-lite","messages":[{"role":"user","content":"hello"}]}'
```

Streaming (SSE):

```bash
curl -N http://localhost:4000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini/gemini-3.1-flash-lite","stream":true,"messages":[{"role":"user","content":"count to 5"}]}'
```

## Anthropic dialect — same gateway, same providers

```bash
curl http://localhost:4000/v1/messages \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini/gemini-3.1-flash-lite","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}'
```

Yes: an Anthropic-shaped request served by Gemini. Cross-dialect translation
is the core of relay.

## Embeddings

```bash
curl http://localhost:4000/v1/embeddings \
  -H "Content-Type: application/json" \
  -d '{"model":"ollama/nomic-embed-text","input":["hello world"]}'
```

## Feedback (labels for `relay train`)

Every response's request id is in the local log (`/dashboard`). Score it:

```bash
curl http://localhost:4000/v1/feedback \
  -H "Content-Type: application/json" \
  -d '{"request_id":"req_...","score":0.9}'
```

## If you set an inbound key

Add `-H "Authorization: Bearer $RELAY_API_KEY"` (or `-H "x-api-key: ..."`)
to every request.
