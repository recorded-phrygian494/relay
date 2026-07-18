// Package evalx is the routing eval harness (DESIGN §0.3): it replays a
// labeled eval set through routing policies without live traffic (dry-run
// against the catalog and pricing registry) and reports cost, quality, and
// per-query decisions, with a machine-readable verdict. The harness exists
// *before* the smart router so the router is judged by an instrument that
// was not built to flatter it.
package evalx

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Band is a model capability band. Eval quality labels are per-band, so
// the set stays independent of concrete model ids (which the eval config
// maps to bands).
type Band string

const (
	BandCheap    Band = "cheap"
	BandMid      Band = "mid"
	BandFrontier Band = "frontier"
)

// Row is one labeled eval query.
type Row struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Domain   string `json:"domain"`   // chitchat | qa | extraction | summarize | code | math | reasoning
	Category string `json:"category"` // generator template family, incl. adversarial ones
	// Difficulty is the labeled difficulty in [0,1].
	Difficulty float64 `json:"difficulty"`
	// OutTokens is the expected completion size, for cost simulation.
	OutTokens int `json:"out_tokens"`
	// Quality is the labeled per-band quality in [0,1]. Synthetic — see
	// assets/eval/README.md for the provenance and its limits.
	Quality map[Band]float64 `json:"quality"`
}

// Set is a versioned eval set.
type Set struct {
	Version string `json:"version"`
	Seed    int64  `json:"seed"`
	Rows    []Row  `json:"-"`
}

// LoadSet reads a JSONL eval set: first line is the header object
// (version, seed), each following line one Row.
func LoadSet(path string) (*Set, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	if !sc.Scan() {
		return nil, fmt.Errorf("%s: empty eval set", path)
	}
	var s Set
	if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
		return nil, fmt.Errorf("%s header: %w", path, err)
	}
	if s.Version == "" {
		return nil, fmt.Errorf("%s: header has no version", path)
	}
	line := 1
	for sc.Scan() {
		line++
		var r Row
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		if r.ID == "" || r.Prompt == "" || len(r.Quality) == 0 {
			return nil, fmt.Errorf("%s line %d: incomplete row", path, line)
		}
		s.Rows = append(s.Rows, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return &s, nil
}
