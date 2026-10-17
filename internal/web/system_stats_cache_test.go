package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/sysinfo"
)

// matrix/hardened: all collection and timing evidence is deterministic; these
// tests never spawn the host's ps, sysctl, vm_stat or netstat collectors.
func TestMatrixSystemStatsCacheTTLAndRollback(t *testing.T) {
	clock := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	start := clock
	calls := 0
	cache := systemStatsCache{
		now: func() time.Time { return clock },
		collect: func() sysinfo.Stats {
			calls++
			return sysinfo.Stats{CPU: sysinfo.CPUStat{Available: true, UsagePercent: float64(calls)}}
		},
	}

	first := cache.load()
	if calls != 1 || first.CPU.UsagePercent != 1 {
		t.Fatalf("initial sample = %+v, calls = %d", first, calls)
	}
	first.CPU.UsagePercent = -1 // A returned value must not mutate the cached one.
	clock = start.Add(systemStatsCacheTTL - time.Nanosecond)
	if got := cache.load(); calls != 1 || got.CPU.UsagePercent != 1 {
		t.Fatalf("fresh sample = %+v, calls = %d", got, calls)
	}
	clock = start.Add(systemStatsCacheTTL)
	if got := cache.load(); calls != 2 || got.CPU.UsagePercent != 2 {
		t.Fatalf("boundary sample = %+v, calls = %d", got, calls)
	}
	clock = start
	if got := cache.load(); calls != 3 || got.CPU.UsagePercent != 3 {
		t.Fatalf("rollback sample = %+v, calls = %d", got, calls)
	}
}

func TestMatrixSystemStatsCacheTTLStartsAfterCollection(t *testing.T) {
	clock := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	cache := systemStatsCache{
		now: func() time.Time { return clock },
		collect: func() sysinfo.Stats {
			calls++
			clock = clock.Add(3 * time.Second)
			return sysinfo.Stats{}
		},
	}
	cache.load()
	completed := clock
	clock = completed.Add(systemStatsCacheTTL - time.Nanosecond)
	cache.load()
	if calls != 1 {
		t.Fatalf("sample expired before its completion-based TTL: calls = %d", calls)
	}
	clock = completed.Add(systemStatsCacheTTL)
	cache.load()
	if calls != 2 {
		t.Fatalf("sample did not expire at completion-based boundary: calls = %d", calls)
	}
}

func TestMatrixSystemStatsCacheConcurrentRequests(t *testing.T) {
	clock := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	s := &Server{systemStats: systemStatsCache{
		now: func() time.Time { return clock },
		collect: func() sysinfo.Stats {
			call := calls.Add(1)
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return sysinfo.Stats{CPU: sysinfo.CPUStat{Available: true, UsagePercent: float64(call)}}
		},
	}}

	runWave := func(wantCalls int32) {
		t.Helper()
		const count = 32
		var ready, done sync.WaitGroup
		ready.Add(count)
		done.Add(count)
		results := make(chan *httptest.ResponseRecorder, count)
		for i := 0; i < count; i++ {
			go func() {
				defer done.Done()
				ready.Done()
				w := httptest.NewRecorder()
				s.handleSystemStats(w, httptest.NewRequest(http.MethodGet, "/api/system/stats", nil))
				results <- w
			}()
		}
		ready.Wait()
		<-started
		close(release)
		done.Wait()
		close(results)
		if got := calls.Load(); got != wantCalls {
			t.Fatalf("concurrent wave collected %d times, want %d", got, wantCalls)
		}
		for w := range results {
			var body struct {
				CPU struct {
					Usage float64 `json:"usage_percent"`
				} `json:"cpu"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || body.CPU.Usage != float64(wantCalls) {
				t.Fatalf("wave result = %d %s", w.Code, w.Body.String())
			}
		}
	}

	runWave(1)
	clock = clock.Add(systemStatsCacheTTL)
	release = make(chan struct{})
	runWave(2)
}

func TestMatrixSystemStatsCacheServerIsolation(t *testing.T) {
	for _, sameProfile := range []bool{false, true} {
		name := "different profiles"
		if sameProfile {
			name = "same profile with different collectors"
		}
		t.Run(name, func(t *testing.T) {
			clock := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			profiles := []string{"first", "second"}
			if sameProfile {
				profiles[1] = profiles[0]
			}
			var calls [2]int
			servers := make([]*Server, 2)
			for i := range servers {
				idx := i
				servers[i] = &Server{cfg: Config{Profile: profiles[i]}, systemStats: systemStatsCache{
					now: func() time.Time { return clock },
					collect: func() sysinfo.Stats {
						calls[idx]++
						return sysinfo.Stats{CPU: sysinfo.CPUStat{Available: true, UsagePercent: float64(idx + 1)}}
					},
				}}
			}
			for round := 0; round < 2; round++ {
				for i, s := range servers {
					if got := s.systemStats.load(); got.CPU.UsagePercent != float64(i+1) {
						t.Fatalf("server %d returned another collector's sample: %+v", i, got)
					}
				}
			}
			if calls != [2]int{1, 1} {
				t.Fatalf("server collection counts = %v, want [1 1]", calls)
			}
		})
	}
}

func TestMatrixSystemStatsCacheAuthAndMethodBeforeCollect(t *testing.T) {
	calls := 0
	s := &Server{cfg: Config{Token: "secret"}, systemStats: systemStatsCache{
		collect: func() sysinfo.Stats {
			calls++
			return sysinfo.Stats{}
		},
	}}
	for _, test := range []struct {
		method string
		status int
	}{
		{http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodGet, http.StatusUnauthorized},
	} {
		w := httptest.NewRecorder()
		s.handleSystemStats(w, httptest.NewRequest(test.method, "/api/system/stats", nil))
		if w.Code != test.status {
			t.Fatalf("%s status = %d, want %d", test.method, w.Code, test.status)
		}
	}
	if calls != 0 {
		t.Fatalf("rejected requests triggered collection %d times", calls)
	}
}

func TestMatrixSystemStatsCacheUnavailableMetricsStayOmitted(t *testing.T) {
	calls := 0
	clock := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	s := &Server{systemStats: systemStatsCache{
		now: func() time.Time { return clock },
		collect: func() sysinfo.Stats {
			calls++
			if calls > 1 {
				return sysinfo.Stats{}
			}
			return sysinfo.Stats{
				CPU:    sysinfo.CPUStat{Available: true, UsagePercent: 0},
				Memory: sysinfo.MemStat{Available: true, UsedBytes: 512, TotalBytes: 1024, UsagePercent: 50},
				GPU:    sysinfo.GPUStat{Available: false, UsagePercent: 99, Name: "unavailable"},
			}
		},
	}}
	for sample := 0; sample < 3; sample++ {
		if sample == 2 {
			clock = clock.Add(systemStatsCacheTTL)
		}
		w := httptest.NewRecorder()
		s.handleSystemStats(w, httptest.NewRequest(http.MethodGet, "/api/system/stats", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if sample < 2 {
			if len(body) != 2 || body["cpu"] == nil || body["memory"] == nil {
				t.Fatalf("partial sample was changed by caching: %s", w.Body.String())
			}
		} else if len(body) != 0 {
			t.Fatalf("unavailable sample reused stale metrics: %s", w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("collection calls = %d, want 2", calls)
	}
}
