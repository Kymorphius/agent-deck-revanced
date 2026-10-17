package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidatedCodexExecutable(t *testing.T) {
	// These inert fixtures exercise selection only; no executable is launched.
	root := t.TempDir()
	executable := filepath.Join(root, "codex.exe")
	wrongName := filepath.Join(root, "other.exe")
	for _, path := range []string{executable, wrongName} {
		if err := os.WriteFile(path, []byte("inert Codex selection fixture\n"), 0600); err != nil {
			t.Fatalf("create fixture %q: %v", path, err)
		}
	}
	directory := filepath.Join(root, "directory", "codex.exe")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatalf("create directory fixture: %v", err)
	}
	if !filepath.IsAbs(executable) {
		t.Fatalf("fixture must be absolute: %q", executable)
	}

	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "empty preserves default"},
		{name: "absolute regular codex.exe", path: executable, want: filepath.Clean(executable)},
		{name: "relative codex.exe", path: filepath.Join("relative", "codex.exe"), wantErr: true},
		{name: "wrong executable filename", path: wrongName, wantErr: true},
		{name: "absent codex.exe", path: filepath.Join(root, "absent", "codex.exe"), wantErr: true},
		{name: "directory named codex.exe", path: directory, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validatedCodexExecutable(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatedCodexExecutable(%q) = (%q, %v), want error %t", tt.path, got, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("validatedCodexExecutable(%q) returned %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
