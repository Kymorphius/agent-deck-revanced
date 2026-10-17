package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/logging"
	"github.com/gorilla/websocket"
)

// matrix/hardened: dashboard traffic is server-push only and shares the
// terminal's existing authentication/origin boundary, without its input bridge.
var dashboardUpgrader = websocket.Upgrader{
	ReadBufferSize: 1024, WriteBufferSize: 4096, CheckOrigin: allowWSOrigin,
}

var (
	dashboardEventsPollInterval = defaultMenuPublisherInterval
	dashboardHeartbeatInterval  = 15 * time.Second
	dashboardPongWait           = 60 * time.Second
	dashboardWriteTimeout       = 5 * time.Second
)

const dashboardMaxInboundBytes = 1024

type dashboardEnvelope struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// matrix/hardened: one upgraded socket per page multiplexes menu and command
// center, avoiding the browser's six ordinary HTTP/1 connections per origin.
// Bad credentials, origins and upgrade requests fail before collector demand.
func (s *Server) handleDashboardWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if !s.authorizeWSRequest(r) {
		writeAPIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
		return
	}
	if !allowWSOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "FORBIDDEN", "websocket origin is not allowed")
		return
	}
	conn, err := dashboardUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	pongWait, heartbeat, writeTimeout := dashboardPongWait, dashboardHeartbeatInterval, dashboardWriteTimeout
	conn.SetReadLimit(dashboardMaxInboundBytes)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(pongWait)) })
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			// Application messages have no meaning on this transport. Only
			// protocol ping/pong/close (handled by gorilla) are accepted.
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "dashboard is server-push only"),
				time.Now().Add(writeTimeout))
			return
		}
	}()
	// matrix/hardened: independently close the socket on cancellation so a
	// blocked network write/read cannot delay server shutdown. Join both workers.
	finished, closerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(closerDone)
		select {
		case <-s.baseCtx.Done():
		case <-r.Context().Done():
		case <-readDone:
		case <-finished:
		}
		_ = conn.Close()
	}()
	defer func() {
		close(finished)
		_ = conn.Close()
		<-readDone
		<-closerDone
	}()
	tracker := &ccPublicationTracker{}
	// matrix/hardened: only this exact opt-in uses versioned/delta menu messages.
	// Missing/unknown values retain the original full-menu envelope unchanged.
	protocol := r.URL.Query()["menu_protocol"]
	deltaMenu := len(protocol) == 1 && protocol[0] == "delta-v1"
	updates, release := s.menuPublisher.subscribeWire(dashboardEventsPollInterval, tracker.observe)
	defer release()
	write := func(encoded []byte) error {
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, encoded)
	}
	writeEnvelope := func(kind string, data any) error {
		encoded, err := json.Marshal(dashboardEnvelope{kind, data})
		if err != nil {
			return err
		}
		return write(encoded)
	}
	lastMenu, lastCC := "", ""
	emit := func(publication menuPublication) error {
		menu, cc, err := s.commandCenterForPublication(publication, tracker)
		if err != nil {
			if tracker.overflowed() {
				_ = writeEnvelope("resync", apiError{Code: "RESYNC_REQUIRED", Message: "Reconnect to refresh dashboard state"})
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "dashboard resync required"), time.Now().Add(writeTimeout))
				return err
			}
			logging.ForComponent(logging.CompWeb).Error("dashboard_refresh_failed", slog.String("error", err.Error()))
			// matrix/hardened: recovery must republish even byte-identical state
			// after an error; otherwise clients remain marked disconnected forever.
			lastMenu, lastCC = "", ""
			// Errors neither consume completion backlog nor present stale data
			// as success. Keep the transport alive for the next successful poll.
			return writeEnvelope("error", apiError{Code: "SNAPSHOT_UNAVAILABLE", Message: "Dashboard snapshot is temporarily unavailable"})
		}
		// matrix/hardened: CC capture can upgrade a queued value to current. Select
		// menu bytes from that returned owner; advance the version only after write.
		if err := writeDashboardMenu(menu, deltaMenu, &lastMenu, write); err != nil {
			return err
		}
		if cc.fingerprint != lastCC || len(cc.snapshot.RecentlyCompleted) > 0 {
			// Base CC encoding is shared; only a connection's completion
			// overlay causes private encoding of that payload.
			encoded := append([]byte(`{"type":"command-center","data":`), cc.encoded...)
			encoded = append(encoded, '}')
			if err := write(encoded); err != nil {
				return err
			}
			lastCC = cc.fingerprint
		}
		return nil
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.baseCtx.Done():
			return
		case <-r.Context().Done():
			return
		case <-readDone:
			return
		case publication, open := <-updates:
			if !open || emit(publication) != nil {
				return
			}
		case <-ticker.C:
			if err := writeEnvelope("heartbeat", struct {
				Time string `json:"time"`
			}{time.Now().UTC().Format(time.RFC3339)}); err != nil {
				return
			}
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				return
			}
		}
	}
}
