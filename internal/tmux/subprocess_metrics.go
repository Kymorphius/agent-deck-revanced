package tmux

import (
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"
)

var subprocessStarts atomic.Int64

// SubprocessStarts counts native tmux commands started through this package.
// Output calls are counted on return; Run and asynchronous calls at Start.
// Deltas across a status pass include concurrent work in the same process.
func SubprocessStarts() int64 { return subprocessStarts.Load() }

// matrix/hardened: fixed, process-local counters reveal query cost without
// retaining arguments, paths, session names, output, or per-command records.
// A chained tmux invocation counts once under the fixed batch category.
var subprocessCategoryNames = [...]string{
	"other", "capture-pane", "list-panes", "has-session", "display-message",
	"list-sessions", "list-windows", "list-clients", "show-environment",
	"show-options", "new-session", "respawn-pane", "attach-session", "send-keys",
	"pipe-pane", "kill-session", "set-environment", "set-option", "source-file",
	"resize-window", "select-window", "bind-key", "unbind-key", "clear-history",
	"batch", "version",
}

type subprocessCategoryCounters struct {
	started          atomic.Uint64
	asyncStarted     atomic.Uint64
	completed        atomic.Uint64
	errors           atomic.Uint64
	totalWallNS      atomic.Uint64
	childCPUNS       atomic.Uint64
	childCPUObserved atomic.Uint64
}

var subprocessCategories [len(subprocessCategoryNames)]subprocessCategoryCounters

// SubprocessCategoryMetrics are lifetime totals for one fixed command category.
// matrix/hardened: completed/errors/timing cover only wrappers that own Wait;
// a failed Start is excluded, while a launched command's nonzero exit is an error.
type SubprocessCategoryMetrics struct {
	Category         string `json:"category"`
	Started          uint64 `json:"started"`
	AsyncStarted     uint64 `json:"async_started"`
	Completed        uint64 `json:"completed"`
	Errors           uint64 `json:"errors"`
	TotalWallNS      uint64 `json:"total_wall_ns"`
	ChildCPUNS       uint64 `json:"child_cpu_ns"`
	ChildCPUObserved uint64 `json:"child_cpu_observed"`
}

// SubprocessMetricsSnapshot describes counter coverage as well as the totals.
// matrix/hardened: atomic fields are individually monotonic, not one transaction;
// concurrent commands may finish while a snapshot reads the remaining fields.
type SubprocessMetricsSnapshot struct {
	TimingCoverage string                      `json:"timing_coverage"`
	ChildCPUScope  string                      `json:"child_cpu_scope"`
	Categories     []SubprocessCategoryMetrics `json:"categories"`
}

// SubprocessMetrics returns bounded diagnostics for native tmux commands only.
// matrix/hardened: Output/CombinedOutput starts become visible on return because
// exec.Cmd owns their Start; commandStart records no completion even if its caller
// later calls Wait directly. Child CPU is the waited client process's user+system
// time, excluding the parent fork cost and a preexisting tmux server's CPU.
func SubprocessMetrics() SubprocessMetricsSnapshot {
	result := SubprocessMetricsSnapshot{
		TimingCoverage: "Run/Output/CombinedOutput own wait; Start is start-only",
		ChildCPUScope:  "waited tmux client user+system; excludes parent and preexisting server",
		Categories:     make([]SubprocessCategoryMetrics, len(subprocessCategoryNames)),
	}
	for i, name := range subprocessCategoryNames {
		c := &subprocessCategories[i]
		result.Categories[i] = SubprocessCategoryMetrics{
			Category: name, Started: c.started.Load(), AsyncStarted: c.asyncStarted.Load(),
			Completed: c.completed.Load(), Errors: c.errors.Load(),
			TotalWallNS: c.totalWallNS.Load(), ChildCPUNS: c.childCPUNS.Load(),
			ChildCPUObserved: c.childCPUObserved.Load(),
		}
	}
	return result
}

