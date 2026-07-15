// Package gemini implements the Google Gemini provider over the AI Studio
// v1beta REST API (generateContent / streamGenerateContent). Vertex AI uses
// the same wire format behind OAuth; it lands later as an auth variant.
package gemini

import (
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
	"github.com/relay-llm/relay/internal/sse"
)

// DefaultBaseURL is the AI Studio API root.
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Client is the Gemini Provider.
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

// wire types

type genRequest struct {
	Contents          []content       `json:"contents"`
	SystemInstruction *content        `json:"systemInstruction,omitempty"`
	Tools             []toolWrapper   `json:"tools,omitempty"`
	ToolConfig        *toolConfig     `json:"toolConfig,omitempty"`
	GenerationConfig  *genConfig      `json:"generationConfig,omitempty"`
	Extra             json.RawMessage `json:"-"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type functionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type toolWrapper struct {
	FunctionDeclarations []functionDecl `json:"functionDeclarations"`
}

type functionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type genConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	MaxOutputTokens  *int            `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	CandidateCount   *int            `json:"candidateCount,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
}

type genResponse struct {
	Candidates    []candidate    `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
	Error         *apiErrorBody  `json:"error,omitempty"`
}

type candidate struct {
	Content      content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
	Index        int     `json:"index,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
}

type apiErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// buildRequest converts an IR request to the Gemini wire format.
func (c *Client) buildRequest(req *core.Request) (*genRequest, error) {
	out := &genRequest{}

	var sysParts []part
	for _, s := range req.System {
		sysParts = append(sysParts, part{Text: s.Text})
	}

	// Tool results reference calls by NAME in Gemini, not id: build the
	// id → name map from prior assistant tool calls.
	toolName := map[string]string{}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if tc, ok := p.(core.ToolCallPart); ok {
				toolName[tc.ID] = tc.Name
			}
		}
	}

	for i, m := range req.Messages {
		switch m.Role {
		case core.RoleSystem, core.RoleDeveloper:
			for _, p := range m.Parts {
				if tp, ok := p.(core.TextPart); ok {
					sysParts = append(sysParts, part{Text: tp.Text})
				}
			}
			continue
		}
		wire, err := toGeminiContent(m, toolName)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		if len(wire.Parts) > 0 {
			out.Contents = append(out.Contents, wire)
		}
	}
	if len(sysParts) > 0 {
		out.SystemInstruction = &content{Parts: sysParts}
	}

	if len(req.Tools) > 0 {
		w := toolWrapper{}
		for _, t := range req.Tools {
			w.FunctionDeclarations = append(w.FunctionDeclarations, functionDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  sanitizeSchema(t.Parameters),
			})
		}
		out.Tools = []toolWrapper{w}
	}

	switch req.ToolChoice.Mode {
	case core.ToolChoiceUnset:
	case core.ToolChoiceAuto:
		tc := &toolConfig{}
		tc.FunctionCallingConfig.Mode = "AUTO"
		out.ToolConfig = tc
	case core.ToolChoiceNone:
		tc := &toolConfig{}
		tc.FunctionCallingConfig.Mode = "NONE"
		out.ToolConfig = tc
	case core.ToolChoiceRequired:
		tc := &toolConfig{}
		tc.FunctionCallingConfig.Mode = "ANY"
		out.ToolConfig = tc
	case core.ToolChoiceTool:
		tc := &toolConfig{}
		tc.FunctionCallingConfig.Mode = "ANY"
		tc.FunctionCallingConfig.AllowedFunctionNames = []string{req.ToolChoice.Name}
		out.ToolConfig = tc
	}

	gc := &genConfig{
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		StopSequences:  req.Stop,
		CandidateCount: req.N,
	}
	if req.MaxTokens != nil {
		gc.MaxOutputTokens = req.MaxTokens
	}
	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			gc.ResponseMimeType = "application/json"
		case "json_schema":
			// Native structured output support.
			gc.ResponseMimeType = "application/json"
			gc.ResponseSchema = sanitizeSchema(rf.Schema)
		}
	}
	out.GenerationConfig = gc
	return out, nil
}

func toGeminiContent(m core.Message, toolName map[string]string) (content, error) {
	role := "user"
	if m.Role == core.RoleAssistant {
		role = "model"
	}
	out := content{Role: role}
	for _, p := range m.Parts {
		switch p := p.(type) {
		case core.TextPart:
			out.Parts = append(out.Parts, part{Text: p.Text})
		case core.ImagePart:
			if p.Data == "" {
				return out, fmt.Errorf("gemini requires inline image data; URL-only images are not fetched (privacy default, DESIGN §5.1)")
			}
			out.Parts = append(out.Parts, part{InlineData: &inlineData{MimeType: p.MediaType, Data: p.Data}})
		case core.ToolCallPart:
			args := json.RawMessage(p.Args)
			if p.Args == "" || !json.Valid(args) {
				return out, fmt.Errorf("tool call %s has non-JSON arguments", p.Name)
			}
			out.Parts = append(out.Parts, part{FunctionCall: &functionCall{Name: p.Name, Args: args}})
		case core.ToolResultPart:
			name := toolName[p.ToolCallID]
			if name == "" {
				name = p.ToolCallID // best effort: unknown id, use it as name
			}
			out.Parts = append(out.Parts, part{FunctionResponse: &functionResponse{
				Name:     name,
				Response: toolResultObject(p),
			}})
		case core.ThinkingPart:
			// dropped cross-dialect in v1
		default:
			return out, fmt.Errorf("unsupported part type %T", p)
		}
	}
	return out, nil
}

// toolResultObject wraps a tool result as the JSON object Gemini requires:
// object results pass through, anything else nests under "result".
func toolResultObject(p core.ToolResultPart) json.RawMessage {
	var text strings.Builder
	for _, c := range p.Content {
		if tp, ok := c.(core.TextPart); ok {
			text.WriteString(tp.Text)
		}
	}
	raw := json.RawMessage(text.String())
	if json.Valid(raw) && strings.HasPrefix(strings.TrimSpace(text.String()), "{") {
		return raw
	}
	wrapped, _ := json.Marshal(map[string]string{"result": text.String()})
	return wrapped
}

// sanitizeSchema strips JSON-Schema keywords the v1beta API rejects
// ($schema, additionalProperties), recursively. Verified against live API
// errors 2026-07; revisit when recorded fixtures land.
func sanitizeSchema(schema json.RawMessage) json.RawMessage {
	if len(schema) == 0 {
		return schema
	}
	var v any
	if err := json.Unmarshal(schema, &v); err != nil {
		return schema
	}
	v = stripKeys(v, map[string]bool{"$schema": true, "additionalProperties": true})
	out, err := json.Marshal(v)
	if err != nil {
		return schema
	}
	return out
}

func stripKeys(v any, drop map[string]bool) any {
	switch v := v.(type) {
	case map[string]any:
		for k, val := range v {
			if drop[k] {
				delete(v, k)
				continue
			}
			v[k] = stripKeys(val, drop)
		}
		return v
	case []any:
		for i := range v {
			v[i] = stripKeys(v[i], drop)
		}
		return v
	default:
		return v
	}
}

var geminiFinishToCore = map[string]core.StopReason{
	"STOP":       core.StopEndTurn,
	"MAX_TOKENS": core.StopMaxTokens,
	"SAFETY":     core.StopContentFilter,
	"RECITATION": core.StopContentFilter,
}

func finishReason(fr string, sawToolCall bool) core.StopReason {
	if sawToolCall {
		return core.StopToolUse
	}
	if stop, ok := geminiFinishToCore[fr]; ok {
		return stop
	}
	return core.StopEndTurn
}

func (c *Client) post(ctx context.Context, path string, body any, stream bool) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.apiKey)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.Transport(c.name, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg, code := strings.TrimSpace(string(raw)), ""
		var wire struct {
			Error *apiErrorBody `json:"error"`
		}
		if err := json.Unmarshal(raw, &wire); err == nil && wire.Error != nil {
			msg, code = wire.Error.Message, wire.Error.Status
		}
		return nil, provider.NewError(c.name, resp.StatusCode, code, msg, raw)
	}
	return resp, nil
}

func fromGeminiParts(parts []part, toolStart int) ([]core.Part, bool) {
	var out []core.Part
	saw := false
	n := toolStart
	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			saw = true
			args := "{}"
			if len(p.FunctionCall.Args) > 0 {
				args = string(p.FunctionCall.Args)
			}
			out = append(out, core.ToolCallPart{
				ID:   fmt.Sprintf("call_%d_%s", n, ids.New("gm")),
				Name: p.FunctionCall.Name,
				Args: args,
			})
			n++
		case p.Thought:
			out = append(out, core.ThinkingPart{Text: p.Text})
		case p.Text != "":
			out = append(out, core.TextPart{Text: p.Text})
		}
	}
	return out, saw
}

// Complete implements Provider.
func (c *Client) Complete(ctx context.Context, req *core.Request) (*core.Response, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, "/models/"+req.Model+":generateContent", wire, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out genResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, provider.Transport(c.name, fmt.Errorf("decoding response: %w", err))
	}
	if len(out.Candidates) == 0 {
		return nil, provider.NewError(c.name, http.StatusOK, "no_candidates", "gemini returned no candidates (possibly safety-blocked prompt)", nil)
	}
	ir := &core.Response{ID: ids.New("chatcmpl"), Model: req.Model}
	for i, cand := range out.Candidates {
		parts, sawTool := fromGeminiParts(cand.Content.Parts, 0)
		ir.Choices = append(ir.Choices, core.Choice{
			Index:      i,
			Parts:      parts,
			StopReason: finishReason(cand.FinishReason, sawTool),
		})
	}
	if out.UsageMetadata != nil {
		ir.Usage = core.Usage{
			InputTokens:     out.UsageMetadata.PromptTokenCount,
			OutputTokens:    out.UsageMetadata.CandidatesTokenCount,
			CacheReadTokens: out.UsageMetadata.CachedContentTokenCount,
		}
	}
	return ir, nil
}

// Stream implements Provider.
func (c *Client) Stream(ctx context.Context, req *core.Request) (core.Stream, error) {
	wire, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, "/models/"+req.Model+":streamGenerateContent?alt=sse", wire, true)
	if err != nil {
		return nil, err
	}
	return &stream{
		name:  c.name,
		model: req.Model,
		body:  resp.Body,
		r:     sse.NewReader(resp.Body),
	}, nil
}

// stream adapts Gemini's SSE chunk stream to core Events. Function calls
// arrive whole, so start/delta/end triples are synthesized (same approach
// as the ollama adapter).
type stream struct {
	name    string
	model   string
	body    io.Closer
	r       *sse.Reader
	pending []core.Event
	started bool
	toolIdx int
	sawTool bool
	usage   *core.Usage
	finish  string
	done    bool
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
		wire, err := s.r.Next()
		if err == io.EOF {
			// Gemini has no [DONE] sentinel; EOF closes the message.
			s.done = true
			s.finalize()
			continue
		}
		if err != nil {
			return core.Event{}, err
		}
		var chunk genResponse
		if err := json.Unmarshal([]byte(wire.Data), &chunk); err != nil {
			return core.Event{}, fmt.Errorf("malformed gemini chunk: %w", err)
		}
		if chunk.Error != nil {
			return core.Event{}, provider.NewError(s.name, chunk.Error.Code, chunk.Error.Status, chunk.Error.Message, nil)
		}
		s.ingest(&chunk)
	}
}

func (s *stream) ingest(chunk *genResponse) {
	if !s.started {
		s.started = true
		s.pending = append(s.pending, core.Event{
			Kind:  core.EventMessageStart,
			ID:    ids.New("chatcmpl"),
			Model: s.model,
		})
	}
	for _, cand := range chunk.Candidates {
		if cand.Index != 0 {
			continue // multi-candidate streaming is not supported
		}
		for _, p := range cand.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				s.sawTool = true
				idx := s.toolIdx
				s.toolIdx++
				args := "{}"
				if len(p.FunctionCall.Args) > 0 {
					args = string(p.FunctionCall.Args)
				}
				id := fmt.Sprintf("call_%d_%s", idx, ids.New("gm"))
				s.pending = append(s.pending,
					core.Event{Kind: core.EventToolCallStart, ToolIndex: idx, ToolID: id, ToolName: p.FunctionCall.Name},
					core.Event{Kind: core.EventToolCallDelta, ToolIndex: idx, ArgsFragment: args},
					core.Event{Kind: core.EventToolCallEnd, ToolIndex: idx, ToolID: id},
				)
			case p.Thought:
				if p.Text != "" {
					s.pending = append(s.pending, core.Event{Kind: core.EventThinkingDelta, Text: p.Text})
				}
			case p.Text != "":
				s.pending = append(s.pending, core.Event{Kind: core.EventTextDelta, Text: p.Text})
			}
		}
		if cand.FinishReason != "" {
			s.finish = cand.FinishReason
		}
	}
	if chunk.UsageMetadata != nil {
		s.usage = &core.Usage{
			InputTokens:     chunk.UsageMetadata.PromptTokenCount,
			OutputTokens:    chunk.UsageMetadata.CandidatesTokenCount,
			CacheReadTokens: chunk.UsageMetadata.CachedContentTokenCount,
		}
	}
}

func (s *stream) finalize() {
	if !s.started {
		return
	}
	s.pending = append(s.pending, core.Event{
		Kind:       core.EventMessageEnd,
		StopReason: finishReason(s.finish, s.sawTool),
	})
	if s.usage != nil {
		s.pending = append(s.pending, core.Event{Kind: core.EventUsage, Usage: s.usage})
	}
}

// Models implements Provider via GET /models.
func (c *Client) Models(ctx context.Context) ([]provider.Model, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models?pageSize=200", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-goog-api-key", c.apiKey)
	resp, err := c.http.Do(httpReq)
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
			Name string `json:"name"` // "models/gemini-2.0-flash"
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, provider.Transport(c.name, fmt.Errorf("decoding models: %w", err))
	}
	models := make([]provider.Model, 0, len(wire.Models))
	for _, m := range wire.Models {
		models = append(models, provider.Model{
			ID:       strings.TrimPrefix(m.Name, "models/"),
			Provider: c.name,
			OwnedBy:  "google",
		})
	}
	return models, nil
}
