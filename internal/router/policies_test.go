package router

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
)

func userReq(text string) *core.Request {
	return &core.Request{
		Model: "alias-under-test",
		Messages: []core.Message{
			{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: text}}},
		},
	}
}

func replayReq() *core.Request {
	r := userReq("weather?")
	r.Messages = append(r.Messages,
		core.Message{Role: core.RoleAssistant, Parts: []core.Part{
			core.ToolCallPart{ID: "call_1", Name: "f", Args: "{}"},
		}},
		core.Message{Role: core.RoleTool, Parts: []core.Part{
			core.ToolResultPart{ToolCallID: "call_1"},
		}},
	)
	return r
}

func targets(cands []Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Provider + "/" + c.Model
	}
	return out
}

func TestCheapestRanksByBlendedPrice(t *testing.T) {
	price := func(prov, model string) (float64, float64, bool) {
		switch prov {
		case "spendy":
			return 10, 30, true
		case "frugal":
			return 0.1, 0.4, true
		}
		return 0, 0, false // "mystery" has no pricing
	}
	c := &Cheapest{
		Alias: "cheap",
		Targets: []Target{
			{"mystery", "m"}, {"spendy", "big"}, {"frugal", "small"},
		},
		Price: price,
	}
	cands, err := c.Route(context.Background(), userReq("hello"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"frugal/small", "spendy/big", "mystery/m"}
	if got := targets(cands); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order: %v", got)
	}
	if !strings.Contains(cands[0].Reason, "$") || !strings.Contains(cands[2].Reason, "price unknown") {
		t.Fatalf("reasons: %q / %q", cands[0].Reason, cands[2].Reason)
	}

	// No pricing at all: declared order preserved.
	c.Price = nil
	cands, _ = c.Route(context.Background(), userReq("hello"))
	if got := targets(cands); got[0] != "mystery/m" {
		t.Fatalf("nil price should keep declared order: %v", got)
	}
}

func TestFastestExploresColdFirst(t *testing.T) {
	ttft := func(prov, model string) (float64, bool) {
		switch prov {
		case "slow":
			return 900, true
		case "quick":
			return 120, true
		}
		return 0, false
	}
	f := &Fastest{
		Alias:   "snappy",
		Targets: []Target{{"slow", "m"}, {"newcomer", "m"}, {"quick", "m"}},
		TTFT:    ttft,
	}
	cands, err := f.Route(context.Background(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"newcomer/m", "quick/m", "slow/m"}
	if got := targets(cands); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order: %v", got)
	}
	if !strings.Contains(cands[0].Reason, "cold") {
		t.Fatalf("cold reason: %q", cands[0].Reason)
	}
}

func TestWeightedIsStickyAndSplits(t *testing.T) {
	w := &Weighted{
		Alias:   "canary",
		Targets: []Target{{"main", "m"}, {"canary", "m"}},
		Weights: []int{90, 10},
	}
	first, err := w.Route(context.Background(), userReq("same conversation opener"))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("want chosen + fallback, got %v", targets(first))
	}
	// Same conversation, later turn (same first user message): same pick.
	again, _ := w.Route(context.Background(), userReq("same conversation opener"))
	if targets(first)[0] != targets(again)[0] {
		t.Fatalf("not sticky: %v vs %v", targets(first), targets(again))
	}
	// Across many distinct conversations, both children get traffic and the
	// split leans heavily toward the heavier child.
	counts := map[string]int{}
	for i := 0; i < 1000; i++ {
		cands, _ := w.Route(context.Background(), userReq(fmt.Sprintf("conversation %d", i)))
		counts[targets(cands)[0]]++
	}
	if counts["main/m"] < 800 || counts["canary/m"] == 0 {
		t.Fatalf("split off: %v", counts)
	}
}

func TestCompileAliasesNestingAndCycles(t *testing.T) {
	specs := map[string]AliasSpec{
		"best":  {Targets: []string{"anthropic/claude-opus-4-8", "openai/gpt-5"}},
		"smart": {Targets: []string{"best", "ollama/llama3"}},
	}
	table, err := CompileAliases(specs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := table["smart"].Route(context.Background(), userReq("hi"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"anthropic/claude-opus-4-8", "openai/gpt-5", "ollama/llama3"}
	if got := targets(cands); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("nested expansion: %v", got)
	}

	_, err = CompileAliases(map[string]AliasSpec{
		"a": {Targets: []string{"b"}},
		"b": {Targets: []string{"a"}},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}

	_, err = CompileAliases(map[string]AliasSpec{
		"a": {Policy: "cheapest", Targets: []string{"b"}},
		"b": {Targets: []string{"x/y"}},
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot reference alias") {
		t.Fatalf("want alias-ref error, got %v", err)
	}
}

type capsTable map[string]provider.Capabilities

func (ct capsTable) lookup(prov, model string) provider.Capabilities {
	return ct[prov+"/"+model]
}

func TestAliasesRouterAndEligibility(t *testing.T) {
	caps := capsTable{
		"gemini/gemini-3.1-flash-lite": {MultiTurnTools: provider.MultiTurnToolsDegraded},
	}
	table, err := CompileAliases(map[string]AliasSpec{
		"fast": {Targets: []string{"gemini/gemini-3.1-flash-lite", "openai/gpt-4o-mini"}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &Aliases{
		Table:  table,
		Inner:  &Static{},
		Filter: &Eligibility{Caps: caps.lookup},
	}

	// Plain request: chain untouched.
	req := userReq("hi")
	req.Model = "fast"
	cands, err := a.Route(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := targets(cands); len(got) != 2 || got[0] != "gemini/gemini-3.1-flash-lite" {
		t.Fatalf("plain chain: %v", got)
	}

	// Tool replay: degraded candidate dropped, survivor annotated.
	rep := replayReq()
	rep.Model = "fast"
	cands, err = a.Route(context.Background(), rep)
	if err != nil {
		t.Fatal(err)
	}
	if got := targets(cands); len(got) != 1 || got[0] != "openai/gpt-4o-mini" {
		t.Fatalf("filtered chain: %v", got)
	}
	if !strings.Contains(cands[0].Reason, "eligibility") {
		t.Fatalf("survivor reason: %q", cands[0].Reason)
	}

	// Fail-open: all candidates degraded → chain unchanged.
	allDegraded, _ := CompileAliases(map[string]AliasSpec{
		"gonly": {Targets: []string{"gemini/gemini-3.1-flash-lite", "gemini/gemini-3.1-flash-lite"}},
	}, nil, nil)
	a.Table = allDegraded
	rep.Model = "gonly"
	cands, _ = a.Route(context.Background(), rep)
	if len(cands) != 2 {
		t.Fatalf("fail-open violated: %v", targets(cands))
	}

	// Unknown model name falls through to Inner (static), which errors with
	// no route.
	miss := userReq("hi")
	miss.Model = "not-an-alias"
	if _, err := a.Route(context.Background(), miss); err == nil {
		t.Fatal("want static fallthrough error")
	}
}
