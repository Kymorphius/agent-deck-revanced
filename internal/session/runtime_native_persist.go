package session

import (
	"encoding/json"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// matrix/hardened: pane/process discoveries are written before publishing the
// identity. Consumers then reload the durable binding and apply the same row.
func (i *Instance) persistRuntimeNativeBindings() error {
	fence := i.runtimeProducerFence.Load()
	base := i.runtimeProducerExpected.Load()
	if fence == nil || base == nil {
		return nil
	}
	i.mu.RLock()
	values := map[string]any{}
	bind := func(harness, id string, at time.Time) {
		if id == "" {
			return
		}
		values[harness+"_session_id"] = id
		if !at.IsZero() {
			values[harness+"_detected_at"] = at.Unix()
		}
	}
	bind("claude", i.ClaudeSessionID, i.ClaudeDetectedAt)
	bind("codex", i.CodexSessionID, i.CodexDetectedAt)
	bind("gemini", i.GeminiSessionID, i.GeminiDetectedAt)
	bind("opencode", i.OpenCodeSessionID, i.OpenCodeDetectedAt)
	bind("copilot", i.CopilotSessionID, i.CopilotDetectedAt)
	if i.PiSessionID != "" {
		values["pi_session_id"] = i.PiSessionID
		values["pi_session_path"] = i.PiSessionPath
	}
	if i.GenericSessionID != "" {
		values["generic_session_id"] = i.GenericSessionID
		if !i.GenericDetectedAt.IsZero() {
			values["generic_detected_at"] = i.GenericDetectedAt.Unix()
		}
		values["generic_session_tool"] = i.GenericSessionTool
		values["generic_session_command"] = i.GenericSessionCommand
		values["generic_session_location"] = i.GenericSessionLocation
	}
	claudeID := i.ClaudeSessionID
	i.mu.RUnlock()
	if claudeID != "" {
		i.claudeSessionIDsFromDiskScanMu.Lock()
		values["claude_session_id_unverified"] = i.claudeSessionIDsFromDiskScan[claudeID]
		i.claudeSessionIDsFromDiskScanMu.Unlock()
	}
	var original map[string]json.RawMessage
	_ = json.Unmarshal(base.ToolData, &original)
	bindings := map[string]json.RawMessage{}
	// Detection refresh times alone do not create a write every polling pass.
	for _, harness := range []string{"claude", "codex", "gemini", "opencode", "copilot", "pi", "generic"} {
		key := harness + "_session_id"
		value, exists := values[key]
		if !exists {
			continue
		}
		var prior string
		_ = json.Unmarshal(original[key], &prior)
		changed := value.(string) != prior
		if harness == "claude" {
			var old bool
			_ = json.Unmarshal(original[toolDataClaudeSessionUnverifiedKey], &old)
			changed = changed || old != values[toolDataClaudeSessionUnverifiedKey]
		}
		if harness == "generic" {
			for _, field := range []string{"generic_session_tool", "generic_session_command", "generic_session_location"} {
				var old string
				_ = json.Unmarshal(original[field], &old)
				changed = changed || old != values[field]
			}
		}
		if harness == "pi" {
			var old string
			_ = json.Unmarshal(original["pi_session_path"], &old)
			changed = changed || old != values["pi_session_path"]
		}
		if !changed {
			continue
		}
		for field, value := range values {
			if field == key || field == harness+"_detected_at" || (harness == "claude" && field == toolDataClaudeSessionUnverifiedKey) || (harness == "pi" && field == "pi_session_path") || (harness == "generic" && (field == "generic_session_tool" || field == "generic_session_command" || field == "generic_session_location")) {
				raw, _ := json.Marshal(value)
				bindings[field] = raw
			}
		}
	}
	if len(bindings) == 0 {
		return nil
	}
	db := i.stateDB()
	if db == nil {
		return statedb.ErrObservationStale
	}
	return db.WriteRuntimeNativeBindings(i.ID, bindings, *fence)
}
