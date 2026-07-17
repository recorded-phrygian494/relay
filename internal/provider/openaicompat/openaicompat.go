// Package openaicompat implements the generic OpenAI-compatible provider
// adapter — the escape hatch that covers vLLM, SGLang, LM Studio, llama.cpp
// server, LocalAI, and every hosted OpenAI-compatible API. Named providers
// (Groq, Together, DeepSeek, ...) are preset profiles over this adapter.
package openaicompat

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

	"github.com/llmrelay/relay/internal/api/openai"
	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/translate"
)

// Quirks captures known deviations of specific compat servers from the
// OpenAI reference behavior.
type Quirks struct {
	// NoStreamUsage: server rejects or ignores stream_options.include_usage.
	NoStreamUsage bool
	// ModelsPath overrides the catalog endpoint (default "/models").
	ModelsPath string
}

// Config configures one openai-compat provider instance.
type Config struct {
	Name    string
	BaseURL string // e.g. https://api.groq.com/openai/v1 — no trailing slash
	APIKey  string // phase 1: single key; the phase-3 key pool wraps this
	Headers map[string]string
	Quirks  Quirks
}

// Client is an openai-compat Provider.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a Client. httpClient may be nil for a sensible default.
func New(cfg Config, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 0} // deadlines come from ctx
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Client{cfg: cfg, http: httpClient}
}

// Name implements Provider.
func (c *Client) Name() string { return c.cfg.Name }

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key := provider.APIKey(ctx, c.cfg.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// apiError normalizes a non-2xx response into a provider.Error.
func (c *Client) apiError(resp *http.Response) *provider.Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var wire openai.ErrorResponse
	msg, code := strings.TrimSpace(string(body)), ""
	if err := json.Unmarshal(body, &wire); err == nil && wire.Error.Message != "" {
		msg, code = wire.Error.Message, wire.Error.Code
	}
	e := provider.NewError(c.cfg.Name, resp.StatusCode, code, msg, body)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

func (c *Client) marshalRequest(req *core.Request, stream bool) ([]byte, error) {
	wire, err := translate.ToOpenAIRequest(req)
	if err != nil {
		return nil, err
	}
	wire.Stream = stream
	if stream && !c.cfg.Quirks.NoStreamUsage {
		// Always ask upstream for usage; the inbound writer decides whether
		// the client sees it.
		wire.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}
	if !stream {
		wire.StreamOptions = nil
	}
	return json.Marshal(wire)
}

// Complete implements Provider.
func (c *Client) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	body, err := c.marshalRequest(req, false)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.Transport(c.cfg.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.apiError(resp)
	}
	var wire openai.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, provider.Transport(c.cfg.Name, fmt.Errorf("decoding response: %w", err))
	}
	return translate.FromOpenAIResponse(&wire)
}

// Stream implements Provider.
func (c *Client) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	body, err := c.marshalRequest(req, true)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.Transport(c.cfg.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, c.apiError(resp)
	}
	return translate.NewOpenAIStream(resp.Body), nil
}

// Models implements Provider.
func (c *Client) Models(ctx context.Context) ([]provider.Model, error) {
	path := c.cfg.Quirks.ModelsPath
	if path == "" {
		path = "/models"
	}
	httpReq, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.Transport(c.cfg.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.apiError(resp)
	}
	var wire struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, provider.Transport(c.cfg.Name, fmt.Errorf("decoding models: %w", err))
	}
	models := make([]provider.Model, 0, len(wire.Data))
	for _, m := range wire.Data {
		models = append(models, provider.Model{
			ID:       m.ID,
			Provider: c.cfg.Name,
			Created:  m.Created,
			OwnedBy:  m.OwnedBy,
		})
	}
	return models, nil
}
