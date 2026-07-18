// Package server wires the inbound HTTP API: routing, auth, handlers, and
// the hot-swappable runtime snapshot.
package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/llmrelay/relay/internal/cache"
	"github.com/llmrelay/relay/internal/config"
	"github.com/llmrelay/relay/internal/pricing"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/reliability"
	"github.com/llmrelay/relay/internal/provider/anthropicprov"
	"github.com/llmrelay/relay/internal/provider/gemini"
	"github.com/llmrelay/relay/internal/provider/ollama"
	"github.com/llmrelay/relay/internal/provider/openaicompat"
	"github.com/llmrelay/relay/internal/provider/openaiprov"
	"github.com/llmrelay/relay/internal/router"
	"github.com/llmrelay/relay/internal/smart"
)

// embSinkKey carries a per-request slot for the tier-2 query embedding
// (log_prompts: embeddings stores the vector, never the text).
type embSinkKey struct{}

// withEmbeddingSink arms a request context to capture the query vector.
func withEmbeddingSink(ctx context.Context, slot *[]float32) context.Context {
	return context.WithValue(ctx, embSinkKey{}, slot)
}

// recordDecisionVector is the router.Smart OnDecision hook: it hands the
// tier-2 embedding to the armed request context, if any.
func recordDecisionVector(ctx context.Context, d smart.Decision) {
	if slot, ok := ctx.Value(embSinkKey{}).(*[]float32); ok && slot != nil && d.Vector != nil {
		*slot = d.Vector
	}
}

// Runtime is one immutable snapshot of config-derived state. Hot reload
// builds a new Runtime and swaps the pointer; in-flight requests keep the
// old one (DESIGN §8).
type Runtime struct {
	Config    *config.Config
	Providers map[string]provider.Provider
	Router    router.Router
	Exec      *reliability.Executor
	Pricing   *pricing.Registry
	Cache     *cache.Exact // nil unless cache.enabled; reset on hot reload
	catalog   *catalogCache

	// degradedWarned dedupes the DESIGN §0.7 multi_turn_tools warning:
	// one log line per conversation, keyed by candidate + first replayed
	// tool-call id. Reset on hot reload, which is acceptable — as is
	// resetting breaker and key-cooldown state with the Executor.
	degradedWarned sync.Map
}

// BuildRuntime constructs providers and the router from a validated config.
func BuildRuntime(cfg *config.Config) (*Runtime, error) {
	httpClient := &http.Client{}
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		switch pc.Type {
		case "openai":
			providers[name] = openaiprov.New(name, pc.BaseURL, pc.APIKey.First(), httpClient)
		case "openai-compat":
			var quirks openaicompat.Quirks
			if pc.Profile != "" {
				quirks = openaicompat.Profiles[pc.Profile].Quirks
			}
			providers[name] = openaicompat.New(openaicompat.Config{
				Name:    name,
				BaseURL: pc.BaseURL,
				APIKey:  pc.APIKey.First(),
				Headers: pc.Headers,
				Quirks:  quirks,
			}, httpClient)
		case "anthropic":
			providers[name] = anthropicprov.New(name, pc.BaseURL, pc.APIKey.First(), httpClient)
		case "gemini":
			providers[name] = gemini.New(name, pc.BaseURL, pc.APIKey.First(), httpClient)
		case "ollama":
			providers[name] = ollama.New(name, pc.BaseURL, httpClient)
		default:
			return nil, fmt.Errorf("provider %q: unhandled type %q", name, pc.Type)
		}
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("no providers configured: set provider API keys in relay.yaml or export OPENAI_API_KEY / run Ollama for zero-config mode")
	}

	registry, priceErr := pricing.Load()
	if priceErr != nil {
		log.Printf("pricing: %v", priceErr)
	}
	rt := &Runtime{
		Config:    cfg,
		Providers: providers,
		Pricing:   registry,
		catalog:   newCatalogCache(providers, 5*time.Minute),
	}
	if cfg.Cache.Enabled {
		ttl := cfg.Cache.TTL.Std()
		if ttl <= 0 {
			ttl = 10 * time.Minute // DESIGN §8 default
		}
		rt.Cache = cache.NewExact(ttl, 0)
	}
	pools := make(map[string]*reliability.KeyPool)
	for name, pc := range cfg.Providers {
		if pool := reliability.NewKeyPool(pc.APIKey); pool != nil {
			pools[name] = pool
		}
	}
	rt.Exec = reliability.NewExecutor(
		func(name string) (provider.Provider, bool) { p, ok := providers[name]; return p, ok },
		pools,
		cfg.Reliability.RetryCount(),
		cfg.Reliability.TTFTTimeout.Std(),
	)
	static := &router.Static{
		Routes:          cfg.Routing.Static,
		HasProvider:     func(name string) bool { _, ok := providers[name]; return ok },
		Catalog:         rt.catalog.ByModel,
		DefaultProvider: cfg.Routing.DefaultProvider,
	}

	var table map[string]router.Router
	if len(cfg.Routing.Aliases) > 0 {
		specs := make(map[string]router.AliasSpec, len(cfg.Routing.Aliases))
		for name, a := range cfg.Routing.Aliases {
			spec := router.AliasSpec{Policy: a.Policy, Targets: a.Candidates}
			for _, ch := range a.Children {
				spec.Targets = append(spec.Targets, ch.Target)
				spec.Weights = append(spec.Weights, ch.Weight)
			}
			specs[name] = spec
		}
		// Stats land later; until then fastest degrades to exploration
		// order, stated in its reasons.
		var err error
		table, err = router.CompileAliases(specs, rt.priceFor, nil)
		if err != nil {
			return nil, fmt.Errorf("routing.aliases: %w", err)
		}
	}

	var inner router.Router = static
	if cfg.Routing.Default == "smart" {
		sm, err := buildSmartRouter(cfg, providers, table)
		if err != nil {
			return nil, err
		}
		inner = &router.StaticThenSmart{Static: static, Smart: sm}
	}

	if table == nil {
		rt.Router = inner
		return rt, nil
	}
	rt.Router = &router.Aliases{
		Table: table,
		Inner: inner,
		Filter: &router.Eligibility{Caps: func(providerName, model string) provider.Capabilities {
			if mc, ok := providers[providerName].(provider.ModelCapabilities); ok {
				return mc.Capabilities(model)
			}
			return provider.Capabilities{}
		}},
	}
	return rt, nil
}

