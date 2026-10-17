package ui

// matrix/hardened: the settings action manages the local configured Codex home;
// it never follows the selected remote session or starts background polling.
import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/asheshgoplani/agent-deck/internal/aiupdate"
	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func (h *Home) openAIUpdatesPanel() tea.Cmd {
	if h.aiUpdatesPanel == nil {
		h.aiUpdatesPanel = NewAIUpdatesPanel()
	}
	h.aiUpdatesPanel.SetSize(h.width, h.height)
	// Keep ownership of an in-flight installation even when settings is reopened.
	if h.aiUpdatesPanel.busy != "" {
		return h.aiUpdatesPanel.Open(nil, "")
	}
	cfg, err := session.LoadUserConfig()
	if err != nil {
		return h.aiUpdatesPanel.Open(nil, err.Error())
	}
	binary, home, err := aiUpdatesConfiguration(cfg, h.profile)
	if err != nil {
		return h.aiUpdatesPanel.Open(nil, err.Error())
	}
	scope := binary + "\x00" + home
	if h.aiUpdatesPanel.scope != scope {
		h.aiUpdatesPanel.status = aiupdate.Status{}
		h.aiUpdatesPanel.loaded = false
		h.aiUpdatesPanel.notice, h.aiUpdatesPanel.err = "", ""
		h.aiUpdatesPanel.scroll = 0
		h.aiUpdatesPanel.scope = scope
	}
	return h.aiUpdatesPanel.Open(aiupdate.New(binary, home), "")
}

func aiUpdatesConfiguration(cfg *session.UserConfig, profile string) (string, string, error) {
	binary, home := "codex", ""
	if cfg != nil {
		if command := strings.TrimSpace(cfg.Codex.Command); command != "" {
			binary = command
		}
		home = cfg.GetProfileCodexConfigDir(profile)
		if home == "" && cfg.Codex.ConfigDir != "" {
			home = session.ExpandPath(cfg.Codex.ConfigDir)
		}
	}
	if ambient := strings.TrimSpace(os.Getenv("CODEX_HOME")); ambient != "" {
		home = session.ExpandPath(ambient)
	}
	// Accept a literal executable/path only; never reinterpret a shell wrapper,
	// embedded environment assignment, or flags as a different Codex target.
	if strings.ContainsAny(binary, "'\"`$;&|<>()\n\r\t=") || (!filepath.IsAbs(binary) && strings.ContainsFunc(binary, unicode.IsSpace)) {
		return "", home, fmt.Errorf("AI updates require [codex].command to be one executable path or name; shell wrappers, arguments, and environment assignments are not supported")
	}
	binary = session.ExpandPath(binary)
	return binary, home, nil
}

func (h *Home) handleAIUpdatesMsg(msg tea.Msg) (tea.Cmd, bool) {
	result, ok := msg.(aiUpdatesResult)
	if !ok {
		return nil, false
	}
	if h.aiUpdatesPanel != nil {
		h.aiUpdatesPanel.accept(result)
	}
	return nil, true
}
