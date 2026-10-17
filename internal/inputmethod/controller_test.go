package inputmethod

// matrix/hardened: exercise ownership/restoration without changing a live IME.
import (
	"reflect"
	"testing"
)

type fakeBackend struct {
	active bool
	source string
	deny   bool
	calls  []string
}

func (b *fakeBackend) foreground() bool { return b.active }
func (b *fakeBackend) current() string  { return b.source }
func (b *fakeBackend) ascii() string    { return "ABC" }
func (b *fakeBackend) selectSource(id string) bool {
	b.calls = append(b.calls, id)
	if b.deny {
		return false
	}
	b.source = id
	return true
}

func TestInputMethodNavigationAndRestore(t *testing.T) {
	b := &fakeBackend{active: true, source: "Rime"}
	c := &Controller{backend: b}
	c.Sync(true)
	c.Sync(true)
	if b.source != "ABC" {
		t.Fatal("navigation must bypass the IME")
	}
	c.Sync(false)
	c.Restore()
	if b.source != "Rime" || !reflect.DeepEqual(b.calls, []string{"ABC", "Rime"}) {
		t.Fatalf("source=%q selections=%v", b.source, b.calls)
	}
}

func TestInputMethodForegroundAndManualChoice(t *testing.T) {
	b := &fakeBackend{source: "Rime"}
	c := &Controller{backend: b}
	c.Sync(true)
	if len(b.calls) != 0 {
		t.Fatal("background terminal changed the source")
	}
	b.active = true
	c.Sync(true)
	b.active = false
	c.Restore()
	if b.source != "ABC" || len(b.calls) != 1 {
		t.Fatal("background cleanup changed another app")
	}
	b.source, b.active = "OtherIME", true
	c.Sync(false)
	if b.source != "OtherIME" || len(b.calls) != 1 {
		t.Fatal("restore overwrote a manual input-source selection")
	}
	c.Sync(true)
	c.Sync(false)
	if b.source != "OtherIME" {
		t.Fatal("new navigation cycle did not remember the new preference")
	}
}

func TestInputMethodSelectionFailure(t *testing.T) {
	b := &fakeBackend{active: true, source: "Rime", deny: true}
	c := &Controller{backend: b}
	c.Sync(true)
	if c.saved != "" {
		t.Fatal("failed selection must not acquire ownership")
	}
	b.deny = false
	c.Sync(true)
	b.deny = true
	c.Restore()
	if c.saved != "Rime" {
		t.Fatal("failed restoration must remain retryable")
	}
	b.deny = false
	c.Restore()
	if b.source != "Rime" || c.saved != "" {
		t.Fatal("retry did not restore the source")
	}
}

func TestInputMethodASCIIAndNil(t *testing.T) {
	b := &fakeBackend{active: true, source: "ABC"}
	c := &Controller{backend: b}
	c.Sync(true)
	c.Sync(false)
	if len(b.calls) != 0 {
		t.Fatal("an ASCII user should not be forced into a different input method")
	}
	var disabled *Controller
	disabled.Sync(true)
	disabled.Restore()
	disabled.Blur()
}

func TestInputMethodBlurredSiblingTab(t *testing.T) {
	b := &fakeBackend{active: true, source: "Rime"}
	c := &Controller{backend: b}
	c.Sync(true)
	c.Blur()
	c.Restore()
	if b.source != "ABC" || len(b.calls) != 1 {
		t.Fatal("background-tab cleanup overwrote the sibling tab's source")
	}
	c.Sync(false)
	if b.source != "Rime" {
		t.Fatal("pending source was not restored on focused typing")
	}
}
