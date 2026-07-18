package smart

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"

	"github.com/llmrelay/relay/internal/core"
)

// Decision is one classifier verdict. Why names the evidence — it is
// logged verbatim into the decisions log (explainability is
// non-negotiable, DESIGN phase-4 brief).
type Decision struct {
	Difficulty float64 // [0,1]
	Class      string  // "easy" | "hard"
	Domain     string  // chitchat | qa | extraction | summarize | code | math | reasoning
	Confidence float64 // [0.5,1] — distance from the threshold
	Why        string
	// Vector is the query embedding when tier 2 produced this decision
	// (consumed by log_prompts=embeddings); nil for tier 1.
	Vector []float32
}

// Classifier maps a request to a Decision.
type Classifier interface {
	Name() string
	Classify(ctx context.Context, req *core.Request) (Decision, error)
}

//go:embed tier1_weights.json
var embeddedWeights []byte

// Weights parameterize the tier-1 linear model. Shipped via go:embed,
// overridable for experiments and future relay train updates.
type Weights struct {
	Version   string             `json:"version"`
	Threshold float64            `json:"threshold"`
	Base      float64            `json:"base"`
	W         map[string]float64 `json:"weights"`
}

// DefaultWeights returns the embedded calibration.
func DefaultWeights() (*Weights, error) {
	var w Weights
	if err := json.Unmarshal(embeddedWeights, &w); err != nil {
		return nil, fmt.Errorf("embedded tier1 weights: %w", err)
	}
	return &w, nil
}

// Lexical is the tier-1 classifier: a transparent linear model over the
// named Features. Zero external calls, deterministic.
type Lexical struct{ Weights *Weights }

// NewLexical builds the tier-1 classifier with embedded weights.
func NewLexical() (*Lexical, error) {
	w, err := DefaultWeights()
	if err != nil {
		return nil, err
	}
	return &Lexical{Weights: w}, nil
}

// Name implements Classifier.
func (l *Lexical) Name() string { return "lexical" }

// Classify implements Classifier.
func (l *Lexical) Classify(_ context.Context, req *core.Request) (Decision, error) {
	f := Featurize(req)
	w := l.Weights.W
	score := l.Weights.Base
	score += float64(f.Words) / 100 * w["per_100_words"]
	score += float64(f.CodeFences) * w["code_fence"]
	score += float64(f.CodeHits) * w["code_hit"]
	score += float64(f.MathHits) * w["math_hit"]
	score += float64(f.ReasonHits) * w["reason_hit"]
	score += float64(f.Numbers) * w["number"]
	score += float64(f.EasyHits) * w["easy_hit"]
	score += float64(f.ExtractHits) * w["extract_hit"]
	score += float64(f.SummaryHits) * w["summary_hit"]
	if f.QuestionOnly {
		score += w["short_question"]
	}
	if f.JSONMode {
		score += w["json_mode"]
	}
	score += float64(f.ToolCount) * w["tool"]
	if f.MsgDepth > 4 {
		score += float64(f.MsgDepth-4) * w["depth_over_4"]
	}
	score = math.Max(0, math.Min(1, score))

	class := "easy"
	if score >= l.Weights.Threshold {
		class = "hard"
	}
	conf := 0.5 + math.Min(0.5, math.Abs(score-l.Weights.Threshold)*2)
	d := Decision{
		Difficulty: math.Round(score*1000) / 1000,
		Class:      class,
		Domain:     domain(f),
		Confidence: math.Round(conf*100) / 100,
	}
	d.Why = fmt.Sprintf("lexical: %s → difficulty %.2f (%s, conf %.2f, threshold %.2f)",
		f.explain(), d.Difficulty, d.Class, d.Confidence, l.Weights.Threshold)
	return d, nil
}

// domain picks the dominant feature family.
func domain(f Features) string {
	type vote struct {
		name string
		n    int
	}
	votes := []vote{
		{"code", f.CodeHits + 3*f.CodeFences},
		{"math", f.MathHits},
		{"reasoning", f.ReasonHits},
		{"extraction", f.ExtractHits},
		{"summarize", f.SummaryHits},
		{"chitchat", f.EasyHits},
	}
	best := vote{"qa", 0}
	for _, v := range votes {
		if v.n > best.n {
			best = v
		}
	}
	return best.name
}
