package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// matrix/hardened: web terminal image upload (handlers_matrix_image.go).

var matrixTestPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)

func matrixImageServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.Profile = "work"
	srv := NewServer(cfg)
	srv.menuData = &fakeMenuDataLoader{snapshot: &MenuSnapshot{
		Profile: "work",
		Items: []MenuItem{{
			Type:    MenuItemTypeSession,
			Session: &MenuSession{ID: "sess-1"},
		}},
	}}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func matrixPostImage(t *testing.T, ts *httptest.Server, session, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/sessions/"+session+"/image", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestMatrixImageUploadStoresFileAndReturnsPath(t *testing.T) {
	ts := matrixImageServer(t, Config{Token: "tok", WebMutations: true})
	resp := matrixPostImage(t, ts, "sess-1", "tok", matrixTestPNG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got matrixImageResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got.Path) || !strings.HasSuffix(got.Path, ".png") {
		t.Fatalf("path = %q, want an absolute .png path", got.Path)
	}
	if filepath.Base(filepath.Dir(got.Path)) != "sess-1" || filepath.Base(filepath.Dir(filepath.Dir(got.Path))) != "uploads" {
		t.Fatalf("path = %q, want .../uploads/sess-1/<file>", got.Path)
	}
	if !strings.HasPrefix(got.Path, os.Getenv("HOME")) {
		t.Fatalf("path = %q escaped the isolated HOME", got.Path)
	}
	info, err := os.Stat(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(got.Path)
	if !bytes.Equal(data, matrixTestPNG) {
		t.Fatal("stored bytes differ from the upload")
	}
}

func TestMatrixImageUploadRejects(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		session string
		token   string
		body    []byte
		want    int
	}{
		{"no token", Config{Token: "tok", WebMutations: true}, "sess-1", "", matrixTestPNG, http.StatusUnauthorized},
		{"read-only", Config{Token: "tok", WebMutations: true, ReadOnly: true}, "sess-1", "tok", matrixTestPNG, http.StatusForbidden},
		{"mutations off", Config{Token: "tok"}, "sess-1", "tok", matrixTestPNG, http.StatusForbidden},
		{"unknown session", Config{Token: "tok", WebMutations: true}, "sess-404", "tok", matrixTestPNG, http.StatusNotFound},
		{"not an image", Config{Token: "tok", WebMutations: true}, "sess-1", "tok", []byte("#!/bin/sh\nrm -rf /\n"), http.StatusUnsupportedMediaType},
		{"empty", Config{Token: "tok", WebMutations: true}, "sess-1", "tok", nil, http.StatusUnsupportedMediaType},
		{"too big", Config{Token: "tok", WebMutations: true}, "sess-1", "tok", append(append([]byte{}, matrixTestPNG...), make([]byte, matrixImageMaxBytes)...), http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := matrixImageServer(t, tc.cfg)
			resp := matrixPostImage(t, ts, tc.session, tc.token, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestMatrixImageUploadRejectsCrossOrigin(t *testing.T) {
	ts := matrixImageServer(t, Config{Token: "tok", WebMutations: true})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/sessions/sess-1/image", bytes.NewReader(matrixTestPNG))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestMatrixPruneImagesDropsOnlyOldFiles(t *testing.T) {
	dir := t.TempDir()
	oldF, newF := filepath.Join(dir, "old.png"), filepath.Join(dir, "new.png")
	for _, f := range []string{oldF, newF} {
		if err := os.WriteFile(f, matrixTestPNG, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * matrixImageKeep)
	if err := os.Chtimes(oldF, past, past); err != nil {
		t.Fatal(err)
	}
	matrixPruneImages(dir, time.Now().Add(-matrixImageKeep))
	if _, err := os.Stat(oldF); !os.IsNotExist(err) {
		t.Fatal("old upload was not pruned")
	}
	if _, err := os.Stat(newF); err != nil {
		t.Fatal("recent upload was pruned")
	}
}
