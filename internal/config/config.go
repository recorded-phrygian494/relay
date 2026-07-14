// Package config loads relay.yaml, applies environment-variable
// interpolation, supplies the zero-config fallback (sniff well-known env
// vars, probe a local Ollama), and watches the file for hot reload.
package config

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/relay-llm/relay/internal/provider/openaicompat"
)

// Config is the root of relay.yaml.
type Config struct {
	Server    Server              `yaml:"server"`
	Providers map[string]Provider `yaml:"providers"`
	Routing   Routing             `yaml:"routing"`
	Logging   Logging             `yaml:"logging"`

	// Path is the file this config was loaded from; empty for zero-config.
	Path string `yaml:"-"`
	// Warnings collected during load (missing env vars, ignored fields).
	Warnings []string `yaml:"-"`
}

// Server configures the inbound listener.
type Server struct {
	Listen string `yaml:"listen"`
	// APIKeys are inbound bearer keys. Empty is allowed only on loopback
	// listeners (DESIGN §0.6).
	APIKeys StringList `yaml:"api_keys"`
}

// Provider configures one upstream.
type Provider struct {
	Type    string            `yaml:"type"` // openai | openai-compat | ollama (phase 1)
	Profile string            `yaml:"profile"`
	BaseURL string            `yaml:"base_url"`
	APIKey  StringList        `yaml:"api_key"` // list = key pool (phase 3); first key used now
	Headers map[string]string `yaml:"headers"`
}

// Routing configures the routing policy. Phase 1: static only.
type Routing struct {
	// Static maps a requested model name to "provider/model".
	Static map[string]string `yaml:"static"`
	// DefaultProvider serves otherwise-unresolved model names.
	DefaultProvider string `yaml:"default_provider"`
}

// Logging configures the local SQLite request log.
type Logging struct {
	DB string `yaml:"db"`
	// LogPrompts: "off" (default) | "embeddings" | "full" (DESIGN §8).
	LogPrompts string `yaml:"log_prompts"`
}

// StringList decodes a YAML scalar or sequence into a slice.
type StringList []string

// UnmarshalYAML accepts "x" or ["x", "y"].
func (s *StringList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var one string
		if err := node.Decode(&one); err != nil {
			return err
		}
		if one != "" {
			*s = StringList{one}
		}
		return nil
	}
	var many []string
	if err := node.Decode(&many); err != nil {
		return err
	}
	out := many[:0]
	for _, v := range many {
		if v != "" {
			out = append(out, v)
		}
	}
	*s = out
	return nil
}

// First returns the first key or "".
func (s StringList) First() string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

const (
	DefaultListen = "127.0.0.1:4000"
	defaultDBName = "relay.db"
)

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} references; missing vars expand to "" and are
// reported as warnings.
func expandEnv(raw string) (string, []string) {
	var warnings []string
	out := envPattern.ReplaceAllStringFunc(raw, func(m string) string {
		name := envPattern.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("environment variable %s is not set (expanded to empty)", name))
		}
		return v
	})
	return out, warnings
}

// DefaultDBPath is ~/.relay/relay.db, created on demand by the store.
func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return defaultDBName
	}
	return filepath.Join(home, ".relay", defaultDBName)
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, warnings := expandEnv(string(raw))
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Path = path
	cfg.Warnings = warnings
	if err := cfg.finalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// finalize applies defaults and validates.
