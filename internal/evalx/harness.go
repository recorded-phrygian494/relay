package evalx

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/llmrelay/relay/internal/core"
)

// ModelRef is one concrete candidate model with its band and $/Mtok
// prices from the pricing registry.
type ModelRef struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Band     Band    `json:"band"`
	InPrice  float64 `json:"in_per_mtok"`
	OutPrice float64 `json:"out_per_mtok"`
}

// Candidates is the band → model mapping an eval run uses.
type Candidates map[Band]ModelRef

// Policy is one routing strategy under evaluation. Decide must be
// deterministic and must not spend money in dry-run harnesses; the smart
// tiers surface their classifier explanation via the reason string.
type Policy interface {
	Name() string
	Decide(ctx context.Context, req *core.Request, cands Candidates) (ModelRef, string, error)
}

// StaticPolicy always routes to one band — the baseline family.
type StaticPolicy struct{ Band Band }

// Name implements Policy.
func (s StaticPolicy) Name() string { return "static-" + string(s.Band) }

// Decide implements Policy.
func (s StaticPolicy) Decide(_ context.Context, _ *core.Request, cands Candidates) (ModelRef, string, error) {
	ref, ok := cands[s.Band]
	if !ok {
		return ModelRef{}, "", fmt.Errorf("no %s candidate configured", s.Band)
	}
	return ref, fmt.Sprintf("static: always %s/%s", ref.Provider, ref.Model), nil
}

// CheapestPolicy routes every query to the lowest-priced candidate,
// mirroring the alias `cheapest` policy over the same candidate set.
type CheapestPolicy struct{}

// Name implements Policy.
func (CheapestPolicy) Name() string { return "cheapest" }

// Decide implements Policy.
func (CheapestPolicy) Decide(_ context.Context, req *core.Request, cands Candidates) (ModelRef, string, error) {
	var best ModelRef
	bestCost := math.Inf(1)
	for _, ref := range cands {
		// Rank by blended price, matching router.Cheapest's 3:1 out:in blend.
		c := ref.InPrice + 3*ref.OutPrice
		if c < bestCost {
			bestCost, best = c, ref
		}
	}
	if math.IsInf(bestCost, 1) {
		return ModelRef{}, "", fmt.Errorf("no candidates configured")
	}
	return best, fmt.Sprintf("cheapest: %s/%s by blended price", best.Provider, best.Model), nil
}

// Decision is one per-query routing decision in the report.
type Decision struct {
	RowID      string  `json:"row_id"`
	Provider   string  `json:"provider"`
	Model      string  `json:"model"`
	Band       Band    `json:"band"`
	Quality    float64 `json:"quality"`
	CostUSD    float64 `json:"cost_usd"`
	Reason     string  `json:"reason"`
	Difficulty float64 `json:"label_difficulty"`
}

// PolicyResult aggregates one policy over the whole set.
type PolicyResult struct {
	Policy      string       `json:"policy"`
	CostUSD     float64      `json:"cost_usd"`
	MeanQuality float64      `json:"mean_quality"`
	// ValidN is how many decisions carry a valid quality observation
	// (live-judged runs exclude missing judgements from the mean; 0 means
	// "all of them" for dry runs, which never have missing labels).
	ValidN    int          `json:"valid_n,omitempty"`
	ByBand    map[Band]int `json:"routed_by_band"`
	Errors    int          `json:"errors"`
	Decisions []Decision   `json:"decisions"`
}

// EstTokensIn approximates prompt tokens for cost simulation (chars/4,
// floor 1) — the standard rough heuristic, stated in the report.
func EstTokensIn(prompt string) int {
	n := len(prompt) / 4
	if n < 1 {
		n = 1
	}
	return n
}

// Run replays every row through every policy. No provider traffic happens
// here: policies that need I/O (tier 2 embedding) do it through the
// closure they were built with, which the caller controls.
func Run(ctx context.Context, set *Set, cands Candidates, policies []Policy) ([]PolicyResult, error) {
	results := make([]PolicyResult, 0, len(policies))
	for _, p := range policies {
		res := PolicyResult{Policy: p.Name(), ByBand: map[Band]int{}}
		for _, row := range set.Rows {
			req := &core.Request{
				Model: "eval",
				Messages: []core.Message{
					{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: row.Prompt}}},
				},
			}
			ref, reason, err := p.Decide(ctx, req, cands)
			if err != nil {
				res.Errors++
				res.Decisions = append(res.Decisions, Decision{RowID: row.ID, Reason: "error: " + err.Error(), Difficulty: row.Difficulty})
				continue
			}
			q, ok := row.Quality[ref.Band]
			if !ok {
				return nil, fmt.Errorf("row %s has no quality label for band %s", row.ID, ref.Band)
			}
			cost := (float64(EstTokensIn(row.Prompt))*ref.InPrice + float64(row.OutTokens)*ref.OutPrice) / 1e6
			res.CostUSD += cost
			res.MeanQuality += q
			res.ByBand[ref.Band]++
			res.Decisions = append(res.Decisions, Decision{
				RowID: row.ID, Provider: ref.Provider, Model: ref.Model, Band: ref.Band,
				Quality: q, CostUSD: cost, Reason: reason, Difficulty: row.Difficulty,
			})
		}
		if n := len(set.Rows) - res.Errors; n > 0 {
			res.MeanQuality /= float64(n)
		}
		results = append(results, res)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].CostUSD < results[j].CostUSD })
	return results, nil
}
