package session

import (
	"slices"
	"testing"
)

// matrix/hardened: these exercise the public local-search contract rather than
// duplicating the matching helper's implementation.
func TestMatrixSessionSearchGroups(t *testing.T) {
	tools := &Instance{Title: "one", GroupPath: "Tools/Agent-Deck"}
	other := &Instance{Title: "two", GroupPath: "Life/Reading"}
	ungrouped := &Instance{Title: "three"}
	defaultGroup := &Instance{Title: "four", GroupPath: DefaultGroupPath}
	instances := []*Instance{tools, other, ungrouped, defaultGroup}
	for _, tt := range []struct {
		query string
		want  []*Instance
	}{
		{"TOOLS", []*Instance{tools}},
		{"agent deck", []*Instance{tools}},
		{"tools agent-deck", []*Instance{tools}},
		{"reading", []*Instance{other}},
		{"my sessions", []*Instance{ungrouped, defaultGroup}},
		{"My-Sessions", []*Instance{ungrouped, defaultGroup}},
		{"no-such-group", nil},
	} {
		t.Run(tt.query, func(t *testing.T) {
			matrixAssertSessionSearch(t, instances, tt.query, tt.want)
		})
	}
}

func TestMatrixSessionSearchNameSeparators(t *testing.T) {
	names := []string{"agent deck", "agent-deck", "agent_deck", "agentdeck", "Agent\u2003Deck", "agent—deck"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			for _, field := range []string{"title", "project"} {
				t.Run(field, func(t *testing.T) {
					inst := &Instance{Title: "unrelated", GroupPath: "Other"}
					if field == "title" {
						inst.Title = name
					} else {
						inst.ProjectPath = "/workspace/" + name
					}
					for _, query := range names {
						matrixAssertSessionSearch(t, []*Instance{inst}, query, []*Instance{inst})
					}
				})
			}
		})
	}
}

