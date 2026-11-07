package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yudai/gotty/web"
)

const (
	passwordSessionCookieName = "gotty_auth_session"
	passwordSessionTTL        = 24 * time.Hour
)

func generateAuthSecret() []byte {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// Fallback entropy if OS CSPRNG ever failed
		h := sha256.Sum256([]byte(fmt.Sprintf("gotty-%d", time.Now().UnixNano())))
		return h[:]
	}
	return secret
}

func (server *Server) isPasswordAuthEnabled() bool {
	return server.options.EnablePasswordAuth || server.options.Password != ""
}

func (server *Server) verifyPassword(candidate string) bool {
	if !server.isPasswordAuthEnabled() || server.options.Password == "" {
		return false
	}
	candHash := sha256.Sum256([]byte(candidate))
	expHash := sha256.Sum256([]byte(server.options.Password))
	return subtle.ConstantTimeCompare(candHash[:], expHash[:]) == 1
}

func (server *Server) signSessionToken(now time.Time) string {
	issuedAt := now.Unix()
	expiresAt := now.Add(passwordSessionTTL).Unix()
	payload := fmt.Sprintf("%d.%d", issuedAt, expiresAt)

	pwdHash := sha256.Sum256([]byte(server.options.Password))
	mac := hmac.New(sha256.New, server.authSecret)
	mac.Write([]byte(payload))
	mac.Write([]byte(":"))
	mac.Write(pwdHash[:])
	sig := hex.EncodeToString(mac.Sum(nil))

	return payload + "." + sig
}

func (server *Server) verifySessionToken(token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	expiresAt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() > expiresAt {
		return false
	}

	payload := parts[0] + "." + parts[1]
	sigBytes, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}

	pwdHash := sha256.Sum256([]byte(server.options.Password))
	mac := hmac.New(sha256.New, server.authSecret)
	mac.Write([]byte(payload))
	mac.Write([]byte(":"))
	mac.Write(pwdHash[:])
	expected := mac.Sum(nil)

	return hmac.Equal(sigBytes, expected)
}

func (server *Server) isRequestAuthenticated(r *http.Request) bool {
	if !server.isPasswordAuthEnabled() {
		return true
	}

	// 1. Check signed HttpOnly session cookie
	if c, err := r.Cookie(passwordSessionCookieName); err == nil && c.Value != "" {
		if server.verifySessionToken(c.Value, time.Now()) {
			return true
		}
	}

	// 2. Check X-GoTTY-Password header (convenient for CLI / API scripts)
	if hdrPwd := r.Header.Get("X-GoTTY-Password"); hdrPwd != "" {
		if server.verifyPassword(hdrPwd) {
			return true
		}
	}

	// 3. Check Authorization header: Bearer <password> (or Basic :<password> when BasicAuth is not separately used)
	authHeader := r.Header.Get("Authorization")
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 {
			scheme := strings.ToLower(strings.TrimSpace(parts[0]))
			cred := strings.TrimSpace(parts[1])
			if scheme == "bearer" && server.verifyPassword(cred) {
				return true
			}
			if scheme == "basic" && !server.options.EnableBasicAuth {
				if decoded, err := base64.StdEncoding.DecodeString(cred); err == nil {
					pair := strings.SplitN(string(decoded), ":", 2)
					if len(pair) == 2 && server.verifyPassword(pair[1]) {
						return true
					}
				}
			}
		}
	}

	return false
}

func isHTTPSRequest(r *http.Request, tlsEnabled bool) bool {
	if tlsEnabled || r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") ||
		strings.EqualFold(r.Header.Get("X-Forwarded-Ssl"), "on") {
		return true
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Origin")), "https://") ||
		strings.HasPrefix(strings.ToLower(r.Header.Get("Referer")), "https://") {
		return true
	}
	return false
}

func (server *Server) setAuthCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	secure := isHTTPSRequest(r, server.options.EnableTLS)
	sameSite := http.SameSiteLaxMode
	if secure {
		// Allows cookie to work in both top-level HTTPS tabs and cross-site HTTPS preview iframes (CHIPS)
		sameSite = http.SameSiteNoneMode
	}

	http.SetCookie(w, &http.Cookie{
		Name:        passwordSessionCookieName,
		Value:       token,
		Path:        "/",
		MaxAge:      maxAge,
		HttpOnly:    true,
		Secure:      secure,
		SameSite:    sameSite,
		Partitioned: secure,
	})
}

