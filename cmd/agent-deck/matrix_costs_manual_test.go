package main

import (
	"fmt"
	"github.com/asheshgoplani/agent-deck/internal/session"
	"os"
	"path/filepath"
	"testing"
)

// matrix/hardened: cost producer tests opt in instead of relying on the old
// background default. Existing account/Recall settings are retained.
func enableAutomaticCostsForTest(t *testing.T) {
	t.Helper()
	session.ClearUserConfigCache()
	cfg, err := session.LoadUserConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		cfg = &session.UserConfig{}
	}
	v := true
	cfg.Costs.AutoCollect = &v
	if err := session.SaveUserConfig(cfg); err != nil {
		t.Fatal(err)
	}
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
}

func TestMatrixCostsManualHookAndRecallNoImplicitImport(t *testing.T) {
	_, personal, _ := hookHome(t, true)
	path := filepath.Join(personal, "projects", "p", "aaaaaaaa-0000-4000-8000-000000000001.jsonl")
	writeTurn(t, path, "aaaaaaaa-0000-4000-8000-000000000001")
	payload := []byte(fmt.Sprintf(`{"hook_event_name":"Stop","transcript_path":%q}`, path))
	writeCostEvent("manual-costs", payload)
	if _, err := os.Stat(getCostEventsDir()); !os.IsNotExist(err) {
		t.Fatalf("manual mode created spool: %v", err)
	}
	// A disabled sink exits before opening or touching any registry database.
	if err := (&recallRegistry{}).Usage("default", "manual-costs", nil); err != nil {
		t.Fatal(err)
	}
	enableAutomaticCostsForTest(t)
	writeCostEvent("auto-costs", payload)
	if len(costEventFiles(t)) != 1 {
		t.Fatal("explicit automatic mode failed to produce usage")
	}
	cfg, err := session.LoadUserConfig()
	if err != nil {
		t.Fatal(err)
	}
	v := false
	cfg.Costs.AutoCollect = &v
	if err := session.SaveUserConfig(cfg); err != nil {
		t.Fatal(err)
	}
	session.ClearUserConfigCache()
	writeCostEvent("manual-again", payload)
	if len(costEventFiles(t)) != 1 {
		t.Fatal("disabled mode continued writing usage")
	}
}
