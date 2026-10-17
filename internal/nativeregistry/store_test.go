// matrix/hardened: tests own explicit temporary roots, with opt-in Windows filesystem/process coverage.
package nativeregistry

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

func nativeTest(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" && os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 to allow isolated Windows registry files and subprocesses")
	}
}

func privateRoot(t *testing.T) string {
	t.Helper()
	nativeTest(t)
	// macOS's /var temp ancestor is itself a symlink; the explicit trusted
	// state path must be canonical even in isolated tests.
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "registry")
}

func testLaunch() terminalhost.LaunchSpec {
	return terminalhost.LaunchSpec{Executable: "cmd.exe", Args: []string{"/d", "/q"}, Dir: `C:\native-project`, Cols: 80, Rows: 24}
}

func openTest(t *testing.T, root string) *Store {
	t.Helper()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestDirectoryReturnsVerifiedOwnedRootUntilClosed(t *testing.T) {
	root := privateRoot(t)
	store := openTest(t, root)
	if actual, err := store.Directory(); err != nil || actual != root {
		t.Fatalf("owned directory: %q, %v", actual, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if actual, err := store.Directory(); actual != "" || !errors.Is(err, ErrClosed) {
		t.Fatalf("closed store still advertised its ownership: %q, %v", actual, err)
	}
	if actual, err := (*Store)(nil).Directory(); actual != "" || !errors.Is(err, ErrClosed) {
		t.Fatalf("nil store directory: %q, %v", actual, err)
	}
}

func TestPersistenceImmutableLaunchAndDescriptionOnlyDelete(t *testing.T) {
	root := privateRoot(t)
	project := filepath.Join(filepath.Dir(root), "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(project, "keep.txt")
	if err := os.WriteFile(marker, []byte("project survives metadata deletion"), 0600); err != nil {
		t.Fatal(err)
	}
	store := openTest(t, root)
	launch := testLaunch()
	launch.Dir = project
	record, err := store.Create("中文 会话 🐈", "shell", launch)
	if err != nil {
		t.Fatal(err)
	}
	launch.Args[0] = "caller mutation"
	if record.Launch.Args[0] != "/d" {
		t.Fatal("Create retained caller-owned argument slice")
	}
	if err := store.Rename(record.ID, "重开后保留"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed List: %v", err)
	}
	reopened := openTest(t, root)
	got, err := reopened.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "重开后保留" || got.Tool != "shell" || !got.CreatedAt.Equal(record.CreatedAt) || got.UpdatedAt.Before(record.UpdatedAt) || got.Launch.Args[0] != "/d" || got.Launch.Dir != project {
		t.Fatalf("persisted description changed: %#v", got)
	}
	got.Launch.Args[0] = "reader mutation"
	listed, err := reopened.List()
	if err != nil || len(listed) != 1 || listed[0].Launch.Args[0] != "/d" {
		t.Fatalf("List did not return detached metadata: %#v, %v", listed, err)
	}
	if err := reopened.Delete(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Get(record.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted Get: %v", err)
	}
	if err := reopened.Delete(record.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("Delete touched launch project: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{DatabaseFilename, OwnerFilename} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("Close removed %s: %v", name, err)
		}
	}
}

func TestStableCreationOrderAndFailedRenameIsAtomic(t *testing.T) {
	store := openTest(t, privateRoot(t))
	first, err := store.Create("一", "shell", testLaunch())
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create("二", "shell", testLaunch())
	if err != nil {
		t.Fatal(err)
	}
	// Force a storage-level failure after validation, proving the transaction
	// does not partially change either the name or the timestamp.
	if _, err := store.db.Exec("CREATE TRIGGER reject_rename BEFORE UPDATE ON native_sessions BEGIN SELECT RAISE(ABORT,'isolated rename failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := store.Rename(first.ID, "失败更名"); err == nil {
		t.Fatal("rename unexpectedly committed")
	}
	got, err := store.Get(first.ID)
	if err != nil || got.Name != first.Name || !got.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("failed transaction mutated record: %#v, %v", got, err)
	}
	if _, err := store.db.Exec("DROP TRIGGER reject_rename"); err != nil {
		t.Fatal(err)
	}
	if err := store.Rename(first.ID, "成功更名"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 2 || listed[0].ID != first.ID || listed[1].ID != second.ID {
		t.Fatalf("creation order: %#v, %v", listed, err)
	}
	// Identical creation times use UUID as the stable secondary ordering.
	if _, err := store.db.Exec("UPDATE native_sessions SET created_at=?,updated_at=?", first.CreatedAt.UnixNano(), first.CreatedAt.UnixNano()); err != nil {
		t.Fatal(err)
	}
	listed, err = store.List()
	if err != nil || len(listed) != 2 || listed[0].ID > listed[1].ID {
		t.Fatalf("UUID tie order: %#v, %v", listed, err)
	}
}

func TestOwnerExclusionInSameProcessAndActualSubprocess(t *testing.T) {
	root := privateRoot(t)
	store := openTest(t, root)
	if second, err := Open(root); !errors.Is(err, ErrOwned) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second owner: %v", err)
	}
	runOwnerHelper(t, root, true)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	runOwnerHelper(t, root, false)
	openTest(t, root)
}

func runOwnerHelper(t *testing.T, root string, wantOwned bool) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestOwnerSubprocessHelper$")
	command.Env = append(os.Environ(), "AGENTDECK_REGISTRY_OWNER_HELPER="+root)
	if wantOwned {
		command.Env = append(command.Env, "AGENTDECK_REGISTRY_EXPECT_OWNED=1")
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("owner subprocess: %v\n%s", err, output)
	}
}

func TestOwnerSubprocessHelper(t *testing.T) {
	root := os.Getenv("AGENTDECK_REGISTRY_OWNER_HELPER")
	if root == "" {
		t.Skip("isolated subprocess helper")
	}
	nativeTest(t)
	store, err := Open(root)
	if os.Getenv("AGENTDECK_REGISTRY_EXPECT_OWNED") == "1" {
		if store != nil {
			_ = store.Close()
		}
		if !errors.Is(err, ErrOwned) {
			t.Fatalf("expected OS owner exclusion, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNamesIDsAndToolValidation(t *testing.T) {
	for _, name := range []string{"中文会话", "emoji 🐱", strings.Repeat("界", 128)} {
		if err := ValidateName(name); err != nil {
			t.Errorf("valid name %q: %v", name, err)
		}
	}
	for _, name := range []string{"", " \t", "a\n", "a\x00", "a\x7f", string([]byte{0xff}), strings.Repeat("界", 129)} {
		if err := ValidateName(name); !errors.Is(err, ErrInvalidData) {
			t.Errorf("invalid name accepted %q: %v", name, err)
		}
	}
	id := uuid.NewString()
	if err := ValidateID(id); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../owner.lock", "00000000-0000-0000-0000-000000000000", strings.ToUpper(id), "urn:uuid:" + id} {
		if err := ValidateID(id); !errors.Is(err, ErrInvalidData) {
			t.Errorf("invalid ID accepted %q: %v", id, err)
		}
	}
	for _, tool := range []string{"", "shell\n", "Codex", "../../project"} {
		if err := validateTool(tool); !errors.Is(err, ErrInvalidData) {
			t.Errorf("invalid tool accepted %q: %v", tool, err)
		}
	}
}

func TestBadMetadataAndFutureSchemaFailClosedAndReleaseOwner(t *testing.T) {
	for _, mutation := range []string{
		"PRAGMA user_version=3",
		"UPDATE native_sessions SET launch_json='{} trailing'",
		"UPDATE native_sessions SET name=char(10)",
		"UPDATE native_sessions SET id='../../project'",
		"UPDATE native_sessions SET group_name=char(10)",
		"UPDATE native_sessions SET group_name=' padded '",
		"ALTER TABLE native_sessions ADD COLUMN future TEXT",
	} {
		t.Run(mutation, func(t *testing.T) {
			root := privateRoot(t)
			store := openTest(t, root)
			if _, err := store.Create("valid", "shell", testLaunch()); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			broken, err := Open(root)
			if broken != nil {
				_ = broken.Close()
			}
			if !errors.Is(err, ErrInvalidData) {
				t.Fatalf("invalid metadata accepted: %v", err)
			}
			// Failing initialization releases its OS lock. A repeated attempt
			// reports the data problem again, rather than ownership contention.
			broken, again := Open(root)
			if broken != nil {
				_ = broken.Close()
			}
			if !errors.Is(again, ErrInvalidData) {
				t.Fatalf("failure leaked owner handle: %v", again)
			}
		})
	}
}

func TestValidationCannotMutateStoredRecord(t *testing.T) {
	store := openTest(t, privateRoot(t))
	record, err := store.Create("original", "shell", testLaunch())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rename(record.ID, "bad\n"); !errors.Is(err, ErrInvalidData) {
		t.Fatal(err)
	}
	if err := store.Rename(uuid.NewString(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := store.Create("valid", "shell", terminalhost.LaunchSpec{}); !errors.Is(err, ErrInvalidData) {
		t.Fatal(err)
	}
	got, err := store.Get(record.ID)
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("rejected write changed record: %#v, %v", got, err)
	}
}
