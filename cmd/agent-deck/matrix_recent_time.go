package main

import (
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: keep unknown visits absent; creation and background output
// must not masquerade as a user interaction in a remote Recent list.
func matrixListAccessedAt(inst *session.Instance) string {
	if inst.LastAccessedAt.IsZero() {
		return ""
	}
	return inst.LastAccessedAt.Format(time.RFC3339Nano)
}
