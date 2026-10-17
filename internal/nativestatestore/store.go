// matrix/hardened: native sessions share the original instances/groups schema via exact row transactions.
package nativestatestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/google/uuid"
)

type Store struct {
	mu       sync.Mutex
	db       *statedb.StateDB
	lease    *nativeregistry.Lease
	closeErr error
}

// Open is opt-in and requires an explicit private directory. It never opens
// the user's profile, imports the prototype registry, or restarts saved agents.
func Open(dir string) (_ *Store, resultErr error) {
	// StateDB.Open accepts a filename DSN. Reject URI aliases before creating
	// any files; the original API remains unchanged for this bounded reuse slice.
	if strings.ContainsAny(dir, "?#%") {
		return nil, fmt.Errorf("%w: shared state directory contains URI metacharacters", nativeregistry.ErrUnsafePath)
	}
	lease, err := nativeregistry.AcquireLease(dir, DatabaseFilename)
	if err != nil {
		return nil, err
	}
	s := &Store{lease: lease}
	defer func() {
		if resultErr != nil {
			_ = s.Close()
		}
	}()
	path := filepath.Join(dir, DatabaseFilename)
	if err := preflight(path); err != nil {
		return nil, err
	}
	s.db, err = statedb.Open(path)
	if err != nil {
		return nil, err
	}
	if err := s.db.Migrate(); err != nil {
		return nil, err
	}
	if _, err := s.list(); err != nil {
		return nil, err
	}
	return s, nil
}

func fileURI(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// Reject declared future/malformed StateDB and native descriptors before
// invoking its additive schema migration. The schema version remains 13.
func preflight(path string) error {
	db, err := sql.Open("sqlite", fileURI(path)+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	var objects int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name NOT GLOB 'sqlite_*'").Scan(&objects); err != nil {
		return err
	}
	if objects == 0 {
		return nil
	}
	var version string
	if err := db.QueryRow("SELECT value FROM metadata WHERE key='schema_version'").Scan(&version); err != nil {
		return invalid("shared StateDB has no declared schema version")
	}
	v, err := strconv.Atoi(version)
	if err != nil || v < 1 || v > statedb.SchemaVersion {
		return invalid("unsupported shared StateDB schema version " + version)
	}
	if v == statedb.SchemaVersion {
		// matrix/hardened: reject conflicting persisted native identity before
		// even invoking additive current-schema initialization.
		rows, err := db.Query("SELECT " + rowColumns + " FROM instances")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if _, _, err := scanNative(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	// Native descriptions are only introduced into the current shared schema.
	rows, err := db.Query("SELECT tool_data FROM instances")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		backend, err := DecodeBackend(json.RawMessage(raw))
		if err != nil {
			return err
		}
		if backend != nil {
			return invalid("native backend requires the current shared StateDB schema")
		}
	}
	return rows.Err()
}

func (s *Store) checkOpen() error {
	if s == nil || s.db == nil {
		return nativeregistry.ErrClosed
	}
	return s.lease.Verify()
}

func (s *Store) Directory() (string, error) {
	if s == nil {
		return "", nativeregistry.ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return "", err
	}
	return s.lease.Directory()
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		s.closeErr = errors.Join(s.closeErr, s.db.Close())
		s.db = nil
	}
	if s.lease != nil {
		s.closeErr = errors.Join(s.closeErr, s.lease.Close())
		s.lease = nil
	}
	return s.closeErr
}

const rowColumns = "id,title,project_path,group_path,tool,tmux_session,tmux_socket_name,created_at,tool_data"

type scanner interface{ Scan(...any) error }

func scanNative(row scanner) (nativeregistry.Record, bool, error) {
	var instance statedb.InstanceRow
	var created int64
	var toolData string
	if err := row.Scan(&instance.ID, &instance.Title, &instance.ProjectPath, &instance.GroupPath, &instance.Tool, &instance.TmuxSession, &instance.TmuxSocketName, &created, &toolData); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nativeregistry.Record{}, false, nativeregistry.ErrNotFound
		}
		return nativeregistry.Record{}, false, err
	}
	instance.ToolData = json.RawMessage(toolData)
	instance.CreatedAt = time.Unix(created, 0).UTC()
	return RecordFromRow(&instance)
}

