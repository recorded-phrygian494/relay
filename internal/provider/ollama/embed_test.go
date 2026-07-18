package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llmrelay/relay/internal/provider"
)

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "nomic-embed-text" || len(req.Input) != 2 {
			t.Errorf("upstream saw %+v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":             req.Model,
			"embeddings":        [][]float32{{0.1, 0.2}, {0.3, 0.4}},
			"prompt_eval_count": 9,
		})
	}))
	defer srv.Close()

	c := New("ollama", srv.URL, nil)
	resp, err := c.Embed(context.Background(), &provider.EmbedRequest{
		Model: "nomic-embed-text", Input: []string{"a", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Vectors) != 2 || resp.Vectors[1][0] != 0.3 || resp.TokensIn != 9 {
		t.Fatalf("resp: %+v", resp)
	}
}

func TestEmbedRejectsDimensions(t *testing.T) {
	c := New("ollama", "http://127.0.0.1:0", nil)
	_, err := c.Embed(context.Background(), &provider.EmbedRequest{
		Model: "m", Input: []string{"a"}, Dimensions: 128,
	})
	if err == nil {
		t.Fatal("want error for dimensions")
	}
}
