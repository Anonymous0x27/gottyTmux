package tmux

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func cleanupSocket(socketName string) {
	cmd := exec.Command("tmux", "-L", socketName, "kill-server")
	cmd.Env = SanitizedEnv(nil)
	_ = cmd.Run()
}

func TestValidateSessionName(t *testing.T) {
	validNames := []string{"aaa", "codex", "jarvis", "dev_01", "release-2.4.1", "A1_b2-c3.d4"}
	for _, name := range validNames {
		if err := ValidateSessionName(name); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", name, err)
		}
	}

	invalidNames := []string{
		"",
		".",
		"..",
		"../etc/passwd",
		"foo/bar",
		"foo\\bar",
		"foo bar",
		"foo;id",
		"$(whoami)",
		"`uname`",
		"foo|bar",
		"foo&bar",
		strings.Repeat("a", MaxSessionNameLength+1),
	}
	for _, name := range invalidNames {
		if err := ValidateSessionName(name); err == nil {
			t.Errorf("expected %q to be rejected, but got nil error", name)
		}
	}

	reserved := []string{"list", "LIST", "manager", "Manager", "api", "ws", "js", "css", "favicon.png", "favicon.ico", "auth_token.js", "config.js"}
	for _, name := range reserved {
		err := ValidateSessionName(name)
		if !errors.Is(err, ErrReservedSessionName) {
			t.Errorf("expected ErrReservedSessionName for %q, got %v", name, err)
		}
	}
}

func TestMissingTmuxBinary(t *testing.T) {
	mgr := NewManagerWithConfig("gotty-nonexistent-tmux-bin-99999", "")
	err := mgr.CheckInstalled()
	if !errors.Is(err, ErrTmuxNotInstalled) {
		t.Fatalf("expected ErrTmuxNotInstalled, got %v", err)
	}

	st, err := mgr.GetStatus()
	if err != nil {
		t.Fatalf("expected nil error from GetStatus when uninstalled, got %v", err)
	}
	if st.Installed || st.ServerRunning || st.SessionCount != 0 {
		t.Fatalf("unexpected status for missing tmux: %+v", st)
	}
}

func TestManagerLifecycleAndExactTargetSafety(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gotty_test_%d", time.Now().UnixNano())
	defer cleanupSocket(socket)

	mgr := NewManagerWithConfig("tmux", socket)

	// 1. Server not running initially -> empty list, server_running=false
	sessions, err := mgr.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed when server stopped: %v", err)
	}
	if sessions == nil || len(sessions) != 0 {
		t.Fatalf("expected empty non-nil slice, got %#v", sessions)
	}

	status, err := mgr.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if !status.Installed || status.ServerRunning || status.SessionCount != 0 {
		t.Fatalf("unexpected initial status: %+v", status)
	}

	// 2. Create "codex-prod" and ensure prefix "codex" does NOT match or kill "codex-prod"
	if err := mgr.CreateSession("codex-prod"); err != nil {
		t.Fatalf("CreateSession(codex-prod) failed: %v", err)
	}

	exists, err := mgr.SessionExists("codex")
	if err != nil {
		t.Fatalf("SessionExists(codex) error: %v", err)
	}
	if exists {
		t.Fatalf("SessionExists(codex) matched prefix codex-prod!")
	}

	if err := mgr.KillSession("codex"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound when killing prefix 'codex', got %v", err)
	}

	existsProd, err := mgr.SessionExists("codex-prod")
	if err != nil || !existsProd {
		t.Fatalf("codex-prod should still exist, exists=%v err=%v", existsProd, err)
	}

	// 3. Duplicate creation returns ErrSessionExists
	if err := mgr.CreateSession("codex-prod"); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("expected ErrSessionExists, got %v", err)
	}

	// 4. Create second session and verify status & list
	if err := mgr.CreateSession("aaa"); err != nil {
		t.Fatalf("CreateSession(aaa) failed: %v", err)
	}

	status, err = mgr.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if !status.Installed || !status.ServerRunning || status.SessionCount != 2 {
		t.Fatalf("expected 2 running sessions, got %+v", status)
	}

	// 5. Kill exact session "aaa" while "codex-prod" remains
	if err := mgr.KillSession("aaa"); err != nil {
		t.Fatalf("KillSession(aaa) failed: %v", err)
	}
	sessions, err = mgr.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name != "codex-prod" {
		t.Fatalf("expected only codex-prod left, got %+v", sessions)
	}
}

func TestHandlerRESTAPI(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gotty_api_test_%d", time.Now().UnixNano())
	defer cleanupSocket(socket)

	mgr := NewManagerWithConfig("tmux", socket)
	h := NewHandler(mgr)

	// 1. GET /list when empty -> {"sessions":[]}
	req := httptest.NewRequest(http.MethodGet, "/list", nil)
	rec := httptest.NewRecorder()
	h.HandleList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /list status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"sessions":[]`) {
		t.Fatalf("expected empty sessions array JSON, got %s", rec.Body.String())
	}

	// 2. POST /api/tmux/sessions -> 200 OK
	req = httptest.NewRequest(http.MethodPost, "/api/tmux/sessions", bytes.NewBufferString(`{"name":"codex"}`))
	rec = httptest.NewRecorder()
	h.HandleSessions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/tmux/sessions status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 3. Duplicate POST /api/tmux/sessions -> 409 Conflict
	req = httptest.NewRequest(http.MethodPost, "/api/tmux/sessions", bytes.NewBufferString(`{"name":"codex"}`))
	rec = httptest.NewRecorder()
	h.HandleSessions(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on duplicate session, got %d", rec.Code)
	}

	// 4. Reserved name POST -> 400 Bad Request
	req = httptest.NewRequest(http.MethodPost, "/api/tmux/sessions", bytes.NewBufferString(`{"name":"manager"}`))
	rec = httptest.NewRecorder()
	h.HandleSessions(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for reserved name, got %d", rec.Code)
	}

	// 5. GET /api/tmux/sessions -> includes codex
	req = httptest.NewRequest(http.MethodGet, "/api/tmux/sessions", nil)
	rec = httptest.NewRecorder()
	h.HandleSessions(rec, req)
	var listResp sessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(listResp.Sessions) != 1 || listResp.Sessions[0].Name != "codex" {
		t.Fatalf("unexpected sessions list: %+v", listResp.Sessions)
	}

	// 6. DELETE nonexistent -> 404
	req = httptest.NewRequest(http.MethodDelete, "/api/tmux/sessions/doesnotexist", nil)
	rec = httptest.NewRecorder()
	h.HandleSessionByName(rec, req, "doesnotexist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found, got %d", rec.Code)
	}

	// 7. DELETE existing -> 200
	req = httptest.NewRequest(http.MethodDelete, "/api/tmux/sessions/codex", nil)
	rec = httptest.NewRecorder()
	h.HandleSessionByName(rec, req, "codex")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on delete, got %d (%s)", rec.Code, rec.Body.String())
	}
}
