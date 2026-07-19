package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/llmrelay/relay/internal/config"
	"github.com/llmrelay/relay/internal/evalx"
	"github.com/llmrelay/relay/internal/pricing"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/server"
	"github.com/llmrelay/relay/internal/smart"
)

// runEval is the §0.3 eval harness CLI: replay the labeled eval set
// through routing policies, dry-run — the only network traffic is tier-2
// query embedding, and only with --live (or a local embedder).
func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	setPath := fs.String("set", "assets/eval/evalset_v1.jsonl", "labeled eval set (JSONL)")
	cheap := fs.String("cheap", "gemini/gemini-3.1-flash-lite", "cheap-band candidate (provider/model)")
	mid := fs.String("mid", "gemini/gemini-3.5-flash", "mid-band candidate")
	frontier := fs.String("frontier", "anthropic/claude-fable-5", "frontier-band candidate")
	tolerance := fs.Float64("tolerance", 0.02, "quality tolerance for 'equal quality' (absolute, on [0,1])")
	jsonPath := fs.String("json", "", "write the machine-readable report+verdict to this path")
	embedder := fs.String("embedder", "", "provider/model for tier-2 KNN (omit to skip tier 2)")
	refs := fs.String("refs", "", "tier-2 reference set from relay train (default: embedded seed texts) — measure your own crossover")
	live := fs.Bool("live", false, "allow network calls (required for a remote --embedder)")
	liveJudge := fs.Bool("live-judge", false, "measure quality: replay routed choices with real completions and judge-score them (spends money; estimate prints first)")
	judgeSpec := fs.String("judge", "anthropic/claude-opus-4-8", "provider/model that scores live-judged replays")
	dryRun := fs.Bool("dry-run", false, "with --live-judge: print the spend estimate and stop")
	yes := fs.Bool("yes", false, "with --live-judge: confirm spending without a prompt")
	cfgPath := fs.String("config", "", "config file for provider credentials (default: discovery + env)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	set, err := evalx.LoadSet(*setPath)
	if err != nil {
		return err
	}
	reg, err := pricing.Load()
	if err != nil {
		return fmt.Errorf("pricing registry: %w", err)
	}
	cands := evalx.Candidates{}
	for band, spec := range map[evalx.Band]string{
		evalx.BandCheap: *cheap, evalx.BandMid: *mid, evalx.BandFrontier: *frontier,
	} {
		prov, model, ok := strings.Cut(spec, "/")
		if !ok {
			return fmt.Errorf("--%s: %q is not provider/model", band, spec)
		}
		in, out, priced := reg.Price([]string{prov}, model)
		if !priced {
			return fmt.Errorf("--%s: %s has no pricing entry — eval cost simulation would lie; add it to pricing.json first", band, spec)
		}
		cands[band] = evalx.ModelRef{Provider: prov, Model: model, Band: band, InPrice: in, OutPrice: out}
	}

	lex, err := smart.NewLexical()
	if err != nil {
		return err
	}
	policies := []evalx.Policy{
		evalx.StaticPolicy{Band: evalx.BandFrontier},
		evalx.StaticPolicy{Band: evalx.BandMid},
		evalx.StaticPolicy{Band: evalx.BandCheap},
		evalx.CheapestPolicy{},
		evalx.TierPolicy{PolicyName: "smart-tier1", Classifier: lex},
	}

	// Live-judged runs make hundreds of real completions; give them an
	// hour, not the dry-run budget (a mid-run deadline zeroes scores and
	// invalidates the whole table).
	budget := 10 * time.Minute
	if *liveJudge {
		budget = time.Hour
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	if *embedder != "" {
		knn, err := buildEvalKNN(ctx, *cfgPath, *embedder, *refs, *live, lex)
		if err != nil {
			return err
		}
		policies = append(policies, evalx.TierPolicy{PolicyName: "smart-tier2", Classifier: knn})
	}

	fmt.Printf("relay eval — set %s (version %s, seed %d, %d rows)\n", *setPath, set.Version, set.Seed, len(set.Rows))
	fmt.Printf("candidates: cheap=%s mid=%s frontier=%s\n", *cheap, *mid, *frontier)
	fmt.Printf("cost model: prompt tokens ~ chars/4, completion tokens from the set; prices from pricing registry %s\n", reg.Version)
	fmt.Printf("quality model: per-band labels from the set — synthetic, see assets/eval/README.md\n\n")

	results, err := evalx.Run(ctx, set, cands, policies)
	if err != nil {
		return err
	}
	verdict := evalx.Judge(results, "static-frontier", "smart-tier1", *tolerance)
	fmt.Println("── synthetic labels ──")
	fmt.Print(evalx.Table(results, verdict))

	var judged []evalx.PolicyResult
	var judgedVerdict evalx.Verdict
	var candSpend, judgeSpend float64
	if *liveJudge {
		judgeProvName, judgeModel, okJ := strings.Cut(*judgeSpec, "/")
		if !okJ {
			return fmt.Errorf("--judge %q is not provider/model", *judgeSpec)
		}
		pairs := livePairs(results)
		candEst, judgeEst := liveEstimate(set, pairs, cands, reg, judgeProvName, judgeModel)
		fmt.Printf("\n── live-judged run ──\n")
		fmt.Printf("plan: %d unique (row × model) completions (max %d output tokens each) + %d judge calls (%s)\n",
			len(pairs), liveJudgeMaxTokens, len(pairs), *judgeSpec)
		fmt.Printf("projected spend, worst case at the token cap: candidates $%.2f + judge $%.2f = $%.2f\n",
			candEst, judgeEst, candEst+judgeEst)
		if *dryRun {
			fmt.Println("--dry-run: stopping before any API call")
			return nil
		}
		if !*yes {
			fmt.Print("proceed and spend? type 'yes' to continue: ")
			var answer string
			if _, err := fmt.Scanln(&answer); err != nil || strings.TrimSpace(answer) != "yes" {
				return fmt.Errorf("not confirmed; nothing was spent")
			}
		}
		rt, err := evalRuntime(*cfgPath)
		if err != nil {
			return err
		}
		judged, candSpend, judgeSpend, err = liveRejudge(ctx, rt, reg, set, results, judgeProvName, judgeModel)
		if err != nil {
			return err
		}
		judgedVerdict = evalx.Judge(judged, "static-frontier", "smart-tier1", *tolerance)
		fmt.Printf("\n── live-judged results (quality measured by %s; actual spend: candidates $%.4f + judge ~$%.4f) ──\n",
			*judgeSpec, candSpend, judgeSpend)
		fmt.Print(evalx.Table(judged, judgedVerdict))
	}

	if *jsonPath != "" {
		out := struct {
			Set              string               `json:"set"`
			SetVersion       string               `json:"set_version"`
			Seed             int64                `json:"seed"`
			Candidates       evalx.Candidates     `json:"candidates"`
			Results          []evalx.PolicyResult `json:"results_synthetic"`
			Verdict          evalx.Verdict        `json:"verdict_synthetic"`
			Judge            string               `json:"judge,omitempty"`
			JudgedResults    []evalx.PolicyResult `json:"results_live_judged,omitempty"`
			JudgedVerdict    *evalx.Verdict       `json:"verdict_live_judged,omitempty"`
			CandidateSpend   float64              `json:"live_candidate_spend_usd,omitempty"`
			JudgeSpendApprox float64              `json:"live_judge_spend_usd_approx,omitempty"`
		}{*setPath, set.Version, set.Seed, cands, results, verdict, "", nil, nil, 0, 0}
		if judged != nil {
			out.Judge = *judgeSpec
			out.JudgedResults = judged
			out.JudgedVerdict = &judgedVerdict
			out.CandidateSpend = candSpend
			out.JudgeSpendApprox = judgeSpend
		}
		raw, err := json.MarshalIndent(out, "", " ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*jsonPath, raw, 0o644); err != nil {
			return err
		}
		fmt.Printf("\nwrote %s\n", *jsonPath)
	}
	return nil
}

// evalRuntime builds providers from config discovery + env for live runs.
func evalRuntime(cfgPath string) (*server.Runtime, error) {
	var cfg *config.Config
	if path := config.Find(cfgPath); path != "" {
		var err error
		if cfg, err = config.Load(path); err != nil {
			return nil, err
		}
	} else {
		cfg = config.Sniff()
	}
	return server.BuildRuntime(cfg)
}

// buildEvalKNN binds tier 2 to a real embedder. The reference set is the
// embedded seed texts by default, or a relay-train-built store via
// --refs so users can measure their own tier-2 crossover. Remote
// embedders require --live: that is network traffic and (tiny) spend.
func buildEvalKNN(ctx context.Context, cfgPath, embedderSpec, refsPath string, live bool, fallback smart.Classifier) (*smart.KNN, error) {
	provName, model, ok := strings.Cut(embedderSpec, "/")
	if !ok {
		return nil, fmt.Errorf("--embedder: %q is not provider/model", embedderSpec)
	}
	var cfg *config.Config
	if path := config.Find(cfgPath); path != "" {
		var err error
		if cfg, err = config.Load(path); err != nil {
			return nil, err
		}
	} else {
		cfg = config.Sniff()
	}
	pc, exists := cfg.Providers[provName]
	if !exists {
		return nil, fmt.Errorf("--embedder: provider %q not configured (no key in env/config)", provName)
	}
	if pc.Type != "ollama" && !live {
		return nil, fmt.Errorf("--embedder %s is remote: pass --live to allow the eval to send eval-set text there (embeds %s texts)", embedderSpec, "seed+eval")
	}
	rt, err := server.BuildRuntime(cfg)
	if err != nil {
		return nil, err
	}
	p, okP := rt.Providers[provName]
	if !okP {
		return nil, fmt.Errorf("--embedder: provider %q failed to build", provName)
	}
	emb, okE := p.(provider.Embedder)
	if !okE {
		return nil, fmt.Errorf("--embedder: provider %q has no embeddings API", provName)
	}
	embedFn := func(ctx context.Context, texts []string) ([][]float32, error) {
		resp, err := emb.Embed(ctx, &provider.EmbedRequest{Model: model, Input: texts})
		if err != nil {
			return nil, err
		}
		return resp.Vectors, nil
	}

	if refsPath != "" {
		st, err := smart.LoadRefStore(refsPath)
		if err != nil {
			return nil, err
		}
		if st.Embedder != embedderSpec {
			return nil, fmt.Errorf("--refs %s was built with embedder %q, not %q — vectors are not portable across embedders", refsPath, st.Embedder, embedderSpec)
		}
		fmt.Printf("tier-2: using trained reference set %s (%d refs)\n", refsPath, len(st.Refs))
		return smart.NewKNN(embedFn, st.Refs, fallback), nil
	}

	refs, err := smart.SeedRefs()
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(refs))
	for i, r := range refs {
		texts[i] = r.Text
	}
	fmt.Printf("tier-2: embedding %d seed reference texts via %s...\n", len(texts), embedderSpec)
	vecs, err := embedFn(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embedding seed refs: %w", err)
	}
	for i := range refs {
		refs[i].Vector = vecs[i]
	}
	return smart.NewKNN(embedFn, refs, fallback), nil
}
