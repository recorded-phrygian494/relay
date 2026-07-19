package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/evalx"
	"github.com/llmrelay/relay/internal/pricing"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/server"
)

// liveJudgeMaxTokens caps replay completions: it bounds both the spend
// estimate (worst case is computable before confirming) and the judge's
// input size. Long-form answers to the hard rows fit comfortably.
const liveJudgeMaxTokens = 700

type pairKey struct {
	rowID string
	spec  string // provider/model
}

type pairResult struct {
	score   float64
	costUSD float64
	note    string
}

// livePairs collects the unique (row, model) pairs the dry-run decisions
// chose — each is completed and judged once, shared across policies.
func livePairs(results []evalx.PolicyResult) []pairKey {
	seen := map[pairKey]bool{}
	var out []pairKey
	for _, res := range results {
		for _, d := range res.Decisions {
			if d.Provider == "" {
				continue
			}
			k := pairKey{d.RowID, d.Provider + "/" + d.Model}
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].rowID != out[j].rowID {
			return out[i].rowID < out[j].rowID
		}
		return out[i].spec < out[j].spec
	})
	return out
}

// liveEstimate is the worst-case spend for the live-judged run, priced
// from the registry with completions capped at liveJudgeMaxTokens.
func liveEstimate(set *evalx.Set, pairs []pairKey, cands evalx.Candidates, reg *pricing.Registry, judgeProvName, judgeModel string) (candUSD, judgeUSD float64) {
	rows := map[string]evalx.Row{}
	for _, r := range set.Rows {
		rows[r.ID] = r
	}
	byModel := map[string]evalx.ModelRef{}
	for _, ref := range cands {
		byModel[ref.Provider+"/"+ref.Model] = ref
	}
	jIn, jOut, _ := reg.Price([]string{judgeProvName}, judgeModel)
	for _, p := range pairs {
		row := rows[p.rowID]
		tokIn := evalx.EstTokensIn(row.Prompt)
		if ref, ok := byModel[p.spec]; ok {
			candUSD += (float64(tokIn)*ref.InPrice + liveJudgeMaxTokens*ref.OutPrice) / 1e6
		}
		judgeUSD += (float64(tokIn+liveJudgeMaxTokens)*jIn + 16*jOut) / 1e6
	}
	return candUSD, judgeUSD
}

