package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/yudai/gotty/backend/localcommand"
	"github.com/yudai/gotty/server"
	"github.com/yudai/gotty/tmux"
	"github.com/yudai/gotty/utils"
	"github.com/yudai/gotty/webtty"
)

func TestPasswordAuthDisabledByDefault(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gotty_nopwd_test_%d", time.Now().UnixNano())
	defer func() {
		cmd := exec.Command("tmux", "-L", socket, "kill-server")
		cmd.Env = tmux.SanitizedEnv(nil)
		_ = cmd.Run()
	}()

	mgr := tmux.NewManagerWithConfig("tmux", socket)
	factory, err := localcommand.NewTmuxFactory(mgr, &localcommand.Options{CloseSignal: 1, CloseTimeout: 5})
	if err != nil {
		t.Fatalf("NewTmuxFactory failed: %v", err)
	}

	port := getFreePort(t)
	opts := &server.Options{}
	_ = utils.ApplyDefaultValues(opts)
	opts.Address = "127.0.0.1"
	opts.Port = port
	opts.Tmux = true
	opts.PermitWrite = true
	opts.TitleVariables = map[string]interface{}{"command": "tmux", "hostname": "testhost"}

	srv, err := server.New(factory, opts)
	if err != nil {
		t.Fatalf("server.New failed: %v", err)
	}
	srv.SetTmuxManager(mgr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	baseURL := "http://127.0.0.1:" + port
	waitHTTPReady(t, baseURL+"/manager")

	// Direct access works without login when Password is not set
	for _, p := range []string{"/manager", "/list", "/api/tmux/sessions", "/api/auth/status"} {
		resp, err := http.Get(baseURL + p)
		if err != nil {
			t.Fatalf("GET %s failed: %v", p, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on %s without --password, got %d (%s)", p, resp.StatusCode, string(body))
		}
	}

	cancel()
	<-done
}

func TestPasswordAuthEnabledFlow(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gotty_pwd_test_%d", time.Now().UnixNano())
	defer func() {
		cmd := exec.Command("tmux", "-L", socket, "kill-server")
		cmd.Env = tmux.SanitizedEnv(nil)
		_ = cmd.Run()
	}()

	mgr := tmux.NewManagerWithConfig("tmux", socket)
	factory, err := localcommand.NewTmuxFactory(mgr, &localcommand.Options{CloseSignal: 1, CloseTimeout: 5})
	if err != nil {
		t.Fatalf("NewTmuxFactory failed: %v", err)
	}

	port := getFreePort(t)
	opts := &server.Options{}
	_ = utils.ApplyDefaultValues(opts)
	opts.Address = "127.0.0.1"
	opts.Port = port
	opts.Tmux = true
	opts.PermitWrite = true
	opts.EnablePasswordAuth = true
	opts.Password = "MyS3cretPass!2026"
	opts.TitleVariables = map[string]interface{}{"command": "tmux", "hostname": "testhost"}

	srv, err := server.New(factory, opts)
	if err != nil {
		t.Fatalf("server.New failed: %v", err)
	}
	srv.SetTmuxManager(mgr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	baseURL := "http://127.0.0.1:" + port
	waitHTTPReady(t, baseURL+"/login")

	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. Unauthenticated browser navigation redirects to /login?next=... and MUST NOT create tmux session
	for _, p := range []string{"/manager", "/unauth_probe"} {
		resp, err := noRedirectClient.Get(baseURL + p)
		if err != nil {
			t.Fatalf("GET %s failed: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Fatalf("expected 302 to /login for %s, got %d Location=%q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	sessions, _ := mgr.ListSessions()
	if len(sessions) != 0 {
		t.Fatalf("unauthenticated visit to /unauth_probe must NOT create session, got %+v", sessions)
	}

	// 2. Unauthenticated API calls return 401 JSON
	for _, p := range []string{"/list", "/api/tmux/sessions", "/api/tmux/status", "/api/auth/status"} {
		resp, err := noRedirectClient.Get(baseURL + p)
		if err != nil {
			t.Fatalf("GET %s failed: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthenticated API %s, got %d", p, resp.StatusCode)
		}
	}

	// 3. Unauthenticated WebSocket dial returns 401
	dialer := websocket.Dialer{Subprotocols: webtty.Protocols}
	_, wsResp, err := dialer.Dial("ws://127.0.0.1:"+port+"/aaa/ws", nil)
	if err == nil || wsResp == nil || wsResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected unauthenticated WS dial to fail with 401, err=%v resp=%v", err, wsResp)
	}

	// 4. Wrong password on POST /login returns 401
	badLogin, err := http.Post(baseURL+"/login", "application/json", bytes.NewBufferString(`{"password":"wrong"}`))
	if err != nil {
		t.Fatalf("POST /login failed: %v", err)
	}
	_ = badLogin.Body.Close()
	if badLogin.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d", badLogin.StatusCode)
	}

	// 5. Correct password with open-redirect attempt sanitizes redirect to /manager and sets session cookie
	jar, _ := cookiejar.New(nil)
	authedClient := &http.Client{Jar: jar}

	goodLogin, err := authedClient.Post(
		baseURL+"/login",
		"application/json",
		bytes.NewBufferString(`{"password":"MyS3cretPass!2026","next":"https://evil.example.com"}`),
	)
	if err != nil {
		t.Fatalf("POST /login failed: %v", err)
	}
	var loginRes struct {
		Success  bool   `json:"success"`
		Redirect string `json:"redirect"`
	}
	_ = json.NewDecoder(goodLogin.Body).Decode(&loginRes)
	_ = goodLogin.Body.Close()
	if goodLogin.StatusCode != http.StatusOK || !loginRes.Success || loginRes.Redirect != "/manager" {
		t.Fatalf("unexpected login response: status=%d res=%+v", goodLogin.StatusCode, loginRes)
	}

	// 6. Authenticated session cookie grants access to /manager, /api/tmux/sessions, /aaa, and WebSocket /aaa/ws
	respMgr, err := authedClient.Get(baseURL + "/manager")
	if err != nil || respMgr.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /manager after login, status=%v err=%v", respMgr.StatusCode, err)
	}
	_ = respMgr.Body.Close()

	respCreate, err := authedClient.Post(baseURL+"/api/tmux/sessions", "application/json", bytes.NewBufferString(`{"name":"aaa"}`))
	if err != nil || respCreate.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating session after login, status=%v err=%v", respCreate.StatusCode, err)
	}
	_ = respCreate.Body.Close()

	u, _ := url.Parse(baseURL)
	wsHeaders := http.Header{}
	var cookieParts []string
	for _, c := range jar.Cookies(u) {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	wsHeaders.Set("Cookie", strings.Join(cookieParts, "; "))

	wsConn, _, err := dialer.Dial("ws://127.0.0.1:"+port+"/aaa/ws", wsHeaders)
	if err != nil {
		t.Fatalf("expected authenticated WS dial to succeed, got: %v", err)
	}
	_ = wsConn.WriteMessage(websocket.TextMessage, []byte(`{"AuthToken":"","Arguments":""}`))
	_ = wsConn.Close()

	// 7. Bearer header also authenticates API directly (CLI convenience)
	bearerReq, _ := http.NewRequest(http.MethodGet, baseURL+"/list", nil)
	bearerReq.Header.Set("Authorization", "Bearer MyS3cretPass!2026")
	bearerResp, err := noRedirectClient.Do(bearerReq)
	if err != nil || bearerResp.StatusCode != http.StatusOK {
		t.Fatalf("expected Bearer auth to succeed on /list, status=%v err=%v", bearerResp.StatusCode, err)
	}
	_ = bearerResp.Body.Close()

	// 8. Logout clears cookie and blocks access again
	logoutReq, _ := http.NewRequest(http.MethodPost, baseURL+"/logout", nil)
	logoutReq.Header.Set("Accept", "application/json")
	logoutResp, err := authedClient.Do(logoutReq)
	if err != nil || logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout failed: status=%v err=%v", logoutResp.StatusCode, err)
	}
	_ = logoutResp.Body.Close()

	afterLogout, err := authedClient.Get(baseURL + "/list")
	if err != nil {
		t.Fatalf("GET /list after logout failed: %v", err)
	}
	_ = afterLogout.Body.Close()
	if afterLogout.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /list after logout, got %d", afterLogout.StatusCode)
	}

	cancel()
	<-done
}
