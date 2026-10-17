package ui

// matrix/hardened: exercise terminal bytes and IME punctuation through the
// actual Home.Update path, so shifted filters cannot become vim count digits.
import (
	"bytes"
	"io"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestMatrixStatusFilterKeyboard_Running(t *testing.T) {
	for _, input := range []string{"!", "！", "\x1b[49;2u", "\x1b[33;2u", "\x1b[27;2;33~"} {
		for _, prefix := range []rune{0, '3', '\''} {
			t.Run(input+"/"+string(prefix), func(t *testing.T) {
				home, instances := buildFocusHome(t)
				instances[0].Status = session.StatusRunning
				instances[1].Status = session.StatusWaiting
				instances[2].Status = session.StatusIdle
				instances[3].Status = session.StatusError
				home.rebuildFlatItems()
				if prefix != 0 {
					home.Update(plainKeyMsg(prefix))
				}
				decoded, err := io.ReadAll(NewCSIuReader(bytes.NewBufferString(input)))
				if err != nil {
					t.Fatal(err)
				}
				key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(string(decoded))}
				home.Update(key)
				if home.statusFilter != session.StatusRunning || home.matrixPendingNav() {
					t.Fatalf("input %q: filter=%q, pending navigation=%v", input, home.statusFilter, home.matrixPendingNav())
				}
				if home.flatItemIndexByID(instances[0].ID) < 0 || home.flatItemIndexByID(instances[1].ID) >= 0 {
					t.Fatal("running view did not include running and exclude waiting")
				}
				home.Update(key)
				if home.statusFilter != "" || home.flatItemIndexByID(instances[1].ID) < 0 {
					t.Fatal("second press did not restore the full list")
				}
			})
		}
	}
}

func TestMatrixStatusFilterKeyboard_FullwidthShortcuts(t *testing.T) {
	for _, tc := range []struct {
		key  rune
		want session.Status
	}{{'！', session.StatusRunning}, {'＠', session.StatusWaiting}, {'＃', session.StatusIdle},
		{'＄', session.StatusError}, {'￥', session.StatusError}, {'＆', session.StatusError}, {'％', FilterModeActive}, {'＾', FilterModeArchived}, {'～', FilterModeInterrupted}} {
		t.Run(string(tc.key), func(t *testing.T) {
			home, _ := buildFocusHome(t)
			home.Update(plainKeyMsg(tc.key))
			if home.statusFilter != tc.want {
				t.Fatalf("%c: filter=%q, want %q", tc.key, home.statusFilter, tc.want)
			}
		})
	}
	for _, tc := range []struct{ input, want string }{{"＄", "$"}, {"￥", "$"}, {"＊", "*"}} {
		home, _ := buildFocusHome(t)
		if got := home.normalizeMainKey(tc.input); got != tc.want {
			t.Errorf("normalize %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestMatrixStatusFilterKeyboard_PlainDigitAndText(t *testing.T) {
	home, _ := buildFocusHome(t)
	home.Update(plainKeyMsg('1'))
	if home.navCount.n != 1 || home.statusFilter != "" {
		t.Fatal("plain 1 must remain a navigation count")
	}
	// IME normalization belongs only to main-list commands, not text decoding.
	decoded, err := io.ReadAll(NewCSIuReader(bytes.NewBufferString("！＠＃")))
	if err != nil || string(decoded) != "！＠＃" {
		t.Fatalf("text changed: %q, err=%v", decoded, err)
	}
}

func TestMatrixStatusFilterKeyboard_CtrlShiftDoesNotFilter(t *testing.T) {
	for _, input := range []string{"\x1b[49;6u", "\x1b[50;6u", "\x1b[51;6u", "\x1b[52;6u", "\x1b[53;6u", "\x1b[54;6u", "\x1b[55;6u"} {
		t.Run(input, func(t *testing.T) {
			home, _ := buildFocusHome(t)
			decoded, err := io.ReadAll(NewCSIuReader(bytes.NewBufferString(input)))
			if err != nil {
				t.Fatal(err)
			}
			home.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(string(decoded))})
			if home.statusFilter != "" || home.showCostDashboard {
				t.Fatalf("Ctrl+Shift input %q executed Shift-only shortcut", input)
			}
		})
	}
}

// matrix/hardened: $ always means abnormal, including real terminal encodings,
// independently of cost availability; pending navigation must not steal it.
func TestMatrixStatusFilterKeyboard_Abnormal(t *testing.T) {
	for _, input := range []string{"$", "&", "\x1b[52;2u", "\x1b[36;2u", "\x1b[27;2;36~"} {
		for _, prefix := range []rune{0, '3', '\''} {
			t.Run(input+"/"+string(prefix), func(t *testing.T) {
				home, instances := buildFocusHome(t)
				instances[0].Status = session.StatusError
				instances[1].Status = session.StatusWaiting
				home.rebuildFlatItems()
				if prefix != 0 {
					home.Update(plainKeyMsg(prefix))
				}
				decoded, err := io.ReadAll(NewCSIuReader(bytes.NewBufferString(input)))
				if err != nil {
					t.Fatal(err)
				}
				key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(string(decoded))}
				home.Update(key)
				if home.statusFilter != session.StatusError || home.showCostDashboard || home.matrixPendingNav() || home.err != nil {
					t.Fatalf("input %q: filter=%q costs=%v pending=%v error=%v", input, home.statusFilter, home.showCostDashboard, home.matrixPendingNav(), home.err)
				}
				if home.flatItemIndexByID(instances[0].ID) < 0 || home.flatItemIndexByID(instances[1].ID) >= 0 {
					t.Fatal("abnormal filter membership incorrect")
				}
				home.Update(key)
				if home.statusFilter != "" {
					t.Fatal("second press did not clear abnormal filter")
				}
			})
		}
	}
}
