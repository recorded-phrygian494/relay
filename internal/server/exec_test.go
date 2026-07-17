package server

import (
	"context"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/router"
	"github.com/llmrelay/relay/internal/store"
)

// degradedProvider is a stub whose models report degraded multi-turn tools.
type degradedProvider struct{}

func (degradedProvider) Name() string { return "gem" }
func (degradedProvider) Models(context.Context) ([]provider.Model, error) {
	return nil, nil
}
func (degradedProvider) Complete(context.Context, *core.Request) (*core.Response, error) {
	return &core.Response{}, nil
}
func (degradedProvider) Stream(context.Context, *core.Request) (core.Stream, error) {
	return nil, nil
}
func (degradedProvider) Capabilities(model string) provider.Capabilities {
	return provider.Capabilities{MultiTurnTools: provider.MultiTurnToolsDegraded}
}

func TestNoteDegradedToolReplay(t *testing.T) {
	rt := &Runtime{}
	p := degradedProvider{}
	cand := router.Candidate{Provider: "gem", Model: "gemini-3.1-flash-lite", Reason: "static"}

	replay := &core.Request{Messages: []core.Message{
		{Role: core.RoleAssistant, Parts: []core.Part{
			core.ToolCallPart{ID: "call_x", Name: "f", Args: "{}"},
		}},
	}}
	rec := &store.Record{RouteReason: cand.Reason}
	rt.noteDegradedToolReplay(p, cand, replay, rec)
	if !strings.Contains(rec.RouteReason, "multi_turn_tools=degraded") {
		t.Fatalf("route reason not annotated: %q", rec.RouteReason)
	}

	// Same conversation again: reason annotated again, but the log dedupe
	// key is already present.
	rec2 := &store.Record{RouteReason: cand.Reason}
	rt.noteDegradedToolReplay(p, cand, replay, rec2)
	if !strings.Contains(rec2.RouteReason, "multi_turn_tools=degraded") {
		t.Fatalf("second request lost its annotation: %q", rec2.RouteReason)
	}
	if _, seen := rt.degradedWarned.Load("gem/gemini-3.1-flash-lite/call_x"); !seen {
		t.Fatal("warn-once key missing")
	}

	// No replay in history: nothing to warn about.
	single := &core.Request{Messages: []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: "hi"}}},
	}}
	rec3 := &store.Record{RouteReason: cand.Reason}
	rt.noteDegradedToolReplay(p, cand, single, rec3)
	if rec3.RouteReason != cand.Reason {
		t.Fatalf("single-turn request must not be annotated: %q", rec3.RouteReason)
	}
}
