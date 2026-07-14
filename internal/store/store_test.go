package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreLogAndStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, lat := range []int64{10, 20, 30, 40, 1000} {
		s.Log(Record{
			ID:             "r" + string(rune('a'+i)),
			TS:             now,
			API:            "openai",
			ModelRequested: "gpt-4o-mini",
			ModelServed:    "gpt-4o-mini",
			Provider:       "openai",
			RoutePolicy:    "static",
			Status:         200,
			TokensIn:       100,
			TokensOut:      50,
			LatencyMS:      lat,
		})
	}
	s.Log(Record{ID: "err1", TS: now, API: "openai", ModelRequested: "gpt-4o-mini",
		ModelServed: "gpt-4o-mini", Provider: "openai", Status: 429, ErrorCode: "rate_limited"})
	if err := s.Close(); err != nil { // Close drains the async queue
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	stats, err := Stats(s2.DB(), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("want 1 group, got %d", len(stats))
	}
	m := stats[0]
	if m.Requests != 6 || m.Errors != 1 {
		t.Fatalf("requests/errors: %+v", m)
	}
	if m.TokensIn != 500 || m.TokensOut != 250 {
		t.Fatalf("tokens: %+v", m)
	}
	if m.P50MS != 30 || m.P95MS != 1000 {
		t.Fatalf("percentiles: p50=%d p95=%d", m.P50MS, m.P95MS)
	}
}

func TestStoreNeverBlocks(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50_000; i++ {
			s.Log(Record{ID: "x", TS: time.Now(), API: "openai", ModelRequested: "m"})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Log blocked under burst load")
	}
}
