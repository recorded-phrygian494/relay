// Package openai defines the OpenAI Chat Completions wire format: request,
// response, and streaming chunk types, plus lenient parsing that preserves
// unknown fields for same-dialect passthrough.
package openai

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// ChatRequest mirrors POST /v1/chat/completions. Unknown top-level fields
// are captured in Extra and re-emitted verbatim on same-dialect hops.
type ChatRequest struct {
	Model               string                     `json:"model"`
	Messages            []ChatMessage              `json:"messages"`
	Tools               []ToolDef                  `json:"tools,omitempty"`
	ToolChoice          json.RawMessage            `json:"tool_choice,omitempty"`
	MaxTokens           *int                       `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                       `json:"max_completion_tokens,omitempty"`
	Temperature         *float64                   `json:"temperature,omitempty"`
	TopP                *float64                   `json:"top_p,omitempty"`
	N                   *int                       `json:"n,omitempty"`
	Stop                StringOrList               `json:"stop,omitempty"`
	Stream              bool                       `json:"stream,omitempty"`
	StreamOptions       *StreamOptions             `json:"stream_options,omitempty"`
	ResponseFormat      *ResponseFormat            `json:"response_format,omitempty"`
	Metadata            map[string]string          `json:"metadata,omitempty"`
	Extra               map[string]json.RawMessage `json:"-"`
}

// StreamOptions mirrors the stream_options request field.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage is one entry of the messages array.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    *MessageContent `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// MessageContent is the string-or-parts content union. Exactly one of Text
// and Parts is meaningful; a JSON null decodes to the zero value.
type MessageContent struct {
	Text  *string
	Parts []ContentPart
}

// UnmarshalJSON accepts a plain string, an array of typed parts, or null.
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
	return json.Unmarshal(b, &c.Parts)
}

// MarshalJSON emits the string form when Text is set, else the array form.
func (c MessageContent) MarshalJSON() ([]byte, error) {
	if c.Text != nil {
		return json.Marshal(*c.Text)
	}
	if c.Parts != nil {
		return json.Marshal(c.Parts)
	}
	return []byte("null"), nil
}

// ContentPart is one element of array-form message content.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is the image_url payload; URL may be an https URL or a data: URI.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// ToolDef is a tool definition (always type "function" in this dialect).
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef carries the function name and JSON-Schema parameters.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolCall is an assistant tool invocation; Index is only present in
// streaming deltas.
type ToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the tool name and raw JSON argument string.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ResponseFormat mirrors the response_format request field.
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

// JSONSchemaSpec is the json_schema variant payload.
type JSONSchemaSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// StringOrList decodes a JSON string or array of strings into a slice.
type StringOrList []string

// UnmarshalJSON accepts "x", ["x","y"], or null.
func (s *StringOrList) UnmarshalJSON(b []byte) error {
	t := strings.TrimSpace(string(b))
	if t == "null" {
		*s = nil
		return nil
	}
	if strings.HasPrefix(t, `"`) {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*s = StringOrList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*s = many
	return nil
}

// MarshalJSON emits the single-string form for one element, else the array.
func (s StringOrList) MarshalJSON() ([]byte, error) {
	if len(s) == 1 {
		return json.Marshal(s[0])
	}
	return json.Marshal([]string(s))
}

// ChatResponse mirrors a non-streaming chat completion response.
type ChatResponse struct {
	ID      string                     `json:"id"`
	Object  string                     `json:"object"`
	Created int64                      `json:"created"`
	Model   string                     `json:"model"`
	Choices []Choice                   `json:"choices"`
	Usage   *Usage                     `json:"usage,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}

// Choice is one response alternative.
type Choice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Usage is the OpenAI token accounting block. Unknown detail fields ride
// Extra so same-dialect hops keep them.
type Usage struct {
	PromptTokens     int                        `json:"prompt_tokens"`
	CompletionTokens int                        `json:"completion_tokens"`
	TotalTokens      int                        `json:"total_tokens"`
	Extra            map[string]json.RawMessage `json:"-"`
}

// ChatChunk mirrors one streaming SSE chunk.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ChunkChoice is one choice delta within a streaming chunk.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason,omitempty"`
}

// Delta is the incremental message payload of a streaming chunk.
type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ErrorResponse is the OpenAI error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner error object.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// jsonKeys returns the json tag names of v's struct fields.
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
	requestKeys  = jsonKeys(ChatRequest{})
	responseKeys = jsonKeys(ChatResponse{})
	usageKeys    = jsonKeys(Usage{})
)

// splitExtra unmarshals b into known (via dst) and unknown fields.
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

// mergeExtra marshals v then overlays extra keys that are not already set.
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
func (r *ChatRequest) UnmarshalJSON(b []byte) error {
	type alias ChatRequest
	var a alias
	extra, err := splitExtra(b, &a, requestKeys)
	if err != nil {
		return err
	}
	a.Extra = extra
	*r = ChatRequest(a)
	return nil
}

// MarshalJSON encodes a request, re-emitting Extra fields.
func (r ChatRequest) MarshalJSON() ([]byte, error) {
	type alias ChatRequest
	return mergeExtra(alias(r), r.Extra)
}

// UnmarshalJSON decodes a response, capturing unknown fields in Extra.
func (r *ChatResponse) UnmarshalJSON(b []byte) error {
	type alias ChatResponse
	var a alias
	extra, err := splitExtra(b, &a, responseKeys)
	if err != nil {
		return err
	}
	a.Extra = extra
	*r = ChatResponse(a)
	return nil
}

// MarshalJSON encodes a response, re-emitting Extra fields.
func (r ChatResponse) MarshalJSON() ([]byte, error) {
	type alias ChatResponse
	return mergeExtra(alias(r), r.Extra)
}

// UnmarshalJSON decodes usage, capturing unknown detail fields in Extra.
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

// ParseChatRequest decodes and minimally validates an inbound request body.
// It never panics on malformed input (fuzzed guarantee).
func ParseChatRequest(body []byte) (*ChatRequest, error) {
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("missing required field: model")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("missing required field: messages")
	}
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool":
		case "":
			return nil, fmt.Errorf("messages[%d]: missing role", i)
		default:
			return nil, fmt.Errorf("messages[%d]: unknown role %q", i, m.Role)
		}
	}
	return &req, nil
}
