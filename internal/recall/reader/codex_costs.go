package reader

import (
	"context"
	"encoding/json"
	"os"
)

// matrix/hardened: retain model context while tailing complete JSONL records.
// The authoritative token_usage_record is used; cumulative token_count UI
// snapshots are deliberately ignored so they cannot double-count requests.
type CodexUsageTail struct {
	state  *codexState
	sink   usageSink
	offset int64
}

func (t *CodexUsageTail) Read(ctx context.Context, path, nativeID string) ([]Usage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if t.state == nil || t.state.src.Path != path || info.Size() < t.offset {
		t.offset = 0
		t.state = &codexState{emitter: newEmitter(SourceRef{Path: path, NativeID: nativeID}, &t.sink)}
	}
	t.sink.out = nil
	off, err := t.state.scan(ctx, t.offset, info.Size(), nil, t.state.usageLine)
	if err != nil {
		return nil, err
	}
	t.offset = off
	return t.sink.out, nil
}

// matrix/hardened: costs need model context and authoritative usage only.
// A standard JSON decode preserves validation and last-value duplicate-key
// semantics without copying unrelated payloads into a RawMessage. Relevant
// records still use the conversation reader's exact model/UUID/token logic.
func (st *codexState) usageLine(line []byte, off, n int64) {
	var header struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &header) != nil {
		return
	}
	switch header.Type {
	case "turn_context", "token_usage_record":
		st.line(line, off, n)
	}
}

func ScanCodexUsage(ctx context.Context, path, nativeID string) ([]Usage, error) {
	var tail CodexUsageTail
	return tail.Read(ctx, path, nativeID)
}
