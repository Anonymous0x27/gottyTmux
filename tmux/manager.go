package tmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// TmuxManager encapsulates all tmux server and session operations.
type TmuxManager interface {
	ListSessions() ([]Session, error)
	SessionExists(name string) (bool, error)
	CreateSession(name string) error
	EnsureSession(name string) error
	KillSession(name string) error
	AttachSession(name string) error
	AttachCommand(name string) (string, []string, error)
	GetStatus() (Status, error)
	CheckInstalled() error
}

// Manager implements TmuxManager using the system tmux binary via exec.Command.
type Manager struct {
	binaryName string
	socketName string
}

// NewManager creates a Manager targeting the default tmux server socket.
func NewManager() *Manager {
	return &Manager{
		binaryName: "tmux",
	}
}

// NewManagerWithConfig creates a Manager with a custom binary name or isolated socket name (-L).
func NewManagerWithConfig(binaryName string, socketName string) *Manager {
	if binaryName == "" {
		binaryName = "tmux"
	}
	return &Manager{
		binaryName: binaryName,
		socketName: socketName,
	}
}

// CheckInstalled verifies that the tmux executable is available on the system.
func (m *Manager) CheckInstalled() error {
	path, err := exec.LookPath(m.binaryName)
	if err != nil || path == "" {
		return fmt.Errorf(
			"%w: %q executable not found in PATH; please install tmux (e.g. 'apt-get install tmux' or 'brew install tmux') before using --tmux",
			ErrTmuxNotInstalled,
			m.binaryName,
		)
	}
	return nil
}

func (m *Manager) resolveBinary() (string, error) {
	path, err := exec.LookPath(m.binaryName)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrTmuxNotInstalled, err)
	}
	return path, nil
}

func (m *Manager) buildArgs(subArgs ...string) []string {
	if m.socketName != "" {
		args := make([]string, 0, len(subArgs)+2)
		args = append(args, "-L", m.socketName)
		args = append(args, subArgs...)
		return args
	}
	return subArgs
}

func (m *Manager) run(subArgs ...string) (string, string, error) {
	binPath, err := m.resolveBinary()
	if err != nil {
		return "", "", err
	}

	args := m.buildArgs(subArgs...)
	cmd := exec.Command(binPath, args...)
	cmd.Env = SanitizedEnv(os.Environ())

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	return stdout.String(), stderr.String(), err
}

// SanitizedEnv prepares a clean environment for tmux commands/clients:
// - Removes TMUX and TMUX_PANE so GoTTY works even when started inside an outer tmux session.
// - Sets TERM=xterm-256color if TERM is unset or "dumb".
func SanitizedEnv(base []string) []string {
	env := make([]string, 0, len(base)+1)
	termVal := ""
	for _, kv := range base {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		if strings.HasPrefix(kv, "TERM=") {
			termVal = strings.TrimPrefix(kv, "TERM=")
			if termVal == "" || termVal == "dumb" {
				continue
			}
		}
		env = append(env, kv)
	}
	if termVal == "" || termVal == "dumb" {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}

func isNoServerOutput(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "error connecting to") ||
		strings.Contains(lower, "failed to connect to server")
}

func isNoSessionsOutput(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "no sessions")
}

func isSessionNotFoundOutput(stderr string) bool {
	lower := strings.ToLower(stderr)
	return isNoServerOutput(lower) ||
		isNoSessionsOutput(lower) ||
		strings.Contains(lower, "can't find session") ||
		strings.Contains(lower, "session not found")
}

// ListSessions returns all active tmux sessions, or an empty slice if none exist or server is stopped.
func (m *Manager) ListSessions() ([]Session, error) {
	sessions, _, err := m.listSessionsWithState()
	if err != nil {
		return []Session{}, err
	}
	return sessions, nil
}

func (m *Manager) listSessionsWithState() ([]Session, bool, error) {
	stdout, stderr, err := m.run("list-sessions", "-F", "#{session_name}|#{session_windows}|#{session_attached}")
	if err != nil {
		if isNoServerOutput(stderr) {
			return []Session{}, false, nil
		}
		if isNoSessionsOutput(stderr) {
			return []Session{}, true, nil
		}
		return []Session{}, false, fmt.Errorf("tmux list-sessions failed: %s (%w)", strings.TrimSpace(stderr), err)
	}

	sessions := make([]Session, 0)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		session, ok := parseSessionLine(line)
		if ok {
			sessions = append(sessions, session)
		}
	}
	return sessions, true, nil
}

