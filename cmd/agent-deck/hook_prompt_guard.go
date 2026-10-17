package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// matrix/hardened: message admission is a separate, explicit host opt-in.
// Resolve the real user home; HOME and account config cannot redirect it.
var hookPromptGuardHome = func() (string, error) {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" || !filepath.IsAbs(u.HomeDir) {
		return "", fmt.Errorf("invalid host home")
	}
	return u.HomeDir, nil
}

type hookPromptGuardState struct {
	home    string
	enabled bool
	err     error
}

// matrix/hardened: a damaged opt-in blocks only Claude prompt submissions.
// Other events retain their normal status/context handling.
func readHookPromptGuardState() hookPromptGuardState {
	home, err := hookPromptGuardHome()
	if err != nil {
		// Host opt-in cannot be established without its real home. Portable
		// systems must not activate protection based on a lookup error alone.
		return hookPromptGuardState{err: err}
	}
	marker := filepath.Join(home, ".local", "lib", "claude-launch-guard", "messages-enabled.json")
	info, err := os.Lstat(marker)
	if os.IsNotExist(err) {
		return hookPromptGuardState{home: home}
	}
	state := hookPromptGuardState{home: home, enabled: true}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		state.err = fmt.Errorf("unsafe message opt-in marker")
		return state
	}
	f, err := os.Open(marker)
	if err != nil {
		state.err = err
		return state
	}
	_ = f.Close()
	return state
}

// matrix/hardened: exact Claude event only. Codex's field presence (even null)
// and Cursor's conversation ID must never count as a Claude message.
func isClaudePromptGuardEvent(p hookPayload) bool {
	return p.HookEventName == "UserPromptSubmit" && p.TurnID == nil && p.ConversationID == ""
}

var hookPromptGuardSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type hookPromptGuardReceipt struct {
	Allowed       *bool   `json:"allowed"`
	Reason        string  `json:"reason"`
	PeriodSeconds float64 `json:"period_seconds"`
	RetryAfter    float64 `json:"retry_after"`
}

// matrix/hardened: helper stdout stays private and bounded; never mix its JSON
// with the hook's context output or include submitted text in diagnostics.
type hookPromptGuardOutput struct {
	data     []byte
	exceeded bool
}

func (o *hookPromptGuardOutput) Write(p []byte) (int, error) {
	if len(o.data)+len(p) > 16*1024 {
		o.exceeded = true
		return 0, fmt.Errorf("message guard response too large")
	}
	o.data = append(o.data, p...)
	return len(p), nil
}

// matrix/hardened: no prompt, digest, cwd, transcript, or account environment
// reaches the guard. The UUID is the only hook payload field sent to it.
func checkHookPromptGuard(state hookPromptGuardState, sessionID string) string {
	const failed = "Claude 本机消息保护检查失败，已阻止本条消息；请检查 claude-launch-guard 配置后重试。"
	if state.err != nil || !hookPromptGuardSessionID.MatchString(sessionID) {
		return failed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(state.home, ".local", "bin", "claude-launch-guard"), "prompt-check", "--session-id", sessionID, "--json")
	cmd.Dir = state.home
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	output := &hookPromptGuardOutput{}
	cmd.Stdout = output
	err := cmd.Run()
	var receipt hookPromptGuardReceipt
	if output.exceeded || json.Unmarshal(output.data, &receipt) != nil || receipt.Allowed == nil {
		return failed
	}
	if *receipt.Allowed {
		if err != nil {
			return failed
		}
		return ""
	}
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 75 || receipt.RetryAfter < 0 || receipt.PeriodSeconds < 0 {
		return failed
	}
	switch receipt.Reason {
	case "periodic_messages":
		if receipt.PeriodSeconds <= 0 {
			return failed
		}
		return fmt.Sprintf("Claude 本机消息保护：检测到约每 %.0f 秒重复提交消息，已阻止本条消息；请停止周期任务，%.0f 秒后再试。", receipt.PeriodSeconds, receipt.RetryAfter)
	case "periodic_message_cooldown", "cooldown":
		return fmt.Sprintf("Claude 本机消息保护：该会话仍处于周期消息保护冷却期，已阻止本条消息；%.0f 秒后再试。", receipt.RetryAfter)
	default:
		return failed
	}
}

