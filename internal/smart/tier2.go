package smart

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/llmrelay/relay/internal/core"
)

// Ref is one labeled reference point for the KNN tier.
type Ref struct {
	ID         string    `json:"id"`
	Text       string    `json:"text,omitempty"` // absent when built from log embeddings (log_prompts: embeddings)
	Difficulty float64   `json:"difficulty"`
	Domain     string    `json:"domain"`
	Source     string    `json:"source,omitempty"` // seed | implicit | judge | feedback
	Vector     []float32 `json:"vector,omitempty"`
}

// RefStore is the persisted reference set: vectors are only meaningful
// within one embedder's space, so the store records which one built it.
type RefStore struct {
	Embedder string `json:"embedder"` // "provider/model"
	Refs     []Ref  `json:"refs"`
}

//go:embed seed_refs.jsonl
var seedRefsRaw []byte

// SeedRefs returns the embedded seed reference texts (synthetic, disjoint
// from the eval set — provenance in assets/eval/README.md). Vectors are
// not shipped: they are embedder-specific and get computed by relay train
// or lazily at startup.
func SeedRefs() ([]Ref, error) {
	var out []Ref
	sc := bufio.NewScanner(bytes.NewReader(seedRefsRaw))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r Ref
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("seed refs: %w", err)
		}
		r.Source = "seed"
		out = append(out, r)
	}
	return out, sc.Err()
}

// LoadRefStore reads a persisted reference set.
func LoadRefStore(path string) (*RefStore, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s RefStore
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// SaveRefStore persists a reference set atomically.
func SaveRefStore(path string, s *RefStore) error {
	raw, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EmbedFunc embeds a batch of texts. Bound to relay's own provider
// embedding path — the same plumbing /v1/embeddings uses.
type EmbedFunc func(ctx context.Context, texts []string) ([][]float32, error)

// KNN is the tier-2 classifier: cosine k-nearest-neighbors over the
// reference set in the configured embedder's space. Opt-in; on any
// embedding failure it falls back to the lexical tier and says so.
type KNN struct {
	Embed     EmbedFunc
	K         int
	Threshold float64 // difficulty >= Threshold routes hard
	Fallback  Classifier

	mu   sync.RWMutex
	refs []Ref // only entries with vectors
}

// NewKNN builds the tier-2 classifier. refs entries without vectors are
// ignored (they haven't been embedded yet).
func NewKNN(embed EmbedFunc, refs []Ref, fallback Classifier) *KNN {
	k := &KNN{Embed: embed, K: 5, Threshold: 0.45, Fallback: fallback}
	k.SetRefs(refs)
	return k
}

// SetRefs atomically replaces the reference set (startup build, train).
func (k *KNN) SetRefs(refs []Ref) {
	usable := make([]Ref, 0, len(refs))
	for _, r := range refs {
		if len(r.Vector) > 0 {
			usable = append(usable, r)
		}
	}
	k.mu.Lock()
	k.refs = usable
	k.mu.Unlock()
}

// Ready reports whether the reference set is usable.
func (k *KNN) Ready() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.refs) >= k.K
}

// Name implements Classifier.
func (k *KNN) Name() string { return "knn" }

// Classify implements Classifier.
func (k *KNN) Classify(ctx context.Context, req *core.Request) (Decision, error) {
	text := lastUserText(req)
	if !k.Ready() || text == "" {
		return k.fallback(ctx, req, "reference set not ready")
	}
	vecs, err := k.Embed(ctx, []string{text})
	if err != nil || len(vecs) != 1 {
		return k.fallback(ctx, req, fmt.Sprintf("embed failed (%v)", err))
	}
	query := vecs[0]

	type scored struct {
		ref Ref
		sim float64
	}
	k.mu.RLock()
	neighbors := make([]scored, 0, len(k.refs))
	for _, r := range k.refs {
		neighbors = append(neighbors, scored{r, cosine(query, r.Vector)})
	}
	k.mu.RUnlock()
	sort.Slice(neighbors, func(i, j int) bool { return neighbors[i].sim > neighbors[j].sim })
	if len(neighbors) > k.K {
		neighbors = neighbors[:k.K]
	}

	// Similarity-weighted difficulty; majority-vote domain.
	var num, den float64
	domains := map[string]int{}
	why := make([]string, 0, len(neighbors))
	for _, n := range neighbors {
		w := math.Max(n.sim, 1e-6)
		num += w * n.ref.Difficulty
		den += w
		domains[n.ref.Domain]++
		why = append(why, fmt.Sprintf("%s d=%.2f sim=%.2f", n.ref.ID, n.ref.Difficulty, n.sim))
	}
	difficulty := num / den
	class := "easy"
	if difficulty >= k.Threshold {
		class = "hard"
	}
	dom, domVotes := "qa", 0
	for name, votes := range domains {
		if votes > domVotes {
			dom, domVotes = name, votes
		}
	}
	conf := 0.5 + math.Min(0.5, math.Abs(difficulty-k.Threshold)*2)
	return Decision{
		Difficulty: math.Round(difficulty*1000) / 1000,
		Class:      class,
		Domain:     dom,
		Confidence: math.Round(conf*100) / 100,
		Why: fmt.Sprintf("knn: %d neighbors [%s] → difficulty %.2f (%s, conf %.2f, threshold %.2f)",
			len(neighbors), strings.Join(why, "; "), difficulty, class, conf, k.Threshold),
		Vector: query,
	}, nil
}

// fallback delegates to the lexical tier, recording why in the decision.
func (k *KNN) fallback(ctx context.Context, req *core.Request, reason string) (Decision, error) {
	if k.Fallback == nil {
		return Decision{}, fmt.Errorf("knn: %s and no fallback classifier", reason)
	}
	d, err := k.Fallback.Classify(ctx, req)
	if err != nil {
		return d, err
	}
	d.Why = fmt.Sprintf("knn: %s → lexical fallback | %s", reason, d.Why)
	return d, nil
}

func lastUserText(req *core.Request) string {
	var last string
	for _, m := range req.Messages {
		if m.Role != core.RoleUser {
			continue
		}
		var b strings.Builder
		for _, p := range m.Parts {
			if tp, ok := p.(core.TextPart); ok {
				b.WriteString(tp.Text)
				b.WriteString(" ")
			}
		}
		last = strings.TrimSpace(b.String())
	}
	return last
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