func parseSessionLine(line string) (Session, bool) {
	// Parse from the right so session names stay intact.
	lastSep := strings.LastIndex(line, "|")
	if lastSep <= 0 {
		return Session{}, false
	}
	attachedStr := line[lastSep+1:]
	rest := line[:lastSep]

	prevSep := strings.LastIndex(rest, "|")
	if prevSep <= 0 {
		return Session{}, false
	}
	windowsStr := rest[prevSep+1:]
	name := rest[:prevSep]

	windows, err := strconv.Atoi(strings.TrimSpace(windowsStr))
	if err != nil {
		windows = 0
	}
	attached, err := strconv.Atoi(strings.TrimSpace(attachedStr))
	if err != nil {
		attached = 0
	}

	return Session{
		Name:     name,
		Windows:  windows,
		Attached: attached,
	}, true
}

// SessionExists checks whether an exact tmux session exists.
func (m *Manager) SessionExists(name string) (bool, error) {
	if err := ValidateSessionName(name); err != nil {
		return false, err
	}

	canonical := CanonicalSessionName(name)
	_, stderr, err := m.run("has-session", "-t", "="+canonical)
	if err != nil {
		if isSessionNotFoundOutput(stderr) {
			return false, nil
		}
		return false, fmt.Errorf("tmux has-session failed: %s (%w)", strings.TrimSpace(stderr), err)
	}
	return true, nil
}

// CreateSession creates a detached tmux session. Returns ErrSessionExists if it already exists.
func (m *Manager) CreateSession(name string) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}

	exists, err := m.SessionExists(name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: %s", ErrSessionExists, CanonicalSessionName(name))
	}

	canonical := CanonicalSessionName(name)
	_, stderr, err := m.run("new-session", "-d", "-s", canonical)
	if err != nil {
		if strings.Contains(strings.ToLower(stderr), "duplicate session") {
			return fmt.Errorf("%w: %s", ErrSessionExists, canonical)
		}
		return fmt.Errorf("tmux new-session failed: %s (%w)", strings.TrimSpace(stderr), err)
	}
	return nil
}

// EnsureSession creates the session if it does not already exist, succeeding if it already exists.
func (m *Manager) EnsureSession(name string) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}

	exists, err := m.SessionExists(name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	err = m.CreateSession(name)
	if err != nil && strings.Contains(err.Error(), ErrSessionExists.Error()) {
		return nil
	}
	return err
}

// KillSession terminates the exact specified tmux session (using -t =name to prevent prefix matching).
func (m *Manager) KillSession(name string) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}

	// Strict exact-name check so we never kill a different session.
	_, stderr, err := m.run("has-session", "-t", "="+name)
	if err != nil {
		if isSessionNotFoundOutput(stderr) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		return fmt.Errorf("tmux has-session failed: %s (%w)", strings.TrimSpace(stderr), err)
	}

	_, stderr, err = m.run("kill-session", "-t", "="+name)
	if err != nil {
		if isSessionNotFoundOutput(stderr) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		return fmt.Errorf("tmux kill-session failed: %s (%w)", strings.TrimSpace(stderr), err)
	}
	return nil
}

// AttachSession ensures the target session exists so a client can attach to it.
func (m *Manager) AttachSession(name string) error {
	return m.EnsureSession(name)
}

// AttachCommand returns the executable path and argument list to launch an interactive tmux client
// inside a PTY (`tmux new-session -A -s <canonicalName>`).
func (m *Manager) AttachCommand(name string) (string, []string, error) {
	if err := ValidateSessionName(name); err != nil {
		return "", nil, err
	}
	binPath, err := m.resolveBinary()
	if err != nil {
		return "", nil, err
	}
	canonical := CanonicalSessionName(name)
	args := m.buildArgs("new-session", "-A", "-s", canonical)
	return binPath, args, nil
}

// GetStatus returns whether tmux is installed, whether the tmux server is running, and session count.
func (m *Manager) GetStatus() (Status, error) {
	if err := m.CheckInstalled(); err != nil {
		return Status{
			Installed:     false,
			ServerRunning: false,
			SessionCount:  0,
		}, nil
	}

	sessions, running, err := m.listSessionsWithState()
	if err != nil {
		// Even on unexpected tmux query error, report installed=true safely without crashing HTTP server.
		return Status{
			Installed:     true,
			ServerRunning: false,
			SessionCount:  0,
		}, err
	}

	return Status{
		Installed:     true,
		ServerRunning: running,
		SessionCount:  len(sessions),
	}, nil
}
