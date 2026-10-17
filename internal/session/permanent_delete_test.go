package session

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/recall"
	recallstore "github.com/asheshgoplani/agent-deck/internal/recall/store"
)

// matrix/hardened: test only synthetic stopped sessions under isolated homes.
func permanentFixture(t *testing.T, tool string) (*Storage, *Instance, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	st, err := NewStorageWithProfile("default")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	inst := NewInstanceWithTool("erase fixture", filepath.Join(home, "project"), tool)
	const sid = "11111111-1111-4111-8111-111111111111"
	root := filepath.Join(home, ".claude")
	path := filepath.Join(root, "projects", "-fixture", sid+".jsonl")
	if tool == "codex" {
		inst.CodexSessionID = sid
		root = filepath.Join(home, ".codex")
		path = filepath.Join(root, "sessions", "2026", "10", "12", "rollout-2026-10-12T10-00-00-"+sid+".jsonl")
	} else {
		inst.ClaudeSessionID = sid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("private fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.Save([]*Instance{inst}); err != nil {
		t.Fatal(err)
	}
	return st, inst, root, path
}

func TestPermanentDeleteNativeAndRegistry(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			st, inst, root, path := permanentFixture(t, tool)
			sid := inst.ClaudeSessionID
			if tool == "codex" {
				sid = inst.CodexSessionID
			}
			neighbor := filepath.Join(root, "neighbor.jsonl")
			_ = os.WriteFile(neighbor, []byte("keep"), 0600)
			history := filepath.Join(root, "history.jsonl")
			original := `{"session_id":"` + sid + `","display":"private prompt"}` + "\n" + `{"session_id":"other","display":"keep prompt"}` + "\n"
			_ = os.WriteFile(history, []byte(original), 0600)
			if tool == "codex" {
				db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY,title TEXT); INSERT INTO threads VALUES('` + sid + `','private'),('other','keep')`)
				if err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			db := st.db.DB()
			if _, err := db.Exec(`INSERT INTO cost_events(id,session_id,timestamp,model,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_microdollars) VALUES('fixture',?,'2026-10-12','fixture',1,1,0,0,1)`, inst.ID); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveRecentSession(inst); err != nil {
				t.Fatal(err)
			}
			rp, err := recall.DBPath()
			if err != nil {
				t.Fatal(err)
			}
			rs, err := recallstore.Open(rp)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{`INSERT OR IGNORE INTO host(host_uid,is_local) VALUES('local',1)`, `INSERT INTO session(sess_id,host_uid,harness,native_id) VALUES(1,'local','` + tool + `','` + sid + `'),(2,'local','` + tool + `','other')`, `INSERT INTO card(sess_id,title) VALUES(1,'private'),(2,'keep')`,
				`INSERT INTO session(sess_id,host_uid,harness,native_id) VALUES(3,'local','` + tool + `','child')`,
				`INSERT INTO conv_edge(from_sess,to_sess,kind,created_at) VALUES(3,1,'subagent_of',0)`, `INSERT INTO msg(msg_id,sess_id,src_id,seq,role,class,ts,rec_off,rec_len) VALUES(1,1,1,1,0,0,0,0,0)`, `INSERT INTO msg_fts(rowid,body) VALUES(1,'private')`} {
				if _, err := rs.W.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			rs.Close()
			plan, err := st.PlanPermanentDelete(inst)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("planning mutated the native transcript")
			}
			if err := st.PermanentlyDelete(inst, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("transcript survived")
			}
			if _, err := os.Stat(neighbor); err != nil {
				t.Fatal("neighbor removed")
			}
			b, _ := os.ReadFile(history)
			if len(b) != len(original) || strings.Contains(string(b), "private prompt") || !strings.Contains(string(b), "keep prompt") {
				t.Fatalf("history: %q", b)
			}
			for _, q := range []string{"SELECT count(*) FROM instances", "SELECT count(*) FROM recent_sessions", "SELECT count(*) FROM cost_events"} {
				var n int
				if err := db.QueryRow(q).Scan(&n); err != nil || n != 0 {
					t.Fatalf("registry %s: %d %v", q, n, err)
				}
			}
			// A stale collector cannot reinsert the removed costs.
			_, err = db.Exec(`INSERT INTO cost_events(id,session_id,timestamp,model,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_microdollars) VALUES('late',?,'2026-10-12','fixture',1,1,0,0,1)`, inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			_ = db.QueryRow("SELECT count(*) FROM cost_events").Scan(&n)
			if n != 0 {
				t.Fatal("late cost resurrected")
			}
			rs, err = recallstore.OpenCurrent(rp)
			if err != nil {
				t.Fatal(err)
			}
			defer rs.Close()
			_ = rs.R.QueryRow("SELECT count(*) FROM session").Scan(&n)
			if n != 1 {
				t.Fatal("recall neighbor lost or target remains")
			}
		})
	}
}

func TestPermanentDeleteRefusesSharedAndChangedPlan(t *testing.T) {
	st, inst, _, path := permanentFixture(t, "codex")
	plan, err := st.PlanPermanentDelete(inst)
	if err != nil {
		t.Fatal(err)
	}
	peer := NewInstanceWithTool("shared", inst.ProjectPath, "codex")
	peer.CodexSessionID = inst.CodexSessionID
	if err := st.Save([]*Instance{inst, peer}); err != nil {
		t.Fatal(err)
	}
	if err := st.PermanentlyDelete(inst, plan); err == nil {
		t.Fatal("shared native conversation deleted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("refusal touched transcript")
	}
	if err := st.Save([]*Instance{inst}); err != nil {
		t.Fatal(err)
	}
	plan.Title = "unapproved title change"
	if err := st.PermanentlyDelete(inst, plan); err == nil {
		t.Fatal("changed plan accepted")
	}
}

func TestPermanentDeleteHistoryKeepsAppendAndSymlinkRefusal(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	_ = os.WriteFile(outside, []byte("keep"), 0600)
	link := filepath.Join(root, "link")
	_ = os.Symlink(outside, link)
	if checkPermanentPath(root, link) == nil {
		t.Fatal("external symlink accepted")
	}
	path := filepath.Join(root, "history.jsonl")
	v, _ := json.Marshal(map[string]string{"session_id": "one", "display": "erase"})
	_ = os.WriteFile(path, append(v, '\n'), 0600)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := wipeHistoryRecords(path, []string{"one"}); err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{\"session_id\":\"other\",\"display\":\"new\"}\n")
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "erase") || !strings.Contains(string(b), "new") {
		t.Fatalf("history append: %q", b)
	}
}

func TestPermanentDeleteRefusesIndexedFileOutsidePlan(t *testing.T) {
	st, inst, _, path := permanentFixture(t, "codex")
	outside := filepath.Join(t.TempDir(), "old-account.jsonl")
	if err := os.WriteFile(outside, []byte("keep old history"), 0600); err != nil {
		t.Fatal(err)
	}
	rp, err := recall.DBPath()
	if err != nil {
		t.Fatal(err)
	}
	rs, err := recallstore.Open(rp)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO session(sess_id,host_uid,harness,native_id) VALUES(1,'local','codex','` + inst.CodexSessionID + `')`,
	} {
		if _, err := rs.W.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rs.W.Exec(`INSERT INTO source(src_id,host_uid,harness,path,dev,ino,sess_id) VALUES(1,'local','codex',?,1,1,1)`, outside); err != nil {
		t.Fatal(err)
	}
	rs.Close()
	if _, err := st.PlanPermanentDelete(inst); err == nil {
		t.Fatal("indexed old-account file omitted from purge scope")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("preflight refusal removed current log")
	}
}
