package smart

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeEmbed maps texts to tiny deterministic vectors: hard-looking texts
// point one way, easy ones the other.
func fakeEmbed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		hard := float32(0)
		if strings.Contains(t, "prove") || strings.Contains(t, "derive") || strings.Contains(t, "goroutine") {
			hard = 1
		}
		out[i] = []float32{hard, 1 - hard, 0.1}
	}
	return out, nil
}

func knnWithRefs(t *testing.T) *KNN {
	t.Helper()
	lex, err := NewLexical()
	if err != nil {
		t.Fatal(err)
	}
	refs := []Ref{}
	for i := 0; i < 5; i++ {
		refs = append(refs,
			Ref{ID: fmt.Sprintf("hard-%d", i), Difficulty: 0.8, Domain: "math", Vector: []float32{1, 0, 0.1}},
			Ref{ID: fmt.Sprintf("easy-%d", i), Difficulty: 0.1, Domain: "chitchat", Vector: []float32{0, 1, 0.1}},
		)
	}
	return NewKNN(fakeEmbed, refs, lex)
}

func TestKNNClassify(t *testing.T) {
	k := knnWithRefs(t)
	d, err := k.Classify(context.Background(), userReq("prove the pigeonhole principle"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Class != "hard" || d.Domain != "math" {
		t.Fatalf("hard query: %+v", d)
	}
	if !strings.Contains(d.Why, "knn:") || !strings.Contains(d.Why, "neighbors") {
		t.Fatalf("why must name neighbors: %s", d.Why)
	}
	if d.Vector == nil {
		t.Fatal("tier-2 decision must carry the query vector for log_prompts=embeddings")
	}

	d, _ = k.Classify(context.Background(), userReq("hey what's up"))
	if d.Class != "easy" {
		t.Fatalf("easy query: %+v", d)
	}
}

func TestKNNFallsBackToLexical(t *testing.T) {
	lex, _ := NewLexical()

	// Not ready: no refs.
	k := NewKNN(fakeEmbed, nil, lex)
	d, err := k.Classify(context.Background(), userReq("prove the pigeonhole principle"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Why, "reference set not ready") || !strings.Contains(d.Why, "lexical") {
		t.Fatalf("fallback must say why: %s", d.Why)
	}
	if d.Class != "hard" {
		t.Fatalf("lexical fallback should still classify the proof hard: %+v", d)
	}

	// Embedder failure.
	k = knnWithRefs(t)
	k.Embed = func(context.Context, []string) ([][]float32, error) { return nil, fmt.Errorf("ollama is down") }
	d, err = k.Classify(context.Background(), userReq("prove the pigeonhole principle"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Why, "embed failed") || !strings.Contains(d.Why, "ollama is down") {
		t.Fatalf("fallback must carry the embed error: %s", d.Why)
	}
}

func TestSeedRefsLoadAndAreLabeled(t *testing.T) {
	refs, err := SeedRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) < 30 {
		t.Fatalf("seed set too small: %d", len(refs))
	}
	var hard, easy int
	for _, r := range refs {
		if r.Text == "" || r.Domain == "" || r.Source != "seed" {
			t.Fatalf("incomplete seed ref: %+v", r)
		}
		if r.Difficulty >= 0.45 {
			hard++
		} else {
			easy++
		}
	}
	if hard < 10 || easy < 10 {
		t.Fatalf("seed set is unbalanced: %d hard / %d easy", hard, easy)
	}
}
