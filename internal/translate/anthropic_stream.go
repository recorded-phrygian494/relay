package translate

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/relay-llm/relay/internal/api/anthropic"
	"github.com/relay-llm/relay/internal/core"
	"github.com/relay-llm/relay/internal/sse"
)

// anthropicStream adapts an upstream Anthropic SSE body into core Events.
type anthropicStream struct {
	r       *sse.Reader
	body    io.Closer
	pending []core.Event

	// blockKind maps the wire's content-block index to its type; toolOrdinal
	// maps tool_use block indexes to the IR's per-message tool ordinal.
	blockKind   map[int]string
	toolOrdinal map[int]int
	nextTool    int

	usage      core.Usage
	stopReason core.StopReason
	done       bool
}

// NewAnthropicStream wraps an upstream Anthropic SSE response body.
func NewAnthropicStream(body io.ReadCloser) core.Stream {
	return &anthropicStream{
		r:           sse.NewReader(body),
		body:        body,
		blockKind:   map[int]string{},
		toolOrdinal: map[int]int{},
		stopReason:  core.StopEndTurn,
	}
}

func (s *anthropicStream) Close() error { return s.body.Close() }

func (s *anthropicStream) Next() (core.Event, error) {
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
		if err != nil {
			return core.Event{}, err
		}
		var ev anthropic.StreamEvent
		if err := json.Unmarshal([]byte(wire.Data), &ev); err != nil {
			return core.Event{}, fmt.Errorf("malformed stream event: %w", err)
		}
		if ev.Type == "error" || wire.Name == "error" {
			msg := "upstream error"
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			return core.Event{}, fmt.Errorf("anthropic stream error: %s", msg)
		}
		s.ingest(&ev)
	}
}

func (s *anthropicStream) ingest(ev *anthropic.StreamEvent) {
	switch ev.Type {
	case "message_start":
		var id, model string
		if ev.Message != nil {
			id, model = ev.Message.ID, ev.Message.Model
			s.usage = fromAnthropicUsage(ev.Message.Usage)
		}
		s.pending = append(s.pending, core.Event{Kind: core.EventMessageStart, ID: id, Model: model})

	case "content_block_start":
		if ev.Index == nil || ev.ContentBlock == nil {
			return
		}
		idx := *ev.Index
		s.blockKind[idx] = ev.ContentBlock.Type
		if ev.ContentBlock.Type == "tool_use" {
			ord := s.nextTool
			s.nextTool++
			s.toolOrdinal[idx] = ord
			s.pending = append(s.pending, core.Event{
				Kind:      core.EventToolCallStart,
				ToolIndex: ord,
				ToolID:    ev.ContentBlock.ID,
				ToolName:  ev.ContentBlock.Name,
			})
		}

	case "content_block_delta":
		if ev.Index == nil || ev.Delta == nil {
			return
		}
		idx := *ev.Index
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				s.pending = append(s.pending, core.Event{Kind: core.EventTextDelta, Text: ev.Delta.Text})
			}
		case "input_json_delta":
			if ev.Delta.PartialJSON != "" {
				s.pending = append(s.pending, core.Event{
					Kind:         core.EventToolCallDelta,
					ToolIndex:    s.toolOrdinal[idx],
					ArgsFragment: ev.Delta.PartialJSON,
				})
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				s.pending = append(s.pending, core.Event{Kind: core.EventThinkingDelta, Text: ev.Delta.Thinking})
			}
		case "signature_delta":
			// Thinking signatures are not modeled in stream events (v1).
		}

	case "content_block_stop":
		if ev.Index == nil {
			return
		}
		idx := *ev.Index
		if s.blockKind[idx] == "tool_use" {
			s.pending = append(s.pending, core.Event{
				Kind:      core.EventToolCallEnd,
				ToolIndex: s.toolOrdinal[idx],
			})
		}

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			if stop, ok := anthropicStopToCore[ev.Delta.StopReason]; ok {
				s.stopReason = stop
			}
		}
		if ev.Usage != nil {
			if ev.Usage.InputTokens > 0 {
				s.usage.InputTokens = ev.Usage.InputTokens
			}
			s.usage.OutputTokens = ev.Usage.OutputTokens
		}

	case "message_stop":
		s.done = true
		u := s.usage
		s.pending = append(s.pending,
			core.Event{Kind: core.EventMessageEnd, StopReason: s.stopReason},
			core.Event{Kind: core.EventUsage, Usage: &u},
		)

	case "ping":
		// keepalive; ignore
	}
}

