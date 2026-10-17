package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// matrix/hardened: the host-local guard lives outside Claude account/scratch
// directories. A detached plan keeps denied launches from changing bindings
// or destroying the old pane. The wrapper consumes this one-use reservation.
const claudeLaunchTicketEnv = "CLAUDE_LAUNCH_GUARD_TICKET"

var claudeLaunchTicketPattern = regexp.MustCompile(`^[0-9a-f]{48}$`)

// matrix/hardened: production follows the host user database, never HOME or
// CLAUDE_CONFIG_DIR; tests inject only this resolver into private fixtures.
var claudeLaunchGuardHome = func() (string, error) {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" || !filepath.IsAbs(u.HomeDir) {
		return "", fmt.Errorf("invalid host home")
	}
	return u.HomeDir, nil
}

type claudeLaunchAdmission struct {
	command        string
	planned        *Instance
	embeddedPrompt bool
	ticketPath     string
	fresh          bool
}

type claudeLaunchGuardReceipt struct {
	Allowed    bool    `json:"allowed"`
	Ticket     string  `json:"ticket"`
	Reason     string  `json:"reason"`
	RetryAfter float64 `json:"retry_after"`
}

// matrix/hardened: cap helper output without waiting indefinitely on stdout
// inherited by a helper descendant; exec.WaitDelay closes remaining pipes.
type claudeGuardOutput struct {
	data     []byte
	exceeded bool
}

func (output *claudeGuardOutput) Write(p []byte) (int, error) {
	if len(output.data)+len(p) > 16*1024 {
		output.exceeded = true
		return 0, fmt.Errorf("Claude launch guard response too large")
	}
	output.data = append(output.data, p...)
	return len(p), nil
}

// matrix/hardened: copy launch inputs explicitly, never mutexes, conversation
// content, runtime ownership, or persisted state. Builders may mutate only this
// temporary instance until admission succeeds.
func (i *Instance) detachedClaudeLaunchInstance() *Instance {
	c := &Instance{
		ID: i.ID, Title: i.Title, ProjectPath: i.ProjectPath, GroupPath: i.GroupPath,
		ParentSessionID: i.ParentSessionID, ParentProjectPath: i.ParentProjectPath,
		IsConductor: i.IsConductor, NoTransitionNotify: i.NoTransitionNotify,
		TitleLocked: i.TitleLocked, AutoName: i.AutoName, Account: i.Account,
		WorktreePath: i.WorktreePath, WorktreeRepoRoot: i.WorktreeRepoRoot,
		WorktreeBranch: i.WorktreeBranch, WorktreeType: i.WorktreeType,
		MultiRepoEnabled: i.MultiRepoEnabled, MultiRepoTempDir: i.MultiRepoTempDir,
		AdditionalPaths: append([]string(nil), i.AdditionalPaths...),
		Command:         i.Command, Wrapper: i.Wrapper, Tool: i.Tool,
		ClaudeSessionID: i.ClaudeSessionID, ClaudeDetectedAt: i.ClaudeDetectedAt,
		Sandbox: i.Sandbox, SSHHost: i.SSHHost, SSHRemotePath: i.SSHRemotePath,
		Channels: append([]string(nil), i.Channels...), Plugins: append([]string(nil), i.Plugins...),
		PluginChannelLinkDisabled: i.PluginChannelLinkDisabled,
		InheritTelegramEnv:        i.InheritTelegramEnv,
		EnvFile:                   i.EnvFile, // matrix/hardened: launch --env-file
		IdentityInjectionDisabled: i.IdentityInjectionDisabled, ContextLevel: i.ContextLevel,
		WorkerScratchConfigDir: i.WorkerScratchConfigDir,
		IsForkAwaitingStart:    i.IsForkAwaitingStart, ForkStartCommand: i.ForkStartCommand,
		ExtraArgs: append([]string(nil), i.ExtraArgs...), StartupQuery: i.StartupQuery,
		ToolOptionsJSON: append(json.RawMessage(nil), i.ToolOptionsJSON...),
		restartEnv:      maps.Clone(i.restartEnv), crossHarnessLaunch: i.crossHarnessLaunch,
		claudeLaunchKind: "new",
	}
	i.claudeSessionIDsFromDiskScanMu.Lock()
	c.claudeSessionIDsFromDiskScan = maps.Clone(i.claudeSessionIDsFromDiskScan)
	i.claudeSessionIDsFromDiskScanMu.Unlock()
	return c
}

