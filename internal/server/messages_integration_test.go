package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/config"
	"github.com/llmrelay/relay/internal/store"
)

// mockAnthropicUpstream speaks the Anthropic wire format.
func mockAnthropicUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-test","created_at":"2026-01-01T00:00:00Z"}]}`))
		case "/v1/messages":
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			if req["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				events := []string{
					`event: message_start`,
					`data: {"type":"message_start","message":{"id":"msg_m1","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":9,"output_tokens":1}}}`,
					``,
					`event: content_block_start`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					``,
					`event: content_block_delta`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`,
					``,
					`event: content_block_stop`,
					`data: {"type":"content_block_stop","index":0}`,
					``,
					`event: message_delta`,
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
					``,
					`event: message_stop`,
					`data: {"type":"message_stop"}`,
					``,
				}
				_, _ = io.WriteString(w, strings.Join(events, "\n"))
			} else {
				_, _ = w.Write([]byte(`{"id":"msg_m2","type":"message","role":"assistant","model":"claude-test",
					"content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn",
					"usage":{"input_tokens":9,"output_tokens":2}}`))
			}
		case "/v1/messages/count_tokens":
			_, _ = w.Write([]byte(`{"input_tokens":33}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func newAnthropicGateway(t *testing.T, upstreamURL string) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Providers: map[string]config.Provider{
			"claude": {Type: "anthropic", BaseURL: upstreamURL, APIKey: config.StringList{"sk-test"}},
		},
		Logging: config.Logging{LogPrompts: "off"},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := BuildRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New(rt, st, "test").Handler())
	t.Cleanup(gw.Close)
	t.Cleanup(func() { _ = st.Close() })
	return gw, st
}

// Claude-Code-shaped request through the gateway to an OpenAI-compat
// upstream: the phase-2 deliverable path (ANTHROPIC_BASE_URL → any model).
func TestAnthropicInboundToOpenAIUpstream(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, _ := newTestGateway(t, up.URL, nil)

	body := `{"model":"mock/test-model","max_tokens":100,
		"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if out.Type != "message" || out.Role != "assistant" || out.StopReason != "end_turn" {
		t.Fatalf("envelope: %s", raw)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "pong" {
		t.Fatalf("content: %s", raw)
	}
	if out.Usage.InputTokens != 5 || out.Usage.OutputTokens != 1 {
		t.Fatalf("usage: %s", raw)
	}
}

// Streaming variant: the OpenAI upstream chunk stream must come back as a
// well-formed Anthropic event envelope.
func TestAnthropicInboundStreaming(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, _ := newTestGateway(t, up.URL, nil)

	body := `{"model":"mock/test-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(gw.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	for _, want := range []string{
		"event: message_start", `"type":"message_start"`,
		"event: content_block_start",
		`"text":"str"`, `"text":"eamed"`,
		`"stop_reason":"end_turn"`,
		`"input_tokens":7`, `"output_tokens":2`,
		"event: message_stop",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}
}

// OpenAI-inbound to an Anthropic upstream: the reverse cross-dialect path.
func TestOpenAIInboundToAnthropicUpstream(t *testing.T) {
	up := mockAnthropicUpstream(t)
	defer up.Close()
	gw, _ := newAnthropicGateway(t, up.URL)

	body := `{"model":"claude/claude-test","messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || out.Choices[0].Message.Content != "pong" || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("response: %s", raw)
	}

	// Streaming: anthropic upstream events → openai chunks.
	body = `{"model":"claude/claude-test","messages":[{"role":"user","content":"ping"}],"stream":true,"stream_options":{"include_usage":true}}`
	resp2, err := http.Post(gw.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	raw2, _ := io.ReadAll(resp2.Body)
	text := string(raw2)
	for _, want := range []string{`"content":"pong"`, `"finish_reason":"stop"`, `"prompt_tokens":9`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}
}

// count_tokens: native passthrough for anthropic targets, estimate otherwise.
func TestCountTokensNativeAndEstimate(t *testing.T) {
	aup := mockAnthropicUpstream(t)
	defer aup.Close()
	gw, _ := newAnthropicGateway(t, aup.URL)

	body := `{"model":"claude/claude-test","messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(gw.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"input_tokens":33`) {
		t.Fatalf("native count: %s", raw)
	}

	oup := mockUpstream(t)
	defer oup.Close()
	gw2, _ := newTestGateway(t, oup.URL, nil)
	body = `{"model":"mock/test-model","messages":[{"role":"user","content":"a reasonably long ping message for estimation"}]}`
	resp2, err := http.Post(gw2.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.InputTokens <= 0 {
		t.Fatalf("estimate: %d", out.InputTokens)
	}
}
