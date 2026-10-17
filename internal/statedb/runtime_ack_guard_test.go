package statedb

import (
	"encoding/json"
	"testing"
)

// matrix/hardened: actual SQLite CAS keeps operator intent distinct from an
// old source turn's reset, without adding a schema migration or poller Touch.
func TestRuntimeAcknowledgmentHasAtomicTimestampAndPreservesExtras(t *testing.T) {
	db := newTestDB(t)
	row := ackRow("one")
	row.ToolData = json.RawMessage(`{"other":"keep"}`)
	if err := db.SaveInstances([]*InstanceRow{row}); err != nil {
		t.Fatal(err)
	}
	stamps, err := db.SetAcknowledgedStamped("one", true)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.ReadAllStatuses()
	if err != nil {
		t.Fatal(err)
	}
	if !rows["one"].Acknowledged || rows["one"].AcknowledgedAt != stamps.After {
		t.Fatalf("flag/time not one write: %+v %+v", rows["one"], stamps)
	}
	if data := readToolData(t, db, "one"); string(data["other"]) != `"keep"` {
		t.Fatalf("unrelated extras lost: %s", data["other"])
	}
	if ok, err := db.WriteObservedAcknowledged("one", "idle", false, stamps.After, stamps.After); err != nil || !ok {
		t.Fatalf("current reset accepted=%v err=%v", ok, err)
	}
	rows, err = db.ReadAllStatuses()
	if err != nil {
		t.Fatal(err)
	}
	if rows["one"].Acknowledged || rows["one"].AcknowledgedAt != stamps.After {
		t.Fatalf("producer rewrote operator timestamp: %+v", rows["one"])
	}
	if touch, err := db.LastModified(); err != nil || touch != stamps.After {
		t.Fatalf("producer touched registry: %d %v", touch, err)
	}
}

func TestRuntimeAcknowledgmentRejectsNewUserAckAndStatus(t *testing.T) {
	db := newTestDB(t)
	if err := db.SaveInstances([]*InstanceRow{ackRow("one")}); err != nil {
		t.Fatal(err)
	}
	first, err := db.SetAcknowledgedStamped("one", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.SetAcknowledgedStamped("one", true)
	if err != nil {
		t.Fatal(err)
	}
	if second.After <= first.After {
		t.Fatal("repeated true did not create explicit ACK identity")
	}
	for name, args := range map[string]struct {
		status       string
		touch, ackAt int64
	}{
		"old operator watermark":         {"idle", first.After, first.After},
		"old ACK with current watermark": {"idle", second.After, first.After},
		"wrong status":                   {"running", second.After, second.After},
	} {
		t.Run(name, func(t *testing.T) {
			if ok, err := db.WriteObservedAcknowledged("one", args.status, false, args.touch, args.ackAt); err != nil || ok {
				t.Fatalf("stale reset accepted=%v err=%v", ok, err)
			}
		})
	}
	rows, err := db.ReadAllStatuses()
	if err != nil {
		t.Fatal(err)
	}
	if !rows["one"].Acknowledged || rows["one"].AcknowledgedAt != second.After {
		t.Fatal("old reset undid new user ACK")
	}
	if ok, err := db.WriteObservedAcknowledged("deleted", "idle", false, second.After, 0); err != nil || ok {
		t.Fatalf("deleted row resurrected: %v %v", ok, err)
	}
}

func TestRuntimeAcknowledgmentHandlesMalformedLegacyToolData(t *testing.T) {
	db := newTestDB(t)
	if err := db.SaveInstances([]*InstanceRow{ackRow("one")}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE instances SET tool_data='bad-json' WHERE id='one'`); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.ReadAllStatuses(); err != nil || rows["one"].AcknowledgedAt != 0 {
		t.Fatalf("legacy malformed row poisoned shared read: %+v %v", rows, err)
	}
	stamps, err := db.SetAcknowledgedStamped("one", false)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := db.ReadAllStatuses(); err != nil || rows["one"].AcknowledgedAt != stamps.After {
		t.Fatalf("ACK timestamp not repaired: %+v %v", rows, err)
	}
}
