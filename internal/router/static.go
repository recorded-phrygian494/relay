package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/llmrelay/relay/internal/core"
)

// Static resolves a requested model name directly to a provider, in order:
//
//  1. explicit "provider/model" prefix, when the provider exists
//  2. the configured routes map (requested name → "provider/model")
//  3. catalog scan: exactly one provider serves that model id
//  4. the configured default provider, if any
type Static struct {
	// Routes maps a requested model name to "provider/model".
	Routes map[string]string
	// HasProvider reports whether a provider name is registered.
	HasProvider func(name string) bool
	// Catalog returns model id → provider names, from the catalog cache.
	Catalog func(ctx context.Context) map[string][]string
	// DefaultProvider, when set, serves any otherwise-unresolved model name.
	DefaultProvider string
}

// Name implements Router.
func (s *Static) Name() string { return "static" }

// Route implements Router.
func (s *Static) Route(ctx context.Context, req *core.Request) ([]Candidate, error) {
	name := req.Model

	if prov, model, ok := strings.Cut(name, "/"); ok && s.HasProvider != nil && s.HasProvider(prov) {
		return []Candidate{{
			Provider: prov,
			Model:    model,
			Reason:   fmt.Sprintf("static: explicit provider prefix %q", prov),
		}}, nil
	}

	if target, ok := s.Routes[name]; ok {
		prov, model, found := strings.Cut(target, "/")
		if !found {
			return nil, fmt.Errorf("static route for %q is %q; want provider/model", name, target)
		}
		return []Candidate{{
			Provider: prov,
			Model:    model,
			Reason:   fmt.Sprintf("static: configured route %q → %q", name, target),
		}}, nil
	}

	if s.Catalog != nil {
		if provs := s.Catalog(ctx)[name]; len(provs) == 1 {
			return []Candidate{{
				Provider: provs[0],
				Model:    name,
				Reason:   fmt.Sprintf("static: only provider %q serves %q", provs[0], name),
			}}, nil
		} else if len(provs) > 1 {
			return nil, fmt.Errorf("%w: model %q is served by %d providers (%s); use provider/model",
				ErrNoRoute, name, len(provs), strings.Join(provs, ", "))
		}
	}

	if s.DefaultProvider != "" {
		return []Candidate{{
			Provider: s.DefaultProvider,
			Model:    name,
			Reason:   fmt.Sprintf("static: default provider %q", s.DefaultProvider),
		}}, nil
	}
	return nil, fmt.Errorf("%w: %q is not an alias, a static route, a provider/model pair, or a catalog model", ErrNoRoute, name)
}
