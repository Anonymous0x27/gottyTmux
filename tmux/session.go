package tmux

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const MaxSessionNameLength = 128

var (
	// SessionNamePattern enforces safe session names: letters, digits, underscore, dot, hyphen.
	SessionNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

	ErrInvalidSessionName  = errors.New("invalid session name")
	ErrReservedSessionName = errors.New("reserved session name")
	ErrSessionExists       = errors.New("session already exists")
	ErrSessionNotFound     = errors.New("session not found")
	ErrTmuxNotInstalled    = errors.New("tmux is not installed")
)

var reservedNames = map[string]struct{}{
	"list":                 {},
	"manager":              {},
	"api":                  {},
	"ws":                   {},
	"login":                {},
	"logout":               {},
	"auth":                 {},
	"js":                   {},
	"css":                  {},
	"static":               {},
	"favicon.png":          {},
	"favicon.ico":          {},
	"apple-touch-icon.png": {},
	"auth_token.js":        {},
	"config.js":            {},
	"robots.txt":           {},
}

// Session represents a single tmux session.
type Session struct {
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Attached int    `json:"attached"`
}

// Status represents the runtime state of tmux on the host.
type Status struct {
	Installed     bool `json:"installed"`
	ServerRunning bool `json:"server_running"`
	SessionCount  int  `json:"session_count"`
}

// IsReservedName checks whether name conflicts with GoTTY / manager routes or static assets.
func IsReservedName(name string) bool {
	_, ok := reservedNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// ValidateSessionName validates a tmux session name against security and routing constraints.
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: session name cannot be empty", ErrInvalidSessionName)
	}
	if len(name) > MaxSessionNameLength {
		return fmt.Errorf("%w: session name exceeds %d characters", ErrInvalidSessionName, MaxSessionNameLength)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: path traversal segment is not allowed", ErrInvalidSessionName)
	}
	if !SessionNamePattern.MatchString(name) {
		return fmt.Errorf("%w: must match ^[a-zA-Z0-9_.-]+$", ErrInvalidSessionName)
	}
	if IsReservedName(name) {
		return fmt.Errorf("%w: %q is a reserved route name", ErrReservedSessionName, name)
	}
	return nil
}

// CanonicalSessionName returns the name tmux stores internally (tmux replaces '.' with '_').
func CanonicalSessionName(name string) string {
	return strings.ReplaceAll(name, ".", "_")
}
