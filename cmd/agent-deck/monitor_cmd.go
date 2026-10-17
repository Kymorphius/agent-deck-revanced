package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/core/daemon"
	"github.com/asheshgoplani/agent-deck/internal/monitor"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// matrix/hardened: this read-only user collector is separate from the
// per-profile command daemon. Starting it installs no hooks or service units.
const monitorUsage = `Usage: agent-deck monitor <serve|status|stop>

Share complete runtime observations across this user's TUI and Web processes.
The private local socket accepts the same OS user only.

  serve            Run the collector until SIGINT/SIGTERM or stop
  status [--json]  Read collector identity and sampling counters
  stop             Stop only the collector; AI panes keep running`

func handleMonitor(args []string) {
	if len(args) == 0 || hooksHelpRequested(args) {
		fmt.Println(monitorUsage)
		return
	}
	fs := flag.NewFlagSet("monitor "+args[0], flag.ContinueOnError)
	jsonMode := fs.Bool("json", false, "Print JSON status")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		os.Exit(1)
	}
	paths, err := monitor.Paths()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	switch args[0] {
	case "serve":
		ensureTmuxInPathOrExit()
		owner, err := monitor.Acquire(paths)
		if err != nil {
			var running *daemon.AlreadyRunningError
			if errors.As(err, &running) {
				return
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// matrix/hardened: finish resource cleanup before the error path os.Exit.
		err = func() error {
			defer owner.Close()
			defer initDaemonLogging()()
			queries := tmux.EnableQueryTransport(ctx)
			server := monitor.New(monitor.Options{Version: Version, QueryTransport: queries})
			defer server.Close()
			return server.Serve(ctx, owner.Listener())
		}()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "status", "stop":
		client := monitor.NewClient(paths.Socket, nil)
		defer client.Close()
		requestCtx, requestCancel := context.WithTimeout(ctx, 3*time.Second)
		defer requestCancel()
		if args[0] == "stop" {
			err = client.Stop(requestCtx)
			if err == nil {
				fmt.Println("Monitor stopped; AI sessions remain running.")
			}
		} else {
			status, statusErr := client.Status(requestCtx)
			err = statusErr
			if err == nil {
				if *jsonMode {
					err = json.NewEncoder(os.Stdout).Encode(status)
				} else {
					fmt.Printf("Monitor PID %d, version %s\n", status.PID, status.Version)
				}
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, monitorUsage)
		os.Exit(1)
	}
}

// newRuntimeMonitor is lazy: constructing Home or an HTTP server does not
// start any profile sampling. The first observation request starts this exact
// executable, and competing callers converge on the socket's exclusive owner.
func newRuntimeMonitor() (*monitor.Client, error) {
	paths, err := monitor.Paths()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var attempted time.Time
	ensure := func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		probe := monitor.NewClient(paths.Socket, nil)
		defer probe.Close()
		if _, err := probe.Status(ctx); err == nil {
			return nil
		} else if !monitor.IsAbsent(err) {
			return err
		}
		if time.Since(attempted) > 5*time.Second {
			attempted = time.Now()
			cmd := exec.Command(exe, "monitor", "serve")
			cmd.Env = os.Environ()
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("start runtime monitor: %w", err)
			}
			go func() { _ = cmd.Wait() }()
		}
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return fmt.Errorf("runtime monitor did not become available")
			case <-tick.C:
				if _, err := probe.Status(ctx); err == nil {
					return nil
				} else if !monitor.IsAbsent(err) {
					return err
				}
			}
		}
	}
	return monitor.NewClient(paths.Socket, ensure), nil
}
