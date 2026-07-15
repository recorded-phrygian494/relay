// Package anthropicprov implements the first-party Anthropic provider over
// the Messages API, including count_tokens and the json_schema emulation:
// OpenAI structured outputs have no Anthropic equivalent, so the adapter
// forces a single synthetic tool and re-synthesizes its input as plain text
// content on the way back — in both non-streaming and streaming forms
// (binding review condition, DESIGN §5.3).
package anthropicprov

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llmrelay/relay/internal/api/anthropic"
	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/translate"
)

// DefaultBaseURL is the Anthropic API root.
const DefaultBaseURL = "https://api.anthropic.com"

// apiVersion is the anthropic-version header value. Recorded reality is the
// source of truth for behavior under this version (2026-07).
const apiVersion = "2023-06-01"

// schemaToolName is the synthetic tool used to emulate json_schema output.
const schemaToolName = "emit_structured_output"

// Client is the Anthropic Provider.
type Client struct {
	name    string
	baseURL string
	apiKey  string
	http    *http.Client
}

// New builds a Client. baseURL may be empty for the default.
func New(name, baseURL, apiKey string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: httpClient}
}

// Name implements Provider.
func (c *Client) Name() string { return c.name }

// prepare applies response_format emulation and returns the wire request
// plus whether schema re-synthesis is needed on the way back.
func (c *Client) prepare(req *core.Request) (*anthropic.MessagesRequest, bool, error) {
	emulate := false
	if rf := req.ResponseFormat; rf != nil {
		clone := *req
		switch rf.Type {
		case "", "text":
		case "json_object":
			// No native JSON mode: instruct via system prompt. Documented
			// best-effort (DESIGN §5.1).
			clone.System = append(append([]core.SystemPart(nil), req.System...),
				core.SystemPart{Text: "Respond with valid JSON only, no prose and no code fences."})
		case "json_schema":
			schema := rf.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			clone.Tools = append(append([]core.Tool(nil), req.Tools...), core.Tool{
				Name:        schemaToolName,
				Description: "Emit the answer as structured output matching the schema.",
				Parameters:  schema,
			})
			clone.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceTool, Name: schemaToolName}
			emulate = true
		default:
			return nil, false, fmt.Errorf("unsupported response_format type %q", rf.Type)
		}
		clone.ResponseFormat = nil
		req = &clone
	}
	wire, err := translate.ToAnthropicRequest(req)
	if err != nil {
		return nil, false, err
	}
	return wire, emulate, nil
}

func (c *Client) do(ctx context.Context, path string, wire any, stream bool) (*http.Response, error) {
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.Transport(c.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, c.apiError(resp)
	}
	return resp, nil
}

func (c *Client) apiError(resp *http.Response) *provider.Error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	msg, code := strings.TrimSpace(string(raw)), ""
	var wire anthropic.ErrorResponse
	if err := json.Unmarshal(raw, &wire); err == nil && wire.Error.Message != "" {
		msg, code = wire.Error.Message, wire.Error.Type
	}
	e := provider.NewError(c.name, resp.StatusCode, code, msg, raw)
	// 529 overloaded_error is Anthropic's shed-load signal; always retryable.
	if resp.StatusCode == 529 {
		e.Retryable = true
	}
	if ra := resp.Header.Get("retry-after"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

// Complete implements Provider.
func (c *Client) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	wire, emulate, err := c.prepare(req)
	if err != nil {
		return nil, err
	}
	wire.Stream = false
	resp, err := c.do(ctx, "/v1/messages", wire, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out anthropic.MessagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, provider.Transport(c.name, fmt.Errorf("decoding response: %w", err))
	}
	ir, err := translate.FromAnthropicResponse(&out)
	if err != nil {
		return nil, err
	}
	if emulate {
		unwrapSchemaResponse(ir)
	}
	return ir, nil
}

// unwrapSchemaResponse converts the forced tool call back into assistant
// text content, restoring the shape an OpenAI json_schema client expects.
func unwrapSchemaResponse(r *core.Response) {
	for ci := range r.Choices {
		choice := &r.Choices[ci]
		var kept []core.Part
		for _, p := range choice.Parts {
			if tc, ok := p.(core.ToolCallPart); ok && tc.Name == schemaToolName {
				kept = append(kept, core.TextPart{Text: tc.Args})
				continue
			}
			kept = append(kept, p)
		}
		choice.Parts = kept
		if choice.StopReason == core.StopToolUse {
			choice.StopReason = core.StopEndTurn
		}
	}
}

// Stream implements Provider.
func (c *Client) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	wire, emulate, err := c.prepare(req)
	if err != nil {
		return nil, err
	}
	wire.Stream = true
	resp, err := c.do(ctx, "/v1/messages", wire, true)
	if err != nil {
		return nil, err
	}
	st := translate.NewAnthropicStream(resp.Body)
	if emulate {
		st = &schemaUnwrapStream{inner: st}
	}
	return st, nil
}

// schemaUnwrapStream re-synthesizes the forced tool's input_json deltas as
// text deltas mid-flight: the client asked for streamed content, not a tool
// call (binding review condition — the streaming shape must match).
type schemaUnwrapStream struct {
	inner      core.Stream
	unwrapping bool
}

func (s *schemaUnwrapStream) Close() error { return s.inner.Close() }

func (s *schemaUnwrapStream) Next() (core.Event, error) {
	for {
		ev, err := s.inner.Next()
		if err != nil {
			return ev, err
		}
		switch ev.Kind {
		case core.EventToolCallStart:
			if ev.ToolName == schemaToolName {
				s.unwrapping = true
				continue // swallow: no tool_call surfaces to the client
			}
		case core.EventToolCallDelta:
			if s.unwrapping {
				return core.Event{Kind: core.EventTextDelta, Choice: ev.Choice, Text: ev.ArgsFragment}, nil
			}
		case core.EventToolCallEnd:
			if s.unwrapping {
				s.unwrapping = false
				continue
			}
		case core.EventMessageEnd:
			if ev.StopReason == core.StopToolUse {
				ev.StopReason = core.StopEndTurn
			}
			return ev, nil
		}
		return ev, nil
	}
}

// Models implements Provider via GET /v1/models.
func (c *Client) Models(ctx context.Context) ([]provider.Model, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models?limit=100", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.Transport(c.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.apiError(resp)
	}
	var wire struct {
		Data []struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, provider.Transport(c.name, fmt.Errorf("decoding models: %w", err))
	}
	models := make([]provider.Model, 0, len(wire.Data))
	for _, m := range wire.Data {
		models = append(models, provider.Model{
			ID:       m.ID,
			Provider: c.name,
			Created:  m.CreatedAt.Unix(),
			OwnedBy:  "anthropic",
		})
	}
	return models, nil
}

// CountTokens serves POST /v1/messages/count_tokens natively.
func (c *Client) CountTokens(ctx context.Context, req *core.Request) (int, error) {
	wire, _, err := c.prepare(req)
	if err != nil {
		return 0, err
	}
	wire.MaxTokens = nil // count_tokens shares the request shape minus max_tokens
	wire.Stream = false
	resp, err := c.do(ctx, "/v1/messages/count_tokens", wire, false)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out anthropic.CountTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, provider.Transport(c.name, fmt.Errorf("decoding count_tokens: %w", err))
	}
	return out.InputTokens, nil
}
