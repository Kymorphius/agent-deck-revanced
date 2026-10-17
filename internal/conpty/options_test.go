// matrix/hardened: validate native launch boundaries on every platform before
// Windows-only integration tests create an isolated owned process tree.
package conpty

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestOptionsValidation(t *testing.T) {
	for name, options := range map[string]Options{
		"empty executable":           {},
		"NUL executable":             {Executable: "cmd\x00.exe"},
		"quoted executable":          {Executable: `cmd".exe`},
		"NUL argument":               {Executable: "cmd.exe", Args: []string{"x\x00y"}},
		"invalid UTF8":               {Executable: "cmd.exe", Args: []string{string([]byte{0xff})}},
		"NUL directory":              {Executable: "cmd.exe", Dir: "x\x00y"},
		"missing environment name":   {Executable: "cmd.exe", Env: []string{"=secret"}},
		"missing environment equals": {Executable: "cmd.exe", Env: []string{"PATH"}},
		"negative columns":           {Executable: "cmd.exe", Cols: -1},
		"oversized rows":             {Executable: "cmd.exe", Rows: 32768},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeOptions(options); err == nil {
				t.Fatal("invalid options accepted")
			}
		})
	}
	args, env := []string{"中文"}, []string{"PATH=test", "=C:=C:\\test"}
	got, err := normalizeOptions(Options{Executable: "cmd.exe", Args: args, Env: env})
	if err != nil || got.Cols != 80 || got.Rows != 24 {
		t.Fatalf("defaults: %+v, %v", got, err)
	}
	args[0], env[0] = "changed", "PATH=changed"
	if got.Args[0] != "中文" || got.Env[0] != "PATH=test" {
		t.Fatal("launch retained caller-owned slices")
	}
	nilEnv, _ := normalizeOptions(Options{Executable: "cmd.exe"})
	emptyEnv, _ := normalizeOptions(Options{Executable: "cmd.exe", Env: []string{}})
	if nilEnv.Env != nil || emptyEnv.Env == nil {
		t.Fatal("inherit and empty environment were conflated")
	}
}

func TestQuoteWindowsArg(t *testing.T) {
	for _, test := range []struct{ arg, want string }{
		{"", `""`},
		{"plain", "plain"},
		{"中文", "中文"},
		{`C:\plain\`, `C:\plain\`},
		{"two words", `"two words"`},
		{"with\ttab", "\"with\ttab\""},
		{`say "hello"`, `"say \"hello\""`},
		{`C:\with space\`, `"C:\with space\\"`},
		{`before\"after`, `"before\\\"after"`},
		{`$HOME & | ; %PATH%`, `"$HOME & | ; %PATH%"`},
	} {
		if got := quoteWindowsArg(test.arg); got != test.want {
			t.Errorf("quote %q = %q, want %q", test.arg, got, test.want)
		}
	}
	if _, err := commandLine("x.exe", []string{strings.Repeat("😀", 16384)}); err == nil {
		t.Fatal("oversized UTF-16 command line accepted")
	}
}

func TestEnvironmentBlock(t *testing.T) {
	block, err := environmentBlock([]string{"Path=first", "Z=last", "PATH=second", "a=中文", "=C:=C:\\space path"})
	if err != nil {
		t.Fatal(err)
	}
	want := "=C:=C:\\space path\x00a=中文\x00PATH=second\x00Z=last\x00\x00"
	if got := string(utf16.Decode(block)); got != want {
		t.Fatalf("block = %q, want %q", got, want)
	}
	empty, err := environmentBlock([]string{})
	if err != nil || !reflect.DeepEqual(empty, []uint16{0, 0}) {
		t.Fatalf("empty block = %v, %v", empty, err)
	}
}

func TestOutputBufferPreservesFinalBytes(t *testing.T) {
	buffer := newOutputBuffer(32)
	readDone := make(chan string, 1)
	go func() {
		data, err := io.ReadAll(buffer)
		if err != nil {
			readDone <- "unexpected error: " + err.Error()
			return
		}
		readDone <- string(data)
	}()
	if err := buffer.append([]byte("中文\r\n")); err != nil {
		t.Fatal(err)
	}
	buffer.finish(nil)
	if got := <-readDone; got != "中文\r\n" {
		t.Fatalf("final output = %q", got)
	}
}

func TestOutputBufferBacklogFailsExplicitly(t *testing.T) {
	buffer := newOutputBuffer(4)
	if err := buffer.append([]byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.append([]byte("overflow")); !errors.Is(err, ErrOutputBacklog) {
		t.Fatalf("overflow = %v", err)
	}
	buffer.finish(nil)
	data, err := io.ReadAll(buffer)
	if string(data) != "keep" || !errors.Is(err, ErrOutputBacklog) {
		t.Fatalf("overflow result = %q, %v", data, err)
	}
}
