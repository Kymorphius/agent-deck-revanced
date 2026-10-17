package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// matrix/hardened: synthetic historical trees retain the old exact resolver as
// an oracle; the optimization changes enumeration, not artifact acceptance.
func matrixCostRolloutFile(t *testing.T, home, day, name string) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "2026", "10", day, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func matrixCostRolloutInstance(home, sid string) *Instance {
	return &Instance{Tool: "codex", Command: "CODEX_HOME='" + home + "' codex", CodexSessionID: sid}
}

func matrixCostRolloutParity(t *testing.T, resolver *CodexCostRolloutResolver, inst *Instance) {
	t.Helper()
	want, wantErr := inst.CodexCostRolloutPath()
	got, gotErr := resolver.Resolve(inst)
	if got != want || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
		t.Fatalf("new resolver = %q, %v; old resolver = %q, %v", got, gotErr, want, wantErr)
	}
}

func TestMatrixCodexCostRolloutResolverOneGlobPerHome(t *testing.T) {
	root := t.TempDir()
	resolver := NewCodexCostRolloutResolver()
	scans := make(map[string]int)
	resolver.glob = func(pattern string) ([]string, error) {
		scans[pattern]++
		return filepath.Glob(pattern)
	}
	for _, home := range []string{filepath.Join(root, "one"), filepath.Join(root, "two")} {
		for n := 0; n < 32; n++ {
			sid := fmt.Sprintf("thread-%02d", n)
			matrixCostRolloutFile(t, home, "13", "rollout-date-"+sid+".jsonl")
		}
		for n := 0; n < 32; n++ {
			inst := matrixCostRolloutInstance(home, fmt.Sprintf("thread-%02d", n))
			matrixCostRolloutParity(t, resolver, inst)
			matrixCostRolloutParity(t, resolver, inst)
		}
	}
	if len(scans) != 2 {
		t.Fatalf("scanned %d homes, want two: %v", len(scans), scans)
	}
	for pattern, count := range scans {
		if count != 1 || !strings.HasSuffix(pattern, "/rollout-*.jsonl") {
			t.Fatalf("scan %q = %d, want one broad rollout glob", pattern, count)
		}
	}
}

func TestMatrixCodexCostRolloutResolverLiteralMatchingParity(t *testing.T) {
	for _, tc := range []struct {
		name, sid, file string
		found           bool
	}{
		{"normal", "thread", "rollout-date-thread.jsonl", true},
		{"empty wildcard", "thread", "rollout--thread.jsonl", true},
		{"overlapping separators", "thread", "rollout-thread.jsonl", false},
		{"other ID suffix", "thread", "rollout-date-otherthread.jsonl", false},
		{"other ID extension", "thread", "rollout-date-thread-other.jsonl", false},
		{"other prefix", "thread", "other-date-thread.jsonl", false},
		{"other extension", "thread", "rollout-date-thread.jsonl.bak", false},
		{"literal punctuation", "sid.$+{}", "rollout-date-sid.$+{}.jsonl", true},
		{"raw whitespace preserved", " thread ", "rollout-date- thread .jsonl", true},
		{"whitespace is not rewritten", " thread ", "rollout-date-thread.jsonl", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := matrixCostRolloutFile(t, home, "13", tc.file)
			inst := matrixCostRolloutInstance(home, tc.sid)
			resolver := NewCodexCostRolloutResolver()
			matrixCostRolloutParity(t, resolver, inst)
			got, err := resolver.Resolve(inst)
			if tc.found && (err != nil || got != path) {
				t.Fatalf("literal file not selected: %q, %v", got, err)
			}
			if !tc.found && !errors.Is(err, errNoExactContextArtifact) {
				t.Fatalf("nonmatch must be missing, got %q, %v", got, err)
			}
		})
	}
}

