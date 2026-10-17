//go:build windows

// matrix/hardened: native registry ownership is a non-inheritable exclusive Windows file handle.
package nativeregistry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/winipc"
	"golang.org/x/sys/windows"
)

func prepareDirectory(dir string) error {
	// ProtectPath applies its stricter drive/path alias validation before any
	// data files are created. Root ancestors are explicitly trusted by Open's
	// contract; still reject existing reparse components here.
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Dir(dir) == dir || strings.HasPrefix(dir, `\\`) || len(dir) < 4 || dir[1:3] != `:\` || !(dir[0] >= 'a' && dir[0] <= 'z' || dir[0] >= 'A' && dir[0] <= 'Z') {
		return fmt.Errorf("%w: require an explicit local drive-absolute dedicated directory", ErrUnsafePath)
	}
	// Validate before Mkdir: Win32 aliases such as trailing dots, device names
	// and alternate streams must never create or chmod a normalized target.
	for _, component := range strings.Split(dir[3:], `\`) {
		if component == "" || component == "." || component == ".." || strings.HasSuffix(component, " ") || strings.HasSuffix(component, ".") {
			return fmt.Errorf("%w: ambiguous registry path component", ErrUnsafePath)
		}
		for _, r := range component {
			if r < 32 || strings.ContainsRune(`:<>"|?*`, r) {
				return fmt.Errorf("%w: invalid registry path character", ErrUnsafePath)
			}
		}
		base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		superscriptDevice := false
		if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
			suffix := base[3:]
			superscriptDevice = suffix == "¹" || suffix == "²" || suffix == "³"
		}
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' || superscriptDevice {
			return fmt.Errorf("%w: DOS device registry path", ErrUnsafePath)
		}
	}
	for current := dir; filepath.Dir(current) != current; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && current == dir {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: trusted ancestor unavailable: %v", ErrUnsafePath, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: registry path contains a non-directory or reparse point", ErrUnsafePath)
		}
		attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(current))
		if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("%w: registry ancestor is a reparse point or unavailable", ErrUnsafePath)
		}
	}
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create native registry root: %w", err)
	}
	if err := winipc.ProtectPath(dir); err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return ErrOwned
		}
		return fmt.Errorf("%w: protect native registry root: %v", ErrUnsafePath, err)
	}
	return nil
}

func openFile(path string, create bool, sharing uint32) (*os.File, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid registry file path", ErrUnsafePath)
	}
	disposition := uint32(windows.OPEN_EXISTING)
	if create {
		disposition = windows.OPEN_ALWAYS
	}
	handle, err := windows.CreateFile(path16, windows.GENERIC_READ|windows.GENERIC_WRITE, sharing, nil, disposition, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, ErrOwned
		}
		return nil, fmt.Errorf("%w: open registry file: %v", ErrUnsafePath, err)
	}
	file := os.NewFile(uintptr(handle), path)
	kind, err := windows.GetFileType(handle)
	var info windows.ByHandleFileInformation
	infoErr := windows.GetFileInformationByHandle(handle, &info)
	if err != nil || infoErr != nil || kind != windows.FILE_TYPE_DISK || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		_ = file.Close()
		return nil, fmt.Errorf("%w: registry file must be a single-link disk file without a reparse point", ErrUnsafePath)
	}
	return file, nil
}

func protectFile(path string, create bool) error {
	file, err := openFile(path, create, 0)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := winipc.ProtectPath(path); err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return ErrOwned
		}
		return fmt.Errorf("%w: protect registry file: %v", ErrUnsafePath, err)
	}
	return nil
}

func acquireOwner(path string) (*os.File, error) {
	if err := protectFile(path, true); err != nil {
		return nil, err
	}
	// matrix/hardened: share=0 is the cross-process lock. Recheck the exact
	// final handle after ACL protection, rejecting aliases and hard links.
	return openFile(path, false, 0)
}

func releaseOwner(file *os.File) error { return file.Close() }

func verifyHeldPath(path string, file *os.File) error {
	// matrix/hardened: os.SameFile lazily opens another Windows handle with
	// share=0, which would collide with our legitimate exclusive owner. Check
	// the held handle's final canonical pathname instead; every held handle
	// denies FILE_SHARE_DELETE, preventing replacement while it is owned.
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 && info.NumberOfLinks != 1 {
		return fmt.Errorf("%w: held registry identity changed", ErrUnsafePath)
	}
	buffer := make([]uint16, 1024)
	size, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil || size == 0 || size >= uint32(len(buffer)) {
		return fmt.Errorf("%w: canonical held registry path unavailable", ErrUnsafePath)
	}
	actual := strings.TrimPrefix(windows.UTF16ToString(buffer[:size]), `\\?\`)
	if !strings.EqualFold(actual, path) {
		return fmt.Errorf("%w: held registry pathname changed or was an alias", ErrUnsafePath)
	}
	return nil
}

func prepareData(path string) (*os.File, error) {
	if err := protectFile(path, true); err != nil {
		return nil, err
	}
	// Hold an independent file identity while SQLite uses its own handles.
	// No FILE_SHARE_DELETE means the descriptor cannot be swapped underneath
	// a running manager by renaming/removing this pathname.
	return openFile(path, false, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
}

func protectOptionalData(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return protectFile(path, false)
}
