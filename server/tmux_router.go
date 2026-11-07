package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/NYTimes/gziphandler"
	"github.com/elazarl/go-bindata-assetfs"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"

	"github.com/yudai/gotty/tmux"
	"github.com/yudai/gotty/web"
	"github.com/yudai/gotty/webtty"
)

func hasTraversalOrEncodedSlash(escapedPath string) bool {
	lower := strings.ToLower(escapedPath)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return true
	}
	segments := strings.Split(escapedPath, "/")
	for _, seg := range segments {
		if seg == "." || seg == ".." {
			return true
		}
		unescaped, err := url.PathUnescape(seg)
		if err != nil {
			return true
		}
		if unescaped == "." || unescaped == ".." || strings.Contains(unescaped, "/") || strings.Contains(unescaped, "\\") {
			return true
		}
	}
	return false
}

func (server *Server) setupTmuxHandlers(ctx context.Context, cancel context.CancelFunc, pathPrefix string, counter *counter) http.Handler {
	if server.tmuxManager == nil {
		server.tmuxManager = tmux.NewManager()
	}

	staticFileHandler := http.FileServer(
		&assetfs.AssetFS{Asset: Asset, AssetDir: AssetDir, Prefix: "static"},
	)
	tmuxAPI := tmux.NewHandler(server.tmuxManager)
	once := new(int64)

	go func() {
		select {
		case <-counter.timer().C:
			cancel()
		case <-ctx.Done():
		}
	}()

	serveFavicon := func(w http.ResponseWriter, r *http.Request) {
		data, err := Asset("static/favicon.png")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	}

	serveManagerHTML := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(web.ManagerHTML)
	}

	serveManagerCSS := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(web.ManagerCSS)
	}

	serveManagerJS := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Write(web.ManagerJS)
	}

	// Core HTTP handler (non-WebSocket requests, wrapped with Gzip + BasicAuth + Headers + Logger)
	httpCore := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped := r.URL.EscapedPath()
		if escaped == "" {
			escaped = r.URL.Path
		}

		if hasTraversalOrEncodedSlash(escaped) {
			http.Error(w, "Bad Request: invalid path", http.StatusBadRequest)
			return
		}

		if !strings.HasPrefix(escaped, pathPrefix) {
			http.NotFound(w, r)
			return
		}
		rel := strings.TrimPrefix(escaped, pathPrefix)

		// 1. Root path -> redirect to /manager (respecting X-Forwarded-Prefix if behind reverse proxy)
		if rel == "" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			target := pathPrefix + "manager"
			if fwdPrefix := strings.TrimSuffix(r.Header.Get("X-Forwarded-Prefix"), "/"); fwdPrefix != "" {
				target = fwdPrefix + pathPrefix + "manager"
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}

		// 2. Web manager page (/manager and /manager/)
		if rel == "manager" {
			serveManagerHTML(w, r)
			return
		}
		if rel == "manager/" {
			http.Redirect(w, r, "../manager", http.StatusFound)
			return
		}

		// 3. Session list endpoint (/list)
		if rel == "list" {
			tmuxAPI.HandleList(w, r)
			return
		}

		// 4. Management REST API (/api/tmux/...)
		if rel == "api/tmux/status" {
			tmuxAPI.HandleStatus(w, r)
			return
		}
		if rel == "api/tmux/sessions" {
			tmuxAPI.HandleSessions(w, r)
			return
		}
		if strings.HasPrefix(rel, "api/tmux/sessions/") {
			rawName := strings.TrimPrefix(rel, "api/tmux/sessions/")
			tmuxAPI.HandleSessionByName(w, r, rawName)
			return
		}
		if rel == "api" || strings.HasPrefix(rel, "api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"success":false,"error":"api endpoint not found"}`))
			return
		}

		// 5. Static & GoTTY frontend config resources at root
		switch rel {
		case "js/manager.js":
			serveManagerJS(w, r)
			return
		case "css/manager.css":
			serveManagerCSS(w, r)
			return
		case "favicon.png", "favicon.ico":
			serveFavicon(w, r)
			return
		case "auth_token.js":
			server.handleAuthToken(w, r)
			return
		case "config.js":
			server.handleConfig(w, r)
			return
		case "robots.txt", "apple-touch-icon.png", "ws":
			http.NotFound(w, r)
			return
		}

		if strings.HasPrefix(rel, "js/") || strings.HasPrefix(rel, "css/") {
			http.StripPrefix(pathPrefix, staticFileHandler).ServeHTTP(w, r)
			return
		}

		// 6. Dynamic session routes: /<session>, /<session>/, and /<session>/<static>
		trimmed := strings.TrimSuffix(rel, "/")
		parts := strings.Split(trimmed, "/")

		if len(parts) == 1 {
			sessionName, err := url.PathUnescape(parts[0])
			if err != nil {
				http.Error(w, "Bad Request: invalid URL encoding", http.StatusBadRequest)
				return
			}
			if err := tmux.ValidateSessionName(sessionName); err != nil {
				http.Error(w, "Bad Request: "+err.Error(), http.StatusBadRequest)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			if r.Method == http.MethodGet {
				if err := server.tmuxManager.EnsureSession(sessionName); err != nil {
					log.Printf("Failed to ensure tmux session %q: %v", sessionName, err)
					http.Error(w, "Failed to initialize tmux session", http.StatusInternalServerError)
					return
				}
			}

			server.handleSessionIndex(w, r, tmux.CanonicalSessionName(sessionName))
			return
		}

		// Sub-resources under /<session>/... (when accessed via trailing slash /<session>/)
		rawSession := parts[0]
		sessionName, err := url.PathUnescape(rawSession)
		if err != nil || tmux.ValidateSessionName(sessionName) != nil {
			http.NotFound(w, r)
			return
		}

		subRel := strings.Join(parts[1:], "/")
		switch subRel {
		case "favicon.png", "favicon.ico":
			serveFavicon(w, r)
			return
		case "auth_token.js":
			server.handleAuthToken(w, r)
			return
		case "config.js":
			server.handleConfig(w, r)
			return
		case "js/manager.js":
			serveManagerJS(w, r)
			return
		case "css/manager.css":
			serveManagerCSS(w, r)
			return
		}

		if strings.HasPrefix(subRel, "js/") || strings.HasPrefix(subRel, "css/") {
			rCopy := new(http.Request)
			*rCopy = *r
			uCopy := new(url.URL)
			*uCopy = *r.URL
			uCopy.Path = "/" + subRel
			rCopy.URL = uCopy
			staticFileHandler.ServeHTTP(w, rCopy)
			return
		}

		http.NotFound(w, r)
	})

	var httpProtected http.Handler = gziphandler.GzipHandler(httpCore)
	if server.options.EnableBasicAuth {
		log.Printf("Using Basic Authentication")
		httpProtected = server.wrapBasicAuth(httpProtected, server.options.Credential)
	}

	// Dispatcher routes /<session>/ws directly to WebSocket handler (preserving http.Hijacker)
	// while delegating all standard HTTP requests to httpProtected.
	wsSessionHandler := server.generateHandleSessionWS(ctx, cancel, counter, once)

	var rootHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped := r.URL.EscapedPath()
		if escaped == "" {
			escaped = r.URL.Path
		}
		if !hasTraversalOrEncodedSlash(escaped) && strings.HasPrefix(escaped, pathPrefix) {
			rel := strings.TrimPrefix(escaped, pathPrefix)
			parts := strings.Split(rel, "/")
			if len(parts) == 2 && parts[1] == "ws" && parts[0] != "" {
				wsSessionHandler.ServeHTTP(w, r)
				return
			}
		}
		httpProtected.ServeHTTP(w, r)
	})

	if server.isPasswordAuthEnabled() {
		log.Printf("Using Web Password Authentication (--password)")
	}
	rootHandler = server.wrapPasswordAuth(rootHandler, pathPrefix)

	return server.wrapLogger(server.wrapHeaders(rootHandler))
}