// buildSmartRouter assembles the smart policy per DESIGN §0.3: tier 1
// (lexical) always available; tier 2 (KNN) opt-in, bound to relay's own
// provider embedding path, its reference set loaded from disk or built
// from the embedded seed texts in the background.
func buildSmartRouter(cfg *config.Config, providers map[string]provider.Provider, table map[string]router.Router) (*router.Smart, error) {
	lex, err := smart.NewLexical()
	if err != nil {
		return nil, err
	}
	var classifier smart.Classifier = lex

	// The default tier is a §0.3 launch-gate decision: "lexical" only
	// because relay eval proved tier 1 beats static-frontier on cost at
	// equal quality (2026-07-18, eval set v1: quality −0.017 within the
	// 0.020 tolerance, cost −18%). If the set or weights change, re-run
	// the gate before trusting this default.
	tier := cfg.Routing.Smart.Tier
	if tier == "" {
		tier = "lexical"
	}
	if tier == "knn" {
		provName, model, _ := strings.Cut(cfg.Routing.Smart.Embeddings, "/")
		p, ok := providers[provName]
		if !ok {
			return nil, fmt.Errorf("routing.smart.embeddings: unknown provider %q", provName)
		}
		emb, ok := p.(provider.Embedder)
		if !ok {
			return nil, fmt.Errorf("routing.smart.embeddings: provider %q has no embeddings API", provName)
		}
		embedFn := func(ctx context.Context, texts []string) ([][]float32, error) {
			resp, err := emb.Embed(ctx, &provider.EmbedRequest{Model: model, Input: texts})
			if err != nil {
				return nil, err
			}
			return resp.Vectors, nil
		}
		knn := smart.NewKNN(embedFn, nil, lex)
		refsPath := cfg.Routing.Smart.Reference
		if refsPath == "" {
			refsPath = config.DefaultSmartRefsPath()
		}
		// Background build: until the reference set is ready the KNN
		// classifier reports lexical-fallback decisions with the reason.
		go initKNNRefs(knn, embedFn, refsPath, cfg.Routing.Smart.Embeddings)
		classifier = knn
	}

	easy, err := router.Chain(cfg.Routing.Smart.Easy, table)
	if err != nil {
		return nil, fmt.Errorf("routing.smart.easy: %w", err)
	}
	hard, err := router.Chain(cfg.Routing.Smart.Hard, table)
	if err != nil {
		return nil, fmt.Errorf("routing.smart.hard: %w", err)
	}
	return &router.Smart{Classifier: classifier, Easy: easy, Hard: hard, OnDecision: recordDecisionVector}, nil
}

