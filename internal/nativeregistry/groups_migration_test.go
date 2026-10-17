// matrix/hardened: exercise real schema-v1 files and atomic metadata-only moves.
package nativeregistry

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// versionOneFixture creates the exact shipped v1 schema without invoking Open,
// so the migration proof cannot accidentally start from an already-v2 database.
func versionOneFixture(t *testing.T) (string, *sql.DB, Record, string) {
	t.Helper()
	root := privateRoot(t)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, DatabaseFilename)
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db := openFixtureDatabase(t, path)
	if _, err := db.Exec(schemaV1SQL); err != nil {
		t.Fatal(err)
	}
	launch := testLaunch()
	launch.Args = append(launch.Args, "中文 🐈")
	launch.Env = []string{"FIXTURE=Unicode 中文 😀", "SECOND=unchanged"}
	created := time.Date(2025, 2, 3, 4, 5, 6, 123456789, time.UTC)
	record := Record{ID: uuid.NewString(), Name: "旧版本会话 🐈", Tool: "shell", Launch: launch, CreatedAt: created, UpdatedAt: created.Add(time.Hour)}
	raw, err := json.MarshalIndent(launch, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO native_sessions(id,name,tool,launch_json,created_at,updated_at) VALUES(?,?,?,?,?,?)", record.ID, record.Name, record.Tool, string(raw), record.CreatedAt.UnixNano(), record.UpdatedAt.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	return root, db, record, string(raw)
}

func openFixtureDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	dataPath := filepath.ToSlash(path)
	if !strings.HasPrefix(dataPath, "/") {
		dataPath = "/" + dataPath
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: dataPath}).String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertVersionOneUnchanged(t *testing.T, db *sql.DB, want Record, raw string) {
	t.Helper()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("v1 version changed: %d, %v", version, err)
	}
	if err := checkSchema(db, 1); err != nil {
		t.Fatalf("v1 schema changed: %v", err)
	}
	rows, err := listRecords(db, 1)
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("v1 row changed: %#v, %v", rows, err)
	}
	var gotRaw string
	if err := db.QueryRow("SELECT launch_json FROM native_sessions WHERE id=?", want.ID).Scan(&gotRaw); err != nil || gotRaw != raw {
		t.Fatalf("v1 launch bytes changed: %q, %v", gotRaw, err)
	}
}

