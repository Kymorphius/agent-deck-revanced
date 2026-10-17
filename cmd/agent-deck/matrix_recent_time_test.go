package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: the remote list retains the visit clock with subsecond
// precision and does not invent visits from creation/background activity.
func TestMatrixRecentListWireTime(t *testing.T) {
	at := time.Date(2026, 10, 17, 10, 20, 30, 987654321, time.FixedZone("test", 8*60*60))
	instances := []*session.Instance{
		{ID: "visited", Title: "visited", LastAccessedAt: at, Status: session.StatusStopped},
		{ID: "unvisited", Title: "unvisited", CreatedAt: at, LastStartedAt: at, Status: session.StatusStopped},
	}
	cached := make(map[*session.Instance]bool)
	for _, inst := range instances {
		inst.SetTmuxSessionForTest(nil)
		cached[inst] = true
	}
	data, err := buildListJSON("_matrix_recent_wire", instances, cached)
	if err != nil {
		t.Fatal(err)
	}
	var rows []session.RemoteSessionInfo
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].LastAccessedAt != at.Format(time.RFC3339Nano) || rows[1].LastAccessedAt != "" {
		t.Fatalf("remote visit clocks lost or invented: %s", data)
	}
	var legacy session.RemoteSessionInfo
	if err := json.Unmarshal([]byte(`{"id":"legacy","last_activity_at":"2026-10-17T10:20:30Z"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.LastAccessedAt != "" {
		t.Fatal("older remote activity became a user visit")
	}
}