// initKNNRefs loads the persisted reference set when it matches the
// configured embedder, otherwise embeds the seed texts and persists.
func initKNNRefs(knn *smart.KNN, embed smart.EmbedFunc, path, embedderID string) {
	if st, err := smart.LoadRefStore(path); err == nil && st.Embedder == embedderID {
		knn.SetRefs(st.Refs)
		log.Printf("smart: tier-2 reference set loaded from %s (%d refs, embedder %s)", path, len(st.Refs), embedderID)
		return
	}
	refs, err := smart.SeedRefs()
	if err != nil {
		log.Printf("smart: seed reference set unavailable: %v (tier 2 will fall back to lexical)", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	texts := make([]string, len(refs))
	for i, r := range refs {
		texts[i] = r.Text
	}
	vecs, err := embed(ctx, texts)
	if err != nil || len(vecs) != len(refs) {
		log.Printf("smart: embedding seed reference set failed: %v (tier 2 will fall back to lexical until relay train succeeds)", err)
		return
	}
	for i := range refs {
		refs[i].Vector = vecs[i]
	}
	knn.SetRefs(refs)
	if err := smart.SaveRefStore(path, &smart.RefStore{Embedder: embedderID, Refs: refs}); err != nil {
		log.Printf("smart: persisting reference set: %v", err)
	} else {
		log.Printf("smart: tier-2 reference set built from %d seed texts and saved to %s", len(refs), path)
	}
}

// kindsFor lists every alias pricing may know a provider by: its config
// name, profile, and type.
func (rt *Runtime) kindsFor(providerName string) []string {
	kinds := []string{providerName}
	if pc, ok := rt.Config.Providers[providerName]; ok {
		if pc.Profile != "" {
			kinds = append(kinds, pc.Profile)
		}
		if pc.Type != "" {
			kinds = append(kinds, pc.Type)
		}
	}
	return kinds
}

// priceFor is the cheapest policy's PriceFunc. Ollama is local compute:
// $0, and it must rank as free rather than unknown.
func (rt *Runtime) priceFor(providerName, model string) (in, out float64, ok bool) {
	if pc, exists := rt.Config.Providers[providerName]; exists && pc.Type == "ollama" {
		return 0, 0, true
	}
	if rt.Pricing == nil {
		return 0, 0, false
	}
	return rt.Pricing.Price(rt.kindsFor(providerName), model)
}

// cost prices one served request for the log. A model absent from the
// pricing registry returns nil — logged as NULL, surfaced as "unpriced" —
// never 0, which would be the dashboard lying about spend.
func (rt *Runtime) cost(providerName, model string, tokensIn, tokensOut int) *float64 {
	in, out, ok := rt.priceFor(providerName, model)
	if !ok {
		return nil
	}
	c := (float64(tokensIn)*in + float64(tokensOut)*out) / 1e6
	return &c
}

// catalogCache merges provider model lists with a TTL. Provider catalog
// failures degrade to an empty list for that provider, never an error: a
// dead Ollama must not take down /v1/models.
type catalogCache struct {
	providers map[string]provider.Provider
	ttl       time.Duration

	mu      sync.Mutex
	fetched time.Time
	models  []provider.Model
}

func newCatalogCache(providers map[string]provider.Provider, ttl time.Duration) *catalogCache {
	return &catalogCache{providers: providers, ttl: ttl}
}

// Models returns the merged catalog, refreshing if stale.
func (c *catalogCache) Models(ctx context.Context) []provider.Model {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.fetched) < c.ttl {
		return c.models
	}
	type result struct {
		models []provider.Model
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ch := make(chan result, len(c.providers))
	for _, p := range c.providers {
		go func(p provider.Provider) {
			models, err := p.Models(ctx)
			if err != nil {
				models = nil
			}
			ch <- result{models}
		}(p)
	}
	var merged []provider.Model
	for range c.providers {
		merged = append(merged, (<-ch).models...)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Provider != merged[j].Provider {
			return merged[i].Provider < merged[j].Provider
		}
		return merged[i].ID < merged[j].ID
	})
	c.models = merged
	c.fetched = time.Now()
	return c.models
}

// ByModel returns model id → provider names, for router disambiguation.
func (c *catalogCache) ByModel(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	for _, m := range c.Models(ctx) {
		out[m.ID] = append(out[m.ID], m.Provider)
	}
	return out
}

// IsLoopback reports whether addr binds only to a loopback interface.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" || host == "" {
		return host == "localhost"
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