func TestMatrixCodexCostRolloutResolverRejectedArtifactsParity(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			const sid = "thread"
			path := matrixCostRolloutFile(t, home, "13", "rollout-date-"+sid+".jsonl")
			switch kind {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				matrixCostRolloutFile(t, home, "14", "rollout-another-"+sid+".jsonl")
			case "directory", "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(filepath.Join(home, "outside"), path); err != nil {
					t.Fatal(err)
				}
			}
			inst := matrixCostRolloutInstance(home, sid)
			resolver := NewCodexCostRolloutResolver()
			matrixCostRolloutParity(t, resolver, inst)
			if got, err := resolver.Resolve(inst); got != "" || err == nil {
				t.Fatalf("accepted %s artifact: %q, %v", kind, got, err)
			}
		})
	}
}

func TestMatrixCodexCostRolloutResolverGatesBeforeGlob(t *testing.T) {
	home := t.TempDir()
	resolver := NewCodexCostRolloutResolver()
	resolver.glob = func(string) ([]string, error) {
		t.Fatal("rejected identity or remote/container path enumerated a local home")
		return nil, nil
	}
	for _, sid := range []string{"", " ", "../thread", "thread/other", "*", "?", "[thread]", "thread\\other", "thread\x00"} {
		matrixCostRolloutParity(t, resolver, matrixCostRolloutInstance(home, sid))
	}
	for _, inst := range []*Instance{
		nil,
		{Tool: "codex", SSHHost: "remote", CodexSessionID: "thread"},
		{Tool: "codex", Sandbox: &SandboxConfig{Enabled: true}, CodexSessionID: "thread"},
	} {
		matrixCostRolloutParity(t, resolver, inst)
	}
}

func TestMatrixCodexCostRolloutResolverAccountAndExpandedHome(t *testing.T) {
	workHome := codexAccountHome(t)
	root := os.Getenv("HOME")
	const sid = "thread"
	workPath := matrixCostRolloutFile(t, workHome, "13", "rollout-date-"+sid+".jsonl")
	globalPath := matrixCostRolloutFile(t, filepath.Join(root, "codex-global"), "13", "rollout-date-"+sid+".jsonl")
	resolver := NewCodexCostRolloutResolver()
	scans := 0
	resolver.glob = func(pattern string) ([]string, error) {
		scans++
		return filepath.Glob(pattern)
	}
	for _, tc := range []struct {
		inst *Instance
		path string
	}{
		{&Instance{Tool: "codex", Account: "work", CodexSessionID: sid}, workPath},
		{matrixCostRolloutInstance("~/codex-work", sid), workPath},
		{matrixCostRolloutInstance(workHome+"/.", sid), workPath},
		{&Instance{Tool: "codex", Account: "missing-account", CodexSessionID: sid}, globalPath},
		{&Instance{Tool: "codex", Account: "work", Command: "CODEX_HOME='~/codex-global' codex", CodexSessionID: sid}, globalPath},
	} {
		matrixCostRolloutParity(t, resolver, tc.inst)
		if got, err := resolver.Resolve(tc.inst); got != tc.path || err != nil {
			t.Fatalf("account/home resolution = %q, %v; want %q", got, err, tc.path)
		}
	}
	if scans != 2 {
		t.Fatalf("account/tilde/clean aliases scanned %d times, want two homes", scans)
	}
	if err := os.Remove(workPath); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{Tool: "codex", Account: "work", CodexSessionID: sid}
	if got, err := resolver.Resolve(inst); got != "" || !errors.Is(err, errNoExactContextArtifact) {
		t.Fatalf("missing bound artifact borrowed the other account: %q, %v", got, err)
	}
}

