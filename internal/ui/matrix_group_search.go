package ui

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
	"github.com/sahilm/fuzzy"
)

// GroupSearchItem identifies a group independently of any session.
// matrix/hardened: include the remote alias so equal paths on different hosts stay distinct.
type GroupSearchItem struct {
	Name   string
	Path   string
	Remote string
}

// GroupSearch is a group-only locator; Home owns navigation after selection.
// matrix/hardened: keep typed group names separate from session status filters.
type GroupSearch struct {
	input    textinput.Model
	allItems []GroupSearchItem
	results  []GroupSearchItem
	cursor   int
	width    int
	height   int
	visible  bool
}

// NewGroupSearch constructs the group locator without opening it.
func NewGroupSearch() *GroupSearch {
	ti := textinput.New()
	ti.Placeholder = "分组名 / 路径..."
	ti.CharLimit = 256
	ti.Width = 40
	return &GroupSearch{input: ti}
}

func (s *GroupSearch) SetSize(width, height int) { s.width, s.height = width, height }

// Show always starts a fresh query and owns a copy of the supplied group list.
func (s *GroupSearch) Show(items []GroupSearchItem) {
	s.input.SetValue("")
	s.cursor = 0
	s.allItems = nil
	s.results = nil
	s.SetItems(items)
	s.visible = true
	s.input.Focus()
}

// SetItems refreshes the corpus while retaining the selected group when possible.
// matrix/hardened: background refreshes must not silently choose a different host.
func (s *GroupSearch) SetItems(items []GroupSearchItem) {
	selected, hadSelection := s.Selected()
	seen := make(map[groupSearchIdentity]struct{}, len(items))
	s.allItems = make([]GroupSearchItem, 0, len(items))
	for _, item := range items {
		id := groupSearchItemIdentity(item)
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		s.allItems = append(s.allItems, item)
	}
	s.updateResults()
	if hadSelection {
		for i, item := range s.results {
			if groupSearchItemIdentity(item) == groupSearchItemIdentity(selected) {
				s.cursor = i
				break
			}
		}
	}
}

func (s *GroupSearch) Hide() {
	s.visible = false
	s.input.Blur()
}

func (s *GroupSearch) IsVisible() bool { return s.visible }

func (s *GroupSearch) Selected() (GroupSearchItem, bool) {
	if len(s.results) == 0 {
		return GroupSearchItem{}, false
	}
	s.cursor = min(max(s.cursor, 0), len(s.results)-1)
	return s.results[s.cursor], true
}

// Update consumes typing and navigation while the locator is visible.
// matrix/hardened: forward cursor blink messages as well as keys to textinput.
func (s *GroupSearch) Update(msg tea.Msg) (*GroupSearch, tea.Cmd) {
	if !s.visible {
		return s, nil
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			s.Hide()
			return s, nil
		case "enter":
			if len(s.results) > 0 {
				s.Hide()
			}
			return s, nil
		case "up", "ctrl+k":
			s.cursor = max(s.cursor-1, 0)
			return s, nil
		case "down", "ctrl+j":
			s.cursor = max(min(s.cursor+1, len(s.results)-1), 0)
			return s, nil
		}
	}
	previous := s.input.Value()
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	if previous != s.input.Value() {
		s.cursor = 0
		s.updateResults()
	}
	return s, cmd
}

type groupSearchIdentity struct{ remote, path string }

func groupSearchItemIdentity(item GroupSearchItem) groupSearchIdentity {
	return groupSearchIdentity{item.Remote, item.Path}
}

// matrix/hardened: punctuation and Unicode spacing are optional separators;
// a punctuation-only query still requires its literal spelling to occur.
func normalizeGroupSearch(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}

func (s *GroupSearch) updateResults() {
	query := strings.ToLower(strings.TrimSpace(s.input.Value()))
	if query == "" {
		s.results = append([]GroupSearchItem(nil), s.allItems...)
	} else {
		type ranked struct {
			item        GroupSearchItem
			tier, score int
		}
		normalized := normalizeGroupSearch(query)
		matches := make([]ranked, 0, len(s.allItems))
		for _, item := range s.allItems {
			fields := []string{item.Name, item.Path}
			tier := 3
			for _, field := range fields {
				if strings.Contains(strings.ToLower(field), query) {
					tier = 0
					break
				}
				if normalized != "" && strings.Contains(normalizeGroupSearch(field), normalized) {
					tier = 1
				}
			}
			score := 0
			if tier == 3 && normalized != "" {
				for i := range fields {
					fields[i] = normalizeGroupSearch(fields[i])
				}
				for _, match := range fuzzy.FindNoSort(normalized, fields) {
					if tier == 3 || match.Score > score {
						score = match.Score
					}
					tier = 2
				}
			}
			if tier < 3 {
				matches = append(matches, ranked{item, tier, score})
			}
		}
		sort.SliceStable(matches, func(i, j int) bool {
			if matches[i].tier != matches[j].tier {
				return matches[i].tier < matches[j].tier
			}
			return matches[i].score > matches[j].score
		})
		s.results = make([]GroupSearchItem, len(matches))
		for i, match := range matches {
			s.results[i] = match.item
		}
	}
	s.cursor = max(min(s.cursor, len(s.results)-1), 0)
}

