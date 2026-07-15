package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
)

func testRequest() *core.Request {
	maxTok := 100
	return &core.Request{
		Model:     "gemini-2.0-flash",
		MaxTokens: &maxTok,
		Inbound:   core.DialectOpenAI,
		System:    []core.SystemPart{{Text: "be brief"}},
		Messages: []core.Message{
			{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: "hi"}}},
		},
	}
}

func TestBuildRequestShapes(t *testing.T) {
	c := New("gemini", "http://unused", "k", nil)
	req := testRequest()
	req.Messages = []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: "weather?"}}},
		{Role: core.RoleAssistant, Parts: []core.Part{
			core.ToolCallPart{ID: "call_1", Name: "get_weather", Args: `{"city":"Dublin"}`},
		}},
		{Role: core.RoleTool, Parts: []core.Part{
			core.ToolResultPart{ToolCallID: "call_1", Content: []core.Part{core.TextPart{Text: "14C"}}},
		}},
	}
	req.Tools = []core.Tool{{
		Name:       "get_weather",
		Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"$schema":"x","properties":{"city":{"type":"string"}}}`),
	}}
	wire, err := c.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if wire.SystemInstruction == nil || wire.SystemInstruction.Parts[0].Text != "be brief" {
		t.Fatalf("systemInstruction: %+v", wire.SystemInstruction)
	}
	if wire.Contents[1].Role != "model" || wire.Contents[1].Parts[0].FunctionCall == nil {
		t.Fatalf("assistant turn: %+v", wire.Contents[1])
	}
	// Tool result must reference the function NAME, resolved from the call id.
	fr := wire.Contents[2].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "get_weather" {
		t.Fatalf("functionResponse: %+v", fr)
	}
	if !strings.Contains(string(fr.Response), "14C") {
		t.Fatalf("response payload: %s", fr.Response)
	}
	// Schema sanitization: additionalProperties and $schema stripped.
	params := string(wire.Tools[0].FunctionDeclarations[0].Parameters)
	if strings.Contains(params, "additionalProperties") || strings.Contains(params, "$schema") {
		t.Fatalf("unsanitized schema: %s", params)
	}
	if !strings.Contains(params, `"city"`) {
		t.Fatalf("schema over-stripped: %s", params)
	}
}

func TestCompleteTranslation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "gemini-2.0-flash:generateContent") {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("api key header missing")
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}`))
	}))
	defer srv.Close()
	c := New("gemini", srv.URL, "k", nil)
	resp, err := c.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if text := resp.Choices[0].Parts[0].(core.TextPart).Text; text != "hello" {
		t.Fatalf("text: %q", text)
	}
	if resp.Choices[0].StopReason != core.StopEndTurn {
		t.Fatalf("stop: %v", resp.Choices[0].StopReason)
	}
	if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 3 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}

func TestStreamWithFunctionCall(t *testing.T) {
	chunks := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Checking."}]},"index":0}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Dublin"}}}]},"index":0}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":12}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "alt=sse") {
			t.Errorf("missing alt=sse: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
	}))
	defer srv.Close()
	c := New("gemini", srv.URL, "k", nil)
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
		t.Fatalf("events: %+v", events)
	}
	for i := range want {
		if events[i].Kind != want[i] {
			t.Fatalf("event %d: want %v got %v", i, want[i], events[i].Kind)
		}
	}
	// Function call present forces tool_use stop even though Gemini says STOP.
	if events[5].StopReason != core.StopToolUse {
		t.Fatalf("stop: %v", events[5].StopReason)
	}
	if events[6].Usage.InputTokens != 30 {
		t.Fatalf("usage: %+v", events[6].Usage)
	}
}

func TestModelsStripPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.0-flash"},{"name":"models/gemini-2.5-pro"}]}`))
	}))
	defer srv.Close()
	c := New("gemini", srv.URL, "k", nil)
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "gemini-2.0-flash" {
		t.Fatalf("models: %+v", models)
	}
}
