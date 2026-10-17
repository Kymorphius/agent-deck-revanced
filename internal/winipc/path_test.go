// matrix/hardened: validate namespace and state path boundaries without touching the host.
package winipc

import (
	"errors"
	"strings"
	"testing"
)

func TestValidatePipePath(t *testing.T) {
	for _, suffix := range []string{"a", "user_123-session-ABC", strings.Repeat("a", 128)} {
		if err := validatePipePath(PipePrefix + suffix); err != nil {
			t.Fatalf("valid suffix %q: %v", suffix, err)
		}
	}
	for _, path := range []string{
		"", PipePrefix, PipePrefix + strings.Repeat("a", 129),
		`\\server\pipe\agent-deck-native-session`, `\\localhost\pipe\agent-deck-native-session`,
		`\\.\pipe\other-session`, PipePrefix + `x\y`, PipePrefix + "../session",
		PipePrefix + "session\x00", PipePrefix + "session name", PipePrefix + "会话",
		strings.ToUpper(PipePrefix) + "session",
	} {
		if err := validatePipePath(path); !errors.Is(err, ErrInvalidPipePath) {
			t.Fatalf("unsafe path %q: got %v", path, err)
		}
	}
}

func TestValidateStatePath(t *testing.T) {
	for _, path := range []string{`C:\state`, `c:/Users/Test User/state/data.json`, `D:\用户\状态`} {
		if err := validateStatePath(path); err != nil {
			t.Fatalf("valid path %q: %v", path, err)
		}
	}
	for _, path := range []string{
		"", ".", "state", `C:state`, `C:\`, `C:/`, `\\server\share\state`, `\\?\C:\state`,
		`\\.\pipe\state`, `C:\state\..\other`, `C:\state\.\data`, `C:\state\\data`,
		`C:\state\`, `C:\state.`, `C:\state `, `C:\state\data:stream`,
		`C:\state\NUL`, `C:\state\con.txt`, `C:\state\COM1.log`, `C:\state\LPT9`,
		"C:\\state\\bad\x00", `C:\state\*.json`,
	} {
		if err := validateStatePath(path); !errors.Is(err, ErrInvalidStatePath) {
			t.Fatalf("unsafe path %q: got %v", path, err)
		}
	}
}
