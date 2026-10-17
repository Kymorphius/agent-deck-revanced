package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
)

var (
	menuEventsPollInterval      = 2 * time.Second
	menuEventsHeartbeatInterval = 15 * time.Second
)

func (s *Server) handleMenuEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if !s.authorizeStreamRequest(r) {
		writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "stream unavailable")
		return
	}

	// matrix/hardened: subscribing creates demand for the single menu collector;
	// a new tab receives the existing publication instead of loading another menu.
	updates, release := s.menuPublisher.subscribeWire(menuEventsPollInterval, nil)
	defer release()
	var publication menuPublication
	select {
	case <-r.Context().Done():
		return
	case next, open := <-updates:
		if !open {
			return
		}
		publication = next
	}
	if publication.Err != nil {
		writeAPIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load menu data")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// matrix/hardened: reuse the publisher's fingerprint and wire encoding.
	lastFingerprint := publication.Fingerprint
	if err := writeEncodedSSEEvent(w, flusher, "menu", publication.encoded); err != nil {
		return
	}

	heartbeatTicker := time.NewTicker(menuEventsHeartbeatInterval)
	defer heartbeatTicker.Stop()

	ctx := r.Context()
	// matrix/hardened: only delivery/heartbeat remain per connection. Collection
	// errors do not emit stale menu data as a successful update.
	emitIfChanged := func(next menuPublication) error {
		if next.Err != nil {
			logging.ForComponent(logging.CompWeb).Error("menu_stream_refresh_failed",
				slog.String("error", next.Err.Error()))
			return nil
		}
		nextFingerprint := next.Fingerprint
		if nextFingerprint == lastFingerprint {
			return nil
		}

		if err := writeEncodedSSEEvent(w, flusher, "menu", next.encoded); err != nil {
			return err
		}
		lastFingerprint = nextFingerprint
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeatTicker.C:
			if err := writeSSEComment(w, flusher, "keepalive"); err != nil {
				return
			}
		case next, open := <-updates:
			if !open {
				return
			}
			if err := emitIfChanged(next); err != nil {
				return
			}
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// matrix/hardened: menu fanout supplies one already-encoded payload.
	return writeEncodedSSEEvent(w, flusher, event, data)
}

func writeEncodedSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, data []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeSSEComment(w http.ResponseWriter, flusher http.Flusher, comment string) error {
	if _, err := fmt.Fprintf(w, ": %s\n\n", comment); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func menuSnapshotFingerprint(snapshot *MenuSnapshot) string {
	if snapshot == nil {
		return "nil"
	}
	fingerprintPayload := struct {
		Profile       string     `json:"profile"`
		TotalGroups   int        `json:"totalGroups"`
		TotalSessions int        `json:"totalSessions"`
		Items         []MenuItem `json:"items"`
	}{
		Profile:       snapshot.Profile,
		TotalGroups:   snapshot.TotalGroups,
		TotalSessions: snapshot.TotalSessions,
		Items:         snapshot.Items,
	}

	raw, err := json.Marshal(fingerprintPayload)
	if err != nil {
		return "marshal-error"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
