package ui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: explicitly fork the conversation in its existing directory.
// No branch, workspace, or state-copy defaults apply; launch options and Docker
// behavior still follow the same rules as a regular quick fork.
func resolveInPlaceForkSpec(source *session.Instance, fork session.ForkSettings) quickForkSpec {
	in := quickForkInputs(source, fork, source.IsSandboxed())
	in.Branch = ""
	in.Plan.Worktree = false
	in.Plan.WithState = false
	in.Plan.WithIgnored = false
	return in
}

// matrix/hardened: Alt+F shares the native conversation fork/start pipeline.
func (h *Home) quickForkSessionInPlace(source *session.Instance) tea.Cmd {
	return h.quickForkSessionWithMode(source, true)
}

// matrix/hardened: supplying a container cwd must preserve saved launch
// options and the normal Claude global defaults when no options were saved.
// Return a separate carrier so preparing the fork never modifies the parent.
func prepareInPlaceForkOptions(source *session.Instance, opts *session.ClaudeOptions) *session.ClaudeOptions {
	var prepared *session.ClaudeOptions
	if opts != nil {
		cloned := *opts
		prepared = &cloned
	} else {
		cfg, _ := session.LoadUserConfig()
		prepared = session.NewClaudeOptions(cfg)
	}
	prepared.WorkDir = source.EffectiveWorkingDir()
	return prepared
}

// matrix/hardened: sharing a multi-repo session also shares its existing
// container and working directory. Keep independent metadata slices so edits
// to either session cannot rewrite the other's cleanup references.
func inheritInPlaceMultiRepo(forked, source *session.Instance) {
	forked.ProjectPath = source.ProjectPath
	forked.WorktreePath = source.WorktreePath
	forked.WorktreeRepoRoot = source.WorktreeRepoRoot
	forked.WorktreeBranch = source.WorktreeBranch
	forked.WorktreeType = source.WorktreeType
	forked.MultiRepoEnabled = source.MultiRepoEnabled
	forked.AdditionalPaths = append([]string(nil), source.AdditionalPaths...)
	forked.MultiRepoTempDir = source.MultiRepoTempDir
	forked.MultiRepoWorktrees = append([]session.MultiRepoWorktree(nil), source.MultiRepoWorktrees...)
	if tmuxSession := forked.GetTmuxSession(); tmuxSession != nil {
		tmuxSession.WorkDir = source.EffectiveWorkingDir()
	}
}
