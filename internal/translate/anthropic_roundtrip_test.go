package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relay-llm/relay/internal/api/anthropic"
)

// normalizeAnthropic canonicalizes the dialect's wire unions so that
// semantically identical forms compare equal: string content ⇄ a single
// text block, string system ⇄ text blocks, and zero-valued optional cache
// counters ⇄ absent ones. Nulls are stripped as in the OpenAI suite.
func normalizeAnthropic(t *testing.T, b []byte) any {
	t.Helper()
	v := normalizeJSON(t, b)
	return canonAnthropic(v, "")
}

func canonAnthropic(v any, parentKey string) any {
	switch v := v.(type) {
	case map[string]any:
		if s, ok := v["system"].(string); ok {
			v["system"] = []any{map[string]any{"type": "text", "text": s}}
		}
		if s, ok := v["content"].(string); ok {
			// message or tool_result content in string form
			v["content"] = []any{map[string]any{"type": "text", "text": s}}
		}
		for _, k := range []string{"cache_creation_input_tokens", "cache_read_input_tokens"} {
			if n, ok := v[k].(float64); ok && n == 0 {
				delete(v, k)
			}
		}
		for k, val := range v {
			v[k] = canonAnthropic(val, k)
		}
		return v
	case []any:
		for i := range v {
			v[i] = canonAnthropic(v[i], parentKey)
		}
		return v
	default:
		return v
	}
}

func anthropicFixtures(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "anthropic", dir, "*"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures in %s (err=%v)", dir, err)
	}
	return paths
}

// TestAnthropicRequestRoundTrip is the binding same-dialect identity suite
// for the Anthropic dialect (DESIGN §5.3): Anthropic-in → IR →
// Anthropic-out must be semantically identical — cache_control blocks,
// thinking blocks, and top-level passthrough (thinking config) included.
func TestAnthropicRequestRoundTrip(t *testing.T) {
	for _, path := range anthropicFixtures(t, "requests") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			runAnthropicRequestRoundTrip(t, raw)
		})
	}
}

func runAnthropicRequestRoundTrip(t *testing.T, raw []byte) {
	t.Helper()
	wire, err := anthropic.ParseMessagesRequest(raw, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := FromAnthropicRequest(wire)
	if err != nil {
		t.Fatalf("to IR: %v", err)
	}
	out, err := ToAnthropicRequest(ir)
	if err != nil {
		t.Fatalf("from IR: %v", err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	w, g := normalizeAnthropic(t, raw), normalizeAnthropic(t, got)
	if !reflect.DeepEqual(w, g) {
		gotJSON, _ := json.Marshal(g)
		wantJSON, _ := json.Marshal(w)
		t.Fatalf("not semantically equal\nwant: %s\ngot:  %s", wantJSON, gotJSON)
	}
}

// TestAnthropicResponseRoundTrip mirrors the request suite for responses.
func TestAnthropicResponseRoundTrip(t *testing.T) {
	for _, path := range anthropicFixtures(t, "responses") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			runAnthropicResponseRoundTrip(t, raw)
		})
	}
}

func runAnthropicResponseRoundTrip(t *testing.T, raw []byte) {
	t.Helper()
	var wire anthropic.MessagesResponse
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("parse: %v", err)
	}
	ir, err := FromAnthropicResponse(&wire)
	if err != nil {
		t.Fatalf("to IR: %v", err)
	}
	out, err := ToAnthropicResponse(ir)
	if err != nil {
		t.Fatalf("from IR: %v", err)
	}
	got, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	w, g := normalizeAnthropic(t, raw), normalizeAnthropic(t, got)
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("not semantically equal\nwant: %s\ngot:  %s", raw, got)
	}
}

// TestClaudeCodeCacheControlSurvives pins the specific field whose silent
// loss would break prompt caching for Claude Code users.
func TestClaudeCodeCacheControlSurvives(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "anthropic", "requests", "claude_code_shape.json"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := anthropic.ParseMessagesRequest(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	ir, err := FromAnthropicRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ToAnthropicRequest(ir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(out)
	for _, want := range []string{
		`"cache_control":{"type":"ephemeral"}`,
		`"user_id":"session-abc"`,
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("output lost %s:\n%s", want, got)
		}
	}
	if len(out.System.Blocks) != 2 || len(out.System.Blocks[0].Extra) == 0 {
		t.Fatalf("system cache_control lost: %+v", out.System)
	}
	if len(out.Tools) != 2 || len(out.Tools[1].Extra) == 0 {
		t.Fatalf("tool cache_control lost: %+v", out.Tools)
	}
}
