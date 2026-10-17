// matrix/hardened: the same fleet lifecycle consumes shared StateDB descriptions without restarting them.
package nativefleet

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

func TestRegistryTypedNilRejected(t *testing.T) {
	var prototype *nativeregistry.Store
	var shared *nativestatestore.Store
	for _, store := range []Registry{prototype, shared} {
		if server, err := New(store, (&launcher{}).launch); server != nil || err == nil {
			t.Fatalf("typed nil store accepted: %+v %v", server, err)
		}
	}
}

func TestFleetUsesOriginalStateDBAndReopensStopped(t *testing.T) {
	root := testRoot(t)
	store, err := nativestatestore.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	launch := &launcher{}
	server, err := New(store, launch.launch)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	spec := terminalhost.LaunchSpec{Executable: "cmd.exe", Dir: "project", Cols: 80, Rows: 24}
	response := server.Dispatch(terminalhost.Request{Op: "fleet-create", Name: "shared", Tool: "shell", Group: "existing/groups", Launch: &spec, AutoStart: true})
	if !response.OK || response.SessionID == "" {
		server.Close()
		t.Fatalf("create: %+v", response)
	}
	id := response.SessionID
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = nativestatestore.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second := &launcher{}
	server, err = New(store, second.launch)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	defer server.Close()
	response = server.Dispatch(terminalhost.Request{Op: "fleet-list"})
	if !response.OK || len(response.Sessions) != 1 || response.Sessions[0].ID != id || response.Sessions[0].State != "stopped" || len(second.processes) != 0 {
		t.Fatalf("manager restarted saved agent: %+v processes=%d", response, len(second.processes))
	}
}
