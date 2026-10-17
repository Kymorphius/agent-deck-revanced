package reader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// matrix/hardened: compare the cost-specific path to the complete conversation
// reader, so its independently exercised semantics remain the authority.
func matrixReferenceCodexUsage(t *testing.T, path, nativeID string) []Usage {
	t.Helper()
	sink := newRecorder()
	if _, err := (Codex{}).Ingest(context.Background(), SourceRef{Path: path, NativeID: nativeID}, 0, sink, nil); err != nil {
		t.Fatal(err)
	}
	return sink.usage
}

const matrixCostSemantics = `{"type":"session_meta","payload":{"id":"metadata-must-not-change-cost-id"}}
{"type":"turn_context","payload":{"model":"model-a"}}
{"timestamp":"2026-10-12T10:00:00Z","type":"token_usage_record","payload":{"response_id":"response-a","turn_id":"ignored","usage":{"input_tokens":130,"cached_input_tokens":40,"cache_write_input_tokens":10,"output_tokens":7,"reasoning_output_tokens":6}}}
{"type":"turn_context","payload":{"model":""}}
{"type":"token_usage_record","payload":{"turn_id":"fallback-turn","usage":{"input_tokens":30,"cached_input_tokens":30,"output_tokens":0}}}
{"type":"response_item","type":"token_usage_record","timestamp":"invalid-time","payload":{"response_id":"duplicate-last-usage","usage":{"input_tokens":5,"output_tokens":1}}}
{"type":"token_usage_record","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"not usage"}],"usage":{"input_tokens":999}}}
{"type":"token_usage_record","payload":{"response_id":"partial-payload","usage":{"input_tokens":100,"cached_input_tokens":"invalid","output_tokens":2}}}
{"type":"turn_context","payload":{"model":42}}
{"type":"token_usage_record","payload":{"response_id":"bad-json","usage":{"input_tokens":1}} broken}
{"type":"token_usage_record","payload":{"response_id":"zero","usage":{"cached_input_tokens":30,"output_tokens":0}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":999999,"output_tokens":999}}}}
{"type":"compacted","payload":{"message":"model and usage in text are not cost records","replacement_history":[{"type":"token_usage_record","payload":{"usage":{"input_tokens":999999}}}]}}
{"payload":{"model":"model-b"}, "type" : "turn_context"}
{"\u0074ype":"token_usage_record","timestamp":"2026-10-12T10:01:00Z","payload":{"turn_id":"last-turn","usage":{"input_tokens":10,"cached_input_tokens":15,"cache_write_input_tokens":2,"output_tokens":3,"reasoning_output_tokens":3}}}
`

func matrixLargeCodexResponse(t testing.TB) string {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type": "message", "role": "assistant",
			"content": []map[string]string{{"type": "output_text", "text": strings.Repeat(`body mentions "type":"token_usage_record" and model context. `, 16384)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(line) + "\n"
}

func TestMatrixCodexCostUsageMatchesConversationReader(t *testing.T) {
	for _, tc := range []struct{ name, data string }{
		{"native-corpus", codexShapes},
		{"cost-semantics", matrixCostSemantics},
		{"large-body", matrixLargeCodexResponse(t) + matrixCostSemantics},
		{"overlong-body", `{"type":"response_item","payload":{"text":"` + strings.Repeat("x", MaxLineBytes) + `"}}` + "\n" + matrixCostSemantics},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			want := matrixReferenceCodexUsage(t, path, "source-thread")
			got, err := ScanCodexUsage(context.Background(), path, "source-thread")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("cost usage differs from conversation reader: got=%+v want=%+v err=%v", got, want, err)
			}
			if tc.name == "cost-semantics" {
				if len(got) != 5 || got[0].UUID != "codex:response-a" || got[0].In != 80 || got[0].Out != 7 || got[0].CacheR != 40 || got[0].CacheW != 10 || got[1].UUID != "codex:source-thread@fallback-turn@" || got[1].Model != "model-a" || got[1].In != 0 || got[2].UUID != "codex:duplicate-last-usage" || !got[2].TS.IsZero() || got[3].UUID != "codex:partial-payload" || got[4].Model != "model-b" || got[4].In != 0 || got[4].Out != 3 {
					t.Fatalf("fixture did not exercise intended native cost semantics: %+v", got)
				}
			}
		})
	}
}

func TestMatrixCodexCostUsageTailBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	contextLine := `{"type":"turn_context","payload":{"model":"first-model"}}` + "\n"
	firstUsage := `{"type":"token_usage_record","payload":{"response_id":"first","usage":{"input_tokens":100,"output_tokens":5}}}` + "\n"
	if err := os.WriteFile(path, []byte(contextLine+firstUsage), 0600); err != nil {
		t.Fatal(err)
	}
	var tail CodexUsageTail
	got, err := tail.Read(context.Background(), path, "thread")
	if err != nil || !reflect.DeepEqual(got, matrixReferenceCodexUsage(t, path, "thread")) {
		t.Fatalf("initial read: %+v %v", got, err)
	}
	initialOffset := tail.offset
	got, err = tail.Read(context.Background(), path, "thread")
	if err != nil || len(got) != 0 || tail.offset != initialOffset {
		t.Fatalf("unchanged read: %+v offset=%d err=%v", got, tail.offset, err)
	}
	appendText := func(text string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.WriteString(text)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("append: %v %v", writeErr, closeErr)
		}
	}
	secondContext := `{"type":"turn_context","payload":{"model":"second-model"}}` + "\n"
	partialUsage := `{"type":"token_usage_record","payload":{"turn_id":"second","usage":{"input_tokens":20,"cached_input_tokens":20}}}`
	appendText(secondContext + partialUsage)
	got, err = tail.Read(context.Background(), path, "thread")
	if err != nil || len(got) != 0 || tail.offset != initialOffset+int64(len(secondContext)) {
		t.Fatalf("torn tail: %+v offset=%d err=%v", got, tail.offset, err)
	}
	appendText("\n")
	got, err = tail.Read(context.Background(), path, "thread")
	want := matrixReferenceCodexUsage(t, path, "thread")[1:]
	if err != nil || !reflect.DeepEqual(got, want) || len(got) != 1 || got[0].Model != "second-model" {
		t.Fatalf("completed tail: got=%+v want=%+v err=%v", got, want, err)
	}
	// Truncation discards old model state and replays all new history.
	if err := os.WriteFile(path, []byte(firstUsage), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = tail.Read(context.Background(), path, "thread")
	want = matrixReferenceCodexUsage(t, path, "thread")
	if err != nil || !reflect.DeepEqual(got, want) || got[0].Model != "" {
		t.Fatalf("truncated source: got=%+v want=%+v err=%v", got, want, err)
	}
	// A cancelled read consumes nothing, and retry still reads full history.
	var cancelledTail CodexUsageTail
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cancelledTail.Read(ctx, path, "thread"); !errors.Is(err, context.Canceled) || cancelledTail.offset != 0 {
		t.Fatalf("cancelled read advanced cursor: offset=%d err=%v", cancelledTail.offset, err)
	}
	got, err = cancelledTail.Read(context.Background(), path, "thread")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("retry after cancellation: got=%+v want=%+v err=%v", got, want, err)
	}
}

func BenchmarkMatrixCodexCostUsageBody(b *testing.B) {
	line := []byte(strings.TrimSuffix(matrixLargeCodexResponse(b), "\n"))
	for _, mode := range []string{"conversation", "cost-only"} {
		b.Run(mode, func(b *testing.B) {
			var sink usageSink
			state := codexState{emitter: newEmitter(SourceRef{NativeID: "thread"}, &sink)}
			b.SetBytes(int64(len(line)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if mode == "conversation" {
					state.line(line, 0, int64(len(line)))
				} else {
					state.usageLine(line, 0, int64(len(line)))
				}
			}
		})
	}
}
