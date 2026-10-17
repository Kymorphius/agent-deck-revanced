package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func moveCodexProbeToUserBin(t *testing.T, f *fakeCodex) string {
	t.Helper()
	bin := filepath.Join(os.Getenv("HOME"), ".local", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.dir, "codex"), bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

// matrix/hardened: SSH discovery must run, or reuse the same successful cache,
// when Codex is installed in the user bin but absent from the process PATH.
func TestMatrixModelProbeMinimalSSHPath(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh_probe", true: "existing_cache"}[warm], func(t *testing.T) {
			f := installFakeCodex(t, fakeCodexOK)
			bin := moveCodexProbeToUserBin(t, f)
			if warm {
				t.Setenv("PATH", filepath.Dir(bin)+":/usr/bin:/bin")
				if got := KnownModelIDsForTool("codex"); got[0] != "gpt-7-nova" {
					t.Fatal(got)
				}
				ResetModelProbeMemo()
			}
			t.Setenv("PATH", "/usr/bin:/bin")
			if _, err := exec.LookPath("codex"); err == nil {
				t.Fatal("test requires Codex absent from PATH")
			}
			if got := codexProbeBinary(); got != bin {
				t.Fatalf("probe binary=%s want %s", got, bin)
			}
			if got := KnownModelIDsForTool("codex"); got[0] != "gpt-7-nova" {
				t.Fatal(got)
			}
			if calls := f.probeCalls(t); calls != 1 {
				t.Fatalf("probe calls=%d want 1 (cached result must not re-probe)", calls)
			}
		})
	}
}

func TestMatrixModelProbeKeepsCommandPrecedence(t *testing.T) {
	f := installFakeCodex(t, fakeCodexOK)
	bin := filepath.Join(os.Getenv("HOME"), ".local", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 9\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := codexProbeBinary(); got != "codex" {
		t.Fatal("PATH binary must win:", got)
	}
	if got := KnownModelIDsForTool("codex"); got[0] != "gpt-7-nova" {
		t.Fatal(got)
	}
	for _, configured := range []string{"codex-v2", "/missing/bin/codex", "./codex"} {
		if got := resolveCodexProbeBinary(configured); got != configured {
			t.Fatalf("explicit command %q replaced with %q", configured, got)
		}
	}
	if f.probeCalls(t) != 1 {
		t.Fatal("PATH probe did not run")
	}
}

func TestMatrixModelProbeRejectsInvalidUserBinary(t *testing.T) {
	for _, kind := range []string{"missing", "not_executable", "directory", "broken_symlink"} {
		t.Run(kind, func(t *testing.T) {
			installFakeCodex(t, fakeCodexOK)
			t.Setenv("PATH", "/usr/bin:/bin")
			bin := filepath.Join(os.Getenv("HOME"), ".local", "bin", "codex")
			if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "not_executable":
				err = os.WriteFile(bin, []byte("not executable"), 0644)
			case "directory":
				err = os.Mkdir(bin, 0755)
			case "broken_symlink":
				err = os.Symlink(filepath.Join(t.TempDir(), "missing"), bin)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := codexProbeBinary(); got != "codex" {
				t.Fatalf("invalid fallback accepted: %s", got)
			}
		})
	}
}

func TestMatrixModelProbeDoesNotHideErrDot(t *testing.T) {
	f := installFakeCodex(t, fakeCodexOK)
	bin := moveCodexProbeToUserBin(t, f)
	t.Chdir(filepath.Dir(bin))
	t.Setenv("PATH", ".")
	// ErrDot must remain a refusal; a separate trusted fallback must not hide it.
	if _, err := exec.LookPath("codex"); err == nil || !errors.Is(err, exec.ErrDot) {
		t.Skip("platform does not report ErrDot")
	}
	if got := codexProbeBinary(); got != "codex" {
		t.Fatal("ErrDot was hidden by fallback:", got)
	}
}
