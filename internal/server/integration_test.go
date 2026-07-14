package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relay-llm/relay/internal/config"
	"github.com/relay-llm/relay/internal/store"
)

// mockUpstream speaks the OpenAI wire format like a real provider would.
func mockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model","owned_by":"mock"}]}`))
		case "/chat/completions":
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			if req["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				chunks := []string{
					`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"role":"assistant","content":"str"},"finish_reason":null}]}`,
					`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{"content":"eamed"},"finish_reason":null}]}`,
					`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"test-model","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`,
				}
				for _, c := range chunks {
					_, _ = io.WriteString(w, "data: "+c+"\n\n")
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			} else {
				_, _ = w.Write([]byte(`{"id":"cmpl-1","object":"chat.completion","created":1,"model":"test-model",
					"choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
					"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func newTestGateway(t *testing.T, upstreamURL string, apiKeys []string) (*httptest.Server, *store.Store) {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0", APIKeys: apiKeys},
		Providers: map[string]config.Provider{
			"mock": {Type: "openai-compat", BaseURL: upstreamURL},
		},
		Logging: config.Logging{LogPrompts: "off"},
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := BuildRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New(rt, st, "test").Handler())
	t.Cleanup(gw.Close)
	t.Cleanup(func() { _ = st.Close() })
	return gw, st
}

func postChat(t *testing.T, gw *httptest.Server, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const chatBody = `{"model":"mock/test-model","messages":[{"role":"user","content":"ping"}]}`

func TestNonStreamingEndToEnd(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, st := newTestGateway(t, up.URL, nil)

	resp := postChat(t, gw, chatBody, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Choices[0].Message.Content != "pong" || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("response: %+v", out)
	}
	if out.Usage.TotalTokens != 6 {
		t.Fatalf("usage: %+v", out.Usage)
	}

	waitForLoggedRequest(t, st, 200)
}

func TestStreamingEndToEnd(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, st := newTestGateway(t, up.URL, nil)

	body := `{"model":"mock/test-model","messages":[{"role":"user","content":"ping"}],"stream":true,"stream_options":{"include_usage":true}}`
	resp := postChat(t, gw, body, nil)
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type: %q", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if !strings.Contains(text, `"content":"str"`) || !strings.Contains(text, `"content":"eamed"`) {
		t.Fatalf("missing deltas:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) || !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("missing terminator:\n%s", text)
	}
	if !strings.Contains(text, `"prompt_tokens":7`) {
		t.Fatalf("usage chunk not forwarded:\n%s", text)
	}

	rec := waitForLoggedRequest(t, st, 200)
	if !rec.stream || rec.tokensIn != 7 || rec.tokensOut != 2 {
		t.Fatalf("logged record: %+v", rec)
	}
}

func TestBareModelResolvesViaCatalog(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, _ := newTestGateway(t, up.URL, nil)
	resp := postChat(t, gw, `{"model":"test-model","messages":[{"role":"user","content":"ping"}]}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
}

func TestUnknownModel404(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, _ := newTestGateway(t, up.URL, nil)
	resp := postChat(t, gw, `{"model":"nope","messages":[{"role":"user","content":"x"}]}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "model_not_found") {
		t.Fatalf("body: %s", b)
	}
}

func TestModelsEndpoint(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, _ := newTestGateway(t, up.URL, nil)
	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"mock/test-model"`) {
		t.Fatalf("catalog: %s", b)
	}
}

func TestAuthEnforcement(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, _ := newTestGateway(t, up.URL, []string{"rk-secret"})

	resp := postChat(t, gw, chatBody, nil)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	resp = postChat(t, gw, chatBody, map[string]string{"Authorization": "Bearer rk-secret"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bearer key: %d", resp.StatusCode)
	}
	resp = postChat(t, gw, chatBody, map[string]string{"x-api-key": "rk-secret"})
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("x-api-key: %d", resp.StatusCode)
	}
	resp = postChat(t, gw, chatBody, map[string]string{"Authorization": "Bearer wrong"})
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("wrong key: %d", resp.StatusCode)
	}
}

func TestMalformedBody400(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, _ := newTestGateway(t, up.URL, nil)
	for _, body := range []string{"{", "null", `{"model":"m"}`, `{"messages":[]}`} {
		resp := postChat(t, gw, body, nil)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("body %q: status %d", body, resp.StatusCode)
		}
	}
}

type loggedRecord struct {
	status    int
	stream    bool
	tokensIn  int
	tokensOut int
}

// waitForLoggedRequest polls for the async log write.
func waitForLoggedRequest(t *testing.T, st *store.Store, wantStatus int) loggedRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var rec loggedRecord
		var stream int
		err := st.DB().QueryRow(
			`SELECT status, stream, tokens_in, tokens_out FROM requests WHERE status = ? LIMIT 1`,
			wantStatus,
		).Scan(&rec.status, &stream, &rec.tokensIn, &rec.tokensOut)
		if err == nil {
			rec.stream = stream == 1
			return rec
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("request was never logged to SQLite")
	return loggedRecord{}
}
