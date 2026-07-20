# LAUNCH.md — the remaining human steps, in order

Everything below requires accounts/credentials only Faith has. Each step is a
literal copy-paste command (PowerShell-safe unless noted), with what it does,
expected output, and rough time. Total: **~35–45 minutes** including account
signups you may already have.

Machine prep already done by the repo: code, tags message, release notes,
issue body, placeholder packages, CI workflow, goreleaser config are all
committed. The only things left are accounts, pushes, and publishes.

---

## 0. One-time accounts (skip any you have)

- GitHub account with permission to create an organization.
- npm account (https://www.npmjs.com/signup) — for the name placeholder.
- PyPI account (https://pypi.org/account/register/) + an API token — for the
  name placeholder.

## 1. Create the GitHub org — web, ~2 min

Open https://github.com/account/organizations/new → plan **Free** →
organization name **llmrelay**. (Orgs cannot be created via `gh`.)

## 2. Authenticate gh — ~1 min

```powershell
gh auth status
```

Expected: `Logged in to github.com`. If not:

```powershell
gh auth login
```

Choose GitHub.com → HTTPS → login via browser. The token must be able to act
on the `llmrelay` org (default web-auth token can).

## 3. Create the repo, push, describe — ~3 min

Run from `C:\Users\faith\Router`:

```powershell
git branch -M main
gh repo create llmrelay/relay --public --source . --remote origin --push
```

Expected: `✓ Created repository llmrelay/relay` then push progress ending in
`branch 'main' set up to track 'origin/main'`.

```powershell
gh repo edit llmrelay/relay --description "Self-hosted LLM gateway + model router. One static binary, OpenAI & Anthropic dialects in, any provider out, zero telemetry." --homepage "https://github.com/llmrelay/relay" --add-topic llm --add-topic gateway --add-topic router --add-topic openai --add-topic anthropic --add-topic gemini --add-topic ollama --add-topic proxy --add-topic self-hosted --add-topic golang
```

Expected: `✓ Edited repository llmrelay/relay`. (This is the About text +
topics; homepage can later become https://llmrelay.dev, see step 9.)

## 4. Create the Homebrew tap BEFORE the release — ~1 min

goreleaser pushes the formula into this repo during the release, so it must
exist first:

```powershell
gh repo create llmrelay/homebrew-tap --public --description "Homebrew tap for relay" --add-readme
```

Expected: `✓ Created repository llmrelay/homebrew-tap`.

## 5. Tag and release — ~10 min

The release runs **locally via goreleaser**, not via a GitHub Action: no
release workflow/secrets are configured for v0.1.0, and a local run keeps the
first release observable end-to-end. (Adding a tag-triggered Action is a good
v0.1.1 chore.) Docker images are skipped for the same reason — ghcr push auth
is not set up; the Dockerfile itself is tested and users can build it.

```powershell
cd C:\Users\faith\Router
git tag -a v0.1.0 -F docs/release/v0.1.0-tag.txt
git push origin v0.1.0
$env:GITHUB_TOKEN = (gh auth token)
go run github.com/goreleaser/goreleaser/v2@latest release --clean --skip=docker
```

Expected: build matrix for linux/darwin/windows × amd64/arm64, archives +
checksums uploaded, `✓ release succeeded` — and the formula pushed to
llmrelay/homebrew-tap. The GitHub release is created as a **draft**
(configured on purpose): open it, paste `docs/release/v0.1.0-notes.md` as the
description (or run the command below), review, press **Publish**.

```powershell
gh release edit v0.1.0 --repo llmrelay/relay --notes-file docs/release/v0.1.0-notes.md
gh release edit v0.1.0 --repo llmrelay/relay --draft=false
```

## 6. File the binding Responses-API tracking issue — ~1 min

The §0.5 commitment: filed at launch, not later.

```powershell
gh issue create --repo llmrelay/relay --title "tracking: OpenAI Responses API (/v1/responses) - v1.1 fast-follow" --body-file .github/RESPONSES_API_ISSUE.md
```

Expected: a URL like `https://github.com/llmrelay/relay/issues/1`.

## 7. Publish the npm name placeholder — ~3 min

```powershell
cd C:\Users\faith\Router\tools\placeholders\npm
npm login
npm publish --access public
```

Expected: `+ llmrelay@0.0.1`. (If the name is taken, stop and reconsider the
package name — do not squat variants.)

## 8. Publish the PyPI name placeholder — ~4 min

Needs Python + a PyPI API token (create at https://pypi.org/manage/account/token/):

```powershell
cd C:\Users\faith\Router\tools\placeholders\pypi
pip install flit
$env:FLIT_USERNAME = "__token__"
$env:FLIT_PASSWORD = "pypi-...your token..."
flit publish
```

Expected: `Package is at https://pypi.org/project/llmrelay/0.0.1/`.

## 9. Optional: llmrelay.dev — ~5 min + DNS propagation

If you register the domain: easiest v1 is GitHub Pages redirect or just point
the repo's homepage at it (`gh repo edit llmrelay/relay --homepage
"https://llmrelay.dev"`). Also update `SECURITY.md`'s report address
(security@llmrelay.dev) to a mailbox that actually exists — **until then,
edit SECURITY.md to the GitHub private-vulnerability-reporting path only**,
or enable it: repo → Settings → Security → "Private vulnerability reporting".

## 10. Post-launch quick checks — ~3 min

```powershell
gh release view v0.1.0 --repo llmrelay/relay --web
brew install llmrelay/tap/relay   # on any Mac — also serves as the darwin smoke test
```

The darwin binaries shipped marked "built, not yet smoke-tested on hardware"
in the release notes; the first `brew install` + `relay version` on a Mac
closes that caveat (then edit the release notes line).

---

### Not on this list (already done by machine)

Snapshot builds + linux/amd64 container smoke + windows/arm64 native smoke,
release notes, tag message, issue body, CHANGELOG, placeholders, CI workflow,
README/DESIGN verdict sync, models-landscape trim (your 10-minute editorial
pass: search `REVIEW:` — 3 hits).
