package router

import (
	"context"
	"fmt"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
)

// AliasSpec is the declarative form of one alias, kept yaml-free so the
// config package owns parsing and this package owns semantics.
type AliasSpec struct {
	Policy  string // "" or "fallback" | "cheapest" | "fastest" | "weighted"
	Targets []string
	Weights []int // parallel to Targets; weighted only
}

// CompileAliases builds a Router per alias. A target may be
// "provider/model" or the name of another alias (resolved by delegation,
// cycles rejected).
func CompileAliases(specs map[string]AliasSpec, price PriceFunc, ttft StatsFunc) (map[string]Router, error) {
	table := map[string]Router{}
	var build func(name string, trail []string) (Router, error)
	build = func(name string, trail []string) (Router, error) {
		for _, seen := range trail {
			if seen == name {
				return nil, fmt.Errorf("alias %q: reference cycle via %v", name, trail)
			}
		}
		if r, done := table[name]; done {
			return r, nil
		}
		spec := specs[name]
		trail = append(trail, name)

		var targets []Target
		var sub []Router // parallel: non-nil where the target is an alias ref
		for _, raw := range spec.Targets {
			if _, isAlias := specs[raw]; isAlias {
				if spec.Policy == "weighted" || spec.Policy == "" || spec.Policy == "fallback" {
					child, err := build(raw, trail)
					if err != nil {
						return nil, err
					}
					targets = append(targets, Target{})
					sub = append(sub, child)
					continue
				}
				return nil, fmt.Errorf("alias %q: %s cannot reference alias %q (only fallback chains may nest aliases)", name, spec.Policy, raw)
			}
			t, err := ParseTarget(raw)
			if err != nil {
				return nil, fmt.Errorf("alias %q: %w", name, err)
			}
			targets = append(targets, t)
			sub = append(sub, nil)
		}
		if len(targets) == 0 {
			return nil, fmt.Errorf("alias %q: no targets", name)
		}

		var compiled Router
		switch spec.Policy {
		case "", "fallback":
			compiled = &nested{alias: name, targets: targets, sub: sub}
		case "cheapest":
			compiled = &Cheapest{Alias: name, Targets: targets, Price: price}
		case "fastest":
			compiled = &Fastest{Alias: name, Targets: targets, TTFT: ttft}
		case "weighted":
			if len(spec.Weights) != len(spec.Targets) {
				return nil, fmt.Errorf("alias %q: weighted needs a weight per target", name)
			}
			if hasAliasRef(sub) {
				return nil, fmt.Errorf("alias %q: weighted cannot reference other aliases in v1", name)
			}
			compiled = &Weighted{Alias: name, Targets: targets, Weights: spec.Weights}
		default:
			return nil, fmt.Errorf("alias %q: unknown policy %q (want fallback | cheapest | fastest | weighted)", name, spec.Policy)
		}
		table[name] = compiled
		return compiled, nil
	}
	for name := range specs {
		if _, err := build(name, nil); err != nil {
			return nil, err
		}
	}
	return table, nil
}

func hasAliasRef(sub []Router) bool {
	for _, s := range sub {
		if s != nil {
			return true
		}
	}
	return false
}

// nested is a fallback chain whose entries may be aliases: alias entries
// expand to their own chains in place.
type nested struct {
	alias   string
	targets []Target
	sub     []Router
}

// Name implements Router.
func (n *nested) Name() string { return "fallback" }

// Route implements Router.
func (n *nested) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	var out []Candidate
	for i, t := range n.targets {
		if r := n.sub[i]; r != nil {
			cands, err := r.Route(ctx, req)
			if err != nil {
				return nil, err
			}
			out = append(out, cands...)
			continue
		}
		out = append(out, Candidate{
			Provider: t.Provider,
			Model:    t.Model,
			Reason:   fmt.Sprintf("alias %q → %s/%s (rank %d)", n.alias, t.Provider, t.Model, i+1),
		})
	}
	return out, nil
}

// Aliases routes virtual model names through per-alias policies, falling
// through to Inner for everything else. Alias chains pass through the
// eligibility filter when one is set.
type Aliases struct {
	Table  map[string]Router
	Inner  Router
	Filter *Eligibility
}

// Name implements Router.
func (a *Aliases) Name() string { return "alias" }

// Route implements Router.
func (a *Aliases) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	r, ok := a.Table[req.Model]
	if !ok {
		return a.Inner.Route(ctx, req)
	}
	cands, err := r.Route(ctx, req)
	if err != nil {
		return nil, err
	}
	if a.Filter != nil {
		cands = a.Filter.Filter(req, cands)
	}
	return cands, nil
}

// Eligibility drops chain candidates with known capability caveats the
// request would trip — today: multi_turn_tools=degraded on function-call
// replay (DESIGN §0.7 condition 2). Fail-open: if every candidate is
// degraded, the chain is returned unchanged (the executor still warns and
// the provider still returns its typed error).
type Eligibility struct {
	// Caps looks up per-model capabilities; nil-safe (nil = no metadata).
	Caps func(providerName, model string) provider.Capabilities
}

// Filter implements the drop rule. Chains of one are left alone: an
// explicit single choice is honored, warning included.
func (e *Eligibility) Filter(req *core.Request, cands []Candidate) []Candidate {
	if e == nil || e.Caps == nil || len(cands) < 2 {
		return cands
	}
	if _, replaying := req.ToolReplayID(); !replaying {
		return cands
	}
	kept := make([]Candidate, 0, len(cands))
	var dropped []string
	for _, c := range cands {
		if e.Caps(c.Provider, c.Model).MultiTurnTools == provider.MultiTurnToolsDegraded {
			dropped = append(dropped, c.Provider+"/"+c.Model)
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 || len(dropped) == 0 {
		return cands
	}
	kept[0].Reason += fmt.Sprintf(" [eligibility: skipped %d multi_turn_tools=degraded candidate(s) for tool replay: %v]",
		len(dropped), dropped)
	return kept
}
