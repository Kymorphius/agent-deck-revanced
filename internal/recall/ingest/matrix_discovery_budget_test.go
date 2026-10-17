package ingest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/recall/query"
	"github.com/asheshgoplani/agent-deck/internal/recall/reader"
	"github.com/asheshgoplani/agent-deck/internal/recall/testcorpus"
)

// matrix/hardened: model a large source tree whose metadata walk alone
// exceeds the interactive parse deadline, with deterministic delay.
type matrixSlowDiscover struct{ reader.Reader }

func (r matrixSlowDiscover) Discover(ctx context.Context, roots []reader.Root, emit func(reader.SourceRef) error) error {
	timer := time.NewTimer(InteractiveDeadline + 20*time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return r.Reader.Discover(ctx, roots, emit)
	}
}

func TestMatrixInteractiveParseBudgetStartsAfterDiscovery(t *testing.T) {
	f := newFixture(t, testcorpus.Options{Files: 1, Seed: 69})
	f.sweep(Options{})
	before := f.count(`SELECT count(*) FROM msg`)
	appendTurns(t, f.stats.Paths[0], f.stats.Sessions[0], 1)
	readers := []reader.Reader{matrixSlowDiscover{Reader: reader.Claude{}}}
	// The unchanged batch semantics demonstrate the old interactive
	// starvation: discovery expires the deadline, leaving the cursor still.
	stalled := f.sweep(Options{Readers: readers, Budget: reader.NewBudget(InteractiveDeadline, InteractiveBytes)})
	if stalled.BytesRead != 0 || stalled.Deferred != 1 || f.count(`SELECT count(*) FROM msg`) != before {
		t.Fatalf("expected discovery to consume the original budget: %+v", stalled)
	}
	// One record per pass forces a real partial-source resume. Both new
	// records must land despite discovery exceeding 150 ms on every pass.
	for pass := int64(1); pass <= 2; pass++ {
		res := f.sweep(Options{
			Readers:                   readers,
			Budget:                    reader.NewBudget(InteractiveDeadline, InteractiveBytes),
			ParseBudgetAfterDiscovery: InteractiveDeadline,
			PerSourceBytes:            1,
		})
		if res.BytesRead == 0 || res.Errors != 0 {
			t.Fatalf("pass %d did not advance: %+v", pass, res)
		}
		if got := f.count(`SELECT count(*) FROM msg`); got != before+pass {
			t.Fatalf("pass %d messages = %d; want %d without duplicate prefixes", pass, got, before+pass)
		}
	}
	if got := f.count(`SELECT count(*) FROM source WHERE parsed_to=size`); got != 1 {
		t.Fatal("resumed parsing did not reach the new transcript tail")
	}
	hits, err := query.New(f.st, "").Search(context.Background(), query.SearchOptions{Query: "zebra"})
	if err != nil || len(hits.Hits) != 1 {
		t.Fatalf("new transcript content is not searchable: %+v, %v", hits, err)
	}
}

func TestMatrixDeferredParseBudgetStillCancelsDiscovery(t *testing.T) {
	f := newFixture(t, testcorpus.Options{Files: 1, Seed: 70})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	res, err := New(f.st, Options{
		Roots:                     f.roots(),
		Readers:                   []reader.Reader{matrixSlowDiscover{Reader: reader.Claude{}}},
		Budget:                    reader.NewBudget(InteractiveDeadline, InteractiveBytes),
		ParseBudgetAfterDiscovery: InteractiveDeadline,
	}).Sweep(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || res.BytesRead != 0 {
		t.Fatalf("discovery cancellation: %+v, %v", res, err)
	}
}
