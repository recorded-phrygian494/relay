package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/relay-llm/relay/internal/api/openai"
	"github.com/relay-llm/relay/internal/store"
)

// Server is the HTTP front end. The Runtime pointer is swapped atomically
// on config hot reload.
type Server struct {
	rt      atomic.Pointer[Runtime]
	store   *store.Store
	version string
}

// New builds a Server around an initial runtime. st may be nil (logging
// disabled, used by some tests).
func New(rt *Runtime, st *store.Store, version string) *Server {
	s := &Server{store: st, version: version}
	s.rt.Store(rt)
	return s
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
	return mux
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
