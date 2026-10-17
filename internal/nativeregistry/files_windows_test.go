//go:build windows

// matrix/hardened: native Windows alias/ACL/lock coverage requires an explicit test opt-in and owns all targets.
package nativeregistry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/winipc"
)

func TestWindowsPrivateACLReadbackAndAliasRejection(t *testing.T) {
	nativeTest(t)
	root := privateRoot(t)
	store := openTest(t, root)
	if _, err := store.Create("原生私有", "shell", testLaunch()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// ProtectPath independently reads back exact restricted ACLs. This also
	// proves Close released all handles that forbid ACL's exclusive opening.
	for _, path := range []string{root, filepath.Join(root, DatabaseFilename), filepath.Join(root, OwnerFilename)} {
		if err := winipc.ProtectPath(path); err != nil {
			t.Fatalf("ACL/readback or leaked handle: %v", err)
		}
	}
	parent := filepath.Dir(root)
	for _, path := range []string{"", ".", `C:\`, `C:relative`, `\\server\share\root`, `\\?\C:\temp\root`, filepath.Join(parent, "trailing."), filepath.Join(parent, "trailing "), filepath.Join(parent, "CON"), filepath.Join(parent, "COM1.txt"), filepath.Join(parent, "LPT¹"), filepath.Join(parent, "root:stream")} {
		opened, err := Open(path)
		if opened != nil {
			_ = opened.Close()
		}
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("path alias accepted %q: %v", path, err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "trailing") {
			t.Fatalf("ambiguous directory created before validation: %s", entry.Name())
		}
	}
}

func TestWindowsHardlinkedStateFilesRejectedWithoutTargetChanges(t *testing.T) {
	nativeTest(t)
	for _, name := range []string{OwnerFilename, DatabaseFilename} {
		t.Run(name, func(t *testing.T) {
			root := privateRoot(t)
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(root), "outside.txt")
			want := []byte("outside target is retained")
			if err := os.WriteFile(target, want, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			opened, err := Open(root)
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("hardlink accepted: %v", err)
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != string(want) {
				t.Fatalf("rejected link changed external data: %v", err)
			}
		})
	}
}
