// Package provider defines the Provider interface every upstream adapter
// implements, plus normalized error and model types. Adapters translate the
// core IR to their wire format and back; they never retry, break circuits,
// or rotate keys — that is the executor's job (DESIGN §6).
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/llmrelay/relay/internal/core"
)

// apiKeyCtx keys the per-attempt API-key override.
type apiKeyCtx struct{}

// WithAPIKey selects the API key for one attempt. The reliability
// executor's key pool sets it; adapters fall back to their configured key
// when absent (DESIGN §6: the executor picks keys, adapters stay dumb).
func WithAPIKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, apiKeyCtx{}, key)
}

// APIKey returns the key an adapter should use for this attempt.
func APIKey(ctx context.Context, configured string) string {
	if k, ok := ctx.Value(apiKeyCtx{}).(string); ok && k != "" {
		return k
	}
	return configured
}

// Provider is one configured upstream.
type Provider interface {
	Name() string
	Models(ctx context.Context) ([]Model, error)
	Complete(ctx context.Context, req *core.Request) (*core.Response, error)
	Stream(ctx context.Context, req *core.Request) (core.Stream, error)
}

// Model is one catalog entry.
type Model struct {
	ID           string
	Provider     string
	Created      int64
	OwnedBy      string
	Capabilities Capabilities
}

// MultiTurnToolsDegraded marks models that validate provider-specific state
// (e.g. thought signatures) on function-call replay, which a cross-dialect
// gateway cannot carry: multi-turn tool use may be rejected upstream.
// See DESIGN §0.7.
const MultiTurnToolsDegraded = "degraded"

// Capabilities is per-model capability metadata consulted by the router's
// eligibility filter and the executor's diagnostics. The zero value means
// "no known caveats". More fields join with the Phase 4 pricing registry;
// built now per DESIGN §0.7 condition 2.
type Capabilities struct {
	// MultiTurnTools is "" (fine) or MultiTurnToolsDegraded.
	MultiTurnTools string
}

// ModelCapabilities is an optional Provider extension. Providers that know
// per-model caveats implement it; callers consult it via type assertion.
type ModelCapabilities interface {
	Capabilities(model string) Capabilities
}

// EmbedRequest is one embeddings call in the neutral IR. Only text inputs
// cross providers; the inbound handler rejects OpenAI token-array inputs
// with an explanation rather than guessing a tokenizer.
type EmbedRequest struct {
	Model string
	Input []string
	// Dimensions truncates the output vectors when > 0 and the provider
	// supports it (OpenAI dimensions / Gemini outputDimensionality).
	Dimensions int
}

// EmbedResponse carries one vector per input, in input order.
type EmbedResponse struct {
	Vectors  [][]float32
	TokensIn int // 0 when the provider does not report usage
}

// Embedder is an optional Provider extension for /v1/embeddings.
// Providers without an embeddings API (Anthropic) simply do not implement
// it, and the inbound handler answers with an honest 404.
type Embedder interface {
	Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error)
}

// Error is a normalized upstream failure. Raw preserves the provider's
// original error body for debugging.
type Error struct {
	Provider   string
	Status     int
	Code       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Raw        json.RawMessage
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s (status %d, code %q)", e.Provider, e.Message, e.Status, e.Code)
}

// NewError builds a normalized Error, deriving Retryable from the status
// code: 429 and 5xx are retryable, other 4xx are not.
func NewError(providerName string, status int, code, message string, raw []byte) *Error {
	return &Error{
		Provider:  providerName,
		Status:    status,
		Code:      code,
		Message:   message,
		Retryable: status == 429 || status >= 500,
		Raw:       raw,
	}
}

// Transport wraps a network-level failure (connect refused, timeout, EOF),
// which is always retryable against the next candidate.
func Transport(providerName string, err error) *Error {
	return &Error{
		Provider:  providerName,
		Status:    0,
		Code:      "transport_error",
		Message:   err.Error(),
		Retryable: true,
	}
}
