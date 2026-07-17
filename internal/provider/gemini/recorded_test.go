package gemini

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
)

// recordedDir is the ground-truth corpus captured by tools/record. All
// dialects' recordings live under internal/translate/testdata (the tool's
// default), including outbound-only ones like Gemini; this test reaches
// over because the adapter, not translate, is the corpus's consumer.
const recordedDir = "../../translate/testdata/gemini/recorded"

// TestRecordedFixtures replays each recorded Gemini response through the
// adapter and checks the IR it produces (any new recording joins
// automatically). This is what pins the adapter to recorded reality —
// gemini-3 era shapes: functionCall.id, thoughtSignature on parts,
// per-chunk cumulative usage, empty trailing text parts.
func TestRecordedFixtures(t *testing.T) {
	scenarios, _ := filepath.Glob(filepath.Join(recordedDir, "*"))
	if len(scenarios) == 0 {
		t.Skip("no recorded gemini fixtures yet (needs GEMINI_API_KEY); run tools/record")
	}
	for _, dir := range scenarios {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			if raw, err := os.ReadFile(filepath.Join(dir, "response.json")); err == nil {
				checkRecordedComplete(t, name, raw)
			}
			if raw, err := os.ReadFile(filepath.Join(dir, "stream.txt")); err == nil {
				checkRecordedStream(t, name, raw)
			}
		})
	}
}

func replayServer(t *testing.T, body []byte, stream bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		_, _ = w.Write(body)
	}))
}

func checkRecordedComplete(t *testing.T, name string, raw []byte) {
	srv := replayServer(t, raw, false)
	defer srv.Close()
	c := New("gemini", srv.URL, "k", nil)
	resp, err := c.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Parts) == 0 {
		t.Fatalf("choices: %+v", resp.Choices)
	}
	if resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens == 0 {
		t.Fatalf("usage not extracted: %+v", resp.Usage)
	}
	switch name {
	case "simple":
		if text := resp.Choices[0].Parts[0].(core.TextPart).Text; text != "hello relay" {
			t.Fatalf("text: %q", text)
		}
		if resp.Choices[0].StopReason != core.StopEndTurn {
			t.Fatalf("stop: %v", resp.Choices[0].StopReason)
		}
	case "tools":
		tc, ok := resp.Choices[0].Parts[0].(core.ToolCallPart)
		if !ok || tc.Name != "get_weather" || !strings.Contains(tc.Args, "Dublin") {
			t.Fatalf("tool call: %+v", resp.Choices[0].Parts[0])
		}
		if tc.ID == "" {
			t.Fatal("tool call id not synthesized")
		}
		if resp.Choices[0].StopReason != core.StopToolUse {
			t.Fatalf("stop: %v", resp.Choices[0].StopReason)
		}
	}
}

func checkRecordedStream(t *testing.T, name string, raw []byte) {
	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	srv := replayServer(t, []byte(body), true)
	defer srv.Close()
	c := New("gemini", srv.URL, "k", nil)
	st, err := c.Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var text strings.Builder
	var toolName, toolArgs string
	var stop core.StopReason
	var usage *core.Usage
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case core.EventTextDelta:
			text.WriteString(ev.Text)
		case core.EventToolCallStart:
			toolName = ev.ToolName
		case core.EventToolCallDelta:
			toolArgs += ev.ArgsFragment
		case core.EventMessageEnd:
			stop = ev.StopReason
		case core.EventUsage:
			usage = ev.Usage
		}
	}
	if usage == nil || usage.InputTokens == 0 || usage.OutputTokens == 0 {
		t.Fatalf("usage not extracted: %+v", usage)
	}
	switch name {
	case "stream_text":
		if text.String() != "1\n2\n3\n4\n5" {
			t.Fatalf("text aggregate: %q", text.String())
		}
		if stop != core.StopEndTurn {
			t.Fatalf("stop: %v", stop)
		}
	case "stream_tools":
		if toolName != "get_weather" || !strings.Contains(toolArgs, "Dublin") {
			t.Fatalf("tool call: name=%q args=%q", toolName, toolArgs)
		}
		if stop != core.StopToolUse {
			t.Fatalf("stop: %v", stop)
		}
	}
}
