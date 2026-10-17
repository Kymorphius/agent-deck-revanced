package web

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// matrix/hardened: retain only the current successful content and one previous
// content owner; equal-content polls must not replace a useful delta baseline.
type menuDeltaContent struct {
	snapshot *MenuSnapshot
	version  string
}

type menuDeltaChange struct {
	Index int      `json:"index"`
	Item  MenuItem `json:"item"`
}

type menuDeltaData struct {
	Profile       string            `json:"profile"`
	GeneratedAt   time.Time         `json:"generatedAt"`
	TotalGroups   int               `json:"totalGroups"`
	TotalSessions int               `json:"totalSessions"`
	Changes       []menuDeltaChange `json:"changes"`
}

// matrix/hardened: both opt-in wire values are encoded once outside publisher
// locks. Whole-row replacement carries nested removals without JSON merge rules.
func withVersionedMenu(previous *MenuSnapshot, baseVersion string, current menuPublication) menuPublication {
	if current.Err != nil || current.Snapshot == nil {
		return current
	}
	current.menuDelta, current.deltaBaseVersion = nil, ""
	current.versionedMenu = make([]byte, 0, len(current.encoded)+len(current.Fingerprint)+40)
	current.versionedMenu = append(current.versionedMenu, `{"type":"menu","version":`...)
	current.versionedMenu = strconv.AppendQuote(current.versionedMenu, current.Fingerprint)
	current.versionedMenu = append(current.versionedMenu, `,"data":`...)
	current.versionedMenu = append(current.versionedMenu, current.encoded...)
	current.versionedMenu = append(current.versionedMenu, '}')
	if previous == nil || baseVersion == "" || baseVersion == current.Fingerprint ||
		previous.Profile != current.Snapshot.Profile ||
		previous.TotalGroups != current.Snapshot.TotalGroups || previous.TotalSessions != current.Snapshot.TotalSessions ||
		current.Snapshot.TotalGroups < 0 || current.Snapshot.TotalSessions < 0 || len(previous.Items) != len(current.Snapshot.Items) {
		return current
	}
	changes := make([]menuDeltaChange, 0)
	seen := make(map[string]struct{}, len(previous.Items))
	groups, sessions := 0, 0
	for index, item := range current.Snapshot.Items {
		old := previous.Items[index]
		identity, valid := menuDeltaIdentity(item, index)
		oldIdentity, oldValid := menuDeltaIdentity(old, index)
		if !valid || !oldValid || identity != oldIdentity || !menuDeltaSameLayout(old, item) {
			return current
		}
		if _, duplicate := seen[identity]; duplicate {
			return current
		}
		seen[identity] = struct{}{}
		if item.Type == MenuItemTypeGroup {
			groups++
		} else {
			sessions++
		}
		if !reflect.DeepEqual(old, item) {
			changes = append(changes, menuDeltaChange{Index: index, Item: item})
		}
	}
	if groups != current.Snapshot.TotalGroups || sessions != current.Snapshot.TotalSessions || len(changes) == 0 {
		return current
	}
	encoded, err := json.Marshal(struct {
		Type        string        `json:"type"`
		BaseVersion string        `json:"baseVersion"`
		Version     string        `json:"version"`
		Data        menuDeltaData `json:"data"`
	}{"menu-delta", baseVersion, current.Fingerprint, menuDeltaData{
		Profile: current.Snapshot.Profile, GeneratedAt: current.Snapshot.GeneratedAt,
		TotalGroups: current.Snapshot.TotalGroups, TotalSessions: current.Snapshot.TotalSessions,
		Changes: changes,
	}})
	if err == nil && len(encoded) < len(current.versionedMenu) {
		current.menuDelta, current.deltaBaseVersion = encoded, baseVersion
	}
	return current
}

// matrix/hardened: a delta only patches a complete ordered list with unique,
// valid row identities. Unknown/mixed rows and invalid indices require full.
func menuDeltaIdentity(item MenuItem, index int) (string, bool) {
	if item.Index != index {
		return "", false
	}
	var identity string
	switch item.Type {
	case MenuItemTypeGroup:
		if item.Group == nil || item.Session != nil {
			return "", false
		}
		identity = item.Group.Path
	case MenuItemTypeSession:
		if item.Session == nil || item.Group != nil {
			return "", false
		}
		identity = item.Session.ID
	default:
		return "", false
	}
	if strings.TrimSpace(identity) == "" || strings.IndexByte(identity, 0) >= 0 {
		return "", false
	}
	return item.Type + ":" + identity, true
}

// matrix/hardened: navigation/layout changes conservatively send a full list,
// even when the same row identities happen to remain in the same order.
func menuDeltaSameLayout(old, next MenuItem) bool {
	if old.Level != next.Level || old.Path != next.Path || old.IsLastInGroup != next.IsLastInGroup ||
		old.IsSubSession != next.IsSubSession || old.IsLastSubSession != next.IsLastSubSession || old.ParentIsLastInGroup != next.ParentIsLastInGroup {
		return false
	}
	if old.Group != nil {
		return old.Group.Order == next.Group.Order && old.Group.Expanded == next.Group.Expanded
	}
	return old.Session.GroupPath == next.Session.GroupPath && old.Session.ParentSessionID == next.Session.ParentSessionID && old.Session.Order == next.Session.Order
}

// matrix/hardened: each connection tracks only the version successfully written.
// Gaps/first connections use full bytes; legacy connections keep identical wire.
func writeDashboardMenu(current menuPublication, optIn bool, lastVersion *string, write func([]byte) error) error {
	if current.Fingerprint == *lastVersion {
		return nil
	}
	encoded := current.dashboardMenu
	if optIn {
		encoded = current.versionedMenu
		if *lastVersion != "" && *lastVersion == current.deltaBaseVersion && len(current.menuDelta) > 0 {
			encoded = current.menuDelta
		}
	}
	if err := write(encoded); err != nil {
		return err
	}
	*lastVersion = current.Fingerprint
	return nil
}
