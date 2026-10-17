package session

import (
	"os"
	"strings"
	"testing"
)

// matrix/hardened: absent configuration clears local search; both explicit
// choices must survive TOML loading and expose the same CLI effective value.
func TestMatrixClearSearchOnOpenConfig(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{name: "absent", want: true},
		{name: "true", body: "[ui]\nclear_search_on_open = true\n", want: true},
		{name: "false", body: "[ui]\nclear_search_on_open = false\n", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			withTempHomeAndConfig(t, tt.body)
			cfg, err := LoadUserConfig()
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.UI.GetClearSearchOnOpen(); got != tt.want {
				t.Fatalf("GetClearSearchOnOpen() = %v, want %v", got, tt.want)
			}
			key, ok := LookupConfigKey("ui.clear_search_on_open")
			if !ok || key.Get(cfg) != tt.want {
				t.Fatalf("CLI config key = %+v (found %v), want effective %v", key, ok, tt.want)
			}
		})
	}
}

// matrix/hardened: CLI clients may discover and set the opt-out without
// changing another setting, and the schema advertises its true default.
func TestMatrixClearSearchOnOpenSchema(t *testing.T) {
	var key ConfigKey
	for _, candidate := range ConfigKeys() {
		if candidate.Key == "ui.clear_search_on_open" {
			key = candidate
			break
		}
	}
	if key.Key == "" || key.Type != "bool" || key.Default != true || key.RestartRequired {
		t.Fatalf("unexpected local-search schema: %+v", key)
	}
	cfg := &UserConfig{}
	for _, raw := range []string{"false", "true"} {
		value, err := key.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		key.Set(cfg, value)
		if cfg.UI.ClearSearchOnOpen == nil || key.Get(cfg) != value {
			t.Fatalf("setting %s did not round-trip: %+v", raw, cfg.UI)
		}
	}
}

// matrix/hardened: a setup-wizard-style panel leaves this pointer nil; a
// settings-panel save supplies it, including false. Neither path may lose
// unrelated UI fields or mutate the cached disk config.
func TestMatrixClearSearchOnOpenMergeAndPersistence(t *testing.T) {
	withTempHomeAndConfig(t, "[ui]\nclear_search_on_open = false\nfooter = \"curated\"\n")
	for _, choice := range []*bool{nil, boolRef(true), boolRef(false)} {
		disk, err := LoadUserConfig()
		if err != nil {
			t.Fatal(err)
		}
		before := disk.UI.GetClearSearchOnOpen()
		want := before
		if choice != nil {
			want = *choice
		}
		merged, err := MergePanelConfigOntoDisk(&UserConfig{UI: UISettings{ClearSearchOnOpen: choice}})
		if err != nil {
			t.Fatal(err)
		}
		if merged.UI.ClearSearchOnOpen == nil || merged.UI.GetClearSearchOnOpen() != want {
			t.Fatalf("merge choice %v lost search preference: %+v", choice, merged.UI)
		}
		if merged.UI.Footer != "curated" || disk.UI.GetClearSearchOnOpen() != before {
			t.Fatalf("merge changed unrelated UI preference or cached disk value: %+v", merged.UI)
		}
		if err := SaveUserConfig(merged); err != nil {
			t.Fatal(err)
		}
		ClearUserConfigCache()
		reloaded, err := LoadUserConfig()
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.UI.ClearSearchOnOpen == nil || reloaded.UI.GetClearSearchOnOpen() != want {
			t.Fatalf("reload lost explicit %v: %+v", want, reloaded.UI)
		}
	}
	path, err := GetUserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "clear_search_on_open = false") {
		t.Fatalf("explicit false was omitted from saved TOML:\n%s", body)
	}
}
