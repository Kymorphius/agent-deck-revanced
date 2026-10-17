package worldstore

import (
	"sort"
	"strings"
)

// Column is one kanban column: a lane with its tasks in board order.
type Column struct {
	Lane  string
	Title string
	Tasks []Task
	// Hidden counts tasks left out (only the done column is capped).
	Hidden int
}

// Lanes in board order, with the title shown for each. matrix/hardened: the
// seven columns shared with World Store's TUI and Multica. "blocked" is
// derived from blockers; statuses agent-deck does not know go to "unknown",
// which is only shown when it has tasks.
var laneOrder = []struct{ lane, title string }{
	{"backlog", "待规划"},
	{"todo", "待办"},
	{"doing", "进行中"},
	{"review", "待验收"},
	{"blocked", "卡住"},
	{"done", "完成"},
	{"unknown", "待核"},
}

// DoneLimit caps the done column to the most recent completions.
const DoneLimit = 20

// ColumnOf is the column a task is shown in: its lane, or "blocked" while
// an unresolved blocker names an open task.
func ColumnOf(t Task) string {
	if t.Blocked && t.Lane != "done" && t.Lane != "cancelled" {
		return "blocked"
	}
	// matrix/hardened: an open task whose Multica issue is being worked on
	// shows in 进行中; its World Store status is unchanged.
	if t.Multica != nil && t.Multica.Root.Status == "in_progress" && (t.Lane == "backlog" || t.Lane == "todo") {
		return "doing"
	}
	return t.Lane
}

// Columns groups tasks into board columns. Cancelled tasks are left out.
// Open lanes sort by plan date (undated last) then name; done sorts by
// completion time, newest first.
func Columns(tasks []Task, filter string) []Column {
	filter = strings.ToLower(strings.TrimSpace(filter))
	by := map[string][]Task{}
	for _, t := range tasks {
		if t.Lane == "cancelled" {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(t.Name+" "+t.Category+" "+t.Status), filter) {
			continue
		}
		by[ColumnOf(t)] = append(by[ColumnOf(t)], t)
	}
	cols := make([]Column, 0, len(laneOrder))
	for _, lo := range laneOrder {
		ts := by[lo.lane]
		if lo.lane == "unknown" && len(ts) == 0 {
			continue
		}
		if lo.lane == "done" {
			sort.SliceStable(ts, func(i, j int) bool { return deref(ts[i].CompletedAt) > deref(ts[j].CompletedAt) })
		} else {
			sort.SliceStable(ts, func(i, j int) bool {
				a, b := ts[i].PlanDate, ts[j].PlanDate
				if (a == nil) != (b == nil) {
					return a != nil
				}
				if a != nil && *a != *b {
					return *a < *b
				}
				return ts[i].Name < ts[j].Name
			})
		}
		col := Column{Lane: lo.lane, Title: lo.title, Tasks: ts}
		if lo.lane == "done" && len(ts) > DoneLimit {
			col.Tasks, col.Hidden = ts[:DoneLimit], len(ts)-DoneLimit
		}
		cols = append(cols, col)
	}
	return cols
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// LaunchPrompt is the first message for a session started from a task.
func LaunchPrompt(t Task) string {
	var sb strings.Builder
	sb.WriteString("World Store 任务：")
	sb.WriteString(t.Name)
	if t.Category != "" {
		sb.WriteString("\n分类：" + t.Category)
	}
	if plan := strings.TrimSpace(t.PlanText); plan != "" {
		sb.WriteString("\n计划说明：\n" + plan)
	}
	sb.WriteString("\n\n请先确认目标和范围，再开始处理。")
	return sb.String()
}
