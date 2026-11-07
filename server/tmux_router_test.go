package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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

func getFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	return port
}

func waitHTTPReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become ready in time", url)
}

func TestTmuxServerEndToEndAndAuth(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	socket := fmt.Sprintf("gotty_srv_test_%d", time.Now().UnixNano())
	defer func() {
		cmd := exec.Command("tmux", "-L", socket, "kill-server")
		cmd.Env = tmux.SanitizedEnv(nil)
		_ = cmd.Run()
	}()

	mgr := tmux.NewManagerWithConfig("tmux", socket)
	backendOpts := &localcommand.Options{CloseSignal: 1, CloseTimeout: 5}
	factory, err := localcommand.NewTmuxFactory(mgr, backendOpts)
	if err != nil {
		t.Fatalf("NewTmuxFactory failed: %v", err)
	}

	port := getFreePort(t)
	opts := &server.Options{}
	if err := utils.ApplyDefaultValues(opts); err != nil {
		t.Fatalf("ApplyDefaultValues failed: %v", err)
	}
	opts.Address = "127.0.0.1"
	opts.Port = port
	opts.Tmux = true
	opts.PermitWrite = true
	opts.EnableBasicAuth = true
	opts.Credential = "admin:s3cr3t"
	opts.TitleVariables = map[string]interface{}{
		"command":  "tmux",
		"hostname": "testhost",
	}

	srv, err := server.New(factory, opts)
	if err != nil {
		t.Fatalf("server.New failed: %v", err)
	}
	srv.SetTmuxManager(mgr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
	}()

	baseURL := "http://127.0.0.1:" + port
	waitHTTPReady(t, baseURL+"/manager")

	// 1. Unauthenticated requests to manager, list, API, and session must return 401
	for _, p := range []string{"/", "/manager", "/list", "/api/tmux/sessions", "/api/tmux/status", "/aaa"} {
		resp, err := http.Get(baseURL + p)
		if err != nil {
			t.Fatalf("GET %s failed: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for unauthenticated GET %s, got %d", p, resp.StatusCode)
		}
	}

	doAuthReq := func(method, path, body string, extraHeaders map[string]string) *http.Response {
		var rdr io.Reader
		if body != "" {
			rdr = bytes.NewBufferString(body)
		}
		req, err := http.NewRequest(method, baseURL+path, rdr)
		if err != nil {
			t.Fatalf("NewRequest failed: %v", err)
		}
		req.SetBasicAuth("admin", "s3cr3t")
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
		client := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s failed: %v", method, path, err)
		}
		return resp
	}

	// 2. Root redirects to /manager (and respects X-Forwarded-Prefix)
	resp := doAuthReq(http.MethodGet, "/", "", map[string]string{"X-Forwarded-Prefix": "/gotty"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/gotty/manager" {
		t.Fatalf("expected 302 to /gotty/manager, got %d Location=%q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// 3. Static assets load cleanly and never create tmux sessions
	for _, assetPath := range []string{"/manager", "/css/manager.css", "/js/manager.js", "/js/gotty-bundle.js", "/css/xterm.css", "/auth_token.js", "/config.js", "/favicon.png", "/favicon.ico"} {
		r := doAuthReq(http.MethodGet, assetPath, "", nil)
		_, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for %s, got %d", assetPath, r.StatusCode)
		}
	}

	sessions, _ := mgr.ListSessions()
	if len(sessions) != 0 {
		t.Fatalf("static asset fetches must not create sessions, found: %+v", sessions)
	}

	// 4. Path traversal and encoded slashes are rejected with 400
	for _, bad := range []string{"/%2e%2e", "/foo%2fbar", "/bad$session"} {
		r := doAuthReq(http.MethodGet, bad, "", nil)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d", bad, r.StatusCode)
		}
	}

	// 5. Visiting /aaa auto-creates session "aaa" and serves terminal HTML
	r := doAuthReq(http.MethodGet, "/aaa", "", nil)
	htmlBytes, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK || !strings.Contains(string(htmlBytes), `id="terminal"`) {
		t.Fatalf("expected terminal HTML on GET /aaa, status=%d", r.StatusCode)
	}

	// 6. Connect two WebSocket clients simultaneously to /aaa/ws, execute interactive command, resize, disconnect
	dialer := websocket.Dialer{Subprotocols: webtty.Protocols}
	wsURL := "ws://127.0.0.1:" + port + "/aaa/ws"

	ws1, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws1 dial failed: %v", err)
	}
	if err := ws1.WriteMessage(websocket.TextMessage, []byte(`{"AuthToken":"admin:s3cr3t","Arguments":""}`)); err != nil {
		t.Fatalf("ws1 auth write failed: %v", err)
	}

	ws2, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws2 dial failed: %v", err)
	}
	if err := ws2.WriteMessage(websocket.TextMessage, []byte(`{"AuthToken":"admin:s3cr3t","Arguments":""}`)); err != nil {
		t.Fatalf("ws2 auth write failed: %v", err)
	}

	// Send terminal resize from ws1
	resizePayload := fmt.Sprintf("%c%s", webtty.ResizeTerminal, `{"columns":120,"rows":36}`)
	if err := ws1.WriteMessage(websocket.TextMessage, []byte(resizePayload)); err != nil {
		t.Fatalf("ws1 resize failed: %v", err)
	}

	// Wait briefly for both tmux clients to attach
	time.Sleep(400 * time.Millisecond)
	sessions, err = mgr.ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].Attached != 2 {
		t.Fatalf("expected 1 session 'aaa' with attached=2, got %+v (err=%v)", sessions, err)
	}

	// Type a marker command on ws1 and verify ws2 ALSO receives the shared output!
	marker := "GOTTY_TMUX_SHARED_OK_98765"
	cmdPayload := fmt.Sprintf("%cecho %s\r", webtty.Input, marker)
	if err := ws1.WriteMessage(websocket.TextMessage, []byte(cmdPayload)); err != nil {
		t.Fatalf("ws1 input failed: %v", err)
	}

	matched := false
	deadline := time.Now().Add(5 * time.Second)
	var accumulated strings.Builder
	for time.Now().Before(deadline) {
		_ = ws2.SetReadDeadline(time.Now().Add(1 * time.Second))
		_, msg, err := ws2.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) > 1 && msg[0] == webtty.Output {
			decoded, err := base64.StdEncoding.DecodeString(string(msg[1:]))
			if err == nil {
				accumulated.Write(decoded)
				if strings.Contains(accumulated.String(), marker) {
					matched = true
					break
				}
			}
		}
	}
	if !matched {
		t.Fatalf("ws2 did not receive shared tmux output containing %q; got: %q", marker, accumulated.String())
	}

	// Disconnect both WebSocket clients and verify session "aaa" stays alive (attached=0)
	_ = ws1.Close()
	_ = ws2.Close()
	time.Sleep(400 * time.Millisecond)

	rList := doAuthReq(http.MethodGet, "/list", "", nil)
	var parsed struct {
		Sessions []tmux.Session `json:"sessions"`
	}
	_ = json.NewDecoder(rList.Body).Decode(&parsed)
	_ = rList.Body.Close()

	if len(parsed.Sessions) != 1 || parsed.Sessions[0].Name != "aaa" || parsed.Sessions[0].Attached != 0 {
		t.Fatalf("expected session 'aaa' to survive browser disconnect with attached=0, got %+v", parsed.Sessions)
	}

	cancel()
	<-done
}
