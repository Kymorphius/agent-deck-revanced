package main

// matrix/hardened: the prototype deliberately avoids the tmux-backed application import tree.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/terminalhost"
	"github.com/asheshgoplani/agent-deck/internal/winipc"
)

// matrix/hardened: the lightweight native release remains separate from the original TUI.
const version = "0.6.0+matrix.lite"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agent-deck-native:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		args = []string{"ui"}
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		_, err := fmt.Fprintln(stdout, "agent-deck-native", version, "(experimental Windows native lightweight edition)")
		return err
	}
	config, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if isFleetCommand(config.Command) {
		return runFleet(config, stdout, stderr)
	}
	switch config.Command {
	case "host":
		return runHost(config, stdout)
	case "attach":
		return runAttach(config, os.Stdin, os.Stdout, stderr)
	}
	request := terminalhost.Request{Op: config.Command, Data: config.Data, After: config.After, Cols: config.Cols, Rows: config.Rows, SessionID: config.SessionID, RunID: config.RunID}
	if config.SessionID != "" && config.RunID == "" && (config.Command == "send" || config.Command == "resize" || config.Command == "stop") {
		info, err := lookupSession(context.Background(), config.Pipe, config.SessionID)
		if err != nil {
			return err
		}
		request.RunID = info.RunID
	}
	response, err := callHost(context.Background(), config.Pipe, request)
	if err != nil {
		return err
	}
	if config.JSON {
		return json.NewEncoder(stdout).Encode(response)
	}
	switch config.Command {
	case "capture":
		if config.Screen {
			_, err := fmt.Fprintln(stdout, response.Screen)
			return err
		}
		targetHead := response.HeadOffset
		for {
			if response.Truncated {
				fmt.Fprintf(stderr, "output before offset %d is no longer retained\n", response.StartOffset)
			}
			if _, err := stdout.Write(response.Output); err != nil {
				return err
			}
			if response.EndOffset >= targetHead {
				return nil
			}
			if response.EndOffset <= request.After {
				return errors.New("host output offset did not advance")
			}
			request.After = response.EndOffset
			response, err = callHost(context.Background(), config.Pipe, request)
			if err != nil {
				return err
			}
		}
	case "status":
		state := "exited"
		if response.Running {
			state = "running"
		}
		fmt.Fprintf(stdout, "host_pid=%d child_pid=%d state=%s size=%dx%d output_head=%d", response.HostPID, response.PID, state, response.Cols, response.Rows, response.HeadOffset)
		if response.ExitCode != nil {
			fmt.Fprintf(stdout, " exit_code=%d", *response.ExitCode)
		}
		_, err := fmt.Fprintln(stdout)
		return err
	default:
		_, err := fmt.Fprintf(stdout, "%s: acknowledged\n", config.Command)
		return err
	}
}

func callHost(ctx context.Context, pipe string, request terminalhost.Request) (terminalhost.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	connection, err := winipc.Dial(ctx, pipe)
	if err != nil {
		return terminalhost.Response{}, fmt.Errorf("connect to terminal host: %w", err)
	}
	defer connection.Close()
	response, err := terminalhost.Call(ctx, connection, request)
	if err != nil {
		return response, err
	}
	if !response.OK {
		return response, fmt.Errorf("host rejected %s: %s", request.Op, response.Error)
	}
	return response, nil
}
