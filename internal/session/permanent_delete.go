package session

// matrix/hardened: permanent deletion is a separate, previewed operation.
// Project files, other threads, forks and external backups are never targets.
import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/recall"
	recallstore "github.com/asheshgoplani/agent-deck/internal/recall/store"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

type PermanentDeletePlan struct {
	SessionID  string   `json:"session_id"`
	Title      string   `json:"title"`
	Harness    string   `json:"harness"`
	NativeIDs  []string `json:"native_ids"`
	Files      []string `json:"files"`
	Home       string   `json:"home"`
	RecallPath string   `json:"recall_path"`
	RecentID   string   `json:"recent_config_id"`
}

func (s *Storage) PlanPermanentDelete(inst *Instance) (PermanentDeletePlan, error) {
	p := PermanentDeletePlan{SessionID: inst.ID, Title: inst.Title}
	p.RecentID = statedb.RecentSessionConfigID(&statedb.RecentSessionRow{Title: inst.Title, ProjectPath: inst.ProjectPath, GroupPath: inst.GroupPath, Command: inst.Command, Wrapper: inst.Wrapper, Tool: inst.Tool, ToolOptions: inst.ToolOptionsJSON, SandboxEnabled: inst.Sandbox != nil, GeminiYoloMode: inst.GeminiYoloMode})
	if inst.IsSSH() || inst.IsSandboxed() {
		return p, errors.New("permanent deletion supports local unsandboxed Claude/Codex sessions only")
	}
	switch {
	case IsClaudeCompatible(inst.Tool):
		p.Harness = "claude"
		p.Home = GetClaudeConfigDirForInstance(inst)
		p.NativeIDs = []string{inst.ClaudeSessionID}
	case IsCodexCompatible(inst.Tool):
		p.Harness = "codex"
		p.Home = inst.ResolvedCodexHome()
		p.NativeIDs = []string{inst.CodexSessionID}
	default:
		return p, errors.New("permanent deletion supports Claude/Codex only")
	}
	links, err := s.db.ListSessionLinks(inst.ID)
	if err != nil {
		return p, err
	}
	for _, l := range links {
		if l.Host != "" || l.Harness != p.Harness {
			return p, errors.New("session has remote or mixed-harness history; refusing incomplete permanent deletion")
		}
		p.NativeIDs = append(p.NativeIDs, l.NativeID)
	}
	seen := map[string]bool{}
	ids := p.NativeIDs
	p.NativeIDs = nil
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		if err := validateExactSessionID(id); err != nil {
			return p, err
		}
		seen[id] = true
		p.NativeIDs = append(p.NativeIDs, id)
	}
	if len(p.NativeIDs) == 0 {
		return p, errors.New("no exact native conversation ID; cannot safely locate history")
	}
	profiles, err := ListProfiles()
	if err != nil {
		return p, err
	}
	for _, profile := range profiles {
		st, err := NewLiveReadOnlyStorageWithProfile(profile)
		if err != nil {
			return p, err
		}
		peers, err := st.Load()
		if err != nil {
			st.Close()
			return p, fmt.Errorf("profile %s: %w", profile, err)
		}
		for _, peer := range peers {
			if profile == s.Profile() && peer.ID == inst.ID {
				continue
			}
			sid := peer.ClaudeSessionID
			if p.Harness == "codex" {
				sid = peer.CodexSessionID
			}
			if seen[sid] {
				st.Close()
				return p, fmt.Errorf("native conversation is also referenced by %q (%s); delete only this registry entry instead", peer.Title, profile)
			}
			ls, e := st.db.ListSessionLinks(peer.ID)
			if e != nil {
				st.Close()
				return p, e
			}
			for _, l := range ls {
				if l.Harness == p.Harness && l.Host == "" && seen[l.NativeID] {
					st.Close()
					return p, fmt.Errorf("history is also linked to %q; refusing permanent deletion", peer.Title)
				}
			}
		}
		st.Close()
	}
	for _, id := range p.NativeIDs {
		if p.Harness == "codex" {
			paths, e := exactCodexRolloutMatches(id, p.Home)
			if e != nil {
				return p, e
			}
			p.Files = append(p.Files, paths...)
		} else {
			// The native UUID is exact; collect every project copy, not a latest-file guess.
			e := filepath.WalkDir(filepath.Join(p.Home, "projects"), func(path string, d os.DirEntry, e error) error {
				if os.IsNotExist(e) {
					return nil
				}
				if e != nil {
					return e
				}
				ownedDir := false
				if d.IsDir() && d.Name() == id {
					rel, e := filepath.Rel(filepath.Join(p.Home, "projects"), path)
					// Only projects/<project-slug>/<conversation-id>, never a
					// whole project whose directory happens to be UUID-shaped.
					ownedDir = e == nil && len(strings.Split(rel, string(os.PathSeparator))) == 2
				}
				if d.Name() == id+".jsonl" || ownedDir {
					p.Files = append(p.Files, path)
					if d.IsDir() {
						return filepath.SkipDir
					}
				}
				return nil
			})
			if e != nil {
				return p, e
			}
			for _, dir := range []string{"session-env", "file-history"} {
				path := filepath.Join(p.Home, dir, id)
				if _, e := os.Lstat(path); e == nil {
					p.Files = append(p.Files, path)
				} else if !os.IsNotExist(e) {
					return p, e
				}
			}
			todos, e := filepath.Glob(filepath.Join(p.Home, "todos", id+"-*.json"))
			if e != nil {
				return p, e
			}
			p.Files = append(p.Files, todos...)
		}
	}
	for _, path := range p.Files {
		if err := checkPermanentPath(p.Home, path); err != nil {
			return p, err
		}
	}
	p.RecallPath, err = recall.DBPath()
	if err != nil {
		return p, err
	}
	// An older account/home may still own indexed native files. Refuse to
	// claim a complete purge when those files are outside this approved scope.
	if err := checkIndexedPurgeScope(p); err != nil {
		return p, err
	}
	return p, nil
}