func TestVersionOneMigrationPreservesDescriptionsAndReopensIdempotently(t *testing.T) {
	root, fixture, want, raw := versionOneFixture(t)
	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTest(t, root)
	got, err := store.Get(want.ID)
	if err != nil || !reflect.DeepEqual(got, want) || got.Group != "" {
		t.Fatalf("migrated identity/launch/timestamps: %#v, %v", got, err)
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("migration version: %d, %v", version, err)
	}
	var gotRaw string
	if err := store.db.QueryRow("SELECT launch_json FROM native_sessions WHERE id=?", want.ID).Scan(&gotRaw); err != nil || gotRaw != raw {
		t.Fatalf("migration rewrote launch JSON: %q, %v", gotRaw, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTest(t, root)
	got, err = reopened.Get(want.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("second open repeated or changed migration: %#v, %v", got, err)
	}
	if err := reopened.Move(want.ID, "研究 🐈"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = openTest(t, root).Get(want.ID)
	if err != nil || got.Group != "研究 🐈" || got.ID != want.ID || !got.CreatedAt.Equal(want.CreatedAt) || !reflect.DeepEqual(got.Launch, want.Launch) {
		t.Fatalf("migrated group did not persist: %#v, %v", got, err)
	}
}

func TestVersionOneMigrationPreservesAllToolsAndLaunchBytes(t *testing.T) {
	root, db, first, firstRaw := versionOneFixture(t)
	wants := []Record{first}
	rawLaunches := map[string]string{first.ID: firstRaw}
	for index, tool := range []string{"codex", "custom"} {
		launch := testLaunch()
		launch.Executable = tool + ".exe"
		launch.Args = []string{"--literal", "中文 'quoted' $argument", ""}
		launch.Env = []string{"OVERRIDE=" + tool + " 中文 🐈"}
		launch.Dir = `C:\projects\` + tool
		created := first.CreatedAt.Add(time.Duration(index+1) * time.Second)
		record := Record{ID: uuid.NewString(), Name: "旧版本 " + tool, Tool: tool, Launch: launch, CreatedAt: created, UpdatedAt: created.Add(time.Minute)}
		raw, err := json.MarshalIndent(launch, "", "    ")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO native_sessions(id,name,tool,launch_json,created_at,updated_at) VALUES(?,?,?,?,?,?)", record.ID, record.Name, record.Tool, string(raw), record.CreatedAt.UnixNano(), record.UpdatedAt.UnixNano()); err != nil {
			t.Fatal(err)
		}
		wants = append(wants, record)
		rawLaunches[record.ID] = string(raw)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTest(t, root)
	got, err := store.List()
	if err != nil || !reflect.DeepEqual(got, wants) {
		t.Fatalf("migration lost IDs/tools/launches/order/timestamps: %#v, %v", got, err)
	}
	for _, record := range wants {
		var raw string
		if err := store.db.QueryRow("SELECT launch_json FROM native_sessions WHERE id=?", record.ID).Scan(&raw); err != nil || raw != rawLaunches[record.ID] {
			t.Fatalf("migration rewrote %s launch JSON bytes: %q, %v", record.Tool, raw, err)
		}
	}
}

func TestVersionOneMigrationRollsBackColumnAndVersionTogether(t *testing.T) {
	root, db, want, raw := versionOneFixture(t)
	store := &Store{db: db}
	failure := errors.New("isolated failure after ALTER and version update")
	if err := store.transaction(func(tx *sql.Tx) error {
		if err := migrateV1(tx); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("migration failure: %v", err)
	}
	assertVersionOneUnchanged(t, db, want, raw)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := openTest(t, root).Get(want.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("rolled-back migration cannot be retried safely: %#v, %v", got, err)
	}
}

func TestReadOnlyMigrationFailurePreservesVersionOneData(t *testing.T) {
	_, db, want, raw := versionOneFixture(t)
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if err := (&Store{db: db}).initialize(); err == nil {
		t.Fatal("read-only migration unexpectedly succeeded")
	}
	assertVersionOneUnchanged(t, db, want, raw)
}

func TestCorruptVersionOneSchemaOrRowsAreRejectedBeforeMigration(t *testing.T) {
	for _, mutation := range []string{
		"UPDATE native_sessions SET launch_json='{}'",
		"UPDATE native_sessions SET id='not-a-session'",
		"ALTER TABLE native_sessions ADD COLUMN unknown TEXT",
		"CREATE INDEX unknown_index ON native_sessions(name)",
		"CREATE TRIGGER unknown_trigger BEFORE UPDATE ON native_sessions BEGIN SELECT RAISE(ABORT,'unknown'); END",
		"CREATE TABLE sqliteX_unknown(data TEXT)",
		"CREATE INDEX sqliteX_unknown ON native_sessions(name)",
		"CREATE TRIGGER sqliteX_unknown BEFORE UPDATE ON native_sessions BEGIN SELECT RAISE(ABORT,'unknown'); END",
		"PRAGMA user_version=3",
	} {
		t.Run(mutation, func(t *testing.T) {
			root, db, _, _ := versionOneFixture(t)
			if _, err := db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			var wantSQL, wantLaunch string
			var wantVersion int
			if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE name='native_sessions'").Scan(&wantSQL); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT launch_json FROM native_sessions").Scan(&wantLaunch); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("PRAGMA user_version").Scan(&wantVersion); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				opened, err := Open(root)
				if opened != nil {
					_ = opened.Close()
				}
				if !errors.Is(err, ErrInvalidData) {
					t.Fatalf("unknown v1 data accepted or owner leaked: %v", err)
				}
			}
			after := openFixtureDatabase(t, filepath.Join(root, DatabaseFilename))
			var gotSQL, gotLaunch string
			var gotVersion int
			if err := after.QueryRow("SELECT sql FROM sqlite_master WHERE name='native_sessions'").Scan(&gotSQL); err != nil {
				t.Fatal(err)
			}
			if err := after.QueryRow("SELECT launch_json FROM native_sessions").Scan(&gotLaunch); err != nil {
				t.Fatal(err)
			}
			if err := after.QueryRow("PRAGMA user_version").Scan(&gotVersion); err != nil {
				t.Fatal(err)
			}
			if gotSQL != wantSQL || gotLaunch != wantLaunch || gotVersion != wantVersion {
				t.Fatalf("rejected migration changed old data/schema/version: %d -> %d", wantVersion, gotVersion)
			}
		})
	}
}

func TestUnversionedDatabaseCannotHideObjectsBehindSQLiteLikePrefix(t *testing.T) {
	root := privateRoot(t)
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, DatabaseFilename)
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db := openFixtureDatabase(t, path)
	if _, err := db.Exec("CREATE TABLE sqliteX_unknown(data TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO sqliteX_unknown(data) VALUES('preserve existing unversioned data')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(root)
	if opened != nil {
		_ = opened.Close()
	}
	if !errors.Is(err, ErrInvalidData) {
		t.Fatalf("unknown unversioned schema accepted: %v", err)
	}
	after := openFixtureDatabase(t, path)
	var version, tables int
	var data string
	if err := after.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Fatalf("rejected unversioned database changed version: %d, %v", version, err)
	}
	if err := after.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='native_sessions'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("rejected unversioned database gained a registry: %d, %v", tables, err)
	}
	if err := after.QueryRow("SELECT data FROM sqliteX_unknown").Scan(&data); err != nil || data != "preserve existing unversioned data" {
		t.Fatalf("rejected unversioned database lost existing data: %q, %v", data, err)
	}
}

func TestSchemaConstraintsAndGroupDefaultAreCheckedExactly(t *testing.T) {
	for _, definition := range []string{
		strings.Replace(schemaV1SQL, "CHECK(created_at > 0)", "CHECK(created_at > 1)", 1),
		strings.Replace(schemaV2SQL, "DEFAULT ''", "DEFAULT 'other'", 1),
		strings.Replace(schemaV2SQL, "DEFAULT ''", "DEFAULT NULL", 1),
		strings.Replace(schemaV2SQL, "TEXT NOT NULL DEFAULT ''", "TEXT DEFAULT ''", 1),
	} {
		t.Run(definition, func(t *testing.T) {
			root := privateRoot(t)
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, DatabaseFilename)
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
			db := openFixtureDatabase(t, path)
			if _, err := db.Exec(definition); err != nil {
				t.Fatal(err)
			}
			version := 2
			if !strings.Contains(definition, "group_name") {
				version = 1
			}
			if _, err := db.Exec("PRAGMA user_version=" + string(rune('0'+version))); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			opened, err := Open(root)
			if opened != nil {
				_ = opened.Close()
			}
			if !errors.Is(err, ErrInvalidData) {
				t.Fatalf("incompatible constraint/default accepted: %v", err)
			}
		})
	}
}

func TestGroupMovesAreAtomicPersistedAndDoNotTouchProject(t *testing.T) {
	root := privateRoot(t)
	project := filepath.Join(filepath.Dir(root), "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(project, "keep.txt")
	wantFile := []byte("unchanged project content\n")
	if err := os.WriteFile(marker, wantFile, 0600); err != nil {
		t.Fatal(err)
	}
	store := openTest(t, root)
	launch := testLaunch()
	launch.Dir = project
	record, err := store.CreateInGroup("中文会话", "shell", "　初始 🐈　", launch)
	if err != nil || record.Group != "初始 🐈" {
		t.Fatalf("canonical create: %#v, %v", record, err)
	}
	if err := store.Move(record.ID, " 研究 🐱 "); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(record.ID)
	if err != nil || got.Group != "研究 🐱" || got.UpdatedAt.Before(record.UpdatedAt) {
		t.Fatalf("canonical move: %#v, %v", got, err)
	}
	want := record
	want.Group, want.UpdatedAt = got.Group, got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("move changed immutable description: %#v", got)
	}
	if _, err := store.db.Exec("CREATE TRIGGER reject_move BEFORE UPDATE ON native_sessions BEGIN SELECT RAISE(ABORT,'isolated move failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := store.Move(record.ID, "不能提交"); err == nil {
		t.Fatal("failed move unexpectedly committed")
	}
	if after, err := store.Get(record.ID); err != nil || !reflect.DeepEqual(after, got) {
		t.Fatalf("failed move changed group/timestamp: %#v, %v", after, err)
	}
	if _, err := store.db.Exec("DROP TRIGGER reject_move"); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{" ", "\n组", strings.Repeat("界", 65)} {
		if err := store.Move(record.ID, group); !errors.Is(err, ErrInvalidData) {
			t.Fatalf("invalid move %q: %v", group, err)
		}
	}
	if err := store.Move(uuid.NewString(), "valid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing move: %v", err)
	}
	if after, err := store.Get(record.ID); err != nil || !reflect.DeepEqual(after, got) {
		t.Fatalf("rejected move changed description: %#v, %v", after, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTest(t, root)
	if after, err := reopened.Get(record.ID); err != nil || !reflect.DeepEqual(after, got) {
		t.Fatalf("group did not persist: %#v, %v", after, err)
	}
	if err := reopened.Move(record.ID, ""); err != nil {
		t.Fatal(err)
	}
	if after, err := reopened.Get(record.ID); err != nil || after.Group != "" || after.ID != record.ID || !reflect.DeepEqual(after.Launch, record.Launch) {
		t.Fatalf("move to default group: %#v, %v", after, err)
	}
	if gotFile, err := os.ReadFile(marker); err != nil || string(gotFile) != string(wantFile) {
		t.Fatalf("group operation changed project content: %v", err)
	}
}

func TestValidateGroupBoundaries(t *testing.T) {
	for _, group := range []string{"", "中文 🐈", "　有空格　", strings.Repeat("界", 64), strings.Repeat("😀", 64)} {
		if err := ValidateGroup(group); err != nil {
			t.Errorf("valid group %q: %v", group, err)
		}
	}
	for _, group := range []string{" ", "　", "\t", "group\n", "group\x00", "group\x7f", string([]byte{0xff}), strings.Repeat("界", 65), strings.Repeat("😀", 65)} {
		if err := ValidateGroup(group); !errors.Is(err, ErrInvalidData) {
			t.Errorf("invalid group %q: %v", group, err)
		}
	}
}
