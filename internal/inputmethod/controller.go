// matrix/hardened: temporarily bypass an IME while the TUI owns navigation keys.
package inputmethod

import "sync"

type backend interface {
	foreground() bool
	current() string
	ascii() string
	selectSource(string) bool
}

// Controller belongs to one foreground terminal, and is used on the UI thread.
// It never guesses which Chinese input method the operator prefers.
type Controller struct {
	mu      sync.Mutex // signal cleanup can race the last UI update
	backend backend
	saved   string
	forced  string
	blurred bool
}

// New returns nil on unsupported builds, SSH connections, and detached processes.
func New() *Controller {
	b := newBackend()
	if b == nil {
		return nil
	}
	return &Controller{backend: b}
}

// Sync saves the current source before navigation, then restores it for typing.
// A source the user selected manually takes precedence over a pending restore.
func (c *Controller) Sync(navigation bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blurred = false
	if !navigation {
		c.restore()
		return
	}
	if !c.backend.foreground() {
		return
	}
	current := c.backend.current()
	if current == "" {
		return
	}
	if c.forced != "" && current == c.forced {
		return
	}
	// A changed source can be a manual choice or a different app's preference.
	c.saved, c.forced = "", ""
	ascii := c.backend.ascii()
	if ascii != "" && current != ascii && c.backend.selectSource(ascii) {
		c.saved, c.forced = current, ascii
	}
}

// Restore only changes a source this controller actually selected, and only
// while its own terminal app is foreground. Failed restores remain retryable.
func (c *Controller) Restore() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restore()
}

// Blur prevents cleanup in a background tab from changing a sibling tab's IME.
// The pending source is retained for the next focused Sync call.
func (c *Controller) Blur() {
	if c != nil {
		c.mu.Lock()
		c.blurred = true
		c.mu.Unlock()
	}
}

func (c *Controller) restore() {
	if c.blurred || c.saved == "" || !c.backend.foreground() {
		return
	}
	current := c.backend.current()
	if current == "" {
		return
	}
	if current != c.forced || c.backend.selectSource(c.saved) {
		c.saved, c.forced = "", ""
	}
}
