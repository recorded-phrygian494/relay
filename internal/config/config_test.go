package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestLoadRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"unknown provider type": "providers:\n  x:\n    type: bogus\n",
		"compat without url":    "providers:\n  x:\n    type: openai-compat\n",
		"bad static route":      "providers:\n  openai: {}\nrouting:\n  static:\n    fast: not-a-pair\n",
		"unknown route provider": "providers:\n  openai: {}\nrouting:\n  static:\n    fast: nope/model\n",
		"unknown log_prompts":   "logging:\n  log_prompts: everything\n",
		"unknown yaml field":    "servers:\n  listen: x\n",
	}
	for name, content := range cases {
		if _, err := Load(writeConfig(t, content)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
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
