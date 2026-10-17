package main

// matrix/hardened: AI package installation and shared-backend restart are
// independent operations. Merely inspecting this command never checks releases.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
	"github.com/asheshgoplani/agent-deck/internal/session"
)

type aiUpdateService interface {
	Inspect(context.Context) (aiupdate.Status, error)
	Check(context.Context) (aiupdate.Status, error)
	Install(context.Context) (aiupdate.Status, error)
	Restart(context.Context) (aiupdate.Status, error)
}

func handleAIUpdate(profile string, args []string) {
	err := runAIUpdate(context.Background(), args, os.Stdout, os.Stderr, func() (aiUpdateService, error) {
		cfg, err := session.LoadUserConfig()
		if err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}
		binary, home := "codex", strings.TrimSpace(os.Getenv("CODEX_HOME"))
		if cfg != nil {
			if strings.TrimSpace(cfg.Codex.Command) != "" {
				binary = strings.TrimSpace(cfg.Codex.Command)
			}
			if home == "" {
				home = cfg.GetProfileCodexConfigDir(profile)
			}
			if home == "" {
				home = cfg.Codex.ConfigDir
			}
		}
		if home != "" {
			home = session.ExpandPath(home)
		}
		return aiupdate.New(binary, home), nil
	})
	if err != nil {
		os.Exit(1)
	}
}

func runAIUpdate(ctx context.Context, args []string, stdout, stderr io.Writer, factory func() (aiUpdateService, error)) error {
	fs := flag.NewFlagSet("ai-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	yes := fs.Bool("yes", false, "Confirm restarting the shared backend (may interrupt all connected tasks)")
	fs.Usage = func() {
		fmt.Fprintln(stdout, "Usage: agent-deck ai-update [status|check|install|restart] [--json] [--yes]")
		fmt.Fprintln(stdout, "\nManage Codex on this host in the selected profile's Codex home.")
		fmt.Fprintln(stdout, "  status   Read CLI, installed backend and running backend versions (no release request)")
		fmt.Fprintln(stdout, "  check    Check the latest official stable Codex release")
		fmt.Fprintln(stdout, "  install  Install for the next backend start; keep current tasks running")
		fmt.Fprintln(stdout, "           Pins the installed backend and disables Codex's native automatic updater.")
		fmt.Fprintln(stdout, "  restart  Activate the installed backend now; requires --yes and may interrupt tasks")
		fmt.Fprintln(stdout, "\nOpening another Codex client reuses a running backend; it does not activate the new package.")
		fmt.Fprintln(stdout, "Restart only when all users of this shared backend are ready. Old releases are retained.")
		fmt.Fprintln(stdout, "\nOptions:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	fail := func(err error, st *aiupdate.Status) error {
		if *jsonOutput {
			payload := map[string]any{"success": false, "error": err.Error(), "code": ErrCodeInvalidOperation}
			if st != nil {
				payload["status"] = st
			}
			if encodeErr := json.NewEncoder(stdout).Encode(payload); encodeErr != nil {
				return encodeErr
			}
		} else {
			fmt.Fprintf(stderr, "Error: %v\n", err)
		}
		return err
	}
	action := "status"
	if fs.NArg() > 1 {
		return fail(errors.New("expected one action: status, check, install, or restart"), nil)
	}
	if fs.NArg() == 1 {
		action = fs.Arg(0)
	}
	if action != "status" && action != "check" && action != "install" && action != "restart" {
		return fail(fmt.Errorf("unknown AI update action %q", action), nil)
	}
	if action == "restart" && !*yes {
		return fail(errors.New("restart affects every client of this shared backend and may interrupt running tasks; choose a suitable time and repeat with --yes"), nil)
	}
	svc, err := factory()
	if err != nil {
		return fail(err, nil)
	}
	timeout := 10 * time.Second
	switch action {
	case "check":
		timeout = 30 * time.Second
	case "install":
		timeout = 15 * time.Minute
	case "restart":
		timeout = 6 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var st aiupdate.Status
	switch action {
	case "status":
		st, err = svc.Inspect(ctx)
	case "check":
		st, err = svc.Check(ctx)
	case "install":
		st, err = svc.Install(ctx)
	case "restart":
		st, err = svc.Restart(ctx)
	}
	if err != nil {
		return fail(err, &st)
	}
	if *jsonOutput {
		return json.NewEncoder(stdout).Encode(map[string]any{"success": true, "action": action, "status": st})
	}
	_, err = fmt.Fprint(stdout, formatAIUpdateStatus(st))
	return err
}

func formatAIUpdateStatus(st aiupdate.Status) string {
	var b strings.Builder
	show := func(s string) string {
		if s == "" {
			return "unknown"
		}
		return s
	}
	fmt.Fprintf(&b, "Codex on this host (%s)\n", st.Home)
	fmt.Fprintf(&b, "  CLI:       %s\n  Installed: %s\n  Running:   %s (%s)\n", show(st.CLIVersion), show(st.InstalledVersion), show(st.RunningVersion), show(st.DaemonStatus))
	if st.LatestVersion != "" {
		fmt.Fprintf(&b, "  Latest:    %s\n", st.LatestVersion)
	}
	if st.Pending {
		b.WriteString("  Installed, waiting for the next shared-backend start. Current tasks keep using the running version.\n")
	}
	if st.Managed && !st.NativeAutoUpdate {
		b.WriteString("  Native automatic updates are off. Installation and backend restart are separate.\n")
	}
	return b.String()
}
