// Package ollama implements the native Ollama adapter: /api/chat for
// completions (NDJSON streaming) and /api/tags for local model discovery.
// Ollama also exposes an OpenAI-compat endpoint, but the native API carries
// more (structured outputs via format, keep_alive) and /api/tags discovery
// is what makes local models first-class.
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/relay-llm/relay/internal/core"
	"github.com/relay-llm/relay/internal/ids"
	"github.com/relay-llm/relay/internal/provider"
)

// DefaultBaseURL is the standard local Ollama address.
const DefaultBaseURL = "http://localhost:11434"

// Client is a native Ollama Provider.
type Client struct {
	name    string
	baseURL string
	http    *http.Client
}

// New builds a Client. baseURL may be empty for the local default.
func New(name, baseURL string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{name: name, baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}
}

// Name implements Provider.
func (c *Client) Name() string { return c.name }

// wire types

type chatRequest struct {
	Model    string          `json:"model"`
	Messages []chatMessage   `json:"messages"`
	Tools    []toolDef       `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type chatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Images    []string   `json:"images,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type toolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type chatChunk struct {
	Model           string      `json:"model"`
	Message         chatMessage `json:"message"`
	Done            bool        `json:"done"`
	DoneReason      string      `json:"done_reason"`
	PromptEvalCount int         `json:"prompt_eval_count"`
	EvalCount       int         `json:"eval_count"`
	Error           string      `json:"error"`
}

func (c *Client) buildRequest(req *core.Request, stream bool) (*chatRequest, error) {
	out := &chatRequest{Model: req.Model, Stream: stream}

	for _, s := range req.System {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: s.Text})
	}
	for i, m := range req.Messages {
		msgs, err := toOllamaMessages(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}

	for _, t := range req.Tools {
		var td toolDef
		td.Type = "function"
		td.Function.Name = t.Name
		td.Function.Description = t.Description
		td.Function.Parameters = t.Parameters
		out.Tools = append(out.Tools, td)
	}

	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			out.Format = json.RawMessage(`"json"`)
		case "json_schema":
			// Ollama structured outputs: format takes a JSON schema directly.
			out.Format = rf.Schema
		}
	}

	opts := map[string]any{}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		opts["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		opts["num_predict"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 {
		opts["stop"] = req.Stop
	}
	if len(opts) > 0 {
		out.Options = opts
	}
	return out, nil
}

func toOllamaMessages(m core.Message) ([]chatMessage, error) {
	role := string(m.Role)
	if m.Role == core.RoleDeveloper {
		role = "system"
	}
	var out []chatMessage
	cur := chatMessage{Role: role}
	var text strings.Builder

	for _, p := range m.Parts {
		switch p := p.(type) {
		case core.TextPart:
			text.WriteString(p.Text)
		case core.ImagePart:
			if p.Data == "" {
				return nil, fmt.Errorf("ollama requires inline image data; URL-only images are not fetched (privacy default, DESIGN §5.1)")
			}
			cur.Images = append(cur.Images, p.Data)
		case core.ToolCallPart:
			var tc toolCall
			tc.Function.Name = p.Name
			args := json.RawMessage(p.Args)
			if !json.Valid(args) || p.Args == "" {
				args, _ = json.Marshal(p.Args) // preserve malformed args as a JSON string
			}
			tc.Function.Arguments = args
			cur.ToolCalls = append(cur.ToolCalls, tc)
		case core.ToolResultPart:
			resultText := ""
			for _, rc := range p.Content {
				if t, ok := rc.(core.TextPart); ok {
					resultText += t.Text
				} else {
					return nil, fmt.Errorf("tool result part type %T not representable in ollama dialect", rc)
				}
			}
			out = append(out, chatMessage{Role: "tool", Content: resultText})
		case core.ThinkingPart:
			// dropped cross-dialect in v1
		default:
			return nil, fmt.Errorf("unsupported part type %T", p)
		}
	}
	cur.Content = text.String()
	if cur.Content != "" || len(cur.ToolCalls) > 0 || len(cur.Images) > 0 {
		out = append(out, cur)
	}
	return out, nil
}

func (c *Client) post(ctx context.Context, path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, provider.Transport(c.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, provider.NewError(c.name, resp.StatusCode, "", msg, raw)
	}
	return resp, nil
}

func stopReason(ch *chatChunk, sawToolCall bool) core.StopReason {
	if sawToolCall || len(ch.Message.ToolCalls) > 0 {
		return core.StopToolUse
	}
	switch ch.DoneReason {
	case "length", "limit":
		return core.StopMaxTokens
	default:
		return core.StopEndTurn
	}
}

