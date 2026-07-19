// Package config loads relay.yaml, applies environment-variable
// interpolation, supplies the zero-config fallback (sniff well-known env
// vars, probe a local Ollama), and watches the file for hot reload.
package config

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/llmrelay/relay/internal/provider/openaicompat"
)

// Config is the root of relay.yaml. Every key of the DESIGN §8 example is
// known to the parser — fields whose feature has not landed yet parse and
// emit a "reserved, ignored" warning rather than failing the strict
// unknown-field check, so the documented example always loads.
type Config struct {
	Server      Server              `yaml:"server"`
	Providers   map[string]Provider `yaml:"providers"`
	Routing     Routing             `yaml:"routing"`
	Logging     Logging             `yaml:"logging"`
	Reliability Reliability         `yaml:"reliability"`
	Cache       Cache               `yaml:"cache"`
	Translate   Translate           `yaml:"translate"`

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

// Routing configures the routing policy.
type Routing struct {
	// Static maps a requested model name to "provider/model".
	Static map[string]string `yaml:"static"`
	// DefaultProvider serves otherwise-unresolved model names.
	DefaultProvider string `yaml:"default_provider"`
	// Aliases are virtual model names: a plain list is a fallback chain; a
	// mapping selects a policy (DESIGN §7/§8).
	Aliases map[string]Alias `yaml:"aliases"`
	// Default is the policy for otherwise-unroutable model names:
	// "static" (default — such names 404) or "smart" (DESIGN §0.3).
	Default string `yaml:"default"`
	// Smart configures the smart policy; only consulted when Default is
	// "smart".
	Smart SmartRouting `yaml:"smart"`
}

// SmartRouting configures tiered smart routing (DESIGN §0.3).
type SmartRouting struct {
	// Easy and Hard are the difficulty chains: an alias name or a bare
	// provider/model.
	Easy string `yaml:"easy"`
	Hard string `yaml:"hard"`
	// Embeddings is the "provider/model" that embeds queries for the
	// KNN tier (dogfoods relay's own embeddings path).
	Embeddings string `yaml:"embeddings"`
	// AllowRemoteEmbeddings must be set explicitly when Embeddings is not
	// a local provider: smart routing silently shipping prompts to a
	// remote API would violate the privacy posture.
	AllowRemoteEmbeddings bool `yaml:"allow_remote_embeddings"`
	// Tier selects the classifier: "lexical" (tier 1, pure Go) or "knn"
	// (tier 2, embedding KNN). Empty selects the launch-gate default.
	Tier string `yaml:"tier"`
	// Reference is the tier-2 reference-set path (default
	// ~/.relay/smart_refs.json).
	Reference string `yaml:"reference"`
}

// Cache configures the exact-match response cache (DESIGN §8).
type Cache struct {
	Enabled bool     `yaml:"enabled"`
	TTL     Duration `yaml:"ttl"`
}

// Translate is reserved: v1 always warns on lossy translation (DESIGN §8).
type Translate struct {
	Strictness string `yaml:"strictness"`
}

// Alias is one virtual model. Two YAML shapes are accepted:
//
//	fast: [groq/llama-3.3-70b, openai/gpt-4o-mini]        # fallback chain
//	cheap: {policy: cheapest, candidates: [a/x, b/y]}
//	canary:
//	  policy: weighted
//	  children: [{target: openai/gpt-4o, weight: 95}, {target: local/m, weight: 5}]
type Alias struct {
	Policy     string          `yaml:"policy"`
	Candidates StringList      `yaml:"candidates"`
	Children   []WeightedChild `yaml:"children"`
}

// WeightedChild is one branch of a weighted split.
type WeightedChild struct {
	Target string `yaml:"target"`
	Weight int    `yaml:"weight"`
}

// UnmarshalYAML accepts the list shorthand or the policy mapping.
func (a *Alias) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.SequenceNode {
		var targets []string
		if err := node.Decode(&targets); err != nil {
			return err
		}
		*a = Alias{Candidates: targets}
		return nil
	}
	type plain Alias // drop the method to avoid recursion
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*a = Alias(p)
	return nil
}