func checkIndexedPurgeScope(p PermanentDeletePlan) error {
	if _, err := os.Stat(p.RecallPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+p.RecallPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	for _, native := range p.NativeIDs {
		rows, err := db.Query(`WITH RECURSIVE gone(id) AS (
			SELECT sess_id FROM session WHERE harness=? AND native_id=? AND host_uid IN (SELECT host_uid FROM host WHERE is_local=1)
			UNION SELECT e.from_sess FROM conv_edge e JOIN gone g ON e.to_sess=g.id WHERE e.kind='subagent_of'
		) SELECT path FROM source WHERE sess_id IN (SELECT id FROM gone)`, p.Harness, native)
		if err != nil {
			return err
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			if _, err := os.Lstat(path); os.IsNotExist(err) {
				continue
			} else if err != nil {
				rows.Close()
				return err
			}
			covered := false
			for _, target := range p.Files {
				if path == target {
					covered = true
					break
				}
				info, e := os.Stat(target)
				if e == nil && info.IsDir() {
					rel, e := filepath.Rel(target, path)
					if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
						covered = true
						break
					}
				}
			}
			if !covered {
				rows.Close()
				return fmt.Errorf("indexed native history outside planned files: %s; refusing incomplete permanent deletion", path)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func checkPermanentPath(root, path string) error {
	r, e := filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	q, e := filepath.EvalSymlinks(path)
	if e != nil {
		return e
	}
	rel, e := filepath.Rel(r, q)
	if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("unsafe permanent deletion path %s", path)
	}
	return nil
}

func (s *Storage) PermanentlyDelete(inst *Instance, approved PermanentDeletePlan) error {
	// Rebuild the plan immediately before teardown: links and ownership may change.
	p, err := s.PlanPermanentDelete(inst)
	if err != nil {
		return err
	}
	a, _ := json.Marshal(approved)
	b, _ := json.Marshal(p)
	if !bytes.Equal(a, b) {
		return errors.New("deletion plan changed; preview and confirm again")
	}
	// matrix/hardened: native UI and CLI execution must both protect their hosting terminal.
	if err := CheckPermanentDeleteTerminal(inst); err != nil {
		return err
	}
	lock, err := recall.LockPath()
	if err != nil {
		return err
	}
	release, err := recallstore.Lock(lock)
	if err != nil {
		return err
	}
	defer release()
	// Stop native writers first. A kill failure must not be reported as deletion.
	unit := inst.ServiceUnitOwnership()
	if inst.Exists() {
		if err := inst.KillAndWait(); err != nil {
			return fmt.Errorf("stop session: %w", err)
		}
	}
	_ = inst.RetireServiceUnit(unit)
	if inst.Exists() {
		return errors.New("session is still running after teardown; refusing history deletion")
	}
	for _, path := range p.Files {
		if err := checkPermanentPath(p.Home, path); err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	for _, name := range []string{"history.jsonl", "session_index.jsonl"} {
		path := filepath.Join(p.Home, name)
		if _, e := os.Lstat(path); e == nil {
			if err := checkPermanentPath(p.Home, path); err != nil {
				return err
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		if err := wipeHistoryRecords(filepath.Join(p.Home, name), p.NativeIDs); err != nil {
			return err
		}
	}
	if p.Harness == "codex" {
		if err := purgeCodexDatabases(p.Home, p.NativeIDs); err != nil {
			return err
		}
	}
	if err := purgeRecall(p.RecallPath, p); err != nil {
		return err
	}
	if path, e := inst.IdentityFilePath(); e == nil {
		root, e := IdentityDir()
		if e != nil {
			return e
		}
		dir := filepath.Dir(path)
		if _, e := os.Lstat(dir); e == nil {
			if e := checkPermanentPath(root, dir); e != nil {
				return e
			}
			if e := os.RemoveAll(dir); e != nil {
				return e
			}
		}
	}
	if err := pruneHookArtifacts(inst.ID); err != nil {
		return err
	}
	if _, err := SweepInboxesForChildSession(inst.ID); err != nil {
		return err
	}
	if _, err := RemoveNotifyStateRecord(inst.ID); err != nil {
		return err
	}
	return s.purgeRegistry(p)
}

// Preserve the append-only history inode/offsets and concurrent appends. Only
// exact matching lines are blanked; no global rewrite/truncate of other history.
func wipeHistoryRecords(path string, ids []string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	r := bufio.NewReader(f)
	var off int64
	for {
		line, e := r.ReadBytes('\n')
		if e != nil && e != io.EOF {
			return e
		}
		if len(line) == 0 {
			break
		}
		var v map[string]json.RawMessage
		if json.Unmarshal(line, &v) == nil {
			for _, key := range []string{"sessionId", "session_id", "thread_id", "id"} {
				var id string
				_ = json.Unmarshal(v[key], &id)
				if set[id] {
					old := make([]byte, len(line))
					if _, e := f.ReadAt(old, off); e != nil {
						return e
					}
					if !bytes.Equal(old, line) {
						return errors.New("history changed while deleting; retry")
					}
					blank := bytes.Repeat([]byte{' '}, len(line))
					if line[len(line)-1] == '\n' {
						blank[len(blank)-1] = '\n'
					}
					if _, e := f.WriteAt(blank, off); e != nil {
						return e
					}
					break
				}
			}
		}
		off += int64(len(line))
		if e == io.EOF {
			break
		}
	}
	return f.Sync()
}

func openPurgeDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=secure_delete(ON)")
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func purgeCodexDatabases(home string, ids []string) error {
	paths, err := filepath.Glob(filepath.Join(home, "state_*.sqlite"))
	if err != nil {
		return err
	}
	paths = append(paths, filepath.Join(home, "thread_history_1.sqlite"))
	for _, path := range paths {
		if _, e := os.Lstat(path); e == nil {
			if err := checkPermanentPath(home, path); err != nil {
				return err
			}
		}
		db, err := openPurgeDB(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		err = purgeNativeRows(db, ids)
		db.Close()
		if err != nil {
			return fmt.Errorf("native history %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func purgeNativeRows(db *sql.DB, ids []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Explicit schemas only; a new native schema must not silently delete all rows.
	targets := map[string][]string{"threads": {"id"}, "thread_dynamic_tools": {"thread_id"}, "thread_spawn_edges": {"parent_thread_id", "child_thread_id"}, "thread_attachments": {"thread_id"}, "thread_turns": {"thread_id"}, "thread_items": {"thread_id"}, "thread_history_projection_state": {"thread_id"}, "thread_realtime_items": {"thread_id"}}
	for table, cols := range targets {
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		for _, col := range cols {
			for _, id := range ids {
				if _, err := tx.Exec(`DELETE FROM "`+table+`" WHERE "`+col+`"=?`, id); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func purgeRecall(path string, p PermanentDeletePlan) error {
	db, err := openPurgeDB(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, native := range p.NativeIDs {
		rows, err := tx.Query(`WITH RECURSIVE gone(id) AS (
			SELECT sess_id FROM session WHERE harness=? AND native_id=? AND host_uid IN (SELECT host_uid FROM host WHERE is_local=1)
			UNION SELECT e.from_sess FROM conv_edge e JOIN gone g ON e.to_sess=g.id WHERE e.kind='subagent_of'
		) SELECT id FROM gone`, p.Harness, native)
		if err != nil {
			return err
		}
		var sessions []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			sessions = append(sessions, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range sessions {
			if _, err := tx.Exec("DELETE FROM msg_fts WHERE rowid IN (SELECT msg_id FROM msg WHERE sess_id=?)", id); err != nil {
				return err
			}
			for _, table := range []string{"msg", "tool_call", "artifact", "enrich_queue", "file_touch", "card", "tombstone", "source", "session"} {
				if _, err := tx.Exec(`DELETE FROM "`+table+`" WHERE sess_id=?`, id); err != nil {
					return err
				}
			}
			if _, err := tx.Exec("DELETE FROM conv_edge WHERE from_sess=? OR to_sess=?", id, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Storage) purgeRegistry(p PermanentDeletePlan) error {
	tx, err := s.db.DB().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("PRAGMA secure_delete=ON"); err != nil {
		return err
	}
	// Tombstones stop an already-running collector or stale TUI from restoring
	// the deleted entry. They contain only the deck ID, never conversation text.
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS permanently_deleted_sessions(id TEXT PRIMARY KEY)`,
		`CREATE TRIGGER IF NOT EXISTS permanent_session_no_restore BEFORE INSERT ON instances WHEN EXISTS(SELECT 1 FROM permanently_deleted_sessions WHERE id=NEW.id) BEGIN SELECT RAISE(IGNORE); END`,
		`CREATE TRIGGER IF NOT EXISTS permanent_cost_no_restore BEFORE INSERT ON cost_events WHEN EXISTS(SELECT 1 FROM permanently_deleted_sessions WHERE id=NEW.session_id) BEGIN SELECT RAISE(IGNORE); END`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO permanently_deleted_sessions(id) VALUES(?)", p.SessionID); err != nil {
		return err
	}
	for _, table := range []string{"cost_events", "session_links", "session_claims"} {
		if _, err := tx.Exec(`DELETE FROM "`+table+`" WHERE session_id=?`, p.SessionID); err != nil {
			return err
		}
	}
	for _, table := range []string{"session_hints", "session_tags"} {
		if _, err := tx.Exec(`DELETE FROM "`+table+`" WHERE scope_kind='instance' AND scope_id=?`, p.SessionID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM watcher_events WHERE session_id=? OR triage_session_id=?", p.SessionID, p.SessionID); err != nil {
		return err
	}
	for _, table := range []string{"recent_sessions", "instances"} {
		id := p.SessionID
		if table == "recent_sessions" {
			id = p.RecentID
		}
		if _, err := tx.Exec(`DELETE FROM "`+table+`" WHERE id=?`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.db.Touch()
}
