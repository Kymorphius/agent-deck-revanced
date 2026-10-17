// matrix/hardened: startup identity must describe the registry actually owned,
// and rejected handshakes must leave caller ownership available for recovery.
package nativefleet

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

func TestManagerIdentityRejectsOtherRootsWithoutConsumingStore(t *testing.T) {
	root := testRoot(t)
	store, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	l := &launcher{}
	for _, identity := range []Identity{
		{StateDir: filepath.Join(filepath.Dir(root), "different-registry"), Version: "0.3.0+matrix.prototype"},
		{StateDir: "relative-registry", Version: "0.3.0+matrix.prototype"},
		{StateDir: root + string(filepath.Separator), Version: "0.3.0+matrix.prototype"},
		{StateDir: root, Version: ""},
		{StateDir: root, Version: " version"},
		{StateDir: root, Version: "version\x1b"},
	} {
		manager, err := New(store, l.launch, identity)
		if manager != nil || err == nil {
			t.Fatalf("invalid startup identity accepted: %+v, %v", identity, err)
		}
		if actual, err := store.Directory(); err != nil || actual != root {
			t.Fatalf("rejected identity consumed caller store: %q, %v", actual, err)
		}
	}
	if manager, err := New(store, l.launch, Identity{}, Identity{}); manager != nil || err == nil {
		t.Fatalf("multiple identities accepted: %+v, %v", manager, err)
	}
	if other, err := nativeregistry.Open(root); !errors.Is(err, nativeregistry.ErrOwned) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("rejected startup released registry ownership: %v", err)
	}
	if records, err := store.List(); err != nil || len(records) != 0 || l.count() != 0 {
		t.Fatalf("rejected startup changed registry or launched: %+v, %v, %d", records, err, l.count())
	}
}

func TestManagerIdentityHandshakeReportsOwnedRootAndProtocol(t *testing.T) {
	root := testRoot(t)
	store, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	l := &launcher{}
	requestedRoot := root
	if runtime.GOOS == "windows" {
		// Win32 namespaces are case-insensitive, but the receipt must use the
		// root held by the store rather than blindly echoing this caller label.
		requestedRoot = strings.ToUpper(root)
	}
	wantVersion := "0.3.0+matrix.prototype"
	s, err := New(store, l.launch, Identity{StateDir: requestedRoot, Version: wantVersion})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	address, _ := serve(t, s)
	status := call(t, address, terminalhost.Request{Op: "fleet-status"})
	if status.ManagerStateDir != root || status.ManagerVersion != wantVersion || status.ProtocolVersion != 2 || status.HostPID != os.Getpid() || status.RunID == "" {
		t.Fatalf("incorrect manager handshake: %+v", status)
	}
	if err := nativeregistry.ValidateID(status.RunID); err != nil {
		t.Fatalf("manager identity is not a new UUID: %v", err)
	}
	if again := call(t, address, terminalhost.Request{Op: "fleet-status"}); again.RunID != status.RunID {
		t.Fatalf("same daemon changed identity between handshakes: %+v", again)
	}
	listed := call(t, address, terminalhost.Request{Op: "fleet-list"})
	if listed.ManagerStateDir != "" || listed.ManagerVersion != "" || listed.ProtocolVersion != 0 || listed.RunID != "" {
		t.Fatalf("ordinary refresh advertised a startup identity: %+v", listed)
	}
	if l.count() != 0 {
		t.Fatal("identity handshake launched a session")
	}
}

func TestManagerIdentityCannotUseClosedStore(t *testing.T) {
	root := testRoot(t)
	store, err := nativeregistry.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := New(store, (&launcher{}).launch, Identity{StateDir: root, Version: "0.3.0+matrix.prototype"})
	if manager != nil || !errors.Is(err, nativeregistry.ErrClosed) {
		t.Fatalf("closed owned root advertised as valid: %+v, %v", manager, err)
	}
}
