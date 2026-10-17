package main

// matrix/hardened: `agent-deck tasks` — the World Store task board from the
// CLI. World Store (personal-panel) owns the tasks; this only reads them and
// sends the user's explicit edits through World Store's local data entry. See
// internal/worldstore.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/worldstore"
)

func handleTasks(profile string, args []string) {
	if len(args) == 0 {
		handleTasksList(args)
		return
	}
	switch args[0] {
	case "list", "ls":
		handleTasksList(args[1:])
	case "show":
		handleTasksShow(args[1:])
	case "start", "pause", "resume", "done", "review", "reject":
		handleTasksAction(args[0], args[1:])
	case "accept": // matrix/hardened: accepting a review is completing the task
		handleTasksAction("done", args[1:])
	case "link", "unlink":
		handleTasksLink(profile, args[0] == "link", args[1:])
	case "launch":
		handleTasksLaunch(profile, args[1:])
	case "mark", "unmark":
		handleTasksMark(args[0] == "mark", args[1:])
	case "init", "init-links":
		handleTasksInit(args[1:])
	case "help", "--help", "-h":
		printTasksHelp()
	default:
		if strings.HasPrefix(args[0], "-") {
			handleTasksList(args)
			return
		}
		fmt.Fprintf(os.Stderr, "Error: unknown tasks command: %s\n", args[0])
		printTasksHelp()
		os.Exit(1)
	}
}

func printTasksHelp() {
	fmt.Println("Usage: agent-deck tasks <command> [options]")
	fmt.Println()
	fmt.Println("World Store task board. Tasks live only in World Store (personal-panel);")
	fmt.Println("agent-deck reads them through World Store's local data entry and writes")
	fmt.Println("only what you ask for. Configure with [worldstore] bridge = \"/abs/path/to/")
	fmt.Println("personal-panel/scripts/worldstore-ai.cjs\" in config.toml.")
	fmt.Println("Status changes follow World Store's flow: start creates a running")
	fmt.Println("execution, pause/resume act on it, done completes the task.")
	fmt.Println("In the TUI the board opens with alt+t.")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  list [--all] [--done] [--filter s]")
	fmt.Println("                              Board by column (default command): tasks whose")
	fmt.Println("                              执行分类 is the board category (default AI) or")
	fmt.Println("                              that have a linked session; --all shows every task")
	fmt.Println("  mark <task>                 Put a task on the board (set 执行分类)")
	fmt.Println("  unmark <task>               Take it off the board (clear 执行分类)")
	fmt.Println("  show <task>                 One task with plan, result and linked sessions")
	fmt.Println("  start <task>                Start work (World Store creates a running execution)")
	fmt.Println("  pause <task>                Pause the task's running execution")
	fmt.Println("  resume <task>               Resume the task's paused execution")
	fmt.Println("  done <task> [--result s]    Mark complete (also closes its executions)")
	fmt.Println("  review <task>               Hand over for review: finish its execution round, set 待验收")
	fmt.Println("  accept <task> [--result s]  Accept a review (same as done)")
	fmt.Println("  reject <task>               Send a review back to 待办 (the TUI's b also messages the session)")
	fmt.Println("  link <task> [session]       Link a session (default: current session)")
	fmt.Println("  unlink <task> [session]     Remove a session link")
	fmt.Println("  launch <task> [path]        Start a session on the task and link it")
	fmt.Println("  init --yes                  Create the 执行分类 and session-link fields in World Store")
	fmt.Println()
	fmt.Println("<task> is a task ID, a unique ID prefix (6+ chars), or an exact task name.")
}

func worldStoreClient() (*worldstore.Client, session.WorldStoreSettings, error) {
	cfg, err := session.LoadUserConfig()
	if err != nil {
		return nil, session.WorldStoreSettings{}, err
	}
	ws := cfg.WorldStore
	client, err := worldstore.New(worldstore.Config{Bridge: ws.Bridge, Node: ws.Node})
	return client, ws, err
}

// tasksSetup parses fs, builds the client and reads the board.
func tasksSetup(fs *flag.FlagSet, args []string, jsonOut *bool) (*CLIOutput, *worldstore.Client, session.WorldStoreSettings, *worldstore.Board) {
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	out := NewCLIOutput(*jsonOut, false)
	client, ws, err := worldStoreClient()
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	board, err := client.Board(context.Background())
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	return out, client, ws, board
}

func tasksErrorCode(err error) string {
	if code := worldstore.CodeOf(err); code != "" {
		return code
	}
	return ErrCodeInvalidOperation
}

