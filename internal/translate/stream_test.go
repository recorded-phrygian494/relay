package translate

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"encoding/json"

	"github.com/relay-llm/relay/internal/api/openai"
	"github.com/relay-llm/relay/internal/core"
	"github.com/relay-llm/relay/internal/sse"
)

func readStreamFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "openai", "streams", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func collectEvents(t *testing.T, raw []byte) []core.Event {
	t.Helper()
	s := NewOpenAIStream(io.NopCloser(strings.NewReader(string(raw))))
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

func kinds(events []core.Event) []core.EventKind {
	out := make([]core.EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func TestParseOpenAIStreamText(t *testing.T) {
	events := collectEvents(t, readStreamFixture(t, "text.txt"))
	want := []core.EventKind{
		core.EventMessageStart, core.EventTextDelta, core.EventTextDelta,
		core.EventMessageEnd, core.EventUsage,
	}
	if got := kinds(events); !equalKinds(got, want) {
		t.Fatalf("event kinds\nwant %v\ngot  %v", want, got)
	}
	if events[1].Text+events[2].Text != "Rayleigh scattering." {
		t.Fatalf("text deltas wrong: %q %q", events[1].Text, events[2].Text)
	}
	if events[3].StopReason != core.StopEndTurn {
		t.Fatalf("stop reason: %v", events[3].StopReason)
	}
	if u := events[4].Usage; u == nil || u.InputTokens != 12 || u.OutputTokens != 4 {
		t.Fatalf("usage: %+v", events[4].Usage)
	}
}

func TestParseOpenAIStreamToolCalls(t *testing.T) {
	events := collectEvents(t, readStreamFixture(t, "tools.txt"))
	want := []core.EventKind{
		core.EventMessageStart, core.EventTextDelta,
		core.EventToolCallStart, core.EventToolCallDelta, core.EventToolCallDelta,
		core.EventToolCallEnd, core.EventMessageEnd, core.EventUsage,
	}
	if got := kinds(events); !equalKinds(got, want) {
		t.Fatalf("event kinds\nwant %v\ngot  %v", want, got)
	}
	if events[2].ToolName != "get_weather" || events[2].ToolID != "call_z9" {
		t.Fatalf("tool start: %+v", events[2])
	}
	if args := events[3].ArgsFragment + events[4].ArgsFragment; args != `{"city":"Dublin"}` {
		t.Fatalf("assembled args: %s", args)
	}
	if events[6].StopReason != core.StopToolUse {
		t.Fatalf("stop reason: %v", events[6].StopReason)
	}
}

func equalKinds(a, b []core.EventKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// aggregate folds a raw OpenAI SSE stream into its final semantic message:
// concatenated text, assembled tool calls, finish reason, usage. Streaming
// round-trip identity is defined over this aggregate (chunk boundaries and
// role-chunk placement are not semantic).
type aggregate struct {
	text     string
	toolByID map[string]string // id -> name + "|" + args
	finish   string
	usage    *openai.Usage
}

func aggregateSSE(t *testing.T, raw string) aggregate {
	t.Helper()
	agg := aggregate{toolByID: map[string]string{}}
	var openToolID string
	r := sse.NewReader(strings.NewReader(raw))
	for {
		ev, err := r.Next()
		if err == io.EOF {
			return agg
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Data == "[DONE]" {
			continue
		}
		var chunk openai.ChatChunk
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", ev.Data, err)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != nil {
				agg.text += *c.Delta.Content
			}
			for _, tc := range c.Delta.ToolCalls {
				if tc.ID != "" {
					openToolID = tc.ID
					agg.toolByID[openToolID] = tc.Function.Name + "|"
				}
				agg.toolByID[openToolID] += tc.Function.Arguments
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				agg.finish = *c.FinishReason
			}
		}
		if chunk.Usage != nil {
			agg.usage = chunk.Usage
		}
	}
}

// TestOpenAIStreamRoundTrip: fixture SSE → core events → re-emitted SSE
// must aggregate to the same semantic message (binding test class,
// DESIGN §5.3, streaming included).
func TestOpenAIStreamRoundTrip(t *testing.T) {
	for _, name := range []string{"text.txt", "tools.txt"} {
		t.Run(name, func(t *testing.T) {
			raw := string(readStreamFixture(t, name))
			events := collectEvents(t, []byte(raw))

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

			want := aggregateSSE(t, raw)
			got := aggregateSSE(t, rec.Body.String())
			if want.text != got.text {
				t.Fatalf("text\nwant %q\ngot  %q", want.text, got.text)
			}
			if want.finish != got.finish {
				t.Fatalf("finish_reason: want %q got %q", want.finish, got.finish)
			}
			if len(want.toolByID) != len(got.toolByID) {
				t.Fatalf("tool calls: want %v got %v", want.toolByID, got.toolByID)
			}
			for id, v := range want.toolByID {
				if got.toolByID[id] != v {
					t.Fatalf("tool %s: want %q got %q", id, v, got.toolByID[id])
				}
			}
			if (want.usage == nil) != (got.usage == nil) {
				t.Fatalf("usage presence: want %v got %v", want.usage, got.usage)
			}
			if want.usage != nil && (want.usage.PromptTokens != got.usage.PromptTokens ||
				want.usage.CompletionTokens != got.usage.CompletionTokens) {
				t.Fatalf("usage: want %+v got %+v", want.usage, got.usage)
			}
			if !strings.Contains(rec.Body.String(), "data: [DONE]") {
				t.Fatal("missing [DONE] sentinel")
			}
		})
	}
}