func TestMatrixCodexCostRolloutResolverNextPassRescans(t *testing.T) {
	home := t.TempDir()
	const sid = "thread"
	inst := matrixCostRolloutInstance(home, sid)
	first := NewCodexCostRolloutResolver()
	if _, err := first.Resolve(inst); !errors.Is(err, errNoExactContextArtifact) {
		t.Fatalf("first pass missing: %v", err)
	}
	path := matrixCostRolloutFile(t, home, "13", "rollout-date-"+sid+".jsonl")
	if _, err := first.Resolve(inst); !errors.Is(err, errNoExactContextArtifact) {
		t.Fatalf("first pass unexpectedly changed its directory snapshot: %v", err)
	}
	second := NewCodexCostRolloutResolver()
	if got, err := second.Resolve(inst); got != path || err != nil {
		t.Fatalf("next pass missed new rollout: %q, %v", got, err)
	}
	duplicate := matrixCostRolloutFile(t, home, "14", "rollout-duplicate-"+sid+".jsonl")
	if got, err := second.Resolve(inst); got != path || err != nil {
		t.Fatalf("same directory snapshot unexpectedly included new duplicate: %q, %v", got, err)
	}
	third := NewCodexCostRolloutResolver()
	if _, err := third.Resolve(inst); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("next pass failed to find duplicate: %v", err)
	}
	if err := os.Remove(duplicate); err != nil {
		t.Fatal(err)
	}
	if got, err := third.Resolve(inst); got != path || err != nil {
		t.Fatalf("per-resolution Lstat failed to notice duplicate removal: %q, %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, resolver := range []*CodexCostRolloutResolver{second, NewCodexCostRolloutResolver()} {
		if _, err := resolver.Resolve(inst); !errors.Is(err, errNoExactContextArtifact) {
			t.Fatalf("removed rollout reused: %v", err)
		}
	}
}

func TestMatrixCodexCostRolloutResolverLstatAfterEnumeration(t *testing.T) {
	for _, replacement := range []string{"removed", "directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			home := t.TempDir()
			path := matrixCostRolloutFile(t, home, "13", "rollout-date-thread.jsonl")
			resolver := NewCodexCostRolloutResolver()
			resolver.glob = func(pattern string) ([]string, error) {
				paths, err := filepath.Glob(pattern)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				switch replacement {
				case "directory":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(filepath.Join(home, "outside"), path); err != nil {
						t.Fatal(err)
					}
				}
				return paths, nil
			}
			inst := matrixCostRolloutInstance(home, "thread")
			if got, err := resolver.Resolve(inst); got != "" || err == nil {
				t.Fatalf("post-enumeration %s accepted: %q, %v", replacement, got, err)
			}
			matrixCostRolloutParity(t, resolver, inst)
		})
	}
}

func TestMatrixCodexCostRolloutResolverGlobErrorIsPassLocal(t *testing.T) {
	home := t.TempDir()
	inst := matrixCostRolloutInstance(home, "thread")
	resolver := NewCodexCostRolloutResolver()
	scans := 0
	resolver.glob = func(string) ([]string, error) {
		scans++
		return nil, filepath.ErrBadPattern
	}
	for n := 0; n < 2; n++ {
		if _, err := resolver.Resolve(inst); !errors.Is(err, filepath.ErrBadPattern) || !strings.HasPrefix(err.Error(), "locate Codex rollout:") {
			t.Fatalf("glob error lost original semantics: %v", err)
		}
	}
	if scans != 1 {
		t.Fatalf("error retried within pass %d times", scans)
	}
	path := matrixCostRolloutFile(t, home, "13", "rollout-date-thread.jsonl")
	if got, err := NewCodexCostRolloutResolver().Resolve(inst); got != path || err != nil {
		t.Fatalf("new pass retained earlier glob error: %q, %v", got, err)
	}
	bad := matrixCostRolloutInstance(filepath.Join(home, "["), "thread")
	matrixCostRolloutParity(t, NewCodexCostRolloutResolver(), bad)
}

func TestMatrixCodexCostRolloutResolverConcurrentSingleEnumeration(t *testing.T) {
	home := t.TempDir()
	path := matrixCostRolloutFile(t, home, "13", "rollout-date-thread.jsonl")
	resolver := NewCodexCostRolloutResolver()
	scans := 0
	resolver.glob = func(pattern string) ([]string, error) {
		scans++ // serialized by the pass resolver
		return filepath.Glob(pattern)
	}
	var wg sync.WaitGroup
	for n := 0; n < 32; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := resolver.Resolve(matrixCostRolloutInstance(home, "thread")); got != path || err != nil {
				t.Errorf("concurrent result = %q, %v", got, err)
			}
		}()
	}
	wg.Wait()
	if scans != 1 {
		t.Fatalf("concurrent pass scanned %d times, want one", scans)
	}
}
