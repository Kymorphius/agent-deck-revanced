package main

// matrix/hardened: `agent-deck session adopt` takes over a Claude Code
// process already running in a tmux session agent-deck did not create,
// without interrupting it (see internal/session/matrix_adopt.go).

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func handleSessionAdopt(profile string, args []string) {
	fs := flag.NewFlagSet("session adopt", flag.ExitOnError)
	list := fs.Bool("list", false, "List the Claude processes running in tmux and whether each can be adopted")
	socket := fs.String("socket", "", "tmux server (-L name) to look on; default: the default server and [tmux].socket_name")
	title := fs.String("title", "", "Session title (default: the tmux session name)")
	titleShort := fs.String("t", "", "Session title (short)")
	group := fs.String("group", "", "Group path (default: derived from the directory)")
	groupShort := fs.String("g", "", "Group path (short)")
	jsonOutput := fs.Bool("json", false, "Output as JSON")
	fs.Usage = func() {
		fmt.Println("Usage: agent-deck session adopt <tmux-session> [-t title] [-g group] [--socket name] [--json]")
		fmt.Println("       agent-deck session adopt --list [--socket name] [--json]")
		fmt.Println()
		fmt.Println("Take over a Claude Code process already running in a tmux session that")
		fmt.Println("agent-deck did not create, without interrupting it: registers a claude")
		fmt.Println("session bound to that conversation and renames the tmux session to the")
		fmt.Println("name agent-deck manages. The tmux session must hold only that one pane,")
		fmt.Println("and no other Claude process may have the same conversation open.")
		fmt.Println()
		fmt.Println("After adopting, removing the session in agent-deck kills that tmux session")
		fmt.Println("and the Claude process in it, like any other agent-deck session.")
		fmt.Println()
		fmt.Println("Options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(normalizeArgs(fs, args)); err != nil {
		os.Exit(1)
	}
	out := NewCLIOutput(*jsonOutput, false)
	cfg, _ := session.LoadUserConfig()
	sockets := session.AdoptSockets(cfg, *socket)

	if *list {
		if fs.NArg() != 0 {
			fs.Usage()
			os.Exit(2)
		}
		_, instances, _, err := loadSessionData(profile)
		if err != nil {
			out.Error(err.Error(), ErrCodeNotFound)
			os.Exit(1)
		}
		cands, err := session.FindAdoptCandidates(cfg, instances, sockets)
		if err != nil {
			out.Error(err.Error(), ErrCodeInvalidOperation)
			os.Exit(1)
		}
		if cands == nil {
			cands = []session.AdoptCandidate{}
		}
		out.Print(formatAdoptList(cands), map[string]any{"success": true, "sockets": sockets, "candidates": cands})
		return
	}
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}

	// Same registration window as `add`: hold the lock across read-decide-write
	// and decide from a list re-read under it.
	regLock, err := session.AcquireRegistrationLock(profile)
	if err != nil {
		out.Error(fmt.Sprintf("failed to acquire session registration lock: %v", err), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	releaseRegistration := func() {
		if regLock != nil {
			regLock.Release()
			regLock = nil
		}
	}
	defer releaseRegistration()
	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		out.Error(fmt.Sprintf("failed to initialize storage: %v", err), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	instances, groups, err := reloadForRegistration(storage)
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}

	cands, err := session.FindAdoptCandidates(cfg, instances, sockets)
	if err != nil {
		out.Error(err.Error(), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	cand, err := session.SelectAdoptCandidate(cands, fs.Arg(0), *socket)
	if err != nil {
		out.Error(err.Error(), ErrCodeNotFound)
		os.Exit(2)
	}
	if !cand.Adoptable {
		out.ErrorWithData(fmt.Sprintf("cannot adopt tmux session %q: %s", cand.TmuxSession, cand.Reason), ErrCodeInvalidOperation, map[string]any{"candidate": cand})
		os.Exit(1)
	}

	sessionTitle := mergeFlags(*title, *titleShort)
	userProvidedTitle := sessionTitle != ""
	if !userProvidedTitle {
		sessionTitle = cand.TmuxSession
	}
	decision := decideAddTitle(instances, sessionTitle, localLocation(cand.Path), userProvidedTitle)
	if decision.Duplicate != nil {
		msg, code := decision.DuplicateError()
		out.ErrorWithData(msg, code, decision.DuplicateJSONFields())
		os.Exit(1)
	}
	sessionTitle = decision.Title

	var inst *session.Instance
	if g := mergeFlags(*group, *groupShort); g != "" {
		inst = session.NewInstanceWithGroup(sessionTitle, cand.Path, g)
	} else {
		inst = session.NewInstance(sessionTitle, cand.Path)
	}
	inst.Tool = "claude"
	inst.Command = "claude"
	if session.NormalizeAdoptSocket(inst.TmuxSocketName) != cand.Socket {
		inst.TmuxSocketName = cand.Socket
		if ts := inst.GetTmuxSession(); ts != nil {
			ts.SocketName = cand.Socket
		}
	}
	inst.ClaudeSessionID = cand.SessionID
	session.MarkClaudeSessionIDVerified(inst) // the running process names it
	inst.ClaudeDetectedAt = time.Now()
	opts := session.NewClaudeOptions(cfg)
	opts.SessionMode = "resume"
	opts.ResumeSessionID = cand.SessionID
	if err := inst.SetClaudeOptions(opts); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to set resume options: %v\n", err)
	}
	ts := inst.GetTmuxSession()
	if ts == nil || ts.Name == "" {
		out.Error("the new session has no tmux name", ErrCodeInvalidOperation)
		os.Exit(1)
	}
	newName := ts.Name

	// Rename first: if it fails nothing has been written. If the save then
	// fails, the rename is undone so the process is back where it was.
	if err := session.RenameAdoptedTmuxSession(cand.Socket, cand.TmuxSession, newName); err != nil {
		code := ErrCodeInvalidOperation
		if errors.Is(err, session.ErrAdoptNameTaken) {
			code = ErrCodeAlreadyExists
		}
		out.Error(fmt.Sprintf("rename tmux session %q: %v", cand.TmuxSession, err), code)
		os.Exit(1)
	}
	instances = append(instances, inst)
	groupTree := session.NewGroupTreeWithGroups(instances, groups)
	if cfg != nil {
		session.ReconcileDeclarativeGroups(groupTree, cfg)
		groupTree.DefaultMaxConcurrent = cfg.GroupDefaults.MaxConcurrent
	}
	if inst.GroupPath != "" {
		groupTree.CreateGroupPath(inst.GroupPath)
	}
	if err := storage.SaveWithGroups(instances, groupTree); err != nil {
		if rerr := session.RenameAdoptedTmuxSession(cand.Socket, newName, cand.TmuxSession); rerr != nil {
			err = fmt.Errorf("%v (and renaming tmux session %q back to %q failed: %v)", err, newName, cand.TmuxSession, rerr)
		}
		out.Error(fmt.Sprintf("failed to save session: %v", err), ErrCodeInvalidOperation)
		os.Exit(1)
	}
	releaseRegistration()
	if db := storage.GetDB(); db != nil {
		if err := db.UpsertSessionLink(inst.ID, "claude", cand.SessionID, "", true); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: session link: %v\n", err)
		}
	}

	out.Success(fmt.Sprintf("Adopted tmux session %q as %q (group %q); Claude pid %d keeps running in tmux session %s on server %s", cand.TmuxSession, inst.Title, inst.GroupPath, cand.ClaudePID, newName, cand.Socket),
		map[string]any{
			"success": true, "id": inst.ID, "title": inst.Title, "group": inst.GroupPath, "path": inst.ProjectPath,
			"claude_session_id": cand.SessionID, "claude_pid": cand.ClaudePID,
			"socket": cand.Socket, "tmux_session": newName, "previous_tmux_session": cand.TmuxSession,
		})
}

func formatAdoptList(cands []session.AdoptCandidate) string {
	if len(cands) == 0 {
		return "No Claude processes found in tmux.\n"
	}
	var b strings.Builder
	for _, c := range cands {
		mark := "✓"
		if !c.Adoptable {
			mark = "✕"
		}
		fmt.Fprintf(&b, "%s %s  (server %s, pid %d, %s)\n    %s  %s\n", mark, c.TmuxSession, c.Socket, c.ClaudePID, c.ClaudeStatus, c.SessionID, c.Path)
		if c.Reason != "" {
			fmt.Fprintf(&b, "    not adoptable: %s\n", c.Reason)
		}
	}
	return b.String()
}
