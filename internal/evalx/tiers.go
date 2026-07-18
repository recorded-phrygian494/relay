package evalx

import (
	"context"
	"fmt"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/smart"
)

// TierPolicy adapts a smart classifier to the harness using the shipped
// smart-config shape: easy → cheap chain, hard → frontier chain.
type TierPolicy struct {
	PolicyName string
	Classifier smart.Classifier
}

// Name implements Policy.
func (t TierPolicy) Name() string { return t.PolicyName }

// Decide implements Policy.
func (t TierPolicy) Decide(ctx context.Context, req *core.Request, cands Candidates) (ModelRef, string, error) {
	d, err := t.Classifier.Classify(ctx, req)
	if err != nil {
		return ModelRef{}, "", err
	}
	band := BandCheap
	if d.Class == "hard" {
		band = BandFrontier
	}
	ref, ok := cands[band]
	if !ok {
		return ModelRef{}, "", fmt.Errorf("no %s candidate configured", band)
	}
	return ref, d.Why, nil
}
