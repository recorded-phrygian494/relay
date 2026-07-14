package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/relay-llm/relay/internal/core"
	"github.com/relay-llm/relay/internal/provider"
)

func testRequest() *core.Request {
	return &core.Request{
		Model:   "test-model",
		Inbound: core.DialectOpenAI,
		Messages: []core.Message{
			{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: "hi"}}},
		},
	}
}

func TestCompleteSendsAuthAndTranslates(t *testing.T) {
	var gotAuth, gotHeader string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHeader = r.Header.Get("X-Custom")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"test-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer srv.Close()

	c := New(Config{Name: "mock", BaseURL: srv.URL, APIKey: "sk-1", Headers: map[string]string{"X-Custom": "v"}}, nil)
	resp, err := c.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-1" || gotHeader != "v" {
		t.Fatalf("headers: auth=%q custom=%q", gotAuth, gotHeader)
	}
	if gotBody["model"] != "test-model" {
		t.Fatalf("body model: %v", gotBody["model"])
	}
	if text := resp.Choices[0].Parts[0].(core.TextPart).Text; text != "hello" {
		t.Fatalf("text: %q", text)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}

func TestErrorNormalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limited"}}`))
	}))
	defer srv.Close()

	c := New(Config{Name: "mock", BaseURL: srv.URL}, nil)
	_, err := c.Complete(context.Background(), testRequest())
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("want provider.Error, got %T: %v", err, err)
	}
	if pe.Status != 429 || !pe.Retryable || pe.Code != "rate_limited" ||
		pe.Message != "slow down" || pe.RetryAfter != 7*time.Second {
		t.Fatalf("normalized error: %+v", pe)
	}
	if pe.Provider != "mock" {
		t.Fatalf("provider tag: %q", pe.Provider)
	}
}

func TestNonRetryable400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad","code":"invalid"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	c := New(Config{Name: "mock", BaseURL: srv.URL}, nil)
	_, err := c.Complete(context.Background(), testRequest())
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Retryable {
		t.Fatalf("400 must be non-retryable: %+v", err)
	}
}

func TestStreamRequestsUsageAndParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if so, ok := body["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
			t.Errorf("upstream should always be asked for usage, got %v", body["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := New(Config{Name: "mock", BaseURL: srv.URL}, nil)
	req := testRequest()
	req.Stream = true
	st, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var events []core.Event
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	if len(events) != 3 || events[1].Text != "hi" || events[2].StopReason != core.StopEndTurn {
		t.Fatalf("events: %+v", events)
	}
}

func TestModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","created":5,"owned_by":"org"},{"id":"m2"}]}`))
	}))
	defer srv.Close()
	c := New(Config{Name: "mock", BaseURL: srv.URL}, nil)
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "m1" || models[0].Provider != "mock" {
		t.Fatalf("models: %+v", models)
	}
}
