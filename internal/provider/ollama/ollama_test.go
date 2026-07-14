package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relay-llm/relay/internal/core"
)

func testRequest() *core.Request {
	temp := 0.2
	maxTok := 100
	return &core.Request{
		Model:       "llama3.2:latest",
		Temperature: &temp,
		MaxTokens:   &maxTok,
		Inbound:     core.DialectOpenAI,
		System:      []core.SystemPart{{Text: "be brief"}},
		Messages: []core.Message{
			{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: "hi"}}},
		},
	}
}

func TestCompleteTranslation(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path: %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"model":"llama3.2:latest","message":{"role":"assistant","content":"hello"},
			"done":true,"done_reason":"stop","prompt_eval_count":10,"eval_count":4}`))
	}))
	defer srv.Close()

	c := New("ollama", srv.URL, nil)
	resp, err := c.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	msgs := gotBody["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Fatalf("system message not hoisted: %v", first)
	}
	opts := gotBody["options"].(map[string]any)
	if opts["temperature"] != 0.2 || opts["num_predict"] != float64(100) {
		t.Fatalf("options: %v", opts)
	}
	if gotBody["stream"] != false {
		t.Fatalf("stream flag: %v", gotBody["stream"])
	}
	if text := resp.Choices[0].Parts[0].(core.TextPart).Text; text != "hello" {
		t.Fatalf("text: %q", text)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if resp.Choices[0].StopReason != core.StopEndTurn {
		t.Fatalf("stop: %v", resp.Choices[0].StopReason)
	}
}

func TestStreamNDJSONWithToolCall(t *testing.T) {
	lines := []string{
		`{"model":"llama3.2:latest","message":{"role":"assistant","content":"Let me check."},"done":false}`,
		`{"model":"llama3.2:latest","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"Dublin"}}}]},"done":false}`,
		`{"model":"llama3.2:latest","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":20,"eval_count":9}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n")
		}
	}))
	defer srv.Close()

	c := New("ollama", srv.URL, nil)
	st, err := c.Stream(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var events []core.Event
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	want := []core.EventKind{
		core.EventMessageStart, core.EventTextDelta,
		core.EventToolCallStart, core.EventToolCallDelta, core.EventToolCallEnd,
		core.EventMessageEnd, core.EventUsage,
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	for i := range want {
		if events[i].Kind != want[i] {
			t.Fatalf("event %d: want %v got %v", i, want[i], events[i].Kind)
		}
	}
	if events[2].ToolName != "get_weather" {
		t.Fatalf("tool name: %q", events[2].ToolName)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(events[3].ArgsFragment), &args); err != nil || args["city"] != "Dublin" {
		t.Fatalf("args: %q (%v)", events[3].ArgsFragment, err)
	}
	// Model called a tool: stop reason must be tool_use even though ollama says "stop".
	if events[5].StopReason != core.StopToolUse {
		t.Fatalf("stop: %v", events[5].StopReason)
	}
	if events[6].Usage.InputTokens != 20 || events[6].Usage.OutputTokens != 9 {
		t.Fatalf("usage: %+v", events[6].Usage)
	}
}

func TestModelsFromTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2:latest"},{"name":"qwen2.5-coder:7b"}]}`))
	}))
	defer srv.Close()
	c := New("ollama", srv.URL, nil)
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "llama3.2:latest" || models[0].Provider != "ollama" {
		t.Fatalf("models: %+v", models)
	}
}

func TestURLOnlyImageRejected(t *testing.T) {
	c := New("ollama", "http://127.0.0.1:1", nil)
	req := testRequest()
	req.Messages = []core.Message{{Role: core.RoleUser, Parts: []core.Part{
		core.ImagePart{URL: "https://example.com/x.png"},
	}}}
	_, err := c.Complete(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "privacy") {
		t.Fatalf("want privacy-default rejection, got %v", err)
	}
}
