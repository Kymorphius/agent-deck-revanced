//go:build !windows

package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// matrix/hardened: exercise the actual handler/PTY boundary. Merely opening a
// terminal, resizing it, pings, empty input, and read-only rejected input never
// promote a conversation; successful nonempty input does.
func TestMatrixWebRecentAccessAcceptedTerminalInput(t *testing.T) {
	requireTmuxForWebIntegration(t)
	t.Setenv("HOME", t.TempDir())
	for _, readOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "interactive", true: "read-only"}[readOnly], func(t *testing.T) {
			sessionID := fmt.Sprintf("recent-%t", readOnly)
			sessionName := fmt.Sprintf("agentdeck_web_recent_%d", time.Now().UnixNano())
			if out, err := exec.Command("tmux", "new-session", "-d", "-s", sessionName).CombinedOutput(); err != nil {
				t.Skipf("tmux new-session unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
			}
			defer func() { _ = exec.Command("tmux", "kill-session", "-t", sessionName).Run() }()
			old := time.Now().Add(-time.Hour).Truncate(time.Second)
			db := matrixRecentAccessDB(t, "recent-web", sessionID, old)
			server := NewServer(Config{Profile: "different-config-profile", ReadOnly: readOnly})
			server.menuData = &fakeMenuDataLoader{snapshot: &MenuSnapshot{
				Profile: "recent-web",
				Items: []MenuItem{{Type: MenuItemTypeSession, Session: &MenuSession{
					ID: sessionID, TmuxSession: sessionName,
				}}},
			}}
			ts := httptest.NewServer(server.Handler())
			defer ts.Close()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts.URL, "/ws/session/"+sessionID), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			waitForStatusOrSkipOnAttachFailure(t, conn, "terminal_attached")
			assertUnchanged := func() {
				t.Helper()
				if got := matrixReadRecentAccess(t, db, sessionID); !got.Equal(old) {
					t.Fatalf("passive/rejected input changed access time to %v", got)
				}
			}
			assertUnchanged()
			for _, msg := range []wsClientMessage{
				{Type: "resize", Cols: 80, Rows: 24},
				{Type: "input", Data: ""},
				{Type: "ping"},
			} {
				if err := conn.WriteJSON(msg); err != nil {
					t.Fatal(err)
				}
			}
			matrixWaitRecentPong(t, conn)
			assertUnchanged()
			if err := conn.WriteJSON(wsClientMessage{Type: "input", Data: " "}); err != nil {
				t.Fatal(err)
			}
			// A following pong proves the input branch completed while persistence
			// runs asynchronously, keeping the WebSocket read loop responsive.
			if err := conn.WriteJSON(wsClientMessage{Type: "ping"}); err != nil {
				t.Fatal(err)
			}
			matrixWaitRecentPong(t, conn)
			if readOnly {
				assertUnchanged()
			} else {
				deadline := time.Now().Add(3 * time.Second)
				for {
					if got := matrixReadRecentAccess(t, db, sessionID); got.After(old) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("accepted terminal input was not persisted")
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		})
	}
}

func matrixWaitRecentPong(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var msg wsServerMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "status" && msg.Event == "pong" {
			return
		}
	}
}
