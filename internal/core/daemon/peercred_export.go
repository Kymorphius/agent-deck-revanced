package daemon

import "net"

// PeerUID exposes the existing platform credential check to the independent
// matrix/hardened: read-only monitor transport, without exposing core commands.
func PeerUID(c *net.UnixConn) (int, error) { return peerUID(c) }
