package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/llmrelay/relay/internal/api/anthropic"
	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/ids"
	"github.com/llmrelay/relay/internal/sse"
	"github.com/llmrelay/relay/internal/store"
	"github.com/llmrelay/relay/internal/translate"
)

// writeAnthropicError renders an error in the Anthropic envelope with the
// dialect's conventional error types.
func writeAnthropicError(w http.ResponseWriter, status int, msg, code string) {
	if code == "" || code == "invalid_request" {
		switch {
		case status == http.StatusUnauthorized:
			code = "authentication_error"
		case status == http.StatusNotFound:
			code = "not_found_error"
		case status == http.StatusTooManyRequests:
			code = "rate_limit_error"
		case status == 529:
			code = "overloaded_error"
		case status >= 500:
			code = "api_error"
		default:
			code = "invalid_request_error"
		}
	}
	writeJSON(w, status, anthropic.ErrorResponse{
		Type:  "error",
		Error: anthropic.ErrorBody{Type: code, Message: msg},
	})
}

// handleMessages serves POST /v1/messages.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	rt := s.Runtime()
	start := time.Now()
	rec := store.Record{ID: ids.New("req"), TS: start, API: "anthropic"}
	defer func() {
		rec.LatencyMS = time.Since(start).Milliseconds()
		if rec.Cached {
			zero := 0.0
			rec.CostUSD = &zero // a cache hit is known-free, never "unpriced"
		} else {
			rec.CostUSD = rt.cost(rec.Provider, rec.ModelServed, rec.TokensIn, rec.TokensOut)
		}
		s.observe(&rec)
		if s.store != nil {
			s.store.Log(rec)
		}
	}()

	body, ok := s.readBody(w, r, &rec, writeAnthropicError)
	if !ok {
		return
	}
	wireReq, err := anthropic.ParseMessagesRequest(body, true)
	if err != nil {
		rec.Status, rec.RouteReason = http.StatusBadRequest, "rejected before routing: "+err.Error()
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	rec.ModelRequested = wireReq.Model
	rec.Stream = wireReq.Stream

	ir, err := translate.FromAnthropicRequest(wireReq)
	if err != nil {
		rec.Status, rec.RouteReason = http.StatusBadRequest, "rejected before routing: "+err.Error()
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	cacheKey, cached := s.cacheLookup(rt, r, ir, &rec)
	if cached != nil {
		if !ir.Stream {
			wire, err := translate.ToAnthropicResponse(cached.Response)
			if err == nil {
				writeJSON(w, http.StatusOK, wire)
				return
			}
			rec.Cached = false // cached IR that no longer renders: serve live
		} else {
			writer := translate.NewAnthropicStreamWriter(sse.NewWriter(w))
			replayEvents(cached.Response, writer, &rec)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), rt.requestTimeout(ir.Stream))
	defer cancel()

	rec.RoutePolicy = rt.Router.Name()
	candidates, err := rt.Router.Route(ctx, ir)
	if err != nil {
		f := routeFailure(&rec, err)
		rec.Status, rec.ErrorCode = f.status, f.code
		writeAnthropicError(w, f.status, f.msg, f.code)
		return
	}

	if !ir.Stream {
		resp, f := walkComplete(ctx, rt, ir, candidates, &rec)
		if resp == nil {
			rec.Status, rec.ErrorCode = f.status, f.code
			writeAnthropicError(w, f.status, f.msg, f.code)
			return
		}
		wire, err := translate.ToAnthropicResponse(resp)
		if err != nil {
			rec.Status, rec.ErrorCode = http.StatusBadGateway, "translation_error"
			writeAnthropicError(w, http.StatusBadGateway, err.Error(), "api_error")
			return
		}
		rec.Status = http.StatusOK
		rec.TokensIn = resp.Usage.InputTokens
		rec.TokensOut = resp.Usage.OutputTokens
		rt.cacheStore(cacheKey, resp, &rec)
		if rt.Config.Logging.LogPrompts == "full" {
			if b, err := json.Marshal(wire); err == nil {
				rec.ResponseBody = string(b)
			}
		}
		writeJSON(w, http.StatusOK, wire)
		return
	}

	stream, f := walkStream(ctx, rt, ir, candidates, &rec)
	if stream == nil {
		rec.Status, rec.ErrorCode = f.status, f.code
		writeAnthropicError(w, f.status, f.msg, f.code)
		return
	}
	rec.Status = http.StatusOK
	writer := translate.NewAnthropicStreamWriter(sse.NewWriter(w))
	pumpStream(stream, writer, &rec)
}

// tokenCounter is implemented by providers with a native count_tokens
// endpoint (Anthropic).
type tokenCounter interface {
	CountTokens(ctx context.Context, req *core.Request) (int, error)
}

// handleCountTokens serves POST /v1/messages/count_tokens: native
// passthrough when the routed provider supports it, a documented
// character-based approximation otherwise.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	rt := s.Runtime()
	rec := store.Record{TS: time.Now()} // not logged; count_tokens is free metadata traffic

	body, ok := s.readBody(w, r, &rec, writeAnthropicError)
	if !ok {
		return
	}
	wireReq, err := anthropic.ParseMessagesRequest(body, false)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	ir, err := translate.FromAnthropicRequest(wireReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	candidates, err := rt.Router.Route(ctx, ir)
	if err != nil {
		f := routeFailure(&rec, err)
		writeAnthropicError(w, f.status, f.msg, f.code)
		return
	}
	for _, cand := range candidates {
		p, ok := rt.Providers[cand.Provider]
		if !ok {
			continue
		}
		if tc, ok := p.(tokenCounter); ok {
			attempt := *ir
			attempt.Model = cand.Model
			if n, err := tc.CountTokens(ctx, &attempt); err == nil {
				writeJSON(w, http.StatusOK, anthropic.CountTokensResponse{InputTokens: n})
				return
			}
		}
		break // fall through to the approximation for non-native providers
	}
	writeJSON(w, http.StatusOK, anthropic.CountTokensResponse{InputTokens: estimateTokens(ir)})
}

// estimateTokens is the fallback count: ~4 characters per token plus small
// per-message overhead. Coarse by design and documented as such; native
// counting is used whenever the provider offers it.
func estimateTokens(r *core.Request) int {
	chars := 0
	countParts := func(parts []core.Part) {
		for _, p := range parts {
			switch p := p.(type) {
			case core.TextPart:
				chars += len(p.Text)
			case core.ToolCallPart:
				chars += len(p.Name) + len(p.Args)
			case core.ToolResultPart:
				for _, c := range p.Content {
					if t, ok := c.(core.TextPart); ok {
						chars += len(t.Text)
					}
				}
			case core.ImagePart:
				chars += 6000 // flat image estimate (~1500 tokens)
			case core.ThinkingPart:
				chars += len(p.Text)
			}
		}
	}
	for _, sp := range r.System {
		chars += len(sp.Text)
	}
	for _, m := range r.Messages {
		chars += 12 // per-message wrapper overhead
		countParts(m.Parts)
	}
	for _, t := range r.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	return chars / 4
}
