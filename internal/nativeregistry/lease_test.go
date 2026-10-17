// matrix/hardened: independently exercise the shared file lease without a duplicate SQLite schema.
package nativeregistry

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSequentialKnownStoreCollisionPreservesAlternateAndReleasesOwner(t *testing.T) {
	for _, existing := range []string{DatabaseFilename, SharedStateDatabaseFilename} {
		t.Run(existing, func(t *testing.T) {
			root := privateRoot(t)
			lease, err := AcquireLease(root, existing)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lease.data.Write([]byte("owned existing database bytes")); err != nil {
				lease.Close()
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(root, existing))
			if err != nil {
				t.Fatal(err)
			}
			selected := DatabaseFilename
			if existing == DatabaseFilename {
				selected = SharedStateDatabaseFilename
			}
			failed, err := AcquireLease(root, selected)
			if failed != nil {
				failed.Close()
			}
			if !errors.Is(err, ErrInvalidData) {
				t.Fatalf("sequential inventory collision accepted: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, selected)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("collision created selected database: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(root, existing))
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatalf("collision changed existing database: %v", err)
			}
			reopened, err := AcquireLease(root, existing)
			if err != nil {
				t.Fatalf("collision retained owner lock: %v", err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			// Unknown simple filenames retain the generic lease contract.
			generic, err := AcquireLease(root, "other-test.db")
			if err != nil {
				t.Fatalf("generic database filename rejected: %v", err)
			}
			if err := generic.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLeaseProtectsExactSharedDatabaseAndReleasesOwnership(t *testing.T) {
	root := privateRoot(t)
	lease, err := AcquireLease(root, "state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if actual, err := lease.Directory(); err != nil || actual != root {
		t.Fatalf("lease directory: %q %v", actual, err)
	}
	if _, err := os.Stat(filepath.Join(root, DatabaseFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease created duplicate registry DB: %v", err)
	}
	if other, err := AcquireLease(root, DatabaseFilename); other != nil || !errors.Is(err, ErrOwned) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("different database overlaps owner: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Verify(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed lease verifies: %v", err)
	}
	other, err := AcquireLease(root, "state.db")
	if err != nil {
		t.Fatalf("lease not released: %v", err)
	}
	defer other.Close()
}

func TestLeaseInvalidFilenameRefusedBeforeCreatingRoot(t *testing.T) {
	for _, filename := range []string{"", ".", "..", "../state.db", `..\state.db`, "state.db:stream", "state?.db", "state.db.", "owner.lock", "state\x00.db", "state|db"} {
		root := privateRoot(t)
		lease, err := AcquireLease(root, filename)
		if lease != nil {
			lease.Close()
		}
		if !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("filename %q accepted: %v", filename, err)
		}
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid filename created root: %v", err)
		}
	}
}