// matrix/hardened: parse only the global-option prefix. In particular, capture's
// own -S history bound must not be mistaken for the global socket-path option.
// Unknown options/commands stay in a fixed "other" bucket; argv never leaves here.
func subprocessCommandCategory(cmd *exec.Cmd) int {
	if filepath.Base(cmd.Path) != "tmux" {
		return -1
	}
	args := cmd.Args
	if len(args) > 0 {
		args = args[1:]
	}
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		if arg == "--" {
			if len(args) == 0 {
				return 0
			}
			return subprocessInvocationCategory(args[0], args[1:])
		}
		if len(arg) == 0 || arg[0] != '-' || arg == "-" {
			return subprocessInvocationCategory(arg, args)
		}
		for i := 1; i < len(arg); i++ {
			switch arg[i] {
			case '2', 'C', 'D', 'h', 'l', 'N', 'u', 'v':
				// Flag bundles such as -CC or -uv contain no values.
			case 'V':
				return len(subprocessCategoryNames) - 1
			case 'c', 'f', 'L', 'S', 'T':
				// A value is attached to this flag or in the next argv element.
				if i+1 == len(arg) {
					if len(args) == 0 {
						return 0
					}
					args = args[1:]
				}
				i = len(arg)
			default:
				return 0
			}
		}
	}
	return 0
}

// matrix/hardened: a standalone tmux command separator marks one subprocess
// containing multiple commands, not several launches. Global-option values
// have already been consumed; escaped separators remain ordinary arguments.
func subprocessInvocationCategory(command string, args []string) int {
	for _, arg := range args {
		if arg == ";" {
			return subprocessCategory("batch")
		}
	}
	return subprocessCategory(command)
}

func subprocessCategory(command string) int {
	// matrix/hardened: tmux's fixed aliases share their canonical categories.
	switch command {
	case "bind":
		command = "bind-key"
	case "unbind":
		command = "unbind-key"
	case "set", "set-window-option", "setw":
		command = "set-option"
	}
	for i, name := range subprocessCategoryNames {
		if command == name {
			return i
		}
	}
	return 0
}

func observeCommandStart(category int, async bool) {
	if category < 0 {
		return
	}
	subprocessStarts.Add(1)
	c := &subprocessCategories[category]
	c.started.Add(1)
	if async {
		c.asyncStarted.Add(1)
	}
}

func observeCommandCompletion(cmd *exec.Cmd, category int, startedAt time.Time, err error) {
	if category < 0 {
		return
	}
	c := &subprocessCategories[category]
	c.completed.Add(1)
	if err != nil {
		c.errors.Add(1)
	}
	c.totalWallNS.Add(uint64(time.Since(startedAt)))
	if cmd.ProcessState != nil {
		c.childCPUNS.Add(uint64(cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()))
		c.childCPUObserved.Add(1)
	}
}

func commandRun(cmd *exec.Cmd) error {
	category := subprocessCommandCategory(cmd)
	var startedAt time.Time
	if category >= 0 {
		startedAt = time.Now()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	observeCommandStart(category, false)
	err := cmd.Wait()
	observeCommandCompletion(cmd, category, startedAt, err)
	return err
}

func commandStart(cmd *exec.Cmd) error {
	category := subprocessCommandCategory(cmd)
	err := cmd.Start()
	if err == nil {
		observeCommandStart(category, true)
	}
	return err
}

func commandOutput(cmd *exec.Cmd) ([]byte, error) {
	category := subprocessCommandCategory(cmd)
	// matrix/hardened: an already-started Cmd can only return a misuse error;
	// do not recount its old Process or ProcessState as a new execution.
	observe := category >= 0 && cmd.Process == nil
	var startedAt time.Time
	if observe {
		startedAt = time.Now()
	}
	out, err := cmd.Output()
	if observe && cmd.Process != nil {
		observeCommandStart(category, false)
		observeCommandCompletion(cmd, category, startedAt, err)
	}
	return out, err
}

func commandCombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	category := subprocessCommandCategory(cmd)
	observe := category >= 0 && cmd.Process == nil
	var startedAt time.Time
	if observe {
		startedAt = time.Now()
	}
	out, err := cmd.CombinedOutput()
	if observe && cmd.Process != nil {
		observeCommandStart(category, false)
		observeCommandCompletion(cmd, category, startedAt, err)
	}
	return out, err
}
