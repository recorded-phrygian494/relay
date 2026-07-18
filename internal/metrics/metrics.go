// Package metrics is the Prometheus registry (DESIGN §9): request counts,
// latency/TTFT histograms, token and cost counters, breaker state, and
// log-queue drops. One Metrics instance lives for the process; hot reload
// does not reset it.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every relay collector.
type Metrics struct {
	registry *prometheus.Registry

	Requests  *prometheus.CounterVec
	Latency   *prometheus.HistogramVec
	TTFT      *prometheus.HistogramVec
	TokensIn  *prometheus.CounterVec
	TokensOut *prometheus.CounterVec
	CostUSD   *prometheus.CounterVec
	Breakers    *prometheus.GaugeVec
	KeysCooling *prometheus.GaugeVec
	LogDrops    prometheus.CounterFunc
}

// New builds the registry. droppedFn reports the store's dropped-record
// count (nil for none).
func New(droppedFn func() float64) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_requests_total",
			Help: "Requests by provider, model, policy, and HTTP status.",
		}, []string{"provider", "model", "policy", "status"}),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relay_request_duration_seconds",
			Help:    "End-to-end request latency.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"provider", "model"}),
		TTFT: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relay_ttft_seconds",
			Help:    "Time to first streamed content event.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"provider", "model"}),
		TokensIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_tokens_input_total",
			Help: "Input tokens by provider and model.",
		}, []string{"provider", "model"}),
		TokensOut: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_tokens_output_total",
			Help: "Output tokens by provider and model.",
		}, []string{"provider", "model"}),
		CostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_cost_usd_total",
			Help: "Estimated spend in USD by provider and model.",
		}, []string{"provider", "model"}),
		Breakers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "relay_circuit_open",
			Help: "1 when the circuit breaker for provider/model is open.",
		}, []string{"provider", "model"}),
		KeysCooling: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "relay_keys_cooling",
			Help: "API keys currently in rate-limit cooldown per provider.",
		}, []string{"provider"}),
	}
	reg.MustRegister(m.Requests, m.Latency, m.TTFT, m.TokensIn, m.TokensOut, m.CostUSD, m.Breakers, m.KeysCooling)
	reg.MustRegister(collectors.NewGoCollector())
	if droppedFn != nil {
		m.LogDrops = prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "relay_log_dropped_records_total",
			Help: "Request-log records dropped to backpressure or write errors.",
		}, droppedFn)
		reg.MustRegister(m.LogDrops)
	}
	return m
}

// Handler serves the /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
