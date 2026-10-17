package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: pin is stored metadata on both static and live lists.
// A known unpinned row must emit the empty value so remote callers can tell it
// apart from a legacy list that has never supported the field.
func TestMatrixSessionListPersistedPin(t *testing.T) {
	profile := fmt.Sprintf("_matrix_list_pin_%d", time.Now().UnixNano())
	want := map[string]session.PinMode{}
	var instances []*session.Instance
	for _, pin := range []session.PinMode{session.PinTop, session.PinBottom, session.PinNone} {
		inst := session.NewInstance(string(pin)+"-pin", t.TempDir())
		inst.Pin = pin
		inst.Status = session.StatusStopped
		inst.SetTmuxSessionForTest(nil)
		instances = append(instances, inst)
		want[inst.ID] = pin
	}
	seedStore(t, profile, nil, instances...)
	r := testRegistry(t, Deps{})
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
			out, result := Invoke[SessionListOut](context.Background(), r, IDSessionList, SessionListIn{Profile: profile, LiveStatus: live})
			if result.Err != nil {
				t.Fatal(result.Err)
			}
			assertMatrixSessionRowPins(t, out.Sessions, want)
		})
	}
	all, result := Invoke[SessionListOut](context.Background(), r, IDSessionList, SessionListIn{AllProfiles: true})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	for _, rows := range all.Profiles {
		if rows.Profile == profile {
			assertMatrixSessionRowPins(t, rows.Sessions, want)
			return
		}
	}
	t.Fatalf("all-profiles list omitted seeded profile %q", profile)
}

func assertMatrixSessionRowPins(t *testing.T, rows []SessionRow, want map[string]session.PinMode) {
	t.Helper()
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var wire []map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	var remote []session.RemoteSessionInfo
	if err := json.Unmarshal(data, &remote); err != nil {
		t.Fatal(err)
	}
	if len(wire) != len(want) {
		t.Fatalf("rows=%d, want %d", len(wire), len(want))
	}
	for i, row := range wire {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			t.Fatal(err)
		}
		pin, exists := row["pin"]
		if !exists {
			t.Fatalf("%s omitted pin: %s", id, data)
		}
		if string(pin) != fmt.Sprintf("%q", want[id]) {
			t.Errorf("%s pin=%s, want %q", id, pin, want[id])
		}
		if !remote[i].PinKnown() || remote[i].PinPosition() != want[id] {
			t.Errorf("%s remote pin=%q, known=%t; want %q, known", id, remote[i].PinPosition(), remote[i].PinKnown(), want[id])
		}
	}
}
