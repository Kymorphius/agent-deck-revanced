package ui

import (
	"encoding/json"
	"fmt"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: save only launch controls, never path/name/branch/parent.
func (d *NewDialog) SaveDefaults() error {
	if d.remoteTarget {
		return fmt.Errorf("save defaults is available for local sessions")
	}
	cfg, err := session.ReloadUserConfig()
	if err != nil {
		return err
	}
	// matrix/hardened: a failed write must not mutate the shared config cache.
	copy := *cfg
	cfg = &copy
	preset := &session.NewSessionDefaults{
		Tool: d.GetSelectedCommand(), Model: d.GetLaunchModelID(),
		ReasoningEffort: d.GetLaunchReasoningEffort(), Worktree: d.worktreeEnabled,
		Sandbox: d.sandboxEnabled, MultiRepo: d.multiRepoEnabled,
	}
	if d.commandCursor == 0 {
		preset.Tool = "shell"
		preset.CustomCommand = d.commandInput.Value()
	}
	switch d.toolKind(preset.Tool) {
	case "codex":
		preset.YoloMode = d.GetCodexYoloMode()
	case "gemini":
		preset.YoloMode = d.IsGeminiYoloMode()
	case "hermes":
		preset.YoloMode = d.GetHermesYoloMode()
	case "claude":
		opts := d.GetClaudeOptions()
		// A resume target identifies one session, not a launch default.
		opts.ResumeSessionID = ""
		if opts.SessionMode == "resume" {
			opts.SessionMode = "new"
		}
		data, err := json.Marshal(opts)
		if err != nil {
			return err
		}
		preset.ClaudeOptions = string(data)
		preset.ClaudeExtraArgs = d.GetClaudeExtraArgs()
		preset.ClaudeAccount = d.GetClaudeAccount()
		preset.ClaudeStartQuery = d.GetClaudeStartQuery()
	case "omp":
		opts := d.GetOMPOptions()
		opts.ResumeID = ""
		if opts.SessionMode == "resume" {
			opts.SessionMode = "new"
		}
		data, err := json.Marshal(opts)
		if err != nil {
			return err
		}
		preset.OMPOptions = string(data)
	}
	cfg.DefaultTool = preset.Tool
	cfg.NewSessionDefaults = preset
	if err := session.SaveUserConfig(cfg); err != nil {
		return err
	}
	d.defaultsStatus = "Defaults saved (Path excluded)"
	return nil
}

func (d *NewDialog) applySavedDefaults(p *session.NewSessionDefaults) {
	if p == nil || d.remoteTarget {
		return
	}
	d.SetDefaultTool(p.Tool)
	if p.Tool == "shell" {
		d.commandCursor = 0
		d.commandInput.SetValue(p.CustomCommand)
	}
	if d.GetSelectedCommand() != p.Tool && p.Tool != "shell" {
		return
	}
	d.modelInput.SetValue(p.Model)
	d.reasoningEffort = p.ReasoningEffort
	d.worktreeEnabled, d.worktreeToggled = p.Worktree, p.Worktree
	d.branchAutoSet = p.Worktree
	d.sandboxEnabled = p.Sandbox
	if p.MultiRepo && !d.multiRepoEnabled {
		d.ToggleMultiRepo()
	}
	switch d.toolKind(p.Tool) {
	case "codex":
		d.codexOptions.SetDefaults(p.YoloMode)
	case "gemini":
		d.geminiOptions.SetDefaults(p.YoloMode)
	case "hermes":
		d.hermesOptions.SetDefaults(p.YoloMode)
	case "claude":
		var opts session.ClaudeOptions
		if json.Unmarshal([]byte(p.ClaudeOptions), &opts) == nil {
			d.claudeOptions.SetFromOptions(&opts)
		}
		d.claudeOptions.SetExtraArgs(p.ClaudeExtraArgs)
		d.claudeOptions.SetAccount(p.ClaudeAccount)
		d.claudeOptions.SetStartQuery(p.ClaudeStartQuery)
	case "omp":
		var opts session.OMPOptions
		if json.Unmarshal([]byte(p.OMPOptions), &opts) == nil {
			d.ompOptions.SetDefaults(&opts)
		}
	}
	d.updateToolOptions()
}

func (d *NewDialog) saveDefaultsFromUI() {
	if err := d.SaveDefaults(); err != nil {
		d.defaultsStatus = ""
		d.SetError("Cannot save defaults: " + err.Error())
	} else {
		d.ClearError()
		// Show the readback even when Ctrl+D was pressed far above the action.
		d.focusIndex = d.indexOf(focusSaveDefaults)
		d.updateFocus()
	}
}
