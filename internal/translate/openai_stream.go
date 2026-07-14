package translate

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/relay-llm/relay/internal/api/openai"
	"github.com/relay-llm/relay/internal/core"
	"github.com/relay-llm/relay/internal/sse"
)

// Event ordering contract (both directions): MessageStart first; per choice,
// content/tool events then exactly one MessageEnd; EventUsage, if any, after
// every MessageEnd. Adapters must uphold this; writers rely on it.

// openAIStream adapts an upstream OpenAI SSE body into core Events.
type openAIStream struct {
	r       *sse.Reader
	body    io.Closer
	pending []core.Event
	started bool
	// openTool tracks the currently-streaming tool call index per choice so
	// ToolCallEnd can be synthesized (the OpenAI wire has no explicit end).
	openTool map[int]int
	toolIDs  map[int]map[int]string
	done     bool
}

// NewOpenAIStream wraps an upstream SSE response body.
func NewOpenAIStream(body io.ReadCloser) core.Stream {
	return &openAIStream{
		r:        sse.NewReader(body),
		body:     body,
		openTool: map[int]int{},
		toolIDs:  map[int]map[int]string{},
	}
}

func (s *openAIStream) Close() error { return s.body.Close() }

func (s *openAIStream) Next() (core.Event, error) {
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
		if wire.Data == "[DONE]" {
			s.done = true
			continue
		}
		var chunk openai.ChatChunk
		if err := json.Unmarshal([]byte(wire.Data), &chunk); err != nil {
			return core.Event{}, fmt.Errorf("malformed stream chunk: %w", err)
		}
		s.ingest(&chunk)
	}
}

