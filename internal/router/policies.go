package router

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"github.com/llmrelay/relay/internal/core"
)

// Target is one parsed "provider/model" chain entry.
type Target struct {
	Provider string
	Model    string
}

// ParseTarget splits "provider/model"; the model part may itself contain
// slashes (e.g. ollama's "library/name:tag").
func ParseTarget(s string) (Target, error) {
	prov, model, ok := strings.Cut(s, "/")
	if !ok || prov == "" || model == "" {
		return Target{}, fmt.Errorf("target %q is not provider/model", s)
	}
	return Target{Provider: prov, Model: model}, nil
}

// PriceFunc reports a model's $/Mtok pricing. ok=false means unknown, in
// which case cheapest preserves declared order for that candidate. The
// pricing registry provides the real implementation later in phase 3.
type PriceFunc func(provider, model string) (inPerMtok, outPerMtok float64, ok bool)

// StatsFunc reports a model's rolling TTFT estimate in milliseconds.
// ok=false means cold, which fastest treats as an optimistic prior: cold
// candidates rank first so they get explored (DESIGN §7).
type StatsFunc func(provider, model string) (ttftMS float64, ok bool)

// Fallback is a literal ordered chain (DESIGN §7).
type Fallback struct {
	Alias   string
	Targets []Target
}

// Name implements Router.
func (f *Fallback) Name() string { return "fallback" }

// Route implements Router.
func (f *Fallback) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	out := make([]Candidate, 0, len(f.Targets))
	for i, t := range f.Targets {
		out = append(out, Candidate{
			Provider: t.Provider,
			Model:    t.Model,
			Reason:   fmt.Sprintf("alias %q → %s/%s (rank %d)", f.Alias, t.Provider, t.Model, i+1),
		})
	}
	return out, nil
}

// Cheapest ranks candidates by blended request cost from the pricing
// registry, input-weighted by a request size estimate. Unknown prices sort
// after known ones, keeping their declared order.
type Cheapest struct {
	Alias   string
	Targets []Target
	Price   PriceFunc // nil means no pricing yet: declared order wins
}

// Name implements Router.
func (c *Cheapest) Name() string { return "cheapest" }

// estimateTokens is a cheap size proxy: ~4 chars/token over text parts.
func estimateTokens(req *core.Request) int {
	n := 0
	for _, s := range req.System {
		n += len(s.Text)
	}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if tp, ok := p.(core.TextPart); ok {
				n += len(tp.Text)
			}
		}
	}
	if n == 0 {
		return 1
	}
	return n / 4
}

// Route implements Router.
func (c *Cheapest) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	type ranked struct {
		t     Target
		cost  float64
		known bool
		pos   int
	}
	estIn := float64(estimateTokens(req))
	const estOut = 500.0 // flat output guess; input dominates ranking anyway
	rows := make([]ranked, 0, len(c.Targets))
	for i, t := range c.Targets {
		r := ranked{t: t, pos: i}
		if c.Price != nil {
			if in, out, ok := c.Price(t.Provider, t.Model); ok {
				r.cost = (estIn*in + estOut*out) / 1e6
				r.known = true
			}
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].known != rows[j].known {
			return rows[i].known
		}
		if !rows[i].known {
			return rows[i].pos < rows[j].pos
		}
		return rows[i].cost < rows[j].cost
	})
	out := make([]Candidate, 0, len(rows))
	for i, r := range rows {
		reason := fmt.Sprintf("alias %q cheapest: %s/%s (rank %d, price unknown)", c.Alias, r.t.Provider, r.t.Model, i+1)
		if r.known {
			reason = fmt.Sprintf("alias %q cheapest: %s/%s (rank %d, ≈$%.6f/request)", c.Alias, r.t.Provider, r.t.Model, i+1, r.cost)
		}
		out = append(out, Candidate{Provider: r.t.Provider, Model: r.t.Model, Reason: reason})
	}
	return out, nil
}

// Fastest ranks candidates by rolling TTFT. Cold candidates rank first
// (optimistic prior) so they get explored.
type Fastest struct {
	Alias   string
	Targets []Target
	TTFT    StatsFunc // nil means no stats yet: everything is cold
}

// Name implements Router.
func (f *Fastest) Name() string { return "fastest" }

// Route implements Router.
func (f *Fastest) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	type ranked struct {
		t    Target
		ttft float64
		cold bool
		pos  int
	}
	rows := make([]ranked, 0, len(f.Targets))
	for i, t := range f.Targets {
		r := ranked{t: t, cold: true, pos: i}
		if f.TTFT != nil {
			if ms, ok := f.TTFT(t.Provider, t.Model); ok {
				r.ttft, r.cold = ms, false
			}
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].cold != rows[j].cold {
			return rows[i].cold
		}
		if rows[i].cold {
			return rows[i].pos < rows[j].pos
		}
		return rows[i].ttft < rows[j].ttft
	})
	out := make([]Candidate, 0, len(rows))
	for i, r := range rows {
		reason := fmt.Sprintf("alias %q fastest: %s/%s (rank %d, cold — exploring)", f.Alias, r.t.Provider, r.t.Model, i+1)
		if !r.cold {
			reason = fmt.Sprintf("alias %q fastest: %s/%s (rank %d, ttft ≈%.0fms)", f.Alias, r.t.Provider, r.t.Model, i+1, r.ttft)
		}
		out = append(out, Candidate{Provider: r.t.Provider, Model: r.t.Model, Reason: reason})
	}
	return out, nil
}

// Weighted splits traffic deterministically across children by hashing the
// conversation's first user message: one conversation stays on one child
// across turns and retries, which is what canarying wants (DESIGN §7). The
// chosen child ranks first; the rest follow in declared order as fallback.
type Weighted struct {
	Alias   string
	Targets []Target
	Weights []int // parallel to Targets; non-positive treated as 0
}

// Name implements Router.
func (w *Weighted) Name() string { return "weighted" }

// bucketKey is the stable per-conversation fingerprint.
func bucketKey(req *core.Request) string {
	for _, m := range req.Messages {
		if m.Role != core.RoleUser {
			continue
		}
		for _, p := range m.Parts {
			if tp, ok := p.(core.TextPart); ok {
				return tp.Text
			}
		}
	}
	return req.Model
}

// Route implements Router.
func (w *Weighted) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	total := 0
	for _, wt := range w.Weights {
		if wt > 0 {
			total += wt
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("alias %q weighted: all weights are zero", w.Alias)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucketKey(req)))
	bucket := int(h.Sum32() % uint32(total))
	chosen := 0
	for i, wt := range w.Weights {
		if wt <= 0 {
			continue
		}
		if bucket < wt {
			chosen = i
			break
		}
		bucket -= wt
	}
	out := make([]Candidate, 0, len(w.Targets))
	t := w.Targets[chosen]
	out = append(out, Candidate{
		Provider: t.Provider,
		Model:    t.Model,
		Reason: fmt.Sprintf("alias %q weighted: %s/%s (weight %d/%d, sticky per conversation)",
			w.Alias, t.Provider, t.Model, w.Weights[chosen], total),
	})
	for i, t := range w.Targets {
		if i == chosen {
			continue
		}
		out = append(out, Candidate{
			Provider: t.Provider,
			Model:    t.Model,
			Reason:   fmt.Sprintf("alias %q weighted: %s/%s (fallback after weighted pick)", w.Alias, t.Provider, t.Model),
		})
	}
	return out, nil
}
