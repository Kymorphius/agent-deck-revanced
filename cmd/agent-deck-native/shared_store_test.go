package main

// matrix/hardened: store identity is explicit through parsing and bootstrap;
// selecting StateDB cannot reuse or replace a prototype manager accidentally.

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

func TestSharedStoreNamespaceIsExplicitAndStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	parse := func(command, store string) options {
		t.Helper()
		o, err := parseOptions([]string{command, "--state-dir", dir, "--store", store}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	prototype := parse("serve", "prototype")
	shared := parse("serve", "statedb")
	again := parse("list", "statedb")
	if shared.Pipe == prototype.Pipe || shared.Pipe != again.Pipe || shared.StateDir != prototype.StateDir {
		t.Fatalf("store namespace was ambiguous: prototype=%+v shared=%+v again=%+v", prototype, shared, again)
	}
	if _, err := parseOptions([]string{"serve", "--store", "unknown"}, io.Discard); err == nil {
		t.Fatal("accepted an unknown store")
	}
}

func TestSharedStoreBootstrapRefusesWrongStoreOnExplicitPipe(t *testing.T) {
	config := bootstrapTestConfig(t)
	config.Store = "statedb"
	response := bootstrapTestResponse(config) // Same version, but prototype store.
	spawns := 0
	_, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
		probe:  func(context.Context, string) (terminalhost.Response, error) { return response, nil },
		spawn:  func(context.Context, options) (uint32, error) { spawns++; return 99, nil },
		absent: bootstrapTestIsAbsent,
	})
	if err == nil || spawns != 0 {
		t.Fatalf("wrong store caused reuse or replacement: error=%v spawns=%d", err, spawns)
	}
	response.ManagerVersion = nativeManagerVersion(config)
	if err := validateNativeManager(config, response); err != nil {
		t.Fatalf("matching StateDB identity rejected: %v", err)
	}
}

func TestSharedStoreBootstrapPassesSelectionToDetachedChild(t *testing.T) {
	config := bootstrapTestConfig(t)
	config.Store = "statedb"
	response := bootstrapTestResponse(config)
	response.ManagerVersion = nativeManagerVersion(config)
	probes := 0
	_, err := ensureNativeManagerWith(context.Background(), config, nativeBootstrapDeps{
		probe: func(context.Context, string) (terminalhost.Response, error) {
			probes++
			if probes == 1 {
				return terminalhost.Response{}, bootstrapTestAbsent
			}
			return response, nil
		},
		spawn: func(_ context.Context, received options) (uint32, error) {
			if received.Store != "statedb" || received.StateDir != config.StateDir {
				t.Fatal("child lost selected store")
			}
			return 99, nil
		},
		absent: bootstrapTestIsAbsent,
	})
	if err != nil {
		t.Fatal(err)
	}
}
