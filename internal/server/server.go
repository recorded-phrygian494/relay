package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/llmrelay/relay/internal/api/openai"
	"github.com/llmrelay/relay/internal/dashboard"
	"github.com/llmrelay/relay/internal/metrics"
	"github.com/llmrelay/relay/internal/store"
)

// Server is the HTTP front end. The Runtime pointer is swapped atomically
// on config hot reload; metrics live for the process.
type Server struct {
	rt      atomic.Pointer[Runtime]
	store   *store.Store
	metrics *metrics.Metrics
	version string
}

// New builds a Server around an initial runtime. st may be nil (logging
// disabled, used by some tests).
func New(rt *Runtime, st *store.Store, version string) *Server {
	s := &Server{store: st, version: version}
	var droppedFn func() float64
	if st != nil {
		droppedFn = func() float64 { return float64(st.Dropped()) }
	}
	s.metrics = metrics.New(droppedFn)
	s.rt.Store(rt)
	return s
}

// observe records one finished request into Prometheus. Called from the
// same defer that writes the request log.
func (s *Server) observe(rec *store.Record) {
	provider, model := rec.Provider, rec.ModelServed
	if provider == "" {
		provider, model = "none", "none"
	}
	s.metrics.Requests.WithLabelValues(provider, model, rec.RoutePolicy, strconv.Itoa(rec.Status)).Inc()
	s.metrics.Latency.WithLabelValues(provider, model).Observe(float64(rec.LatencyMS) / 1000)
	if rec.TTFTMS > 0 {
		s.metrics.TTFT.WithLabelValues(provider, model).Observe(float64(rec.TTFTMS) / 1000)
	}
	if rec.TokensIn > 0 {
		s.metrics.TokensIn.WithLabelValues(provider, model).Add(float64(rec.TokensIn))
	}
	if rec.TokensOut > 0 {
		s.metrics.TokensOut.WithLabelValues(provider, model).Add(float64(rec.TokensOut))
	}
	switch {
	case rec.CostUSD != nil && *rec.CostUSD > 0:
		s.metrics.CostUSD.WithLabelValues(provider, model).Add(*rec.CostUSD)
	case rec.CostUSD == nil && rec.Provider != "":
		// Served but not in the pricing registry: totals are incomplete.
		s.metrics.Unpriced.WithLabelValues(provider, model).Inc()
	}
}

// refreshGauges snapshots breaker and key-pool state into gauges on scrape.
func (s *Server) refreshGauges() {
	rt := s.Runtime()
	if rt.Exec == nil {
		return
	}
	if rt.Exec.Breakers != nil {
		for key, open := range rt.Exec.Breakers.States() {
			prov, model, _ := strings.Cut(key, "/")
			v := 0.0
			if open {
				v = 1.0
			}
			s.metrics.Breakers.WithLabelValues(prov, model).Set(v)
		}
	}
	for prov, pool := range rt.Exec.Pools {
		s.metrics.KeysCooling.WithLabelValues(prov).Set(float64(pool.Cooling()))
	}
}

// Swap installs a new runtime snapshot (hot reload).
func (s *Server) Swap(rt *Runtime) { s.rt.Store(rt) }

// Runtime returns the current snapshot.
func (s *Server) Runtime() *Runtime { return s.rt.Load() }

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.auth(s.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", s.auth(s.handleChatCompletions))
	mux.HandleFunc("POST /v1/messages", s.auth(s.handleMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.auth(s.handleCountTokens))
	mux.HandleFunc("POST /v1/embeddings", s.auth(s.handleEmbeddings))
	promHandler := s.metrics.Handler()
	mux.Handle("GET /metrics", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.refreshGauges()
		promHandler.ServeHTTP(w, r)
	}))
	mux.Handle("GET /dashboard", s.auth(dashboard.Handler()))
	mux.HandleFunc("GET /dashboard/data", s.auth(s.handleDashboardData))
	return mux
}

// handleDashboardData serves the JSON behind the dashboard page.
func (s *Server) handleDashboardData(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Version    string             `json:"version"`
		LogDropped int64              `json:"log_dropped"`
		Spend      []store.SpendRow   `json:"spend"`
		Latency    []store.LatencyRow `json:"latency"`
		Recent     []store.DecisionRow `json:"recent"`
	}{Version: s.version, Spend: []store.SpendRow{}, Latency: []store.LatencyRow{}, Recent: []store.DecisionRow{}}
	if s.store == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.LogDropped = s.store.Dropped()
	db := s.store.DB()
	var err error
	if out.Spend, err = store.SpendByDay(db, time.Now().Add(-7*24*time.Hour)); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "dashboard_query")
		return
	}
	if out.Latency, err = store.LatencyStats(db, time.Now().Add(-24*time.Hour)); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "dashboard_query")
		return
	}
	if out.Recent, err = store.RecentDecisions(db, 50); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "dashboard_query")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// auth enforces the inbound API key when one is configured. Keys are
// accepted as Authorization: Bearer or x-api-key (Anthropic SDK convention,
// needed in phase 2 anyway).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys := s.Runtime().Config.Server.APIKeys
		if len(keys) == 0 {
			next(w, r)
			return
		}
		got := r.Header.Get("x-api-key")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		for _, k := range keys {
			if subtle.ConstantTimeCompare([]byte(got), []byte(k)) == 1 {
				next(w, r)
				return
			}
		}
		writeOpenAIError(w, http.StatusUnauthorized, "invalid API key", "invalid_api_key")
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

// handleModels serves the merged catalog. Ids are "provider/model" —
// unambiguous and directly usable as a request model (DESIGN §7 resolution
// order also accepts bare names when unique).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	rt := s.Runtime()
	models := rt.catalog.Models(r.Context())
	type wireModel struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string      `json:"object"`
		Data   []wireModel `json:"data"`
	}{Object: "list", Data: []wireModel{}}
	for _, m := range models {
		out.Data = append(out.Data, wireModel{
			ID:      m.Provider + "/" + m.ID,
			Object:  "model",
			Created: m.Created,
			OwnedBy: m.OwnedBy,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, msg, code string) {
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "api_error"
	}
	writeJSON(w, status, openai.ErrorResponse{Error: openai.ErrorBody{
		Message: msg, Type: typ, Code: code,
	}})
}
