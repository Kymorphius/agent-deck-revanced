package ui

import "sort"

// matrix/hardened: Local is always a valid create destination, regardless of
// which rows are currently visible. Keep cursor parent context on its own host.
func (g *GroupDialog) SetCreationTargets(names []string) {
	g.creationTargets = []string{""}
	seen := map[string]bool{"": true}
	for _, name := range append(names, g.remoteName) {
		if !seen[name] {
			g.creationTargets = append(g.creationTargets, name)
			seen[name] = true
		}
	}
	sort.Strings(g.creationTargets[1:])
	g.originTarget, g.originParentPath, g.originParentName = g.remoteName, g.contextParentPath, g.contextParentName
}

func (g *GroupDialog) focusCreationTarget() {
	g.focusIndex = 2
	g.nameInput.Blur()
	g.pathInput.Blur()
}

func (g *GroupDialog) cycleCreationTarget(delta int) {
	if len(g.creationTargets) < 2 {
		return
	}
	index := 0
	for i, target := range g.creationTargets {
		if target == g.remoteName {
			index = i
			break
		}
	}
	g.remoteName = g.creationTargets[(index+delta+len(g.creationTargets))%len(g.creationTargets)]
	g.groupPath, g.parentName = "", ""
	g.contextParentPath, g.contextParentName = "", ""
	if g.remoteName == g.originTarget {
		g.contextParentPath, g.contextParentName = g.originParentPath, g.originParentName
	}
	// Paths and parent groups cannot carry across machines.
	g.pathInput.SetValue("")
	g.validationErr = ""
	g.pathSuggestionsHidden = true
}
