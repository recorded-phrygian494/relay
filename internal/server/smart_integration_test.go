package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/llmrelay/relay/internal/config"
)

// smartGateway routes unaliased names via routing.default: smart with
// tier-1 lexical, easy → mock/cheap-model, hard → mock/frontier-model.
// The upstream records which model each request asked for.
func smartGateway(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var served []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		served = append(served, req.Model)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"` + req.Model + `",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Providers: map[string]config.Provider{
			"mock": {Type: "openai-compat", BaseURL: up.URL},
		},
		Routing: config.Routing{
			Default: "smart",
			Smart:   config.SmartRouting{Easy: "mock/cheap-model", Hard: "mock/frontier-model"},
		},
		Logging: config.Logging{LogPrompts: "off"},
	}
	rt, err := BuildRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New(rt, nil, "test").Handler())
	t.Cleanup(gw.Close)
	return gw, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), served...)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSmartRoutesByDifficulty(t *testing.T) {
	gw, servedModels := smartGateway(t)

	for _, tc := range []struct {
		prompt    string
		wantModel string
	}{
		{"hey! how's it going?", "cheap-model"},
		{"Prove that the square root of 2 is irrational.", "frontier-model"},
	} {
		resp := postChat(t, gw, `{"model":"auto","messages":[{"role":"user","content":`+mustJSON(tc.prompt)+`}]}`, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%q: status %d: %s", tc.prompt, resp.StatusCode, body)
		}
	}
	served := servedModels()
	if len(served) != 2 || served[0] != "cheap-model" || served[1] != "frontier-model" {
		t.Fatalf("smart routed to %v, want [cheap-model frontier-model]", served)
	}
}

// Explicit provider/model names must bypass the classifier entirely.
func TestSmartExplicitNamesStillStatic(t *testing.T) {
	gw, servedModels := smartGateway(t)
	resp := postChat(t, gw, `{"model":"mock/exact-model","messages":[{"role":"user","content":"Prove something hard."}]}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if served := servedModels(); len(served) != 1 || served[0] != "exact-model" {
		t.Fatalf("explicit name was rerouted: %v", served)
	}
}

func TestFeedbackEndpoint(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, st := newTestGateway(t, up.URL, nil)

	resp := postChat(t, gw, chatBody, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	waitForLoggedRequest(t, st, 200)
	var id string
	if err := st.DB().QueryRow(`SELECT id FROM requests LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	post := func(body string) (int, string) {
		res, err := http.Post(gw.URL+"/v1/feedback", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}

	if code, body := post(`{"request_id":"` + id + `","score":0.9}`); code != 200 {
		t.Fatalf("feedback: %d %s", code, body)
	}
	var score float64
	if err := st.DB().QueryRow(`SELECT feedback_score FROM requests WHERE id = ?`, id).Scan(&score); err != nil || score != 0.9 {
		t.Fatalf("stored score %v err %v", score, err)
	}
	if code, _ := post(`{"request_id":"req_nope","score":0.5}`); code != 404 {
		t.Fatalf("unknown id: %d", code)
	}
	if code, _ := post(`{"request_id":"` + id + `","score":1.5}`); code != 400 {
		t.Fatalf("out-of-range score: %d", code)
	}
}
