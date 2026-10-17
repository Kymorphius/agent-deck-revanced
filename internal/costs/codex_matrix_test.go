package costs_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/costs"
	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
)

// matrix/hardened: use native records plus their cumulative UI snapshots.
const codexCostsFixture = `{"type":"turn_context","payload":{"model":"gpt-6.1-sol"}}
{"timestamp":"2026-10-10T10:00:00Z","type":"token_usage_record","payload":{"response_id":"resp_one","usage":{"input_tokens":100000,"cached_input_tokens":80000,"output_tokens":1000,"reasoning_output_tokens":900}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100000,"output_tokens":1000}}}}
`

func TestMatrixCodexCostsTailSyncAndRecallDeduplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(codexCostsFixture), 0600); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	pricer := costs.NewPricer(costs.PricerConfig{})
	sources := []costs.SyncSession{{InstanceID: "deck", CodexPath: path, CodexSessionID: "thread", Tool: "codex"}}
	r := costs.SyncFromTranscripts(store, pricer, sources)
	if r.EventsImported != 1 || len(r.Errors) != 0 {
		t.Fatalf("sync: %+v", r)
	}
	sum, err := store.TotalBySession("deck")
	if err != nil || sum.TotalInputTokens != 20000 || sum.TotalCacheReadTokens != 80000 || sum.TotalOutputTokens != 1000 || sum.TotalCostMicrodollars != 58000 {
		t.Fatalf("sum: %+v %v", sum, err)
	}
	// A replayed response linked to a different deck session remains one cost.
	sources[0].InstanceID = "fork"
	r = costs.SyncFromTranscripts(store, pricer, sources)
	if r.EventsImported != 0 || r.EventsSkipped != 1 {
		t.Fatalf("replay: %+v", r)
	}
	var tail reader.CodexUsageTail
	events, err := tail.Read(context.Background(), path, "thread")
	if err != nil || len(events) != 1 {
		t.Fatalf("tail: %v %v", events, err)
	}
	i, s, e := costs.NewUsageImporter(store, pricer).Import("deck", events)
	if i != 0 || s != 1 || len(e) != 0 {
		t.Fatalf("recall dedup: %d %d %v", i, s, e)
	}
	// No new bytes produces no events. A partial JSONL line is retried.
	events, err = tail.Read(context.Background(), path, "thread")
	if err != nil || len(events) != 0 {
		t.Fatalf("unchanged: %v %v", events, err)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(`{"type":"turn_context","payload":{"model":"gpt-6-astra"}}` + "\n" + `{"timestamp":"2026-10-10T10:01:00Z","type":"token_usage_record","payload":{"response_id":"resp_two","usage":{"input_tokens":100,"cached_input_tokens":100,"output_tokens":0}}}`)
	_ = f.Close()
	events, err = tail.Read(context.Background(), path, "thread")
	if err != nil || len(events) != 0 {
		t.Fatalf("partial: %v %v", events, err)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("\n")
	_ = f.Close()
	events, err = tail.Read(context.Background(), path, "thread")
	if err != nil || len(events) != 1 || events[0].Model != "gpt-6-astra" || events[0].In != 0 {
		t.Fatalf("appended: %v %v", events, err)
	}
	i, _, e = costs.NewUsageImporter(store, pricer).Import("deck", events)
	if i != 1 || len(e) != 0 {
		t.Fatalf("cache-only: %d %v", i, e)
	}
}

func TestMatrixCodexLongContextPricing(t *testing.T) {
	p := costs.NewPricer(costs.PricerConfig{})
	if got := p.ComputeCost("gpt-6.1-sol", 20000, 1000, 252000, 0); got != 75200 {
		t.Fatalf("threshold: %d", got)
	}
	if got := p.ComputeCost("gpt-6.1-sol", 20001, 1000, 252000, 0); got != 145404 {
		t.Fatalf("long: %d", got)
	}
	p = costs.NewPricer(costs.PricerConfig{Overrides: map[string]costs.PriceOverride{"gpt-6.1-sol": {InputPerMtok: 2, OutputPerMtok: 10, CacheReadPerMtok: .1}}})
	if got := p.ComputeCost("gpt-6.1-sol", 20001, 1000, 252000, 0); got != 75202 {
		t.Fatalf("override: %d", got)
	}
}
