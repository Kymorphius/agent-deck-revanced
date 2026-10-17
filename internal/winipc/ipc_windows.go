//go:build windows

// matrix/hardened: named pipes accept only the current user and SYSTEM, with remote clients rejected.
package winipc

import (
	"context"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Listen creates a byte-stream local named pipe. It refuses an existing pipe;
// go-winio sets FILE_PIPE_REJECT_REMOTE_CLIENTS on every server handle.
func Listen(path string) (net.Listener, error) {
	if err := validatePipePath(path); err != nil {
		return nil, err
	}
	sid, err := CurrentUserSID()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)",
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
}

// Dial connects only inside the native IPC namespace. Before exposing the
// connection, it verifies the server process runs under this user's SID.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	if err := validatePipePath(path); err != nil {
		return nil, err
	}
	conn, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := verifyServerUser(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func verifyServerUser(conn net.Conn) error {
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("verify native IPC server: connection has no Windows handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(fd.Fd()), &pid); err != nil {
		return fmt.Errorf("query native IPC server PID: %w", err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("open native IPC server process: %w", err)
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("query native IPC server token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("query native IPC server user: %w", err)
	}
	current, err := CurrentUserSID()
	if err != nil {
		return err
	}
	if user.User.Sid.String() != current {
		return fmt.Errorf("native IPC server belongs to another user")
	}
	return nil
}

func CurrentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("query native IPC current user: %w", err)
	}
	return user.User.Sid.String(), nil
}
