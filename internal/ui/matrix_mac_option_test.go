package ui

// matrix/hardened: macOS Option characters reach the alt+key hotkeys.

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestMatrixMacOption_CharactersMapToAltKeys(t *testing.T) {
	h := NewHome()
	for in, want := range map[string]string{
		"∑": "alt+w", "†": "alt+t", "å": "alt+a", "∆": "alt+j", "˚": "alt+k", "¡": "alt+1",
		"w": "w", "j": "j", "中": "中", // ordinary keys untouched
	} {
		if got := h.normalizeMainKey(in); got != want {
			t.Errorf("normalizeMainKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatrixMacOption_SigmaOpensWebServicePanel(t *testing.T) {
	home, _ := buildTwoGroupHome(t)

	model, _ := home.handleMainKey(plainKeyMsg('∑'))
	h := model.(*Home)

	if h.webServicePanel == nil || !h.webServicePanel.IsVisible() {
		t.Fatal("Option+W (∑) did not open the alt+w web service panel")
	}
}

func TestMatrixMacOption_GroupNavWithOptionJK(t *testing.T) {
	home, idx := buildTwoGroupHome(t)
	home.cursor = idx["a1"]

	h := matrixPressKeys(t, home, '∆') // Option+J = alt+j: next session in group
	if it := h.flatItems[h.cursor]; it.Type != session.ItemTypeSession || it.Session.Title != "a2" {
		t.Fatalf("Option+J (∆): cursor on %s, want a2", describeCursor(h))
	}
	h = matrixPressKeys(t, h, '˚') // Option+K = alt+k
	if h.cursor != idx["a1"] {
		t.Fatalf("Option+K (˚): cursor on %s, want a1", describeCursor(h))
	}
}
