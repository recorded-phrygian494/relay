# Routing eval set — provenance and limits

`evalset_v1.jsonl` is the committed, versioned eval set behind `relay eval` and the
DESIGN §0.3 launch gate. Read this before quoting any number produced from it.

## What it is

49 synthetic queries across seven domains (chitchat, qa, extraction, summarize,
code, math, reasoning), including adversarial families: **short-hard** (four-word
proof requests) and **long-easy** (long pastes that need a one-word reply).
Deterministically generated — same seed, same bytes:

```
go run ./tools/genevalset > assets/eval/evalset_v1.jsonl   # version v1, seed 1
```

Each row carries a difficulty label in [0,1] and a per-band quality label
(`cheap` / `mid` / `frontier`).

## Where the quality labels come from — and their limits

The quality labels are **synthetic assumptions, not measurements**. The curve
(in `tools/genevalset/main.go`):

```
frontier: q = 0.96 − 0.10·d
mid:      q = 0.96 − 0.30·d^1.3
cheap:    q = 0.96 − 0.55·d^1.5      (d = difficulty; −0.04 on hard code/math/reasoning
                                      for non-frontier; ±0.02 seeded jitter)
```

The shape encodes two well-known observations: model bands converge on trivial
traffic (the premise of routing at all) and cheap models fall off steepest as
difficulty rises. The *specific coefficients* are assumptions. Any eval verdict
derived from this set is a verdict **on this model of quality** — a starting
point, not a benchmark result. Nothing here was measured against live models.

## Known circularity — stated plainly

The tier-1 lexical classifier's weights were hand-calibrated against this same
set (recorded in `internal/smart/tier1_weights.json`), so tier-1 results on it
are **in-sample** and flattering. Mitigations: the adversarial families exist
precisely to punish surface-feature shortcuts, and the tier-2 seed reference set
(`internal/smart/seed_refs.jsonl`) is disjoint from this set — no shared
prompts. The honest numbers for *your* workload come from `relay train`
(labels from your own traffic) and future `relay eval --live` measurement runs.

## Versioning

Never edit `evalset_v1.jsonl` by hand. Changes go through the generator with a
version bump (`v2`, new file, keep `v1`), because verdicts cite the set version.