// matrix/hardened: nil means this machine has not opted into the external
// guard, or the process runs on another host. No helper is called for resumes.
func (i *Instance) planClaudeLaunchAdmission(fresh, restart bool, env map[string]string) (_ *claudeLaunchAdmission, returnErr error) {
	if !IsClaudeCompatible(i.Tool) || i.SSHHost != "" || i.IsSandboxed() {
		return nil, nil
	}
	home, err := claudeLaunchGuardHome()
	if err != nil {
		return nil, fmt.Errorf("Claude launch guard: resolve home: %w", err)
	}
	marker := filepath.Join(home, ".local", "lib", "claude-launch-guard", "enabled.json")
	info, err := os.Lstat(marker)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("Claude launch guard: enabled marker is unreadable or unsafe")
	}
	if f, openErr := os.Open(marker); openErr != nil {
		return nil, fmt.Errorf("Claude launch guard: read enabled marker: %w", openErr)
	} else {
		_ = f.Close()
	}
	// Lifecycle overrides appended after the builder can change or duplicate
	// its session selector. Refuse ambiguity before changing the live pane.
	if extraArgsSelectSession(i.ExtraArgs) {
		return nil, fmt.Errorf("Claude launch guard: lifecycle flags in ExtraArgs are unsupported; use structured session, resume, or fork options")
	}
	if i.hasEffectiveWrapper() {
		return nil, fmt.Errorf("Claude launch guard: opaque launch wrappers are unsupported; use the guarded Claude command directly")
	}
	for _, token := range i.ExtraArgs {
		flag, _, _ := strings.Cut(token, "=")
		switch flag {
		case "--bg", "--background", "--cloud", "--desktop", "--environment", "--remote-control":
			return nil, fmt.Errorf("Claude launch guard: unsupported background or remote dispatch option; use a local interactive session")
		}
	}
	if i.crossHarnessLaunch != nil {
		if _, present := i.crossHarnessLaunch.Environment[claudeLaunchTicketEnv]; present {
			return nil, fmt.Errorf("Claude launch guard: immutable launch plan overrides reserved admission environment")
		}
	}
	c := i.detachedClaudeLaunchInstance()
	envMarker := "__AGENTDECK_CLAUDE_ADMISSION_" + generateUUID() + "__; "
	c.claudeLaunchEnvironmentSetup = envMarker
	defer func() {
		// A refused detached plan never launched this private generation. Keep
		// the live instance's generation intact while removing only ours.
		if returnErr != nil && c.WorkerScratchConfigDir != i.WorkerScratchConfigDir {
			c.CleanupWorkerScratchConfigDir()
		}
	}()
	if env != nil {
		c.restartEnv = maps.Clone(env)
	}
	if fresh {
		c.ClaudeSessionID, c.ClaudeDetectedAt = "", time.Time{}
		c.IsForkAwaitingStart, c.ForkStartCommand = false, ""
		// A deliberate fresh restart must not replay a stored fork/selector.
		commandTokens := strings.Fields(c.Command)
		for n := range commandTokens {
			commandTokens[n] = strings.Trim(commandTokens[n], "'\"")
		}
		if extraArgsSelectSession(commandTokens) {
			c.Command = "claude"
		}
		opts := c.GetClaudeOptions()
		if opts == nil {
			cfg, _ := LoadUserConfig()
			opts = NewClaudeOptions(cfg)
		}
		opts.SessionMode, opts.ResumeSessionID = "new", ""
		if err := c.SetClaudeOptions(opts); err != nil {
			return nil, err
		}
	}
	if c.crossHarnessLaunch == nil && !c.IsForkAwaitingStart {
		c.prepareWorkerScratchConfigDirForSpawn()
	}
	plan := &claudeLaunchAdmission{planned: c, fresh: fresh}
	switch {
	case c.crossHarnessLaunch != nil:
		plan.command, plan.embeddedPrompt, err = crossHarnessPlanCommand(c, c.crossHarnessLaunch, "")
		if err != nil {
			return nil, err
		}
		c.claudeLaunchKind = "new"
	case c.IsForkAwaitingStart && !restart:
		plan.command = c.consumeForkStartCommand()
		// Stored fork commands were built before this admission existed. With
		// effective wrappers excluded, the native exec is a fixed builder token.
		if strings.Count(plan.command, "exec claude ") != 1 {
			return nil, fmt.Errorf("Claude launch guard: unsupported stored fork command")
		}
		plan.command = strings.Replace(plan.command, "exec claude ", envMarker+"exec claude ", 1)
		c.claudeLaunchKind = "fork"
	default:
		if restart {
			c.ensureClaudeSessionIDFromDiskForRestart()
			// Restore an owned orphan before deciding resume versus fresh, as
			// the original respawn path does. The live binding stays untouched.
			if c.Tool == "claude" && c.ClaudeSessionID != "" {
				if _, err := RestoreOrphanedConversationBackup(c, GetClaudeConfigDirForInstance(c)); err != nil {
					return nil, fmt.Errorf("Claude launch guard: restore owned conversation: %w", err)
				}
			}
		} else {
			c.ensureClaudeSessionIDFromDisk()
		}
		if c.ClaudeSessionID != "" {
			plan.command = c.buildClaudeResumeCommand()
		} else {
			plan.command = c.buildClaudeCommand(c.Command)
		}
	}
	if strings.Count(plan.command, envMarker) != 1 {
		return nil, fmt.Errorf("Claude launch guard: ambiguous admission environment placement")
	}
	// Never inherit a ticket after configured or one-shot env sources execute.
	if c.claudeLaunchKind == "resume" {
		plan.command = strings.Replace(plan.command, envMarker, "unset "+claudeLaunchTicketEnv+"; ", 1)
		return plan, nil
	}
	ticket, err := reserveClaudeLaunchAdmission(home, c.claudeLaunchKind, c.ClaudeSessionID)
	if err != nil {
		return nil, err
	}
	// Command strings are logged and persisted by spawn diagnostics. Keep the
	// opaque capability in a private one-use file, never a command literal.
	receiptDir := filepath.Join(home, ".local", "lib", "claude-launch-guard", "deck-receipts")
	if err := os.MkdirAll(receiptDir, 0o700); err != nil {
		return nil, fmt.Errorf("Claude launch guard: create receipt directory: %w", err)
	}
	if info, err := os.Lstat(receiptDir); err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("Claude launch guard: unsafe receipt directory")
	}
	f, err := os.CreateTemp(receiptDir, "admission-")
	if err != nil {
		return nil, fmt.Errorf("Claude launch guard: create receipt: %w", err)
	}
	plan.ticketPath = f.Name()
	_, writeErr := io.WriteString(f, ticket+"\n")
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		plan.cleanup()
		return nil, fmt.Errorf("Claude launch guard: write receipt failed")
	}
	pathExpr := shellescape.Quote(plan.ticketPath)
	setup := "unset " + claudeLaunchTicketEnv + "; if ! IFS= read -r " + claudeLaunchTicketEnv + " < " + pathExpr + "; then exit 78; fi; command rm -f -- " + pathExpr + "; export " + claudeLaunchTicketEnv + "; "
	plan.command = strings.Replace(plan.command, envMarker, setup, 1)
	return plan, nil
}

