package main

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/update"
)

// TestHeadlessAutoInstall_Gates pins how `web --no-tui` gets its own
// unattended installer: none when the process is test-, CI- or
// script-driven (issue #2251), none for a Homebrew-managed binary, and
// otherwise one that runs `<exe> update --unattended --trigger web` with
// the update settings as its on/off switch.
func TestHeadlessAutoInstall_Gates(t *testing.T) {
	// matrix/hardened: with default settings (check_enabled and auto_install
	// off) no installer is built at all, so nothing polls GitHub.
	stubHeadlessSuppression(t, "")
	if inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return false }); inst != nil {
		t.Fatal("matrix/hardened: default settings must not build an installer")
	}
	on := true
	prevSettings := headlessUpdateSettings
	headlessUpdateSettings = func() session.UpdateSettings {
		return session.UpdateSettings{CheckEnabled: &on, AutoInstall: &on, MatrixOptIn: true}
	}
	t.Cleanup(func() { headlessUpdateSettings = prevSettings })

	stubHeadlessSuppression(t, "CI=true")
	if inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return false }); inst != nil {
		t.Fatal("a suppressed daemon must not build an installer")
	}

	stubHeadlessSuppression(t, "")
	if inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return true }); inst != nil {
		t.Fatal("a Homebrew-managed binary must not build an installer; brew owns it")
	}
	inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return false })
	if inst == nil {
		t.Fatal("an unsuppressed daemon must get an installer")
	}
	if inst.Exe != "/bin/agent-deck" || inst.Trigger != "web" || inst.RunningVersion != Version {
		t.Fatalf("installer = %+v, want exe, trigger web and the running version", inst)
	}
	// matrix/hardened: Enabled reads the real config (auto_install default
	// off), so it is not asserted to be on here.
	if inst.Enabled == nil {
		t.Fatal("the installer must carry its on/off switch")
	}

	headlessAutoUpdateSuppressed = update.AutoUpdateSuppressed
	if inst := newHeadlessAutoInstaller("/bin/agent-deck", func() bool { return false }); inst != nil {
		t.Fatal("the real predicate must suppress under go test")
	}
}
