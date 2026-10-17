//go:build !darwin && !linux

package aiupdate

// matrix/hardened: unsupported native lock semantics must fail closed before mutation.
import (
	"context"
	"errors"
	"os"
)

func acquireNativeLocks(context.Context, string, string) (func(), error) {
	return nil, errors.New("deferred Codex daemon installation currently supports only macOS and Linux")
}

func openReadNoFollow(path string) (*os.File, error) { return os.Open(path) }
