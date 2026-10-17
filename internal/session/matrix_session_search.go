package session

import (
	"sort"
	"strings"
	"unicode"

	"github.com/sahilm/fuzzy"
)

// matrix/hardened: local search treats punctuation and Unicode whitespace as
// optional name separators, but preserves symbols in literal queries.
func matrixNormalizeSessionSearch(value string) string {
	var normalized strings.Builder
	for _, r := range strings.ToLower(value) {
		if !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

type matrixSessionSearchMatch struct {
	instance *Instance
	tier     int
	score    int
}

// matrix/hardened: each field matches independently. Full paths retain literal
// and normalized substring search; fuzzy abbreviations use only the project
// basename and individual group segments so parent directories cannot invent a
// project-name match. Stable sorting leaves equal-quality results in list order.
func matrixFilterSessionSearch(instances []*Instance, query string) []*Instance {
	normalizedQuery := matrixNormalizeSessionSearch(query)
	matches := make([]matrixSessionSearchMatch, 0, len(instances))
	seen := make(map[*Instance]struct{}, len(instances))
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		if _, ok := seen[inst]; ok {
			continue
		}
		seen[inst] = struct{}{}

		groupPath := inst.GroupPath
		if groupPath == "" {
			groupPath = DefaultGroupPath
		}
		fields := []string{inst.Title, inst.ProjectPath, inst.Tool, groupPath}
		if groupPath == DefaultGroupPath {
			fields = append(fields, DefaultGroupName)
		}
		literalMatch := false
		normalizedMatch := false
		for _, field := range fields {
			if strings.Contains(strings.ToLower(field), query) {
				literalMatch = true
				break
			}
			if normalizedQuery != "" && strings.Contains(matrixNormalizeSessionSearch(field), normalizedQuery) {
				normalizedMatch = true
			}
		}
		if literalMatch {
			matches = append(matches, matrixSessionSearchMatch{instance: inst, tier: 0})
			continue
		}
		if normalizedMatch {
			matches = append(matches, matrixSessionSearchMatch{instance: inst, tier: 1})
			continue
		}
		if normalizedQuery == "" {
			// A punctuation-only query must not turn into a match-all search.
			continue
		}

		projectPath := strings.TrimRight(strings.ReplaceAll(inst.ProjectPath, "\\", "/"), "/")
		projectName := projectPath[strings.LastIndex(projectPath, "/")+1:]
		fuzzyFields := []string{inst.Title, projectName, inst.Tool}
		fuzzyFields = append(fuzzyFields, strings.Split(groupPath, "/")...)
		if groupPath == DefaultGroupPath {
			fuzzyFields = append(fuzzyFields, DefaultGroupName)
		}
		for i := range fuzzyFields {
			fuzzyFields[i] = matrixNormalizeSessionSearch(fuzzyFields[i])
		}
		fieldMatches := fuzzy.FindNoSort(normalizedQuery, fuzzyFields)
		if len(fieldMatches) == 0 {
			continue
		}
		bestScore := fieldMatches[0].Score
		for _, match := range fieldMatches[1:] {
			if match.Score > bestScore {
				bestScore = match.Score
			}
		}
		matches = append(matches, matrixSessionSearchMatch{instance: inst, tier: 2, score: bestScore})
	}

	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].tier != matches[j].tier {
			return matches[i].tier < matches[j].tier
		}
		return matches[i].score > matches[j].score
	})
	filtered := make([]*Instance, 0, len(matches))
	for _, match := range matches {
		filtered = append(filtered, match.instance)
	}
	return filtered
}
