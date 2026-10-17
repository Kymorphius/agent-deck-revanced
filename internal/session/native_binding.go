package session

// matrix/hardened: native descriptions use the original session row and
// tool_data extras. Loading metadata never creates a manager or a terminal.

import (
	"encoding/json"
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/nativestatestore"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func nativeBackendFromRow(row *statedb.InstanceRow) (json.RawMessage, error) {
	_, native, err := nativestatestore.RecordFromRow(row)
	if err != nil || !native {
		return nil, err
	}
	var extras map[string]json.RawMessage
	if err := json.Unmarshal(row.ToolData, &extras); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), extras[nativestatestore.BackendExtraKey]...), nil
}

func writeNativeBackendData(toolData, descriptor json.RawMessage) (json.RawMessage, error) {
	if len(descriptor) == 0 {
		return toolData, nil
	}
	var extras map[string]json.RawMessage
	if err := json.Unmarshal(toolData, &extras); err != nil {
		return nil, err
	}
	if extras == nil {
		extras = make(map[string]json.RawMessage)
	}
	extras[nativestatestore.BackendExtraKey] = append(json.RawMessage(nil), descriptor...)
	result, err := json.Marshal(extras)
	if err != nil {
		return nil, err
	}
	if _, err := nativestatestore.DecodeBackend(result); err != nil {
		return nil, err
	}
	return result, nil
}

func validateNativeInstanceData(data *InstanceData) error {
	if len(data.NativeBackend) == 0 {
		return nil
	}
	toolData, err := writeNativeBackendData(json.RawMessage(`{}`), data.NativeBackend)
	if err != nil {
		return err
	}
	_, native, err := nativestatestore.RecordFromRow(&statedb.InstanceRow{
		ID: data.ID, Title: data.Title, ProjectPath: data.ProjectPath,
		GroupPath: data.GroupPath, Tool: data.Tool, CreatedAt: data.CreatedAt,
		TmuxSession: data.TmuxSession, TmuxSocketName: data.TmuxSocketName,
		ToolData: toolData,
	})
	if err != nil {
		return err
	}
	if !native {
		return fmt.Errorf("explicit native backend was not recognized")
	}
	return nil
}
