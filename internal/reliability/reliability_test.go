package reliability

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/router"
)

// fakeClock drives KeyPool and Breaker deterministically.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestKeyPoolRotationAndCooldown(t *testing.T) {
	if NewKeyPool([]string{"only"}) != nil {
		t.Fatal("single key should not build a pool")
	}
	clk := &fakeClock{t: time.Unix(0, 0)}
	p := NewKeyPool([]string{"a", "b", "c"})
	p.now = clk.now

	got := []string{}
	for i := 0; i < 4; i++ {
		k, ok := p.Next()
		if !ok {
			t.Fatal("pool starved")
		}
		got = append(got, k)
	}
	if got[0] != "a" || got[1] != "b" || got[2] != "c" || got[3] != "a" {
		t.Fatalf("rotation: %v", got)
	}

	p.Cooldown("a", 0) // default 30s
	p.Cooldown("b", time.Minute)
	for i := 0; i < 3; i++ {
		if k, ok := p.Next(); !ok || k != "c" {
			t.Fatalf("want only c, got %q ok=%v", k, ok)
		}
	}
	p.Cooldown("c", time.Minute)
	if _, ok := p.Next(); ok {
		t.Fatal("all cooling: want starvation")
	}
	clk.advance(31 * time.Second) // a's default cooldown expires
	if k, ok := p.Next(); !ok || k != "a" {
		t.Fatalf("after cooldown want a, got %q ok=%v", k, ok)
	}
}

func TestBreakerTripsAndProbes(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	s := NewBreakerSet()
	s.now = clk.now
	b := s.For("p", "m")

	// Below min samples: never trips.
	for i := 0; i < breakerMinSamples-1; i++ {
		if !b.Allow() {
			t.Fatal("closed breaker refused")
		}
		b.Record(false)
	}
	if b.Open() {
		t.Fatal("tripped below min samples")
	}
	b.Record(false) // crosses min samples at 100% failure
	if b.Allow() {
		t.Fatal("open breaker allowed")
	}

	// Half-open after the hold-off: one probe only.
	clk.advance(breakerOpenFor + time.Second)
	if !b.Allow() {
		t.Fatal("probe refused")
	}
	if b.Allow() {
		t.Fatal("second concurrent probe allowed")
	}
	b.Record(false) // probe fails → open again
	if b.Allow() {
		t.Fatal("reopened breaker allowed")
	}
	clk.advance(breakerOpenFor + time.Second)
	if !b.Allow() {
		t.Fatal("second probe refused")
	}
	b.Record(true) // probe succeeds → closed
	if !b.Allow() || b.Open() {
		t.Fatal("breaker did not close after successful probe")
	}
}

// scriptedProvider returns queued errors then succeeds.
type scriptedProvider struct {
	mu       sync.Mutex
	errs     []error
	calls    int
	lastKeys []string
	stream   func() core.Stream
}

func (s *scriptedProvider) Name() string { return "scripted" }
func (s *scriptedProvider) Models(context.Context) ([]provider.Model, error) {
	return nil, nil
}

