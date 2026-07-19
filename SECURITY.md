# Security policy

## Reporting a vulnerability

Email **security@llmrelay.dev** (or use GitHub's private vulnerability
reporting on this repository). Please do not open public issues for
exploitable problems. We aim to acknowledge within 72 hours.

## Security posture (what relay promises)

- **Closed by default.** relay binds to loopback (`127.0.0.1:4000`) unless you
  configure otherwise. Binding to a non-loopback address with no inbound
  `server.api_keys` **refuses to start** — an open proxy that spends your
  provider keys is the one foot-gun this product must not ship. The override
  (`--insecure-no-auth`) is deliberate, explicit, and discouraged.
- **Keys stay yours.** Provider keys come from your environment/config and are
  only sent to the provider they belong to. Inbound auth accepts
  `Authorization: Bearer` or `x-api-key`, compared in constant time.
- **Zero telemetry.** No phone-home of any kind, ever — not even opt-in
  version pings. The pricing registry refreshes only on explicit
  `relay pricing update`.
- **Privacy tiers in the log.** `log_prompts: off` (default) stores request
  metadata only; `embeddings` stores query vectors, never text; `full` is an
  explicit choice. Smart routing never sends prompts to a remote embedder
  without `allow_remote_embeddings: true` in your config.

## Scope notes

relay is a single-admin gateway (one shared inbound key space); it is not a
multi-tenant billing boundary. Treat the SQLite log (`~/.relay/relay.db`) with
the same care as the traffic it records, especially under `log_prompts: full`.
