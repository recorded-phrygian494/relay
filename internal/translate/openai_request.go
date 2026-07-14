// Package translate converts between API wire dialects and the core IR.
// Each dialect gets exactly two codecs (inbound wire → IR, IR → outbound
// wire) for requests, responses, and streams; cross-dialect behavior falls
// out of composing them through the IR.
package translate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/relay-llm/relay/internal/api/openai"
	"github.com/relay-llm/relay/internal/core"
)

// FromOpenAIRequest converts an inbound OpenAI chat request to the IR.
func FromOpenAIRequest(r *openai.ChatRequest) (*core.Request, error) {
	req := &core.Request{
		Model:       r.Model,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		N:           r.N,
		Stop:        []string(r.Stop),
		Stream:      r.Stream,
		Metadata:    r.Metadata,
		Inbound:     core.DialectOpenAI,
	}
	switch {
	case r.MaxCompletionTokens != nil:
		req.MaxTokens = r.MaxCompletionTokens
	case r.MaxTokens != nil:
		req.MaxTokens = r.MaxTokens
		req.LegacyMaxTokens = true
	}
	if r.StreamOptions != nil {
		req.IncludeStreamUsage = r.StreamOptions.IncludeUsage
	}

	for i, m := range r.Messages {
		msg, err := fromOpenAIMessage(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, msg)
	}

	for _, t := range r.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("unsupported tool type %q", t.Type)
		}
		req.Tools = append(req.Tools, core.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
			Strict:      t.Function.Strict,
		})
	}

	tc, err := fromOpenAIToolChoice(r.ToolChoice)
	if err != nil {
		return nil, err
	}
	req.ToolChoice = tc

	if r.ResponseFormat != nil {
		rf := &core.ResponseFormat{Type: r.ResponseFormat.Type}
		if js := r.ResponseFormat.JSONSchema; js != nil {
			rf.SchemaName = js.Name
			rf.Description = js.Description
			rf.Schema = js.Schema
			rf.Strict = js.Strict
		}
		req.ResponseFormat = rf
	}

	for k, v := range r.Extra {
		req.Ext.Set(core.DialectOpenAI, k, v)
	}
	return req, nil
}

func fromOpenAIMessage(m openai.ChatMessage) (core.Message, error) {
	msg := core.Message{Role: core.Role(m.Role), Name: m.Name}

	var parts []core.Part
	if m.Content != nil {
		var err error
		parts, err = fromOpenAIContent(*m.Content)
		if err != nil {
			return msg, err
		}
	}

	if m.Role == "tool" {
		msg.Parts = []core.Part{core.ToolResultPart{ToolCallID: m.ToolCallID, Content: parts}}
		return msg, nil
	}

	msg.Parts = parts
	for _, tc := range m.ToolCalls {
		msg.Parts = append(msg.Parts, core.ToolCallPart{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}
	return msg, nil
}

func fromOpenAIContent(c openai.MessageContent) ([]core.Part, error) {
	if c.Text != nil {
		if *c.Text == "" {
			return nil, nil
		}
		return []core.Part{core.TextPart{Text: *c.Text}}, nil
	}
	var parts []core.Part
	for i, p := range c.Parts {
		switch p.Type {
		case "text":
			parts = append(parts, core.TextPart{Text: p.Text})
		case "image_url":
			if p.ImageURL == nil {
				return nil, fmt.Errorf("content[%d]: image_url part missing image_url", i)
			}
			img, err := fromOpenAIImage(*p.ImageURL)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			parts = append(parts, img)
		default:
			return nil, fmt.Errorf("content[%d]: unsupported part type %q", i, p.Type)
		}
	}
	return parts, nil
}

func fromOpenAIImage(u openai.ImageURL) (core.ImagePart, error) {
	img := core.ImagePart{Detail: u.Detail}
	if data, ok := strings.CutPrefix(u.URL, "data:"); ok {
		mediaType, payload, found := strings.Cut(data, ";base64,")
		if !found {
			return img, fmt.Errorf("unsupported data: URI (only base64 is supported)")
		}
		if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
			return img, fmt.Errorf("invalid base64 image payload: %w", err)
		}
		img.MediaType = mediaType
		img.Data = payload
		return img, nil
	}
	img.URL = u.URL
	return img, nil
}

func fromOpenAIToolChoice(raw json.RawMessage) (core.ToolChoice, error) {
	if len(raw) == 0 {
		return core.ToolChoice{}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return core.ToolChoice{Mode: core.ToolChoiceAuto}, nil
		case "none":
			return core.ToolChoice{Mode: core.ToolChoiceNone}, nil
		case "required":
			return core.ToolChoice{Mode: core.ToolChoiceRequired}, nil
		default:
			return core.ToolChoice{}, fmt.Errorf("unknown tool_choice %q", s)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return core.ToolChoice{}, fmt.Errorf("invalid tool_choice: %w", err)
	}
	if obj.Function.Name == "" {
		return core.ToolChoice{}, fmt.Errorf("tool_choice object missing function.name")
	}
	return core.ToolChoice{Mode: core.ToolChoiceTool, Name: obj.Function.Name}, nil
}

// ToOpenAIRequest converts an IR request to the OpenAI wire format for an
// outbound provider call.
func ToOpenAIRequest(r *core.Request) (*openai.ChatRequest, error) {
	out := &openai.ChatRequest{
		Model:       r.Model,
		Temperature: clampTemperature(r.Temperature, core.DialectOpenAI),
		TopP:        r.TopP,
		N:           r.N,
		Stop:        openai.StringOrList(r.Stop),
		Stream:      r.Stream,
		Metadata:    r.Metadata,
		Extra:       r.Ext.For(core.DialectOpenAI),
	}
	if r.MaxTokens != nil {
		if r.LegacyMaxTokens {
			out.MaxTokens = r.MaxTokens
		} else {
			out.MaxCompletionTokens = r.MaxTokens
		}
	}

	// Anthropic-inbound system parameter becomes a leading system message.
	for i := len(r.System) - 1; i >= 0; i-- {
		text := r.System[i].Text
		out.Messages = append([]openai.ChatMessage{{
			Role:    "system",
			Content: &openai.MessageContent{Text: &text},
		}}, out.Messages...)
	}

	for i, m := range r.Messages {
		wire, err := toOpenAIMessages(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, wire...)
	}

	for _, t := range r.Tools {
		out.Tools = append(out.Tools, openai.ToolDef{
			Type: "function",
			Function: openai.FunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
				Strict:      t.Strict,
			},
		})
	}

	switch r.ToolChoice.Mode {
	case core.ToolChoiceUnset:
	case core.ToolChoiceTool:
		out.ToolChoice, _ = json.Marshal(map[string]any{
			"type":     "function",
			"function": map[string]string{"name": r.ToolChoice.Name},
		})
	default:
		out.ToolChoice, _ = json.Marshal(string(r.ToolChoice.Mode))
	}

	if rf := r.ResponseFormat; rf != nil {
		wire := &openai.ResponseFormat{Type: rf.Type}
		if rf.Type == "json_schema" {
			wire.JSONSchema = &openai.JSONSchemaSpec{
				Name:        rf.SchemaName,
				Description: rf.Description,
				Schema:      rf.Schema,
				Strict:      rf.Strict,
			}
		}
		out.ResponseFormat = wire
	}

	if r.Stream && r.IncludeStreamUsage {
		out.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}
	return out, nil
}