func (s *openAIStream) ingest(c *openai.ChatChunk) {
	if !s.started {
		s.started = true
		s.pending = append(s.pending, core.Event{
			Kind:  core.EventMessageStart,
			ID:    c.ID,
			Model: c.Model,
		})
	}
	for _, ch := range c.Choices {
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			s.pending = append(s.pending, core.Event{
				Kind:   core.EventTextDelta,
				Choice: ch.Index,
				Text:   *ch.Delta.Content,
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			s.ingestToolCall(ch.Index, tc)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.closeOpenTool(ch.Index)
			stop, ok := openAIStopToCore[*ch.FinishReason]
			if !ok {
				stop = core.StopEndTurn
			}
			s.pending = append(s.pending, core.Event{
				Kind:       core.EventMessageEnd,
				Choice:     ch.Index,
				StopReason: stop,
			})
		}
	}
	if c.Usage != nil {
		s.pending = append(s.pending, core.Event{
			Kind: core.EventUsage,
			Usage: &core.Usage{
				InputTokens:  c.Usage.PromptTokens,
				OutputTokens: c.Usage.CompletionTokens,
			},
		})
	}
}

func (s *openAIStream) ingestToolCall(choice int, tc openai.ToolCall) {
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	open, hasOpen := s.openTool[choice]
	if !hasOpen || open != idx {
		if hasOpen {
			s.closeOpenTool(choice)
		}
		s.openTool[choice] = idx
		if s.toolIDs[choice] == nil {
			s.toolIDs[choice] = map[int]string{}
		}
		s.toolIDs[choice][idx] = tc.ID
		s.pending = append(s.pending, core.Event{
			Kind:      core.EventToolCallStart,
			Choice:    choice,
			ToolIndex: idx,
			ToolID:    tc.ID,
			ToolName:  tc.Function.Name,
		})
	}
	if tc.Function.Arguments != "" {
		s.pending = append(s.pending, core.Event{
			Kind:         core.EventToolCallDelta,
			Choice:       choice,
			ToolIndex:    idx,
			ArgsFragment: tc.Function.Arguments,
		})
	}
}

func (s *openAIStream) closeOpenTool(choice int) {
	idx, ok := s.openTool[choice]
	if !ok {
		return
	}
	delete(s.openTool, choice)
	s.pending = append(s.pending, core.Event{
		Kind:      core.EventToolCallEnd,
		Choice:    choice,
		ToolIndex: idx,
		ToolID:    s.toolIDs[choice][idx],
	})
}

// OpenAIStreamWriter renders core Events as OpenAI streaming chunks for the
// inbound client.
type OpenAIStreamWriter struct {
	w            *sse.Writer
	created      int64
	includeUsage bool
	id           string
	model        string
	sentRole     map[int]bool
}

// NewOpenAIStreamWriter wraps an SSE writer. includeUsage reflects the
// client's stream_options.include_usage; created is stamped on every chunk.
func NewOpenAIStreamWriter(w *sse.Writer, created int64, includeUsage bool) *OpenAIStreamWriter {
	return &OpenAIStreamWriter{w: w, created: created, includeUsage: includeUsage, sentRole: map[int]bool{}}
}

func (o *OpenAIStreamWriter) send(chunk *openai.ChatChunk) error {
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return o.w.Send("", string(b))
}

func (o *OpenAIStreamWriter) chunk(choices ...openai.ChunkChoice) *openai.ChatChunk {
	if choices == nil {
		choices = []openai.ChunkChoice{}
	}
	return &openai.ChatChunk{
		ID:      o.id,
		Object:  "chat.completion.chunk",
		Created: o.created,
		Model:   o.model,
		Choices: choices,
	}
}

func (o *OpenAIStreamWriter) roleChoice(choice int) *openai.ChunkChoice {
	if o.sentRole[choice] {
		return nil
	}
	o.sentRole[choice] = true
	return &openai.ChunkChoice{Index: choice, Delta: openai.Delta{Role: "assistant"}}
}

// OnEvent renders one core Event. Call Done after the final event.
func (o *OpenAIStreamWriter) OnEvent(ev core.Event) error {
	switch ev.Kind {
	case core.EventMessageStart:
		o.id = ev.ID
		o.model = ev.Model
		if rc := o.roleChoice(0); rc != nil {
			return o.send(o.chunk(*rc))
		}
		return nil
	case core.EventTextDelta:
		text := ev.Text
		cc := openai.ChunkChoice{Index: ev.Choice, Delta: openai.Delta{Content: &text}}
		if rc := o.roleChoice(ev.Choice); rc != nil {
			rc.Delta.Content = &text
			cc = *rc
		}
		return o.send(o.chunk(cc))
	case core.EventToolCallStart:
		idx := ev.ToolIndex
		return o.send(o.chunk(openai.ChunkChoice{
			Index: ev.Choice,
			Delta: openai.Delta{ToolCalls: []openai.ToolCall{{
				Index:    &idx,
				ID:       ev.ToolID,
				Type:     "function",
				Function: openai.FunctionCall{Name: ev.ToolName, Arguments: ""},
			}}},
		}))
	case core.EventToolCallDelta:
		idx := ev.ToolIndex
		return o.send(o.chunk(openai.ChunkChoice{
			Index: ev.Choice,
			Delta: openai.Delta{ToolCalls: []openai.ToolCall{{
				Index:    &idx,
				Function: openai.FunctionCall{Arguments: ev.ArgsFragment},
			}}},
		}))
	case core.EventToolCallEnd, core.EventThinkingDelta:
		// No wire representation in this dialect.
		return nil
	case core.EventMessageEnd:
		fr, ok := coreStopToOpenAI[ev.StopReason]
		if !ok {
			fr = "stop"
		}
		return o.send(o.chunk(openai.ChunkChoice{Index: ev.Choice, Delta: openai.Delta{}, FinishReason: &fr}))
	case core.EventUsage:
		if !o.includeUsage || ev.Usage == nil {
			return nil
		}
		c := o.chunk()
		c.Usage = &openai.Usage{
			PromptTokens:     ev.Usage.InputTokens,
			CompletionTokens: ev.Usage.OutputTokens,
			TotalTokens:      ev.Usage.InputTokens + ev.Usage.OutputTokens,
		}
		return o.send(c)
	default:
		return nil
	}
}

// Done terminates the stream with the [DONE] sentinel.
func (o *OpenAIStreamWriter) Done() error {
	return o.w.Send("", "[DONE]")
}

// WriteError emits a mid-stream error event in OpenAI's convention (an
// error object in place of a chunk; no [DONE] follows).
func (o *OpenAIStreamWriter) WriteError(msg, code string) error {
	b, err := json.Marshal(openai.ErrorResponse{Error: openai.ErrorBody{Message: msg, Type: "relay_upstream_error", Code: code}})
	if err != nil {
		return err
	}
	return o.w.Send("", string(b))
}
