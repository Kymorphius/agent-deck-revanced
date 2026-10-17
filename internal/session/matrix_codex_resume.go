package session

// matrix/hardened: resuming a Codex thread found by recall (`recall open`,
// global search `G`). Upstream only resumes Claude conversations from the
// index; a Codex hit was "searchable but not resumable yet".
//
// `codex resume <id>` looks the thread up by id under CODEX_HOME, so unlike
// Claude the start directory does not decide whether it is found. What does
// decide it is the home: buildCodexCommand silently drops a thread id whose
// rollout is not under the home the session launches with (#756) and starts
// a fresh conversation. So the rollout is located in exactly that home, and
// a miss is an error rather than a new, empty conversation.
//
// A thread still open elsewhere is not refused (unlike Claude): with the
// Codex app-server daemon every codex client of a thread goes through the
// daemon, which is its only writer, so a resumed session attaches to the
// live thread alongside the Codex app, provided the resume changes no
// config (CodexOptions.SharedThread). Codex's own thread-writer lock decides
// anything else.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// CodexResumeLocation is where a Codex thread can be resumed.
type CodexResumeLocation struct {
	Dir     string    // the thread's last recorded working directory
	Rollout string    // the rollout file `codex resume` will load
	Home    string    // the CODEX_HOME the new session launches with
	ModTime time.Time // the rollout's last write
}

// MarkCodexSharedThread records on inst that its Codex thread may be open
// elsewhere (CodexOptions.SharedThread), keeping its other Codex options.
func MarkCodexSharedThread(inst *Instance) error {
	opts := inst.GetCodexOptions()
	if opts == nil {
		opts = &CodexOptions{}
	}
	opts.SharedThread = true
	return inst.SetCodexOptions(opts)
}

func (i *Instance) codexSharedThread() bool {
	opts := i.GetCodexOptions()
	return opts != nil && opts.SharedThread
}

// IsBareCodexThreadID reports whether s is a bare lowercase UUID, the form
// Codex thread ids take. It is interpolated into `codex resume %s`.
func IsBareCodexThreadID(s string) bool {
	return uuidBareRegex.MatchString(s)
}

// LocateCodexResume finds threadID's rollout under the CODEX_HOME a new codex
// session for account ("" for none) would launch with, and the directory to
// start it in. It refuses subagent threads (codex never accepts input on
// them; resume their parent), and fails when the rollout is not in that home
// or the directory no longer exists, never guessing either.
func LocateCodexResume(account, threadID string) (CodexResumeLocation, error) {
	if !IsBareCodexThreadID(threadID) {
		return CodexResumeLocation{}, fmt.Errorf("thread id %q is not a bare Codex thread uuid", threadID)
	}
	home := CodexHomeDirForInstance(&Instance{Tool: "codex", Command: "codex", Account: strings.TrimSpace(account)})
	path := codexRolloutPathInHome(threadID, home)
	if path == "" {
		return CodexResumeLocation{}, fmt.Errorf("no rollout of Codex thread %s under %s, the Codex home a new session%s launches with; nothing to resume", threadID, home, accountClause(account))
	}
	info, err := os.Stat(path)
	if err != nil {
		return CodexResumeLocation{}, err
	}
	if meta := readCodexRolloutThreadMeta(path); meta.ThreadSource == "subagent" {
		if meta.ParentThreadID != "" {
			return CodexResumeLocation{}, fmt.Errorf("thread %s is a Codex subagent thread; resume its parent %s instead", threadID, meta.ParentThreadID)
		}
		return CodexResumeLocation{}, fmt.Errorf("thread %s is a Codex subagent thread; resume its parent instead", threadID)
	}
	dir := lastCodexRolloutCWD(path)
	if dir == "" {
		return CodexResumeLocation{}, fmt.Errorf("rollout %s records no working directory; nothing to resume from", path)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return CodexResumeLocation{}, fmt.Errorf("thread %s last ran in %s, which no longer exists", threadID, dir)
	}
	return CodexResumeLocation{Dir: dir, Rollout: path, Home: home, ModTime: info.ModTime()}, nil
}

func accountClause(account string) string {
	if account = strings.TrimSpace(account); account != "" {
		return " for account " + account
	}
	return ""
}

// lastCodexRolloutCWD returns the last cwd a rollout's session_meta or
// turn_context records carry: the directory the thread was last working in,
// which is what codex compares against when it is resumed elsewhere. Other
// records are skipped unparsed (tool output lines can be megabytes).
func lastCodexRolloutCWD(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	var cwd string
	for {
		line, readErr := reader.ReadBytes('\n')
		head := line
		if len(head) > 256 {
			head = head[:256]
		}
		if bytes.Contains(head, []byte(`"type":"turn_context"`)) || bytes.Contains(head, []byte(`"type":"session_meta"`)) {
			var rec struct {
				Payload struct {
					CWD string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Payload.CWD != "" {
				cwd = rec.Payload.CWD
			}
		}
		if readErr != nil {
			return cwd
		}
	}
}
