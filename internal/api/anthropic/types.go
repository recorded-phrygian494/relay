// Package anthropic defines the Anthropic Messages API wire format:
// request, response, and streaming event types, plus lenient parsing that
// preserves unknown fields for same-dialect passthrough.
package anthropic

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// MessagesRequest mirrors POST /v1/messages.
type MessagesRequest struct {
	Model         string                     `json:"model"`
	MaxTokens     *int                       `json:"max_tokens,omitempty"` // required by the API; pointer so count_tokens can omit it
	Messages      []Message                  `json:"messages"`
	System        *SystemPrompt              `json:"system,omitempty"`
	Tools         []Tool                     `json:"tools,omitempty"`
	ToolChoice    *ToolChoice                `json:"tool_choice,omitempty"`
	Temperature   *float64                   `json:"temperature,omitempty"`
	TopP          *float64                   `json:"top_p,omitempty"`
	StopSequences []string                   `json:"stop_sequences,omitempty"`
	Stream        bool                       `json:"stream,omitempty"`
	Metadata      map[string]string          `json:"metadata,omitempty"`
	Extra         map[string]json.RawMessage `json:"-"` // top_k, thinking, betas, ...
}

// SystemPrompt is the string-or-blocks union of the system parameter.
type SystemPrompt struct {
	Text   *string
	Blocks []ContentBlock
}

// UnmarshalJSON accepts a plain string or an array of text blocks.
func (s *SystemPrompt) UnmarshalJSON(b []byte) error {
	*s = SystemPrompt{}
	if strings.HasPrefix(strings.TrimSpace(string(b)), `"`) {
		var text string
		if err := json.Unmarshal(b, &text); err != nil {
			return err
		}
		s.Text = &text
		return nil
	}
	return json.Unmarshal(b, &s.Blocks)
}

// MarshalJSON emits the string form when Text is set, else the block array.
func (s SystemPrompt) MarshalJSON() ([]byte, error) {
	if s.Text != nil {
		return json.Marshal(*s.Text)
	}
	return json.Marshal(s.Blocks)
}

// Message is one conversation turn (role user or assistant only; tool
// results ride inside user messages).
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

// MessageContent is the string-or-blocks content union.
type MessageContent struct {
	Text   *string
	Blocks []ContentBlock
}

