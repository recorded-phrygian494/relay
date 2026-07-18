package smart

import (
	"context"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/evalx"
)

func userReq(text string) *core.Request {
	return &core.Request{Messages: []core.Message{
		{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: text}}},
	}}
}

func TestLexicalExplainability(t *testing.T) {
	l, err := NewLexical()
	if err != nil {
		t.Fatal(err)
	}
	d, err := l.Classify(context.Background(), userReq("Prove that the square root of 2 is irrational."))
	if err != nil {
		t.Fatal(err)
	}
	if d.Class != "hard" || d.Domain != "math" {
		t.Fatalf("proof classified %s/%s: %+v", d.Class, d.Domain, d)
	}
	// The Why string must name the evidence, not just assert the outcome.
	for _, want := range []string{"prove", "difficulty", "threshold"} {
		if !strings.Contains(d.Why, want) {
			t.Fatalf("why lacks %q: %s", want, d.Why)
		}
	}

	d, _ = l.Classify(context.Background(), userReq("hey! how's it going?"))
	if d.Class != "easy" || d.Domain != "chitchat" {
		t.Fatalf("greeting classified %s/%s: %s", d.Class, d.Domain, d.Why)
	}
}

func TestLexicalDeterminism(t *testing.T) {
	l, _ := NewLexical()
	req := userReq("Write a Python function that merges overlapping intervals.")
	a, _ := l.Classify(context.Background(), req)
	b, _ := l.Classify(context.Background(), req)
	if a.Difficulty != b.Difficulty || a.Class != b.Class || a.Why != b.Why {
		t.Fatalf("non-deterministic: %+v vs %+v", a, b)
	}
}

// TestLexicalCalibrationFloor pins tier-1 quality against the committed
// eval set: no hard row (label >= 0.55) may classify easy — that misroute
// sends hard traffic to a cheap model, the expensive failure — and total
// misses stay bounded. Calibration was hand-tuned against this set
// (documented in tier1_weights.json); this test keeps regressions out.
func TestLexicalCalibrationFloor(t *testing.T) {
	set, err := evalx.LoadSet("../../assets/eval/evalset_v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	l, _ := NewLexical()
	misses, unsafe := 0, 0
	for _, row := range set.Rows {
		d, err := l.Classify(context.Background(), userReq(row.Prompt))
		if err != nil {
			t.Fatal(err)
		}
		wantHard := row.Difficulty >= l.Weights.Threshold
		gotHard := d.Class == "hard"
		if wantHard != gotHard {
			misses++
			if row.Difficulty >= 0.55 && !gotHard {
				unsafe++
				t.Errorf("hard row %s (label %.2f) classified easy: %s", row.ID, row.Difficulty, d.Why)
			}
		}
	}
	if unsafe > 0 {
		t.Fatalf("%d hard rows would route to the cheap chain", unsafe)
	}
	if misses > 3 {
		t.Fatalf("calibration drifted: %d misses (max 3)", misses)
	}
}
