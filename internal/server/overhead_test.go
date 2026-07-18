package server

import (
	"bufio"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// DESIGN §11: the benchmark gate. Relay's added latency over a loopback
// mock upstream must stay under 5ms p50 non-streaming and 2ms p50 added
// TTFT. Budgets scale under -race, which slows everything severalfold.

func p50(durs []time.Duration) time.Duration {
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return durs[len(durs)/2]
}

func doOne(t *testing.T, url, body string, stream bool) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if stream {
		// TTFT: stop at the first response byte.
		if _, err := bufio.NewReader(resp.Body).ReadByte(); err != nil {
			t.Fatal(err)
		}
		return
	}
	buf := make([]byte, 4096)
	for {
		if _, err := resp.Body.Read(buf); err != nil {
			break
		}
	}
}

// timeRequests returns the per-request average across `batches` batches of
// `perBatch` sequential requests. Batching beats the Windows monotonic
// timer's ~0.5ms granularity: per-batch totals are far above it, so the
// division recovers microsecond resolution. (For streams the drain stops
// at the first byte, so the batch total is a sum of TTFTs.)
func timeRequests(t *testing.T, batches, perBatch int, url, body string, stream bool) []time.Duration {
	t.Helper()
	out := make([]time.Duration, 0, batches)
	for b := 0; b < batches; b++ {
		start := time.Now()
		for i := 0; i < perBatch; i++ {
			doOne(t, url, body, stream)
		}
		out = append(out, time.Since(start)/time.Duration(perBatch))
	}
	return out
}

func TestOverheadBudget(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	gw, _ := newTestGateway(t, up.URL, nil)

	nonStreamBudget, ttftBudget := 5*time.Millisecond, 2*time.Millisecond
	if raceEnabled {
		nonStreamBudget *= 5
		ttftBudget *= 5
	}

	const batches, perBatch = 20, 30
	directBody := `{"model":"test-model","messages":[{"role":"user","content":"ping"}]}`
	streamRelay := `{"model":"mock/test-model","messages":[{"role":"user","content":"ping"}],"stream":true}`
	streamDirect := `{"model":"test-model","messages":[{"role":"user","content":"ping"}],"stream":true}`

	timeRequests(t, 1, 30, up.URL+"/chat/completions", directBody, false) // warm-up
	timeRequests(t, 1, 30, gw.URL+"/v1/chat/completions", chatBody, false)

	direct := p50(timeRequests(t, batches, perBatch, up.URL+"/chat/completions", directBody, false))
	relay := p50(timeRequests(t, batches, perBatch, gw.URL+"/v1/chat/completions", chatBody, false))
	overhead := relay - direct
	t.Logf("non-streaming p50: direct=%v relay=%v overhead=%v (budget %v)", direct, relay, overhead, nonStreamBudget)
	if overhead > nonStreamBudget {
		t.Errorf("non-streaming p50 overhead %v exceeds %v", overhead, nonStreamBudget)
	}

	timeRequests(t, 1, 30, up.URL+"/chat/completions", streamDirect, true) // warm-up
	timeRequests(t, 1, 30, gw.URL+"/v1/chat/completions", streamRelay, true)

	directTTFT := p50(timeRequests(t, batches, perBatch, up.URL+"/chat/completions", streamDirect, true))
	relayTTFT := p50(timeRequests(t, batches, perBatch, gw.URL+"/v1/chat/completions", streamRelay, true))
	added := relayTTFT - directTTFT
	t.Logf("TTFT p50: direct=%v relay=%v added=%v (budget %v)", directTTFT, relayTTFT, added, ttftBudget)
	if added > ttftBudget {
		t.Errorf("added TTFT p50 %v exceeds %v", added, ttftBudget)
	}
}
