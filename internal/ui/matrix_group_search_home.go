package ui

// matrix/hardened: the group locator reads the existing trees only. It never
// starts sessions, polls a remote, or treats a session title as a group name.
import (
	"fmt"
	"sort"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func (h *Home) groupSearchItems() []GroupSearchItem {
	var items []GroupSearchItem
	if h.groupTree != nil {
		for _, group := range h.groupTree.GroupList {
			if group == nil || group.Path == "" || !h.isInGroupScope(group.Path) {
				continue
			}
			name := group.Name
			if name == "" {
				name = group.Path[strings.LastIndex(group.Path, "/")+1:]
			}
			items = append(items, GroupSearchItem{Name: name, Path: group.Path})
		}
	}

	// Snapshot only hosts already represented by the remote tree. A group
	// cache without a corresponding host cannot yet be revealed by the list.
	h.remoteSessionsMu.RLock()
	pathsByHost := make(map[string][]string, len(h.remoteSessions))
	for host, sessions := range h.remoteSessions {
		paths := append([]string(nil), h.remoteGroups[host]...)
		for _, inst := range sessions {
			paths = append(paths, inst.Group)
		}
		pathsByHost[host] = paths
	}
	h.remoteSessionsMu.RUnlock()
	hosts := make([]string, 0, len(pathsByHost))
	for host := range pathsByHost {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		root := "remotes/" + host
		if h.isInGroupScope(root) {
			items = append(items, GroupSearchItem{Name: host, Path: root, Remote: host})
		}
		// The remote tree synthesizes intermediate parents as well as the
		// explicitly reported groups, including groups with no sessions.
		groups := make(map[string]bool)
		for _, path := range pathsByHost[host] {
			parts := strings.Split(normalizeRemoteGroupPath(path), "/")
			for i := range parts {
				groups[strings.Join(parts[:i+1], "/")] = true
			}
		}
		paths := make([]string, 0, len(groups))
		for path := range groups {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			full := root + "/" + path
			if h.isInGroupScope(full) {
				items = append(items, GroupSearchItem{
					Name: path[strings.LastIndex(path, "/")+1:], Path: full, Remote: host,
				})
			}
		}
	}
	return items
}

func (h *Home) openGroupSearch() {
	if h.groupSearch == nil {
		h.groupSearch = NewGroupSearch()
	}
	h.groupSearch.SetSize(h.width, h.height)
	h.groupSearch.Show(h.groupSearchItems())
}

func (h *Home) handleGroupSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "enter" {
		selected, ok := h.groupSearch.Selected()
		if !ok {
			return h, nil
		}
		if !h.locateSearchedGroup(selected) {
			h.groupSearch.Hide()
			h.setError(fmt.Errorf("分组已变化，请重新搜索"))
			return h, nil
		}
		h.groupSearch.Hide()
		return h, nil
	}
	var cmd tea.Cmd
	h.groupSearch, cmd = h.groupSearch.Update(msg)
	return h, cmd
}

func (h *Home) locateSearchedGroup(target GroupSearchItem) bool {
	// Validate against current identities before touching filters or the cursor;
	// a reload may have removed/renamed the group while the picker was open.
	exists := false
	for _, item := range h.groupSearchItems() {
		if item.Remote == target.Remote && item.Path == target.Path {
			exists = true
			break
		}
	}
	if !exists {
		return false
	}
	h.statusFilter = ""
	h.keepEmptyFilter = false
	h.timeFilter = session.TimeFilterAll
	h.groupViewMode = session.GroupViewNormal
	if h.embeddedLayout {
		h.sidebarMode = sidebarGrouped
	}
	if target.Remote == "" {
		h.groupTree.ExpandGroupWithParents(target.Path)
	} else {
		root := "remotes/" + target.Remote
		for path := target.Path; ; path = groupParentPath(path) {
			delete(h.remoteGroupsCollapsed, path)
			if path == root {
				break
			}
		}
	}
	h.rebuildFlatItems()
	for index, item := range h.flatItems {
		local := target.Remote == "" && item.Type == session.ItemTypeGroup
		remote := target.Remote != "" && item.Type == session.ItemTypeRemoteGroup && item.RemoteName == target.Remote
		if (local || remote) && item.Path == target.Path {
			h.jumpToIndex(index)
			h.saveGroupState()
			h.saveUIState()
			return true
		}
	}
	return false
}
