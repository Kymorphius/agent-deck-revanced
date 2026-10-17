package session

import (
	"github.com/BurntSushi/toml"
	"testing"
)

// matrix/hardened: omitted costs settings remain manual, and a stored explicit
// option round-trips through the same schema used by config get/set.
func TestMatrixCostsManualDefaultAndOptIn(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"", false}, {"[costs]\ncurrency='usd'", false},
		{"[costs]\nauto_collect=false", false}, {"[costs]\nauto_collect=true", true},
	} {
		var cfg UserConfig
		if _, err := toml.Decode(tc.input, &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.Costs.GetAutoCollect() != tc.want {
			t.Fatalf("%q: wrong mode", tc.input)
		}
	}
	key, ok := LookupConfigKey("costs.auto_collect")
	if !ok || key.Get(nil) != false || !key.RestartRequired {
		t.Fatal("missing safe schema default/reload requirement")
	}
	cfg := &UserConfig{}
	for _, v := range []bool{true, false} {
		key.Set(cfg, v)
		if key.Get(cfg) != v || cfg.Costs.AutoCollect == nil {
			t.Fatal("explicit option not retained")
		}
	}
}
