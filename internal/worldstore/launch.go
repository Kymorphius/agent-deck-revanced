package worldstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// LaunchRequest describes a session started from a task.
type LaunchRequest struct {
	// Exe is the agent-deck binary to run (normally os.Executable()).
	Exe     string
	Profile string
	Path    string
	Group   string
	Tool    string
	Title   string
	Prompt  string
	TaskID  string
	// NoParent stops `launch` from parenting the new session under the
	// calling agent-deck session (the TUI is not a session).
	NoParent bool
}

// LaunchArgs is the `agent-deck launch` command line for req. The prompt goes
// on stdin (--message-file -) so no shell or argv quoting can mangle it.
func LaunchArgs(req LaunchRequest) []string {
	var args []string
	if req.Profile != "" {
		args = append(args, "-p", req.Profile)
	}
	args = append(args, "launch")
	tool := req.Tool
	if tool == "" {
		tool = "claude"
	}
	args = append(args, "-c", tool, "--json")
	if req.Title != "" {
		args = append(args, "-t", req.Title)
	}
	if req.Group != "" {
		args = append(args, "-g", req.Group)
	}
	if req.TaskID != "" {
		args = append(args, "--hint", "worldstore_task="+req.TaskID)
	}
	if req.NoParent {
		args = append(args, "-no-parent")
	}
	if req.Prompt != "" {
		args = append(args, "--message-file", "-")
	}
	// Flags must precede the positional path: Go's flag parsing stops there.
	if req.Path != "" {
		args = append(args, req.Path)
	}
	return args
}

// Launch runs `agent-deck launch` and returns the new session's ID.
func Launch(ctx context.Context, req LaunchRequest) (string, error) {
	if req.Exe == "" {
		return "", fmt.Errorf("launch: agent-deck binary unknown")
	}
	cmd := exec.CommandContext(ctx, req.Exe, LaunchArgs(req)...)
	if req.Prompt != "" {
		cmd.Stdin = strings.NewReader(req.Prompt)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var resp struct {
		Success   bool   `json:"success"`
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		Error     any    `json:"error"`
		Message   string `json:"message"`
	}
	// launch prints one JSON object; tolerate log lines before it.
	out := stdout.Bytes()
	if i := bytes.LastIndex(out, []byte("\n{")); i >= 0 {
		out = out[i+1:]
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil || !resp.Success {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" && runErr != nil {
			detail = runErr.Error()
		}
		if len(detail) > 400 {
			detail = detail[:400]
		}
		return "", fmt.Errorf("launch failed: %s", detail)
	}
	if resp.SessionID != "" {
		return resp.SessionID, nil
	}
	return resp.ID, nil
}

// SendToSession queues text for an agent-deck session (delivered when it is
// idle). matrix/hardened: used to pass a rejected review's reason back to the
// session that did the work. The text goes on stdin, never into argv.
func SendToSession(ctx context.Context, exe, profile, sessionID, text string) error {
	if exe == "" {
		return fmt.Errorf("send: agent-deck binary unknown")
	}
	var args []string
	if profile != "" {
		args = append(args, "-p", profile)
	}
	args = append(args, "session", "send", "--queue", "--message-file", "-", sessionID)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("send to %s failed: %w %s", sessionID, err, msg)
	}
	return nil
}
