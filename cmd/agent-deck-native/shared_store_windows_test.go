//go:build windows

package main

// matrix/hardened: the real detached CLI must inherit its selected StateDB,
// and a shared-store mutation cannot act through a prototype manager pipe.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/google/uuid"
)

// matrix/hardened: the displayed default group must round-trip through the
// shared store without changing its persisted group ID or process generation.
func TestWindowsSharedUIGroupUsesOriginalDefaultGroup(t *testing.T) {
	cli := nativeBootstrapCLI(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := options{Store: "statedb", StateDir: filepath.Join(parent, "shared-ui"), Pipe: winipc.PipePrefix + "shared-ui-" + uuid.NewString()}
	var identity terminalhost.Response
	nativeBootstrapCleanup(t, config, &identity)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, cli, "manager-start", "--store", config.Store, "--state-dir", config.StateDir, "--pipe", config.Pipe, "--json").CombinedOutput()
	if err != nil {
		t.Fatalf("shared UI bootstrap failed: %v %s", err, output)
	}
	if err := json.Unmarshal(output, &identity); err != nil {
		t.Fatal(err)
	}
	if err := validateNativeManager(config, identity); err != nil {
		t.Fatal(err)
	}
	service := &nativeService{pipe: config.Pipe, store: config.Store}
	created, err := service.Create(ctx, "默认分组会话", "shell", parent)
	if err != nil || created.Group != "" || created.ID == "" || created.RunID == "" {
		t.Fatalf("UI create did not select its default group: %+v %v", created, err)
	}
	for _, group := range []string{"研发", ""} {
		if err := service.Move(ctx, created.ID, created.RunID, group); err != nil {
			t.Fatal(err)
		}
		items, err := service.List(ctx)
		if err != nil || len(items) != 1 || items[0].Group != group || items[0].RunID != created.RunID || items[0].PID != created.PID {
			t.Fatalf("UI group move changed process or presentation: %+v %v", items, err)
		}
	}
	raw := nativeCall(t, config.Pipe, terminalhost.Request{Op: "fleet-list"})
	if len(raw.Sessions) != 1 || raw.Sessions[0].Group != nativestatestore.DefaultGroup {
		t.Fatalf("UI translation changed persisted default group: %+v", raw.Sessions)
	}
	if err := service.Stop(ctx, created.ID, created.RunID); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsSharedStoreCLIUsesOriginalSchemaAndRefusesForeignStore(t *testing.T) {
	cli := nativeBootstrapCLI(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := options{Store: "statedb", StateDir: filepath.Join(parent, "shared"), Pipe: winipc.PipePrefix + "shared-cli-" + uuid.NewString()}
	var identity terminalhost.Response
	nativeBootstrapCleanup(t, config, &identity)
	run := func(config options, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		args = append(args, "--state-dir", config.StateDir, "--pipe", config.Pipe)
		if config.Store != "" {
			args = append(args, "--store", config.Store)
		}
		return exec.CommandContext(ctx, cli, args...).CombinedOutput()
	}
	data, err := run(config, "manager-start", "--json")
	if err != nil {
		t.Fatalf("shared CLI bootstrap: %v %s", err, data)
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatal(err)
	}
	if err := validateNativeManager(config, identity); err != nil {
		t.Fatal(err)
	}
	data, err = run(config, "create", "--name", "原数据库会话", "--tool", "shell", "--cwd", parent, "--group", "研发/Windows", "--no-start", "--json")
	if err != nil {
		t.Fatalf("shared CLI create: %v %s", err, data)
	}
	var created terminalhost.Response
	if err := json.Unmarshal(data, &created); err != nil || len(created.Sessions) != 1 {
		t.Fatalf("invalid create: %v %s", err, data)
	}
	db, err := statedb.OpenReadOnlyLive(filepath.Join(config.StateDir, nativestatestore.DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	rows, err := db.LoadInstances()
	if err != nil || len(rows) != 1 || rows[0].ID != created.Sessions[0].ID || rows[0].Title != "原数据库会话" || rows[0].Status != "unknown" || rows[0].TmuxSession != "" {
		t.Fatalf("original schema readback: rows=%+v error=%v", rows, err)
	}
	if !bytes.Contains(rows[0].ToolData, []byte(`"native_backend"`)) {
		t.Fatal("original schema lost native descriptor")
	}
	if _, err := os.Stat(filepath.Join(config.StateDir, "registry.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("shared CLI created a second registry: %v", err)
	}
	wrongPrototype := config
	wrongPrototype.Store = "prototype"
	if data, err = run(wrongPrototype, "create", "--name", "must-not-write-shared", "--cwd", parent, "--no-start", "--json"); err == nil {
		t.Fatalf("prototype mutation accepted shared manager: %s", data)
	}
	if rows, err = db.LoadInstances(); err != nil || len(rows) != 1 || rows[0].ID != created.Sessions[0].ID {
		t.Fatalf("prototype refusal changed shared inventory: rows=%+v error=%v", rows, err)
	}
	prototype := options{Store: "prototype", StateDir: filepath.Join(parent, "prototype"), Pipe: winipc.PipePrefix + "prototype-cli-" + uuid.NewString()}
	var otherIdentity terminalhost.Response
	nativeBootstrapCleanup(t, prototype, &otherIdentity)
	data, err = run(prototype, "manager-start", "--json")
	if err != nil {
		t.Fatalf("prototype fixture bootstrap: %v %s", err, data)
	}
	if err := json.Unmarshal(data, &otherIdentity); err != nil {
		t.Fatal(err)
	}
	wrong := prototype
	wrong.Store = "statedb"
	if data, err = run(wrong, "create", "--name", "must-not-write", "--cwd", parent, "--no-start", "--json"); err == nil {
		t.Fatalf("shared mutation accepted foreign manager: %s", data)
	}
	response := nativeCall(t, prototype.Pipe, terminalhost.Request{Op: "fleet-list"})
	if len(response.Sessions) != 0 {
		t.Fatal("foreign store refusal changed prototype inventory")
	}
}
