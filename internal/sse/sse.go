// Package sse implements minimal server-sent-events reading and writing —
// just the subset the OpenAI and Anthropic streaming protocols use.
package sse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Event is one SSE event: an optional event name and the joined data payload.
type Event struct {
	Name string
	Data string
}

// Reader incrementally decodes SSE events from an upstream body.
type Reader struct {
	s *bufio.Scanner
}

// NewReader wraps r. Lines longer than 10 MiB are an error, not a panic.
func NewReader(r io.Reader) *Reader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	return &Reader{s: s}
}

// Next returns the next event, or io.EOF at end of stream.
func (r *Reader) Next() (Event, error) {
	var ev Event
	var data []string
	for r.s.Scan() {
		line := r.s.Text()
		if line == "" {
			if len(data) > 0 || ev.Name != "" {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
			// comment; ignore
		case strings.HasPrefix(line, "event:"):
			ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := r.s.Err(); err != nil {
		return Event{}, err
	}
	if len(data) > 0 || ev.Name != "" {
		ev.Data = strings.Join(data, "\n")
		return ev, nil
	}
	return Event{}, io.EOF
}

// Writer emits SSE events to an HTTP response, flushing after each event.
type Writer struct {
	w http.ResponseWriter
	f http.Flusher
}

// NewWriter sets streaming headers on w and returns a Writer. The status
// line is written immediately.
func NewWriter(w http.ResponseWriter) *Writer {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &Writer{w: w, f: f}
}

// Send writes one event. name may be empty (OpenAI-style data-only events).
func (w *Writer) Send(name, data string) error {
	var b bytes.Buffer
	if name != "" {
		fmt.Fprintf(&b, "event: %s\n", name)
	}
	fmt.Fprintf(&b, "data: %s\n\n", data)
	if _, err := w.w.Write(b.Bytes()); err != nil {
		return err
	}
	if w.f != nil {
		w.f.Flush()
	}
	return nil
}
