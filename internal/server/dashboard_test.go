package server

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llmrelay/relay/internal/config"
	"github.com/llmrelay/relay/internal/reliability"
	"github.com/llmrelay/relay/internal/store"
)

func dashboardServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	base := time.Now().Add(-time.Hour)
	for i, rec := range []store.Record{
		{API: "openai", ModelRequested: "fast", Provider: "groq", ModelServed: "llama-3.3-70b",
			RoutePolicy: "alias", RouteReason: "alias \"fast\" → groq/llama-3.3-70b (rank 1)",
			Attempts: 1, Status: 200, TokensIn: 100, TokensOut: 50, CostUSD: 0.0001,
			LatencyMS: 400, TTFTMS: 120, Stream: true},
		{API: "openai", ModelRequested: "fast", Provider: "openai", ModelServed: "gpt-4o-mini",
			RoutePolicy: "alias", RouteReason: "alias \"fast\" → openai/gpt-4o-mini (rank 2) [warning: multi_turn_tools=degraded — thought-signature replay may be rejected; DESIGN §0.7]",
			Attempts: 2, Status: 200, TokensIn: 200, TokensOut: 80, CostUSD: 0.00008,
			LatencyMS: 900},
		{API: "anthropic", ModelRequested: "claude-haiku-4-5", Status: 404, RoutePolicy: "static"},
	} {
		rec.ID = "req_" + strings.Repeat("x", i+1)
		rec.TS = base.Add(time.Duration(i) * time.Minute)
		st.Log(rec)
	}
	// Drain the async writer so queries see the rows.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		_ = st.DB().QueryRow("SELECT COUNT(*) FROM requests").Scan(&n)
		if n == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	pool := reliability.NewKeyPool([]string{"k1", "k2"})
	pool.Cooldown("k1", time.Minute)
	rt := &Runtime{Config: &config.Config{},
		Exec: reliability.NewExecutor(nil, map[string]*reliability.KeyPool{"groq": pool}, 0, 0)}
	return New(rt, st, "test")
}

func TestDashboardData(t *testing.T) {
	s := dashboardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard/data", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Version string             `json:"version"`
		Spend   []store.SpendRow   `json:"spend"`
		Latency []store.LatencyRow `json:"latency"`
		Recent  []store.DecisionRow `json:"recent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != "test" {
		t.Fatalf("version: %q", d.Version)
	}
	if len(d.Spend) != 2 {
		t.Fatalf("spend rows: %+v", d.Spend)
	}
	if len(d.Latency) != 2 {
		t.Fatalf("latency rows: %+v", d.Latency)
	}
	// Failed request (no provider) is excluded from spend/latency but shows
	// in recent decisions.
	if len(d.Recent) != 3 {
		t.Fatalf("recent rows: %d", len(d.Recent))
	}
	// The §0.7 warning must survive verbatim into the recent-decisions feed.
	found := false
	for _, r := range d.Recent {
		if strings.Contains(r.Reason, "multi_turn_tools=degraded") {
			found = true
		}
	}
	if !found {
		t.Fatal("degraded-tools warning not surfaced in recent decisions")
	}
}

func TestDashboardPageAndMetrics(t *testing.T) {
	s := dashboardServer(t)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "relay dashboard") {
		t.Fatalf("dashboard page: status %d", rec.Code)
	}

	// Record a request through observe, then scrape.
	s.observe(&store.Record{Provider: "groq", ModelServed: "llama-3.3-70b",
		RoutePolicy: "alias", Status: 200, LatencyMS: 350, TTFTMS: 90,
		TokensIn: 10, TokensOut: 5, CostUSD: 0.001})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics: status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`relay_requests_total{model="llama-3.3-70b",policy="alias",provider="groq",status="200"} 1`,
		"relay_tokens_input_total",
		"relay_cost_usd_total",
		"relay_log_dropped_records_total",
		`relay_keys_cooling{provider="groq"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q", want)
		}
	}
}
