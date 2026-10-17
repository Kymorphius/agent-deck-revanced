// matrix/hardened: native Windows sessions use structured launch options without
// a shell, shared Unix state, telemetry, hooks, or automatic installation.
package conpty

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ValidateOptions checks structured launch data without starting a process.
// matrix/hardened: the native registry reuses the backend's launch contract.
func ValidateOptions(options Options) error {
	_, err := normalizeOptions(options)
	return err
}

var (
	ErrUnsupported = errors.New("native ConPTY sessions require Windows 10 version 1809 or newer")
	ErrClosed      = errors.New("ConPTY process is closed")
	// ErrOutputBacklog is returned by Output when its consumer falls more than
	// eight MiB behind. The owned process tree is stopped instead of silently
	// losing terminal bytes or allowing unbounded memory growth.
	ErrOutputBacklog = errors.New("ConPTY output consumer exceeded the eight MiB backlog limit")
)

// Options describes one native process. Executable is resolved using the host's
// PATH when it is not a path. Args use Microsoft C runtime argument quoting;
// shell interpreters may impose their own additional parsing rules.
type Options struct {
	Executable string
	Args       []string
	// Env nil inherits the host environment. A non-nil empty slice supplies an
	// empty environment; duplicate names are case insensitive and last wins.
	Env  []string
	Dir  string
	Cols int // Zero defaults to 80; valid explicit values are 1 through 32767.
	Rows int // Zero defaults to 24; valid explicit values are 1 through 32767.
}

func normalizeOptions(options Options) (Options, error) {
	if options.Executable == "" {
		return Options{}, errors.New("ConPTY executable is required")
	}
	if strings.ContainsRune(options.Executable, '"') {
		return Options{}, errors.New("ConPTY executable contains a quotation mark")
	}
	for label, value := range map[string]string{"executable": options.Executable, "directory": options.Dir} {
		if err := validateString(label, value); err != nil {
			return Options{}, err
		}
	}
	for index, arg := range options.Args {
		if err := validateString(fmt.Sprintf("argument %d", index), arg); err != nil {
			return Options{}, err
		}
	}
	for _, entry := range options.Env {
		if _, err := environmentName(entry); err != nil {
			return Options{}, err
		}
	}
	if options.Cols == 0 {
		options.Cols = 80
	}
	if options.Rows == 0 {
		options.Rows = 24
	}
	if err := validateSize(options.Cols, options.Rows); err != nil {
		return Options{}, err
	}
	// matrix/hardened: detach caller-owned slices so later mutation cannot
	// change the launch while native process attributes are being assembled.
	options.Args = append([]string(nil), options.Args...)
	if options.Env != nil {
		options.Env = append([]string{}, options.Env...)
	}
	return options, nil
}

func validateSize(cols, rows int) error {
	if cols < 1 || cols > 32767 || rows < 1 || rows > 32767 {
		return fmt.Errorf("ConPTY dimensions must be 1 through 32767, got %dx%d", cols, rows)
	}
	return nil
}

func validateString(label, value string) error {
	if strings.ContainsRune(value, '\x00') || !utf8.ValidString(value) {
		return fmt.Errorf("ConPTY %s contains NUL or invalid UTF-8", label)
	}
	return nil
}

func environmentName(entry string) (string, error) {
	if err := validateString("environment entry", entry); err != nil {
		return "", err
	}
	separator := strings.IndexByte(entry, '=')
	// Windows maintains hidden per-drive current directories as =C:=path.
	if len(entry) >= 4 && entry[0] == '=' && entry[2] == ':' && entry[3] == '=' &&
		((entry[1] >= 'a' && entry[1] <= 'z') || (entry[1] >= 'A' && entry[1] <= 'Z')) {
		separator = 3
	}
	if separator <= 0 {
		// Environment values may contain credentials; validation errors never
		// include the entry that failed validation.
		return "", errors.New("ConPTY environment entry must have a nonempty name and '='")
	}
	return entry[:separator], nil
}

func environmentBlock(env []string) ([]uint16, error) {
	entries := make(map[string]string, len(env))
	for _, entry := range env {
		name, err := environmentName(entry)
		if err != nil {
			return nil, err
		}
		entries[strings.ToUpper(name)] = entry
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	ordered := make([]string, len(names))
	for index, name := range names {
		ordered[index] = entries[name]
	}
	return utf16.Encode([]rune(strings.Join(ordered, "\x00") + "\x00\x00")), nil
}

func commandLine(executable string, args []string) (string, error) {
	words := make([]string, 1, len(args)+1)
	words[0] = quoteWindowsArg(executable)
	for _, arg := range args {
		words = append(words, quoteWindowsArg(arg))
	}
	line := strings.Join(words, " ")
	if len(utf16.Encode([]rune(line))) > 32766 {
		return "", errors.New("ConPTY command line exceeds the Windows 32767 UTF-16 unit limit")
	}
	return line, nil
}

// quoteWindowsArg follows CommandLineToArgvW / Microsoft CRT backslash and
// quote handling. It does not interpolate shell metacharacters or variables.
func quoteWindowsArg(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\n\v\"") {
		return arg
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	slashes := 0
	for _, ch := range arg {
		if ch == '\\' {
			slashes++
			continue
		}
		if ch == '"' {
			quoted.WriteString(strings.Repeat("\\", 2*slashes+1))
		} else {
			quoted.WriteString(strings.Repeat("\\", slashes))
		}
		slashes = 0
		quoted.WriteRune(ch)
	}
	quoted.WriteString(strings.Repeat("\\", 2*slashes))
	quoted.WriteByte('"')
	return quoted.String()
}
