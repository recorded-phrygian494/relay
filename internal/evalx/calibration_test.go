package evalx

import (
	"context"
	"testing"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/smart"
)

// TestLexicalCalibrationFloor pins tier-1 quality against the committed
// eval set: no hard row (label >= 0.55) may classify easy — that misroute
// sends hard traffic to a cheap model, the expensive failure — and total
// misses stay bounded. Calibration was hand-tuned against this set
// (documented in tier1_weights.json); this test keeps regressions out.
// It lives in evalx (not smart) to avoid a test-only import cycle.
func TestLexicalCalibrationFloor(t *testing.T) {
	set, err := LoadSet("../../assets/eval/evalset_v1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	l, err := smart.NewLexical()
	if err != nil {
		t.Fatal(err)
	}
	misses, unsafe := 0, 0
	for _, row := range set.Rows {
		req := &core.Request{Messages: []core.Message{
			{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: row.Prompt}}},
		}}
		d, err := l.Classify(context.Background(), req)
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
