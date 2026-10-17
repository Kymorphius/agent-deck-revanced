// matrix/hardened: native Windows IPC has a fixed local namespace and explicit path validation.
package winipc

import (
	"errors"
	"fmt"
	"strings"
)

const PipePrefix = `\\.\pipe\agent-deck-native-`

var (
	ErrUnsupported      = errors.New("native named-pipe IPC requires Windows")
	ErrInvalidPipePath  = errors.New("invalid native IPC pipe path")
	ErrInvalidStatePath = errors.New("invalid native IPC state path")
)

func validatePipePath(path string) error {
	if !strings.HasPrefix(path, PipePrefix) {
		return fmt.Errorf("%w: expected local agent-deck namespace", ErrInvalidPipePath)
	}
	suffix := strings.TrimPrefix(path, PipePrefix)
	if len(suffix) == 0 || len(suffix) > 128 {
		return fmt.Errorf("%w: suffix must contain 1-128 ASCII characters", ErrInvalidPipePath)
	}
	for _, c := range suffix {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return fmt.Errorf("%w: suffix must contain letters, digits, hyphens or underscores", ErrInvalidPipePath)
	}
	return nil
}

// validateStatePath accepts an explicit drive-absolute path, excluding device
// aliases, drive roots, alternate data streams and paths Windows normalizes.
func validateStatePath(path string) error {
	path = strings.ReplaceAll(path, "/", `\`)
	if len(path) < 4 || !(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z') || path[1:3] != `:\` {
		return fmt.Errorf("%w: require a local drive-absolute path below the drive root", ErrInvalidStatePath)
	}
	for _, component := range strings.Split(path[3:], `\`) {
		if component == "" || component == "." || component == ".." || strings.HasSuffix(component, " ") || strings.HasSuffix(component, ".") {
			return fmt.Errorf("%w: ambiguous path component", ErrInvalidStatePath)
		}
		for _, c := range component {
			if c < 32 || strings.ContainsRune(`:<>"|?*`, c) {
				return fmt.Errorf("%w: unsupported path character", ErrInvalidStatePath)
			}
		}
		base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return fmt.Errorf("%w: reserved DOS device name", ErrInvalidStatePath)
		}
	}
	return nil
}
