//go:build !windows

// matrix/hardened: alternate-store presence checks never follow or harden unsafe files.
package nativeregistry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnsafeAlternateKnownStoreRefusedWithoutFollowing(t *testing.T) {
	for _, selected := range []string{DatabaseFilename, SharedStateDatabaseFilename} {
		for _, kind := range []string{"symlink", "hardlink", "directory"} {
			t.Run(selected+"-"+kind, func(t *testing.T) {
				root := privateRoot(t)
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(filepath.Dir(root), "external-keep")
				if err := os.WriteFile(target, []byte("external bytes must survive"), 0400); err != nil {
					t.Fatal(err)
				}
				alternate := DatabaseFilename
				if selected == DatabaseFilename {
					alternate = SharedStateDatabaseFilename
				}
				path := filepath.Join(root, alternate)
				var err error
				switch kind {
				case "symlink":
					err = os.Symlink(target, path)
				case "hardlink":
					err = os.Link(target, path)
				case "directory":
					err = os.Mkdir(path, 0500)
				}
				if err != nil {
					t.Fatal(err)
				}
				lease, err := AcquireLease(root, selected)
				if lease != nil {
					lease.Close()
				}
				if !errors.Is(err, ErrInvalidData) {
					t.Fatalf("unsafe alternate accepted: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(root, selected)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("collision created selected DB: %v", err)
				}
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "external bytes must survive" {
					t.Fatalf("alternate target mutated: %q %v", data, err)
				}
				info, err := os.Stat(target)
				if err != nil || info.Mode().Perm() != 0400 {
					t.Fatalf("alternate target was hardened: %v", err)
				}
				generic, err := AcquireLease(root, "unrelated.db")
				if err != nil {
					t.Fatalf("failed collision retained owner: %v", err)
				}
				if err := generic.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