// toOpenAIMessages converts one IR message; a message mixing tool results
// with other content fans out to multiple wire messages (OpenAI requires
// role:"tool" messages to carry exactly one result).
func toOpenAIMessages(m core.Message) ([]openai.ChatMessage, error) {
	var out []openai.ChatMessage
	cur := openai.ChatMessage{Role: string(m.Role), Name: m.Name}
	var contentParts []openai.ContentPart
	var plainText *string

	flushContent := func() {
		if plainText != nil {
			cur.Content = &openai.MessageContent{Text: plainText}
		} else if contentParts != nil {
			cur.Content = &openai.MessageContent{Parts: contentParts}
		}
	}

	addText := func(text string) {
		if plainText == nil && contentParts == nil {
			plainText = &text
			return
		}
		if plainText != nil {
			contentParts = append(contentParts, openai.ContentPart{Type: "text", Text: *plainText})
			plainText = nil
		}
		contentParts = append(contentParts, openai.ContentPart{Type: "text", Text: text})
	}

	for _, p := range m.Parts {
		switch p := p.(type) {
		case core.TextPart:
			addText(p.Text)
		case core.ImagePart:
			if plainText != nil {
				contentParts = append(contentParts, openai.ContentPart{Type: "text", Text: *plainText})
				plainText = nil
			}
			contentParts = append(contentParts, openai.ContentPart{Type: "image_url", ImageURL: toOpenAIImage(p)})
		case core.ToolCallPart:
			cur.ToolCalls = append(cur.ToolCalls, openai.ToolCall{
				ID:       p.ID,
				Type:     "function",
				Function: openai.FunctionCall{Name: p.Name, Arguments: p.Args},
			})
		case core.ToolResultPart:
			text, err := toolResultText(p)
			if err != nil {
				return nil, err
			}
			out = append(out, openai.ChatMessage{
				Role:       "tool",
				ToolCallID: p.ToolCallID,
				Content:    &openai.MessageContent{Text: &text},
			})
		case core.ThinkingPart:
			// Cross-dialect thinking is dropped in v1 (see DESIGN §5.1).
		default:
			return nil, fmt.Errorf("unsupported part type %T", p)
		}
	}

	flushContent()
	if cur.Content != nil || len(cur.ToolCalls) > 0 {
		out = append(out, cur)
	}
	return out, nil
}

func toOpenAIImage(p core.ImagePart) *openai.ImageURL {
	u := &openai.ImageURL{Detail: p.Detail}
	if p.Data != "" {
		u.URL = "data:" + p.MediaType + ";base64," + p.Data
	} else {
		u.URL = p.URL
	}
	return u
}

func toolResultText(p core.ToolResultPart) (string, error) {
	var sb strings.Builder
	for _, c := range p.Content {
		switch c := c.(type) {
		case core.TextPart:
			sb.WriteString(c.Text)
		default:
			return "", fmt.Errorf("tool result part type %T not representable in OpenAI dialect", c)
		}
	}
	return sb.String(), nil
}

// clampTemperature clamps to the target dialect's legal range. Values are
// never rescaled — rescaling silently changes sampling behavior (DESIGN §5.1).
func clampTemperature(t *float64, d core.Dialect) *float64 {
	if t == nil {
		return nil
	}
	max := 1.0
	if d == core.DialectOpenAI {
		max = 2.0
	}
	v := *t
	if v < 0 {
		v = 0
	}
	if v > max {
		v = max
	}
	return &v
}
