package gemini

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
		if r.URL.Path != "/models/gemini-embedding-001:batchEmbedContents" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Requests []struct {
				Model                string `json:"model"`
				OutputDimensionality int    `json:"outputDimensionality"`
				Content              struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"requests"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Requests) != 2 || req.Requests[0].Model != "models/gemini-embedding-001" ||
			req.Requests[0].Content.Parts[0].Text != "a" || req.Requests[1].OutputDimensionality != 256 {
			t.Errorf("upstream saw %+v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": []map[string]any{
				{"values": []float32{0.1, 0.2}},
				{"values": []float32{0.3, 0.4}},
			},
		})
	}))
	defer srv.Close()

	c := New("gemini", srv.URL, "key", nil)
	resp, err := c.Embed(context.Background(), &provider.EmbedRequest{
		Model: "gemini-embedding-001", Input: []string{"a", "b"}, Dimensions: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Vectors) != 2 || resp.Vectors[0][1] != 0.2 {
		t.Fatalf("resp: %+v", resp)
	}
}
