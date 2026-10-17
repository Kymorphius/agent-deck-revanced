package session

import "encoding/json"

// matrix/hardened: `launch --env-file` path in the tool_data extras zone.
// The launching process saves the instance and the first spawn may happen in
// another process that reloads it, so the path (never the file's content)
// has to survive the round-trip. The owner deletes the file when its task
// ends; a later restart then finds nothing to source.
const toolDataEnvFileKey = "env_file"

// WriteEnvFileToToolData stores (or with "" removes) the env file path.
func WriteEnvFileToToolData(td json.RawMessage, path string) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(td) > 0 {
		_ = json.Unmarshal(td, &m)
	}
	if path != "" {
		raw, _ := json.Marshal(path)
		m[toolDataEnvFileKey] = raw
	} else {
		delete(m, toolDataEnvFileKey)
	}
	out, _ := json.Marshal(m)
	return out
}

// ReadEnvFileFromToolData returns the stored env file path, or "".
func ReadEnvFileFromToolData(td json.RawMessage) string {
	if len(td) == 0 {
		return ""
	}
	var blob struct {
		EnvFile string `json:"env_file"`
	}
	_ = json.Unmarshal(td, &blob)
	return blob.EnvFile
}
