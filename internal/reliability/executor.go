package reliability

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/router"
)

// ErrNoCandidate means the chain was exhausted without any provider even
// being attempted (all unknown, breaker-open, or key-starved).
var ErrNoCandidate = errors.New("no configured provider could serve this request")

const (
	backoffBase = 250 * time.Millisecond
	backoffCap  = 8 * time.Second
	// DefaultTTFT bounds the wait for the first upstream content event
	// before failing over (DESIGN §6).
	DefaultTTFT = 30 * time.Second
)

// Executor walks candidate chains. One Executor serves the whole process;
// breakers and key pools persist across requests (and across config
// reloads when the caller carries them over).
type Executor struct {
	Lookup   func(name string) (provider.Provider, bool)
	Pools    map[string]*KeyPool // by provider name; nil entries fine
	Breakers *BreakerSet
	Retries  int           // per-candidate retry budget for retryable errors
	TTFT     time.Duration // 0 → DefaultTTFT

	// Sleep and rng are test seams.
	sleep func(context.Context, time.Duration) error
	rngMu sync.Mutex
	rng   *rand.Rand
}

// NewExecutor builds an Executor with production defaults.
func NewExecutor(lookup func(string) (provider.Provider, bool), pools map[string]*KeyPool, retries int, ttft time.Duration) *Executor {
	return &Executor{
		Lookup:   lookup,
		Pools:    pools,
		Breakers: NewBreakerSet(),
		Retries:  retries,
		TTFT:     ttft,
		sleep:    ctxSleep,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoff returns the sleep before retry attempt n (0-based), exponential
// with full jitter, honoring Retry-After when the provider sent one.
func (e *Executor) backoff(n int, pe *provider.Error) time.Duration {
	if pe != nil && pe.RetryAfter > 0 {
		return pe.RetryAfter
	}
	d := backoffBase << n
	if d > backoffCap {
		d = backoffCap
	}
	e.rngMu.Lock()
	defer e.rngMu.Unlock()
	return time.Duration(e.rng.Int63n(int64(d) + 1))
}

// OnAttempt observes each provider attempt, for request logging.
type OnAttempt func(cand router.Candidate)

// attempt runs one candidate with its retry budget. run performs a single
// try; it must return a retryable-classified error via provider.Error.
func (e *Executor) attempt(ctx context.Context, cand router.Candidate, onAttempt OnAttempt,
	run func(context.Context) error) error {

	br := e.Breakers.For(cand.Provider, cand.Model)
	if !br.Allow() {
		return &provider.Error{
			Provider: cand.Provider, Status: http.StatusServiceUnavailable,
			Code: "circuit_open", Retryable: true,
			Message: "circuit breaker open for " + cand.Provider + "/" + cand.Model,
		}
	}
	pool := e.Pools[cand.Provider]
	var last error
	for try := 0; try <= e.Retries; try++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attemptCtx := ctx
		key := ""
		if pool != nil {
			var ok bool
			key, ok = pool.Next()
			if !ok {
				return &provider.Error{
					Provider: cand.Provider, Status: http.StatusTooManyRequests,
					Code: "all_keys_cooling", Retryable: true,
					Message: "every API key for " + cand.Provider + " is rate-limit cooling",
				}
			}
			attemptCtx = provider.WithAPIKey(ctx, key)
		}
		if onAttempt != nil {
			onAttempt(cand)
		}
		err := run(attemptCtx)
		if err == nil {
			br.Record(true)
			return nil
		}
		br.Record(false)
		last = err
		var pe *provider.Error
		if !errors.As(err, &pe) || !pe.Retryable {
			return err
		}
		if pe.Status == http.StatusTooManyRequests && pool != nil {
			pool.Cooldown(key, pe.RetryAfter)
			continue // rotating keys needs no backoff
		}
		if try < e.Retries {
			if err := e.sleep(ctx, e.backoff(try, pe)); err != nil {
				return last
			}
		}
	}
	return last
}

// retryable reports whether the chain should move on after err.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}

// Complete walks the chain for a non-streaming request.
func (e *Executor) Complete(ctx context.Context, req *core.Request, candidates []router.Candidate, onAttempt OnAttempt) (*core.Response, error) {
	var last error = ErrNoCandidate
	for _, cand := range candidates {
		p, ok := e.Lookup(cand.Provider)
		if !ok {
			continue
		}
		var resp *core.Response
		err := e.attempt(ctx, cand, onAttempt, func(ctx context.Context) error {
			r, err := p.Complete(ctx, e.pinned(req, cand))
			resp = r
			return err
		})
		if err == nil {
			return resp, nil
		}
		last = err
		if !retryable(err) || ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// Stream walks the chain until a candidate delivers its first content
// event within the TTFT budget. Failover is invisible to the client: no
// inbound byte is written until content arrives (DESIGN §6).
func (e *Executor) Stream(ctx context.Context, req *core.Request, candidates []router.Candidate, onAttempt OnAttempt) (core.Stream, error) {
	ttft := e.TTFT
	if ttft <= 0 {
		ttft = DefaultTTFT
	}
	var last error = ErrNoCandidate
	for _, cand := range candidates {
		p, ok := e.Lookup(cand.Provider)
		if !ok {
			continue
		}
		var opened core.Stream
		err := e.attempt(ctx, cand, onAttempt, func(ctx context.Context) error {
			st, err := p.Stream(ctx, e.pinned(req, cand))
			if err != nil {
				return err
			}
			buffered, err := awaitContent(ctx, st, ttft, cand.Provider)
			if err != nil {
				return err
			}
			opened = buffered
			return nil
		})
		if err == nil {
			return opened, nil
		}
		last = err
		if !retryable(err) || ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// pinned copies req with the candidate's model, leaving the original
// untouched for later candidates.
func (e *Executor) pinned(req *core.Request, cand router.Candidate) *core.Request {
	attempt := *req
	attempt.Model = cand.Model
	return &attempt
}

// contentEvent reports whether ev commits us to this candidate: any event
// a client could observe as model output (or a terminal event, for
// legitimately empty responses).
func contentEvent(ev core.Event) bool {
	switch ev.Kind {
	case core.EventTextDelta, core.EventThinkingDelta, core.EventSignatureDelta,
		core.EventRedactedThinking, core.EventToolCallStart, core.EventMessageEnd:
		return true
	}
	return false
}

// awaitContent pulls events until the first content event, the TTFT budget,
// or an error. On success it returns a stream that replays the buffered
// prefix; on failure the upstream is closed and the error is retryable.
func awaitContent(ctx context.Context, st core.Stream, ttft time.Duration, providerName string) (core.Stream, error) {
	type pulled struct {
		ev  core.Event
		err error
	}
	ch := make(chan pulled)
	stop := make(chan struct{})
	go func() {
		for {
			ev, err := st.Next()
			select {
			case ch <- pulled{ev, err}:
				if err != nil {
					return
				}
				if contentEvent(ev) {
					return
				}
			case <-stop:
				return
			}
		}
	}()

	timer := time.NewTimer(ttft)
	defer timer.Stop()
	var prefix []core.Event
	for {
		select {
		case p := <-ch:
			if p.err != nil {
				_ = st.Close()
				return nil, &provider.Error{
					Provider: providerName, Status: http.StatusBadGateway,
					Code: "stream_failed_before_content", Retryable: true,
					Message: "upstream stream failed before first content event: " + p.err.Error(),
				}
			}
			prefix = append(prefix, p.ev)
			if contentEvent(p.ev) {
				return &prefixStream{prefix: prefix, rest: st}, nil
			}
		case <-timer.C:
			close(stop)
			_ = st.Close()
			return nil, &provider.Error{
				Provider: providerName, Status: http.StatusGatewayTimeout,
				Code: "ttft_timeout", Retryable: true,
				Message: "no content event within the TTFT budget",
			}
		case <-ctx.Done():
			close(stop)
			_ = st.Close()
			return nil, ctx.Err()
		}
	}
}

// prefixStream replays buffered events, then delegates.
type prefixStream struct {
	prefix []core.Event
	rest   core.Stream
}

// Next implements core.Stream.
func (s *prefixStream) Next() (core.Event, error) {
	if len(s.prefix) > 0 {
		ev := s.prefix[0]
		s.prefix = s.prefix[1:]
		return ev, nil
	}
	return s.rest.Next()
}

// Close implements core.Stream.
func (s *prefixStream) Close() error { return s.rest.Close() }