func sanitizeNextRedirect(rawNext string, defaultTarget string) string {
	trimmed := strings.TrimSpace(rawNext)
	if trimmed == "" {
		return defaultTarget
	}
	if strings.ContainsAny(trimmed, "\r\n") {
		return defaultTarget
	}
	if !strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, `/\`) {
		return defaultTarget
	}
	return trimmed
}

func prefixedPath(r *http.Request, path string) string {
	fwd := strings.TrimSuffix(r.Header.Get("X-Forwarded-Prefix"), "/")
	if fwd != "" && strings.HasPrefix(fwd, "/") && !strings.HasPrefix(fwd, "//") {
		return fwd + path
	}
	return path
}

func writeAuthJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// wrapPasswordAuth enforces web password authentication across HTTP pages, REST APIs, and WebSockets
// whenever --password is provided. When --password is not set, it is a no-op pass-through (except /api/auth/status).
func (server *Server) wrapPasswordAuth(next http.Handler, pathPrefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped := r.URL.EscapedPath()
		if escaped == "" {
			escaped = r.URL.Path
		}
		rel := ""
		hasPrefix := strings.HasPrefix(escaped, pathPrefix)
		if hasPrefix {
			rel = strings.TrimPrefix(escaped, pathPrefix)
		}

		// Always expose /api/auth/status so /manager UI knows whether Logout button should appear
		if hasPrefix && rel == "api/auth/status" && r.Method == http.MethodGet {
			enabled := server.isPasswordAuthEnabled()
			authed := server.isRequestAuthenticated(r)
			if enabled && !authed {
				writeAuthJSON(w, http.StatusUnauthorized, map[string]interface{}{
					"password_enabled": true,
					"authenticated":    false,
				})
				return
			}
			writeAuthJSON(w, http.StatusOK, map[string]interface{}{
				"password_enabled": enabled,
				"authenticated":    true,
			})
			return
		}

		// If --password was not provided, do not enable password authentication
		if !server.isPasswordAuthEnabled() {
			next.ServeHTTP(w, r)
			return
		}

		defaultHome := prefixedPath(r, pathPrefix)
		if server.options.Tmux {
			defaultHome = prefixedPath(r, pathPrefix+"manager")
		}
		loginPath := prefixedPath(r, pathPrefix+"login")

		// Publicly accessible endpoints needed before login: /login, /logout, /favicon.png, /favicon.ico
		if hasPrefix {
			switch rel {
			case "favicon.png", "favicon.ico":
				next.ServeHTTP(w, r)
				return

			case "login":
				server.handlePasswordLogin(w, r, defaultHome)
				return

			case "logout":
				server.handlePasswordLogout(w, r, loginPath)
				return
			}
		}

		// Verify authentication for all protected pages, APIs, static bundles, and WebSocket upgrades
		if server.isRequestAuthenticated(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Unauthenticated WebSocket upgrade -> 401
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || rel == "ws" || strings.HasSuffix(rel, "/ws") {
			http.Error(w, "Unauthorized: password authentication required", http.StatusUnauthorized)
			return
		}

		// Unauthenticated API / JSON / non-GET request -> 401 JSON
		accept := r.Header.Get("Accept")
		isAPIRoute := rel == "list" || rel == "api" || strings.HasPrefix(rel, "api/")
		if isAPIRoute || strings.Contains(accept, "application/json") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			writeAuthJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"success": false,
				"error":   "unauthorized: password authentication required",
			})
			return
		}

		// Unauthenticated browser navigation -> 302 redirect to /login?next=...
		origTarget := prefixedPath(r, r.URL.Path)
		if r.URL.RawQuery != "" {
			origTarget += "?" + r.URL.RawQuery
		}
		redirectURL := loginPath
		if origTarget != "" && origTarget != loginPath {
			redirectURL = loginPath + "?next=" + url.QueryEscape(origTarget)
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
	})
}

type loginRequest struct {
	Password string `json:"password"`
	Next     string `json:"next"`
}

func (server *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request, defaultHome string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if server.isRequestAuthenticated(r) {
			target := sanitizeNextRedirect(r.URL.Query().Get("next"), defaultHome)
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(web.LoginHTML)

	case http.MethodPost:
		defer r.Body.Close()

		contentType := r.Header.Get("Content-Type")
		wantsJSON := strings.Contains(contentType, "application/json") || strings.Contains(r.Header.Get("Accept"), "application/json")

		var password, rawNext string
		if strings.Contains(contentType, "application/json") {
			var req loginRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
				writeAuthJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   "invalid JSON request body",
				})
				return
			}
			password = req.Password
			rawNext = req.Next
		} else {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			password = r.FormValue("password")
			rawNext = r.FormValue("next")
			if rawNext == "" {
				rawNext = r.URL.Query().Get("next")
			}
		}

		if !server.verifyPassword(password) {
			log.Printf("Password Authentication Failed: %s", r.RemoteAddr)
			if wantsJSON {
				writeAuthJSON(w, http.StatusUnauthorized, map[string]interface{}{
					"success": false,
					"error":   "认证口令错误，请重新输入",
				})
				return
			}
			http.Error(w, "Unauthorized: invalid password", http.StatusUnauthorized)
			return
		}

		log.Printf("Password Authentication Succeeded: %s", r.RemoteAddr)
		token := server.signSessionToken(time.Now())
		server.setAuthCookie(w, r, token, int(passwordSessionTTL.Seconds()))

		target := sanitizeNextRedirect(rawNext, defaultHome)
		if wantsJSON {
			writeAuthJSON(w, http.StatusOK, map[string]interface{}{
				"success":  true,
				"redirect": target,
			})
			return
		}
		http.Redirect(w, r, target, http.StatusFound)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (server *Server) handlePasswordLogout(w http.ResponseWriter, r *http.Request, loginPath string) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	server.setAuthCookie(w, r, "", -1)

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeAuthJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
		})
		return
	}
	http.Redirect(w, r, loginPath, http.StatusFound)
}