func toolCallParts(calls []toolCall, startIdx int) []core.Part {
	var parts []core.Part
	for i, tc := range calls {
		// Ollama does not assign tool-call ids; synthesize stable ones.
		parts = append(parts, core.ToolCallPart{
			ID:   fmt.Sprintf("call_%d_%s", startIdx+i, ids.New("ol")),
			Name: tc.Function.Name,
			Args: string(tc.Function.Arguments),
		})
	}
	return parts
}

// Complete implements Provider.
func (c *Client) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	body, err := c.buildRequest(req, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, "/api/chat", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ch chatChunk
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
		return nil, provider.Transport(c.name, fmt.Errorf("decoding response: %w", err))
	}
	if ch.Error != "" {
		return nil, provider.NewError(c.name, http.StatusOK, "", ch.Error, nil)
	}
	parts := []core.Part{}
	if ch.Message.Content != "" {
		parts = append(parts, core.TextPart{Text: ch.Message.Content})
	}
	parts = append(parts, toolCallParts(ch.Message.ToolCalls, 0)...)
	return &core.Response{
		ID:    ids.New("chatcmpl"),
		Model: ch.Model,
		Choices: []core.Choice{{
			Parts:      parts,
			StopReason: stopReason(&ch, false),
		}},
		Usage: core.Usage{InputTokens: ch.PromptEvalCount, OutputTokens: ch.EvalCount},
	}, nil
}

// Stream implements Provider.
func (c *Client) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	body, err := c.buildRequest(req, true)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, "/api/chat", body)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	return &stream{name: c.name, body: resp.Body, sc: sc}, nil
}

// stream adapts Ollama's NDJSON chat stream to core Events.
type stream struct {
	name        string
	body        io.Closer
	sc          *bufio.Scanner
	pending     []core.Event
	started     bool
	sawToolCall bool
	toolIdx     int
	done        bool
}

func (s *stream) Close() error { return s.body.Close() }

func (s *stream) Next() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			return core.Event{}, io.EOF
		}
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return core.Event{}, err
			}
			return core.Event{}, io.EOF
		}
		line := bytes.TrimSpace(s.sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ch chatChunk
		if err := json.Unmarshal(line, &ch); err != nil {
			return core.Event{}, fmt.Errorf("malformed ollama stream line: %w", err)
		}
		if ch.Error != "" {
			return core.Event{}, provider.NewError(s.name, http.StatusOK, "", ch.Error, line)
		}
		s.ingest(&ch)
	}
}

func (s *stream) ingest(ch *chatChunk) {
	if !s.started {
		s.started = true
		s.pending = append(s.pending, core.Event{
			Kind:  core.EventMessageStart,
			ID:    ids.New("chatcmpl"),
			Model: ch.Model,
		})
	}
	if ch.Message.Content != "" {
		s.pending = append(s.pending, core.Event{Kind: core.EventTextDelta, Text: ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		// Ollama emits each tool call whole, not incrementally: synthesize
		// the start/delta/end triple so downstream writers see one shape.
		s.sawToolCall = true
		idx := s.toolIdx
		s.toolIdx++
		id := fmt.Sprintf("call_%d_%s", idx, ids.New("ol"))
		s.pending = append(s.pending,
			core.Event{Kind: core.EventToolCallStart, ToolIndex: idx, ToolID: id, ToolName: tc.Function.Name},
			core.Event{Kind: core.EventToolCallDelta, ToolIndex: idx, ArgsFragment: string(tc.Function.Arguments)},
			core.Event{Kind: core.EventToolCallEnd, ToolIndex: idx, ToolID: id},
		)
	}
	if ch.Done {
		s.done = true
		s.pending = append(s.pending,
			core.Event{Kind: core.EventMessageEnd, StopReason: stopReason(ch, s.sawToolCall)},
			core.Event{Kind: core.EventUsage, Usage: &core.Usage{
				InputTokens:  ch.PromptEvalCount,
				OutputTokens: ch.EvalCount,
			}},
		)
	}
}

// Models implements Provider via GET /api/tags.
func (c *Client) Models(ctx context.Context) ([]provider.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, provider.Transport(c.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, provider.NewError(c.name, resp.StatusCode, "", strings.TrimSpace(string(raw)), raw)
	}
	var wire struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, provider.Transport(c.name, fmt.Errorf("decoding tags: %w", err))
	}
	models := make([]provider.Model, 0, len(wire.Models))
	for _, m := range wire.Models {
		models = append(models, provider.Model{ID: m.Name, Provider: c.name, OwnedBy: "ollama"})
	}
	return models, nil
}
