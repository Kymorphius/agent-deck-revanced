package session

// matrix/hardened: persist launch controls independently of paths and session
// names. JSON option payloads use the existing tool option schema.
type NewSessionDefaults struct {
	Tool             string   `toml:"tool"`
	Model            string   `toml:"model,omitempty"`
	ReasoningEffort  string   `toml:"reasoning_effort,omitempty"`
	YoloMode         bool     `toml:"yolo_mode,omitempty"`
	Worktree         bool     `toml:"worktree,omitempty"`
	Sandbox          bool     `toml:"sandbox,omitempty"`
	MultiRepo        bool     `toml:"multi_repo,omitempty"`
	ClaudeOptions    string   `toml:"claude_options,omitempty"`
	ClaudeExtraArgs  []string `toml:"claude_extra_args,omitempty"`
	ClaudeAccount    string   `toml:"claude_account,omitempty"`
	ClaudeStartQuery string   `toml:"claude_start_query,omitempty"`
	OMPOptions       string   `toml:"omp_options,omitempty"`
	CustomCommand    string   `toml:"custom_command,omitempty"`
}
