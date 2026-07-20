# LAUNCH.md — the remaining human steps, in binding order

The final public release **cannot** happen before the darwin/arm64 artifact
passes its hardware test on the Mac mini (steps 9–13). Do not publish final
`v0.1.0` first and use the public release as the initial Mac test.

Distribution channels for v0.1.0: **GitHub Releases, Homebrew, Docker**.
(npm/PyPI placeholders were removed from the launch — see
`future/package-installers/README.md` for the bar they must meet first.)

Each step: exact command, what it does, expected output, rough time.
Total: **~60–75 min** across two machines (this PC + the Mac mini).

---

## 1. Resolve the three editorial markers — ~10 min, editorial judgement

```powershell
findstr /s /n "REVIEW:" docs\models-landscape.md
```

Three `<!-- REVIEW: -->` markers (Groq TTFT reputation, DeepSeek
quality-per-dollar reputation, Cerebras throughput). Keep, reword, or delete
each — the final wording is yours; nothing else in the file is opinion.
Delete the marker comments when done, then:

```powershell
cd C:\Users\faith\Router
git add docs/models-landscape.md
git commit -m "docs: editorial pass on models-landscape"
```

## 2. Create the GitHub org — web, ~2 min

Open https://github.com/account/organizations/new → plan **Free** → name
**llmrelay**. (Orgs cannot be created via `gh`.)

## 3. Create the repository as PRIVATE and push — ~3 min

```powershell
gh auth status
```
Expected: `Logged in to github.com`. If not: `gh auth login` (GitHub.com →
HTTPS → browser; the token must act on the `llmrelay` org).

```powershell
cd C:\Users\faith\Router
git branch -M main
gh repo create llmrelay/relay --private --source . --remote origin --push
```

Expected: `✓ Created repository llmrelay/relay` then push output ending
`branch 'main' set up to track 'origin/main'`. The repo stays **private**
until step 14.

## 4. Description, topics, security settings — ~2 min

```powershell
gh repo edit llmrelay/relay --description "Self-hosted LLM gateway + model router. One static binary, OpenAI & Anthropic dialects in, any provider out, zero telemetry." --add-topic llm --add-topic gateway --add-topic router --add-topic openai --add-topic anthropic --add-topic gemini --add-topic ollama --add-topic proxy --add-topic self-hosted --add-topic golang
```

Expected: `✓ Edited repository llmrelay/relay`. Then in the browser: repo →
Settings → Advanced Security → enable **Private vulnerability reporting**
(SECURITY.md points there).

## 5. Create the Homebrew tap repo — ~1 min

Required by the release configuration (formula lands there in step 14):

```powershell
gh repo create llmrelay/homebrew-tap --public --description "Homebrew tap for relay" --add-readme
```

Expected: `✓ Created repository llmrelay/homebrew-tap`.

## 6. Tag and build the RC (prerelease) — ~10 min

The release runs **locally via goreleaser** (no Actions release workflow or
secrets are configured for v0.1.0; a local run keeps the first release
observable end-to-end — adding a tag-triggered workflow is a v0.1.1 chore).
Docker images are skipped: ghcr auth isn't set up; the Dockerfile itself is
tested and users can build it. The `-rc1` tag is auto-marked **prerelease**
by the config; the formula upload is disabled in config until step 14.

```powershell
cd C:\Users\faith\Router
git tag -a v0.1.0-rc1 -m "relay v0.1.0-rc1 - prerelease for darwin/arm64 hardware verification"
git push origin main
git push origin v0.1.0-rc1
$env:GITHUB_TOKEN = (gh auth token)
go run github.com/goreleaser/goreleaser/v2@latest release --clean --skip=docker
```

Expected: 6-target build matrix, archives + `checksums.txt` uploaded,
`release succeeded`. The GitHub release is created as a **draft prerelease**
— open it and press **Publish** (it stays a prerelease on a private repo):

```powershell
gh release edit v0.1.0-rc1 --repo llmrelay/relay --draft=false
```

## 7–10. On the Mac mini: fetch and verify the RC artifact — ~10 min

On the Mac (needs `gh` + auth, since the repo is private):

```bash
gh auth login            # if not already
mkdir -p ~/relay-rc-test && cd ~/relay-rc-test
gh release download v0.1.0-rc1 --repo llmrelay/relay --pattern "*darwin_arm64*" --pattern "checksums.txt"
shasum -a 256 -c <(grep darwin_arm64 checksums.txt)
tar xzf relay_*_darwin_arm64.tar.gz
file relay && ./relay version
```

Expected, in order: checksum line ending `: OK`; `Mach-O 64-bit executable
arm64`; `relay 0.1.0-rc1`. Any mismatch → stop, report.

## 11. On the Mac mini: the RC test battery — ~10 min