func (c *Config) finalize() error {
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Logging.DB == "" {
		c.Logging.DB = DefaultDBPath()
	}
	if strings.HasPrefix(c.Logging.DB, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			c.Logging.DB = filepath.Join(home, c.Logging.DB[2:])
		}
	}
	switch c.Logging.LogPrompts {
	case "", "off":
		c.Logging.LogPrompts = "off"
	case "embeddings", "full":
	default:
		return fmt.Errorf("logging.log_prompts: unknown value %q (want off | embeddings | full)", c.Logging.LogPrompts)
	}

	for name, p := range c.Providers {
		switch p.Type {
		case "":
			// Untyped: well-known names imply their type; a profile implies compat.
			switch {
			case name == "openai":
				p.Type = "openai"
			case name == "ollama":
				p.Type = "ollama"
			case p.Profile != "":
				p.Type = "openai-compat"
			case openaicompat.Profiles[name].BaseURL != "":
				p.Type = "openai-compat"
				p.Profile = name
			default:
				return fmt.Errorf("providers.%s: missing type (want openai | openai-compat | ollama)", name)
			}
		case "openai", "ollama", "openai-compat":
		case "anthropic", "gemini":
			return fmt.Errorf("providers.%s: type %q lands in phase 2", name, p.Type)
		default:
			return fmt.Errorf("providers.%s: unknown type %q", name, p.Type)
		}
		if p.Profile != "" {
			prof, ok := openaicompat.Profiles[p.Profile]
			if !ok {
				return fmt.Errorf("providers.%s: unknown profile %q", name, p.Profile)
			}
			if p.BaseURL == "" {
				p.BaseURL = prof.BaseURL
			}
		}
		if p.Type == "openai-compat" && p.BaseURL == "" {
			return fmt.Errorf("providers.%s: openai-compat requires base_url or profile", name)
		}
		c.Providers[name] = p
	}

	for alias, target := range c.Routing.Static {
		prov, _, ok := strings.Cut(target, "/")
		if !ok {
			return fmt.Errorf("routing.static.%s: %q is not provider/model", alias, target)
		}
		if _, exists := c.Providers[prov]; !exists {
			return fmt.Errorf("routing.static.%s: unknown provider %q", alias, prov)
		}
	}
	if dp := c.Routing.DefaultProvider; dp != "" {
		if _, ok := c.Providers[dp]; !ok {
			return fmt.Errorf("routing.default_provider: unknown provider %q", dp)
		}
	}
	return nil
}

// Find locates the config file: explicit path, ./relay.yaml, then
// ~/.relay/relay.yaml. Returns "" when none exists (zero-config mode).
func Find(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if _, err := os.Stat("relay.yaml"); err == nil {
		return "relay.yaml"
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".relay", "relay.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Sniff builds a zero-config Config from well-known environment variables
// and a local Ollama probe (DESIGN §8).
func Sniff() *Config {
	cfg := &Config{
		Providers: map[string]Provider{},
		Routing:   Routing{Static: map[string]string{}},
	}
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		cfg.Providers["openai"] = Provider{Type: "openai", APIKey: StringList{k}}
	}
	for name, prof := range openaicompat.Profiles {
		if k := os.Getenv(prof.EnvKey); k != "" {
			cfg.Providers[name] = Provider{Type: "openai-compat", Profile: name, BaseURL: prof.BaseURL, APIKey: StringList{k}}
		}
	}
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		cfg.Warnings = append(cfg.Warnings, "ANTHROPIC_API_KEY found but the anthropic adapter lands in phase 2; ignoring for now")
	}
	if probeOllama() {
		cfg.Providers["ollama"] = Provider{Type: "ollama"}
	}
	_ = cfg.finalize()
	return cfg
}

// probeOllama reports whether a local Ollama answers /api/tags quickly.
func probeOllama() bool {
	client := &http.Client{Timeout: 750 * time.Millisecond}
	resp, err := client.Get("http://localhost:11434/api/tags")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Watch polls path's mtime every interval and invokes onChange with each
// successfully loaded new config. Invalid edits are reported via onError and
// the previous config stays live. Returns a stop function.
func Watch(path string, interval time.Duration, onChange func(*Config), onError func(error)) (stop func()) {
	done := make(chan struct{})
	go func() {
		var lastMod time.Time
		if fi, err := os.Stat(path); err == nil {
			lastMod = fi.ModTime()
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fi, err := os.Stat(path)
				if err != nil || !fi.ModTime().After(lastMod) {
					continue
				}
				lastMod = fi.ModTime()
				cfg, err := Load(path)
				if err != nil {
					onError(err)
					continue
				}
				onChange(cfg)
			}
		}
	}()
	return func() { close(done) }
}
