package core

import "testing"

func TestToolReplayID(t *testing.T) {
	fresh := &Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{TextPart{Text: "hi"}}},
	}}
	if id, ok := fresh.ToolReplayID(); ok {
		t.Fatalf("no replay expected, got %q", id)
	}

	replay := &Request{Messages: []Message{
		{Role: RoleUser, Parts: []Part{TextPart{Text: "weather?"}}},
		{Role: RoleAssistant, Parts: []Part{
			TextPart{Text: "checking"},
			ToolCallPart{ID: "call_1", Name: "get_weather", Args: `{}`},
			ToolCallPart{ID: "call_2", Name: "get_time", Args: `{}`},
		}},
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolCallID: "call_1"}}},
	}}
	id, ok := replay.ToolReplayID()
	if !ok || id != "call_1" {
		t.Fatalf("want call_1, got %q (ok=%v)", id, ok)
	}

	// A tool result without a replayed assistant tool call (adjacent-turn
	// pruning by clients) is not a replay shape Gemini validates.
	resultOnly := &Request{Messages: []Message{
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolCallID: "call_9"}}},
	}}
	if id, ok := resultOnly.ToolReplayID(); ok {
		t.Fatalf("no assistant tool call replayed, got %q", id)
	}
}
