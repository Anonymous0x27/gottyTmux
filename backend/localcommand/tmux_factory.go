package localcommand

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/yudai/gotty/server"
	"github.com/yudai/gotty/tmux"
)

// TmuxFactory creates PTY-backed tmux client Slaves (`tmux new-session -A -s <name>`) per WebSocket connection.
type TmuxFactory struct {
	manager tmux.TmuxManager
	options *Options
}

// NewTmuxFactory creates a TmuxFactory backed by the provided TmuxManager.
func NewTmuxFactory(manager tmux.TmuxManager, options *Options) (*TmuxFactory, error) {
	if manager == nil {
		return nil, fmt.Errorf("tmux manager cannot be nil")
	}
	if options == nil {
		options = &Options{
			CloseSignal:  int(syscall.SIGHUP),
			CloseTimeout: -1,
		}
	}
	return &TmuxFactory{
		manager: manager,
		options: options,
	}, nil
}

func (f *TmuxFactory) Name() string {
	return "tmux client"
}

// NewSessionSlave spawns an independent tmux client PTY attached to sessionName.
func (f *TmuxFactory) NewSessionSlave(sessionName string) (server.Slave, error) {
	binPath, argv, err := f.manager.AttachCommand(sessionName)
	if err != nil {
		return nil, err
	}

	closeSig := syscall.Signal(f.options.CloseSignal)
	if closeSig == 0 {
		closeSig = syscall.SIGHUP
	}
	closeTimeout := 5 * time.Second
	if f.options.CloseTimeout >= 0 {
		closeTimeout = time.Duration(f.options.CloseTimeout) * time.Second
	}

	canonical := tmux.CanonicalSessionName(sessionName)
	opts := []Option{
		WithCloseSignal(closeSig),
		WithCloseTimeout(closeTimeout),
		WithEnv(tmux.SanitizedEnv(os.Environ())),
		WithTitleVariables(map[string]interface{}{
			"command": "tmux:" + canonical,
			"session": canonical,
		}),
	}

	return New(binPath, argv, opts...)
}

// New implements server.Factory.
func (f *TmuxFactory) New(params map[string][]string) (server.Slave, error) {
	sessions := params["session"]
	if len(sessions) == 0 || sessions[0] == "" {
		return nil, fmt.Errorf("missing tmux session name")
	}
	return f.NewSessionSlave(sessions[0])
}
