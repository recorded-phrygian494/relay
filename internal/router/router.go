// Package router defines the Router interface and routing policies. A
// Router turns a request into an ordered fallback chain of candidates; the
// reliability executor owns actually walking the chain.
package router

import (
	"context"
	"errors"

	"github.com/relay-llm/relay/internal/core"
)

// Candidate is one (provider, model) pair in a fallback chain.
type Candidate struct {
	Provider string // provider registry key
	Model    string // provider-native model id
	Reason   string // human-readable explanation, logged verbatim
}

// Router chooses an ordered fallback chain for a request. Implementations
// must be fast (<1ms) and side-effect free.
type Router interface {
	Name() string
	Route(ctx context.Context, req *core.Request) ([]Candidate, error)
}

// ErrNoRoute means no policy could map the requested model to a provider.
var ErrNoRoute = errors.New("no route for requested model")
