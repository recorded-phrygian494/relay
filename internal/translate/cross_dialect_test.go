package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/relay-llm/relay/internal/api/anthropic"
	"github.com/relay-llm/relay/internal/api/openai"
	"github.com/relay-llm/relay/internal/core"
)

func loadOpenAIRequest(t *testing.T, name string) *core.Request {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "openai", "requests", name))
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
	return ir
}

func loadAnthropicRequest(t *testing.T, name string) *core.Request {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "anthropic", "requests", name))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := anthropic.ParseMessagesRequest(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	ir, err := FromAnthropicRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	return ir
}

// OpenAI-in → Anthropic-out: tool_calls become tool_use blocks; role:tool
// messages fold into the next user turn; max_tokens is injected.
func TestOpenAIToAnthropicTools(t *testing.T) {
	ir := loadOpenAIRequest(t, "tools.json")
	out, err := ToAnthropicRequest(ir)
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens == nil || *out.MaxTokens != DefaultAnthropicMaxTokens {
		t.Fatalf("max_tokens injection: %v", out.MaxTokens)
	}
	if len(out.Messages) != 3 {
		b, _ := json.Marshal(out.Messages)
		t.Fatalf("want 3 merged messages, got %d: %s", len(out.Messages), b)
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" || len(asst.Content.Blocks) != 1 || asst.Content.Blocks[0].Type != "tool_use" {
		t.Fatalf("assistant turn: %+v", asst)
	}
	if got := string(asst.Content.Blocks[0].Input); got != `{"city":"Dublin"}` {
		t.Fatalf("tool_use input: %s", got)
	}
	// tool result + follow-up question merged into one user turn
	last := out.Messages[2]
	if last.Role != "user" || len(last.Content.Blocks) != 2 ||
		last.Content.Blocks[0].Type != "tool_result" || last.Content.Blocks[1].Type != "text" {
		b, _ := json.Marshal(last)
		t.Fatalf("merged user turn: %s", b)
	}
	if out.ToolChoice == nil || out.ToolChoice.Type != "auto" {
		t.Fatalf("tool_choice: %+v", out.ToolChoice)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "get_weather" {
		t.Fatalf("tools: %+v", out.Tools)
	}
}

// OpenAI system/developer messages hoist into the system parameter.
func TestOpenAIToAnthropicSystemHoist(t *testing.T) {
	ir := loadOpenAIRequest(t, "simple.json")
	out, err := ToAnthropicRequest(ir)
	if err != nil {
		t.Fatal(err)
	}
	if out.System == nil || out.System.Text == nil || *out.System.Text != "You are terse." {
		t.Fatalf("system: %+v", out.System)
	}
	for _, m := range out.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			t.Fatalf("system role leaked into messages: %+v", m)
		}
	}
}

// Temperature is clamped to the Anthropic range, never rescaled.
func TestOpenAIToAnthropicTemperatureClamp(t *testing.T) {
	temp := 1.8
	ir := &core.Request{
		Model:       "m",
		Temperature: &temp,
		Inbound:     core.DialectOpenAI,
		Messages:    []core.Message{{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: "hi"}}}},
	}
	out, err := ToAnthropicRequest(ir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Temperature == nil || *out.Temperature != 1.0 {
		t.Fatalf("clamp: %v", out.Temperature)
	}
}

// n > 1 is rejected, not silently degraded.
func TestOpenAIToAnthropicRejectsN(t *testing.T) {
	n := 2
	ir := &core.Request{
		Model: "m", N: &n, Inbound: core.DialectOpenAI,
		Messages: []core.Message{{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: "hi"}}}},
	}
	if _, err := ToAnthropicRequest(ir); err == nil {
		t.Fatal("want error for n=2")
	}
}

// Anthropic-in → OpenAI-out: tool_use becomes tool_calls; tool_result user
// turns become role:tool messages; system parameter becomes a leading
// system message.
func TestAnthropicToOpenAITools(t *testing.T) {
	ir := loadAnthropicRequest(t, "tools.json")
	out, err := ToOpenAIRequest(ir)
	if err != nil {
		t.Fatal(err)
	}
	// [user, assistant(text+tool_calls), tool, user] — the mixed user turn
	// fans out into a tool message plus a user message.
	if len(out.Messages) != 4 {
		b, _ := json.Marshal(out.Messages)
		t.Fatalf("want 4 messages, got %d: %s", len(out.Messages), b)
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 ||
		asst.ToolCalls[0].Function.Name != "get_weather" {
		b, _ := json.Marshal(asst)
		t.Fatalf("assistant: %s", b)
	}
	// Args preserve source formatting (RawMessage passthrough); compare parsed.
	var args map[string]string
	if err := json.Unmarshal([]byte(asst.ToolCalls[0].Function.Arguments), &args); err != nil || args["city"] != "Dublin" {
		t.Fatalf("arguments: %q (%v)", asst.ToolCalls[0].Function.Arguments, err)
	}
	toolMsg := out.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "toolu_abc123" ||
		toolMsg.Content == nil || toolMsg.Content.Text == nil || *toolMsg.Content.Text != "14C, raining" {
		b, _ := json.Marshal(toolMsg)
		t.Fatalf("tool message: %s", b)
	}
	if out.Messages[3].Role != "user" {
		t.Fatalf("trailing user turn: %+v", out.Messages[3])
	}
}

func TestAnthropicToOpenAISystemBecomesMessage(t *testing.T) {
	ir := loadAnthropicRequest(t, "simple.json")
	out, err := ToOpenAIRequest(ir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Messages[0].Role != "system" || *out.Messages[0].Content.Text != "You are terse." {
		t.Fatalf("leading system message: %+v", out.Messages[0])
	}
	if out.MaxCompletionTokens == nil || *out.MaxCompletionTokens != 256 {
		t.Fatalf("max_tokens carried: %v", out.MaxCompletionTokens)
	}
	if len(out.Stop) != 1 || out.Stop[0] != "END" {
		t.Fatalf("stop sequences: %v", out.Stop)
	}
}

// Anthropic vision blocks map to OpenAI image_url parts (base64 → data URI).
func TestAnthropicToOpenAIVision(t *testing.T) {
	ir := loadAnthropicRequest(t, "vision.json")
	out, err := ToOpenAIRequest(ir)
	if err != nil {
		t.Fatal(err)
	}
	parts := out.Messages[0].Content.Parts
	if len(parts) != 3 {
		t.Fatalf("parts: %+v", parts)
	}
	if parts[0].ImageURL == nil || parts[0].ImageURL.URL != "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==" {
		t.Fatalf("base64 image: %+v", parts[0])
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/cat.jpg" {
		t.Fatalf("url image: %+v", parts[1])
	}
}
