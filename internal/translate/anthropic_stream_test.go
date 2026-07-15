package translate

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relay-llm/relay/internal/api/anthropic"
	"github.com/relay-llm/relay/internal/core"
	"github.com/relay-llm/relay/internal/sse"
)

func readAnthropicStream(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "anthropic", "streams", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func collectAnthropicEvents(t *testing.T, raw string) []core.Event {
	t.Helper()
	s := NewAnthropicStream(io.NopCloser(strings.NewReader(raw)))
	var events []core.Event
	for {
		ev, err := s.Next()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		events = append(events, ev)
	}
}

func TestParseAnthropicStreamText(t *testing.T) {
	events := collectAnthropicEvents(t, readAnthropicStream(t, "text.txt"))
	want := []core.EventKind{
		core.EventMessageStart, core.EventTextDelta, core.EventTextDelta,
		core.EventMessageEnd, core.EventUsage,
	}
	if got := kinds(events); !equalKinds(got, want) {
		t.Fatalf("kinds\nwant %v\ngot  %v", want, got)
	}
	if events[0].ID != "msg_01S" || events[0].Model != "claude-sonnet-5" {
		t.Fatalf("message start: %+v", events[0])
	}
	if events[1].Text+events[2].Text != "1 2 3 4 5" {
		t.Fatalf("text: %q %q", events[1].Text, events[2].Text)
	}
	if events[3].StopReason != core.StopEndTurn {
		t.Fatalf("stop: %v", events[3].StopReason)
	}
	// input tokens from message_start, output from message_delta
	if u := events[4].Usage; u.InputTokens != 12 || u.OutputTokens != 8 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestParseAnthropicStreamTools(t *testing.T) {
	events := collectAnthropicEvents(t, readAnthropicStream(t, "tools.txt"))
	want := []core.EventKind{
		core.EventMessageStart, core.EventTextDelta,
		core.EventToolCallStart, core.EventToolCallDelta, core.EventToolCallDelta,
		core.EventToolCallEnd, core.EventMessageEnd, core.EventUsage,
	}
	if got := kinds(events); !equalKinds(got, want) {
		t.Fatalf("kinds\nwant %v\ngot  %v", want, got)
	}
	if events[2].ToolID != "toolu_01W" || events[2].ToolName != "get_weather" {
		t.Fatalf("tool start: %+v", events[2])
	}
	if args := events[3].ArgsFragment + events[4].ArgsFragment; args != `{"city":"Dublin"}` {
		t.Fatalf("args: %s", args)
	}
	if events[6].StopReason != core.StopToolUse {
		t.Fatalf("stop: %v", events[6].StopReason)
	}
}

// anthropicAggregate folds an Anthropic SSE stream into its semantic
// message, mirroring aggregateSSE for the OpenAI dialect.
type anthropicAggregate struct {
	text   string
	tools  map[string]string // id -> name + "|" + assembled json
	stop   string
	output int
}

func aggregateAnthropicSSE(t *testing.T, raw string) anthropicAggregate {
	t.Helper()
	agg := anthropicAggregate{tools: map[string]string{}}
	openTools := map[int]string{} // block index -> tool id
	r := sse.NewReader(strings.NewReader(raw))
	for {
		wire, err := r.Next()
		if err == io.EOF {
			return agg
		}
		if err != nil {
			t.Fatal(err)
		}
		var ev anthropic.StreamEvent
		if err := json.Unmarshal([]byte(wire.Data), &ev); err != nil {
			t.Fatalf("bad event %q: %v", wire.Data, err)
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				openTools[*ev.Index] = ev.ContentBlock.ID
				agg.tools[ev.ContentBlock.ID] = ev.ContentBlock.Name + "|"
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				agg.text += ev.Delta.Text
			case "input_json_delta":
				agg.tools[openTools[*ev.Index]] += ev.Delta.PartialJSON
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				agg.stop = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				agg.output = ev.Usage.OutputTokens
			}
		}
	}
}

// TestAnthropicStreamRoundTrip: fixture SSE → core events → re-emitted SSE
// must aggregate to the same semantic message (binding suite, streaming).
func TestAnthropicStreamRoundTrip(t *testing.T) {
	for _, name := range []string{"text.txt", "tools.txt"} {
		t.Run(name, func(t *testing.T) {
			runAnthropicStreamRoundTrip(t, readAnthropicStream(t, name))
		})
	}
}

func runAnthropicStreamRoundTrip(t *testing.T, raw string) {
	t.Helper()
	events := collectAnthropicEvents(t, raw)

	rec := httptest.NewRecorder()
	w := NewAnthropicStreamWriter(sse.NewWriter(rec))
	for _, ev := range events {
		if err := w.OnEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Done(); err != nil {
		t.Fatal(err)
	}

	want := aggregateAnthropicSSE(t, raw)
	got := aggregateAnthropicSSE(t, rec.Body.String())
	if want.text != got.text || want.stop != got.stop || want.output != got.output {
		t.Fatalf("aggregate mismatch\nwant %+v\ngot  %+v\nstream:\n%s", want, got, rec.Body.String())
	}
	if len(want.tools) != len(got.tools) {
		t.Fatalf("tools: want %v got %v", want.tools, got.tools)
	}
	for id, v := range want.tools {
		if got.tools[id] != v {
			t.Fatalf("tool %s: want %q got %q", id, v, got.tools[id])
		}
	}
	if !strings.Contains(rec.Body.String(), "event: message_stop") {
		t.Fatal("missing message_stop")
	}
}

// Cross-dialect streaming: an OpenAI upstream stream rendered to an
// Anthropic client must produce a correct envelope with the same semantics.
func TestOpenAIStreamToAnthropicClient(t *testing.T) {
	raw := readStreamFixture(t, "tools.txt")
	events := collectEvents(t, raw)

	rec := httptest.NewRecorder()
	w := NewAnthropicStreamWriter(sse.NewWriter(rec))
	for _, ev := range events {
		if err := w.OnEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Done(); err != nil {
		t.Fatal(err)
	}
	got := aggregateAnthropicSSE(t, rec.Body.String())
	if got.text != "Checking." {
		t.Fatalf("text: %q", got.text)
	}
	if got.stop != "tool_use" {
		t.Fatalf("stop: %q", got.stop)
	}
	if len(got.tools) != 1 || got.tools["call_z9"] != `get_weather|{"city":"Dublin"}` {
		t.Fatalf("tools: %v", got.tools)
	}
	if got.output != 18 {
		t.Fatalf("usage: %+v", got)
	}
	// Envelope order sanity: message_start before any block, blocks closed.
	body := rec.Body.String()
	if strings.Index(body, "message_start") > strings.Index(body, "content_block_start") {
		t.Fatal("message_start must precede content blocks")
	}
}

// And the reverse: an Anthropic upstream stream rendered to an OpenAI client.
func TestAnthropicStreamToOpenAIClient(t *testing.T) {
	events := collectAnthropicEvents(t, readAnthropicStream(t, "tools.txt"))
	rec := httptest.NewRecorder()
	w := NewOpenAIStreamWriter(sse.NewWriter(rec), 1751234570, true)
	for _, ev := range events {
		if err := w.OnEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Done(); err != nil {
		t.Fatal(err)
	}
	got := aggregateSSE(t, rec.Body.String())
	if got.text != "Checking." || got.finish != "tool_calls" {
		t.Fatalf("aggregate: %+v", got)
	}
	if got.toolByID["toolu_01W"] != `get_weather|{"city":"Dublin"}` {
		t.Fatalf("tools: %v", got.toolByID)
	}
	if got.usage == nil || got.usage.PromptTokens != 40 || got.usage.CompletionTokens != 25 {
		t.Fatalf("usage: %+v", got.usage)
	}
}
