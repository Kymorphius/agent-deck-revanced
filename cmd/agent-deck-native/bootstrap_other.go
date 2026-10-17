//go:build !windows

package main

// matrix/hardened: automatic native hosting is Windows-only, without a TCP fallback.

import (
	"context"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
)

func ensureNativeManager(context.Context, options) (terminalhost.Response, error) {
	return terminalhost.Response{}, winipc.ErrUnsupported
}
