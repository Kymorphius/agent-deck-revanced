package nativeterminal

// matrix/hardened: the original session verifies the returned identity; a
// caller editing that value cannot redirect the immutable wire target.

import (
	"context"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"github.com/google/uuid"
)

func TestBindingIdentityIsImmutableValue(t *testing.T) {
	id, run := uuid.NewString(), uuid.NewString()
	terminal, err := NewWithCaller(winipc.PipePrefix+"binding-value", id, run, func(context.Context, string, terminalhost.Request) (terminalhost.Response, error) {
		t.Fatal("reading binding identity contacted the manager")
		return terminalhost.Response{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := terminal.Binding()
	if binding.SessionID != id || binding.RunID != run {
		t.Fatal("binding identity was not the selected run")
	}
	binding.SessionID, binding.RunID = uuid.NewString(), uuid.NewString()
	if got := terminal.Binding(); got.SessionID != id || got.RunID != run {
		t.Fatal("returned value redirected the terminal")
	}
}
