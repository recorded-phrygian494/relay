package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/llmrelay/relay/internal/api/openai"
	"github.com/llmrelay/relay/internal/ids"
	"github.com/llmrelay/relay/internal/sse"
	"github.com/llmrelay/relay/internal/store"
	"github.com/llmrelay/relay/internal/translate"
)

const (
	maxBodyBytes            = 50 << 20 // vision payloads are large
	defaultStreamTimeout    = 5 * time.Minute
	defaultNonStreamTimeout = 2 * time.Minute
)

// readBody enforces the size cap and records the prompt hash/body.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, rec *store.Record, writeErr func(http.ResponseWriter, int, string, string)) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		rec.Status = http.StatusBadRequest
		writeErr(w, http.StatusBadRequest, "failed to read request body", "invalid_request")
		return nil, false
	}
	if len(body) > maxBodyBytes {
		rec.Status = http.StatusRequestEntityTooLarge
		writeErr(w, http.StatusRequestEntityTooLarge, "request body exceeds 50 MiB", "request_too_large")
		return nil, false
	}
	sum := sha256.Sum256(body)
	rec.PromptHash = hex.EncodeToString(sum[:])
	if s.Runtime().Config.Logging.LogPrompts == "full" {
		rec.PromptBody = string(body)
	}
	return body, true
}

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	rt := s.Runtime()
	start := time.Now()
	rec := store.Record{ID: ids.New("req"), TS: start, API: "openai"}
	defer func() {
		rec.LatencyMS = time.Since(start).Milliseconds()
		rec.CostUSD = rt.cost(rec.Provider, rec.ModelServed, rec.TokensIn, rec.TokensOut)
		if s.store != nil {
			s.store.Log(rec)
		}
	}()

	body, ok := s.readBody(w, r, &rec, writeOpenAIError)
	if !ok {
		return
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

	ctx, cancel := context.WithTimeout(r.Context(), rt.requestTimeout(ir.Stream))
	defer cancel()

	rec.RoutePolicy = rt.Router.Name()
	candidates, err := rt.Router.Route(ctx, ir)
	if err != nil {
		f := routeFailure(err)
		rec.Status, rec.ErrorCode = f.status, f.code
		writeOpenAIError(w, f.status, f.msg, f.code)
		return
	}

	if !ir.Stream {
		resp, f := walkComplete(ctx, rt, ir, candidates, &rec)
		if resp == nil {
			rec.Status, rec.ErrorCode = f.status, f.code
			writeOpenAIError(w, f.status, f.msg, f.code)
			return
		}
		wire, err := translate.ToOpenAIResponse(resp)
		if err != nil {
			rec.Status, rec.ErrorCode = http.StatusBadGateway, "translation_error"
			writeOpenAIError(w, http.StatusBadGateway, err.Error(), "translation_error")
			return
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

	stream, f := walkStream(ctx, rt, ir, candidates, &rec)
	if stream == nil {
		rec.Status, rec.ErrorCode = f.status, f.code
		writeOpenAIError(w, f.status, f.msg, f.code)
		return
	}
	rec.Status = http.StatusOK
	writer := translate.NewOpenAIStreamWriter(sse.NewWriter(w), time.Now().Unix(), ir.IncludeStreamUsage)
	pumpStream(stream, writer, &rec)
}
