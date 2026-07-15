// Package core defines the canonical intermediate representation (IR) that
// every inbound API dialect and outbound provider adapter translates to and
// from. Nothing in this package depends on any wire format; translation
// correctness lives in the translate package.
package core

import "encoding/json"

// Dialect identifies an API wire format.
type Dialect string

const (
	DialectOpenAI    Dialect = "openai"
	DialectAnthropic Dialect = "anthropic"
)

// Role is a message author role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer" // OpenAI developer role; treated as system cross-dialect
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// StopReason is the normalized reason a completion ended.
type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopToolUse       StopReason = "tool_use"
	StopSequence      StopReason = "stop_sequence"
	StopContentFilter StopReason = "content_filter"
)

// Ext carries dialect-specific fields the IR does not model. Values are
// applied on the way out only when the outbound dialect matches the key;
// cross-dialect they are dropped or rejected per the strictness policy.
type Ext map[Dialect]map[string]json.RawMessage

// For returns the passthrough fields recorded for dialect d, or nil.
func (e Ext) For(d Dialect) map[string]json.RawMessage {
	if e == nil {
		return nil
	}
	return e[d]
}

// Set records a passthrough field for dialect d, allocating as needed.
func (e *Ext) Set(d Dialect, key string, v json.RawMessage) {
	if *e == nil {
		*e = Ext{}
	}
	if (*e)[d] == nil {
		(*e)[d] = map[string]json.RawMessage{}
	}
	(*e)[d][key] = v
}

// Request is the canonical, dialect-neutral form of a completion request.
//
// Invariant: OpenAI-inbound requests keep system/developer messages in
// Messages (position preserved); Anthropic-inbound requests carry the
// top-level system parameter in System and never place system messages in
// Messages. Outbound codecs reconcile the two shapes.
type Request struct {
	Model              string
	System             []SystemPart
	Messages           []Message
	Tools              []Tool
	ToolChoice         ToolChoice
	MaxTokens          *int
	LegacyMaxTokens    bool // OpenAI inbound used max_tokens rather than max_completion_tokens
	Temperature        *float64
	TopP               *float64
	N                  *int
	Stop               []string
	Stream             bool
	IncludeStreamUsage bool // OpenAI inbound stream_options.include_usage
	ResponseFormat     *ResponseFormat
	Metadata           map[string]string
	Ext                Ext
	Inbound            Dialect
}

// BlockExt carries block-level dialect-specific fields (e.g. Anthropic
// cache_control) for same-dialect passthrough. Dropping these silently on a
// passthrough hop would break prompt caching, so they ride the IR.
type BlockExt map[string]json.RawMessage

// SystemPart is one block of the top-level system prompt.
type SystemPart struct {
	Text string
	Ext  BlockExt
}

// Message is one conversation turn.
type Message struct {
	Role  Role
	Name  string // OpenAI per-message name; same-dialect passthrough
	Parts []Part
}

// Part is one content block. It is a closed union: TextPart, ImagePart,
// ToolCallPart, ToolResultPart, ThinkingPart.
type Part interface{ part() }

// TextPart is plain text content.
type TextPart struct {
	Text string
	Ext  BlockExt
}

// ImagePart is an image input, either by URL or inline base64 data.
type ImagePart struct {
	URL       string // remote https URL, when provided that way
	MediaType string // e.g. image/png, required when Data is set
	Data      string // base64 payload, when inline
	Detail    string // OpenAI detail hint; same-dialect passthrough
	Ext       BlockExt
}

// ToolCallPart is an assistant request to invoke a tool. Args is the raw
// JSON argument string exactly as produced by the model.
type ToolCallPart struct {
	ID   string
	Name string
	Args string
	Ext  BlockExt
}

// ToolResultPart is the caller-supplied result of a prior tool call.
type ToolResultPart struct {
	ToolCallID string
	Content    []Part
	IsError    bool
	Ext        BlockExt
}

// ThinkingPart is a model reasoning block. Passthrough-only in v1. Redacted
// holds the opaque payload of a redacted_thinking block, in which case Text
// and Signature are empty.
type ThinkingPart struct {
	Text      string
	Signature string
	Redacted  string
	Ext       BlockExt
}

func (TextPart) part()       {}
func (ImagePart) part()      {}
func (ToolCallPart) part()   {}
func (ToolResultPart) part() {}
func (ThinkingPart) part()   {}

// Tool is a normalized tool definition with a JSON-Schema parameter spec.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Strict      *bool // OpenAI strict mode; dropped cross-dialect
	Ext         BlockExt
}

// ToolChoiceMode says how the model may use tools.
type ToolChoiceMode string

const (
	ToolChoiceUnset    ToolChoiceMode = ""
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceTool     ToolChoiceMode = "tool" // force the named tool
)

// ToolChoice constrains tool use for a request.
type ToolChoice struct {
	Mode            ToolChoiceMode
	Name            string // set when Mode == ToolChoiceTool
	DisableParallel *bool  // Anthropic disable_parallel_tool_use passthrough
}

// ResponseFormat requests plain text, JSON mode, or schema-constrained output.
type ResponseFormat struct {
	Type        string // "text" | "json_object" | "json_schema"
	SchemaName  string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

// Usage is normalized token accounting. Ext carries dialect-specific detail
// blocks (e.g. OpenAI prompt_tokens_details) for same-dialect passthrough.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Ext              Ext
}

// Response is the canonical form of a non-streaming completion response.
type Response struct {
	ID      string
	Model   string // model that actually served the request
	Created int64
	Choices []Choice // len >= 1; index 0 is canonical
	Usage   Usage
	Ext     Ext // top-level same-dialect passthrough (e.g. system_fingerprint)
}

// Choice is one completion alternative.
type Choice struct {
	Index      int
	Parts      []Part
	StopReason StopReason
	Ext        Ext
}

// EventKind discriminates streaming events.
type EventKind int

const (
	EventMessageStart EventKind = iota
	EventTextDelta
	EventThinkingDelta
	EventSignatureDelta   // thinking-signature fragment; Text carries the fragment.
	EventRedactedThinking // complete redacted_thinking block; Text carries the opaque data.
	EventToolCallStart
	EventToolCallDelta
	EventToolCallEnd
	EventUsage
	EventMessageEnd // one per choice, carries the stop reason
)

// Event is the canonical streaming unit. Provider adapters produce Events;
// inbound SSE writers consume them and emit dialect-correct wire events.
type Event struct {
	Kind   EventKind
	Choice int // choice index; 0 unless the client requested n > 1

	// MessageStart
	ID    string
	Model string

	// TextDelta / ThinkingDelta
	Text string

	// ToolCallStart / ToolCallDelta / ToolCallEnd
	ToolIndex    int // per-choice tool call ordinal
	ToolID       string
	ToolName     string
	ArgsFragment string // incremental raw-JSON argument text

	// Usage
	Usage *Usage

	// MessageEnd
	StopReason StopReason
}

// Stream is a pull iterator over Events. Next returns io.EOF after the final
// event. Close aborts the upstream request and is safe to call twice.
type Stream interface {
	Next() (Event, error)
	Close() error
}