// matrix/hardened: columns describe the group without repeating its name or
// remote namespace; legacy aliases retain their complete relative path.
func groupSearchColumns(item GroupSearchItem) (name, path, host string) {
	host = item.Remote
	if host == "" {
		host = "本机"
	}
	path = groupSearchRelativePath(item)
	parent, leaf := "", path
	if last := strings.LastIndexByte(path, '/'); last >= 0 {
		parent, leaf = path[:last], path[last+1:]
	}
	name = item.Name
	if name == "" {
		name = leaf
	}
	if normalizeGroupSearch(name) == normalizeGroupSearch(leaf) {
		path = parent
	}
	if path == "" {
		path = "—"
	}
	return name, path, host
}

func groupSearchRelativePath(item GroupSearchItem) string {
	path := item.Path
	if item.Remote != "" {
		prefix := "remotes/" + item.Remote
		if path == prefix {
			return ""
		}
		path = strings.TrimPrefix(path, prefix+"/")
	}
	return path
}

// matrix/hardened: keep every column aligned in terminal cells, including
// Chinese names and emoji, and retain the device even on narrow screens.
func groupSearchRow(name, path, host, prefix string, budget int) string {
	dataWidth := max(budget-cellWidth(prefix), 1)
	hostWidth := min(max(dataWidth/7, 8), 12)
	if budget < 42 {
		hostWidth = min(hostWidth, max(dataWidth-4, 1))
		nameWidth := max(dataWidth-hostWidth-2, 1)
		return prefix + fitCellWidth(cellTruncate(name, nameWidth, "…"), nameWidth) + "  " + fitCellWidth(cellTruncate(host, hostWidth, "…"), hostWidth)
	}
	nameWidth := max(dataWidth*38/100, 1)
	pathWidth := max(dataWidth-nameWidth-hostWidth-4, 1)
	return prefix + fitCellWidth(cellTruncate(name, nameWidth, "…"), nameWidth) + "  " +
		fitCellWidth(cellTruncate(path, pathWidth, "…"), pathWidth) + "  " +
		fitCellWidth(cellTruncate(host, hostWidth, "…"), hostWidth)
}

// matrix/hardened: the fixed selected identity keeps the distinguishing path
// suffix visible even when a narrow table has to hide its path column.
func groupSearchIdentityText(item GroupSearchItem, width int) string {
	_, _, host := groupSearchColumns(item)
	identity := host + " · " + groupSearchRelativePath(item)
	if cellWidth(identity) <= width {
		return identity
	}
	if width <= 1 {
		return cellTruncate(identity, width, "…")
	}
	leftWidth := (width - 1) / 2
	rightWidth := width - leftWidth - 1
	clusters := uniseg.NewGraphemes(identity)
	var suffix []string
	for clusters.Next() {
		suffix = append(suffix, clusters.Str())
	}
	start, used := len(suffix), 0
	for start > 0 && used+cellWidth(suffix[start-1]) <= rightWidth {
		start--
		used += cellWidth(suffix[start])
	}
	return cellTruncate(identity, leftWidth, "") + "…" + strings.Join(suffix[start:], "")
}

// View fits the locator to the same responsive frame used by session search.
// matrix/hardened: a single row per group lets terminal height determine the
// visible result count; only this locator expands on wider terminals.
func (s *GroupSearch) View() string {
	if !s.visible {
		return ""
	}
	overlayWidth := searchOverlayWidth(s.width)
	if s.width >= 70 {
		overlayWidth = min(96, s.width-10)
	}
	contentWidth := overlayWidth - overlayStyle.GetHorizontalFrameSize()
	innerWidth := max(contentWidth-searchBoxStyle.GetHorizontalFrameSize(), 10)
	s.input.Width = max(innerWidth-lipgloss.Width(s.input.Prompt), 1)
	header := lipgloss.NewStyle().Foreground(ColorAccent).Bold(true).Render("🔍 Group Search")
	searchBox := searchBoxStyle.Width(innerWidth).Render(s.input.View())

	budget := max(contentWidth-resultItemStyle.GetHorizontalFrameSize(), 1)
	columns := lipgloss.NewStyle().Foreground(ColorComment).Render(resultItemStyle.Render(groupSearchRow("分组", "路径", "设备", "  ", budget)))
	rows := make([]string, 0, len(s.results))
	for i, item := range s.results {
		name, path, host := groupSearchColumns(item)
		prefix := "  "
		style := resultItemStyle
		if i == s.cursor {
			prefix = "› "
			style = selectedResultStyle
		}
		rows = append(rows, style.Render(groupSearchRow(name, path, host, prefix, budget)))
	}
	if len(rows) == 0 {
		rows = []string{"No groups found"}
	}
	count := fmt.Sprintf("%d groups", len(s.results))
	if len(s.results) > 0 {
		count = fmt.Sprintf("%d/%d groups", s.cursor+1, len(s.results))
	}
	identity := ""
	if selected, ok := s.Selected(); ok {
		identity = groupSearchIdentityText(selected, contentWidth)
	}
	keys := renderDialogFooterRows(contentWidth, 2, "  ",
		[]string{"[Enter] Locate", "[↑↓] Move", "[Esc] Cancel"}, 1)
	sections := dialogSections{
		head:  []string{header, searchBox, columns},
		body:  rows,
		focus: s.cursor,
		foot: []string{
			lipgloss.NewStyle().Foreground(ColorComment).Render(count),
			lipgloss.NewStyle().Foreground(ColorComment).Render(identity),
			lipgloss.NewStyle().Foreground(ColorComment).Render(glueBracketHintGroups(keys)),
		},
	}
	box := renderFittedDialog(overlayStyle.Width(overlayWidth), centeredDialogHeight(s.height), sections)
	return centerInScreen(box, s.width, s.height)
}