func (server *Server) handleSessionIndex(w http.ResponseWriter, r *http.Request, sessionName string) {
	serverVars := make(map[string]interface{})
	for k, v := range server.options.TitleVariables {
		serverVars[k] = v
	}
	serverVars["command"] = "tmux:" + sessionName
	serverVars["session"] = sessionName

	titleVars := server.titleVariables(
		[]string{"server", "master"},
		map[string]map[string]interface{}{
			"server": serverVars,
			"master": map[string]interface{}{
				"remote_addr": r.RemoteAddr,
			},
		},
	)

	titleBuf := new(bytes.Buffer)
	err := server.titleTemplate.Execute(titleBuf, titleVars)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	indexVars := map[string]interface{}{
		"title": titleBuf.String(),
	}

	indexBuf := new(bytes.Buffer)
	err = server.indexTemplate.Execute(indexBuf, indexVars)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexBuf.Bytes())
}

func (server *Server) generateHandleSessionWS(ctx context.Context, cancel context.CancelFunc, counter *counter, once *int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		escaped := r.URL.EscapedPath()
		if escaped == "" {
			escaped = r.URL.Path
		}
		trimmed := strings.Trim(escaped, "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 || parts[len(parts)-1] != "ws" {
			http.NotFound(w, r)
			return
		}
		rawSession := parts[len(parts)-2]
		sessionName, err := url.PathUnescape(rawSession)
		if err != nil {
			http.Error(w, "Bad Request: invalid URL encoding", http.StatusBadRequest)
			return
		}
		if err := tmux.ValidateSessionName(sessionName); err != nil {
			http.Error(w, "Bad Request: "+err.Error(), http.StatusBadRequest)
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if server.options.Once {
			success := atomic.CompareAndSwapInt64(once, 0, 1)
			if !success {
				http.Error(w, "Server is shutting down", http.StatusServiceUnavailable)
				return
			}
		}

		num := counter.add(1)
		closeReason := "unknown reason"

		defer func() {
			num := counter.done()
			log.Printf(
				"Connection closed for tmux session %q by %s: %s, connections: %d/%d",
				sessionName, r.RemoteAddr, closeReason, num, server.options.MaxConnection,
			)

			if server.options.Once {
				cancel()
			}
		}()

		if int64(server.options.MaxConnection) != 0 {
			if num > server.options.MaxConnection {
				closeReason = "exceeding max number of connections"
				http.Error(w, "Too many connections", http.StatusServiceUnavailable)
				return
			}
		}

		log.Printf("New client connected to tmux session %q: %s, connections: %d/%d", sessionName, r.RemoteAddr, num, server.options.MaxConnection)

		conn, err := server.upgrader.Upgrade(w, r, nil)
		if err != nil {
			closeReason = err.Error()
			return
		}
		defer conn.Close()

		err = server.processSessionWSConn(ctx, conn, sessionName)

		switch err {
		case ctx.Err():
			closeReason = "cancelation"
		case webtty.ErrSlaveClosed:
			closeReason = server.factory.Name()
		case webtty.ErrMasterClosed:
			closeReason = "client"
		default:
			closeReason = fmt.Sprintf("an error: %s", err)
		}
	}
}

