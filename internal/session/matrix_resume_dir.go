package session

// matrix/hardened: the directory a found Claude conversation must be resumed
// from.
//
// Search results (the recall index behind `G` and `recall open`) carry the
// LAST cwd a transcript recorded: Claude follows `cd` in its Bash tool, so a
// long conversation ends up in some subdirectory. `claude --resume <id>` only
// finds the conversation from the directory it was started in, because the
// project dir under <config>/projects/ is named after that launch dir.
// Resuming from the last cwd missed the transcript, and the start path then
// fell back to `--session-id <id>`: a new, empty conversation under the
// picked id, in an unrelated directory. This locates the
// transcript itself and returns the launch dir its project dir encodes.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ClaudeConversationLiveWindow is how recently a transcript may have been
// written before a resume is refused as probably still open elsewhere: two
// Claude processes on one conversation both append to the same file.
const ClaudeConversationLiveWindow = 2 * time.Minute

// ClaudeResumeLocation is where a Claude conversation can be resumed.
type ClaudeResumeLocation struct {
	Dir        string    // the directory `claude --resume` must start in
	Transcript string    // the transcript file that will be resumed
	ModTime    time.Time // its last write
}

// LiveFor reports how long ago the transcript was written and whether that
// is within ClaudeConversationLiveWindow of now.
func (l ClaudeResumeLocation) LiveFor(now time.Time) (time.Duration, bool) {
	age := now.Sub(l.ModTime)
	return age, age < ClaudeConversationLiveWindow
}

// LocateClaudeResumeDir finds sessionID's transcript under every known Claude
// config dir (configDirs first, then cfg's profiles and ~/.claude) and
// returns, for the largest copy, the recorded cwd that encodes to the project
// dir it sits in: the directory Claude was running in when it wrote there.
// That is not always the first cwd: a conversation continued from another
// directory keeps its older records. A copy recording no such cwd cannot be
// resumed from anywhere and is ignored, as are the smaller stubs a
// mis-directed `--session-id` launch wrote. It fails when no copy qualifies
// or the directory no longer exists, never guessing a directory.
func LocateClaudeResumeDir(cfg *UserConfig, sessionID string, configDirs ...string) (ClaudeResumeLocation, error) {
	if !IsBareClaudeSessionUUID(sessionID) {
		return ClaudeResumeLocation{}, fmt.Errorf("conversation id %q is not a bare Claude session uuid", sessionID)
	}
	fileName := sessionID + ".jsonl"
	var best ClaudeResumeLocation
	bestSize := int64(-1)
	for _, configDir := range conversationConfigDirCandidates(cfg, configDirs...) {
		entries, err := os.ReadDir(filepath.Join(configDir, "projects"))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path, err := containedConversationPath(configDir, "projects", entry.Name(), fileName)
			if err != nil {
				continue
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() <= bestSize {
				continue
			}
			cwd, ok := transcriptCWDForProjectDir(path, entry.Name())
			if !ok {
				continue
			}
			if !transcriptFileHasConversationData(path) {
				continue
			}
			best = ClaudeResumeLocation{Dir: cwd, Transcript: path, ModTime: info.ModTime()}
			bestSize = info.Size()
		}
	}
	if bestSize < 0 {
		return ClaudeResumeLocation{}, fmt.Errorf("no transcript of Claude conversation %s on this machine; nothing to resume", sessionID)
	}
	if info, err := os.Stat(best.Dir); err != nil || !info.IsDir() {
		return ClaudeResumeLocation{}, fmt.Errorf("conversation %s was started in %s, which no longer exists; claude can only resume it from there", sessionID, best.Dir)
	}
	return best, nil
}

// transcriptCWDForProjectDir returns the first cwd recorded in the transcript
// at path that encodes to projDirName, reading it a line at a time.
func transcriptCWDForProjectDir(path, projDirName string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<20)
	tried := map[string]bool{}
	for {
		line, readErr := reader.ReadBytes('\n')
		if cwd, ok := recordCWD(line); ok && !tried[cwd] {
			tried[cwd] = true
			if encodesSameWorkingDir(projDirName, cwd, resolveRealPath(cwd)) {
				return cwd, true
			}
		}
		if readErr != nil {
			return "", false
		}
	}
}

// recordCWD extracts the "cwd" field of one transcript record without
// decoding the whole (possibly multi-megabyte) line. Claude writes the key
// near the start of every record, before the message body; a "cwd" inside a
// message is escaped and does not match.
func recordCWD(line []byte) (string, bool) {
	const key = `"cwd":"`
	i := bytes.Index(line, []byte(key))
	if i < 0 {
		return "", false
	}
	quoted := line[i+len(key)-1:]
	for j := 1; j < len(quoted); j++ {
		switch quoted[j] {
		case '\\':
			j++
		case '"':
			var cwd string
			if json.Unmarshal(quoted[:j+1], &cwd) != nil || cwd == "" {
				return "", false
			}
			return cwd, true
		}
	}
	return "", false
}
