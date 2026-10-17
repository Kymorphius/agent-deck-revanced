package statedb

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRuntimeObservationGuardRejectsConcurrentConfigAndStop(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	if err := db.SaveInstance(&InstanceRow{ID: "runtime", Title: "runtime", Tool: "codex", Status: "running", ToolData: json.RawMessage(`{"codex_session_id":"original"}`), CreatedAt: now, LastAccessed: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.Touch(); err != nil {
		t.Fatal(err)
	}
	fence, err := db.LastModified()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Touch(); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.WriteObservedStatusIfCurrent("runtime", "running", "waiting", "codex", fence); err != nil || ok {
		t.Fatalf("stale status accepted=%v err=%v", ok, err)
	}
	for name, write := range map[string]func() error{
		"codex":  func() error { return db.WriteCodexSessionBinding("runtime", "stale", now, fence) },
		"claude": func() error { return db.WriteClaudeSessionBinding("runtime", "stale", now, fence) },
		"gemini": func() error { return db.WriteGeminiSessionBinding("runtime", "stale", now, fence) },
		"generic": func() error {
			return db.WriteGenericSessionBinding("runtime", "stale", "custom", "custom", "local:/p", now, fence)
		},
		"activity": func() error { return db.WriteLastActivityAt("runtime", now, fence) },
		"link":     func() error { return db.UpsertSessionLink("runtime", "codex", "stale", "", true, fence) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := write(); !errors.Is(err, ErrObservationStale) {
				t.Fatalf("guard=%v", err)
			}
		})
	}
	links, err := db.ListSessionLinks("runtime")
	if err != nil || len(links) != 0 {
		t.Fatalf("rejected writes created links: %+v %v", links, err)
	}
	data := readToolData(t, db, "runtime")
	if string(data["codex_session_id"]) != `"original"` {
		t.Fatal("stale native binding overwrote original")
	}
	current, err := db.LastModified()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WriteStatus("runtime", "stopped", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteCodexSessionBinding("runtime", "stale", now, current); !errors.Is(err, ErrObservationStale) {
		t.Fatalf("stopped binding=%v", err)
	}
}

func TestRuntimeObservationGuardAllowsOwnWritesWithoutAdvancingFence(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	if err := db.SaveInstance(&InstanceRow{ID: "runtime", Title: "runtime", Tool: "codex", Status: "running", CreatedAt: now, LastAccessed: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.Touch(); err != nil {
		t.Fatal(err)
	}
	fence, err := db.LastModified()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.WriteCodexSessionBinding("runtime", "current", now, fence); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteLastActivityAt("runtime", now, fence); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.WriteObservedStatusIfCurrent("runtime", "running", "waiting", "codex", fence); err != nil || !ok {
		t.Fatalf("fresh status accepted=%v err=%v", ok, err)
	}
	if got, err := db.LastModified(); err != nil || got != fence {
		t.Fatalf("producer advanced mutation fence %d -> %d err=%v", fence, got, err)
	}
	links, err := db.ListSessionLinks("runtime")
	if err != nil || len(links) != 1 || links[0].NativeID != "current" {
		t.Fatalf("atomic binding link %+v err=%v", links, err)
	}
	if _, err := db.RetractSessionLink("runtime", "codex", "current", fence-1); !errors.Is(err, ErrObservationStale) {
		t.Fatalf("stale retract=%v", err)
	}
	links, _ = db.ListSessionLinks("runtime")
	if len(links) != 1 {
		t.Fatal("stale retract removed current link")
	}
}
