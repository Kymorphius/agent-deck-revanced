package web

import (
	"context"
	"encoding/json"
	"github.com/asheshgoplani/agent-deck/internal/costs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// matrix/hardened: a read reports the loaded mode, keeps historical data, and
// cannot trigger the separately injected transcript synchronization callback.
func TestMatrixCostsSummaryStoredDataAndActualMode(t *testing.T) {
	store := newTestCostStore(t)
	if err := store.WriteCostEvent(costs.CostEvent{ID: "historical", SessionID: "preserved", Timestamp: time.Now(), CostMicrodollars: 42000}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	srv.SetCostStore(store)
	calls := 0
	srv.SetCostSync(func(context.Context) (*costs.SyncResult, error) { calls++; return &costs.SyncResult{}, nil })
	for _, mode := range []bool{false, true, false} {
		srv.SetCostAutoCollect(mode)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, newLocalRequest(http.MethodGet, "/api/costs/summary", nil))
		var got struct {
			Auto   bool `json:"auto_collect"`
			Events int  `json:"today_events"`
		}
		if rr.Code != 200 {
			t.Fatalf("summary: %d %s", rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Auto != mode || got.Events != 1 || calls != 0 {
			t.Fatalf("summary triggered/replaced history: %+v calls=%d", got, calls)
		}
	}
}