func tasksErrorText(err error) string {
	switch worldstore.CodeOf(err) {
	case worldstore.CodeUnconfirmed, worldstore.CodeWriteUncertain:
		return err.Error() + "\nThe write may have been applied. Run `agent-deck tasks show <task>` to check; do not repeat it blindly."
	case worldstore.CodeRedlineReview, worldstore.CodeRedlineUnavailable:
		return err.Error() + "\nRed lines must be reviewed before starting; start this task in World Store."
	case worldstore.CodeBusy:
		return err.Error() + "\nNothing was written; re-read and try again."
	case worldstore.CodeUnavailable:
		return err.Error() + "\nIs World Store's data service set up? (personal-panel: npm run setup:headless)"
	}
	return err.Error()
}

func resolveTask(board *worldstore.Board, ref string) (*worldstore.Task, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("task is required")
	}
	if t := board.Find(ref); t != nil {
		return t, nil
	}
	var matches []*worldstore.Task
	for i := range board.Tasks {
		t := &board.Tasks[i]
		if t.Name == ref || (len(ref) >= 6 && strings.HasPrefix(t.ID, ref)) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no task matches %q", ref)
	case 1:
		return matches[0], nil
	}
	var ids []string
	for _, m := range matches {
		ids = append(ids, m.ID+"  "+m.Name)
	}
	return nil, fmt.Errorf("%q matches %d tasks; use the ID:\n  %s", ref, len(matches), strings.Join(ids, "\n  "))
}

func formatPlanDate(ms *int64) string { // Unix seconds
	if ms == nil {
		return ""
	}
	return time.Unix(*ms, 0).Local().Format("2006-01-02")
}

