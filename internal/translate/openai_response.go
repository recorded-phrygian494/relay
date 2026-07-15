package translate

import (
	"fmt"

	"github.com/llmrelay/relay/internal/api/openai"
	"github.com/llmrelay/relay/internal/core"
)

// finish_reason ⇄ core.StopReason. stop_sequence has no OpenAI equivalent
// and folds into "stop"; the legacy "function_call" reason maps to tool_use.
var openAIStopToCore = map[string]core.StopReason{
	"stop":           core.StopEndTurn,
	"length":         core.StopMaxTokens,
	"tool_calls":     core.StopToolUse,
	"function_call":  core.StopToolUse,
	"content_filter": core.StopContentFilter,
}

var coreStopToOpenAI = map[core.StopReason]string{
	core.StopEndTurn:       "stop",
	core.StopSequence:      "stop",
	core.StopMaxTokens:     "length",
	core.StopToolUse:       "tool_calls",
	core.StopContentFilter: "content_filter",
}

// FromOpenAIResponse converts an upstream OpenAI response to the IR.
func FromOpenAIResponse(r *openai.ChatResponse) (*core.Response, error) {
	resp := &core.Response{
		ID:      r.ID,
		Model:   r.Model,
		Created: r.Created,
	}
	if r.Usage != nil {
		resp.Usage = core.Usage{
			InputTokens:  r.Usage.PromptTokens,
			OutputTokens: r.Usage.CompletionTokens,
		}
		for k, v := range r.Usage.Extra {
			resp.Usage.Ext.Set(core.DialectOpenAI, k, v)
		}
	}
	for k, v := range r.Extra {
		resp.Ext.Set(core.DialectOpenAI, k, v)
	}
	for i, c := range r.Choices {
		msg, err := fromOpenAIMessage(c.Message)
		if err != nil {
			return nil, fmt.Errorf("choices[%d]: %w", i, err)
		}
		stop, ok := openAIStopToCore[c.FinishReason]
		if !ok {
			// Unknown finish reasons from compat providers degrade to end_turn
			// rather than failing the whole response.
			stop = core.StopEndTurn
		}
		resp.Choices = append(resp.Choices, core.Choice{
			Index:      c.Index,
			Parts:      msg.Parts,
			StopReason: stop,
		})
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("response has no choices")
	}
	return resp, nil
}

// ToOpenAIResponse converts an IR response to the OpenAI wire format for
// the inbound client.
func ToOpenAIResponse(r *core.Response) (*openai.ChatResponse, error) {
	out := &openai.ChatResponse{
		ID:      r.ID,
		Object:  "chat.completion",
		Created: r.Created,
		Model:   r.Model,
		Usage: &openai.Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
			Extra:            r.Usage.Ext.For(core.DialectOpenAI),
		},
		Extra: r.Ext.For(core.DialectOpenAI),
	}
	for _, c := range r.Choices {
		msgs, err := toOpenAIMessages(core.Message{Role: core.RoleAssistant, Parts: c.Parts})
		if err != nil {
			return nil, fmt.Errorf("choices[%d]: %w", c.Index, err)
		}
		msg := openai.ChatMessage{Role: "assistant"}
		if len(msgs) == 1 {
			msg = msgs[0]
		} else if len(msgs) > 1 {
			return nil, fmt.Errorf("choices[%d]: assistant content fanned out to %d messages", c.Index, len(msgs))
		}
		if msg.Content == nil {
			// OpenAI responses carry an explicit content: null for pure
			// tool-call messages; MessageContent zero value serializes to null.
			msg.Content = &openai.MessageContent{}
		}
		fr, ok := coreStopToOpenAI[c.StopReason]
		if !ok {
			fr = "stop"
		}
		out.Choices = append(out.Choices, openai.Choice{
			Index:        c.Index,
			Message:      msg,
			FinishReason: fr,
		})
	}
	return out, nil
}
