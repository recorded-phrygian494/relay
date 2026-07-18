package store

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ModelStats is one row of the `relay stats` summary.
type ModelStats struct {
	Provider  string
	Model     string
	Requests  int64
	Errors    int64
	TokensIn  int64
	TokensOut int64
	CostUSD   float64
	Unpriced  int64 // requests with no pricing entry — CostUSD is incomplete
	P50MS     int64
	P95MS     int64
}

// Stats summarizes traffic since the given time, grouped by provider+model.
func Stats(db *sql.DB, since time.Time) ([]ModelStats, error) {
	rows, err := db.Query(`
		SELECT COALESCE(provider,''), COALESCE(model_served, model_requested),
		       COUNT(*),
		       SUM(CASE WHEN status >= 400 OR status = 0 THEN 1 ELSE 0 END),
		       COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0),
		       COALESCE(SUM(cost_usd),0),
		       SUM(cost_usd IS NULL AND COALESCE(provider,'') != '')
		FROM requests WHERE ts >= ?
		GROUP BY 1, 2 ORDER BY 3 DESC`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelStats
	for rows.Next() {
		var m ModelStats
		if err := rows.Scan(&m.Provider, &m.Model, &m.Requests, &m.Errors,
			&m.TokensIn, &m.TokensOut, &m.CostUSD, &m.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		p50, p95, err := latencyPercentiles(db, out[i].Provider, out[i].Model, since)
		if err != nil {
			return nil, err
		}
		out[i].P50MS, out[i].P95MS = p50, p95
	}
	return out, nil
}

// latencyPercentiles computes p50/p95 in Go over a bounded recent sample
// (SQLite has no percentile function).
func latencyPercentiles(db *sql.DB, providerName, model string, since time.Time) (int64, int64, error) {
	rows, err := db.Query(`
		SELECT latency_ms FROM requests
		WHERE ts >= ? AND COALESCE(provider,'') = ?
		  AND COALESCE(model_served, model_requested) = ?
		  AND latency_ms IS NOT NULL AND status < 400 AND status > 0
		ORDER BY ts DESC LIMIT 10000`, since.UnixMilli(), providerName, model)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var lat []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return 0, 0, err
		}
		lat = append(lat, v)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(lat) == 0 {
		return 0, 0, nil
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	// Nearest-rank with ceiling: conservative (never under-reports) and
	// stable for small samples.
	pct := func(p float64) int64 {
		idx := int(math.Ceil(p * float64(len(lat)-1)))
		return lat[idx]
	}
	return pct(0.50), pct(0.95), nil
}

// FormatTable renders stats as an aligned text table for the CLI.
func FormatTable(stats []ModelStats) string {
	if len(stats) == 0 {
		return "no requests logged in this window\n"
	}
	out := fmt.Sprintf("%-14s %-34s %8s %6s %12s %12s %10s %8s %8s\n",
		"PROVIDER", "MODEL", "REQS", "ERRS", "TOKENS_IN", "TOKENS_OUT", "COST_USD", "P50_MS", "P95_MS")
	var unpriced []string
	for _, m := range stats {
		cost := fmt.Sprintf("%10.4f", m.CostUSD)
		if m.Unpriced > 0 {
			cost = fmt.Sprintf("%10s", "—")
			unpriced = append(unpriced, fmt.Sprintf("%s/%s (%d reqs)", m.Provider, m.Model, m.Unpriced))
		}
		out += fmt.Sprintf("%-14s %-34s %8d %6d %12d %12d %s %8d %8d\n",
			m.Provider, m.Model, m.Requests, m.Errors, m.TokensIn, m.TokensOut, cost, m.P50MS, m.P95MS)
	}
	if len(unpriced) > 0 {
		out += "\nunpriced (no pricing.json entry — cost totals are incomplete): " +
			strings.Join(unpriced, ", ") + "\n"
	}
	return out
}
