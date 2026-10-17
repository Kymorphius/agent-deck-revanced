package session

import (
	"os"
	"path/filepath"
	"testing"
)

// The native-SSH TUI acceptance (cmd/agent-deck/native_tui_ssh_test.go) makes
// itself release-independent by writing `[updates]\ncheck_enabled = false` into
// its fixture config: without it, promptForUpdate() prints a startup
// "Update available" banner into the TUI pane the moment a newer release
// exists, displacing the SESSIONS/PREVIEW divider the test asserts (this is
// what broke it after v1.16.7 shipped). This guards the exact config key that
// fix relies on — if `check_enabled` is renamed or the parse breaks, the
// determinism fix would silently stop working and this fails first.
func TestUpdateCheckEnabled_FixtureConfigDisablesStartupCheck(t *testing.T) {
	writeConfig := func(t *testing.T, body string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
		t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
		dir := filepath.Join(home, ".config", "agent-deck")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		ClearUserConfigCache()
		t.Cleanup(ClearUserConfigCache)
	}

	// The exact update stanza the acceptance fixture writes disables the check.
	writeConfig(t, "[telemetry]\ndisabled = true\n[updates]\ncheck_enabled = false\n[claude]\nhooks_enabled = false\n")
	if GetUpdateSettings().GetCheckEnabled() {
		t.Fatal("check_enabled = false must disable the startup update check (fixture would still banner)")
	}

	// matrix/hardened: without the stanza the check now defaults OFF too.
	writeConfig(t, "[telemetry]\ndisabled = true\n[claude]\nhooks_enabled = false\n")
	if GetUpdateSettings().GetCheckEnabled() {
		t.Fatal("matrix/hardened: update check must default off")
	}
}

// matrix/hardened: check_enabled / auto_install written by a stock install
// (template or settings-panel save) are not consent; they only count with
// [updates] matrix_opt_in = true, which this build's panel / `config set` write.
func TestUpdateSettings_StockWrittenValuesNeedMatrixOptIn(t *testing.T) {
	on := true
	stock := UpdateSettings{CheckEnabled: &on, AutoInstall: &on}
	if stock.GetCheckEnabled() || stock.GetAutoInstall() {
		t.Fatal("stock-written check_enabled/auto_install must be ignored without matrix_opt_in")
	}
	stock.MatrixOptIn = true
	if !stock.GetCheckEnabled() || !stock.GetAutoInstall() {
		t.Fatal("explicit opt-in in this build must enable check_enabled/auto_install")
	}
}

// matrix/hardened: `config set updates.check_enabled true` must not also
// activate a stale stock-written auto_install = true (and vice versa).
func TestConfigSet_OptInDoesNotActivateOtherStockKey(t *testing.T) {
	for _, tc := range []struct{ set, other string }{
		{"updates.check_enabled", "updates.auto_install"},
		{"updates.auto_install", "updates.check_enabled"},
	} {
		on := true
		c := &UserConfig{Updates: UpdateSettings{CheckEnabled: &on, AutoInstall: &on}}
		k, ok := LookupConfigKey(tc.set)
		if !ok {
			t.Fatalf("missing key %s", tc.set)
		}
		k.Set(c, true)
		o, _ := LookupConfigKey(tc.other)
		if k.Get(c) != true {
			t.Errorf("%s: set key not enabled", tc.set)
		}
		if o.Get(c) != false {
			t.Errorf("%s: opting in activated stale %s", tc.set, tc.other)
		}
		// Once opted in, setting the other key is independent.
		o.Set(c, true)
		if k.Get(c) != true || o.Get(c) != true {
			t.Errorf("%s: second explicit opt-in lost a value", tc.set)
		}
	}
}
