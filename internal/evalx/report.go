package evalx

import (
	"fmt"
	"strings"
)

// Verdict is the machine-readable §0.3 launch-gate outcome.
type Verdict struct {
	Baseline         string  `json:"baseline"`
	QualityTolerance float64 `json:"quality_tolerance"`
	// Passing lists every policy that beats the baseline on cost at equal
	// quality: mean quality within tolerance of baseline AND strictly
	// lower cost.
	Passing []string `json:"passing"`
	// Tier1Passes is THE launch gate: does the pure-Go lexical tier earn
	// on-by-default status?
	Tier1Passes bool   `json:"tier1_passes_gate"`
	Rule        string `json:"rule"`
}

// Judge applies the §0.3 gate to a result set.
func Judge(results []PolicyResult, baselineName, tier1Name string, tolerance float64) Verdict {
	v := Verdict{
		Baseline:         baselineName,
		QualityTolerance: tolerance,
		Rule: fmt.Sprintf("pass = mean_quality >= %s mean_quality - %.3f AND cost_usd < %s cost_usd",
			baselineName, tolerance, baselineName),
	}
	var baseline *PolicyResult
	for i := range results {
		if results[i].Policy == baselineName {
			baseline = &results[i]
		}
	}
	if baseline == nil {
		return v
	}
	for _, r := range results {
		if r.Policy == baselineName || r.Errors > 0 {
			continue
		}
		if r.MeanQuality >= baseline.MeanQuality-tolerance && r.CostUSD < baseline.CostUSD {
			v.Passing = append(v.Passing, r.Policy)
			if r.Policy == tier1Name {
				v.Tier1Passes = true
			}
		}
	}
	return v
}

// Table renders the human-readable comparison.
func Table(results []PolicyResult, v Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %12s %9s %8s %s\n", "POLICY", "COST_USD", "QUALITY", "ERRORS", "ROUTED (cheap/mid/frontier)")
	var baseCost, baseQ float64
	for _, r := range results {
		if r.Policy == v.Baseline {
			baseCost, baseQ = r.CostUSD, r.MeanQuality
		}
	}
	for _, r := range results {
		mark := " "
		for _, p := range v.Passing {
			if p == r.Policy {
				mark = "*"
			}
		}
		if r.Policy == v.Baseline {
			mark = "="
		}
		delta := ""
		if r.Policy != v.Baseline && baseCost > 0 {
			delta = fmt.Sprintf("  (cost %+.0f%%, quality %+.3f)", (r.CostUSD/baseCost-1)*100, r.MeanQuality-baseQ)
		}
		fmt.Fprintf(&b, "%s%-15s %12.6f %9.3f %8d %d/%d/%d%s\n",
			mark, r.Policy, r.CostUSD, r.MeanQuality, r.Errors,
			r.ByBand[BandCheap], r.ByBand[BandMid], r.ByBand[BandFrontier], delta)
	}
	fmt.Fprintf(&b, "\n= baseline, * beats baseline on cost at equal quality (tolerance %.3f)\n", v.QualityTolerance)
	fmt.Fprintf(&b, "§0.3 launch gate — tier 1 on-by-default: %v\n", v.Tier1Passes)
	return b.String()
}
