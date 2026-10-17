// matrix/hardened: shared storage acceptance uses only explicitly owned test databases and no real accounts.
package nativestatestore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

func TestSequentialPrototypeAndSharedInventoriesCannotShareRoot(t *testing.T) {
	for _, existing := range []string{"prototype", "statedb"} {
		t.Run(existing, func(t *testing.T) {
			root := privateRoot(t)
			oldFilename, selectedFilename := nativeregistry.DatabaseFilename, DatabaseFilename
			if existing == "prototype" {
				first, err := nativeregistry.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := first.Create("keep prototype", "shell", launchFor(t)); err != nil {
					first.Close()
					t.Fatal(err)
				}
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				oldFilename, selectedFilename = DatabaseFilename, nativeregistry.DatabaseFilename
				first := openTest(t, root)
				if _, err := first.Create("keep shared", "shell", launchFor(t)); err != nil {
					t.Fatal(err)
				}
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(root, oldFilename))
			if err != nil {
				t.Fatal(err)
			}
			if existing == "prototype" {
				second, err := Open(root)
				if second != nil {
					second.Close()
				}
				if !errors.Is(err, nativeregistry.ErrInvalidData) {
					t.Fatalf("sequential shared inventory opened: %v", err)
				}
			} else {
				second, err := nativeregistry.Open(root)
				if second != nil {
					second.Close()
				}
				if !errors.Is(err, nativeregistry.ErrInvalidData) {
					t.Fatalf("sequential prototype inventory opened: %v", err)
				}
			}
			if _, err := os.Lstat(filepath.Join(root, selectedFilename)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("collision created selected DB: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(root, oldFilename))
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatalf("alternate DB bytes changed: %v", err)
			}
			// Reopen the established store to independently prove lock release.
			if existing == "prototype" {
				first, err := nativeregistry.Open(root)
				if err != nil {
					t.Fatal(err)
				}
				rows, err := first.List()
				if err != nil || len(rows) != 1 || rows[0].Name != "keep prototype" {
					first.Close()
					t.Fatalf("existing prototype inventory changed: %+v %v", rows, err)
				}
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				first := openTest(t, root)
				rows, err := first.List()
				if err != nil || len(rows) != 1 || rows[0].Name != "keep shared" {
					t.Fatalf("existing shared inventory changed: %+v %v", rows, err)
				}
			}
		})
	}
}

func privateRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" && os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 for owned Windows StateDB test files")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "shared-state")
}

