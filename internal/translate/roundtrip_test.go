package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/llmrelay/relay/internal/api/openai"
)

// normalizeJSON decodes b and strips null-valued object members recursively:
// "content": null and an absent content key are semantically identical.
func normalizeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	return stripNulls(v)
}

func stripNulls(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range v {
			if val == nil {
				continue
			}
			out[k] = stripNulls(val)
		}
		return out
	case []any:
		for i := range v {
			v[i] = stripNulls(v[i])
		}
		return v
	default:
		return v
	}
}

func requireSemanticEqual(t *testing.T, want, got []byte) {
	t.Helper()
	w := canonOpenAI(normalizeJSON(t, want))
	g := canonOpenAI(normalizeJSON(t, got))
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("not semantically equal\nwant: %s\ngot:  %s", want, got)
	}
}

// canonOpenAI folds equivalent wire forms together: a message's
// "content": "" equals an absent content key (OpenAI proper sends null for
// pure tool-call messages, Ollama's compat endpoint sends "" — recorded
// reality, 2026-07), and streaming tool_calls indexes are positional noise
// in non-streaming messages.
func canonOpenAI(v any) any {
	switch v := v.(type) {
	case map[string]any:
		if _, hasRole := v["role"]; hasRole {
			if s, ok := v["content"].(string); ok && s == "" {
				delete(v, "content")
			}
		}
		if _, hasFn := v["function"]; hasFn {
			delete(v, "index") // Ollama emits streaming-style indexes in non-streaming tool_calls
		}
		for k, val := range v {
			v[k] = canonOpenAI(val)
		}
		return v
	case []any:
		for i := range v {
			v[i] = canonOpenAI(v[i])
		}
		return v
	default:
		return v
	}
}

func fixtures(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "openai", dir, "*"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures in %s (err=%v)", dir, err)
	}
	return paths
}

// TestOpenAIRequestRoundTrip is the binding same-dialect identity suite
// (DESIGN §5.3): OpenAI-in → IR → OpenAI-out must be semantically identical
// for every recorded request fixture, Extra passthrough included.
func TestOpenAIRequestRoundTrip(t *testing.T) {
	for _, path := range fixtures(t, "requests") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := openai.ParseChatRequest(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			ir, err := FromOpenAIRequest(wire)
			if err != nil {
				t.Fatalf("to IR: %v", err)
			}
			out, err := ToOpenAIRequest(ir)
			if err != nil {
				t.Fatalf("from IR: %v", err)
			}
			got, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			requireSemanticEqual(t, raw, got)
		})
	}
}

// TestOpenAIResponseRoundTrip: upstream response → IR → client response
// must be semantically identical for same-dialect hops.
func TestOpenAIResponseRoundTrip(t *testing.T) {
	for _, path := range fixtures(t, "responses") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var wire openai.ChatResponse
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("parse: %v", err)
			}
			ir, err := FromOpenAIResponse(&wire)
			if err != nil {
				t.Fatalf("to IR: %v", err)
			}
			out, err := ToOpenAIResponse(ir)
			if err != nil {
				t.Fatalf("from IR: %v", err)
			}
			got, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			requireSemanticEqual(t, raw, got)
		})
	}
}

func TestFromOpenAIRequestShapes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "openai", "requests", "tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := openai.ParseChatRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	ir, err := FromOpenAIRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Messages) != 4 {
		t.Fatalf("want 4 IR messages, got %d", len(ir.Messages))
	}
	if len(ir.Tools) != 1 || ir.Tools[0].Name != "get_weather" {
		t.Fatalf("tools not translated: %+v", ir.Tools)
	}
	if _, ok := ir.Ext.For("openai")["parallel_tool_calls"]; !ok {
		t.Fatal("unknown field parallel_tool_calls should ride Ext")
	}
}
