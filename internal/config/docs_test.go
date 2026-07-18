package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var yamlFence = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// configKey marks a yaml block as a relay config example (vs. an unrelated
// fragment) by the presence of a top-level config section.
var configKey = regexp.MustCompile(`(?m)^(server|providers|routing|reliability|cache|logging|translate):`)

// TestDocExamplesLoad guards against docs drifting from the schema: every
// yaml config example embedded in DESIGN.md and README.md must load
// cleanly through the real parser. A copy-pasteable example that errors
// is a P1 doc bug — the first-five-minutes experience failing.
func TestDocExamplesLoad(t *testing.T) {
	found := 0
	for _, doc := range []string{"DESIGN.md", "README.md"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", doc))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range yamlFence.FindAllStringSubmatch(string(raw), -1) {
			block := m[1]
			if !configKey.MatchString(block) {
				continue
			}
			found++
			path := filepath.Join(t.TempDir(), "relay.yaml")
			if err := os.WriteFile(path, []byte(block), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Errorf("%s yaml block %d does not load: %v\n---\n%s", doc, i+1, err, block)
				continue
			}
			// Warnings are fine (unset env vars, reserved fields) as long
			// as each names its field rather than being a parse leftover.
			for _, w := range cfg.Warnings {
				if strings.Contains(w, "unknown") {
					t.Errorf("%s yaml block %d: unexpected warning %q", doc, i+1, w)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no yaml config examples found in docs — extraction regex broken?")
	}
}