func handleTasksList(args []string) {
	fs := flag.NewFlagSet("tasks list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	all := fs.Bool("all", false, "Show every task, not only the board category")
	done := fs.Bool("done", false, "List the done column")
	filter := fs.String("filter", "", "Only tasks whose name, category or status contains this text")
	out, _, ws, board := tasksSetup(fs, args, jsonOut)
	category := ws.GetExecutionCategory()
	tasks := board.Tasks
	if !*all {
		tasks = nil
		for _, t := range board.Tasks {
			if worldstore.OnBoard(t, category) {
				tasks = append(tasks, t)
			}
		}
	}

	cols := worldstore.Columns(tasks, *filter)
	if *jsonOut {
		out.Success("", map[string]any{"success": true, "owner": board.Owner, "updatedAt": board.UpdatedAt, "category": category, "allTasks": *all, "execReady": board.ExecReady, "linksReady": board.LinksReady, "columns": cols})
		return
	}
	var sb strings.Builder
	for _, c := range cols {
		if c.Lane == "done" && !*done {
			fmt.Fprintf(&sb, "%s (%d, --done to show)\n\n", c.Title, len(c.Tasks)+c.Hidden)
			continue
		}
		fmt.Fprintf(&sb, "%s (%d)\n", c.Title, len(c.Tasks)+c.Hidden)
		for _, t := range c.Tasks {
			line := "  " + t.ID[:min(16, len(t.ID))] + "  " + t.Name
			var tags []string
			if c.Lane == "unknown" || c.Lane == "blocked" {
				tags = append(tags, t.Status)
			}
			if t.Execution != nil {
				tags = append(tags, "执行"+t.Execution.Status)
			}
			if t.Category != "" {
				tags = append(tags, t.Category)
			}
			if d := formatPlanDate(t.PlanDate); d != "" {
				tags = append(tags, "计划 "+d)
			}
			if n := len(t.AgentDeckLinks); n > 0 {
				tags = append(tags, fmt.Sprintf("%d session(s)", n))
			}
			if len(tags) > 0 {
				line += "  [" + strings.Join(tags, " · ") + "]"
			}
			sb.WriteString(line + "\n")
		}
		if c.Hidden > 0 {
			fmt.Fprintf(&sb, "  … %d older\n", c.Hidden)
		}
		sb.WriteString("\n")
	}
	if len(tasks) == 0 && !*all {
		fmt.Fprintf(&sb, "No tasks have 执行分类 = %s yet. See every task with --all, then: agent-deck tasks mark <task>\n", category)
	}
	fmt.Print(sb.String())
}

func handleTasksShow(args []string) {
	fs := flag.NewFlagSet("tasks show", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	out, client, _, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	full, err := client.Get(context.Background(), board.Owner, t.ID)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	// task.get carries no execution state; the board read does.
	full.Execution, full.Lane = t.Execution, t.Lane
	if *jsonOut {
		out.Success("", map[string]any{"success": true, "owner": board.Owner, "task": full})
		return
	}
	fmt.Printf("%s\n  id: %s\n  status: %s\n", full.Name, full.ID, full.Status)
	if full.Category != "" {
		fmt.Printf("  category: %s\n", full.Category)
	}
	if d := formatPlanDate(full.PlanDate); d != "" {
		fmt.Printf("  plan date: %s\n", d)
	}
	if e := full.Execution; e != nil {
		fmt.Printf("  execution: %s", e.Status)
		if e.StartedAt > 0 {
			fmt.Printf(" since %s", time.Unix(e.StartedAt, 0).Local().Format("2006-01-02 15:04"))
		}
		fmt.Println()
	}
	if full.ReadOnly {
		fmt.Println("  read-only (imported record)")
	}
	if full.PlanText != "" {
		fmt.Printf("  plan:\n    %s\n", strings.ReplaceAll(full.PlanText, "\n", "\n    "))
	}
	if full.ResultText != "" {
		fmt.Printf("  result:\n    %s\n", strings.ReplaceAll(full.ResultText, "\n", "\n    "))
	}
	switch {
	case full.LinksUnknown:
		fmt.Println("  sessions: stored links are in an unknown format; check the task in World Store")
	case len(full.AgentDeckLinks) > 0:
		fmt.Println("  sessions:")
		for _, l := range full.AgentDeckLinks {
			fmt.Printf("    %s  (%s, linked %s)\n", l.SessionID, l.DeviceID, time.Unix(l.LinkedAt, 0).Local().Format("2006-01-02 15:04"))
		}
	}
}

func handleTasksAction(kind string, args []string) {
	fs := flag.NewFlagSet("tasks "+kind, flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	result := fs.String("result", "", "Completion note for done (optional; empty keeps the stored result)")
	out, client, _, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	if t.ReadOnly {
		out.Error("imported task — read-only; edit it in World Store", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	ctx := context.Background()
	switch kind {
	case "start":
		err = client.Start(ctx, board.Owner, *t, worldstore.NewOperationID())
	case "pause":
		err = client.Pause(ctx, board.Owner, *t)
	case "resume":
		err = client.Resume(ctx, board.Owner, *t)
	case "done":
		err = client.Complete(ctx, board.Owner, *t, *result)
	case "review":
		err = client.SubmitForReview(ctx, board.Owner, *t)
	case "reject":
		if t.Status != worldstore.StatusReview {
			err = &worldstore.Error{Code: worldstore.CodeBadInput, Message: "only tasks waiting for review (待验收) can be sent back"}
		} else {
			err = client.SetStatus(ctx, board.Owner, *t, worldstore.StatusTodo)
		}
	}
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	// matrix/hardened: a decision on a dispatched task reaches Multica now.
	if t.Multica != nil && (kind == "done" || kind == "review" || kind == "reject") {
		client.TriggerMulticaSync()
	}
	out.Success(fmt.Sprintf("%s: %s", kind, t.Name), map[string]any{"success": true, "action": kind, "taskId": t.ID})
}

func handleTasksLink(profile string, link bool, args []string) {
	name := "tasks unlink"
	if link {
		name = "tasks link"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	out, client, ws, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	_, instances, _, err := loadSessionData(profile)
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	device := worldstore.DeviceID(ws.DeviceID)
	sessionID := fs.Arg(1)
	if inst, msg, code := ResolveSessionOrCurrent(sessionID, instances); inst != nil {
		sessionID = inst.ID
	} else if link || sessionID == "" || !t.LinkedTo(device, sessionID) {
		// Unlink may name a session that no longer exists in this deck.
		out.Error(msg, code)
		os.Exit(1)
	}
	var links []worldstore.Link
	if link {
		links, err = client.Link(context.Background(), board.Owner, *t, device, sessionID)
	} else {
		links, err = client.Unlink(context.Background(), board.Owner, *t, device, sessionID)
	}
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	verb := "unlinked from"
	if link {
		verb = "linked to"
	}
	out.Success(fmt.Sprintf("session %s %s %s", sessionID, verb, t.Name), map[string]any{"success": true, "taskId": t.ID, "sessionId": sessionID, "links": links})
}

func handleTasksLaunch(profile string, args []string) {
	fs := flag.NewFlagSet("tasks launch", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	tool := fs.String("c", "claude", "Tool to run")
	group := fs.String("g", "", "Group path")
	title := fs.String("t", "", "Session title (default: task name)")
	out, client, ws, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	if t.ReadOnly || t.Lane == "done" {
		out.Error("task is read-only or already done", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	full, err := client.Get(context.Background(), board.Owner, t.ID)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	exe, err := os.Executable()
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	if *title == "" {
		*title = t.Name
	}
	sessionID, err := worldstore.Launch(context.Background(), worldstore.LaunchRequest{
		Exe: exe, Profile: profile, Path: fs.Arg(1), Group: *group, Tool: *tool,
		Title: *title, Prompt: worldstore.LaunchPrompt(*full), TaskID: t.ID,
	})
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	if !board.LinksReady {
		out.Success(fmt.Sprintf("session %s started on %s (not linked: run `agent-deck tasks init --yes` first)", sessionID, t.Name), map[string]any{"success": true, "taskId": t.ID, "sessionId": sessionID, "linked": false})
		return
	}
	links, err := client.Link(context.Background(), board.Owner, *t, worldstore.DeviceID(ws.DeviceID), sessionID)
	if err != nil {
		out.Error(fmt.Sprintf("session %s started but linking failed: %s", sessionID, tasksErrorText(err)), tasksErrorCode(err))
		os.Exit(1)
	}
	out.Success(fmt.Sprintf("session %s started on %s and linked", sessionID, t.Name), map[string]any{"success": true, "taskId": t.ID, "sessionId": sessionID, "links": links})
}

func handleTasksMark(mark bool, args []string) {
	name := "tasks unmark"
	if mark {
		name = "tasks mark"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	out, client, ws, board := tasksSetup(fs, args, jsonOut)
	t, err := resolveTask(board, fs.Arg(0))
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(1)
	}
	if !board.ExecReady {
		out.Error("the 执行分类 field does not exist in World Store yet; run `agent-deck tasks init --yes` once", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	value := ""
	if mark {
		value = ws.GetExecutionCategory()
	}
	if t.ExecCategory == value {
		out.Success(fmt.Sprintf("%s: 执行分类 already %q", t.Name, value), map[string]any{"success": true, "taskId": t.ID, "changed": false})
		return
	}
	if err := client.SetExecCategory(context.Background(), board.Owner, *t, value); err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	out.Success(fmt.Sprintf("%s: 执行分类 %q → %q", t.Name, t.ExecCategory, value), map[string]any{"success": true, "taskId": t.ID, "changed": true, "execCategory": value})
}

// handleTasksInit sets up the two World Store task fields the board uses.
// Both change the World Store schema, so nothing is created without --yes.
func handleTasksInit(args []string) {
	fs := flag.NewFlagSet("tasks init", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output as JSON")
	yes := fs.Bool("yes", false, "Really create the fields (changes the World Store schema)")
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	out := NewCLIOutput(*jsonOut, false)
	client, _, err := worldStoreClient()
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	ctx := context.Background()
	exec, err := client.InspectExecCategory(ctx)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	links, err := client.InspectLinks(ctx)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	var missing []string
	if !exec.Ready {
		missing = append(missing, worldstore.ExecCategoryField)
	}
	if !links.Ready {
		missing = append(missing, worldstore.LinkField)
	}
	if len(missing) == 0 {
		out.Success("World Store task fields are ready", map[string]any{"success": true, "ready": true})
		return
	}
	if !*yes {
		out.Error(fmt.Sprintf("not set up yet: %s. Creating them adds relations to the 任务 type in World Store; re-run with --yes", strings.Join(missing, ", ")), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	board, err := client.Board(ctx)
	if err != nil {
		out.Error(tasksErrorText(err), tasksErrorCode(err))
		os.Exit(1)
	}
	// One at a time: each takes World Store's exclusive schema lease.
	if !exec.Ready {
		if err := client.InitializeExecCategory(ctx, board.Owner); err != nil {
			out.Error(worldstore.ExecCategoryField+": "+tasksErrorText(err), tasksErrorCode(err))
			os.Exit(1)
		}
	}
	if !links.Ready {
		if err := client.InitializeLinks(ctx, board.Owner); err != nil {
			out.Error(worldstore.LinkField+": "+tasksErrorText(err), tasksErrorCode(err))
			os.Exit(1)
		}
	}
	out.Success("created: "+strings.Join(missing, ", "), map[string]any{"success": true, "ready": true, "created": missing})
}