// withRetry runs f with backoff on retryable provider errors (free-tier
// rate limits are expected here).
func withRetry(ctx context.Context, f func() error) error {
	delays := []time.Duration{0, 3 * time.Second, 8 * time.Second, 20 * time.Second}
	var err error
	for _, d := range delays {
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = f(); err == nil {
			return nil
		}
		var pe *provider.Error
		if errors.As(err, &pe) {
			if !pe.Retryable {
				return err
			}
			if pe.RetryAfter > 0 {
				select {
				case <-time.After(pe.RetryAfter):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			continue
		}
		return err
	}
	return err
}

// liveRejudge replays every unique (row, model) pair with a real
// completion, scores it with the judge, and rebuilds each policy's
// results with measured quality and usage-priced cost. Judge spend is
// harness overhead, reported separately, never attributed to a policy.
func liveRejudge(ctx context.Context, rt *server.Runtime, reg *pricing.Registry, set *evalx.Set, results []evalx.PolicyResult, judgeProvName, judgeModel string) ([]evalx.PolicyResult, float64, float64, error) {
	judgeProv, ok := rt.Providers[judgeProvName]
	if !ok {
		return nil, 0, 0, fmt.Errorf("judge provider %q not configured", judgeProvName)
	}
	rows := map[string]evalx.Row{}
	for _, r := range set.Rows {
		rows[r.ID] = r
	}

	pairs := livePairs(results)
	measured := map[pairKey]pairResult{}
	var candSpend, judgeSpend float64
	jIn, jOut, _ := reg.Price([]string{judgeProvName}, judgeModel)

	// A small worker pool keeps wall time reasonable without hammering
	// free-tier rate limits (withRetry absorbs 429s).
	const workers = 4
	var mu sync.Mutex
	var done atomic.Int64
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, pk := range pairs {
		wg.Add(1)
		sem <- struct{}{}
		go func(pk pairKey) {
			defer wg.Done()
			defer func() { <-sem }()
			row := rows[pk.rowID]
			provName, model, _ := cutSpec(pk.spec)
			p, okP := rt.Providers[provName]
			if !okP {
				mu.Lock()
				measured[pk] = pairResult{note: "provider missing"}
				mu.Unlock()
				return
			}
			var answer string
			var usage core.Usage
			err := withRetry(ctx, func() error {
				var e error
				answer, usage, e = completeTextN(ctx, p, model, row.Prompt, liveJudgeMaxTokens)
				return e
			})
			pr := pairResult{}
			if err != nil {
				// A model that cannot answer scores zero — a real quality
				// signal; the note says what happened.
				pr.note = "completion failed: " + err.Error()
				mu.Lock()
				measured[pk] = pr
				mu.Unlock()
				fmt.Printf("  [%d/%d] %s × %s: FAILED (%v)\n", done.Add(1), len(pairs), pk.rowID, pk.spec, err)
				return
			}
			var cost float64
			if in, out, okPr := reg.Price([]string{provName}, model); okPr {
				cost = (float64(usage.InputTokens)*in + float64(usage.OutputTokens)*out) / 1e6
				pr.costUSD = cost
			}
			var score float64
			err = withRetry(ctx, func() error {
				var e error
				score, e = judgeScore(ctx, judgeProv, judgeModel, row.Prompt, answer)
				return e
			})
			mu.Lock()
			defer mu.Unlock()
			candSpend += cost
			if err != nil {
				pr.note = "judge failed: " + err.Error()
				measured[pk] = pr
				fmt.Printf("  [%d/%d] %s × %s: judge FAILED (%v)\n", done.Add(1), len(pairs), pk.rowID, pk.spec, err)
				return
			}
			judgeSpend += (float64(evalx.EstTokensIn(row.Prompt+answer))*jIn + 16*jOut) / 1e6
			pr.score = score
			measured[pk] = pr
			if n := done.Add(1); n%20 == 0 || n == int64(len(pairs)) {
				fmt.Printf("  [%d/%d] measured (last: %s × %s → %.2f)\n", n, len(pairs), pk.rowID, pk.spec, score)
			}
		}(pk)
	}
	wg.Wait()

	// Integrity guard: infrastructure failures (exhausted credits, daily
	// quotas, dead providers) are not quality signals. A run where a
	// meaningful share of completions or judge calls failed would emit a
	// poisoned table — refuse instead.
	failed := 0
	for _, m := range measured {
		if m.note != "" {
			failed++
		}
	}
	if len(pairs) == 0 || failed*10 > len(pairs) { // >10% failed
		return nil, candSpend, judgeSpend, fmt.Errorf(
			"live-judged run is INVALID: %d/%d pairs failed (credits/quotas/outages, see log above) — no verdict table will be produced from partial data; fix the account limits and re-run",
			failed, len(pairs))
	}

	// Rebuild policy results with measured numbers.
	judged := make([]evalx.PolicyResult, 0, len(results))
	for _, res := range results {
		jr := evalx.PolicyResult{Policy: res.Policy, ByBand: map[evalx.Band]int{}}
		n := 0
		for _, d := range res.Decisions {
			if d.Provider == "" {
				jr.Errors++
				continue
			}
			pk := pairKey{d.RowID, d.Provider + "/" + d.Model}
			m, okM := measured[pk]
			if !okM {
				jr.Errors++
				continue
			}
			nd := d
			nd.Quality, nd.CostUSD = m.score, m.costUSD
			if m.note != "" {
				nd.Reason = "[" + m.note + "] " + nd.Reason
			}
			jr.CostUSD += m.costUSD
			jr.MeanQuality += m.score
			jr.ByBand[d.Band]++
			jr.Decisions = append(jr.Decisions, nd)
			n++
		}
		if n > 0 {
			jr.MeanQuality /= float64(n)
		}
		judged = append(judged, jr)
	}
	sort.SliceStable(judged, func(i, j int) bool { return judged[i].CostUSD < judged[j].CostUSD })
	return judged, candSpend, judgeSpend, nil
}

func cutSpec(spec string) (prov, model string, ok bool) {
	for i := 0; i < len(spec); i++ {
		if spec[i] == '/' {
			return spec[:i], spec[i+1:], true
		}
	}
	return spec, "", false
}