func (server *Server) processSessionWSConn(ctx context.Context, conn *websocket.Conn, sessionName string) error {
	typ, initLine, err := conn.ReadMessage()
	if err != nil {
		return errors.Wrapf(err, "failed to authenticate websocket connection")
	}
	if typ != websocket.TextMessage {
		return errors.New("failed to authenticate websocket connection: invalid message type")
	}

	var init InitMessage
	err = json.Unmarshal(initLine, &init)
	if err != nil {
		return errors.Wrapf(err, "failed to authenticate websocket connection")
	}
	if init.AuthToken != server.options.Credential {
		return errors.New("failed to authenticate websocket connection")
	}

	var slave Slave
	if sf, ok := server.factory.(SessionFactory); ok {
		slave, err = sf.NewSessionSlave(sessionName)
	} else {
		slave, err = server.factory.New(map[string][]string{
			"session": {sessionName},
		})
	}
	if err != nil {
		return errors.Wrapf(err, "failed to create tmux client backend")
	}
	defer slave.Close()

	serverVars := make(map[string]interface{})
	for k, v := range server.options.TitleVariables {
		serverVars[k] = v
	}
	canonical := tmux.CanonicalSessionName(sessionName)
	serverVars["command"] = "tmux:" + canonical
	serverVars["session"] = canonical

	titleVars := server.titleVariables(
		[]string{"server", "master", "slave"},
		map[string]map[string]interface{}{
			"server": serverVars,
			"master": map[string]interface{}{
				"remote_addr": conn.RemoteAddr(),
			},
			"slave": slave.WindowTitleVariables(),
		},
	)

	titleBuf := new(bytes.Buffer)
	err = server.titleTemplate.Execute(titleBuf, titleVars)
	if err != nil {
		return errors.Wrapf(err, "failed to fill window title template")
	}

	opts := []webtty.Option{
		webtty.WithWindowTitle(titleBuf.Bytes()),
	}
	if server.options.PermitWrite {
		opts = append(opts, webtty.WithPermitWrite())
	}
	if server.options.EnableReconnect {
		opts = append(opts, webtty.WithReconnect(server.options.ReconnectTime))
	}
	if server.options.Width > 0 {
		opts = append(opts, webtty.WithFixedColumns(server.options.Width))
	}
	if server.options.Height > 0 {
		opts = append(opts, webtty.WithFixedRows(server.options.Height))
	}
	if server.options.Preferences != nil {
		opts = append(opts, webtty.WithMasterPreferences(server.options.Preferences))
	}

	tty, err := webtty.New(&wsWrapper{conn}, slave, opts...)
	if err != nil {
		return errors.Wrapf(err, "failed to create webtty")
	}

	return tty.Run(ctx)
}
