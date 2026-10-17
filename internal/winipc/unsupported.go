//go:build !windows

// matrix/hardened: unsupported hosts cannot silently replace private Windows IPC with network IPC.
package winipc

import (
	"context"
	"net"
)

func Listen(path string) (net.Listener, error) {
	if err := validatePipePath(path); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

func Dial(_ context.Context, path string) (net.Conn, error) {
	if err := validatePipePath(path); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

func ProtectPath(path string) error {
	if err := validateStatePath(path); err != nil {
		return err
	}
	return ErrUnsupported
}

func CurrentUserSID() (string, error) { return "", ErrUnsupported }
