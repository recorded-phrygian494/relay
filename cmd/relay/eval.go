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
	live := fs.Bool("live", false, "allow network calls (required for a remote --embedder)")
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if *embedder != "" {
		knn, err := buildEvalKNN(ctx, *cfgPath, *embedder, *live, lex)
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
	fmt.Print(evalx.Table(results, verdict))

	if *jsonPath != "" {
		out := struct {
			Set        string               `json:"set"`
			SetVersion string               `json:"set_version"`
			Seed       int64                `json:"seed"`
			Candidates evalx.Candidates     `json:"candidates"`
			Results    []evalx.PolicyResult `json:"results"`
			Verdict    evalx.Verdict        `json:"verdict"`
		}{*setPath, set.Version, set.Seed, cands, results, verdict}
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

// buildEvalKNN binds tier 2 to a real embedder and embeds the seed
// reference set for the run. Remote embedders require --live: that is
// network traffic and (tiny) spend.
func buildEvalKNN(ctx context.Context, cfgPath, embedderSpec string, live bool, fallback smart.Classifier) (*smart.KNN, error) {
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
