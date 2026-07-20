package evalx

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

const liveVerdictAsset = "../../assets/eval/verdict-v2-live-judged-2026-07-20.json"

// TestReadmeVerdictMatchesAsset binds the human-readable verdict table to
// the committed full-precision asset: every rendered delta must be the
// 4-decimal rounding of the asset value, the stored verdict must equal a
// recomputation from full precision, and no policy may be labelled a
// failure while its rounded delta visually sits on the boundary — unless
// the unrounded value is strictly below the bound AND the table carries
// the rounding note. Prevents a "−0.0200 vs 0.02 tolerance: FAIL" table
// from shipping without explanation.
func TestReadmeVerdictMatchesAsset(t *testing.T) {
	raw, err := os.ReadFile(liveVerdictAsset)
	if err != nil {
		t.Skipf("live verdict asset not present: %v", err)
	}
	var asset struct {
		Results []PolicyResult `json:"results_live_judged"`
		Verdict Verdict        `json:"verdict_live_judged"`
	}
	if err := json.Unmarshal(raw, &asset); err != nil {
		t.Fatal(err)
	}
	byName := map[string]PolicyResult{}
	for _, r := range asset.Results {
		byName[r.Policy] = r
	}
	base, ok := byName["static-frontier"]
	if !ok {
		t.Fatal("asset has no static-frontier baseline")
	}
	tol := asset.Verdict.QualityTolerance

	// 1. The stored verdict must equal a recomputation from full precision
	// (inclusive rule: mean >= baseline - tolerance, strictly cheaper).
	recomputed := Judge(asset.Results, "static-frontier", "smart-tier1", tol)
	if recomputed.Tier1Passes != asset.Verdict.Tier1Passes {
		t.Fatalf("stored tier1 verdict %v != recomputed %v", asset.Verdict.Tier1Passes, recomputed.Tier1Passes)
	}
	passing := map[string]bool{}
	for _, p := range recomputed.Passing {
		passing[p] = true
	}

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(readme)

	// 2. Every policy's rendered 4-decimal delta must appear verbatim.
	for _, r := range asset.Results {
		if r.Policy == "static-frontier" {
			continue
		}
		delta := r.MeanQuality - base.MeanQuality
		rendered := fmt.Sprintf("%+.4f", delta)
		rendered = strings.ReplaceAll(rendered, "-", "−") // README uses U+2212
		if !strings.Contains(text, rendered) {
			t.Errorf("README lacks the asset-derived delta %s for %s", rendered, r.Policy)
		}

		// 3. Boundary honesty: a policy that fails the rule must be
		// strictly below the bound at full precision; if its 4dp rounding
		// touches the bound, the rounding note must be present.
		if !passing[r.Policy] && r.CostUSD < base.CostUSD {
			if delta >= -tol {
				t.Errorf("%s labelled fail but full-precision delta %.17g is within inclusive tolerance %.17g", r.Policy, delta, tol)
			}
			if math.Abs(math.Round(delta*1e4)/1e4+tol) < 1e-9 || math.Round(delta*1e4)/1e4 >= -tol {
				// rounded value visually sits on/inside the bound
				if !strings.Contains(text, "Gate decisions use the unrounded values") {
					t.Errorf("%s rounds onto the boundary; README needs the rounding note", r.Policy)
				}
			}
		}
	}

	// 4. The disclaimer must accompany the table regardless.
	if !strings.Contains(text, "Values are shown rounded. Gate decisions use the unrounded values") {
		t.Error("README verdict table is missing the rounding disclaimer")
	}
}
