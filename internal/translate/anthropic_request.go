package translate

import (
	"encoding/json"
	"fmt"

	"github.com/llmrelay/relay/internal/api/anthropic"
	"github.com/llmrelay/relay/internal/core"
)

// DefaultAnthropicMaxTokens is injected when an OpenAI-inbound request omits
// max_tokens but targets an Anthropic provider, which requires it.
const DefaultAnthropicMaxTokens = 4096

// FromAnthropicRequest converts an inbound Anthropic messages request to the
// IR. Per the core invariant, the system parameter lands in System and
// Messages never contains system turns.
func FromAnthropicRequest(r *anthropic.MessagesRequest) (*core.Request, error) {
	req := &core.Request{
		Model:       r.Model,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        r.StopSequences,
		Stream:      r.Stream,
		Metadata:    r.Metadata,
		Inbound:     core.DialectAnthropic,
	}

	if r.System != nil {
		if r.System.Text != nil {
			req.System = []core.SystemPart{{Text: *r.System.Text}}
		} else {
			for i, blk := range r.System.Blocks {
				if blk.Type != "text" {
					return nil, fmt.Errorf("system[%d]: unsupported block type %q", i, blk.Type)
				}
				req.System = append(req.System, core.SystemPart{Text: blk.Text, Ext: blk.Extra})
			}
		}
	}

	for i, m := range r.Messages {
		parts, err := fromAnthropicContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		// role "system" inside messages (Claude Code does this; see
		// ParseMessagesRequest) keeps RoleSystem here and is hoisted into
		// the system parameter by outbound codecs.
		req.Messages = append(req.Messages, core.Message{Role: core.Role(m.Role), Parts: parts})
	}

	for _, t := range r.Tools {
		req.Tools = append(req.Tools, core.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
			Ext:         t.Extra,
		})
	}

	if tc := r.ToolChoice; tc != nil {
		choice := core.ToolChoice{DisableParallel: tc.DisableParallelToolUse}
		switch tc.Type {
		case "auto":
			choice.Mode = core.ToolChoiceAuto
		case "any":
			choice.Mode = core.ToolChoiceRequired
		case "none":
			choice.Mode = core.ToolChoiceNone
		case "tool":
			choice.Mode = core.ToolChoiceTool
			choice.Name = tc.Name
		default:
			return nil, fmt.Errorf("unknown tool_choice type %q", tc.Type)
		}
		req.ToolChoice = choice
	}

	for k, v := range r.Extra {
		req.Ext.Set(core.DialectAnthropic, k, v)
	}
	return req, nil
}

func fromAnthropicContent(c anthropic.MessageContent) ([]core.Part, error) {
	if c.Text != nil {
		if *c.Text == "" {
			return nil, nil
		}
		return []core.Part{core.TextPart{Text: *c.Text}}, nil
	}
	var parts []core.Part
	for i, blk := range c.Blocks {
		p, err := fromAnthropicBlock(blk)
		if err != nil {
			return nil, fmt.Errorf("content[%d]: %w", i, err)
		}
		parts = append(parts, p)
	}
	return parts, nil
}

func fromAnthropicBlock(blk anthropic.ContentBlock) (core.Part, error) {
	switch blk.Type {
	case "text":
		return core.TextPart{Text: blk.Text, Ext: blk.Extra}, nil
	case "image":
		if blk.Source == nil {
			return nil, fmt.Errorf("image block missing source")
		}
		img := core.ImagePart{Ext: blk.Extra}
		switch blk.Source.Type {
		case "base64":
			img.MediaType = blk.Source.MediaType
			img.Data = blk.Source.Data
		case "url":
			img.URL = blk.Source.URL
		default:
			return nil, fmt.Errorf("unsupported image source type %q", blk.Source.Type)
		}
		return img, nil
	case "tool_use":
		args := "{}"
		if len(blk.Input) > 0 {
			args = string(blk.Input)
		}
		return core.ToolCallPart{ID: blk.ID, Name: blk.Name, Args: args, Ext: blk.Extra}, nil
	case "tool_result":
		var inner []core.Part
		if blk.Content != nil {
			var err error
			inner, err = fromAnthropicContent(*blk.Content)
			if err != nil {
				return nil, err
			}
		}
		isErr := blk.IsError != nil && *blk.IsError
		return core.ToolResultPart{ToolCallID: blk.ToolUseID, Content: inner, IsError: isErr, Ext: blk.Extra}, nil
	case "thinking":
		return core.ThinkingPart{Text: blk.Thinking, Signature: blk.Signature, Ext: blk.Extra}, nil
	case "redacted_thinking":
		return core.ThinkingPart{Redacted: blk.Data, Ext: blk.Extra}, nil
	default:
		return nil, fmt.Errorf("unsupported block type %q", blk.Type)
	}
}

