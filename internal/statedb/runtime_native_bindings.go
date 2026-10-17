package statedb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// matrix/hardened: a read-only monitor may discover native identities, but may
// only persist their narrow binding fields under the original registry fence.
var runtimeNativeBindingKeys = map[string]string{
	"claude_session_id": "string", "claude_detected_at": "integer", "claude_session_id_unverified": "bool",
	"codex_session_id": "string", "codex_detected_at": "integer",
	"gemini_session_id": "string", "gemini_detected_at": "integer",
	"opencode_session_id": "string", "opencode_detected_at": "integer",
	"copilot_session_id": "string", "copilot_detected_at": "integer",
	"pi_session_id": "string", "pi_session_path": "string",
	"generic_session_id": "string", "generic_detected_at": "integer", "generic_session_tool": "string", "generic_session_command": "string", "generic_session_location": "string",
}

func (s *StateDB) WriteRuntimeNativeBindings(id string, bindings map[string]json.RawMessage, expectedTouch int64) error {
	if len(bindings) == 0 {
		return nil
	}
	keys := make([]string, 0, len(bindings))
	for key, raw := range bindings {
		kind, ok := runtimeNativeBindingKeys[key]
		if !ok {
			return fmt.Errorf("unsupported runtime native binding %q", key)
		}
		if !json.Valid(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("invalid runtime native binding %q", key)
		}
		switch kind {
		case "string":
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("runtime native binding %q must be string", key)
			}
		case "integer":
			var value int64
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("runtime native binding %q must be integer", key)
			}
		case "bool":
			var value bool
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("runtime native binding %q must be boolean", key)
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return withBusyRetry(func() error {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := guardRuntimeObservation(tx, id, []int64{expectedTouch}); err != nil {
			return err
		}
		pairs := make([]string, 0, len(keys))
		args := make([]any, 0, 2*len(keys)+1)
		for _, key := range keys {
			pairs = append(pairs, "?, json(?)")
			args = append(args, "$."+key, string(bindings[key]))
		}
		args = append(args, id)
		if _, err := tx.Exec(`UPDATE instances SET tool_data=json_set(COALESCE(tool_data,'{}'),`+strings.Join(pairs, ",")+`) WHERE id=?`, args...); err != nil {
			return err
		}
		for _, harness := range []string{"claude", "codex", "gemini", "opencode", "copilot", "pi", "generic"} {
			raw, changed := bindings[harness+"_session_id"]
			if !changed {
				continue
			}
			var native string
			_ = json.Unmarshal(raw, &native)
			if native == "" {
				continue
			}
			linkHarness := harness
			if harness == "generic" {
				_ = tx.QueryRow(`SELECT COALESCE(json_extract(tool_data,'$.generic_session_tool'),'generic') FROM instances WHERE id=?`, id).Scan(&linkHarness)
				if linkHarness == "" {
					linkHarness = "generic"
				}
			}
			authoritative := true
			if harness == "claude" {
				var unverified bool
				_ = tx.QueryRow(`SELECT COALESCE(json_extract(tool_data,'$.claude_session_id_unverified'),0) FROM instances WHERE id=?`, id).Scan(&unverified)
				authoritative = !unverified
			}
			if err := upsertSessionLink(tx, id, linkHarness, native, "", authoritative, time.Now()); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}
