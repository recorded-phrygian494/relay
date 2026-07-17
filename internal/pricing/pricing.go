// Package pricing loads the model pricing registry: embedded snapshot at
// build time, overridable by a local file, refreshed only on explicit
// `relay pricing update` (DESIGN §9). Prices feed the cheapest router and
// per-request cost accounting.
package pricing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/llmrelay/relay/assets"
)

// Entry prices one model (or model-id prefix) at one provider kind.
type Entry struct {
	Provider string  `json:"provider"` // provider kind: matched against name, profile, and type
	Model    string  `json:"model"`    // model id prefix; longest match wins
	In       float64 `json:"in"`       // $ per Mtok input
	Out      float64 `json:"out"`      // $ per Mtok output
	Context  int     `json:"context,omitempty"`
}

type file struct {
	Version string  `json:"version"`
	Models  []Entry `json:"models"`
}

// Registry answers price lookups.
type Registry struct {
	Version string
	entries []Entry
}

// OverridePath is where `relay pricing update` writes and Load looks for a
// local override.
func OverridePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "pricing.json"
	}
	return filepath.Join(home, ".relay", "pricing.json")
}

// Load returns the registry: the local override file when present and
// valid, else the embedded snapshot. A broken override falls back to the
// embedded data with the error returned alongside (caller logs it; the
// gateway still starts).
func Load() (*Registry, error) {
	embedded, err := parse(assets.PricingJSON)
	if err != nil {
		// The embedded file is compiled in; failing to parse it is a build bug.
		panic(fmt.Sprintf("embedded pricing.json invalid: %v", err))
	}
	raw, readErr := os.ReadFile(OverridePath())
	if readErr != nil {
		return embedded, nil
	}
	override, err := parse(raw)
	if err != nil {
		return embedded, fmt.Errorf("ignoring %s: %w", OverridePath(), err)
	}
	return override, nil
}

// Validate checks that raw parses as a usable registry, for `relay pricing
// update` before it overwrites the local override.
func Validate(raw []byte) error {
	_, err := parse(raw)
	return err
}

func parse(raw []byte) (*Registry, error) {
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if len(f.Models) == 0 {
		return nil, fmt.Errorf("no models in pricing file")
	}
	return &Registry{Version: f.Version, entries: f.Models}, nil
}

// Price returns $/Mtok for a model at a provider. kinds holds every alias
// the caller knows for the provider (its config name, profile, type);
// entries match any of them, longest model prefix wins.
func (r *Registry) Price(kinds []string, model string) (in, out float64, ok bool) {
	best := -1
	for i, e := range r.entries {
		if !matchKind(e.Provider, kinds) || !strings.HasPrefix(model, e.Model) {
			continue
		}
		if best == -1 || len(e.Model) > len(r.entries[best].Model) {
			best = i
		}
	}
	if best == -1 {
		return 0, 0, false
	}
	return r.entries[best].In, r.entries[best].Out, true
}

func matchKind(entry string, kinds []string) bool {
	for _, k := range kinds {
		if strings.EqualFold(entry, k) {
			return true
		}
	}
	return false
}

// Cost prices one request in USD. ok=false means the model is unknown and
// the cost is 0 — never guessed.
func (r *Registry) Cost(kinds []string, model string, tokensIn, tokensOut int) (float64, bool) {
	in, out, ok := r.Price(kinds, model)
	if !ok {
		return 0, false
	}
	return (float64(tokensIn)*in + float64(tokensOut)*out) / 1e6, true
}
