// matrix/hardened: managed sessions share a private state-root scoped endpoint.
package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/nativeregistry"
	"github.com/google/uuid"
)

func isFleetCommand(command string) bool {
	switch command {
	case "serve", "ui", "manager-start", "list", "create", "start", "resume", "rename", "move", "remove", "shutdown":
		return true
	}
	return false
}

func defaultStateDirectory() (string, error) {
	if root := os.Getenv("LOCALAPPDATA"); runtime.GOOS == "windows" && root != "" {
		return filepath.Join(root, "AgentDeckNativeManager"), nil
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "agent-deck-native"), nil
}

func fleetPipeFor(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	identity, err := user.Current()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	sum := sha256.Sum256([]byte(identity.Uid + "\x00" + filepath.Clean(abs)))
	return pipePrefix + fmt.Sprintf("manager-%x", sum[:16]), nil
}

func parseFleetOptions(args []string, help io.Writer) (options, error) {
	o := options{Command: args[0]}
	flags := flag.NewFlagSet(o.Command, flag.ContinueOnError)
	flags.SetOutput(help)
	flags.StringVar(&o.Pipe, "pipe", "", "manager pipe (default: current user and state directory)")
	flags.StringVar(&o.StateDir, "state-dir", "", "dedicated private registry directory")
	// matrix/hardened: keep the validated v0.3 registry intact while accepting
	// the original session schema in an explicitly selected integration scope.
	flags.StringVar(&o.Store, "store", "prototype", "session store: prototype or statedb (original session schema)")
	if o.Command != "serve" && o.Command != "ui" {
		flags.BoolVar(&o.JSON, "json", false, "JSON response")
	}
	var env envFlags
	switch o.Command {
	case "serve":
		flags.BoolVar(&o.Background, "background", false, "internal detached child: readiness is provided over IPC")
	case "ui":
		flags.StringVar(&o.Cwd, "cwd", "", "default project directory")
		// matrix/hardened: an explicit launcher preference never changes saved sessions.
		flags.StringVar(&o.Tool, "tool", "shell", "default new-session tool: shell, codex or powershell")
		// matrix/hardened: this window selects a validated native executable only
		// for newly created Codex descriptions, never for already saved sessions.
		flags.StringVar(&o.Executable, "codex-executable", "", "absolute native Codex path for new Codex sessions")
	case "create":
		flags.StringVar(&o.Name, "name", "", "required session name")
		flags.StringVar(&o.Tool, "tool", "shell", "shell, codex, powershell or custom")
		flags.StringVar(&o.Group, "group", "", "session group (empty: default)")
		flags.StringVar(&o.Cwd, "cwd", "", "project working directory")
		flags.IntVar(&o.Cols, "cols", 120, "initial columns")
		flags.IntVar(&o.Rows, "rows", 30, "initial rows")
		flags.Var(&env, "env", "saved environment override NAME=VALUE")
		flags.BoolVar(&o.NoStart, "no-start", false, "save without launching")
	case "start", "resume", "rename", "move", "remove":
		flags.StringVar(&o.SessionID, "session", "", "required session UUID")
		flags.StringVar(&o.RunID, "run-id", "", "expected run UUID")
		if o.Command == "rename" {
			flags.StringVar(&o.Name, "name", "", "required new name")
		}
		if o.Command == "move" {
			flags.StringVar(&o.Group, "group", "", "required group; empty moves to default")
		}
	}
	flagArgs := args[1:]
	var childArgs []string
	if o.Command == "create" {
		for i, arg := range flagArgs {
			if arg == "--" {
				childArgs, flagArgs = flagArgs[i+1:], flagArgs[:i]
				break
			}
		}
	}
	if err := flags.Parse(flagArgs); err != nil {
		return o, err
	}
	if len(flags.Args()) != 0 {
		return o, errors.New("unexpected positional argument")
	}
	if o.Store != "prototype" && o.Store != "statedb" {
		return o, errors.New("--store must be prototype or statedb")
	}
	if o.Command == "ui" && o.Tool != "shell" && o.Tool != "codex" && o.Tool != "powershell" {
		return o, errors.New("ui --tool must be shell, codex or powershell")
	}
	if o.Command == "ui" && o.Executable != "" {
		// matrix/hardened: invalid explicit paths fail before manager startup.
		validated, err := validatedCodexExecutable(o.Executable)
		if err != nil {
			return o, err
		}
		o.Executable = validated
	}
	if o.StateDir == "" {
		dir, err := defaultStateDirectory()
		if err != nil {
			return o, err
		}
		o.StateDir = dir
		if o.Store == "statedb" {
			o.StateDir += "Shared"
		}
	}
	abs, err := filepath.Abs(o.StateDir)
	if err != nil {
		return o, err
	}
	o.StateDir = abs
	if o.Pipe == "" {
		o.Pipe, err = fleetPipeFor(o.StateDir)
		if err != nil {
			return o, err
		}
		if o.Store == "statedb" {
			o.Pipe += "-statedb"
		}
	}
	if err := validatePipe(o.Pipe); err != nil {
		return o, err
	}
	for _, id := range []string{o.SessionID, o.RunID} {
		if id != "" {
			if _, err := uuid.Parse(id); err != nil {
				return o, errors.New("session and run identifiers must be UUIDs")
			}
		}
	}
	if (o.Command == "start" || o.Command == "resume" || o.Command == "rename" || o.Command == "move" || o.Command == "remove") && o.SessionID == "" {
		return o, errors.New("--session is required")
	}
	if (o.Command == "create" || o.Command == "rename") && strings.TrimSpace(o.Name) == "" {
		return o, errors.New("--name is required")
	}
	if o.Command == "create" || o.Command == "move" {
		if err := nativeregistry.ValidateGroup(o.Group); err != nil {
			return o, err
		}
	}
	if o.Command == "move" {
		groupProvided := false
		flags.Visit(func(f *flag.Flag) { groupProvided = groupProvided || f.Name == "group" })
		if !groupProvided {
			return o, errors.New("move requires --group; an empty value selects the default group")
		}
	}
	if o.Command == "create" {
		if o.Tool != "shell" && o.Tool != "codex" && o.Tool != "powershell" && o.Tool != "custom" {
			return o, errors.New("tool must be shell, codex, powershell or custom")
		}
		if o.Cols < 1 || o.Cols > 500 || o.Rows < 1 || o.Rows > 300 {
			return o, errors.New("dimensions must be 1..500 by 1..300")
		}
		if len(childArgs) > 0 {
			o.Executable, o.Args = childArgs[0], childArgs[1:]
		}
		if o.Tool == "custom" && o.Executable == "" {
			return o, errors.New("custom tool requires '-- executable [args...]'")
		}
		o.Env = env
	}
	if o.Command == "create" || o.Command == "ui" {
		if o.Cwd == "" {
			o.Cwd, err = os.Getwd()
			if err != nil {
				return o, err
			}
		}
		o.Cwd, err = filepath.Abs(o.Cwd)
		if err != nil {
			return o, err
		}
	}
	return o, nil
}
