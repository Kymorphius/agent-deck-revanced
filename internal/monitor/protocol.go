// Package monitor shares complete runtime observations among local clients.
// matrix/hardened: this protocol contains no session command execution routes.
package monitor

import (
	"errors"
	"fmt"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

const Protocol = session.RuntimeProtocolVersion
const protocolHeader = "Agent-Deck-Monitor-Protocol"
const maxResponseBytes = 16 << 20
const maxHistoryBytes = 64 << 20

var ErrClosed = errors.New("monitor client is closed")

// GapError means ordered observations were retired or the owner restarted.
// Consumers must explicitly re-establish their baseline rather than infer edges.
type GapError struct {
	Epoch  string `json:"epoch"`
	Oldest uint64 `json:"oldest"`
	Latest uint64 `json:"latest"`
}

func (e *GapError) Error() string {
	return fmt.Sprintf("monitor observations gap: epoch %s, retained revisions %d..%d", e.Epoch, e.Oldest, e.Latest)
}
func (e *GapError) RuntimeObservationGap() bool { return true }

type wireError struct {
	Error string    `json:"error"`
	Gap   *GapError `json:"gap,omitempty"`
}

// Status provides operational counters without returning session data.
type Status struct {
	Protocol       int       `json:"protocol"`
	PID            int       `json:"pid"`
	Version        string    `json:"version"`
	Epoch          string    `json:"epoch"`
	StartedAt      time.Time `json:"startedAt"`
	Profiles       int       `json:"profiles"`
	ActiveProfiles int       `json:"activeProfiles"`
	Connections    int64     `json:"connections"`
	Requests       uint64    `json:"requests"`
	SourceLoads    uint64    `json:"sourceLoads"`
	Publications   uint64    `json:"publications"`
	Errors         uint64    `json:"errors"`
	Inflight       int       `json:"inflight"`
	// matrix/hardened: local, fixed-category counters expose query cost, not argv.
	TmuxCommands tmux.SubprocessMetricsSnapshot `json:"tmuxCommands"`
	// matrix/hardened: fixed batch admission/dispatch counters contain no targets.
	BatchScheduling CaptureBatchScheduleStats `json:"batchScheduling"`
	// matrix/hardened: bounded counters/PIDs expose owned query clients only.
	QueryTransport tmux.QueryTransportStats `json:"queryTransport"`
}

type publication struct {
	snapshot session.RuntimeProfileSnapshot
	encoded  []byte
}
