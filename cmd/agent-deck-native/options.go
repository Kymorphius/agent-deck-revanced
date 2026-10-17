package main

// matrix/hardened: keep the Windows terminal experiment isolated from the existing app.

import (
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/asheshgoplani/agent-deck/internal/winipc"
)

const pipePrefix = winipc.PipePrefix
const maxInputBytes = 64 << 10

type options struct {
	Command, Pipe, Cwd, Executable         string
	Args, Env                              []string
	Cols, Rows                             int
	After                                  uint64
	Screen, JSON                           bool
	Data                                   []byte
	StateDir, SessionID, RunID, Name, Tool string
	// matrix/hardened: opt-in original StateDB reuse has a distinct manager identity.
	Store      string
	Group      string
	Background bool
	NoStart    bool
}

type envFlags []string

func (e *envFlags) String() string { return strings.Join(*e, ",") }
func (e *envFlags) Set(value string) error {
	name, _, found := strings.Cut(value, "=")
	if !found || name == "" || strings.ContainsAny(name, "=\x00") || strings.ContainsRune(value, '\x00') {
		return errors.New("--env requires NAME=VALUE without NUL bytes")
	}
	*e = append(*e, value)
	return nil
}

func parseOptions(args []string, help io.Writer) (options, error) {
	var result options
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printHelp(help)
		return result, flag.ErrHelp
	}
	result.Command = args[0]
	if isFleetCommand(result.Command) {
		return parseFleetOptions(args, help)
	}
	switch result.Command {
	case "host", "status", "send", "capture", "resize", "stop", "attach":
	default:
		return result, fmt.Errorf("unknown command %q; use --help", result.Command)
	}
	flags := flag.NewFlagSet(result.Command, flag.ContinueOnError)
	flags.SetOutput(help)
	flags.StringVar(&result.Pipe, "pipe", "", "required local pipe: "+pipePrefix+"NAME")
	if result.Command != "host" {
		flags.StringVar(&result.SessionID, "session", "", "managed session UUID (omit for single host)")
		flags.StringVar(&result.RunID, "run-id", "", "expected managed process generation UUID")
	}
	if result.Command != "attach" && result.Command != "host" {
		flags.BoolVar(&result.JSON, "json", false, "emit one protocol response as JSON")
	}
	var env envFlags
	var text, encoded string
	var enter bool
	switch result.Command {
	case "host":
		flags.StringVar(&result.Cwd, "cwd", "", "child working directory (default: inherited)")
		flags.IntVar(&result.Cols, "cols", 120, "initial terminal columns (1..500)")
		flags.IntVar(&result.Rows, "rows", 30, "initial terminal rows (1..300)")
		flags.Var(&env, "env", "inherited environment override NAME=VALUE; repeatable")
	case "send":
		flags.StringVar(&text, "text", "", "UTF-8 input text; optional with --enter")
		flags.BoolVar(&enter, "enter", false, "append a carriage return (Enter)")
		flags.StringVar(&encoded, "base64", "", "exact bytes; mutually exclusive with --text/--enter")
	case "capture":
		flags.Uint64Var(&result.After, "after", 0, "raw output byte offset to resume after")
		flags.BoolVar(&result.Screen, "screen", false, "emit current plain-text screen instead of raw output")
	case "resize":
		flags.IntVar(&result.Cols, "cols", 0, "required terminal columns (1..500)")
		flags.IntVar(&result.Rows, "rows", 0, "required terminal rows (1..300)")
	}
	flags.Usage = func() {
		fmt.Fprintf(help, "Experimental Windows native lightweight edition.\nUsage: agent-deck-native %s [flags]", result.Command)
		if result.Command == "host" {
			fmt.Fprint(help, " -- executable [args...]")
		}
		fmt.Fprintln(help)
		flags.PrintDefaults()
	}
	flagArgs := args[1:]
	var childArgs []string
	if result.Command == "host" {
		for index, arg := range flagArgs {
			if arg == "--" {
				childArgs, flagArgs = flagArgs[index+1:], flagArgs[:index]
				break
			}
		}
	}
	if err := flags.Parse(flagArgs); err != nil {
		return result, err
	}
	if err := validatePipe(result.Pipe); err != nil {
		return result, err
	}
	for _, id := range []string{result.SessionID, result.RunID} {
		if id != "" {
			if _, err := uuid.Parse(id); err != nil {
				return result, errors.New("--session and --run-id must be UUIDs")
			}
		}
	}
	seen := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if result.Command == "host" {
		// matrix/hardened: preserve the executable and argv as separate values; no implicit shell.
		if len(flags.Args()) != 0 || len(childArgs) == 0 || childArgs[0] == "" {
			return result, errors.New("host requires '-- executable [args...]'")
		}
		result.Executable, result.Args = childArgs[0], childArgs[1:]
		for _, value := range append([]string{result.Executable, result.Cwd}, result.Args...) {
			if strings.ContainsRune(value, '\x00') {
				return result, errors.New("executable, arguments and cwd cannot contain NUL bytes")
			}
		}
		result.Env = env
	} else if len(flags.Args()) > 0 {
		return result, fmt.Errorf("%s does not accept positional arguments", result.Command)
	}
	if result.Command == "host" || result.Command == "resize" {
		if result.Cols < 1 || result.Cols > 500 || result.Rows < 1 || result.Rows > 300 {
			return result, errors.New("--cols must be 1..500 and --rows must be 1..300")
		}
	}
	if result.Command == "capture" && result.Screen && result.JSON {
		return result, errors.New("--screen and --json are mutually exclusive")
	}
	if result.Command == "send" {
		if seen["base64"] {
			if seen["text"] || seen["enter"] {
				return result, errors.New("--base64 is mutually exclusive with --text/--enter")
			}
			if base64.StdEncoding.DecodedLen(len(encoded)) > maxInputBytes+2 {
				return result, errors.New("input exceeds 64 KiB")
			}
			var err error
			result.Data, err = base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil {
				return result, fmt.Errorf("invalid --base64: %w", err)
			}
		} else {
			result.Data = []byte(text)
			if enter {
				result.Data = append(result.Data, '\r')
			}
		}
		if len(result.Data) == 0 {
			return result, errors.New("send requires nonempty --text, --enter, or --base64")
		}
		if len(result.Data) > maxInputBytes {
			return result, errors.New("input exceeds 64 KiB")
		}
	}
	return result, nil
}