func (s *scriptedProvider) step(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastKeys = append(s.lastKeys, provider.APIKey(ctx, "default"))
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func (s *scriptedProvider) Complete(ctx context.Context, _ *core.Request) (*core.Response, error) {
	if err := s.step(ctx); err != nil {
		return nil, err
	}
	return &core.Response{Model: "m"}, nil
}

func (s *scriptedProvider) Stream(ctx context.Context, _ *core.Request) (core.Stream, error) {
	if err := s.step(ctx); err != nil {
		return nil, err
	}
	return s.stream(), nil
}

// sliceStream yields fixed events.
type sliceStream struct {
	events []core.Event
	i      int
	closed bool
	block  chan struct{} // when set, Next blocks after events run out
}

func (s *sliceStream) Next() (core.Event, error) {
	if s.i < len(s.events) {
		ev := s.events[s.i]
		s.i++
		return ev, nil
	}
	if s.block != nil {
		<-s.block
	}
	return core.Event{}, io.EOF
}

func (s *sliceStream) Close() error {
	s.closed = true
	if s.block != nil {
		select {
		case <-s.block:
		default:
			close(s.block)
		}
	}
	return nil
}

func newTestExecutor(p provider.Provider, pools map[string]*KeyPool, retries int) *Executor {
	e := NewExecutor(func(name string) (provider.Provider, bool) {
		if name == "scripted" {
			return p, true
		}
		return nil, false
	}, pools, retries, 200*time.Millisecond)
	e.sleep = func(context.Context, time.Duration) error { return nil } // no real waiting in tests
	e.rng = rand.New(rand.NewSource(1))
	return e
}

func cand() []router.Candidate {
	return []router.Candidate{{Provider: "scripted", Model: "m", Reason: "test"}}
}

func req() *core.Request { return &core.Request{Model: "m"} }

func retryableErr(status int) *provider.Error {
	return provider.NewError("scripted", status, "e", "boom", nil)
}

func TestCompleteRetriesThenSucceeds(t *testing.T) {
	p := &scriptedProvider{errs: []error{retryableErr(500), retryableErr(503)}}
	e := newTestExecutor(p, nil, 3)
	attempts := 0
	resp, err := e.Complete(context.Background(), req(), cand(), func(router.Candidate) { attempts++ })
	if err != nil || resp == nil {
		t.Fatalf("err=%v", err)
	}
	if p.calls != 3 || attempts != 3 {
		t.Fatalf("calls=%d attempts=%d", p.calls, attempts)
	}
}

func TestCompleteNonRetryableStopsImmediately(t *testing.T) {
	p := &scriptedProvider{errs: []error{retryableErr(400)}}
	e := newTestExecutor(p, nil, 3)
	_, err := e.Complete(context.Background(), req(), cand(), nil)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Status != 400 {
		t.Fatalf("want the 400 back, got %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("retried a non-retryable error: %d calls", p.calls)
	}
}

func TestRateLimitRotatesKeysWithCooldown(t *testing.T) {
	p := &scriptedProvider{errs: []error{retryableErr(429), retryableErr(429)}}
	pool := NewKeyPool([]string{"k1", "k2", "k3"})
	e := newTestExecutor(p, map[string]*KeyPool{"scripted": pool}, 3)
	resp, err := e.Complete(context.Background(), req(), cand(), nil)
	if err != nil || resp == nil {
		t.Fatalf("err=%v", err)
	}
	if len(p.lastKeys) != 3 || p.lastKeys[0] != "k1" || p.lastKeys[1] != "k2" || p.lastKeys[2] != "k3" {
		t.Fatalf("key rotation: %v", p.lastKeys)
	}
	// The two 429'd keys are cooling: only k3 remains usable.
	if k, ok := pool.Next(); !ok || k != "k3" {
		t.Fatalf("cooldown state: %q ok=%v", k, ok)
	}
}

func TestBreakerSkipsCandidate(t *testing.T) {
	p := &scriptedProvider{}
	e := newTestExecutor(p, nil, 0)
	e.Breakers.For("scripted", "m").state = stateOpen
	e.Breakers.For("scripted", "m").openedAt = time.Now()
	_, err := e.Complete(context.Background(), req(), cand(), nil)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Code != "circuit_open" {
		t.Fatalf("want circuit_open, got %v", err)
	}
	if p.calls != 0 {
		t.Fatal("open breaker still called the provider")
	}
}

func TestStreamFailsOverBeforeContent(t *testing.T) {
	// First candidate's stream dies after MessageStart but before content;
	// executor must retry and the client must never see the dead stream.
	first := true
	p := &scriptedProvider{stream: func() core.Stream {
		if first {
			first = false
			return &sliceStream{events: []core.Event{{Kind: core.EventMessageStart, ID: "dead"}}}
		}
		return &sliceStream{events: []core.Event{
			{Kind: core.EventMessageStart, ID: "live"},
			{Kind: core.EventTextDelta, Text: "hi"},
			{Kind: core.EventMessageEnd, StopReason: core.StopEndTurn},
		}}
	}}
	e := newTestExecutor(p, nil, 1)
	st, err := e.Stream(context.Background(), req(), cand(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []core.EventKind
	var id string
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == core.EventMessageStart {
			id = ev.ID
		}
		kinds = append(kinds, ev.Kind)
	}
	if id != "live" {
		t.Fatalf("client saw the dead stream: %q", id)
	}
	if len(kinds) != 3 || kinds[1] != core.EventTextDelta {
		t.Fatalf("events: %v", kinds)
	}
}

func TestStreamTTFTTimeoutFailsOver(t *testing.T) {
	block := make(chan struct{})
	first := true
	var stalled *sliceStream
	p := &scriptedProvider{stream: func() core.Stream {
		if first {
			first = false
			stalled = &sliceStream{block: block} // never yields anything
			return stalled
		}
		return &sliceStream{events: []core.Event{{Kind: core.EventTextDelta, Text: "ok"}}}
	}}
	e := newTestExecutor(p, nil, 1)
	e.TTFT = 50 * time.Millisecond
	st, err := e.Stream(context.Background(), req(), cand(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := st.Next()
	if err != nil || ev.Text != "ok" {
		t.Fatalf("ev=%+v err=%v", ev, err)
	}
	if !stalled.closed {
		t.Fatal("stalled upstream not closed")
	}
}

func TestChainFallsThroughCandidates(t *testing.T) {
	bad := &scriptedProvider{errs: []error{retryableErr(500)}}
	good := &scriptedProvider{}
	e := NewExecutor(func(name string) (provider.Provider, bool) {
		switch name {
		case "bad":
			return bad, true
		case "good":
			return good, true
		}
		return nil, false
	}, nil, 0, time.Second)
	e.sleep = func(context.Context, time.Duration) error { return nil }
	chain := []router.Candidate{
		{Provider: "missing", Model: "m"},
		{Provider: "bad", Model: "m"},
		{Provider: "good", Model: "m"},
	}
	resp, err := e.Complete(context.Background(), req(), chain, nil)
	if err != nil || resp == nil {
		t.Fatalf("err=%v", err)
	}
	if bad.calls != 1 || good.calls != 1 {
		t.Fatalf("bad=%d good=%d", bad.calls, good.calls)
	}

	// Empty/unknown-only chain: ErrNoCandidate.
	_, err = e.Complete(context.Background(), req(), []router.Candidate{{Provider: "missing", Model: "m"}}, nil)
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("want ErrNoCandidate, got %v", err)
	}
}

func TestBackoffHonorsRetryAfter(t *testing.T) {
	e := newTestExecutor(&scriptedProvider{}, nil, 0)
	pe := retryableErr(429)
	pe.RetryAfter = 7 * time.Second
	if d := e.backoff(0, pe); d != 7*time.Second {
		t.Fatalf("retry-after ignored: %v", d)
	}
	for i := 0; i < 20; i++ {
		if d := e.backoff(10, nil); d > backoffCap {
			t.Fatalf("uncapped backoff: %v", d)
		}
	}
}
