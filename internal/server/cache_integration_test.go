package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/llmrelay/relay/internal/config"
)

// newCachingGateway builds a gateway with the exact-match cache enabled
// over a call-counting mock upstream.
func newCachingGateway(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var upstreamCalls atomic.Int64
	inner := mockUpstream(t)
	t.Cleanup(inner.Close)
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			upstreamCalls.Add(1)
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(counting.Close)

	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Providers: map[string]config.Provider{
			"mock": {Type: "openai-compat", BaseURL: counting.URL},
		},
		Cache:   config.Cache{Enabled: true},
		Logging: config.Logging{LogPrompts: "off"},
	}
	rt, err := BuildRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New(rt, nil, "test").Handler())
	t.Cleanup(gw.Close)
	return gw, &upstreamCalls
}

const cacheableChatBody = `{"model":"mock/test-model","temperature":0,"max_completion_tokens":64,"messages":[{"role":"user","content":"ping"}]}`

func TestCacheHitSkipsUpstream(t *testing.T) {
	gw, calls := newCachingGateway(t)

	for i := 0; i < 2; i++ {
		resp := postChat(t, gw, cacheableChatBody, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), "pong") {
			t.Fatalf("request %d: status %d: %s", i, resp.StatusCode, body)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls: %d (second request must be served from cache)", got)
	}
}

func TestCacheSkipsNonDeterministic(t *testing.T) {
	gw, calls := newCachingGateway(t)
	warm := `{"model":"mock/test-model","temperature":0.9,"messages":[{"role":"user","content":"ping"}]}`
	for i := 0; i < 2; i++ {
		resp := postChat(t, gw, warm, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls: %d (temperature 0.9 must never cache)", got)
	}
}

func TestCacheCrossDialectAndStreamReplay(t *testing.T) {
	gw, calls := newCachingGateway(t)

	// Warm the cache through the OpenAI dialect, non-streaming.
	resp := postChat(t, gw, cacheableChatBody, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Same request as an Anthropic-dialect *stream* must hit the same
	// entry (key erases dialect + stream flag) and replay as SSE.
	anthropicBody := `{"model":"mock/test-model","temperature":0,"max_tokens":64,"stream":true,"messages":[{"role":"user","content":"ping"}]}`
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(anthropicBody))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	text := string(raw)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type: %q body: %s", ct, text)
	}
	if !strings.Contains(text, "message_start") || !strings.Contains(text, "pong") || !strings.Contains(text, "message_stop") {
		t.Fatalf("synthetic replay malformed:\n%s", text)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls: %d (cross-dialect stream must be a cache hit)", got)
	}
}
