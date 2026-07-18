package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadWithEnvInterpolation(t *testing.T) {
	t.Setenv("TEST_RELAY_KEY", "sk-test-123")
	cfg, err := Load(writeConfig(t, `
providers:
  openai:
    api_key: "${TEST_RELAY_KEY}"
  groq:
    profile: groq
    api_key: ["${TEST_RELAY_KEY}", "${TEST_RELAY_MISSING}"]
routing:
  static:
    fast: "groq/llama-3.3-70b"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers["openai"].APIKey.First(); got != "sk-test-123" {
		t.Fatalf("api_key: %q", got)
	}
	if cfg.Providers["openai"].Type != "openai" {
		t.Fatalf("openai type inference: %q", cfg.Providers["openai"].Type)
	}
	groq := cfg.Providers["groq"]
	if groq.Type != "openai-compat" || !strings.Contains(groq.BaseURL, "groq.com") {
		t.Fatalf("groq profile not applied: %+v", groq)
	}
	// Missing env var expands empty and is dropped from the key list, with a warning.
	if len(groq.APIKey) != 1 {
		t.Fatalf("empty key should be dropped: %v", groq.APIKey)
	}
	if len(cfg.Warnings) == 0 || !strings.Contains(cfg.Warnings[0], "TEST_RELAY_MISSING") {
		t.Fatalf("want missing-env warning, got %v", cfg.Warnings)
	}
	if cfg.Server.Listen != DefaultListen {
		t.Fatalf("default listen: %q", cfg.Server.Listen)
	}
}

func TestReservedFieldsWarnNotError(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
providers:
  openai: {}
cache: { enabled: true, ttl: 10m }
translate: { strictness: strict }
logging: { retain: 90d }
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"translate.strictness", "logging.retain"} {
		found := false
		for _, w := range cfg.Warnings {
			if strings.Contains(w, field) {
				found = true
			}
		}
		if !found {
			t.Errorf("reserved field %s: no warning in %v", field, cfg.Warnings)
		}
	}
	if !cfg.Cache.Enabled {
		t.Fatal("cache.enabled not parsed")
	}
	if got := cfg.Logging.Retain.Std(); got != 90*24*time.Hour {
		t.Fatalf("retain 90d parsed as %v", got)
	}
}

func TestSmartRoutingConfig(t *testing.T) {
	// Valid: chains reference an alias and a provider/model; local embedder.
	cfg, err := Load(writeConfig(t, `
providers:
  openai: {}
  ollama: {}
routing:
  default: smart
  aliases:
    cheap: [ollama/llama3.2]
  smart:
    easy: cheap
    hard: openai/gpt-5
    embeddings: ollama/nomic-embed-text
    tier: knn
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Smart.Tier != "knn" {
		t.Fatalf("smart config: %+v", cfg.Routing.Smart)
	}

	// Remote embedder without the explicit opt-in is refused.
	_, err = Load(writeConfig(t, `
providers:
  openai: {}
routing:
  default: smart
  smart: { easy: openai/gpt-4o-mini, hard: openai/gpt-5, embeddings: openai/text-embedding-3-small, tier: knn }
`))
	if err == nil || !strings.Contains(err.Error(), "allow_remote_embeddings") {
		t.Fatalf("remote embedder must require opt-in, got %v", err)
	}

	// With the opt-in it loads, but warns.
	cfg, err = Load(writeConfig(t, `
providers:
  openai: {}
routing:
  default: smart
  smart:
    easy: openai/gpt-4o-mini
    hard: openai/gpt-5
    embeddings: openai/text-embedding-3-small
    allow_remote_embeddings: true
    tier: knn
`))
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "prompts will be sent to remote provider") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("remote embedder opt-in must still warn: %v", cfg.Warnings)
	}

	// Broken smart configs are rejected.
	for name, content := range map[string]string{
		"missing chains":   "providers:\n  openai: {}\nrouting:\n  default: smart\n",
		"bad tier":         "providers:\n  openai: {}\nrouting:\n  default: smart\n  smart: {easy: openai/a, hard: openai/b, tier: quantum}\n",
		"knn no embedder":  "providers:\n  openai: {}\nrouting:\n  default: smart\n  smart: {easy: openai/a, hard: openai/b, tier: knn}\n",
		"bad chain target": "providers:\n  openai: {}\nrouting:\n  default: smart\n  smart: {easy: nowhere/x, hard: openai/b}\n",
		"bad default":      "providers:\n  openai: {}\nrouting:\n  default: smartest\n",
	} {
		if _, err := Load(writeConfig(t, content)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}

	// log_prompts: embeddings without an embedding source downgrades to off.
	cfg, err = Load(writeConfig(t, "providers:\n  openai: {}\nlogging: { log_prompts: embeddings }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.LogPrompts != "off" {
		t.Fatalf("log_prompts should downgrade to off, got %q", cfg.Logging.LogPrompts)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"unknown provider type":  "providers:\n  x:\n    type: bogus\n",
		"compat without url":     "providers:\n  x:\n    type: openai-compat\n",
		"bad static route":       "providers:\n  openai: {}\nrouting:\n  static:\n    fast: not-a-pair\n",
		"unknown route provider": "providers:\n  openai: {}\nrouting:\n  static:\n    fast: nope/model\n",
		"unknown log_prompts":    "logging:\n  log_prompts: everything\n",
		"unknown yaml field":     "servers:\n  listen: x\n",
		"alias unknown policy":   "providers:\n  openai: {}\nrouting:\n  aliases:\n    x: {policy: smartest, candidates: [openai/gpt-4o]}\n",
		"alias empty":            "providers:\n  openai: {}\nrouting:\n  aliases:\n    x: {policy: cheapest}\n",
		"alias bad target":       "providers:\n  openai: {}\nrouting:\n  aliases:\n    x: [nowhere/model]\n",
		"weighted no children":   "providers:\n  openai: {}\nrouting:\n  aliases:\n    x: {policy: weighted, candidates: [openai/a]}\n",
		"weighted zero weight":   "providers:\n  openai: {}\nrouting:\n  aliases:\n    x:\n      policy: weighted\n      children: [{target: openai/a, weight: 0}]\n",
	}
	for name, content := range cases {
		if _, err := Load(writeConfig(t, content)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestAliasShapes(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
providers:
  openai: {}
  ollama: {}
routing:
  aliases:
    fast: [openai/gpt-4o-mini, ollama/llama3]
    cheap: {policy: cheapest, candidates: [ollama/llama3, openai/gpt-4o-mini]}
    smart: [cheap, openai/gpt-5]
    canary:
      policy: weighted
      children:
        - {target: openai/gpt-4o, weight: 95}
        - {target: ollama/llama3, weight: 5}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Routing.Aliases["fast"].Candidates; len(got) != 2 || got[0] != "openai/gpt-4o-mini" {
		t.Fatalf("list shorthand: %+v", got)
	}
	if cfg.Routing.Aliases["cheap"].Policy != "cheapest" {
		t.Fatalf("policy mapping: %+v", cfg.Routing.Aliases["cheap"])
	}
	// "cheap" inside smart's chain is accepted as an alias reference.
	if got := cfg.Routing.Aliases["smart"].Candidates; got[0] != "cheap" {
		t.Fatalf("alias ref: %+v", got)
	}
	canary := cfg.Routing.Aliases["canary"]
	if len(canary.Children) != 2 || canary.Children[0].Weight != 95 {
		t.Fatalf("weighted children: %+v", canary.Children)
	}
}

func TestUntypedProviderWithProfileName(t *testing.T) {
	t.Setenv("K", "k")
	cfg, err := Load(writeConfig(t, "providers:\n  deepseek:\n    api_key: \"${K}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["deepseek"]
	if p.Type != "openai-compat" || !strings.Contains(p.BaseURL, "deepseek.com") {
		t.Fatalf("profile-by-name inference failed: %+v", p)
	}
}

func TestSniffZeroConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-x")
	t.Setenv("GROQ_API_KEY", "gk-x")
	cfg := Sniff()
	if _, ok := cfg.Providers["openai"]; !ok {
		t.Fatal("OPENAI_API_KEY should register openai")
	}
	if _, ok := cfg.Providers["groq"]; !ok {
		t.Fatal("GROQ_API_KEY should register groq")
	}
	if cfg.Server.Listen != DefaultListen {
		t.Fatalf("listen: %q", cfg.Server.Listen)
	}
}
