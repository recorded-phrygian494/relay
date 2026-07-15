package translate

import (
	"encoding/json"
	"fmt"

	"github.com/relay-llm/relay/internal/api/anthropic"
	"github.com/relay-llm/relay/internal/core"
)

// stop_reason ⇄ core.StopReason. "refusal" is the closest Anthropic
// analogue of a content filter stop.
var anthropicStopToCore = map[string]core.StopReason{
	"end_turn":      core.StopEndTurn,
	"max_tokens":    core.StopMaxTokens,
	"tool_use":      core.StopToolUse,
	"stop_sequence": core.StopSequence,
	"refusal":       core.StopContentFilter,
}

var coreStopToAnthropic = map[core.StopReason]string{
	core.StopEndTurn:       "end_turn",
	core.StopMaxTokens:     "max_tokens",
	core.StopToolUse:       "tool_use",
	core.StopSequence:      "stop_sequence",
	core.StopContentFilter: "refusal",
}

func fromAnthropicUsage(u *anthropic.Usage) core.Usage {
	if u == nil {
		return core.Usage{}
	}
	out := core.Usage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	for k, v := range u.Extra {
		out.Ext.Set(core.DialectAnthropic, k, v)
	}
	return out
}

func toAnthropicUsage(u core.Usage) *anthropic.Usage {
	return &anthropic.Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadTokens,
		CacheCreationInputTokens: u.CacheWriteTokens,
		Extra:                    u.Ext.For(core.DialectAnthropic),
	}
}

// FromAnthropicResponse converts an upstream Anthropic response to the IR.
func FromAnthropicResponse(r *anthropic.MessagesResponse) (*core.Response, error) {
	stop, ok := anthropicStopToCore[r.StopReason]
	if !ok {
		stop = core.StopEndTurn
	}
	var parts []core.Part
	for i, blk := range r.Content {
		p, err := fromAnthropicBlock(blk)
		if err != nil {
			return nil, fmt.Errorf("content[%d]: %w", i, err)
		}
		parts = append(parts, p)
	}
	resp := &core.Response{
		ID:      r.ID,
		Model:   r.Model,
		Choices: []core.Choice{{Parts: parts, StopReason: stop}},
		Usage:   fromAnthropicUsage(r.Usage),
	}
	if r.StopSequence != nil {
		raw, _ := json.Marshal(*r.StopSequence)
		resp.Ext.Set(core.DialectAnthropic, "stop_sequence", raw)
	}
	for k, v := range r.Extra {
		resp.Ext.Set(core.DialectAnthropic, k, v)
	}
	return resp, nil
}

// ToAnthropicResponse converts an IR response to the Anthropic wire format
// for the inbound client. Only choice 0 is representable in this dialect.
func ToAnthropicResponse(r *core.Response) (*anthropic.MessagesResponse, error) {
	choice := r.Choices[0]
	out := &anthropic.MessagesResponse{
		ID:    r.ID,
		Type:  "message",
		Role:  "assistant",
		Model: r.Model,
		Usage: toAnthropicUsage(r.Usage),
		Extra: r.Ext.For(core.DialectAnthropic),
	}
	stop, ok := coreStopToAnthropic[choice.StopReason]
	if !ok {
		stop = "end_turn"
	}
	out.StopReason = stop
	for _, p := range choice.Parts {
		blk, err := toAnthropicBlock(p)
		if err != nil {
			return nil, err
		}
		out.Content = append(out.Content, blk)
	}
	if out.Content == nil {
		out.Content = []anthropic.ContentBlock{}
	}
	return out, nil
}
