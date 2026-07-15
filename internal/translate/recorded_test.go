package translate

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/api/openai"
	"github.com/llmrelay/relay/internal/sse"
)

// TestRecordedOpenAIFixtures runs the binding identity suite over ground
// truth captured by tools/record (any new recording joins automatically).
// Streams are compared by semantic aggregate.
func TestRecordedOpenAIFixtures(t *testing.T) {
	scenarios, err := filepath.Glob(filepath.Join("testdata", "openai", "recorded", "*"))
	if err != nil || len(scenarios) == 0 {
		t.Skip("no recorded openai fixtures; run tools/record")
	}
	for _, dir := range scenarios {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			if raw, err := os.ReadFile(filepath.Join(dir, "request.json")); err == nil {
				wire, err := openai.ParseChatRequest(raw)
				if err != nil {
					t.Fatalf("parse request: %v", err)
				}
				ir, err := FromOpenAIRequest(wire)
				if err != nil {
					t.Fatalf("request to IR: %v", err)
				}
				out, err := ToOpenAIRequest(ir)
				if err != nil {
					t.Fatalf("request from IR: %v", err)
				}
				got, _ := json.Marshal(out)
				requireSemanticEqual(t, raw, got)
			}
			if raw, err := os.ReadFile(filepath.Join(dir, "response.json")); err == nil {
				var wire openai.ChatResponse
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatalf("parse response: %v", err)
				}
				ir, err := FromOpenAIResponse(&wire)
				if err != nil {
					t.Fatalf("response to IR: %v", err)
				}
				out, err := ToOpenAIResponse(ir)
				if err != nil {
					t.Fatalf("response from IR: %v", err)
				}
				got, _ := json.Marshal(out)
				requireSemanticEqual(t, raw, got)
			}
			if raw, err := os.ReadFile(filepath.Join(dir, "stream.txt")); err == nil {
				text := strings.ReplaceAll(string(raw), "\r\n", "\n")
				events := collectEvents(t, []byte(text))
				rec := httptest.NewRecorder()
				w := NewOpenAIStreamWriter(sse.NewWriter(rec), 1, true)
				for _, ev := range events {
					if err := w.OnEvent(ev); err != nil {
						t.Fatal(err)
					}
				}
				_ = w.Done()
				want := aggregateSSE(t, text)
				got := aggregateSSE(t, rec.Body.String())
				if want.text != got.text || want.finish != got.finish {
					t.Fatalf("stream aggregate\nwant %+v\ngot  %+v", want, got)
				}
				for id, v := range want.toolByID {
					if got.toolByID[id] != v {
						t.Fatalf("tool %s: want %q got %q", id, v, got.toolByID[id])
					}
				}
			}
		})
	}
}

// TestRecordedAnthropicFixtures does the same for the Anthropic dialect.
// Skipped until a maintainer records against a real key:
//
//	go run ./tools/record --dialect anthropic --key-env ANTHROPIC_API_KEY --model claude-haiku-4-5-20251001
func TestRecordedAnthropicFixtures(t *testing.T) {
	scenarios, _ := filepath.Glob(filepath.Join("testdata", "anthropic", "recorded", "*"))
	if len(scenarios) == 0 {
		t.Skip("no recorded anthropic fixtures yet (needs ANTHROPIC_API_KEY); synthetic fixtures cover the dialect meanwhile")
	}
	for _, dir := range scenarios {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			if raw, err := os.ReadFile(filepath.Join(dir, "request.json")); err == nil {
				runAnthropicRequestRoundTrip(t, raw)
			}
			if raw, err := os.ReadFile(filepath.Join(dir, "response.json")); err == nil {
				runAnthropicResponseRoundTrip(t, raw)
			}
			if raw, err := os.ReadFile(filepath.Join(dir, "stream.txt")); err == nil {
				runAnthropicStreamRoundTrip(t, strings.ReplaceAll(string(raw), "\r\n", "\n"))
			}
		})
	}
}
