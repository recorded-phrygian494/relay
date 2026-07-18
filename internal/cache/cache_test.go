package cache

import (
	"testing"
	"time"

	"github.com/llmrelay/relay/internal/core"
)

func f64(v float64) *float64 { return &v }

func req(model, text string) *core.Request {
	return &core.Request{
		Model:       model,
		Temperature: f64(0),
		Messages: []core.Message{
			{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: text}}},
		},
	}
}

func TestCacheable(t *testing.T) {
	r := req("m", "hi")
	if !Cacheable(r, "") {
		t.Fatal("temperature 0 must be cacheable")
	}
	r.Temperature = f64(0.7)
	if Cacheable(r, "") {
		t.Fatal("temperature 0.7 must not be cacheable")
	}
	if !Cacheable(r, "allow") {
		t.Fatal("x-relay-cache: allow must override")
	}
	r.Temperature = nil
	if Cacheable(r, "") {
		t.Fatal("unset temperature is not deterministic")
	}
}

func TestKeyErasesDialectAndStream(t *testing.T) {
	a, b := req("m", "hi"), req("m", "hi")
	a.Stream, a.IncludeStreamUsage, a.Inbound = true, true, core.DialectOpenAI
	b.Inbound = core.DialectAnthropic
	if Key(a) != Key(b) {
		t.Fatal("stream/dialect variants must share a key")
	}
	if Key(req("m", "hi")) == Key(req("m", "bye")) {
		t.Fatal("different content must not collide")
	}
	if Key(req("m1", "hi")) == Key(req("m2", "hi")) {
		t.Fatal("different models must not collide")
	}
}

func TestExactTTLAndLRU(t *testing.T) {
	c := NewExact(time.Minute, 2)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }

	ent := &Entry{Response: &core.Response{ID: "r1"}, Provider: "p", Model: "m"}
	c.Put("a", ent)
	if got, ok := c.Get("a"); !ok || got.Response.ID != "r1" {
		t.Fatal("miss after put")
	}
	// TTL expiry.
	now = now.Add(2 * time.Minute)
	if _, ok := c.Get("a"); ok {
		t.Fatal("expired entry served")
	}
	// LRU eviction at cap 2: touch a, add c -> b evicted.
	c.Put("a", ent)
	c.Put("b", ent)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should be present")
	}
	c.Put("c", ent)
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted (LRU)")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should have survived (recently used)")
	}
}

func TestEventsSynthesis(t *testing.T) {
	resp := &core.Response{
		ID: "r1", Model: "m",
		Choices: []core.Choice{{
			Parts: []core.Part{
				core.TextPart{Text: "hello"},
				core.ToolCallPart{ID: "t1", Name: "f", Args: `{"x":1}`},
			},
			StopReason: core.StopToolUse,
		}},
		Usage: core.Usage{InputTokens: 3, OutputTokens: 5},
	}
	events := Events(resp)
	kinds := make([]core.EventKind, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	want := []core.EventKind{
		core.EventMessageStart, core.EventTextDelta,
		core.EventToolCallStart, core.EventToolCallDelta, core.EventToolCallEnd,
		core.EventUsage, core.EventMessageEnd,
	}
	if len(kinds) != len(want) {
		t.Fatalf("kinds: %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event %d: got %v want %v", i, kinds[i], want[i])
		}
	}
	if events[5].Usage.OutputTokens != 5 || events[6].StopReason != core.StopToolUse {
		t.Fatalf("payloads: %+v", events)
	}
}