// UnmarshalJSON accepts a plain string or an array of typed blocks.
func (c *MessageContent) UnmarshalJSON(b []byte) error {
	*c = MessageContent{}
	t := strings.TrimSpace(string(b))
	if t == "null" {
		return nil
	}
	if strings.HasPrefix(t, `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.Text = &s
		return nil
	}
	return json.Unmarshal(b, &c.Blocks)
}

// MarshalJSON emits the string form when Text is set, else the block array.
func (c MessageContent) MarshalJSON() ([]byte, error) {
	if c.Text != nil {
		return json.Marshal(*c.Text)
	}
	if c.Blocks != nil {
		return json.Marshal(c.Blocks)
	}
	return []byte("null"), nil
}

// ContentBlock is one typed content element. Exactly the fields for the
// declared Type are meaningful; unknown block-level fields (cache_control,
// citations, ...) ride Extra.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *ImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   *MessageContent `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`

	// thinking / redacted_thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// ImageSource is the image payload union: base64 or url.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Tool is an Anthropic tool definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`

	Extra map[string]json.RawMessage `json:"-"` // cache_control, type (server tools), ...
}

// ToolChoice mirrors the tool_choice object.
type ToolChoice struct {
	Type                   string `json:"type"` // auto | any | tool | none
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// MessagesResponse mirrors a non-streaming response.
type MessagesResponse struct {
	ID           string                     `json:"id"`
	Type         string                     `json:"type"` // "message"
	Role         string                     `json:"role"` // "assistant"
	Model        string                     `json:"model"`
	Content      []ContentBlock             `json:"content"`
	StopReason   string                     `json:"stop_reason,omitempty"`
	StopSequence *string                    `json:"stop_sequence,omitempty"`
	Usage        *Usage                     `json:"usage,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

// Usage is the Anthropic token accounting block.
type Usage struct {
	InputTokens              int                        `json:"input_tokens"`
	OutputTokens             int                        `json:"output_tokens"`
	CacheCreationInputTokens int                        `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int                        `json:"cache_read_input_tokens,omitempty"`
	Extra                    map[string]json.RawMessage `json:"-"`
}

// StreamEvent is the union of all SSE event payloads.
type StreamEvent struct {
	Type string `json:"type"`

	// message_start
	Message *MessagesResponse `json:"message,omitempty"`

	// content_block_start / content_block_delta / content_block_stop
	Index        *int          `json:"index,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	Delta        *StreamDelta  `json:"delta,omitempty"`

	// message_delta
	Usage *Usage `json:"usage,omitempty"`

	// error
	Error *ErrorBody `json:"error,omitempty"`
}

// StreamDelta is the delta payload of content_block_delta and message_delta.
type StreamDelta struct {
	Type string `json:"type,omitempty"` // text_delta | input_json_delta | thinking_delta | signature_delta

	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`

	// message_delta
	StopReason   string  `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

// ErrorResponse is the Anthropic error envelope.
type ErrorResponse struct {
	Type  string    `json:"type"` // "error"
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner error object.
type ErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// CountTokensResponse mirrors POST /v1/messages/count_tokens.
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// --- unknown-field passthrough plumbing (same mechanism as api/openai) ---

func jsonKeys(v any) []string {
	t := reflect.TypeOf(v)
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			keys = append(keys, tag)
		}
	}
	return keys
}

var (
	requestKeys  = jsonKeys(MessagesRequest{})
	responseKeys = jsonKeys(MessagesResponse{})
	blockKeys    = jsonKeys(ContentBlock{})
	toolKeys     = jsonKeys(Tool{})
	usageKeys    = jsonKeys(Usage{})
)

func splitExtra(b []byte, dst any, known []string) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(b, dst); err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for _, k := range known {
		delete(m, k)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

func mergeExtra(v any, extra map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return b, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, val := range extra {
		if _, ok := m[k]; !ok {
			m[k] = val
		}
	}
	return json.Marshal(m)
}

// UnmarshalJSON decodes a request, capturing unknown fields in Extra.
func (r *MessagesRequest) UnmarshalJSON(b []byte) error {
	type alias MessagesRequest
	var a alias
	extra, err := splitExtra(b, &a, requestKeys)
	if err != nil {
		return err
	}
	a.Extra = extra
	*r = MessagesRequest(a)
	return nil
}

// MarshalJSON encodes a request, re-emitting Extra fields.
func (r MessagesRequest) MarshalJSON() ([]byte, error) {
	type alias MessagesRequest
	return mergeExtra(alias(r), r.Extra)
}

// UnmarshalJSON decodes a response, capturing unknown fields in Extra.
func (r *MessagesResponse) UnmarshalJSON(b []byte) error {
	type alias MessagesResponse
	var a alias
	extra, err := splitExtra(b, &a, responseKeys)
	if err != nil {
		return err
	}
	a.Extra = extra
	*r = MessagesResponse(a)
	return nil
}

// MarshalJSON encodes a response, re-emitting Extra fields.
func (r MessagesResponse) MarshalJSON() ([]byte, error) {
	type alias MessagesResponse
	return mergeExtra(alias(r), r.Extra)
}

// UnmarshalJSON decodes a block, capturing unknown fields in Extra.
func (c *ContentBlock) UnmarshalJSON(b []byte) error {
	type alias ContentBlock
	var a alias
	extra, err := splitExtra(b, &a, blockKeys)
	if err != nil {
		return err
	}
	a.Extra = extra
	*c = ContentBlock(a)
	return nil
}

// MarshalJSON encodes a block, re-emitting Extra fields.
func (c ContentBlock) MarshalJSON() ([]byte, error) {
	type alias ContentBlock
	return mergeExtra(alias(c), c.Extra)
}

// UnmarshalJSON decodes a tool, capturing unknown fields in Extra.
func (t *Tool) UnmarshalJSON(b []byte) error {
	type alias Tool
	var a alias
	extra, err := splitExtra(b, &a, toolKeys)
	if err != nil {
		return err
	}
	a.Extra = extra
	*t = Tool(a)
	return nil
}

// MarshalJSON encodes a tool, re-emitting Extra fields.
func (t Tool) MarshalJSON() ([]byte, error) {
	type alias Tool
	return mergeExtra(alias(t), t.Extra)
}

// UnmarshalJSON decodes usage, capturing unknown fields in Extra.
func (u *Usage) UnmarshalJSON(b []byte) error {
	type alias Usage
	var a alias
	extra, err := splitExtra(b, &a, usageKeys)
	if err != nil {
		return err
	}
	a.Extra = extra
	*u = Usage(a)
	return nil
}

// MarshalJSON encodes usage, re-emitting Extra fields.
func (u Usage) MarshalJSON() ([]byte, error) {
	type alias Usage
	return mergeExtra(alias(u), u.Extra)
}

// ParseMessagesRequest decodes and minimally validates an inbound request.
// It never panics on malformed input (fuzzed guarantee). requireMaxTokens is
// false for count_tokens, which shares the shape minus max_tokens.
func ParseMessagesRequest(body []byte, requireMaxTokens bool) (*MessagesRequest, error) {
	var req MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("missing required field: model")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("missing required field: messages")
	}
	if requireMaxTokens && req.MaxTokens == nil {
		return nil, fmt.Errorf("missing required field: max_tokens")
	}
	for i, m := range req.Messages {
		// The docs say user|assistant only, but Claude Code sends role
		// "system" inside messages on some paths and the live API tolerates
		// it (recorded reality, 2026-07-14). Accept it; the codecs hoist it
		// into the system parameter on the way out.
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" {
			return nil, fmt.Errorf("messages[%d]: role must be user or assistant, got %q", i, m.Role)
		}
		for j, blk := range m.Content.Blocks {
			if blk.Type == "" {
				return nil, fmt.Errorf("messages[%d].content[%d]: missing block type", i, j)
			}
		}
	}
	return &req, nil
}
