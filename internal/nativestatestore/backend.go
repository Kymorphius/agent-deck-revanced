// Package nativestatestore adapts the original Agent Deck StateDB to native terminals.
// matrix/hardened: the shared backend descriptor lives in the existing tool_data extras zone.
package nativestatestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
)

const (
	DatabaseFilename = nativeregistry.SharedStateDatabaseFilename
	BackendExtraKey  = "native_backend"
	BackendVersion   = 1
	BackendKind      = "conpty"
	DefaultGroup     = "my-sessions"
	maxBackendBytes  = 72 * 1024
)

// Backend contains only immutable explicit launch overrides and edit times.
// Runtime PID/RunID, inherited environments and terminal output are not stored.
type Backend struct {
	Version   int                     `json:"version"`
	Kind      string                  `json:"kind"`
	Launch    terminalhost.LaunchSpec `json:"launch"`
	CreatedAt int64                   `json:"created_at"`
	UpdatedAt int64                   `json:"updated_at"`
}

func invalid(message string) error {
	return fmt.Errorf("%w: %s", nativeregistry.ErrInvalidData, message)
}

func validateBackend(b Backend) error {
	if b.Version != BackendVersion || b.Kind != BackendKind {
		return invalid("unsupported native backend version or kind")
	}
	if b.CreatedAt <= 0 || b.UpdatedAt < b.CreatedAt {
		return invalid("invalid native backend timestamps")
	}
	if err := terminalhost.ValidateLaunchSpec(b.Launch); err != nil {
		return invalid("invalid native launch: " + err.Error())
	}
	return nil
}

// MarshalBackend returns the value for tool_data.native_backend, not a full
// tool_data blob. It never copies the current process environment.
func MarshalBackend(b Backend) (json.RawMessage, error) {
	if err := validateBackend(b); err != nil {
		return nil, err
	}
	launch, err := json.Marshal(b.Launch)
	if err != nil {
		return nil, err
	}
	if len(launch) > 64*1024 {
		return nil, invalid("oversized native launch description")
	}
	data, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBackendBytes {
		return nil, invalid("oversized native backend descriptor")
	}
	return data, nil
}

// DecodeBackend returns nil for a legacy row without the extra. A declared
// malformed, null, future or unknown native backend is an error, never tmux.
func DecodeBackend(toolData json.RawMessage) (*Backend, error) {
	if len(toolData) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if !utf8.Valid(toolData) || json.Unmarshal(toolData, &fields) != nil || fields == nil {
		return nil, invalid("tool_data must be a JSON object")
	}
	raw, declared := fields[BackendExtraKey]
	if !declared {
		return nil, nil
	}
	if len(raw) > maxBackendBytes || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, invalid("invalid native backend descriptor")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var backend Backend
	if err := decoder.Decode(&backend); err != nil {
		return nil, invalid("native backend descriptor: " + err.Error())
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid("trailing native backend data")
	}
	if _, err := MarshalBackend(backend); err != nil {
		return nil, err
	}
	return &backend, nil
}

// RecordFromRow supplies the same codec to the original session hydration path.
// Native records have no tmux identity; legacy records retain their old IDs.
func RecordFromRow(row *statedb.InstanceRow) (nativeregistry.Record, bool, error) {
	if row == nil {
		return nativeregistry.Record{}, false, nil
	}
	backend, err := DecodeBackend(row.ToolData)
	if err != nil || backend == nil {
		return nativeregistry.Record{}, false, err
	}
	if row.TmuxSession != "" || row.TmuxSocketName != "" {
		return nativeregistry.Record{}, true, invalid("native row also declares a tmux identity")
	}
	if row.ProjectPath != backend.Launch.Dir || row.CreatedAt.Unix() != time.Unix(0, backend.CreatedAt).Unix() {
		return nativeregistry.Record{}, true, invalid("native row conflicts with launch directory or creation time")
	}
	if err := nativeregistry.ValidateID(row.ID); err != nil {
		return nativeregistry.Record{}, true, err
	}
	if err := nativeregistry.ValidateName(row.Title); err != nil {
		return nativeregistry.Record{}, true, err
	}
	if err := validateGroup(row.GroupPath); err != nil {
		return nativeregistry.Record{}, true, err
	}
	if row.Tool != "shell" && row.Tool != "codex" && row.Tool != "powershell" && row.Tool != "custom" {
		return nativeregistry.Record{}, true, invalid("unsupported native tool")
	}
	return nativeregistry.Record{ID: row.ID, Name: row.Title, Tool: row.Tool, Group: row.GroupPath, Launch: backend.Launch, CreatedAt: time.Unix(0, backend.CreatedAt).UTC(), UpdatedAt: time.Unix(0, backend.UpdatedAt).UTC()}, true, nil
}

func validateGroup(group string) error {
	if err := nativeregistry.ValidateGroup(group); err != nil {
		return err
	}
	if group == "" || group != strings.TrimSpace(group) {
		return invalid("shared native group requires a canonical nonempty path")
	}
	for _, part := range strings.Split(group, "/") {
		if part == "" || part == "." || part == ".." || part != strings.TrimSpace(part) {
			return invalid("invalid shared native group path")
		}
	}
	return nil
}

func canonicalGroup(group string) (string, error) {
	if group == "" {
		group = DefaultGroup
	} else {
		group = strings.TrimSpace(group)
	}
	return group, validateGroup(group)
}
