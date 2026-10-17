package aiupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// matrix/hardened: malicious or damaged downloads must never escape staging or
// be accepted after a partial extract, while the complete native layout works.
func TestExtractPackageLayoutAndPermissions(t *testing.T) {
	archive := packageTestArchive(t, []packageTestEntry{
		{name: "./", kind: tar.TypeDir},
		{name: "bin/codex", body: "native codex", mode: 04751},
		{name: "bin/codex-code-mode-host", body: "native host", mode: 0755},
		{name: "codex-path/rg", body: "rg", mode: 0755},
		{name: "codex-package.json", body: `{ "version": "1.0.0" }`, mode: 0666},
		{name: "share/readme", body: "package data", mode: 0644},
		{name: "bin/", kind: tar.TypeDir},
	})
	destination := t.TempDir()
	if err := extractPackage(writePackageTestArchive(t, archive), destination); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "bin", "codex"))
	if err != nil || string(content) != "native codex" {
		t.Fatalf("codex payload = %q, %v", content, err)
	}
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{"bin/codex": 0755, "codex-package.json": 0644, "bin": 0755} {
			info, err := os.Stat(filepath.Join(destination, name))
			if err != nil || info.Mode().Perm() != want || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
				t.Errorf("%s permissions = %v, %v; want %v without special bits", name, info, err, want)
			}
		}
	}
}

func TestExtractPackageRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries []packageTestEntry
	}{
		{"parent", []packageTestEntry{{name: "../escaped", body: "bad"}}},
		{"normalized-parent", []packageTestEntry{{name: "bin/../escaped", body: "bad"}}},
		{"absolute", []packageTestEntry{{name: "/escaped", body: "bad"}}},
		{"windows-drive", []packageTestEntry{{name: "C:/escaped", body: "bad"}}},
		{"windows-separator", []packageTestEntry{{name: `bin\escaped`, body: "bad"}}},
		{"windows-unc", []packageTestEntry{{name: `\\host\escaped`, body: "bad"}}},
		{"symlink", []packageTestEntry{{name: "bin", kind: tar.TypeSymlink, link: "../outside"}}},
		{"hardlink", []packageTestEntry{{name: "bin/codex", kind: tar.TypeLink, link: "../outside"}}},
		{"device", []packageTestEntry{{name: "device", kind: tar.TypeChar}}},
		{"fifo", []packageTestEntry{{name: "pipe", kind: tar.TypeFifo}}},
		{"duplicate", []packageTestEntry{{name: "bin/codex", body: "first"}, {name: "./bin/codex", body: "second"}}},
		{"file-as-directory", []packageTestEntry{{name: "bin", body: "first"}, {name: "bin/codex", body: "second"}}},
		{"directory-as-file", []packageTestEntry{{name: "bin/codex", body: "first"}, {name: "bin", body: "second"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := extractPackage(writePackageTestArchive(t, packageTestArchive(t, test.entries)), t.TempDir()); err == nil {
				t.Fatal("unsafe archive was accepted")
			}
		})
	}
}

func TestExtractPackageRejectsExistingDestinationContents(t *testing.T) {
	archivePath := writePackageTestArchive(t, packageTestArchive(t, []packageTestEntry{{name: "bin/codex", body: "payload"}}))
	outside := t.TempDir()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(destination, "bin")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := extractPackage(archivePath, destination); err == nil {
		t.Fatal("nonempty staging directory was accepted")
	}
	rootLink := filepath.Join(t.TempDir(), "stage")
	if err := os.Symlink(outside, rootLink); err != nil {
		t.Fatal(err)
	}
	if err := extractPackage(archivePath, rootLink); err == nil {
		t.Fatal("symlink staging directory was accepted")
	}
	content, err := os.ReadFile(filepath.Join(outside, "keep"))
	if err != nil || string(content) != "original" {
		t.Fatalf("outside file changed: %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "codex")); !os.IsNotExist(err) {
		t.Fatalf("outside codex exists or cannot be checked: %v", err)
	}
}

func TestExtractPackageRejectsTruncatedAndTrailingData(t *testing.T) {
	valid := packageTestArchive(t, []packageTestEntry{{name: "bin/codex", body: "payload"}})
	badChecksum := append([]byte(nil), valid...)
	badChecksum[len(badChecksum)-8] ^= 0xff
	raw, err := gzip.NewReader(bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	untarred, err := io.ReadAll(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"missing-gzip-footer":    valid[:len(valid)-4],
		"bad-gzip-checksum":      badChecksum,
		"missing-tar-end":        packageTestGzip(t, untarred[:len(untarred)-1024]),
		"single-tar-end-block":   packageTestGzip(t, untarred[:len(untarred)-512]),
		"missing-file-data":      packageTestGzip(t, untarred[:515]),
		"nonzero-after-tar-end":  packageTestGzip(t, append(append([]byte(nil), untarred...), byte(1))),
		"second-gzip-payload":    append(append([]byte(nil), valid...), valid...),
		"trailing-garbage":       append(append([]byte(nil), valid...), []byte("garbage")...),
		"excessive-zero-padding": packageTestGzip(t, append(append([]byte(nil), untarred...), make([]byte, maxArchivePadding+1)...)),
	}
	for name, archive := range tests {
		t.Run(name, func(t *testing.T) {
			if err := extractPackage(writePackageTestArchive(t, archive), t.TempDir()); err == nil {
				t.Fatal("damaged or trailing archive data was accepted")
			}
		})
	}
}

func TestExtractPackageLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []packageTestEntry
		limits  packageExtractLimits
		want    string
	}{
		{"single-file", []packageTestEntry{{name: "one", body: "12345"}}, packageExtractLimits{4, 10, 2}, "size limit"},
		{"total-files", []packageTestEntry{{name: "one", body: "123"}, {name: "two", body: "456"}}, packageExtractLimits{4, 5, 2}, "size limit"},
		{"entry-count", []packageTestEntry{{name: "one"}, {name: "two"}}, packageExtractLimits{4, 5, 1}, "too many entries"},
		{"hidden-pax-entries", []packageTestEntry{{name: strings.Repeat("long", 60)}}, packageExtractLimits{4, 5, 1}, "too many entries"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := extractPackageWithLimits(writePackageTestArchive(t, packageTestArchive(t, test.entries)), t.TempDir(), test.limits)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v; want %q", err, test.want)
			}
		})
	}
}

func TestExtractPackageAcceptsPAXLongPath(t *testing.T) {
	name := strings.Repeat("long", 60)
	destination := t.TempDir()
	archive := packageTestArchive(t, []packageTestEntry{{name: name, body: "long name"}})
	if err := extractPackage(writePackageTestArchive(t, archive), destination); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(destination, name)); err != nil || string(content) != "long name" {
		t.Fatalf("long path payload = %q, %v", content, err)
	}
}

type packageTestEntry struct {
	name string
	body string
	kind byte
	link string
	mode int64
}

func packageTestArchive(t *testing.T, entries []packageTestEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: kind, Linkname: entry.link}
		if kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			if _, err := io.WriteString(tw, entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return packageTestGzip(t, raw.Bytes())
}

func packageTestGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var result bytes.Buffer
	w := gzip.NewWriter(&result)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func writePackageTestArchive(t *testing.T, data []byte) string {
	t.Helper()
	archive := filepath.Join(t.TempDir(), "package.tar.gz")
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	return archive
}