func (plan *claudeLaunchAdmission) cleanup() {
	if plan != nil && plan.ticketPath != "" {
		_ = os.Remove(plan.ticketPath)
	}
}

// matrix/hardened: invoke the fixed helper directly; account config and PATH
// cannot redirect admission. Bounded stdout and timeout fail closed.
func reserveClaudeLaunchAdmission(home, kind, sessionID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{"reserve", "--kind", kind, "--operation-id", generateUUID(), "--json"}
	if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(home, ".local", "bin", "claude-launch-guard"), args...)
	cmd.Env = os.Environ()
	cmd.Stderr = io.Discard // helper diagnostics may contain opaque receipts
	cmd.WaitDelay = time.Second
	output := &claudeGuardOutput{}
	cmd.Stdout = output
	waitErr := cmd.Run()
	var receipt claudeLaunchGuardReceipt
	if output.exceeded || json.Unmarshal(output.data, &receipt) != nil {
		if _, ok := waitErr.(*exec.Error); ok {
			return "", fmt.Errorf("Claude launch guard: helper unavailable: %w", waitErr)
		}
		if os.IsNotExist(waitErr) {
			return "", fmt.Errorf("Claude launch guard: helper unavailable: %w", waitErr)
		}
		return "", fmt.Errorf("Claude launch guard: invalid helper response")
	}
	if !receipt.Allowed {
		if exit, ok := waitErr.(*exec.ExitError); ok && exit.ExitCode() == 75 {
			return "", fmt.Errorf("Claude new-session launch denied: %s; retry after %.0f seconds", receipt.Reason, receipt.RetryAfter)
		}
		return "", fmt.Errorf("Claude launch guard: admission failed")
	}
	if waitErr != nil || !claudeLaunchTicketPattern.MatchString(receipt.Ticket) {
		return "", fmt.Errorf("Claude launch guard: invalid admission receipt")
	}
	return receipt.Ticket, nil
}

// matrix/hardened: apply only after reservation, retaining the exact UUID and
// fork state used to build the command that will consume its receipt.
func (i *Instance) applyClaudeLaunchAdmission(plan *claudeLaunchAdmission) string {
	c := plan.planned
	i.ClaudeSessionID, i.ClaudeDetectedAt = c.ClaudeSessionID, c.ClaudeDetectedAt
	i.IsForkAwaitingStart, i.ForkStartCommand = c.IsForkAwaitingStart, c.ForkStartCommand
	i.WorkerScratchConfigDir = c.WorkerScratchConfigDir
	i.Channels = append([]string(nil), c.Channels...)
	if plan.fresh {
		// Persist selector normalization so later starts cannot re-adopt the
		// old explicit UUID from a stale command or option.
		i.Command = c.Command
		i.ToolOptionsJSON = append(json.RawMessage(nil), c.ToolOptionsJSON...)
	}
	i.claudeSessionIDsFromDiskScanMu.Lock()
	i.claudeSessionIDsFromDiskScan = maps.Clone(c.claudeSessionIDsFromDiskScan)
	i.claudeSessionIDsFromDiskScanMu.Unlock()
	return plan.command
}
