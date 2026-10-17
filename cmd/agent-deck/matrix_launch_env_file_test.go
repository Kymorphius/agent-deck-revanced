package main

import (
	"os"
	"path/filepath"
	"testing"
)

// matrix/hardened: only private, absolute, quote-safe env files are accepted.
func TestValidateLaunchEnvFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(good, []byte("export X='1'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateLaunchEnvFile(good); err != nil {
		t.Fatalf("private file refused: %v", err)
	}
	open := filepath.Join(dir, "open.env")
	_ = os.WriteFile(open, nil, 0o644)
	odd := filepath.Join(dir, "a$b.env")
	_ = os.WriteFile(odd, nil, 0o600)
	for _, bad := range []string{"agent.env", open, odd, filepath.Join(dir, "missing.env"), dir} {
		if validateLaunchEnvFile(bad) == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}
