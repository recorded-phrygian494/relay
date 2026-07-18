package server

import (
	"encoding/json"
	"io"
	"net/http"
)

// handleFeedback serves POST /v1/feedback {request_id, score} — the
// explicit label source for relay train (DESIGN §0.4). Scores are [0,1];
// 0 = useless, 1 = great. Ignored by relay unless training consults it.
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "request logging is disabled; feedback has nowhere to go", "feedback_unavailable")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request")
		return
	}
	var in struct {
		RequestID string   `json:"request_id"`
		Score     *float64 `json:"score"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request")
		return
	}
	if in.RequestID == "" || in.Score == nil {
		writeOpenAIError(w, http.StatusBadRequest, "request_id and score are required", "invalid_request")
		return
	}
	if *in.Score < 0 || *in.Score > 1 {
		writeOpenAIError(w, http.StatusBadRequest, "score must be in [0,1]", "invalid_request")
		return
	}
	found, err := s.store.SetFeedback(in.RequestID, *in.Score)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "feedback_error")
		return
	}
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "no logged request with id "+in.RequestID, "request_not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "request_id": in.RequestID, "score": *in.Score})
}
