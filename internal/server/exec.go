package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/relay-llm/relay/internal/core"
	"github.com/relay-llm/relay/internal/provider"
	"github.com/relay-llm/relay/internal/router"
	"github.com/relay-llm/relay/internal/store"
)

// failure is a dialect-neutral error to report to the client; each inbound
// handler renders it in its own error envelope.
type failure struct {
	status int
	code   string
	msg    string
}

func classify(err error) (failure, bool) {
	var pe *provider.Error
	if errors.As(err, &pe) {
		status := pe.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		return failure{status: status, code: pe.Code, msg: pe.Message}, pe.Retryable
	}
	return failure{status: http.StatusBadGateway, code: "upstream_error", msg: err.Error()}, false
}

var noProviderFailure = failure{
	status: http.StatusBadGateway,
	code:   "no_provider",
	msg:    "no configured provider could serve this request",
}

func routeFailure(err error) failure {
	if errors.Is(err, router.ErrNoRoute) {
		return failure{status: http.StatusNotFound, code: "model_not_found", msg: err.Error()}
	}
	return failure{status: http.StatusBadGateway, code: "routing_error", msg: err.Error()}
}

// walkComplete walks the candidate chain for a non-streaming request.
func walkComplete(ctx context.Context, rt *Runtime, ir *core.Request, candidates []router.Candidate, rec *store.Record) (*core.Response, failure) {
	last := noProviderFailure
	for _, cand := range candidates {
		p, ok := rt.Providers[cand.Provider]
		if !ok {
			continue
		}
		rec.Attempts++
		rec.Provider = cand.Provider
		rec.ModelServed = cand.Model
		rec.RouteReason = cand.Reason

		attempt := *ir
		attempt.Model = cand.Model
		resp, err := p.Complete(ctx, &attempt)
		if err == nil {
			return resp, failure{}
		}
		var retry bool
		last, retry = classify(err)
		if !retry || ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// walkStream walks the chain until a stream is successfully opened. Failover
// happens only here — before any byte reaches the client (DESIGN §6).
func walkStream(ctx context.Context, rt *Runtime, ir *core.Request, candidates []router.Candidate, rec *store.Record) (core.Stream, failure) {
	last := noProviderFailure
	for _, cand := range candidates {
		p, ok := rt.Providers[cand.Provider]
		if !ok {
			continue
		}
		rec.Attempts++
		rec.Provider = cand.Provider
		rec.ModelServed = cand.Model
		rec.RouteReason = cand.Reason

		attempt := *ir
		attempt.Model = cand.Model
		st, err := p.Stream(ctx, &attempt)
		if err == nil {
			return st, failure{}
		}
		var retry bool
		last, retry = classify(err)
		if !retry || ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// eventWriter is the dialect-specific streaming renderer; both the OpenAI
// and Anthropic writers satisfy it.
type eventWriter interface {
	OnEvent(core.Event) error
	Done() error
	WriteError(msg, code string) error
}

// pumpStream drains upstream events into the inbound writer, recording
// TTFT and usage. The record's status must already be set.
func pumpStream(stream core.Stream, w eventWriter, rec *store.Record) {
	defer stream.Close()
	var sawFirst bool
	start := rec.TS
	for {
		ev, err := stream.Next()
		if err == io.EOF {
			_ = w.Done()
			return
		}
		if err != nil {
			rec.ErrorCode = "upstream_stream_error"
			_ = w.WriteError("upstream stream failed: "+err.Error(), "upstream_stream_error")
			return
		}
		if !sawFirst {
			sawFirst = true
			rec.TTFTMS = time.Since(start).Milliseconds()
		}
		if ev.Kind == core.EventUsage && ev.Usage != nil {
			rec.TokensIn = ev.Usage.InputTokens
			rec.TokensOut = ev.Usage.OutputTokens
		}
		if err := w.OnEvent(ev); err != nil {
			rec.ErrorCode = "client_disconnected"
			return
		}
	}
}

// requestTimeout picks the per-request budget.
func requestTimeout(stream bool) time.Duration {
	if stream {
		return defaultStreamTimeout
	}
	return defaultNonStreamTimeout
}
