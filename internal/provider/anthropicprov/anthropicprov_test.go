package anthropicprov

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
)

func schemaRequest(stream bool) *core.Request {
	return &core.Request{
		Model:   "claude-sonnet-5",
		Stream:  stream,
		Inbound: core.DialectOpenAI,
		Messages: []core.Message{
			{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: "Extract: John is 30"}}},
		},
		ResponseFormat: &core.ResponseFormat{
			Type:       "json_schema",
			SchemaName: "person",
			Schema:     json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}`),
		},
	}
}

// The forced-tool emulation must inject the synthetic tool on the way out
// and re-synthesize its input as plain text on the way back (binding
// review condition, non-streaming form).
func TestSchemaEmulationComplete(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("anthropic-version"); v == "" {
			t.Error("missing anthropic-version header")
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",
			"content":[{"type":"tool_use","id":"toolu_1","name":"emit_structured_output","input":{"name":"John","age":30}}],
			"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":15}}`))
	}))
	defer srv.Close()

	c := New("anthropic", srv.URL, "sk-ant-x", nil)
	resp, err := c.Complete(context.Background(), schemaRequest(false))
	if err != nil {
		t.Fatal(err)
	}

	// Outbound: forced tool present, tool_choice pinned to it.
	tools := gotBody["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "emit_structured_output" {
		t.Fatalf("tools sent: %v", tools)
	}
	tc := gotBody["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "emit_structured_output" {
		t.Fatalf("tool_choice sent: %v", tc)
	}

	// Inbound: tool call re-synthesized as text, stop reason not tool_use.
	parts := resp.Choices[0].Parts
	if len(parts) != 1 {
		t.Fatalf("parts: %+v", parts)
	}
	text, ok := parts[0].(core.TextPart)
	if !ok {
		t.Fatalf("want TextPart, got %T", parts[0])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil || decoded["name"] != "John" {
		t.Fatalf("re-synthesized text: %q (%v)", text.Text, err)
	}
	if resp.Choices[0].StopReason != core.StopEndTurn {
		t.Fatalf("stop reason: %v", resp.Choices[0].StopReason)
	}
}

// Streaming form of the same binding condition: the client expects content
// text deltas, not tool-input deltas, and finish must read as a normal stop.
func TestSchemaEmulationStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"usage":{"input_tokens":20,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_2","name":"emit_structured_output","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"name\":\"Jo"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"hn\",\"age\":30}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":12}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer srv.Close()

	c := New("anthropic", srv.URL, "sk-ant-x", nil)
	st, err := c.Stream(context.Background(), schemaRequest(true))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var text string
	var stop core.StopReason
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case core.EventToolCallStart, core.EventToolCallDelta, core.EventToolCallEnd:
			t.Fatalf("tool event leaked to client: %+v", ev)
		case core.EventTextDelta:
			text += ev.Text
		case core.EventMessageEnd:
			stop = ev.StopReason
		}
	}
	if text != `{"name":"John","age":30}` {
		t.Fatalf("assembled text: %q", text)
	}
	if stop != core.StopEndTurn {
		t.Fatalf("stop: %v", stop)
	}
}

func TestErrorNormalizationOverloaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	}))
	defer srv.Close()
	c := New("anthropic", srv.URL, "sk-ant-x", nil)
	_, err := c.Complete(context.Background(), schemaRequest(false))
	var pe *provider.Error
	if !errors.As(err, &pe) || !pe.Retryable || pe.Code != "overloaded_error" {
		t.Fatalf("529 must be retryable overloaded_error: %+v", err)
	}
}

func TestCountTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("path: %s", r.URL.Path)
		}
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if _, ok := body["max_tokens"]; ok {
			t.Error("count_tokens must omit max_tokens")
		}
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer srv.Close()
	c := New("anthropic", srv.URL, "sk-ant-x", nil)
	req := schemaRequest(false)
	req.ResponseFormat = nil
	n, err := c.CountTokens(context.Background(), req)
	if err != nil || n != 42 {
		t.Fatalf("count: %d, %v", n, err)
	}
}