// ToAnthropicRequest converts an IR request to the Anthropic wire format.
// ResponseFormat must already be cleared (the anthropic provider adapter
// owns json_schema emulation); a leftover non-text format is an error.
func ToAnthropicRequest(r *core.Request) (*anthropic.MessagesRequest, error) {
	if r.N != nil && *r.N > 1 {
		return nil, fmt.Errorf("n=%d is not supported by the Anthropic dialect (DESIGN §5.1)", *r.N)
	}
	if rf := r.ResponseFormat; rf != nil && rf.Type != "" && rf.Type != "text" {
		return nil, fmt.Errorf("response_format %q reached the Anthropic codec unemulated", rf.Type)
	}

	out := &anthropic.MessagesRequest{
		Model:         r.Model,
		Temperature:   clampTemperature(r.Temperature, core.DialectAnthropic),
		TopP:          r.TopP,
		StopSequences: r.Stop,
		Stream:        r.Stream,
		Metadata:      r.Metadata,
		Extra:         r.Ext.For(core.DialectAnthropic),
	}
	maxTokens := DefaultAnthropicMaxTokens
	if r.MaxTokens != nil {
		maxTokens = *r.MaxTokens
	}
	out.MaxTokens = &maxTokens

	system := append([]core.SystemPart(nil), r.System...)

	for i, m := range r.Messages {
		switch m.Role {
		case core.RoleSystem, core.RoleDeveloper:
			// Hoist mid-conversation system turns into the system parameter
			// (position is lost; Anthropic has no positional system turns).
			for _, p := range m.Parts {
				tp, ok := p.(core.TextPart)
				if !ok {
					return nil, fmt.Errorf("messages[%d]: non-text system content", i)
				}
				system = append(system, core.SystemPart{Text: tp.Text, Ext: tp.Ext})
			}
		case core.RoleUser, core.RoleAssistant, core.RoleTool:
			msg, err := toAnthropicMessage(m)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			out.Messages = append(out.Messages, msg)
		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}

	if len(system) > 0 {
		out.System = toAnthropicSystem(system)
	}

	// Cross-dialect only: merge consecutive same-role turns (OpenAI expresses
	// tool results as separate role:tool messages; Anthropic wants them as
	// blocks of the single next user turn). Same-dialect input keeps its
	// structure untouched.
	if r.Inbound != core.DialectAnthropic {
		out.Messages = mergeConsecutiveRoles(out.Messages)
	}

	for _, t := range r.Tools {
		out.Tools = append(out.Tools, anthropic.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
			Extra:       t.Ext,
		})
	}

	switch r.ToolChoice.Mode {
	case core.ToolChoiceUnset:
	case core.ToolChoiceAuto:
		out.ToolChoice = &anthropic.ToolChoice{Type: "auto", DisableParallelToolUse: r.ToolChoice.DisableParallel}
	case core.ToolChoiceRequired:
		out.ToolChoice = &anthropic.ToolChoice{Type: "any", DisableParallelToolUse: r.ToolChoice.DisableParallel}
	case core.ToolChoiceNone:
		out.ToolChoice = &anthropic.ToolChoice{Type: "none"}
	case core.ToolChoiceTool:
		out.ToolChoice = &anthropic.ToolChoice{Type: "tool", Name: r.ToolChoice.Name, DisableParallelToolUse: r.ToolChoice.DisableParallel}
	}

	return out, nil
}

