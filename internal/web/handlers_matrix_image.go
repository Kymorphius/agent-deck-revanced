package web

// matrix/hardened: image upload for the web terminal. A phone has no way to
// get a screenshot onto the host, so the terminal strip grows an "image"
// button: the browser POSTs the raw file here, the server stores it under the
// profile dir and answers with its absolute path, and the client pastes that
// path into the pane, where Claude Code turns it into an [Image #n] attachment.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

const (
	matrixImageMaxBytes = 20 << 20
	matrixImageKeep     = 7 * 24 * time.Hour
)

// Claude reads these four; the type is sniffed from the bytes, never taken
// from the client's Content-Type.
var matrixImageExt = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

type matrixImageResponse struct {
	Path string `json:"path"`
}

func init() {
	registerFeatureRoutes(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("POST /api/sessions/{id}/image", s.handleMatrixImageUpload)
	})
}

func (s *Server) handleMatrixImageUpload(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeRequest(r) {
		writeAPIError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "unauthorized")
		return
	}
	// Same gate as terminal input: an upload exists only to be typed into a pane.
	if s.cfg.ReadOnly {
		writeAPIError(w, http.StatusForbidden, ErrCodeForbidden, "uploads are disabled in read-only mode")
		return
	}
	if !s.checkMutationsAllowed(w) || !s.checkMutationRateLimit(w) {
		return
	}

	// matrix/hardened: image target lookup shares the current menu publication.
	snapshot, err := s.menuPublisher.LoadMenuSnapshot()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to load session data")
		return
	}
	menuSession, found := snapshotSessionByID(snapshot, r.PathValue("id"))
	if !found {
		writeAPIError(w, http.StatusNotFound, ErrCodeNotFound, "session not found")
		return
	}
	// The id names a directory below; only a plain path element may do that.
	id := menuSession.ID
	if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
		writeAPIError(w, http.StatusBadRequest, ErrCodeBadRequest, "invalid session id")
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, matrixImageMaxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, ErrCodeBadRequest, "image is larger than 20 MB")
			return
		}
		writeAPIError(w, http.StatusBadRequest, ErrCodeBadRequest, "failed to read upload")
		return
	}
	ext, ok := matrixImageExt[http.DetectContentType(data)]
	if len(data) == 0 || !ok {
		writeAPIError(w, http.StatusUnsupportedMediaType, ErrCodeBadRequest, "only PNG, JPEG, GIF or WebP images")
		return
	}

	profileDir, err := session.GetProfileDir(s.cfg.Profile)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, ErrCodeInternalError, "no profile directory")
		return
	}
	dir := filepath.Join(profileDir, "uploads", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeAPIError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to create upload directory")
		return
	}
	matrixPruneImages(dir, time.Now().Add(-matrixImageKeep))

	var suffix [3]byte
	_, _ = rand.Read(suffix[:])
	path := filepath.Join(dir, time.Now().Format("20060102-150405-")+hex.EncodeToString(suffix[:])+ext)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		writeAPIError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to save image")
		return
	}
	writeJSON(w, http.StatusOK, matrixImageResponse{Path: path})
}

// matrixPruneImages drops this session's uploads older than cutoff, so the
// directory does not grow forever. Best effort: errors are ignored.
func matrixPruneImages(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