func (s *Store) list() ([]nativeregistry.Record, error) {
	rows, err := s.db.DB().Query("SELECT " + rowColumns + " FROM instances ORDER BY created_at,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]nativeregistry.Record, 0)
	for rows.Next() {
		record, native, err := scanNative(rows)
		if err != nil {
			return nil, err
		}
		if native {
			records = append(records, record)
		}
	}
	return records, rows.Err()
}

func (s *Store) List() ([]nativeregistry.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	return s.list()
}

func nativeResult(row scanner) (nativeregistry.Record, error) {
	record, native, err := scanNative(row)
	if err != nil {
		return nativeregistry.Record{}, err
	}
	if !native {
		return nativeregistry.Record{}, nativeregistry.ErrNotFound
	}
	return record, nil
}

func (s *Store) Get(id string) (nativeregistry.Record, error) {
	if err := nativeregistry.ValidateID(id); err != nil {
		return nativeregistry.Record{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nativeregistry.Record{}, err
	}
	return nativeResult(s.db.DB().QueryRow("SELECT "+rowColumns+" FROM instances WHERE id=?", id))
}

// BEGIN IMMEDIATE fences validation and exact row mutation against other
// StateDB writers. No stale full-row replacement or whole-table sweep is used.
func (s *Store) transaction(fn func(*sql.Conn) error) error {
	ctx := context.Background()
	conn, err := s.db.DB().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	if err := fn(conn); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func ensureGroup(conn *sql.Conn, group string) error {
	var path string
	for _, name := range strings.Split(group, "/") {
		if path == "" {
			path = name
		} else {
			path += "/" + name
		}
		if _, err := conn.ExecContext(context.Background(), "INSERT OR IGNORE INTO groups(path,name,expanded,sort_order,default_path,max_concurrent) VALUES(?,?,1,0,'',1)", path, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Create(name, tool string, launch terminalhost.LaunchSpec) (nativeregistry.Record, error) {
	return s.CreateInGroup(name, tool, "", launch)
}

func (s *Store) CreateInGroup(name, tool, group string, launch terminalhost.LaunchSpec) (nativeregistry.Record, error) {
	if err := nativeregistry.ValidateName(name); err != nil {
		return nativeregistry.Record{}, err
	}
	if tool != "shell" && tool != "codex" && tool != "powershell" && tool != "custom" {
		return nativeregistry.Record{}, invalid("unsupported native tool")
	}
	group, err := canonicalGroup(group)
	if err != nil {
		return nativeregistry.Record{}, err
	}
	now := time.Now().UTC()
	raw, err := MarshalBackend(Backend{Version: BackendVersion, Kind: BackendKind, Launch: launch, CreatedAt: now.UnixNano(), UpdatedAt: now.UnixNano()})
	if err != nil {
		return nativeregistry.Record{}, err
	}
	toolData, err := json.Marshal(map[string]json.RawMessage{BackendExtraKey: raw})
	if err != nil {
		return nativeregistry.Record{}, err
	}
	// Clone slices so callers cannot mutate a saved description through a DTO.
	backend, err := DecodeBackend(toolData)
	if err != nil {
		return nativeregistry.Record{}, err
	}
	record := nativeregistry.Record{ID: uuid.NewString(), Name: name, Tool: tool, Group: group, Launch: backend.Launch, CreatedAt: now, UpdatedAt: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nativeregistry.Record{}, err
	}
	err = s.transaction(func(conn *sql.Conn) error {
		if err := ensureGroup(conn, group); err != nil {
			return err
		}
		// matrix/hardened: a saved launch is neutral until a runtime observer
		// reports facts; do not manufacture an AI error from a stopped record.
		_, err := conn.ExecContext(context.Background(), "INSERT INTO instances(id,title,project_path,group_path,tool,status,created_at,tool_data) VALUES(?,?,?,?,?,'unknown',?,?)", record.ID, name, launch.Dir, group, tool, now.Unix(), string(toolData))
		return err
	})
	if err != nil {
		return nativeregistry.Record{}, err
	}
	return record, nil
}

func (s *Store) mutate(id string, change func(*sql.Conn, nativeregistry.Record) error) error {
	if err := nativeregistry.ValidateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	return s.transaction(func(conn *sql.Conn) error {
		record, err := nativeResult(conn.QueryRowContext(context.Background(), "SELECT "+rowColumns+" FROM instances WHERE id=?", id))
		if err != nil {
			return err
		}
		return change(conn, record)
	})
}

func (s *Store) Rename(id, name string) error {
	if err := nativeregistry.ValidateName(name); err != nil {
		return err
	}
	return s.mutate(id, func(conn *sql.Conn, record nativeregistry.Record) error {
		_, err := conn.ExecContext(context.Background(), "UPDATE instances SET title=?,auto_name=0,tool_data=json_set(tool_data,'$.native_backend.updated_at',?) WHERE id=?", name, max(record.UpdatedAt.UnixNano(), time.Now().UTC().UnixNano()), id)
		return err
	})
}

func (s *Store) Move(id, group string) error {
	group, err := canonicalGroup(group)
	if err != nil {
		return err
	}
	return s.mutate(id, func(conn *sql.Conn, record nativeregistry.Record) error {
		if err := ensureGroup(conn, group); err != nil {
			return err
		}
		_, err := conn.ExecContext(context.Background(), "UPDATE instances SET group_path=?,tool_data=json_set(tool_data,'$.native_backend.updated_at',?) WHERE id=?", group, max(record.UpdatedAt.UnixNano(), time.Now().UTC().UnixNano()), id)
		return err
	})
}

func (s *Store) Delete(id string) error {
	return s.mutate(id, func(conn *sql.Conn, _ nativeregistry.Record) error {
		_, err := conn.ExecContext(context.Background(), "DELETE FROM instances WHERE id=?", id)
		return err
	})
}
