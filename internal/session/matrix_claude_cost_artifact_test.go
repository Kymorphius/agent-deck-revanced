package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// matrix/hardened: cost resolution shares context export's account/cwd exact
// path rather than rediscovering a potentially foreign transcript by ID.
func matrixClaudeCostFile(t *testing.T, config, project, sid string) string {
	t.Helper()
	path := filepath.Join(config, "projects", ConvertToClaudeDirName(project), sid+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMatrixClaudeCostTranscriptAccountsRemainSeparate(t *testing.T) {
	home, err := filepath.EvalSymlinks(withTempAgentDeckHome(t, `
[profiles.personal.claude]
config_dir = "~/.claude-personal"
[profiles.work.claude]
config_dir = "~/.claude-work"
`))
	if err != nil {
		t.Fatal(err)
	}
	// macOS /var is a symlink; use its real temporary root rather than
	// weakening the canonical resolver's account-path safety assertion.
	t.Setenv("HOME", home)
	ClearUserConfigCache()
	t.Cleanup(ClearUserConfigCache)
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	const sid = "same-thread"
	personal := matrixClaudeCostFile(t, filepath.Join(home, ".claude-personal"), project, sid)
	work := matrixClaudeCostFile(t, filepath.Join(home, ".claude-work"), project, sid)
	for account, path := range map[string]string{"personal": personal, "work": work} {
		inst := &Instance{Tool: "claude", Account: account, ProjectPath: project, ClaudeSessionID: sid}
		got, err := inst.ClaudeCostTranscriptPath()
		if got != path || err != nil {
			t.Fatalf("account %q = %q, %v; want %q", account, got, err, path)
		}
	}
	if err := os.Remove(personal); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{Tool: "claude", Account: "personal", ProjectPath: project, ClaudeSessionID: sid}
	if got, err := inst.ClaudeCostTranscriptPath(); got != personal || err != nil {
		t.Fatalf("missing personal path borrowed work's file: %q, %v", got, err)
	}
	inst.Account = "unknown"
	if got, err := inst.ClaudeCostTranscriptPath(); got != "" || err == nil || !strings.Contains(err.Error(), "no configured config_dir") {
		t.Fatalf("unknown account fell back to another home: %q, %v", got, err)
	}
}

func TestMatrixClaudeCostTranscriptUsesEffectiveWorkingDir(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	project := filepath.Join(home, "display-project")
	working := filepath.Join(home, "actual-launch-cwd")
	const sid = "thread"
	matrixClaudeCostFile(t, config, project, sid)
	path := matrixClaudeCostFile(t, config, working, sid)
	inst := &Instance{Tool: "claude", ProjectPath: project, MultiRepoEnabled: true, MultiRepoTempDir: working, ClaudeSessionID: sid}
	if got, err := inst.ClaudeCostTranscriptPath(); got != path || err != nil {
		t.Fatalf("effective cwd = %q, %v; want %q", got, err, path)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := inst.ClaudeCostTranscriptPath(); got != path || err != nil {
		t.Fatalf("missing effective cwd file fell back to display project: %q, %v", got, err)
	}
}

func TestMatrixClaudeCostTranscriptRejectsNonlocalAndInvalidIdentity(t *testing.T) {
	project := t.TempDir()
	for _, inst := range []*Instance{
		nil,
		{Tool: "claude", ProjectPath: project, ClaudeSessionID: "thread", SSHHost: "remote"},
		{Tool: "claude", ProjectPath: project, ClaudeSessionID: "thread", Sandbox: &SandboxConfig{Enabled: true}},
	} {
		if path, err := inst.ClaudeCostTranscriptPath(); path != "" || err == nil || !strings.Contains(err.Error(), "not locally readable") {
			t.Fatalf("nonlocal instance selected %q: %v", path, err)
		}
	}
	for _, sid := range []string{"", " ", "../foreign", "nested/thread", "nested\\thread", "*", "?", "[thread]", "thread\x00"} {
		inst := &Instance{Tool: "claude", ProjectPath: project, ClaudeSessionID: sid}
		if path, err := inst.ClaudeCostTranscriptPath(); path != "" || err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("invalid identity %q selected %q: %v", sid, path, err)
		}
	}
	if path, err := (&Instance{Tool: "claude", ClaudeSessionID: "thread"}).ClaudeCostTranscriptPath(); path != "" || err == nil || !strings.Contains(err.Error(), "working directory is empty") {
		t.Fatalf("empty working directory selected %q: %v", path, err)
	}
}

func TestMatrixClaudeCostTranscriptLiteralMissingPath(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	project := filepath.Join(home, "project")
	for _, sid := range []string{"sid.$+{}", " raw-thread "} {
		inst := &Instance{Tool: "claude", ProjectPath: project, ClaudeSessionID: sid}
		want := filepath.Join(config, "projects", ConvertToClaudeDirName(project), sid+".jsonl")
		got, err := inst.ClaudeCostTranscriptPath()
		if got != want || err != nil {
			t.Fatalf("literal missing path = %q, %v; want %q", got, err, want)
		}
		if _, err := os.Stat(got); !os.IsNotExist(err) {
			t.Fatalf("fixture must remain missing for reader skip: %v", err)
		}
		canonical, err := canonicalClaudeExactTranscriptPath(inst)
		if canonical != got || err != nil {
			t.Fatalf("cost path diverged from canonical export path: %q, %v", canonical, err)
		}
	}
}

func TestMatrixClaudeCostTranscriptRejectsSymlinkComponents(t *testing.T) {
	for _, component := range []string{"account", "projects", "project", "artifact"} {
		t.Run(component, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(home, "claude")
			t.Setenv("CLAUDE_CONFIG_DIR", config)
			project := filepath.Join(home, "working")
			const sid = "thread"
			path := matrixClaudeCostFile(t, config, project, sid)
			link := config
			switch component {
			case "projects":
				link = filepath.Join(config, "projects")
			case "project":
				link = filepath.Dir(path)
			case "artifact":
				link = path
			}
			outside := filepath.Join(home, "outside")
			if err := os.Rename(link, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			inst := &Instance{Tool: "claude", ProjectPath: project, ClaudeSessionID: sid}
			if got, err := inst.ClaudeCostTranscriptPath(); got != "" || err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlink %s accepted: %q, %v", component, got, err)
			}
		})
	}
}
