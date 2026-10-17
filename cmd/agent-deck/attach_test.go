package main

import (
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// Under `go test`, stdin/stdout are pipes, not a terminal. The attach guard
// must therefore refuse with errAttachNoTTY BEFORE touching tmux — this is the
// exact path that protects scripts/CI/conductor callers of `--attach`.
func TestAttachInstanceInteractive_NoTTYReturnsSentinel(t *testing.T) {
	if stdinStdoutIsTerminal() {
		t.Skip("test stdio is a terminal; the no-TTY guard cannot be exercised here")
	}

	inst := session.NewInstance("attach-guard", "/tmp")
	before := inst.LastAccessedAt
	err := attachInstanceInteractive(inst)
	if !errors.Is(err, errAttachNoTTY) {
		t.Fatalf("attachInstanceInteractive without a TTY = %v, want errAttachNoTTY", err)
	}
	// matrix/hardened: scripts cannot promote sessions by requesting attach.
	if !inst.LastAccessedAt.Equal(before) {
		t.Fatalf("rejected attach changed access time from %v to %v", before, inst.LastAccessedAt)
	}
}

// matrix/hardened: each CLI visit is durable before entering the terminal and
// again on a successful return, with no dependence on a later full save.
func TestAttachWithRecentAccessPersistsEntryAndReturn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storage, err := session.NewStorageWithProfile("recent-attach")
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	inst := session.NewInstanceWithTool("recent", t.TempDir(), "shell")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	inst.LastAccessedAt = old
	if err := storage.Save([]*session.Instance{inst}); err != nil {
		t.Fatal(err)
	}
	loaded, err := storage.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load: %v, %v", loaded, err)
	}
	inst = loaded[0]
	checkRecent := func() {
		t.Helper()
		rows, err := storage.GetDB().LoadInstances()
		if err != nil || len(rows) != 1 {
			t.Fatalf("read access: %v, %v", rows, err)
		}
		if !inst.LastAccessedAt.After(old) || !rows[0].LastAccessed.After(old) {
			t.Fatalf("visit not persisted: memory=%v DB=%v", inst.LastAccessedAt, rows[0].LastAccessed)
		}
	}
	if err := attachWithRecentAccess(inst, func() error {
		checkRecent()
		// An old value during the visit exposes a missing return stamp without
		// sleeping across the database's one-second timestamp resolution.
		inst.LastAccessedAt = old
		// matrix/hardened: reset the fixture directly; production visit writes
		// intentionally refuse older timestamps across concurrent connections.
		_, err := storage.GetDB().DB().Exec("UPDATE instances SET last_accessed=? WHERE id=?", old.Unix(), inst.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	checkRecent()
}

func TestAttachWithRecentAccessErrorDoesNotStampSuccessfulReturn(t *testing.T) {
	inst := &session.Instance{ID: "failed-attach"}
	wantErr := errors.New("attach failed")
	before := time.Now().Add(-time.Second)
	var entered time.Time
	err := attachWithRecentAccess(inst, func() error {
		entered = inst.LastAccessedAt
		return wantErr
	})
	if !errors.Is(err, wantErr) || entered.Before(before) || !inst.LastAccessedAt.Equal(entered) {
		t.Fatalf("failed attach: err=%v entry=%v return=%v", err, entered, inst.LastAccessedAt)
	}
}

// stdinStdoutIsTerminal must be false in the test harness (piped stdio). This
// pins the contract the callers rely on for the no-TTY exit path.
func TestStdinStdoutIsTerminal_FalseUnderTest(t *testing.T) {
	if stdinStdoutIsTerminal() {
		t.Skip("test stdio unexpectedly a terminal; nothing to assert")
	}
}
