package main

// matrix/hardened: foreground ownership makes host lifetime explicit without installing persistence.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"

	"github.com/asheshgoplani/agent-deck/internal/conpty"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
)

func runHost(config options, stdout io.Writer) error {
	listener, err := winipc.Listen(config.Pipe)
	if err != nil {
		return err
	}
	defer listener.Close()
	process, err := conpty.Start(conpty.Options{
		Executable: config.Executable, Args: config.Args, Env: mergedEnv(os.Environ(), config.Env),
		Dir: config.Cwd, Cols: config.Cols, Rows: config.Rows,
	})
	if err != nil {
		return err
	}
	defer process.Close()
	server, err := terminalhost.New(process, config.Cols, config.Rows)
	if err != nil {
		return err
	}
	defer server.Close()
	// matrix/hardened: ready output contains process identity but never argv/environment contents.
	if err := json.NewEncoder(stdout).Encode(struct {
		Ready   bool   `json:"ready"`
		Pipe    string `json:"pipe"`
		PID     uint32 `json:"pid"`
		HostPID int    `json:"host_pid"`
	}{true, config.Pipe, process.PID(), os.Getpid()}); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