From a clean temporary directory, with one provider key (the documented
minimum config — zero-config sniffs it):

```bash
cd "$(mktemp -d)"
export GEMINI_API_KEY=...        # any one supported provider key
~/relay-rc-test/relay serve > relay.log 2>&1 &
sleep 3
grep -E "zero-config|listening" relay.log
```

Expected: `no relay.yaml found; zero-config mode with 1 provider(s)` and
`listening on http://127.0.0.1:4000`.

```bash
# (a) real non-streaming completion
curl -s http://127.0.0.1:4000/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"gemini/gemini-3.1-flash-lite","messages":[{"role":"user","content":"Say RC-OK and nothing else."}]}'

# (b) SSE streaming completion
curl -sN http://127.0.0.1:4000/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"gemini/gemini-3.1-flash-lite","stream":true,"messages":[{"role":"user","content":"count to 3"}]}' | head -5

# (c) a supported tool call
curl -s http://127.0.0.1:4000/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"gemini/gemini-3.1-flash-lite","messages":[{"role":"user","content":"What is the weather in Lagos? Use the tool."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}'

# (d) loopback-only by default
lsof -iTCP:4000 -sTCP:LISTEN -n -P

# (e) smart routing disabled by default: unroutable name must 404, not route
curl -s http://127.0.0.1:4000/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}'

# (f) clean shutdown
kill -INT %1 && sleep 2 && tail -2 relay.log
```

Expected: (a) JSON with `"content":"RC-OK"`; (b) `data: {...delta...}` SSE
lines; (c) a `tool_calls` entry naming `get_weather` with a `city` argument;
(d) exactly one listener on `127.0.0.1:4000` (not `*:4000`);
(e) `model_not_found` with reason text — proves nothing routes by default;
(f) `shutting down` in the log, process gone.

## 12. Record the Mac evidence — ~5 min

Paste the actual outputs (checksum line, `file` output, version, and the
a–f results) into `docs/releases/v0.1.0-rc1-mac-arm64.md` in the repo,
commit and push:

```bash
git add docs/releases/v0.1.0-rc1-mac-arm64.md && git commit -m "docs: darwin/arm64 RC hardware evidence" && git push
```

## 13. If anything failed

Fix the smallest underlying issue, tag `v0.1.0-rc2`, repeat steps 6–12.
Do not proceed on a failed RC.

## 14. Final release — only after the RC passes — ~15 min

Back on this PC (pull the Mac evidence commit first):

```powershell
cd C:\Users\faith\Router
git pull
git tag -a v0.1.0 -F docs/releases/v0.1.0-tag.txt
git push origin v0.1.0
$env:GITHUB_TOKEN = (gh auth token)
go run github.com/goreleaser/goreleaser/v2@latest release --clean --skip=docker
gh release edit v0.1.0 --repo llmrelay/relay --notes-file docs/releases/v0.1.0-notes.md
gh release edit v0.1.0 --repo llmrelay/relay --draft=false
```

Expected: `release succeeded`, then the release is live (still private).
Before publishing the notes, delete the darwin caveat line in them — the RC
evidence replaced it. Now, in this order:

```powershell
# make the repository public
gh repo edit llmrelay/relay --visibility public --accept-visibility-change-consequences

# publish the Homebrew formula (rendered by goreleaser, upload disabled by config)
git clone https://github.com/llmrelay/homebrew-tap.git $env:TEMP\homebrew-tap
mkdir $env:TEMP\homebrew-tap\Formula -Force
copy dist\homebrew\Formula\relay.rb $env:TEMP\homebrew-tap\Formula\relay.rb
cd $env:TEMP\homebrew-tap
git add Formula/relay.rb; git commit -m "relay v0.1.0"; git push
```

Then the public Homebrew installation test (Mac mini or any Mac):

```bash
brew install llmrelay/tap/relay && relay version
```

Expected: `relay 0.1.0`. Finally, file the binding §0.5 tracking issue:

```powershell
cd C:\Users\faith\Router
gh issue create --repo llmrelay/relay --title "tracking: OpenAI Responses API (/v1/responses) - v1.1 fast-follow" --body-file .github/RESPONSES_API_ISSUE.md
```

Expected: `https://github.com/llmrelay/relay/issues/1`.

## 15. Public post-launch checks — ~5 min

```powershell
gh release view v0.1.0 --repo llmrelay/relay --web   # notes render, assets present
```

- README renders correctly on the public page (tables, links).
- CI is green on `main` (the push triggered it).
- `docker build -t relay .` from a fresh clone (optional but quick).
- Optional: register llmrelay.dev and `gh repo edit llmrelay/relay
  --homepage "https://llmrelay.dev"`; add a security@ mailbox to SECURITY.md
  only when the domain and mail exist.
