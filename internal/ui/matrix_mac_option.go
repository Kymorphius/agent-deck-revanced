package ui

// matrix/hardened: Option+key hotkeys on macOS terminals that keep Option as
// "Normal" (iTerm2's and Terminal.app's default). There Option+W does not
// reach us as alt+w but as the character the keyboard layout puts on it,
// "∑", so every alt+<key> hotkey of the session list (alt+w, alt+t, alt+a,
// alt+j/k, alt+1-9, ...) did nothing. normalizeMainKey maps those characters
// back, for the session list only: dialogs, search and the ":" prompt read
// their own keys and still get the character.
//
// The table is the US/ABC layout. Option+E/I/N/U/` are dead keys there (they
// wait for the next key and send nothing alone), so alt+e/i/n/u/` cannot be
// recovered this way; setting the terminal's Option key to Esc+ makes every
// alt+key arrive as ESC+key instead.
var matrixMacOptionKeys = map[string]string{
	"å": "alt+a", "∫": "alt+b", "ç": "alt+c", "∂": "alt+d", "ƒ": "alt+f",
	"©": "alt+g", "˙": "alt+h", "∆": "alt+j", "˚": "alt+k", "¬": "alt+l",
	"µ": "alt+m", "ø": "alt+o", "π": "alt+p", "œ": "alt+q", "®": "alt+r",
	"ß": "alt+s", "†": "alt+t", "√": "alt+v", "∑": "alt+w", "≈": "alt+x",
	"¥": "alt+y", "Ω": "alt+z",
	"¡": "alt+1", "™": "alt+2", "£": "alt+3", "¢": "alt+4", "∞": "alt+5",
	"§": "alt+6", "¶": "alt+7", "•": "alt+8", "ª": "alt+9", "º": "alt+0",
	"÷": "alt+/", "¿": "alt+?", // matrix/hardened: group/view search with ordinary Option keys.
}

// matrixMacOptionKey returns the alt+key a macOS Option character stands for,
// or pressed unchanged.
func matrixMacOptionKey(pressed string) string {
	if alt, ok := matrixMacOptionKeys[pressed]; ok {
		return alt
	}
	return pressed
}
