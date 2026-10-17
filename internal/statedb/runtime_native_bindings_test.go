package statedb

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// matrix/hardened: discovery writes are narrow, atomic, taint-preserving and
// fenced against operator edits, including edits on the same native ID.
func TestRuntimeNativeBindingsGuardWhitelistAndRecall(t *testing.T) {
	db := newTestDB(t)
	if err := db.SaveInstance(&InstanceRow{ID: "native", Tool: "codex", Status: "running", ToolData: json.RawMessage(`{"notes":"keep","codex_session_id":"old"}`), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.Touch(); err != nil {
		t.Fatal(err)
	}
	fence, err := db.LastModified()
	if err != nil {
		t.Fatal(err)
	}
	bindings := map[string]json.RawMessage{"codex_session_id": json.RawMessage(`"new"`), "codex_detected_at": json.RawMessage(`123`), "claude_session_id": json.RawMessage(`"scanned"`), "claude_session_id_unverified": json.RawMessage(`true`), "pi_session_id": json.RawMessage(`"pi-native"`), "pi_session_path": json.RawMessage(`"/owned/pi.jsonl"`)}
	if err := db.WriteRuntimeNativeBindings("native", bindings, fence); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.LoadInstances()
	var got map[string]any
	_ = json.Unmarshal(rows[0].ToolData, &got)
	if got["notes"] != "keep" || got["codex_session_id"] != "new" || got["claude_session_id_unverified"] != true || got["pi_session_path"] != "/owned/pi.jsonl" {
		t.Fatalf("narrow discovery lost fields: %s", rows[0].ToolData)
	}
	links, _ := db.ListSessionLinks("native")
	codex, claude := false, false
	for _, link := range links {
		if link.Harness == "codex" && link.NativeID == "new" {
			codex = link.Authoritative
		}
		if link.Harness == "claude" && link.NativeID == "scanned" {
			claude = !link.Authoritative
		}
	}
	if !codex || !claude {
		t.Fatalf("recall/taint mismatch: %+v", links)
	}
	before := string(rows[0].ToolData)
	for _, bad := range []map[string]json.RawMessage{{"model": json.RawMessage(`"other"`)}, {"codex_session_id": json.RawMessage(`null`)}, {"codex_session_id": json.RawMessage(`7`)}, {"codex_detected_at": json.RawMessage(`1.5`)}, {"claude_session_id_unverified": json.RawMessage(`"true"`)}} {
		if err := db.WriteRuntimeNativeBindings("native", bad, fence); err == nil {
			t.Fatalf("invalid binding accepted: %v", bad)
		}
	}
	rows, _ = db.LoadInstances()
	if string(rows[0].ToolData) != before {
		t.Fatal("invalid binding changed registry")
	}
	if err := db.Touch(); err != nil {
		t.Fatal(err)
	}
	if err := db.WriteRuntimeNativeBindings("native", map[string]json.RawMessage{"codex_session_id": json.RawMessage(`"late"`)}, fence); !errors.Is(err, ErrObservationStale) {
		t.Fatalf("obsolete source write: %v", err)
	}
	rows, _ = db.LoadInstances()
	if string(rows[0].ToolData) != before {
		t.Fatal("obsolete source changed registry")
	}
	links, _ = db.ListSessionLinks("native")
	for _, link := range links {
		if link.NativeID == "late" {
			t.Fatal("obsolete source leaked recall link")
		}
	}
}
