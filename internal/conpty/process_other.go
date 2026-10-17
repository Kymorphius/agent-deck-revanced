//go:build !windows

// matrix/hardened: the experimental Windows backend never falls back to tmux,
// WSL, or another host when native Windows support is unavailable.
package conpty

import (
	"io"
	"strings"
)

type Process struct{}

func Start(options Options) (*Process, error) {
	if _, err := normalizeOptions(options); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

func (*Process) PID() uint32           { return 0 }
func (*Process) Input() io.Writer      { return unsupportedWriter{} }
func (*Process) Output() io.Reader     { return strings.NewReader("") }
func (*Process) Resize(int, int) error { return ErrUnsupported }
func (*Process) Wait() (uint32, error) { return 0, ErrUnsupported }
func (*Process) Close() error          { return ErrUnsupported }

type unsupportedWriter struct{}

func (unsupportedWriter) Write([]byte) (int, error) { return 0, ErrUnsupported }
