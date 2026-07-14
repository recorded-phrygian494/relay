package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/relay-llm/relay/internal/api/openai"
	"github.com/relay-llm/relay/internal/core"
	"github.com/relay-llm/relay/internal/ids"
	"github.com/relay-llm/relay/internal/provider"
	"github.com/relay-llm/relay/internal/router"
	"github.com/relay-llm/relay/internal/sse"
	"github.com/relay-llm/relay/internal/store"
	"github.com/relay-llm/relay/internal/translate"
)

const (
	maxBodyBytes            = 50 << 20 // vision payloads are large
	defaultStreamTimeout    = 5 * time.Minute
	defaultNonStreamTimeout = 2 * time.Minute
)

// handleChatCompletions serves POST /v1/chat/completions: parse → IR →
// route → walk the candidate chain → answer in the inbound dialect.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	rt := s.Runtime()
	start := time.Now()

	rec := store.Record{
		ID: ids.New("req"),
		TS: start,
		API: "openai",
	}
	defer func() {
		rec.LatencyMS = time.Since(start).Milliseconds()
		if s.store != nil {
			s.store.Log(rec)
		}
	}()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		rec.Status = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request")
		return
	}
	if len(body) > maxBodyBytes {
		rec.Status = http.StatusRequestEntityTooLarge
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request body exceeds 50 MiB", "request_too_large")
		return
	}
	sum := sha256.Sum256(body)
	rec.PromptHash = hex.EncodeToString(sum[:])
	if rt.Config.Logging.LogPrompts == "full" {
		rec.PromptBody = string(body)
	}

	wireReq, err := openai.ParseChatRequest(body)
	if err != nil {
		rec.Status = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	rec.ModelRequested = wireReq.Model
	rec.Stream = wireReq.Stream

	ir, err := translate.FromOpenAIRequest(wireReq)
	if err != nil {
		rec.Status = http.StatusBadRequest
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}

	timeout := defaultNonStreamTimeout
	if ir.Stream {
		timeout = defaultStreamTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	rec.RoutePolicy = rt.Router.Name()
	candidates, err := rt.Router.Route(ctx, ir)
	if err != nil {
		status, code := http.StatusBadGateway, "routing_error"
		if errors.Is(err, router.ErrNoRoute) {
			status, code = http.StatusNotFound, "model_not_found"
		}
		rec.Status = status
		rec.ErrorCode = code
		writeOpenAIError(w, status, err.Error(), code)
		return
	}

	if ir.Stream {
		s.serveStream(ctx, w, rt, ir, candidates, &rec)
	} else {
		s.serveOnce(ctx, w, rt, ir, candidates, &rec)
	}
}

// nextErr decides whether the chain continues past err and records it.
func nextErr(rec *store.Record, err error) (status int, code, msg string, retryNext bool) {
	var pe *provider.Error
	if errors.As(err, &pe) {
		status = pe.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		return status, pe.Code, pe.Message, pe.Retryable
	}
	return http.StatusBadGateway, "upstream_error", err.Error(), false
}

// serveOnce handles the non-streaming path, walking the candidate chain.
func (s *Server) serveOnce(ctx context.Context, w http.ResponseWriter, rt *Runtime, ir *core.Request, candidates []router.Candidate, rec *store.Record) {
	var lastStatus int
	var lastCode, lastMsg string
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
		if err != nil {
			var retryNext bool
			lastStatus, lastCode, lastMsg, retryNext = nextErr(rec, err)
			if retryNext && ctx.Err() == nil {
				continue
			}
			break
		}

		wire, err := translate.ToOpenAIResponse(resp)
		if err != nil {
			lastStatus, lastCode, lastMsg = http.StatusBadGateway, "translation_error", err.Error()
			break
		}
		rec.Status = http.StatusOK
		rec.TokensIn = resp.Usage.InputTokens
		rec.TokensOut = resp.Usage.OutputTokens
		if rt.Config.Logging.LogPrompts == "full" {
			if b, err := json.Marshal(wire); err == nil {
				rec.ResponseBody = string(b)
			}
		}
		writeJSON(w, http.StatusOK, wire)
		return
	}
	if lastStatus == 0 {
		lastStatus, lastCode, lastMsg = http.StatusBadGateway, "no_provider", "no configured provider could serve this request"
	}
	rec.Status = lastStatus
	rec.ErrorCode = lastCode
	writeOpenAIError(w, lastStatus, lastMsg, lastCode)
}

// serveStream handles the streaming path. Failover happens only while
// obtaining the stream (before any byte reaches the client); once events
// flow, an upstream error surfaces as a dialect-correct error event
// (DESIGN §6).
func (s *Server) serveStream(ctx context.Context, w http.ResponseWriter, rt *Runtime, ir *core.Request, candidates []router.Candidate, rec *store.Record) {
	var stream core.Stream
	var lastStatus int
	var lastCode, lastMsg string
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
		if err != nil {
			var retryNext bool
			lastStatus, lastCode, lastMsg, retryNext = nextErr(rec, err)
			if retryNext && ctx.Err() == nil {
				continue
			}
			break
		}
		stream = st
		break
	}
	if stream == nil {
		if lastStatus == 0 {
			lastStatus, lastCode, lastMsg = http.StatusBadGateway, "no_provider", "no configured provider could serve this request"
		}
		rec.Status = lastStatus
		rec.ErrorCode = lastCode
		writeOpenAIError(w, lastStatus, lastMsg, lastCode)
		return
	}
	defer stream.Close()

	writer := translate.NewOpenAIStreamWriter(sse.NewWriter(w), time.Now().Unix(), ir.IncludeStreamUsage)
	rec.Status = http.StatusOK // headers are committed once the first event writes
	var sawFirst bool
	start := rec.TS
	for {
		ev, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			rec.ErrorCode = "upstream_stream_error"
			_ = writer.WriteError(fmt.Sprintf("upstream stream failed: %v", err), "upstream_stream_error")
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
		if err := writer.OnEvent(ev); err != nil {
			// Client went away; nothing more to write.
			rec.ErrorCode = "client_disconnected"
			return
		}
	}
	_ = writer.Done()
}
