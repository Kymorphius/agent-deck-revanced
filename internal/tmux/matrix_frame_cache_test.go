package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

func matrixFrameCacheCounts(s *Session) (uint64, uint64) {
	s.frameTextCache.mu.Lock()
	defer s.frameTextCache.mu.Unlock()
	return s.frameTextCache.preparedBuilds, s.frameTextCache.normalizedBuilds
}

func matrixFrameCapture(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	frame := filepath.Join(dir, "frame")
	if err := os.WriteFile(frame, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
for arg in "$@"; do
 case "$arg" in
 has-session) exit 0 ;;
 list-panes) printf '0\n'; exit 0 ;;
 capture-pane) cat "$MATRIX_FRAME_CAPTURE"; exit 0 ;;
 esac
done
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MATRIX_FRAME_CAPTURE", frame)
}

// matrix/hardened: exercise independent consumers of one cached capture;
// observe builds, not merely identical outputs. Classification still creates
// fresh completion observations on repeated successful reads of identical bytes.
func TestMatrixFrameCacheSharesPreparationAcrossReadPaths(t *testing.T) {
	matrixFrameCapture(t, realisticClaudeDoneContent)
	s := &Session{Name: "matrix-frame-cache-paths", Command: "claude", detectedTool: "claude", SocketName: DefaultSocketName()}
	if !s.startupShowsAgentAlive() {
		t.Fatal("fixture should expose the ready composer")
	}
	if got, _ := s.GetSubstateWithLiveSpinner(); got != SubstateIdleAtEmptyPrompt {
		t.Fatalf("substate = %q", got)
	}
	_, firstSample := s.CachedCompletedTurnSample()
	if firstSample.IsZero() {
		t.Fatal("classification did not record a completion observation")
	}
	if work, blocked, busy := s.BackgroundWorkSince(time.Time{}); work.InFlight() || blocked || busy {
		t.Fatalf("completed frame misclassified as background: %+v %v %v", work, blocked, busy)
	}
	// Force a new successful raw capture with the same bytes, without an
	// action/invalidation. Only text may be reused; observation time advances.
	s.cacheMu.Lock()
	s.cacheTime = time.Time{}
	s.cacheMu.Unlock()
	if _, busy := s.GetSubstateWithLiveSpinner(); busy {
		t.Fatal("completed frame acquired a live spinner")
	}
	_, secondSample := s.CachedCompletedTurnSample()
	if !secondSample.After(firstSample) {
		t.Fatal("same-byte fresh capture lost its new completion observation")
	}
	if prepared, _ := matrixFrameCacheCounts(s); prepared != 1 {
		t.Fatalf("one frame prepared %d times across read paths, want 1", prepared)
	}
}

func TestMatrixFrameCachePreparationMatchesOracleAndRestoresBackground(t *testing.T) {
	for _, fixture := range []string{"workflow-running.txt", "workflow-finished.txt", "agent-and-shells.txt", "monitor-footer-busy-turn.txt", "all-finished-stale-waiting-line.txt"} {
		matrixCheckPreparedOracle(t, loadBackgroundFixture(t, fixture))
	}
	matrixCheckPreparedOracle(t, "\x1b[32m"+issueScreenshotFrame+"\x1b[0m")
	matrixCheckPreparedOracle(t, "Select an option:\n1. Continue\n2. Cancel\n"+issueScreenshotFrame)
	for _, frame := range []string{realisticClaudeContent, realisticClaudeDoneContent, "Please run /login\n❯\n", "Session startup timed out before the agent became interactive\n"} {
		matrixCheckPreparedOracle(t, frame)
	}
}

func matrixCheckPreparedOracle(t *testing.T, raw string) {
	t.Helper()
	ref := &Session{Command: "claude", detectedTool: "claude"}
	want := ref.prepareFrame(StripANSI(raw))
	s := &Session{Command: "claude", detectedTool: "claude"}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < 3; i++ {
		// Another reporting path may have changed these fields. A hit must
		// restore the complete prepared-frame context, not only its string.
		s.lastBackgroundWork = BackgroundWork{Kind: BackgroundKindWorkflow, Task: "unrelated-frame"}
		s.lastBackgroundBlocked = !ref.lastBackgroundBlocked
		got := s.preparedFrameLocked(raw, nil)
		if got != want || s.lastBackgroundWork != ref.lastBackgroundWork || s.lastBackgroundBlocked != ref.lastBackgroundBlocked {
			t.Fatalf("prepared frame differs from uncached oracle: work=%+v want=%+v blocked=%v want=%v", s.lastBackgroundWork, ref.lastBackgroundWork, s.lastBackgroundBlocked, ref.lastBackgroundBlocked)
		}
	}
	if prepared, _ := matrixFrameCacheCounts(s); prepared != 1 {
		t.Fatalf("identical frame built %d times", prepared)
	}
}

