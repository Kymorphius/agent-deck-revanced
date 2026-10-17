package core

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: direct CLI list and registered remote session.list retain
// the same interaction clock, including static/all-profile list responses.
func TestMatrixRecentStaticSessionRow(t *testing.T) {
	at := time.Date(2026, 10, 17, 11, 12, 13, 123456789, time.UTC)
	for _, visited := range []bool{false, true} {
		inst := &session.Instance{ID: "row", CreatedAt: at}
		if visited {
			inst.LastAccessedAt = at
		}
		row := staticSessionRow(inst, []*session.Instance{inst}, "test")
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var remote session.RemoteSessionInfo
		if err := json.Unmarshal(data, &remote); err != nil {
			t.Fatal(err)
		}
		want := ""
		if visited {
			want = at.Format(time.RFC3339Nano)
		}
		if row.LastAccessedAt != want || remote.LastAccessedAt != want {
			t.Fatalf("visited=%v: row=%q remote=%q, want %q", visited, row.LastAccessedAt, remote.LastAccessedAt, want)
		}
	}
}
