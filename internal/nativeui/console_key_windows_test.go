//go:build windows

package nativeui

import (
	"context"
	"io"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// matrix/hardened: verify the real Tea filter drops a pending first half and
// creates separate decoder state even when the same option is reused.
func TestConsoleKeyProgramOptionUsesIndependentWindowState(t *testing.T) {
	option := consoleKeyProgramOption()
	if got := runConsoleKeyProgram(t, option, []tea.Msg{consoleRunes(0xD83D)}); got != "" {
		t.Fatalf("first window emitted an unfinished pair: %q", got)
	}
	if got := runConsoleKeyProgram(t, option, []tea.Msg{consoleRunes(0xDE00), consoleRunes(0xD83D), tea.WindowSizeMsg{Width: 90, Height: 30}, consoleRunes(0xDE00), consoleRunes('中', '文', '😀')}); got != "😀中文😀" {
		t.Fatalf("second window input = %q, want 😀中文😀", got)
	}
}

type consoleKeyTestModel struct{ text string }

func (*consoleKeyTestModel) Init() tea.Cmd { return nil }
func (*consoleKeyTestModel) View() string  { return "" }
func (m *consoleKeyTestModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyRunes {
		m.text += string(key.Runes)
	}
	return m, nil
}

func runConsoleKeyProgram(t *testing.T, option tea.ProgramOption, input []tea.Msg) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	program := tea.NewProgram(&consoleKeyTestModel{}, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler(), option)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for _, msg := range input {
			program.Send(msg)
		}
		program.Send(tea.Quit())
	}()
	model, err := program.Run()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sent:
	case <-ctx.Done():
		t.Fatal("program sender did not stop")
	}
	return model.(*consoleKeyTestModel).text
}
