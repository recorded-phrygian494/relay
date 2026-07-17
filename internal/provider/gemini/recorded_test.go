package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
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

// TestMissingThoughtSignatureError pins the DESIGN §0.7 condition-1 mapping
// to the recorded validation error: Gemini's thought-signature 400 must
// surface as the typed, self-explaining gemini_missing_thought_signature.
func TestMissingThoughtSignatureError(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(recordedDir, "missing_thought_signature", "error.json"))
	if err != nil {
		t.Skip("missing_thought_signature fixture not recorded")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	c := New("gemini", srv.URL, "k", nil)
	_, err = c.Complete(context.Background(), testRequest())
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *provider.Error, got %T: %v", err, err)
	}
	if pe.Code != "gemini_missing_thought_signature" {
		t.Fatalf("code: %q", pe.Code)
	}
	if pe.Retryable {
		t.Fatal("signature validation failure must not be retryable")
	}
	for _, want := range []string{"quirks.md", "§0.7", "alias/fallback", "single-turn"} {
		if !strings.Contains(pe.Message, want) {
			t.Fatalf("message missing %q: %s", want, pe.Message)
		}
	}
	if len(pe.Raw) == 0 {
		t.Fatal("raw provider error body not preserved")
	}
}

// TestCapabilities pins the multi_turn_tools flag to the gemini-3 family
// (DESIGN §0.7 condition 2).
func TestCapabilities(t *testing.T) {
	c := New("gemini", "http://unused", "k", nil)
	if got := c.Capabilities("gemini-3.1-flash-lite").MultiTurnTools; got != provider.MultiTurnToolsDegraded {
		t.Fatalf("gemini-3.1-flash-lite: %q", got)
	}
	if got := c.Capabilities("gemini-2.0-flash").MultiTurnTools; got != "" {
		t.Fatalf("gemini-2.0-flash: %q", got)
	}
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
