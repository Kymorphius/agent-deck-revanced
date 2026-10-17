package tmux

import (
	"reflect"
	"regexp"
	"slices"
	"sync"
)

// matrix/hardened: this cache contains pure text transformations, never a
// status, acknowledgment, spinner grace, or completed-turn observation.
// One prepared and one normalized input are retained per Session. Preparation
// of a different frame also drops normalization, releasing the old frame.
type matrixFrameTextCache struct {
	mu sync.Mutex

	preparedValid bool
	raw, prepared string
	key           matrixPreparedFrameKey
	work          BackgroundWork
	blocked       bool

	normalizedValid    bool
	input, normalized  string
	thinking, ellipsis matrixRegexKey
	name, socket       string

	// Private counters let regression tests verify avoided work rather than
	// asserting only that two identical calls return identical strings.
	preparedBuilds, normalizedBuilds uint64
}

type matrixRegexKey struct {
	present  bool
	text     string
	compiled *regexp.Regexp
}

func matrixRegexIdentity(re *regexp.Regexp) matrixRegexKey {
	if re == nil {
		return matrixRegexKey{}
	}
	return matrixRegexKey{true, re.String(), re}
}

// Copy freezes public mutable matching options such as Longest. Comparing
// the opaque values also distinguishes Compile and CompilePOSIX with the
// same expression, without assumptions about named standard-library fields.
// Only cache misses copy; regular regexp matching does not mutate the value.
func (k matrixRegexKey) snapshot() matrixRegexKey {
	if k.compiled != nil {
		k.compiled = k.compiled.Copy()
	}
	return k
}

func (k matrixRegexKey) same(other matrixRegexKey) bool {
	return k.present == other.present && k.text == other.text && reflect.DeepEqual(k.compiled, other.compiled)
}

// The startup flags affect prepareFrame's error/background precedence.
// Copies of custom slices protect against sequential in-place configuration
// edits; pointer equality alone would miss them.
type matrixPreparedFrameKey struct {
	name, socket, tool, command, customTool string
	startupZero, startupTimedOut            bool
	busy, prompt, detect                    []string
	thinking, ellipsis                      matrixRegexKey
}

func (s *Session) matrixThinkingKeys() (matrixRegexKey, matrixRegexKey) {
	thinking, ellipsis := thinkingPattern, thinkingPatternEllipsis
	if p := s.resolvedPatterns; p != nil {
		if p.ThinkingPattern != nil {
			thinking = p.ThinkingPattern
		}
		if p.ThinkingPatternEllipsis != nil {
			ellipsis = p.ThinkingPatternEllipsis
		}
	}
	return matrixRegexIdentity(thinking), matrixRegexIdentity(ellipsis)
}

func (k matrixPreparedFrameKey) matches(s *Session, tool string, thinking, ellipsis matrixRegexKey) bool {
	return k.name == s.Name && k.socket == s.SocketName && k.tool == tool && k.command == s.Command &&
		k.customTool == s.customToolName && k.startupZero == s.startupAt.IsZero() && k.startupTimedOut == s.startupTimedOut &&
		slices.Equal(k.busy, s.customBusyPatterns) && slices.Equal(k.prompt, s.customPromptPatterns) &&
		slices.Equal(k.detect, s.customDetectPatterns) && k.thinking.same(thinking) && k.ellipsis.same(ellipsis)
}

// Caller holds s.mu, as for the original prepareFrame. No IO or acquisition
// of s.mu/cacheMu occurs while text.mu is held. Capture failures bypass reuse.
func (s *Session) preparedFrameLocked(raw string, captureErr error) string {
	if captureErr != nil {
		s.clearFrameTextCache()
		return s.prepareFrame(StripANSI(raw))
	}
	tool := inferToolFromSessionFields(s.detectedTool, s.customToolName, s.Command)
	thinking, ellipsis := s.matrixThinkingKeys()
	c := &s.frameTextCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.preparedValid && c.raw == raw && c.key.matches(s, tool, thinking, ellipsis) {
		s.lastBackgroundWork, s.lastBackgroundBlocked = c.work, c.blocked
		return c.prepared
	}
	prepared := s.prepareFrame(StripANSI(raw))
	c.raw, c.prepared, c.preparedValid = raw, prepared, true
	c.key = matrixPreparedFrameKey{name: s.Name, socket: s.SocketName, tool: tool, command: s.Command,
		customTool: s.customToolName, startupZero: s.startupAt.IsZero(), startupTimedOut: s.startupTimedOut,
		busy: slices.Clone(s.customBusyPatterns), prompt: slices.Clone(s.customPromptPatterns), detect: slices.Clone(s.customDetectPatterns),
		thinking: thinking.snapshot(), ellipsis: ellipsis.snapshot()}
	c.work, c.blocked = s.lastBackgroundWork, s.lastBackgroundBlocked
	c.preparedBuilds++
	c.normalizedValid, c.input, c.normalized = false, "", ""
	return prepared
}

// normalizeContent retains the original byte-for-byte transformation as the
// uncached oracle. Its only configurable inputs are the two thinking regexps;
// their actual expressions/default selection, not pointer identities, key it.
// Production callers hold s.mu, as when reading resolvedPatterns previously.
func (s *Session) normalizeContent(content string) string {
	thinking, ellipsis := s.matrixThinkingKeys()
	c := &s.frameTextCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.normalizedValid && c.input == content && c.thinking.same(thinking) && c.ellipsis.same(ellipsis) && c.name == s.Name && c.socket == s.SocketName {
		return c.normalized
	}
	c.input, c.normalized = content, s.normalizeContentUncached(content)
	c.thinking, c.ellipsis, c.name, c.socket = thinking.snapshot(), ellipsis.snapshot(), s.Name, s.SocketName
	c.normalizedValid = true
	c.normalizedBuilds++
	return c.normalized
}

// Safe both with and without s.mu. Never reacquires instance/capture locks.
func (s *Session) clearFrameTextCache() {
	c := &s.frameTextCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.preparedValid, c.normalizedValid = false, false
	c.raw, c.prepared, c.input, c.normalized = "", "", "", ""
	c.key = matrixPreparedFrameKey{}
	c.work, c.blocked = BackgroundWork{}, false
	c.thinking, c.ellipsis, c.name, c.socket = matrixRegexKey{}, matrixRegexKey{}, "", ""
	// Counters remain lifetime totals across invalidation.
}
