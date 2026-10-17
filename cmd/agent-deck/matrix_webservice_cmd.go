package main

// matrix/hardened: `agent-deck webservice` shows, starts, stops and restarts
// the user's own long-running `agent-deck web` service on this host (see
// internal/session/matrix_webservice.go). The TUI's alt+w panel runs it here
// or on a remote over SSH, so both agree on what the state is.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func handleWebService(args []string) {
	fs := flag.NewFlagSet("webservice", flag.ExitOnError)
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck webservice [status|start|stop|restart] [--json]")
		fmt.Println()
		fmt.Println("Show or control this host's own `agent-deck web` service: a LaunchAgent")
		fmt.Println("or systemd unit you installed yourself. agent-deck never installs it.")
		fmt.Println("Stopping a LaunchAgent unloads it until the next login or `start`.")
		fmt.Println()
		fmt.Println("Configure it in config.toml:")
		fmt.Println("  [webservice]")
		fmt.Println("  manager = \"launchd\"            # launchd | systemd | systemd-user")
		fmt.Println("  service = \"my.agent-deck-web\"  # launchd label or systemd unit")
		fmt.Println("  url = \"http://127.0.0.1:8420\"  # optional: probe <url>/healthz")
		fmt.Println("  # plist = \"...\"                # launchd: default ~/Library/LaunchAgents/<service>.plist")
		fmt.Println("  # sudo = true                  # systemd: control via `sudo -n systemctl`")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	action := "status"
	switch fs.NArg() {
	case 0:
	case 1:
		action = fs.Arg(0)
	default:
		fs.Usage()
		os.Exit(2)
	}
	out := NewCLIOutput(*jsonOutput, false)
	cfg, err := session.LoadUserConfig()
	if err != nil || cfg == nil {
		out.Error(fmt.Sprintf("load config: %v", err), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	settings := cfg.WebService

	switch action {
	case "status":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st := session.CheckWebService(ctx, settings)
		out.Print(formatWebServiceStatus(st), map[string]any{"success": true, "status": st})
	case session.WebServiceStart, session.WebServiceStop, session.WebServiceRestart:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		st, err := session.ControlWebService(ctx, settings, action)
		if err != nil {
			out.ErrorWithData(err.Error(), ErrCodeInvalidOperation, map[string]any{"status": st})
			os.Exit(1)
		}
		out.Print(formatWebServiceStatus(st), map[string]any{"success": true, "action": action, "status": st})
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func formatWebServiceStatus(st session.WebServiceStatus) string {
	if !st.Configured {
		return "No web service configured on this host (see `agent-deck webservice --help`).\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s): %s\n", st.Service, st.Manager, st.Summary())
	if st.State != "" {
		fmt.Fprintf(&b, "  state:  %s\n", st.State)
	}
	if st.ManagerError != "" {
		fmt.Fprintf(&b, "  error:  %s\n", st.ManagerError)
	}
	if st.URL != "" {
		fmt.Fprintf(&b, "  url:    %s\n", st.URL)
	}
	if st.HealthError != "" {
		fmt.Fprintf(&b, "  health: %s\n", st.HealthError)
	}
	return b.String()
}
