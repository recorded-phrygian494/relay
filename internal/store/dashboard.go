package store

import (
	"database/sql"
	"sort"
	"time"
)

// SpendRow is one (day, provider, model) aggregate for the dashboard.
type SpendRow struct {
	Day       string  `json:"day"`
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	Requests  int     `json:"requests"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	// Unpriced: some requests in this row have no pricing entry (cost
	// logged NULL) — CostUSD is incomplete and must not render as $0.
	Unpriced bool `json:"unpriced"`
}

// SpendByDay aggregates served traffic since the cutoff.
func SpendByDay(db *sql.DB, since time.Time) ([]SpendRow, error) {
	rows, err := db.Query(`
		SELECT date(ts/1000, 'unixepoch') AS day, provider, model_served,
		       COUNT(*), SUM(tokens_in), SUM(tokens_out), SUM(cost_usd),
		       SUM(cost_usd IS NULL)
		FROM requests
		WHERE ts >= ? AND provider != ''
		GROUP BY day, provider, model_served
		ORDER BY day DESC, SUM(cost_usd) DESC`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SpendRow{}
	for rows.Next() {
		var r SpendRow
		var tokIn, tokOut, unpriced sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&r.Day, &r.Provider, &r.Model, &r.Requests, &tokIn, &tokOut, &cost, &unpriced); err != nil {
			return nil, err
		}
		r.TokensIn, r.TokensOut, r.CostUSD = tokIn.Int64, tokOut.Int64, cost.Float64
		r.Unpriced = unpriced.Int64 > 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatencyRow summarizes latency percentiles for one (provider, model).
type LatencyRow struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Requests int    `json:"requests"`
	P50MS    int64  `json:"p50_ms"`
	P95MS    int64  `json:"p95_ms"`
	P99MS    int64  `json:"p99_ms"`
	TTFTP50  int64  `json:"ttft_p50_ms"`
}

// LatencyStats computes percentiles in Go over recent successful requests
// (capped, newest first — exact enough for a dashboard).
func LatencyStats(db *sql.DB, since time.Time) ([]LatencyRow, error) {
	rows, err := db.Query(`
		SELECT provider, model_served, latency_ms, ttft_ms
		FROM requests
		WHERE ts >= ? AND provider != '' AND status = 200
		ORDER BY ts DESC LIMIT 20000`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type samples struct{ lat, ttft []int64 }
	groups := map[[2]string]*samples{}
	for rows.Next() {
		var prov, model string
		var lat, ttft sql.NullInt64
		if err := rows.Scan(&prov, &model, &lat, &ttft); err != nil {
			return nil, err
		}
		key := [2]string{prov, model}
		g, ok := groups[key]
		if !ok {
			g = &samples{}
			groups[key] = g
		}
		g.lat = append(g.lat, lat.Int64)
		if ttft.Int64 > 0 {
			g.ttft = append(g.ttft, ttft.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []LatencyRow{}
	for key, g := range groups {
		sort.Slice(g.lat, func(i, j int) bool { return g.lat[i] < g.lat[j] })
		sort.Slice(g.ttft, func(i, j int) bool { return g.ttft[i] < g.ttft[j] })
		out = append(out, LatencyRow{
			Provider: key[0], Model: key[1], Requests: len(g.lat),
			P50MS: percentile(g.lat, 0.50), P95MS: percentile(g.lat, 0.95),
			P99MS: percentile(g.lat, 0.99), TTFTP50: percentile(g.ttft, 0.50),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return out, nil
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// DecisionRow is one recent routing decision, reason verbatim — the
// explainability story (DESIGN §4.2).
type DecisionRow struct {
	TS             string  `json:"ts"`
	API            string  `json:"api"`
	ModelRequested string  `json:"model_requested"`
	Provider       string  `json:"provider"`
	ModelServed    string  `json:"model_served"`
	Policy         string  `json:"policy"`
	Reason         string  `json:"reason"`
	Attempts       int      `json:"attempts"`
	Status         int      `json:"status"`
	LatencyMS      int64    `json:"latency_ms"`
	CostUSD        *float64 `json:"cost_usd"` // null = unpriced model
}

// RecentDecisions returns the newest routing decisions.
func RecentDecisions(db *sql.DB, limit int) ([]DecisionRow, error) {
	rows, err := db.Query(`
		SELECT ts, api, model_requested, provider, model_served,
		       route_policy, route_reason, attempts, status, latency_ms, cost_usd
		FROM requests ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DecisionRow{}
	for rows.Next() {
		var r DecisionRow
		var ts int64
		var prov, served, policy, reason sql.NullString
		var attempts, status sql.NullInt64
		var lat sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&ts, &r.API, &r.ModelRequested, &prov, &served,
			&policy, &reason, &attempts, &status, &lat, &cost); err != nil {
			return nil, err
		}
		r.TS = time.UnixMilli(ts).UTC().Format(time.RFC3339)
		r.Provider, r.ModelServed = prov.String, served.String
		r.Policy, r.Reason = policy.String, reason.String
		r.Attempts, r.Status = int(attempts.Int64), int(status.Int64)
		r.LatencyMS = lat.Int64
		if cost.Valid {
			c := cost.Float64
			r.CostUSD = &c
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
