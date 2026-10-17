package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// matrix/hardened: the production TUI has a connected channel, unlike a CLI
// smoke check. Directory completion must bypass that channel's verb registry.
func TestMatrixDirectoryCompletionBypassesConnectedChannel(t *testing.T) {
	t.Setenv("AGENT_DECK_REMOTE_CHANNEL", "1")
	t.Cleanup(CloseRemoteChannels)
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	script := `#!/bin/sh
for last; do :; done
printf '%s\n' "$last" >> "$DIRECTORY_SSH_CALLS"
printf '%s\n' '["/remote/333.dev"]'
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DIRECTORY_SSH_CALLS", calls)
	agent := newFakeAgent(t, true, false)
	r := &SSHRunner{name: "directory-channel-test", Host: "user@remote", Profile: "default", AgentDeckPath: "/bin/agent-deck", dialChannelFn: agent.dial}
	ch := channelFor(r)
	if ch == nil || !waitUntil(t, 2*time.Second, ch.Connected) {
		t.Fatal("channel did not connect")
	}
	if out, err := r.Run(context.Background(), "list", "--json"); err != nil || string(out) != "out:list --json" {
		t.Fatalf("listing should use connected channel: %s, %v", out, err)
	}
	out, err := r.Run(context.Background(), "__complete-path", "/remote/333.")
	if err != nil || strings.TrimSpace(string(out)) != `["/remote/333.dev"]` {
		t.Fatalf("completion must use exec: %s, %v", out, err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "'__complete-path' '/remote/333.'") || strings.Contains(string(data), "'list'") {
		t.Fatalf("unexpected exec routing: %s", data)
	}
	if remoteChannelArgsSafe([]string{"__complete-path", "/remote/333."}) {
		t.Fatal("completion was admitted to persistent channel")
	}
}