// matrix/hardened: stderr is Claude's visible blocking reason; stdout must
// remain empty so rejection cannot consume or inject inbox/context output.
func rejectHookPromptGuard(state hookPromptGuardState, p hookPayload) bool {
	if !state.enabled || !isClaudePromptGuardEvent(p) {
		return false
	}
	if reason := checkHookPromptGuard(state, p.SessionID); reason != "" {
		fmt.Fprintln(os.Stderr, reason)
		return true
	}
	return false
}

// matrix/hardened: keep the legacy payload cap while scanning all guard
// metadata. Large prompt strings are validated and discarded, never buffered.
type hookPromptPrefix struct{ data []byte }

func (p *hookPromptPrefix) Write(b []byte) (int, error) {
	remaining := maxHookPayloadSize + 1 - len(p.data)
	if remaining > 0 {
		p.data = append(p.data, b[:min(len(b), remaining)]...)
	}
	return len(b), nil
}

type hookPromptMetadataScanner struct{ r *bufio.Reader }

func (s *hookPromptMetadataScanner) next() (byte, error) {
	for {
		c, err := s.r.ReadByte()
		if err != nil || (c != ' ' && c != '\n' && c != '\r' && c != '\t') {
			return c, err
		}
	}
}

// A bounded raw string can be decoded with the same escaping semantics as
// hookPayload; unknown strings use limit zero and allocate no content buffer.
func (s *hookPromptMetadataScanner) stringValue(limit int) ([]byte, bool, error) {
	var raw []byte
	overflow := false
	appendByte := func(c byte) {
		if limit > 0 {
			if len(raw) < limit {
				raw = append(raw, c)
			} else {
				overflow = true
			}
		}
	}
	appendByte('"')
	for {
		c, err := s.r.ReadByte()
		if err != nil {
			return nil, overflow, err
		}
		appendByte(c)
		if c == '"' {
			return raw, overflow, nil
		}
		if c < 0x20 {
			return nil, overflow, fmt.Errorf("control byte in JSON string")
		}
		if c != '\\' {
			continue
		}
		escape, err := s.r.ReadByte()
		if err != nil {
			return nil, overflow, err
		}
		appendByte(escape)
		if strings.ContainsRune(`"\/bfnrt`, rune(escape)) {
			continue
		}
		if escape != 'u' {
			return nil, overflow, fmt.Errorf("invalid JSON string escape")
		}
		for range 4 {
			h, err := s.r.ReadByte()
			if err != nil {
				return nil, overflow, err
			}
			appendByte(h)
			if !((h >= '0' && h <= '9') || (h >= 'a' && h <= 'f') || (h >= 'A' && h <= 'F')) {
				return nil, overflow, fmt.Errorf("invalid JSON unicode escape")
			}
		}
	}
}

func (s *hookPromptMetadataScanner) literal(rest string) error {
	for n := range len(rest) {
		c, err := s.r.ReadByte()
		if err != nil || c != rest[n] {
			return fmt.Errorf("invalid JSON literal")
		}
	}
	return nil
}

func (s *hookPromptMetadataScanner) peek() byte {
	b, err := s.r.Peek(1)
	if err != nil {
		return 0
	}
	return b[0]
}

func (s *hookPromptMetadataScanner) digits() int {
	n := 0
	for c := s.peek(); c >= '0' && c <= '9'; c = s.peek() {
		_, _ = s.r.ReadByte()
		n++
	}
	return n
}

func (s *hookPromptMetadataScanner) number(first byte) error {
	if first == '-' {
		var err error
		first, err = s.r.ReadByte()
		if err != nil {
			return err
		}
	}
	if first >= '1' && first <= '9' {
		s.digits()
	} else if first != '0' {
		return fmt.Errorf("invalid JSON number")
	}
	if s.peek() == '.' {
		_, _ = s.r.ReadByte()
		if s.digits() == 0 {
			return fmt.Errorf("invalid JSON fraction")
		}
	}
	if c := s.peek(); c == 'e' || c == 'E' {
		_, _ = s.r.ReadByte()
		if c := s.peek(); c == '+' || c == '-' {
			_, _ = s.r.ReadByte()
		}
		if s.digits() == 0 {
			return fmt.Errorf("invalid JSON exponent")
		}
	}
	return nil
}