// Duration decodes yaml scalars like "30s" / "5m".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler. On top of time.ParseDuration
// it accepts a whole-day "Nd" suffix ("90d"), used by logging.retain.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	if days, ok := strings.CutSuffix(s, "d"); ok && !strings.ContainsAny(days, "hms") {
		var n float64
		if _, err := fmt.Sscanf(days, "%g", &n); err == nil {
			*d = Duration(time.Duration(n * 24 * float64(time.Hour)))
			return nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std converts to time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Reliability tunes the executor (DESIGN §6/§8).
type Reliability struct {
	// Retries is the per-candidate retry budget for retryable errors.
	Retries *int `yaml:"retries"`
	// TTFTTimeout bounds the wait for a stream's first content event.
	TTFTTimeout Duration `yaml:"ttft_timeout"`
	// RequestTimeout is the whole-chain budget for streaming requests;
	// non-streaming requests get half of it.
	RequestTimeout Duration `yaml:"request_timeout"`
}

// RetryCount applies the default of 3.
func (r Reliability) RetryCount() int {
	if r.Retries == nil {
		return 3
	}
	return *r.Retries
}

// Logging configures the local SQLite request log.
type Logging struct {
	DB string `yaml:"db"`
	// Retain is reserved: log retention is not implemented yet.
	Retain Duration `yaml:"retain"`
	// LogPrompts: "off" (default) | "embeddings" | "full" (DESIGN §8).
	LogPrompts string `yaml:"log_prompts"`
}

// targets returns every raw target string an alias references, whichever
// shape it was declared in.
func (a Alias) targets() []string {
	if len(a.Children) > 0 {
		out := make([]string, 0, len(a.Children))
		for _, ch := range a.Children {
			out = append(out, ch.Target)
		}
		return out
	}
	return a.Candidates
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

// DefaultSmartRefsPath is where the tier-2 reference set persists.
func DefaultSmartRefsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "smart_refs.json"
	}
	return filepath.Join(home, ".relay", "smart_refs.json")
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
	// Reserved fields: accepted so the DESIGN §8 example loads verbatim,
	// but inert until their feature lands — say so instead of silently
	// pretending.
	if c.Translate.Strictness != "" && c.Translate.Strictness != "warn" {
		c.Warnings = append(c.Warnings, fmt.Sprintf("translate.strictness: only %q is implemented; %q ignored", "warn", c.Translate.Strictness))
	}
	if c.Logging.Retain != 0 {
		c.Warnings = append(c.Warnings, "logging.retain: log retention is not implemented yet; ignored")
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
			case name == "anthropic":
				p.Type = "anthropic"
			case name == "gemini":
				p.Type = "gemini"
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
		case "openai", "anthropic", "gemini", "ollama", "openai-compat":
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
	for name, a := range c.Routing.Aliases {
		switch a.Policy {
		case "", "fallback", "cheapest", "fastest":
			if len(a.Children) > 0 {
				return fmt.Errorf("routing.aliases.%s: children is only for policy weighted", name)
			}
			if len(a.Candidates) == 0 {
				return fmt.Errorf("routing.aliases.%s: no candidates", name)
			}
		case "weighted":
			if len(a.Candidates) > 0 {
				return fmt.Errorf("routing.aliases.%s: weighted takes children, not candidates", name)
			}
			if len(a.Children) == 0 {
				return fmt.Errorf("routing.aliases.%s: weighted needs children", name)
			}
			for i, ch := range a.Children {
				if ch.Target == "" {
					return fmt.Errorf("routing.aliases.%s.children[%d]: missing target", name, i)
				}
				if ch.Weight <= 0 {
					return fmt.Errorf("routing.aliases.%s.children[%d]: weight must be positive", name, i)
				}
			}
		default:
			return fmt.Errorf("routing.aliases.%s: unknown policy %q (want fallback | cheapest | fastest | weighted)", name, a.Policy)
		}
		for _, raw := range a.targets() {
			prov, _, ok := strings.Cut(raw, "/")
			if ok {
				if _, exists := c.Providers[prov]; exists {
					continue
				}
			}
			if _, isAlias := c.Routing.Aliases[raw]; isAlias {
				continue // alias reference; cycles are caught at compile
			}
			return fmt.Errorf("routing.aliases.%s: target %q is neither provider/model nor a known alias", name, raw)
		}
	}
	if dp := c.Routing.DefaultProvider; dp != "" {
		if _, ok := c.Providers[dp]; !ok {
			return fmt.Errorf("routing.default_provider: unknown provider %q", dp)
		}
	}

	switch c.Routing.Default {
	case "", "static":
	case "smart":
		s := &c.Routing.Smart
		if s.Easy == "" || s.Hard == "" {
			return fmt.Errorf("routing.default: smart requires routing.smart.easy and routing.smart.hard chains")
		}
		for _, t := range []struct{ field, target string }{{"easy", s.Easy}, {"hard", s.Hard}} {
			if err := c.validateChainTarget(t.target); err != nil {
				return fmt.Errorf("routing.smart.%s: %w", t.field, err)
			}
		}
		switch s.Tier {
		case "":
			// §0.3 launch-gate outcome (v2 re-run, 2026-07-19): no smart
			// tier has proven cost-at-equal-quality on the held-out set,
			// so nothing is silently on-by-default. KNN is the documented
			// tier when an embedder is configured; otherwise the choice is
			// explicit.
			if s.Embeddings != "" {
				s.Tier = "knn"
			} else {
				return fmt.Errorf("routing.smart.tier: choose explicitly — \"knn\" (documented tier, needs routing.smart.embeddings) or \"lexical\" (experimental: did not pass the launch gate on the held-out eval set; run `relay eval` on your own traffic first)")
			}
		case "lexical":
		case "knn":
			if s.Embeddings == "" {
				return fmt.Errorf("routing.smart.tier: knn requires routing.smart.embeddings")
			}
		default:
			return fmt.Errorf("routing.smart.tier: unknown tier %q (want lexical | knn)", s.Tier)
		}
	default:
		return fmt.Errorf("routing.default: unknown policy %q (want static | smart)", c.Routing.Default)
	}
	if e := c.Routing.Smart.Embeddings; e != "" {
		prov, _, ok := strings.Cut(e, "/")
		if !ok {
			return fmt.Errorf("routing.smart.embeddings: %q is not provider/model", e)
		}
		pc, exists := c.Providers[prov]
		if !exists {
			return fmt.Errorf("routing.smart.embeddings: unknown provider %q", prov)
		}
		if !providerIsLocal(pc) {
			// Privacy posture: smart routing must never silently ship
			// prompts to a remote API just to classify them.
			if !c.Routing.Smart.AllowRemoteEmbeddings {
				return fmt.Errorf("routing.smart.embeddings: %q is a remote provider — routing would send every prompt to it for classification; set routing.smart.allow_remote_embeddings: true to accept that", prov)
			}
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"routing.smart: prompts will be sent to remote provider %q for routing classification (allow_remote_embeddings: true)", prov))
		}
	}
	if c.Logging.LogPrompts == "embeddings" && c.Routing.Smart.Embeddings == "" {
		c.Warnings = append(c.Warnings, "logging.log_prompts: \"embeddings\" needs an embedding source (routing.smart.embeddings); falling back to \"off\"")
		c.Logging.LogPrompts = "off"
	}
	return nil
}

// validateChainTarget accepts an alias name or a provider/model pair.
func (c *Config) validateChainTarget(target string) error {
	if _, isAlias := c.Routing.Aliases[target]; isAlias {
		return nil
	}
	prov, _, ok := strings.Cut(target, "/")
	if ok {
		if _, exists := c.Providers[prov]; exists {
			return nil
		}
	}
	return fmt.Errorf("target %q is neither a configured alias nor provider/model", target)
}

// providerIsLocal reports whether an embedder stays on this machine:
// Ollama, or any provider whose base_url points at a loopback host.
func providerIsLocal(p Provider) bool {
	if p.Type == "ollama" {
		return true
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		cfg.Providers["anthropic"] = Provider{Type: "anthropic", APIKey: StringList{k}}
	}
	if k := os.Getenv("GEMINI_API_KEY"); k != "" {
		cfg.Providers["gemini"] = Provider{Type: "gemini", APIKey: StringList{k}}
	} else if k := os.Getenv("GOOGLE_API_KEY"); k != "" {
		cfg.Providers["gemini"] = Provider{Type: "gemini", APIKey: StringList{k}}
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
