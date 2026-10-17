package session

import (
	"encoding/json"
	"testing"
)

// matrix/hardened: preserve top/bottom/empty pins while keeping omitted legacy
// metadata unknown. Roundtrip must not erase a known empty pin.
func TestMatrixRemotePinWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wire  string
		pin   PinMode
		known bool
	}{
		{"top", `{"id":"r","pin":"top"}`, PinTop, true},
		{"bottom", `{"id":"r","pin":"bottom"}`, PinBottom, true},
		{"unpinned", `{"id":"r","pin":""}`, PinNone, true},
		{"legacy", `{"id":"r"}`, PinNone, false},
		{"null", `{"id":"r","pin":null}`, PinNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var remote RemoteSessionInfo
			if err := json.Unmarshal([]byte(tc.wire), &remote); err != nil {
				t.Fatal(err)
			}
			if got := remote.PinPosition(); got != tc.pin || remote.PinKnown() != tc.known {
				t.Fatalf("pin=%q, known=%t; want %q, known=%t", got, remote.PinKnown(), tc.pin, tc.known)
			}
			data, err := json.Marshal(remote)
			if err != nil {
				t.Fatal(err)
			}
			var row map[string]json.RawMessage
			if err := json.Unmarshal(data, &row); err != nil {
				t.Fatal(err)
			}
			if _, exists := row["pin"]; exists != tc.known {
				t.Fatalf("roundtrip pin present=%t, want %t: %s", exists, tc.known, data)
			}
			var again RemoteSessionInfo
			if err := json.Unmarshal(data, &again); err != nil {
				t.Fatal(err)
			}
			if again.PinPosition() != tc.pin || again.PinKnown() != tc.known {
				t.Fatalf("roundtrip pin=%q, known=%t; want %q, known=%t", again.PinPosition(), again.PinKnown(), tc.pin, tc.known)
			}
		})
	}
}
