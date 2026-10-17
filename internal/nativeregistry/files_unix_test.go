//go:build !windows

// matrix/hardened: path/permission tests touch only this test's private root and prove alias targets survive unchanged.
package nativeregistry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateUnixModesAndEscapedDatabasePath(t *testing.T) {
	root := privateRoot(t) + "?literal#路径"
	store := openTest(t, root)
	if _, err := store.Create("私有", "shell", testLaunch()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"": 0700, DatabaseFilename: 0600, OwnerFilename: 0600} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("mode for %s: %v, %v", name, info, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if records, err := openTest(t, root).List(); err != nil || len(records) != 1 {
		t.Fatalf("URI characters redirected data: %v, %v", records, err)
	}
}

func TestRejectUnixPathAliasesBeforeChangingTargets(t *testing.T) {
	for _, kind := range []string{"root-symlink", "ancestor-symlink", "owner-symlink", "data-symlink", "owner-hardlink", "data-hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := privateRoot(t)
			parent := filepath.Dir(root)
			target := filepath.Join(parent, "target")
			if kind == "root-symlink" || kind == "ancestor-symlink" {
				if err := os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
				if kind == "root-symlink" {
					if err := os.Symlink(target, root); err != nil {
						t.Fatal(err)
					}
				} else {
					alias := filepath.Join(parent, "alias")
					if err := os.Symlink(target, alias); err != nil {
						t.Fatal(err)
					}
					root = filepath.Join(alias, "registry")
				}
			} else {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("unaltered external target"), 0644); err != nil {
					t.Fatal(err)
				}
				name := OwnerFilename
				if kind == "data-symlink" || kind == "data-hardlink" {
					name = DatabaseFilename
				}
				var err error
				if kind == "owner-hardlink" || kind == "data-hardlink" {
					err = os.Link(target, filepath.Join(root, name))
				} else {
					err = os.Symlink(target, filepath.Join(root, name))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			opened, err := Open(root)
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("alias accepted: %v", err)
			}
			after, err := os.Stat(target)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() {
				t.Fatalf("rejected alias changed target: %v", err)
			}
		})
	}
	for _, path := range []string{"", ".", "relative/root", "/", filepath.Join(privateRoot(t), "..", "other") + "/.."} {
		store, err := Open(path)
		if store != nil {
			_ = store.Close()
		}
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("ambiguous root accepted %q: %v", path, err)
		}
	}
}

func TestReplacedRootFailsClosedAndCloseDoesNotTraverse(t *testing.T) {
	root := privateRoot(t)
	store := openTest(t, root)
	if _, err := store.Create("owned", "shell", testLaunch()); err != nil {
		t.Fatal(err)
	}
	moved := root + "-owned"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(filepath.Dir(root), "external")
	if err := os.Mkdir(external, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(external, "keep")
	if err := os.WriteFile(marker, []byte("remain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("replaced root read: %v", err)
	}
	if advertised, err := store.Directory(); advertised != "" || !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("replaced root advertised as owned: %q, %v", advertised, err)
	}
	if _, err := store.Create("rejected", "shell", testLaunch()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("replaced root write: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("Close traversed replacement: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, DatabaseFilename)); err != nil {
		t.Fatalf("Close deleted held root metadata: %v", err)
	}
}
