package router

import (
	"context"
	"fmt"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/smart"
)

// Smart routes by classified difficulty (DESIGN §0.3): easy traffic to
// the cheap chain, hard traffic to the frontier chain. The classifier's
// Why string is prepended to the winning candidate's reason so every
// smart decision in the log names its evidence.
type Smart struct {
	Classifier smart.Classifier
	Easy, Hard Router
	// OnDecision, when set, observes every classification (the serve path
	// uses it to hand the tier-2 query embedding to the request log under
	// log_prompts: embeddings).
	OnDecision func(context.Context, smart.Decision)
}

// Name implements Router.
func (s *Smart) Name() string { return "smart" }

// Route implements Router.
func (s *Smart) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	d, err := s.Classifier.Classify(ctx, req)
	if err != nil {
		// Fail safe, not cheap: an unclassifiable request goes to the
		// hard chain and says why.
		cands, rerr := s.Hard.Route(ctx, req)
		if rerr != nil {
			return nil, rerr
		}
		prefix(cands, fmt.Sprintf("smart: classifier error (%v) → hard chain (fail-safe)", err))
		return cands, nil
	}
	if s.OnDecision != nil {
		s.OnDecision(ctx, d)
	}
	chain, label := s.Easy, "easy"
	if d.Class == "hard" {
		chain, label = s.Hard, "hard"
	}
	cands, err := chain.Route(ctx, req)
	if err != nil {
		return nil, err
	}
	prefix(cands, fmt.Sprintf("smart → %s chain [%s]", label, d.Why))
	return cands, nil
}

func prefix(cands []Candidate, p string) {
	if len(cands) > 0 {
		cands[0].Reason = p + " | " + cands[0].Reason
	}
}

// StaticThenSmart resolves explicit names statically (provider/model
// prefixes, configured routes, catalog matches keep working exactly as
// before) and hands only otherwise-unroutable names to the smart policy —
// the semantics of `routing.default: smart`.
type StaticThenSmart struct {
	Static Router
	Smart  *Smart
}

// Name implements Router.
func (d *StaticThenSmart) Name() string { return "smart" }

// Route implements Router.
func (d *StaticThenSmart) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	cands, err := d.Static.Route(ctx, req)
	if err == nil {
		return cands, nil
	}
	return d.Smart.Route(ctx, req)
}

// Chain compiles one smart chain target: the name of a configured alias,
// or a bare provider/model pair.
func Chain(target string, aliases map[string]Router) (Router, error) {
	if r, ok := aliases[target]; ok {
		return r, nil
	}
	t, err := ParseTarget(target)
	if err != nil {
		return nil, fmt.Errorf("smart chain target %q is neither an alias nor provider/model: %w", target, err)
	}
	return &singleTarget{t: t}, nil
}

type singleTarget struct{ t Target }

// Name implements Router.
func (s *singleTarget) Name() string { return "smart" }

// Route implements Router.
func (s *singleTarget) Route(context.Context, *core.Request) ([]Candidate, error) {
	return []Candidate{{
		Provider: s.t.Provider,
		Model:    s.t.Model,
		Reason:   fmt.Sprintf("chain target %s/%s", s.t.Provider, s.t.Model),
	}}, nil
}
