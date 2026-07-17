package pricing

import (
	"testing"

	"github.com/llmrelay/relay/assets"
)

func TestEmbeddedRegistryParses(t *testing.T) {
	r, err := parse(assets.PricingJSON)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version == "" || len(r.entries) == 0 {
		t.Fatalf("registry: version=%q entries=%d", r.Version, len(r.entries))
	}
	for _, e := range r.entries {
		if e.Provider == "" || e.Model == "" || e.In < 0 || e.Out < 0 {
			t.Fatalf("bad entry: %+v", e)
		}
	}
}

func TestLongestPrefixWins(t *testing.T) {
	r, _ := parse(assets.PricingJSON)

	// gpt-4o-mini must match its own entry, not the shorter gpt-4o prefix.
	in, _, ok := r.Price([]string{"openai"}, "gpt-4o-mini")
	if !ok || in != 0.15 {
		t.Fatalf("gpt-4o-mini: in=%v ok=%v", in, ok)
	}
	in, _, ok = r.Price([]string{"openai"}, "gpt-4o-2024-08-06")
	if !ok || in != 2.5 {
		t.Fatalf("dated gpt-4o snapshot: in=%v ok=%v", in, ok)
	}

	// Anthropic date-suffixed ids resolve through the family prefix.
	in, out, ok := r.Price([]string{"anthropic"}, "claude-haiku-4-5-20251001")
	if !ok || in != 1.0 || out != 5.0 {
		t.Fatalf("haiku: in=%v out=%v ok=%v", in, out, ok)
	}

	// Provider kind gates matches: a groq-served llama id must not match
	// under an unrelated provider kind.
	if _, _, ok := r.Price([]string{"openai"}, "llama-3.3-70b"); ok {
		t.Fatal("llama priced under openai kind")
	}
	if _, _, ok := r.Price([]string{"my-groq", "groq", "openai-compat"}, "llama-3.3-70b-versatile"); !ok {
		t.Fatal("llama not priced under groq profile kind")
	}

	// Unknown stays unknown.
	if _, _, ok := r.Price([]string{"openai"}, "totally-new-model"); ok {
		t.Fatal("unknown model got a price")
	}
}

func TestCost(t *testing.T) {
	r, _ := parse(assets.PricingJSON)
	// 1M in + 1M out on haiku = $1 + $5.
	cost, ok := r.Cost([]string{"anthropic"}, "claude-haiku-4-5", 1_000_000, 1_000_000)
	if !ok || cost != 6.0 {
		t.Fatalf("cost=%v ok=%v", cost, ok)
	}
	if cost, ok := r.Cost([]string{"x"}, "y", 10, 10); ok || cost != 0 {
		t.Fatalf("unknown model must cost 0/false, got %v/%v", cost, ok)
	}
}
