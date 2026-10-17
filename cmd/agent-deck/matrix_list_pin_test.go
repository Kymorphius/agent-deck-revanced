package main

import (
	"encoding/json"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: the legacy remote agent push path must expose the same
// persisted pin contract as the registered session.list command.
func TestMatrixBuildListJSONPin(t *testing.T) {
	instances := []*session.Instance{
		{ID: "top", Title: "top", Pin: session.PinTop, Status: session.StatusStopped},
		{ID: "bottom", Title: "bottom", Pin: session.PinBottom, Status: session.StatusStopped},
		{ID: "plain", Title: "plain", Pin: session.PinNone, Status: session.StatusStopped},
	}
	cached := make(map[*session.Instance]bool, len(instances))
	for _, inst := range instances {
		inst.SetTmuxSessionForTest(nil)
		cached[inst] = true
	}
	data, err := buildListJSON("_matrix_pin_wire", instances, cached)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	var remote []session.RemoteSessionInfo
	if err := json.Unmarshal(data, &remote); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(instances) {
		t.Fatalf("rows=%d, want %d", len(rows), len(instances))
	}
	for i, inst := range instances {
		pin, exists := rows[i]["pin"]
		if !exists {
			t.Fatalf("%s omitted pin: %s", inst.ID, data)
		}
		var got session.PinMode
		if err := json.Unmarshal(pin, &got); err != nil {
			t.Fatal(err)
		}
		if got != inst.Pin || !remote[i].PinKnown() || remote[i].PinPosition() != inst.Pin {
			t.Errorf("%s wire pin=%q, remote pin=%q, known=%t; want %q, known", inst.ID, got, remote[i].PinPosition(), remote[i].PinKnown(), inst.Pin)
		}
	}
}
