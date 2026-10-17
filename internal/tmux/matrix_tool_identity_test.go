package tmux

import (
	"testing"
	"time"
)

// matrix/hardened: command arguments and generated instructions are not
// evidence of the runtime's executable. Unknown wrappers fail closed.
func TestMatrixDetectToolFromCommandRejectsIncidentalToolText(t *testing.T) {
	tests := map[string]string{
		"generated Codex env prelude":          `export AGENTDECK_SESSION_ID='session' && export AGENTDECK_TOOL='codex' && codex -c 'developer_instructions="agent-deck launch <path> -t Title -c claude"'`,
		"generated Codex PATH prelude":         `for d in "$HOME/.local/bin"; do export PATH="$d:$PATH"; done; codex -c 'developer_instructions="Claude and Codex sessions"'`,
		"bash launch wrapper":                  `bash -c 'codex -c '\''developer_instructions="launch with -c claude"'\'''`,
		"unrelated executable":                 `echo claude gemini opencode codex copilot crush cursor hermes pi @deepseek-ai/dsh @oh-my-pi/pi-coding-agent`,
		"tool name in executable path":         `/opt/claude/bin/printf '%s' codex`,
		"tool name in environment value":       `PROFILE=claude env OTHER=codex bash`,
		"npx package option is not a launcher": `npx --package @github/copilot echo claude`,
		"npx unrelated launcher argument":      `npx example-package @deepseek-ai/dsh`,
		"npx package suffix":                   `npx @github/copilot-example`,
		"malformed quoted command":             `codex -c 'developer_instructions=claude`,
		"env unset argument":                   `env -u claude bash`,
		"sudo user argument":                   `sudo -u claude bash`,
		"sudo host argument":                   `sudo -h claude bash`,
		"sudo shell wrapper":                   `sudo -s claude`,
		"unknown env option":                   `env --split-string 'claude'`,
		"unknown sudo option":                  `sudo --role claude bash`,
	}
	tests["env missing option value"] = `env -u -i codex`
	tests["sudo missing option value"] = `sudo -u -- codex`
	for _, tool := range []string{"claude", "gemini", "opencode", "open-code", "codex", "copilot", "crush", "muse", "cursor", "hermes", "dsh", "pi", "omp", "oh-my-pi"} {
		tests["argument mentions "+tool] = "echo " + tool
	}
	for name, command := range tests {
		t.Run(name, func(t *testing.T) {
			if got := detectToolFromCommand(command); got != "" {
				t.Fatalf("detectToolFromCommand(%q) = %q, want no runtime identity", command, got)
			}
		})
	}
}

// matrix/hardened: cache expiry must not let generated Claude references
// overwrite the Codex runtime that was already recognized.
func TestMatrixDetectToolKeepsCodexForWrappedInstructions(t *testing.T) {
	sess := NewSession("matrix-tool-identity-wrapper", "/tmp")
	sess.Command = `export AGENTDECK_TOOL=codex && codex -c 'developer_instructions="launch -c claude"'`
	sess.detectedTool = "codex"
	sess.toolDetectedAt = time.Now().Add(-time.Hour)
	sess.cacheContent = "Welcome to Claude Code!"
	sess.cacheTime = time.Now()
	seedPaneCommand(t, sess.Name, "bash")

	if got := sess.DetectTool(); got != "codex" {
		t.Fatalf("DetectTool() = %q, want codex despite wrapped instructions and incidental content", got)
	}
}

// matrix/hardened: supported executable aliases and exact package launchers
// remain usable without widening detection to arbitrary argument text.
func TestMatrixDetectToolFromCommandSupportedPrefixes(t *testing.T) {
	tests := map[string]string{
		`codex -c 'developer_instructions="launch -c claude"'`: "codex",
		`env AGENTDECK_TOOL=codex codex`:                       "codex",
		`env -- codex`:                                         "codex",
		`env -i codex`:                                         "codex",
		`env -u CLAUDE_CODE_ENTRYPOINT codex`:                  "codex",
		`sudo codex`:                                           "codex",
		`sudo -- codex`:                                        "codex",
		`sudo -n -u claude codex`:                              "codex",
		`env TOOL=claude sudo -g claude codex`:                 "codex",
		`npx @github/copilot`:                                  "copilot",
		`env PROFILE=work npx @deepseek-ai/dsh web`:            "deepseek",
		`sudo npx @oh-my-pi/pi-coding-agent`:                   "omp",
	}
	tests[`sudo -E -H -S codex`] = "codex"
	tests[`npx -y @github/copilot`] = "copilot"
	tests[`npx --yes -- @deepseek-ai/dsh`] = "deepseek"
	tests[`/opt/AI\ Tools/CODEX.EXE`] = "codex"
	for command, want := range tests {
		if got := detectToolFromCommand(command); got != want {
			t.Errorf("detectToolFromCommand(%q) = %q, want %q", command, got, want)
		}
	}
}