func openTest(t *testing.T, root string) *Store {
	t.Helper()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func independentWriter(t *testing.T, root string) *statedb.StateDB {
	t.Helper()
	db, err := statedb.Open(filepath.Join(root, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func launchFor(t *testing.T) terminalhost.LaunchSpec {
	t.Helper()
	return terminalhost.LaunchSpec{Executable: "cmd.exe", Args: []string{"/d", "/q"}, Env: []string{"EXPLICIT_ONLY=中文"}, Dir: filepath.Dir(privateRoot(t)), Cols: 80, Rows: 24}
}

func TestOriginalReaderSeesNativeInstancesGroupsAndLegacySurvives(t *testing.T) {
	root := privateRoot(t)
	s := openTest(t, root)
	writer := independentWriter(t, root)
	legacy := &statedb.InstanceRow{ID: "legacy-not-a-UUID", Title: "原会话", ProjectPath: "legacy-project", GroupPath: "legacy-group", Tool: "codex", Status: "waiting", TmuxSession: "existing-pane", TmuxSocketName: "existing-socket", CreatedAt: time.Unix(1712345678, 0), LastAccessed: time.Unix(1712345688, 0), Account: "existing-account", Pin: "top", ArchivedAt: time.Unix(1712345698, 0), ToolData: json.RawMessage(`{"codex_session_id":"existing-native-id","custom":{"keep":true}}`)}
	if err := writer.InsertInstanceRow(legacy); err != nil {
		t.Fatal(err)
	}
	before, err := writer.LoadInstanceByID(legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.SaveGroup(&statedb.GroupRow{Path: "destination", Name: "keep custom group name", DefaultPath: "keep/default", Expanded: false, Order: 17}); err != nil {
		t.Fatal(err)
	}
	record, err := s.CreateInGroup("原生 会话", "shell", "开发/Windows", launchFor(t))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := statedb.OpenReadOnlyLive(filepath.Join(root, DatabaseFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	row, err := reader.LoadInstanceByID(record.ID)
	if err != nil || row == nil {
		t.Fatalf("original reader sees no native row: %v", err)
	}
	if row.Title != record.Name || row.ProjectPath != record.Launch.Dir || row.GroupPath != "开发/Windows" || row.TmuxSession != "" || row.TmuxSocketName != "" {
		t.Fatalf("native instance identity: %+v", row)
	}
	if row.Status != "unknown" {
		t.Fatalf("saved native description fabricates AI status: %q", row.Status)
	}
	for _, group := range []string{"开发", "开发/Windows"} {
		if row, err := reader.LoadGroup(group); err != nil || row == nil {
			t.Fatalf("original group reader %s: %+v %v", group, row, err)
		}
	}
	if err := s.Rename(record.ID, "共享数据库改名"); err != nil {
		t.Fatal(err)
	}
	if err := s.Move(record.ID, "destination"); err != nil {
		t.Fatal(err)
	}
	group, err := reader.LoadGroup("destination")
	if err != nil || group.Name != "keep custom group name" || group.DefaultPath != "keep/default" || group.Expanded || group.Order != 17 {
		t.Fatalf("existing group overwritten: %+v %v", group, err)
	}
	after, err := reader.LoadInstanceByID(legacy.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("unrelated legacy row changed:\nbefore=%+v\nafter=%+v err=%v", before, after, err)
	}
	rows, err := s.List()
	if err != nil || len(rows) != 1 || rows[0].ID != record.ID {
		t.Fatalf("legacy row admitted: %+v %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(root, nativeregistry.DatabaseFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dummy second database exists: %v", err)
	}
	var nativeTables int
	if err := writer.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='native_sessions'").Scan(&nativeTables); err != nil || nativeTables != 0 {
		t.Fatalf("second schema: %d %v", nativeTables, err)
	}
	if err := s.Delete(record.ID); err != nil {
		t.Fatal(err)
	}
	after, err = reader.LoadInstanceByID(legacy.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("native delete changed legacy row: %+v %v", after, err)
	}
}

func TestLaunchImmutableTargetedEditsPreserveNativeExtrasAndStaleSaves(t *testing.T) {
	root := privateRoot(t)
	s := openTest(t, root)
	writer := independentWriter(t, root)
	launch := launchFor(t)
	record, err := s.Create("native", "shell", launch)
	if err != nil {
		t.Fatal(err)
	}
	launch.Args[0] = "caller mutation"
	launch.Env[0] = "CALLER_MUTATION=1"
	if record.Launch.Args[0] != "/d" || record.Launch.Env[0] != "EXPLICIT_ONLY=中文" || record.Group != DefaultGroup {
		t.Fatalf("mutable/default description: %+v", record)
	}
	row, err := writer.LoadInstanceByID(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.Account = "preserve-account"
	row.Pin = "bottom"
	row.ArchivedAt = time.Unix(1712345678, 0)
	row.ToolData = json.RawMessage(`{"additional_extra":"keep"}`) // stale typed save has not read native_backend
	if err := writer.SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(record.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Move(record.ID, "after/move"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(record.ID)
	if err != nil || !reflect.DeepEqual(record.Launch, got.Launch) || record.ID != got.ID || !record.CreatedAt.Equal(got.CreatedAt) || got.UpdatedAt.Before(record.UpdatedAt) {
		t.Fatalf("launch identity changed: %+v %v", got, err)
	}
	updated, err := writer.LoadInstanceByID(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	var extras map[string]json.RawMessage
	if err := json.Unmarshal(updated.ToolData, &extras); err != nil {
		t.Fatal(err)
	}
	if updated.Account != row.Account || updated.Pin != row.Pin || !updated.ArchivedAt.Equal(row.ArchivedAt) || string(extras["additional_extra"]) != `"keep"` {
		t.Fatalf("targeted edits replaced unrelated fields: %+v extras=%s", updated, updated.ToolData)
	}
	// matrix/hardened: exclusive ACL protection during manager acquisition
	// requires prior independent SQLite handles to be closed on Windows.
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTest(t, root)
	got, err = reopened.Get(record.ID)
	if err != nil || !reflect.DeepEqual(record.Launch, got.Launch) || got.Name != "renamed" || got.Group != "after/move" {
		t.Fatalf("reopened descriptor: %+v %v", got, err)
	}
}

func TestDeclaredInvalidNativeBackendAndConflictingTmuxFailClosed(t *testing.T) {
	for _, change := range []string{"null", "future", "kind", "unknown-field", "bad-launch", "tmux", "directory"} {
		t.Run(change, func(t *testing.T) {
			root := privateRoot(t)
			s := openTest(t, root)
			writer := independentWriter(t, root)
			record, err := s.Create("native", "shell", launchFor(t))
			if err != nil {
				t.Fatal(err)
			}
			statement := ""
			switch change {
			case "null":
				statement = "UPDATE instances SET tool_data=json_set(tool_data,'$.native_backend',NULL) WHERE id=?"
			case "future":
				statement = "UPDATE instances SET tool_data=json_set(tool_data,'$.native_backend.version',999) WHERE id=?"
			case "kind":
				statement = "UPDATE instances SET tool_data=json_set(tool_data,'$.native_backend.kind','unknown') WHERE id=?"
			case "unknown-field":
				statement = "UPDATE instances SET tool_data=json_set(tool_data,'$.native_backend.future_field',1) WHERE id=?"
			case "bad-launch":
				statement = "UPDATE instances SET tool_data=json_set(tool_data,'$.native_backend.launch.executable','') WHERE id=?"
			case "tmux":
				statement = "UPDATE instances SET tmux_session='conflicting-pane' WHERE id=?"
			case "directory":
				statement = "UPDATE instances SET project_path='conflicting-directory' WHERE id=?"
			}
			if _, err := writer.DB().Exec(statement, record.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.List(); !errors.Is(err, nativeregistry.ErrInvalidData) {
				t.Fatalf("invalid list: %v", err)
			}
			if err := s.Rename(record.ID, "must-not-write"); !errors.Is(err, nativeregistry.ErrInvalidData) {
				t.Fatalf("invalid rename: %v", err)
			}
			if err := s.Delete(record.ID); !errors.Is(err, nativeregistry.ErrInvalidData) {
				t.Fatalf("invalid delete: %v", err)
			}
			row, err := writer.LoadInstanceByID(record.ID)
			if err != nil || row == nil || row.Title != "native" {
				t.Fatalf("invalid mutation wrote: %+v %v", row, err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := Open(root)
			if opened != nil {
				opened.Close()
			}
			if !errors.Is(err, nativeregistry.ErrInvalidData) {
				t.Fatalf("invalid reopened: %v", err)
			}
		})
	}
}

func TestFutureAndMalformedStateDBVersionRefusedWithoutMigration(t *testing.T) {
	for _, version := range []string{"999", "corrupt", "0", "-1"} {
		t.Run(version, func(t *testing.T) {
			root := privateRoot(t)
			s := openTest(t, root)
			writer := independentWriter(t, root)
			if _, err := writer.DB().Exec("UPDATE metadata SET value=? WHERE key='schema_version'", version); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := Open(root)
			if opened != nil {
				opened.Close()
			}
			if !errors.Is(err, nativeregistry.ErrInvalidData) {
				t.Fatalf("declared version accepted: %v", err)
			}
			reader, err := statedb.OpenReadOnly(filepath.Join(root, DatabaseFilename))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var got string
			if err := reader.DB().QueryRow("SELECT value FROM metadata WHERE key='schema_version'").Scan(&got); err != nil || got != version {
				t.Fatalf("version mutated: %q %v", got, err)
			}
		})
	}
}

func TestNativeDescriptorInOldSchemaRefusedBeforeMigration(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid-native", true: "conflicting-tmux"}[conflict], func(t *testing.T) {
			root := privateRoot(t)
			s := openTest(t, root)
			record, err := s.Create("native", "shell", launchFor(t))
			if err != nil {
				t.Fatal(err)
			}
			writer := independentWriter(t, root)
			if _, err := writer.DB().Exec("UPDATE metadata SET value='12' WHERE key='schema_version'"); err != nil {
				t.Fatal(err)
			}
			if conflict {
				if _, err := writer.DB().Exec("UPDATE instances SET tmux_session='keep-conflict' WHERE id=?", record.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := Open(root)
			if opened != nil {
				opened.Close()
			}
			if !errors.Is(err, nativeregistry.ErrInvalidData) {
				t.Fatalf("native old schema accepted: %v", err)
			}
			reader, err := statedb.OpenReadOnly(filepath.Join(root, DatabaseFilename))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var version string
			if err := reader.DB().QueryRow("SELECT value FROM metadata WHERE key='schema_version'").Scan(&version); err != nil || version != "12" {
				t.Fatalf("migration ran before native validation: %q %v", version, err)
			}
			row, err := reader.LoadInstanceByID(record.ID)
			if err != nil || row == nil || row.Title != "native" || (conflict && row.TmuxSession != "keep-conflict") {
				t.Fatalf("rejected old native row changed: %+v %v", row, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			// Failed open must release only its exact manager lease.
			lease, err := nativeregistry.AcquireLease(root, DatabaseFilename)
			if err != nil {
				t.Fatalf("failed preflight retained ownership: %v", err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSharedLeaseBlocksOtherOwnersAndReleasesAfterClose(t *testing.T) {
	root := privateRoot(t)
	s := openTest(t, root)
	if opened, err := Open(root); opened != nil || !errors.Is(err, nativeregistry.ErrOwned) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("second StateDB owner: %v", err)
	}
	if opened, err := nativeregistry.Open(root); opened != nil || !errors.Is(err, nativeregistry.ErrOwned) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("prototype owner overlaps: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Directory(); !errors.Is(err, nativeregistry.ErrClosed) {
		t.Fatalf("closed directory still advertised: %v", err)
	}
	if _, err := s.List(); !errors.Is(err, nativeregistry.ErrClosed) {
		t.Fatalf("closed list: %v", err)
	}
	openTest(t, root)
}

func TestURIPathRefusedBeforeCreatingFiles(t *testing.T) {
	for _, suffix := range []string{"?literal", "#literal", "%literal"} {
		root := privateRoot(t) + suffix
		// matrix/hardened: Windows rejects '?' before Lstat can report absence;
		// compare the trusted parent to prove that refusal created no files.
		parent := filepath.Dir(root)
		before, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if opened, err := Open(root); opened != nil || !errors.Is(err, nativeregistry.ErrUnsafePath) {
			if opened != nil {
				opened.Close()
			}
			t.Fatalf("URI path accepted: %v", err)
		}
		after, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(before) != len(after) {
			t.Fatalf("rejected root changed its parent: before=%v after=%v", before, after)
		}
		for i := range before {
			if before[i].Name() != after[i].Name() {
				t.Fatal("rejected root changed a parent entry")
			}
		}
	}
}

func TestLegacyUUIDRowsAreNeverMutatedByNativeCommands(t *testing.T) {
	root := privateRoot(t)
	s := openTest(t, root)
	writer := independentWriter(t, root)
	id := uuid.NewString()
	if err := writer.InsertInstanceRow(&statedb.InstanceRow{ID: id, Title: "legacy", ProjectPath: "legacy", GroupPath: "legacy", Tool: "shell", TmuxSession: "legacy-pane", CreatedAt: time.Now(), ToolData: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); !errors.Is(err, nativeregistry.ErrNotFound) {
		t.Fatalf("legacy get: %v", err)
	}
	for _, err := range []error{s.Rename(id, "replacement"), s.Move(id, "replacement"), s.Delete(id)} {
		if !errors.Is(err, nativeregistry.ErrNotFound) {
			t.Fatalf("legacy mutation: %v", err)
		}
	}
	row, err := writer.LoadInstanceByID(id)
	if err != nil || row == nil || row.Title != "legacy" || row.GroupPath != "legacy" {
		t.Fatalf("legacy modified: %+v %v", row, err)
	}
}
