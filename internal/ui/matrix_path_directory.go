package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: completion reads the target filesystem, never history paths.
type pathDirectoryResult struct {
	dialog        *NewDialog
	seq           uint64
	query, remote string
	matches       []string
	err           error
}

func (d *NewDialog) completePathDirectory() tea.Cmd {
	d.pathSoftSelected = false
	d.suggestionsActive = false
	d.suggestionNavigated = false
	d.pathInput.Focus()
	d.pathCompletionStatus = ""
	if d.pathCycler.IsActive() {
		d.pathInput.SetValue(d.pathCycler.Next())
		d.pathInput.CursorEnd()
		return nil
	}
	query := d.pathInput.Value()
	if !d.remoteTarget {
		matches, err := session.GetDirectoryCompletions(query)
		d.applyDirectoryMatches(matches, err)
		return nil
	}
	d.pathCompletionSeq++
	seq, remote := d.pathCompletionSeq, d.remoteName
	d.pathCompletionStatus = "Completing on " + remote + "…"
	return func() tea.Msg {
		result := pathDirectoryResult{dialog: d, seq: seq, query: query, remote: remote}
		cfg, err := session.LoadUserConfig()
		if err != nil {
			result.err = err
			return result
		}
		rc, ok := cfg.Remotes[remote]
		if !ok {
			result.err = fmt.Errorf("unknown remote %q", remote)
			return result
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		data, err := session.NewSSHRunner(remote, rc).Run(ctx, "__complete-path", query)
		if err == nil {
			err = json.Unmarshal(data, &result.matches)
		}
		result.err = err
		return result
	}
}

func (d *NewDialog) applyDirectoryMatches(matches []string, err error) {
	d.pathCompletionStatus = ""
	if err != nil {
		reason := cellTruncate(sanitizeMatchForDisplay(err.Error()), 100, "…")
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "remote request timed out"
		}
		d.pathCompletionStatus = "Directory completion unavailable: " + reason
		uiLog.Warn("path_directory_completion_failed", "remote", d.remoteName, "error", err)
		return
	}
	if len(matches) == 0 {
		d.pathCompletionStatus = "No matching directories"
		return
	}
	d.pathCycler.Reset()
	if len(matches) == 1 {
		d.pathInput.SetValue(withTrailingPathSep(matches[0]))
	} else {
		d.pathCycler.SetMatches(matches)
		d.pathInput.SetValue(d.pathCycler.Next())
	}
	d.pathInput.CursorEnd()
	d.filterPathSuggestions()
}

func (d *NewDialog) acceptDirectoryResult(r pathDirectoryResult) {
	if r.dialog != d || !d.visible || r.seq != d.pathCompletionSeq || r.remote != d.remoteName || r.query != d.pathInput.Value() || (!d.multiRepoEditing && d.currentTarget() != focusPath) {
		return
	}
	d.applyDirectoryMatches(r.matches, r.err)
}
