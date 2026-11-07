package tmux

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Handler provides HTTP endpoints for tmux session listing and management REST APIs.
type Handler struct {
	manager TmuxManager
}

// NewHandler creates a new tmux HTTP API handler backed by TmuxManager.
func NewHandler(manager TmuxManager) *Handler {
	return &Handler{
		manager: manager,
	}
}

type sessionsResponse struct {
	Sessions []Session `json:"sessions"`
}

type createSessionRequest struct {
	Name string `json:"name"`
}

type createSessionResponse struct {
	Success bool   `json:"success"`
	Name    string `json:"name"`
}

type deleteSessionResponse struct {
	Success bool `json:"success"`
}

type errorResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{
		Success: false,
		Error:   message,
	})
}

// HandleList handles GET /list.
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	sessions, err := h.manager.ListSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sessions == nil {
		sessions = []Session{}
	}

	writeJSON(w, http.StatusOK, sessionsResponse{
		Sessions: sessions,
	})
}

// HandleSessions handles GET /api/tmux/sessions and POST /api/tmux/sessions.
func (h *Handler) HandleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.HandleList(w, r)

	case http.MethodPost:
		h.handleCreateSession(w, r)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var req createSessionRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}

	name := strings.TrimSpace(req.Name)
	if err := ValidateSessionName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.manager.CreateSession(name); err != nil {
		if errors.Is(err, ErrSessionExists) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, ErrInvalidSessionName) || errors.Is(err, ErrReservedSessionName) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, createSessionResponse{
		Success: true,
		Name:    CanonicalSessionName(name),
	})
}

// HandleSessionByName handles DELETE /api/tmux/sessions/{name}.
func (h *Handler) HandleSessionByName(w http.ResponseWriter, r *http.Request, rawSegment string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if rawSegment == "" || strings.Contains(rawSegment, "/") {
		writeError(w, http.StatusBadRequest, "invalid session path")
		return
	}
	lowerRaw := strings.ToLower(rawSegment)
	if strings.Contains(lowerRaw, "%2f") || strings.Contains(lowerRaw, "%5c") {
		writeError(w, http.StatusBadRequest, "encoded path separator is not allowed")
		return
	}

	name, err := url.PathUnescape(rawSegment)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid URL encoding in session name")
		return
	}

	if err := ValidateSessionName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.manager.KillSession(name); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, ErrInvalidSessionName) || errors.Is(err, ErrReservedSessionName) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, deleteSessionResponse{
		Success: true,
	})
}

// HandleStatus handles GET /api/tmux/status.
func (h *Handler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	status, _ := h.manager.GetStatus()
	writeJSON(w, http.StatusOK, status)
}
