package server

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/ids"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/store"
)

// embedWire is the OpenAI-dialect POST /v1/embeddings request.
type embedWire struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	EncodingFormat string          `json:"encoding_format"`
	Dimensions     int             `json:"dimensions"`
	User           string          `json:"user"`
}

// parseEmbedInput accepts "text" or ["text", ...]. OpenAI additionally
// allows pre-tokenized int arrays; those are tied to one model's tokenizer,
// which a multi-provider gateway cannot honestly re-interpret — rejected
// with an explanation rather than mis-embedded.
func parseEmbedInput(raw json.RawMessage) ([]string, error) {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		if len(many) == 0 {
			return nil, fmt.Errorf("input must not be empty")
		}
		return many, nil
	}
	var tokens []json.RawMessage
	if err := json.Unmarshal(raw, &tokens); err == nil {
		return nil, fmt.Errorf("token-array input is not supported: token ids are specific to one model's tokenizer, which a multi-provider gateway cannot re-interpret — send text instead")
	}
	return nil, fmt.Errorf("input must be a string or an array of strings")
}

// handleEmbeddings serves POST /v1/embeddings (OpenAI dialect, DESIGN §2).
// Providers without an embeddings API yield an honest 404 naming the gap.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	rt := s.Runtime()
	start := time.Now()
	rec := store.Record{ID: ids.New("req"), TS: start, API: "openai"}
	defer func() {
		rec.LatencyMS = time.Since(start).Milliseconds()
		rec.CostUSD = rt.cost(rec.Provider, rec.ModelServed, rec.TokensIn, rec.TokensOut)
		s.observe(&rec)
		if s.store != nil {
			s.store.Log(rec)
		}
	}()

	body, ok := s.readBody(w, r, &rec, writeOpenAIError)
	if !ok {
		return
	}
	var wire embedWire
	if err := json.Unmarshal(body, &wire); err != nil {
		rec.Status, rec.RouteReason = http.StatusBadRequest, "rejected before routing: "+err.Error()
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request")
		return
	}
	rec.ModelRequested = wire.Model
	if wire.Model == "" {
		rec.Status, rec.RouteReason = http.StatusBadRequest, "rejected before routing: missing model"
		writeOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request")
		return
	}
	switch wire.EncodingFormat {
	case "", "float", "base64":
	default:
		rec.Status, rec.RouteReason = http.StatusBadRequest, "rejected before routing: bad encoding_format"
		writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("encoding_format %q is not supported (want float or base64)", wire.EncodingFormat), "invalid_request")
		return
	}
	input, err := parseEmbedInput(wire.Input)
	if err != nil {
		rec.Status, rec.RouteReason = http.StatusBadRequest, "rejected before routing: "+err.Error()
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), rt.requestTimeout(false))
	defer cancel()

	rec.RoutePolicy = rt.Router.Name()
	candidates, err := rt.Router.Route(ctx, &core.Request{Model: wire.Model})
	if err != nil {
		f := routeFailure(&rec, err)
		rec.Status, rec.ErrorCode = f.status, f.code
		writeOpenAIError(w, f.status, f.msg, f.code)
		return
	}

	// Walk the chain: skip providers without an embeddings API, fail over
	// on retryable provider errors. (The chat executor's retries/breakers
	// don't apply — embeddings are cheap, idempotent one-shots.)
	var lastErr error
	var unsupported []string
	for _, cand := range candidates {
		p, ok := rt.Providers[cand.Provider]
		if !ok {
			continue
		}
		emb, ok := p.(provider.Embedder)
		if !ok {
			unsupported = append(unsupported, cand.Provider)
			continue
		}
		rec.Attempts++
		rec.Provider, rec.ModelServed, rec.RouteReason = cand.Provider, cand.Model, cand.Reason
		resp, err := emb.Embed(ctx, &provider.EmbedRequest{Model: cand.Model, Input: input, Dimensions: wire.Dimensions})
		if err != nil {
			lastErr = err
			if pe, ok := err.(*provider.Error); ok && pe.Retryable {
				continue
			}
			f, _ := classify(err)
			rec.Status, rec.ErrorCode = f.status, f.code
			writeOpenAIError(w, f.status, f.msg, f.code)
			return
		}
		rec.Status = http.StatusOK
		rec.TokensIn = resp.TokensIn
		writeJSON(w, http.StatusOK, renderEmbeddings(resp, cand.Model, wire.EncodingFormat))
		return
	}

	switch {
	case lastErr != nil:
		f, _ := classify(lastErr)
		rec.Status, rec.ErrorCode = f.status, f.code
		writeOpenAIError(w, f.status, f.msg, f.code)
	case len(unsupported) > 0:
		msg := fmt.Sprintf("no routed provider supports embeddings for %q (tried: %s) — Anthropic has no embeddings API; route embeddings to an openai, openai-compat, gemini, or ollama model",
			wire.Model, strings.Join(unsupported, ", "))
		if rec.RouteReason != "" {
			rec.RouteReason += " → "
		}
		rec.RouteReason += "embeddings unsupported by: " + strings.Join(unsupported, ", ")
		rec.Status, rec.ErrorCode = http.StatusNotFound, "embeddings_not_supported"
		writeOpenAIError(w, http.StatusNotFound, msg, "embeddings_not_supported")
	default:
		rec.RouteReason = "no candidate attempted: empty candidate chain"
		rec.Status, rec.ErrorCode = noProviderFailure.status, noProviderFailure.code
		writeOpenAIError(w, noProviderFailure.status, noProviderFailure.msg, noProviderFailure.code)
	}
}

// renderEmbeddings shapes the OpenAI response, honoring encoding_format.
func renderEmbeddings(resp *provider.EmbedResponse, model, format string) map[string]any {
	data := make([]map[string]any, len(resp.Vectors))
	for i, vec := range resp.Vectors {
		var embedding any = vec
		if format == "base64" {
			buf := make([]byte, 4*len(vec))
			for j, f := range vec {
				binary.LittleEndian.PutUint32(buf[4*j:], math.Float32bits(f))
			}
			embedding = base64.StdEncoding.EncodeToString(buf)
		}
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": embedding}
	}
	return map[string]any{
		"object": "list",
		"data":   data,
		"model":  model,
		"usage":  map[string]int{"prompt_tokens": resp.TokensIn, "total_tokens": resp.TokensIn},
	}
}
