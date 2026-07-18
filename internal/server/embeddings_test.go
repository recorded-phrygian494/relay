package server

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/config"
)

// embedUpstream mocks an OpenAI-compatible /embeddings endpoint.
func embedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type row struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]row, len(req.Input))
		for i := range req.Input {
			// Deterministic per-input vector; reversed index order to prove
			// the adapter restores it.
			j := len(req.Input) - 1 - i
			data[i] = row{Object: "embedding", Index: j, Embedding: []float32{float32(j), 0.5}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list", "data": data, "model": req.Model,
			"usage": map[string]int{"prompt_tokens": 7, "total_tokens": 7},
		})
	}))
}

func newEmbedGateway(t *testing.T) *httptest.Server {
	t.Helper()
	up := embedUpstream(t)
	t.Cleanup(up.Close)
	cfg := &config.Config{
		Server: config.Server{Listen: "127.0.0.1:0"},
		Providers: map[string]config.Provider{
			"mock":      {Type: "openai-compat", BaseURL: up.URL},
			"anthropic": {Type: "anthropic", BaseURL: up.URL, APIKey: config.StringList{"k"}},
		},
		Logging: config.Logging{LogPrompts: "off"},
	}
	rt, err := BuildRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New(rt, nil, "test").Handler())
	t.Cleanup(gw.Close)
	return gw
}

func postEmbeddings(t *testing.T, gw *httptest.Server, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(gw.URL+"/v1/embeddings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func TestEmbeddingsEndToEnd(t *testing.T) {
	gw := newEmbedGateway(t)
	resp, raw := postEmbeddings(t, gw, `{"model":"mock/embed-model","input":["a","b","c"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) != 3 || out.Model != "embed-model" || out.Usage.PromptTokens != 7 {
		t.Fatalf("response: %s", raw)
	}
	// The upstream returned rows in reversed index order; the adapter must
	// restore input order.
	for i, d := range out.Data {
		if d.Index != i || d.Embedding[0] != float32(i) {
			t.Fatalf("row %d out of order: %+v", i, d)
		}
	}
}

func TestEmbeddingsBase64(t *testing.T) {
	gw := newEmbedGateway(t)
	resp, raw := postEmbeddings(t, gw, `{"model":"mock/embed-model","input":"a","encoding_format":"base64"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			Embedding string `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	buf, err := base64.StdEncoding.DecodeString(out.Data[0].Embedding)
	if err != nil {
		t.Fatalf("not base64: %v", err)
	}
	if len(buf) != 8 {
		t.Fatalf("want 2 float32s, got %d bytes", len(buf))
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(buf[4:])); got != 0.5 {
		t.Fatalf("second component: %v", got)
	}
}

func TestEmbeddingsTokenArrayRejected(t *testing.T) {
	gw := newEmbedGateway(t)
	resp, raw := postEmbeddings(t, gw, `{"model":"mock/embed-model","input":[1,2,3]}`)
	if resp.StatusCode != 400 || !strings.Contains(string(raw), "tokenizer") {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
}

func TestEmbeddingsHonest404WithoutProviderSupport(t *testing.T) {
	gw := newEmbedGateway(t)
	resp, raw := postEmbeddings(t, gw, `{"model":"anthropic/claude-sonnet-5","input":"a"}`)
	if resp.StatusCode != 404 {
		t.Fatalf("status %d: %s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, "embeddings_not_supported") || !strings.Contains(body, "Anthropic has no embeddings API") {
		t.Fatalf("error must name the gap: %s", body)
	}
}