func TestMatrixFrameCacheInvalidatesContentToolConfigurationAndStartup(t *testing.T) {
	s := &Session{Name: "one", SocketName: "first-socket", Command: "claude", detectedTool: "claude"}
	busy := []string{"busy-one"}
	s.SetCustomPatterns("claude", busy, []string{"prompt"}, []string{"detect"})
	raw := issueScreenshotFrame
	prepare := func(frame string) {
		s.mu.Lock()
		s.preparedFrameLocked(frame, nil)
		s.mu.Unlock()
	}
	prepare(raw)
	prepare(raw)
	if n, _ := matrixFrameCacheCounts(s); n != 1 {
		t.Fatalf("initial cache = %d", n)
	}
	// The setter retains the caller's slice. An in-place config edit must be
	// detected even when all pointers and raw terminal bytes stay the same.
	busy[0] = "busy-two"
	prepare(raw)
	s.SetPatterns(&ResolvedPatterns{ThinkingPattern: regexp.MustCompile(`(Thinking)\.\.\.`)})
	prepare(raw)
	s.resolvedPatterns.ThinkingPattern = regexp.MustCompile(`(Working)\.\.\.`)
	prepare(raw)
	s.SetCustomPatterns("codex", nil, nil, nil)
	s.mu.Lock()
	s.detectedTool = "codex"
	s.mu.Unlock()
	prepare(raw)
	s.Name = "two"
	prepare(raw)
	s.SocketName = "second-socket"
	prepare(raw)
	prepare(raw + "\nchanged")
	if n, _ := matrixFrameCacheCounts(s); n != 8 {
		t.Fatalf("content/tool/config/rebind changes built %d times, want 8", n)
	}
	// Startup state changes error/background precedence on identical text.
	s.SetCustomPatterns("claude", nil, nil, nil)
	s.mu.Lock()
	s.detectedTool = "claude"
	s.startupAt = time.Now()
	timeoutFrame := "Session startup timed out before the agent became interactive\n" + issueScreenshotFrame
	s.preparedFrameLocked(timeoutFrame, nil)
	if s.lastBackgroundBlocked {
		t.Fatal("preserved timeout text blocked a new startup")
	}
	s.startupAt = time.Time{}
	s.preparedFrameLocked(timeoutFrame, nil)
	if !s.lastBackgroundBlocked {
		t.Fatal("ended startup reused the prior unblocked verdict")
	}
	s.startupAt = time.Now()
	s.startupTimedOut = true
	s.preparedFrameLocked(timeoutFrame, nil)
	if !s.lastBackgroundBlocked {
		t.Fatal("timed-out startup reused an unblocked verdict")
	}
	s.mu.Unlock()
}

func TestMatrixFrameCacheNormalizationMatchesOracleAndPatternEdits(t *testing.T) {
	s := &Session{Command: "claude", detectedTool: "claude"}
	frames := []string{realisticClaudeContent, realisticClaudeDoneContent, issueScreenshotFrame, "\x1b[31m⠋ Thinking... (45s · ↑ 1234 tokens)\x1b[0m\n\n\n download 1.2MB/5.6MB 45% [====>  ] 12:34:56 \t\n"}
	for _, frame := range frames {
		want := s.normalizeContentUncached(frame)
		s.mu.Lock()
		for i := 0; i < 5; i++ {
			if got := s.normalizeContent(frame); got != want {
				t.Fatalf("normalization differs from original transformation: %q want %q", got, want)
			}
		}
		s.mu.Unlock()
	}
	if _, n := matrixFrameCacheCounts(s); n != uint64(len(frames)) {
		t.Fatalf("normalization built %d times for %d repeated frames", n, len(frames))
	}
	p := &ResolvedPatterns{ThinkingPattern: regexp.MustCompile(`(Foobar) [0-9]+`), ThinkingPatternEllipsis: regexp.MustCompile(`Foobar…`)}
	s.SetPatterns(p)
	content := "Foobar 123\nFoobar…\nBaz 456"
	got := s.normalizeContent(content)
	if want := s.normalizeContentUncached(content); got != want {
		t.Fatalf("custom normalization = %q want %q", got, want)
	}
	p.ThinkingPattern = regexp.MustCompile(`(Baz) [0-9]+`)
	if changed := s.normalizeContent(content); changed == got || changed != s.normalizeContentUncached(content) {
		t.Fatalf("in-place thinking regex edit reused old result: %q -> %q", got, changed)
	}
	p.ThinkingPatternEllipsis = nil
	if changed := s.normalizeContent(content); changed != s.normalizeContentUncached(content) {
		t.Fatal("nil/default thinking branch reused prior custom result")
	}
}