// AnthropicStreamWriter renders core Events as Anthropic streaming events,
// maintaining the message_start → content_block* → message_delta →
// message_stop envelope. Only choice 0 is representable; events for other
// choices are dropped.
type AnthropicStreamWriter struct {
	w *sse.Writer

	id        string
	model     string
	nextBlock int
	openKind  string // "" | "text" | "thinking" | "tool_use"

	stopReason core.StopReason
	usage      core.Usage
}

// NewAnthropicStreamWriter wraps an SSE writer.
func NewAnthropicStreamWriter(w *sse.Writer) *AnthropicStreamWriter {
	return &AnthropicStreamWriter{w: w, stopReason: core.StopEndTurn}
}

func (a *AnthropicStreamWriter) send(eventType string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return a.w.Send(eventType, string(b))
}

func (a *AnthropicStreamWriter) openBlock(kind string, block anthropic.ContentBlock) error {
	if err := a.closeBlock(); err != nil {
		return err
	}
	idx := a.nextBlock
	a.openKind = kind
	return a.send("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": block,
	})
}

func (a *AnthropicStreamWriter) closeBlock() error {
	if a.openKind == "" {
		return nil
	}
	idx := a.nextBlock
	a.nextBlock++
	a.openKind = ""
	return a.send("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (a *AnthropicStreamWriter) delta(d anthropic.StreamDelta) error {
	return a.send("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": a.nextBlock,
		"delta": d,
	})
}

// OnEvent renders one core Event. Call Done after the final event.
func (a *AnthropicStreamWriter) OnEvent(ev core.Event) error {
	if ev.Choice != 0 {
		return nil
	}
	switch ev.Kind {
	case core.EventMessageStart:
		a.id, a.model = ev.ID, ev.Model
		if a.id == "" {
			a.id = "msg_relay"
		}
		return a.send("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            a.id,
				"type":          "message",
				"role":          "assistant",
				"model":         a.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				// Real input_tokens arrive with the final message_delta; some
				// upstreams (OpenAI dialect) only report usage at end of stream.
				"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
			},
		})
	case core.EventTextDelta:
		if a.openKind != "text" {
			if err := a.openBlock("text", anthropic.ContentBlock{Type: "text"}); err != nil {
				return err
			}
		}
		return a.delta(anthropic.StreamDelta{Type: "text_delta", Text: ev.Text})
	case core.EventThinkingDelta:
		if a.openKind != "thinking" {
			if err := a.openBlock("thinking", anthropic.ContentBlock{Type: "thinking"}); err != nil {
				return err
			}
		}
		return a.delta(anthropic.StreamDelta{Type: "thinking_delta", Thinking: ev.Text})
	case core.EventToolCallStart:
		id := ev.ToolID
		if id == "" {
			id = fmt.Sprintf("toolu_relay_%d", ev.ToolIndex)
		}
		return a.openBlock("tool_use", anthropic.ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  ev.ToolName,
			Input: json.RawMessage("{}"),
		})
	case core.EventToolCallDelta:
		if a.openKind != "tool_use" {
			return nil // defensive: delta without start
		}
		return a.delta(anthropic.StreamDelta{Type: "input_json_delta", PartialJSON: ev.ArgsFragment})
	case core.EventToolCallEnd:
		if a.openKind == "tool_use" {
			return a.closeBlock()
		}
		return nil
	case core.EventMessageEnd:
		a.stopReason = ev.StopReason
		return a.closeBlock()
	case core.EventUsage:
		if ev.Usage != nil {
			a.usage = *ev.Usage
		}
		return nil
	default:
		return nil
	}
}

// Done closes the envelope: message_delta (stop reason + usage), message_stop.
func (a *AnthropicStreamWriter) Done() error {
	if err := a.closeBlock(); err != nil {
		return err
	}
	stop, ok := coreStopToAnthropic[a.stopReason]
	if !ok {
		stop = "end_turn"
	}
	if err := a.send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]int{
			"input_tokens":  a.usage.InputTokens,
			"output_tokens": a.usage.OutputTokens,
		},
	}); err != nil {
		return err
	}
	return a.send("message_stop", map[string]any{"type": "message_stop"})
}

// WriteError emits a mid-stream error event in Anthropic's convention.
func (a *AnthropicStreamWriter) WriteError(msg, code string) error {
	if code == "" {
		code = "api_error"
	}
	return a.send("error", anthropic.ErrorResponse{
		Type:  "error",
		Error: anthropic.ErrorBody{Type: code, Message: msg},
	})
}