func TestMatrixSessionSearchAbbreviationsAndBoundaries(t *testing.T) {
	title := &Instance{Title: "agent-deck", GroupPath: "Other"}
	unixPath := &Instance{Title: "one", ProjectPath: "/workspace/agent-deck/", GroupPath: "Other"}
	windowsPath := &Instance{Title: "two", ProjectPath: `C:\workspace\Agent-Deck\`, GroupPath: "Other"}
	group := &Instance{Title: "three", GroupPath: "Tools/Agent-Deck"}
	tool := &Instance{Title: "four", Tool: "agent-deck", GroupPath: "Other"}
	for _, inst := range []*Instance{title, unixPath, windowsPath, group, tool} {
		matrixAssertSessionSearch(t, []*Instance{inst}, "adk", []*Instance{inst})
	}

	// Search does not combine independent fields or distant parent directories
	// into a synthetic project/group abbreviation.
	crossFields := &Instance{Title: "a", ProjectPath: "/tmp/d", Tool: "k", GroupPath: "Other"}
	crossDirectories := &Instance{Title: "one", ProjectPath: "/alpha/demo/kappa", GroupPath: "Other"}
	crossWindowsDirectories := &Instance{Title: "two", ProjectPath: `C:\alpha\demo\kappa`, GroupPath: "Other"}
	crossGroups := &Instance{Title: "three", GroupPath: "alpha/demo/kappa"}
	instances := []*Instance{crossFields, crossDirectories, crossWindowsDirectories, crossGroups}
	matrixAssertSessionSearch(t, instances, "adk", nil)
	matrixAssertSessionSearch(t, instances, "zebrawombat", nil)

	// Full paths remain available for the existing continuous path search.
	matrixAssertSessionSearch(t, []*Instance{crossDirectories}, "/alpha/demo", []*Instance{crossDirectories})
	matrixAssertSessionSearch(t, []*Instance{crossDirectories}, "alpha demo", []*Instance{crossDirectories})
	matrixAssertSessionSearch(t, []*Instance{crossGroups}, "alpha demo", []*Instance{crossGroups})
}

func TestMatrixSessionSearchStatusWordsStayExact(t *testing.T) {
	for _, status := range []Status{StatusWaiting, StatusRunning, StatusIdle, StatusError, StatusStopped} {
		inst := &Instance{Title: "ordinary", Status: status}
		textOnly := &Instance{Title: string(status), GroupPath: "Other", Status: Status("different")}
		matrixAssertSessionSearch(t, []*Instance{nil, textOnly, inst}, " \t"+string(status)+"\u2003", []*Instance{inst})
	}
	runningTitle := &Instance{Title: "RUNNING notes", Status: StatusIdle}
	matrixAssertSessionSearch(t, []*Instance{runningTitle}, "running", nil)
	matrixAssertSessionSearch(t, []*Instance{runningTitle}, "RUN", []*Instance{runningTitle})
}

func TestMatrixSessionSearchEmptyAndPunctuationQueries(t *testing.T) {
	one := &Instance{Title: "one", GroupPath: "Other"}
	instances := []*Instance{nil, one}
	for _, query := range []string{"", " \t\n\u2003"} {
		got := FilterByQuery(instances, query)
		if !slices.Equal(got, instances) || &got[0] != &instances[0] {
			t.Fatalf("empty query %q should preserve the input slice", query)
		}
	}
	if got := FilterByQuery(nil, ""); got != nil {
		t.Fatalf("empty query should preserve nil input, got %v", got)
	}
	matrixAssertSessionSearch(t, instances, "one", []*Instance{one})
	matrixAssertSessionSearch(t, nil, "one", nil)

	hash := &Instance{Title: "C# notes", GroupPath: "Other"}
	underscores := &Instance{Title: "name__tag", GroupPath: "Other"}
	separatedHashes := &Instance{Title: "# separated #", GroupPath: "Other"}
	instances = []*Instance{one, hash, underscores, separatedHashes}
	matrixAssertSessionSearch(t, instances, "#", []*Instance{hash, separatedHashes})
	matrixAssertSessionSearch(t, instances, "__", []*Instance{underscores})
	matrixAssertSessionSearch(t, instances, "##", nil)
	matrixAssertSessionSearch(t, instances, "!!!", nil)
}

func TestMatrixSessionSearchRankAndInputOrder(t *testing.T) {
	weak := &Instance{Title: "a123d123k", GroupPath: "Other"}
	normalizedOne := &Instance{Title: "a_d-k", GroupPath: "Other"}
	literalOne := &Instance{Title: "ADK notes", GroupPath: "Other"}
	literalTwo := &Instance{Title: "notes adk", Tool: "adk", GroupPath: "adk"}
	normalizedTwo := &Instance{Title: "a-d_k", GroupPath: "Other"}
	strongOne := &Instance{Title: "axdk", GroupPath: "Other"}
	strongTwo := &Instance{Title: "axdk", GroupPath: "Other"}
	instances := []*Instance{weak, normalizedOne, literalOne, literalTwo, normalizedTwo, strongOne, strongTwo, nil, literalOne}
	before := slices.Clone(instances)
	want := []*Instance{literalOne, literalTwo, normalizedOne, normalizedTwo, strongOne, strongTwo, weak}
	for range 3 {
		matrixAssertSessionSearch(t, instances, "adk", want)
		if !slices.Equal(instances, before) {
			t.Fatal("search reordered its input slice")
		}
	}
	if literalOne.Title != "ADK notes" || normalizedOne.Title != "a_d-k" || weak.Title != "a123d123k" {
		t.Fatal("search changed a session field")
	}
}

func TestMatrixSessionSearchUnicode(t *testing.T) {
	chinese := &Instance{Title: "项目—管理", GroupPath: "工具/个人面板"}
	accented := &Instance{Title: "ÉTÉ notes", GroupPath: "Other"}
	instances := []*Instance{chinese, accented}
	for _, query := range []string{"项目\u3000管理", "项目管理", "项管", "个人 面板", "工"} {
		matrixAssertSessionSearch(t, instances, query, []*Instance{chinese})
	}
	matrixAssertSessionSearch(t, instances, "été", []*Instance{accented})
	matrixAssertSessionSearch(t, instances, "Été", []*Instance{accented})
	matrixAssertSessionSearch(t, instances, "管理项目", nil)
}

func matrixAssertSessionSearch(t *testing.T, instances []*Instance, query string, want []*Instance) {
	t.Helper()
	got := FilterByQuery(instances, query)
	if !slices.Equal(got, want) {
		gotTitles := make([]string, 0, len(got))
		for _, inst := range got {
			gotTitles = append(gotTitles, inst.Title)
		}
		wantTitles := make([]string, 0, len(want))
		for _, inst := range want {
			wantTitles = append(wantTitles, inst.Title)
		}
		t.Fatalf("FilterByQuery(%q) = %v, want %v", query, gotTitles, wantTitles)
	}
}