func TestMatrixFrameCacheNormalizationDistinguishesRegexpMatchingOptions(t *testing.T) {
	s := &Session{}
	p := &ResolvedPatterns{ThinkingPattern: regexp.MustCompile(`(a|aa)`)}
	s.SetPatterns(p)
	const content = "aa"
	first := s.normalizeContent(content)
	if first != "a...a..." {
		t.Fatalf("leftmost-first fixture = %q", first)
	}
	// Same String(), different matching mode, replaced in the retained config.
	p.ThinkingPattern = regexp.MustCompilePOSIX(`(a|aa)`)
	if got := s.normalizeContent(content); got != "aa..." || got != s.normalizeContentUncached(content) {
		t.Fatalf("same-expression POSIX replacement reused old result: %q", got)
	}
	p.ThinkingPattern = regexp.MustCompile(`(a|aa)`)
	if got := s.normalizeContent(content); got != first {
		t.Fatalf("restored leftmost-first = %q", got)
	}
	// Public Longest mutates the same regexp object without changing String().
	p.ThinkingPattern.Longest()
	if got := s.normalizeContent(content); got != "aa..." || got != s.normalizeContentUncached(content) {
		t.Fatalf("direct Longest edit reused old result: %q", got)
	}
	p.ThinkingPattern = regexp.MustCompile(`(a|aa)`)
	s.SetPatterns(p)
	if got := s.normalizeContent(content); got != first {
		t.Fatalf("setter reset = %q", got)
	}
	p.ThinkingPattern.Longest()
	s.SetPatterns(p)
	if got := s.normalizeContent(content); got != "aa..." {
		t.Fatalf("Longest plus setter = %q", got)
	}
	if _, n := matrixFrameCacheCounts(s); n != 6 {
		t.Fatalf("each matching-mode change must rebuild: %d builds", n)
	}
}

func TestMatrixFrameCacheFailuresAndInvalidationCannotReuseBusyEvidence(t *testing.T) {
	marker := issue2502CaptureFailure(t)
	s := &Session{Name: "matrix-frame-cache-failure", Command: "claude", detectedTool: "claude", SocketName: DefaultSocketName()}
	s.mu.Lock()
	s.preparedFrameLocked(realisticClaudeContent, nil)
	s.classifyFrameLocked(s.frameTextCache.prepared)
	s.mu.Unlock()
	if sub, busy := s.GetSubstateWithLiveSpinner(); sub != SubstateNone || busy {
		t.Fatalf("failed capture reused cached live frame: %q busy=%v", sub, busy)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("expected a real failed capture attempt", err)
	}
	s.frameTextCache.mu.Lock()
	valid := s.frameTextCache.preparedValid
	s.frameTextCache.mu.Unlock()
	if valid {
		t.Fatal("failed capture left cached preparation usable")
	}
	s.mu.Lock()
	s.preparedFrameLocked(realisticClaudeContent, nil)
	s.preparedFrameLocked("", errors.New("capture failed"))
	s.mu.Unlock()
	s.invalidateCache()
	s.frameTextCache.mu.Lock()
	defer s.frameTextCache.mu.Unlock()
	if s.frameTextCache.preparedValid || s.frameTextCache.normalizedValid || s.frameTextCache.raw != "" {
		t.Fatal("action invalidation retained old frame")
	}
}

func TestMatrixFrameCacheConcurrentInvalidationLockOrder(t *testing.T) {
	s := &Session{Command: "claude", detectedTool: "claude"}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			s.mu.Lock()
			prepared := s.preparedFrameLocked(realisticClaudeContent, nil)
			s.normalizeContent(prepared)
			s.classifyFrameLocked(prepared)
			s.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			s.invalidateCache()
			s.SetPatterns(&ResolvedPatterns{})
		}
	}()
	wg.Wait()
}

func BenchmarkMatrixFrameTextReuse(b *testing.B) {
	for _, mode := range []string{"uncached", "cached"} {
		b.Run(mode, func(b *testing.B) {
			s := &Session{Command: "claude", detectedTool: "claude"}
			raw := "\x1b[32m" + issueScreenshotFrame + "\x1b[0m"
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s.mu.Lock()
				if mode == "uncached" {
					frame := s.prepareFrame(StripANSI(raw))
					s.normalizeContentUncached(frame)
				} else {
					frame := s.preparedFrameLocked(raw, nil)
					s.normalizeContent(frame)
				}
				s.mu.Unlock()
			}
		})
	}
}
