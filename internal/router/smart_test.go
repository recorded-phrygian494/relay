package router

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/smart"
)

type fixedClassifier struct {
	d   smart.Decision
	err error
}

func (f fixedClassifier) Name() string { return "fixed" }
func (f fixedClassifier) Classify(context.Context, *core.Request) (smart.Decision, error) {
	return f.d, f.err
}

func smartReq(model, text string) *core.Request {
	return &core.Request{Model: model, Messages: []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: text}}},
	}}
}

func mustChain(t *testing.T, target string) Router {
	t.Helper()
	r, err := Chain(target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSmartRoutesByClass(t *testing.T) {
	s := &Smart{
		Easy: mustChain(t, "cheapprov/small"),
		Hard: mustChain(t, "bigprov/large"),
	}

	s.Classifier = fixedClassifier{d: smart.Decision{Class: "easy", Why: "lexical: easy=2"}}
	cands, err := s.Route(context.Background(), smartReq("auto", "hi"))
	if err != nil || len(cands) != 1 || cands[0].Provider != "cheapprov" {
		t.Fatalf("easy: %v %v", cands, err)
	}
	// The classifier's evidence must survive verbatim into the reason.
	if !strings.Contains(cands[0].Reason, "easy chain") || !strings.Contains(cands[0].Reason, "lexical: easy=2") {
		t.Fatalf("reason lost the evidence: %s", cands[0].Reason)
	}

	s.Classifier = fixedClassifier{d: smart.Decision{Class: "hard", Why: "lexical: reason=3"}}
	cands, _ = s.Route(context.Background(), smartReq("auto", "prove it"))
	if cands[0].Provider != "bigprov" {
		t.Fatalf("hard: %v", cands)
	}
}

func TestSmartClassifierErrorFailsSafeToHard(t *testing.T) {
	s := &Smart{
		Classifier: fixedClassifier{err: fmt.Errorf("embedder down")},
		Easy:       mustChain(t, "cheapprov/small"),
		Hard:       mustChain(t, "bigprov/large"),
	}
	cands, err := s.Route(context.Background(), smartReq("auto", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].Provider != "bigprov" {
		t.Fatalf("classifier failure must fail safe (hard chain): %v", cands)
	}
	if !strings.Contains(cands[0].Reason, "classifier error") || !strings.Contains(cands[0].Reason, "embedder down") {
		t.Fatalf("reason must say why: %s", cands[0].Reason)
	}
}

func TestSmartOnDecisionObserves(t *testing.T) {
	var seen *smart.Decision
	s := &Smart{
		Classifier: fixedClassifier{d: smart.Decision{Class: "easy", Vector: []float32{1, 2}}},
		Easy:       mustChain(t, "cheapprov/small"),
		Hard:       mustChain(t, "bigprov/large"),
		OnDecision: func(_ context.Context, d smart.Decision) { seen = &d },
	}
	if _, err := s.Route(context.Background(), smartReq("auto", "hi")); err != nil {
		t.Fatal(err)
	}
	if seen == nil || len(seen.Vector) != 2 {
		t.Fatalf("OnDecision did not observe the decision: %+v", seen)
	}
}

func TestStaticThenSmart(t *testing.T) {
	static := &Static{HasProvider: func(name string) bool { return name == "known" }}
	d := &StaticThenSmart{
		Static: static,
		Smart: &Smart{
			Classifier: fixedClassifier{d: smart.Decision{Class: "easy"}},
			Easy:       mustChain(t, "cheapprov/small"),
			Hard:       mustChain(t, "bigprov/large"),
		},
	}

	// Explicit provider/model resolves statically — the classifier never runs.
	cands, err := d.Route(context.Background(), smartReq("known/exact", "prove something hard"))
	if err != nil || cands[0].Provider != "known" || cands[0].Model != "exact" {
		t.Fatalf("explicit name: %v %v", cands, err)
	}

	// Unroutable names fall through to smart instead of 404.
	cands, err = d.Route(context.Background(), smartReq("auto", "hi"))
	if err != nil || cands[0].Provider != "cheapprov" {
		t.Fatalf("smart fallthrough: %v %v", cands, err)
	}
}

func TestChainResolvesAliasesAndTargets(t *testing.T) {
	table := map[string]Router{"cheap": mustChain(t, "aliasprov/x")}
	r, err := Chain("cheap", table)
	if err != nil {
		t.Fatal(err)
	}
	cands, _ := r.Route(context.Background(), smartReq("auto", "hi"))
	if cands[0].Provider != "aliasprov" {
		t.Fatalf("alias chain: %v", cands)
	}
	if _, err := Chain("not-an-alias-or-pair", table); err == nil {
		t.Fatal("bad chain target must error")
	}
}
