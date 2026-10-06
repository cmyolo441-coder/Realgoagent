package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/llm"
)

// Handler wires the Server to HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions", s.handleCreate)
	mux.HandleFunc("GET /api/sessions", s.handleList)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleInfo)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /api/sessions/{id}/prompt", s.handlePrompt)
	mux.HandleFunc("POST /api/sessions/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /api/sessions/{id}/answer", s.handleAnswer)
	mux.HandleFunc("POST /api/sessions/{id}/handoff", s.handleHandoff)
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.handleMessages)
	mux.HandleFunc("GET /api/sessions/{id}/usage", s.handleUsage)
	mux.HandleFunc("POST /api/sessions/{id}/reset", s.handleReset)
	mux.HandleFunc("POST /api/sessions/{id}/planmode", s.handlePlanMode)
	mux.HandleFunc("GET /api/sessions/{id}/systemprompt", s.handleSystemPrompt)
	return s.withAuth(mux)
}

// withAuth enforces the bearer token when one is configured.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != s.token {
				writeErr(w, http.StatusUnauthorized, "bad or missing token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, ErrorResponse{Error: msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	info, err := s.Create(req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.List())
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	info, err := s.Info(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var req PromptRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, "empty prompt")
		return
	}
	if err := s.Prompt(r.PathValue("id"), req.Text); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.Cancel(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var req AnswerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Answer(r.PathValue("id"), req.Text); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleHandoff replaces the session's history (local -> cloud migration).
func (s *Server) handleHandoff(w http.ResponseWriter, r *http.Request) {
	var req HistoryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.InjectHistory(r.PathValue("id"), req.Messages); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sess, ok := s.sessions[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	writeJSON(w, http.StatusOK, sess.ag.Messages())
}

func (s *Server) sessionByID(w http.ResponseWriter, r *http.Request) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[r.PathValue("id")]
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return nil
	}
	return sess
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionByID(w, r)
	if sess == nil {
		return
	}
	writeJSON(w, http.StatusOK, sess.ag.Usage())
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if err := s.Reset(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePlanMode(w http.ResponseWriter, r *http.Request) {
	var req PlanModeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.SetPlanMode(r.PathValue("id"), req.Enabled); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSystemPrompt(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionByID(w, r)
	if sess == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"system_prompt": sess.ag.SystemPrompt()})
}

// handleEvents streams session events as SSE. It replays nothing: connect
// before prompting, or use /messages for history.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sess, ok := s.sessions[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch, unsub := sess.subscribe()
	defer unsub()

	// Hello first, so the client knows the stream is live.
	fmt.Fprintf(w, "event: hello\ndata: %s\n\n", mustJSON(sess.info))
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sess.done:
			return
		case ev := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(ev))
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

var (
	_ = agent.EvDone
	_ = llm.RoleUser
)
