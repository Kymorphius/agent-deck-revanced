package main

// matrix/hardened: validate shell boundaries and exact binary input without starting real sessions.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestHostPreservesStructuredArgv(t *testing.T) {
	config, err := parseOptions([]string{"host", "--pipe", pipePrefix + "test", "--cwd", `C:\project with spaces`, "--env", "AD_MARK=中文值", "--", `C:\Program Files\Agent\agent.exe`, "--model", "two words", `literal$(value);&`, ""}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if config.Executable != `C:\Program Files\Agent\agent.exe` || !reflect.DeepEqual(config.Args, []string{"--model", "two words", `literal$(value);&`, ""}) {
		t.Fatalf("argv changed: %#v", config)
	}
	if config.Cols != 120 || config.Rows != 30 || !reflect.DeepEqual(config.Env, []string{"AD_MARK=中文值"}) {
		t.Fatalf("host options changed: %#v", config)
	}
}

func TestSendPreservesUTF8AndBinary(t *testing.T) {
	config, err := parseOptions([]string{"send", "--pipe", pipePrefix + "test", "--text", "你好\n世界", "--enter"}, io.Discard)
	if err != nil || !bytes.Equal(config.Data, []byte("你好\n世界\r")) {
		t.Fatalf("text changed: %q, %v", config.Data, err)
	}
	want := []byte{0, 0xff, 0x1b, 0x11, '\r', '\n'}
	config, err = parseOptions([]string{"send", "--pipe", pipePrefix + "test", "--base64", base64.StdEncoding.EncodeToString(want)}, io.Discard)
	if err != nil || !bytes.Equal(config.Data, want) {
		t.Fatalf("binary changed: %v, %v", config.Data, err)
	}
}

func TestOptionsRejectUnsafeOrAmbiguousInputs(t *testing.T) {
	tests := [][]string{
		{"host", "--pipe", pipePrefix + "test", "cmd.exe"},
		{"host", "--pipe", pipePrefix + "test", "cmd.exe", "--", "extra"},
		{"host", "--pipe", pipePrefix + "test", "--"},
		{"host", "--pipe", pipePrefix + "test", "--env", "MISSING_EQUALS", "--", "cmd.exe"},
		{"host", "--pipe", pipePrefix + "test", "--env", "=VALUE", "--", "cmd.exe"},
		{"host", "--pipe", pipePrefix + "test", "--", "cmd.exe", "bad\x00arg"},
		{"send", "--pipe", pipePrefix + "test", "--base64", "AA==", "--text", ""},
		{"send", "--pipe", pipePrefix + "test", "--base64", "AA==", "--enter"},
		{"send", "--pipe", pipePrefix + "test", "--base64", "invalid!"},
		{"send", "--pipe", pipePrefix + "test", "--text", strings.Repeat("x", maxInputBytes), "--enter"},
		{"send", "--pipe", pipePrefix + "test"},
		{"status", "--pipe", `\\other-host\pipe\agent-deck-native-test`},
		{"status", "--pipe", pipePrefix},
		{"status", "--pipe", pipePrefix + `nested\pipe`},
		{"status", "--pipe", pipePrefix + "中文"},
		{"status", "--pipe", pipePrefix + "invalid.dot"},
		{"status", "--pipe", pipePrefix + "test", "unexpected"},
		{"resize", "--pipe", pipePrefix + "test", "--cols", "80"},
		{"resize", "--pipe", pipePrefix + "test", "--cols", "1001", "--rows", "24"},
		{"capture", "--pipe", pipePrefix + "test", "--after", "-1"},
		{"capture", "--pipe", pipePrefix + "test", "--screen", "--json"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args[:min(len(args), 3)], "_"), func(t *testing.T) {
			if _, err := parseOptions(args, io.Discard); err == nil {
				t.Fatalf("accepted %#v", args)
			}
		})
	}
}

func TestMergedEnvPreservesInheritanceAndWindowsCase(t *testing.T) {
	base := []string{"Path=C:\\old", "KEEP=yes", "path=C:\\duplicate"}
	original := append([]string(nil), base...)
	got := mergedEnv(base, []string{"PATH=C:\\new", "AD_MARK=first", "ad_mark=中文"})
	want := []string{"KEEP=yes", "PATH=C:\\new", "ad_mark=中文"}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(base, original) {
		t.Fatalf("merged=%v; inherited=%v", got, base)
	}
	if got := mergedEnv(base, nil); got != nil {
		t.Fatalf("default must use nil inheritance, got %v", got)
	}
}

func TestHelpAndVersionAreExplicitlyExperimental(t *testing.T) {
	var output bytes.Buffer
	if _, err := parseOptions([]string{"--help"}, &output); !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "not the complete Agent Deck port") {
		t.Fatalf("help=%s; err=%v", output.String(), err)
	}
	output.Reset()
	if err := run([]string{"--version"}, &output, io.Discard); err != nil || !strings.Contains(output.String(), version) || !strings.Contains(output.String(), "experimental") {
		t.Fatalf("version=%s; err=%v", output.String(), err)
	}
}

func TestDetachForwardsOnlyBytesBeforeCtrlQ(t *testing.T) {
	data, detach := splitDetach([]byte("中文\x11trailing"))
	if !detach || string(data) != "中文" {
		t.Fatalf("forward=%q detach=%v", data, detach)
	}
	data, detach = splitDetach([]byte("普通输入\x03"))
	if detach || string(data) != "普通输入\x03" {
		t.Fatalf("forward=%q detach=%v", data, detach)
	}
}