func (s *hookPromptMetadataScanner) skip(first byte, depth int) error {
	if depth > 512 {
		return fmt.Errorf("JSON nesting exceeds metadata scan limit")
	}
	switch first {
	case '"':
		_, _, err := s.stringValue(0)
		return err
	case 't':
		return s.literal("rue")
	case 'f':
		return s.literal("alse")
	case 'n':
		return s.literal("ull")
	case '{', '[':
		close := byte(']')
		if first == '{' {
			close = '}'
		}
		c, err := s.next()
		if err != nil || c == close {
			return err
		}
		for {
			if first == '{' {
				if c != '"' {
					return fmt.Errorf("invalid JSON object key")
				}
				if _, _, err := s.stringValue(0); err != nil {
					return err
				}
				if c, err = s.next(); err != nil || c != ':' {
					return fmt.Errorf("invalid JSON object separator")
				}
				if c, err = s.next(); err != nil {
					return err
				}
			}
			if err := s.skip(c, depth+1); err != nil {
				return err
			}
			if c, err = s.next(); err != nil || c == close {
				return err
			}
			if c != ',' {
				return fmt.Errorf("invalid JSON collection separator")
			}
			if c, err = s.next(); err != nil {
				return err
			}
		}
	default:
		return s.number(first)
	}
}

// matrix/hardened: only small top-level guard fields survive the scan. String
// duplicates use last-value semantics; null preserves the prior Go string.
func readHookPromptMetadata(input io.Reader) (payload hookPayload, data []byte, scanErr error) {
	prefix := &hookPromptPrefix{}
	s := hookPromptMetadataScanner{r: bufio.NewReaderSize(io.TeeReader(input, prefix), 32*1024)}
	defer func() { data = prefix.data }()
	c, err := s.next()
	if err != nil || c != '{' {
		return payload, nil, fmt.Errorf("invalid hook JSON object")
	}
	c, err = s.next()
	if err != nil {
		return payload, nil, err
	}
	for c != '}' {
		if c != '"' {
			return payload, nil, fmt.Errorf("invalid hook JSON key")
		}
		rawKey, overflow, err := s.stringValue(1024)
		if err != nil {
			return payload, nil, err
		}
		key := ""
		if !overflow {
			_ = json.Unmarshal(rawKey, &key)
		}
		if c, err = s.next(); err != nil || c != ':' {
			return payload, nil, fmt.Errorf("invalid hook JSON separator")
		}
		if c, err = s.next(); err != nil {
			return payload, nil, err
		}
		var field *string
		switch {
		case strings.EqualFold(key, "hook_event_name"):
			field = &payload.HookEventName
		case strings.EqualFold(key, "session_id"):
			field = &payload.SessionID
		case strings.EqualFold(key, "conversation_id"):
			field = &payload.ConversationID
		case strings.EqualFold(key, "turn_id"):
			payload.TurnID = json.RawMessage("null")
		}
		if field != nil && c == '"' {
			raw, tooLong, err := s.stringValue(1024)
			if err != nil {
				return payload, nil, err
			}
			if tooLong {
				*field = "" // no oversized metadata is forwarded to the helper.
				if field == &payload.ConversationID {
					*field = "oversized-conversation" // retain other-harness evidence.
				}
				scanErr = fmt.Errorf("hook metadata string exceeds limit")
			} else if err := json.Unmarshal(raw, field); err != nil {
				return payload, nil, err
			}
		} else {
			if err := s.skip(c, 1); err != nil {
				return payload, nil, err
			}
			if field != nil && c != 'n' {
				scanErr = fmt.Errorf("invalid hook metadata string type")
			}
		}
		if c, err = s.next(); err != nil {
			return payload, nil, err
		}
		if c == '}' {
			break
		}
		if c != ',' {
			return payload, nil, fmt.Errorf("invalid hook JSON field separator")
		}
		if c, err = s.next(); err != nil || c == '}' {
			return payload, nil, fmt.Errorf("invalid hook JSON field")
		}
	}
	if _, err := s.next(); err != io.EOF {
		return payload, nil, fmt.Errorf("trailing hook JSON data")
	}
	return payload, nil, scanErr
}
