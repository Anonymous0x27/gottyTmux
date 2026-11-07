package server

import (
	"github.com/yudai/gotty/webtty"
)

// Slave is webtty.Slave with some additional methods.
type Slave interface {
	webtty.Slave

	Close() error
}

type Factory interface {
	Name() string
	New(params map[string][]string) (Slave, error)
}

// SessionFactory is an optional extension of Factory for session-scoped PTY slaves (e.g. tmux mode).
type SessionFactory interface {
	Factory
	NewSessionSlave(sessionName string) (Slave, error)
}
