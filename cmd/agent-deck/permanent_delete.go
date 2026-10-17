package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// matrix/hardened: planning is read-only; execution requires a separate explicit confirmation.
func runPermanentDelete(storage *session.Storage, inst *session.Instance, dryRun, yes, jsonMode bool) error {
	plan, err := storage.PlanPermanentDelete(inst)
	if err != nil {
		return err
	}
	// matrix/hardened: preview remains allowed; confirmation shares the backend terminal guard.
	if !dryRun {
		if err := session.CheckPermanentDeleteTerminal(inst); err != nil {
			return err
		}
	}
	if jsonMode {
		if !dryRun && !yes {
			return fmt.Errorf("JSON permanent deletion requires --yes or --dry-run")
		}
		if dryRun {
			return json.NewEncoder(os.Stdout).Encode(plan)
		}
	} else {
		fmt.Printf("Permanent deletion: %s\nID: %s\nNative IDs: %s\n", plan.Title, plan.SessionID, strings.Join(plan.NativeIDs, ", "))
		fmt.Println("Delete native transcript/snapshots:")
		for _, path := range plan.Files {
			fmt.Println("  " + path)
		}
		fmt.Println("Clear matching native history/index/database rows, agent-deck recall, costs and recent entries.")
		fmt.Println("Project/worktree files and other conversations are preserved. External backups, forked copies and cloud records are outside this operation.")
		if dryRun {
			fmt.Println("Preview only; nothing changed.")
			return nil
		}
		if !yes {
			fmt.Print("This cannot be undone. Type DELETE to confirm (Enter cancels): ")
			answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil {
				return err
			}
			if strings.TrimSpace(answer) != "DELETE" {
				fmt.Println("Cancelled; nothing changed.")
				return nil
			}
		}
	}
	if err := storage.PermanentlyDelete(inst, plan); err != nil {
		return fmt.Errorf("permanent deletion incomplete: %w; inspect and retry", err)
	}
	if jsonMode {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"permanently_deleted": true, "session_id": inst.ID})
	}
	fmt.Println("Permanently deleted local conversation records. No undo entry retained.")
	return nil
}