// toAnthropicSystem emits the string form for the single plain-text case,
// else text blocks. Both forms are equivalent on the wire.
func toAnthropicSystem(parts []core.SystemPart) *anthropic.SystemPrompt {
	if len(parts) == 1 && len(parts[0].Ext) == 0 {
		text := parts[0].Text
		return &anthropic.SystemPrompt{Text: &text}
	}
	sp := &anthropic.SystemPrompt{}
	for _, p := range parts {
		sp.Blocks = append(sp.Blocks, anthropic.ContentBlock{Type: "text", Text: p.Text, Extra: p.Ext})
	}
	return sp
}

func toAnthropicMessage(m core.Message) (anthropic.Message, error) {
	role := string(m.Role)
	if m.Role == core.RoleTool {
		role = "user" // tool results live in user turns
	}
	msg := anthropic.Message{Role: role}
	var blocks []anthropic.ContentBlock
	for _, p := range m.Parts {
		blk, err := toAnthropicBlock(p)
		if err != nil {
			return msg, err
		}
		blocks = append(blocks, blk)
	}
	if len(blocks) == 1 && blocks[0].Type == "text" && len(blocks[0].Extra) == 0 {
		text := blocks[0].Text
		msg.Content = anthropic.MessageContent{Text: &text}
		return msg, nil
	}
	msg.Content = anthropic.MessageContent{Blocks: blocks}
	return msg, nil
}

func toAnthropicBlock(p core.Part) (anthropic.ContentBlock, error) {
	switch p := p.(type) {
	case core.TextPart:
		return anthropic.ContentBlock{Type: "text", Text: p.Text, Extra: p.Ext}, nil
	case core.ImagePart:
		src := &anthropic.ImageSource{}
		if p.Data != "" {
			src.Type, src.MediaType, src.Data = "base64", p.MediaType, p.Data
		} else {
			src.Type, src.URL = "url", p.URL
		}
		return anthropic.ContentBlock{Type: "image", Source: src, Extra: p.Ext}, nil
	case core.ToolCallPart:
		input := json.RawMessage(p.Args)
		if p.Args == "" {
			input = json.RawMessage("{}")
		}
		if !json.Valid(input) {
			return anthropic.ContentBlock{}, fmt.Errorf("tool call %s has non-JSON arguments", p.Name)
		}
		return anthropic.ContentBlock{Type: "tool_use", ID: p.ID, Name: p.Name, Input: input, Extra: p.Ext}, nil
	case core.ToolResultPart:
		blk := anthropic.ContentBlock{Type: "tool_result", ToolUseID: p.ToolCallID, Extra: p.Ext}
		if p.IsError {
			isErr := true
			blk.IsError = &isErr
		}
		if len(p.Content) > 0 {
			var inner []anthropic.ContentBlock
			for _, c := range p.Content {
				cb, err := toAnthropicBlock(c)
				if err != nil {
					return blk, err
				}
				inner = append(inner, cb)
			}
			if len(inner) == 1 && inner[0].Type == "text" && len(inner[0].Extra) == 0 {
				text := inner[0].Text
				blk.Content = &anthropic.MessageContent{Text: &text}
			} else {
				blk.Content = &anthropic.MessageContent{Blocks: inner}
			}
		}
		return blk, nil
	case core.ThinkingPart:
		if p.Redacted != "" {
			return anthropic.ContentBlock{Type: "redacted_thinking", Data: p.Redacted, Extra: p.Ext}, nil
		}
		return anthropic.ContentBlock{Type: "thinking", Thinking: p.Text, Signature: p.Signature, Extra: p.Ext}, nil
	default:
		return anthropic.ContentBlock{}, fmt.Errorf("unsupported part type %T", p)
	}
}

// mergeConsecutiveRoles folds runs of same-role messages into one message
// with concatenated blocks, forcing block form for merged content.
func mergeConsecutiveRoles(msgs []anthropic.Message) []anthropic.Message {
	var out []anthropic.Message
	for _, m := range msgs {
		if len(out) > 0 && out[len(out)-1].Role == m.Role {
			prev := &out[len(out)-1]
			prev.Content = anthropic.MessageContent{
				Blocks: append(contentBlocks(prev.Content), contentBlocks(m.Content)...),
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

// contentBlocks canonicalizes string-form content to a single text block.
func contentBlocks(c anthropic.MessageContent) []anthropic.ContentBlock {
	if c.Text != nil {
		return []anthropic.ContentBlock{{Type: "text", Text: *c.Text}}
	}
	return c.Blocks
}
