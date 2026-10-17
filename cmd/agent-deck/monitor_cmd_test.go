package main

import (
	"os"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/monitor"
)

// matrix/hardened: CLI/TUI setup must stay lazy, and control/status must not
// accidentally start a profile sampler before an authorized observation read.
func TestRuntimeMonitorClientConstructionIsLazy(t *testing.T) {
	paths, err := monitor.Paths()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.Dir); !os.IsNotExist(err) {
		t.Fatalf("fixture monitor dir already exists: %v", err)
	}
	client, err := newRuntimeMonitor()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(paths.Dir); !os.IsNotExist(err) {
		t.Fatalf("construction activated collector: %v", err)
	}
	if !commandRegistry["monitor"] {
		t.Fatal("monitor not registered for global profile flag parsing")
	}
}