func validatePipe(path string) error {
	if !strings.HasPrefix(path, pipePrefix) {
		return fmt.Errorf("--pipe is required and must start with %s", pipePrefix)
	}
	name := path[len(pipePrefix):]
	if len(name) == 0 || len(name) > 128 {
		return errors.New("pipe name must contain 1..128 letters, numbers, '_' or '-'")
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return errors.New("pipe name must contain only letters, numbers, '_' or '-'")
		}
	}
	return nil
}

// matrix/hardened: overriding one variable must not discard PATH or the inherited agent configuration.
func mergedEnv(base, overrides []string) []string {
	if len(overrides) == 0 {
		return nil
	}
	result := append([]string(nil), base...)
	for _, override := range overrides {
		name, _, _ := strings.Cut(override, "=")
		filtered := result[:0]
		for _, current := range result {
			currentName, _, _ := strings.Cut(current, "=")
			if !strings.EqualFold(name, currentName) {
				filtered = append(filtered, current)
			}
		}
		result = append(filtered, override)
	}
	return result
}

func printHelp(output io.Writer) {
	// matrix/hardened: show the actual lightweight build version and launcher preference.
	fmt.Fprintln(output, "agent-deck-native", version)
	fmt.Fprintln(output, `Experimental standalone Windows lightweight edition; this is not the complete Agent Deck port.

Usage:
  agent-deck-native --version
  agent-deck-native serve [--state-dir DIR] [--pipe NAME]
  agent-deck-native ui [--state-dir DIR] [--pipe NAME] [--cwd DIR] [--tool shell|codex|powershell] [--codex-executable PATH]
  agent-deck-native manager-start [--state-dir DIR] [--pipe NAME] [--json]
  agent-deck-native list [--state-dir DIR] [--pipe NAME] [--json]

  Managed commands accept --store statedb to reuse the original session schema
  in a dedicated integration directory; the prototype store remains the default.
  agent-deck-native create --name NAME [--tool shell|codex|powershell|custom] [--cwd DIR] [--group NAME] [--no-start] [--pipe NAME] [-- executable args...]
  agent-deck-native start --session UUID [--pipe NAME] [--json]
  agent-deck-native resume --session UUID [--run-id UUID] [--pipe NAME] [--json]
  agent-deck-native rename --session UUID --name NAME [--pipe NAME]
  agent-deck-native move --session UUID --group NAME [--run-id UUID] [--pipe NAME]
  agent-deck-native remove --session UUID [--run-id UUID] [--pipe NAME]
  agent-deck-native shutdown [--pipe NAME]
  agent-deck-native host --pipe NAME [--cwd DIR] [--cols N --rows N] [--env NAME=VALUE] -- executable [args...]
  agent-deck-native status --pipe NAME [--json]
  agent-deck-native send --pipe NAME [--text TEXT] [--enter] [--json]
  agent-deck-native send --pipe NAME --base64 BYTES [--json]
  agent-deck-native capture --pipe NAME [--after OFFSET] [--screen | --json]
  agent-deck-native resize --pipe NAME --cols N --rows N [--json]
  agent-deck-native stop --pipe NAME [--json]
  agent-deck-native attach --pipe NAME

NAME is a full local path such as \\.\pipe\agent-deck-native-demo.
host runs in the foreground and owns one ConPTY child. Launch it independently
to keep the child alive when a client closes. Ctrl+Q detaches attach only.
No service, scheduled task, hook, update check or telemetry is installed.
serve owns multiple sessions and a private local registry; ui exits independently.
ui (or no arguments) starts or reuses a verified detached manager automatically.
Use / to search, Tab to filter groups, and g to move a selected session.
For stopped/exited Codex, Enter starts a new dialog; h (or resume) opens Codex's
history picker for the saved working directory. Running sessions must stop first.
ui --codex-executable selects a native Codex executable for new sessions only.
Managed attach/send/capture/resize/stop accept --session UUID and --run-id UUID.
Use COMMAND --help for flags.`)
}
