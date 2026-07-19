# Cursor through relay

Cursor supports overriding the OpenAI base URL, which points its
OpenAI-dialect traffic at relay.

1. relay must be reachable by Cursor. Same machine: run `relay serve`
   (loopback default is fine). If Cursor's backend proxies model calls for
   your setup, you need relay on an address it can reach — then inbound
   `server.api_keys` is **required** (relay refuses to run as an open proxy).

2. In Cursor: Settings → Models → **OpenAI API key** — enable "Override
   OpenAI Base URL", set:
   - Base URL: `http://localhost:4000/v1`
   - API key: your `RELAY_API_KEY` (any non-empty string on loopback)

3. Add the model names you want Cursor to offer — either real
   `provider/model` ids relay serves, or your aliases (`bulk`, `private`,
   ...). Aliases are the nicer experience: retarget them in relay.yaml
   without touching Cursor again.

Every Cursor request now shows up in `/dashboard` with its routing decision
and cost. Note: Cursor features that require Cursor's own hosted models
ignore the override; this covers the OpenAI-API-key path.
