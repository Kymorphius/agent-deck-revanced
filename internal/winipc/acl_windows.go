//go:build windows

// matrix/hardened: ACL changes are limited to an explicit caller-owned handle, with independent readback.
package winipc

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProtectPath protects an existing dedicated state file or directory. It does
// not create, remove or traverse children. Directory ACEs are inheritable for
// future children; existing children retain their ACLs and need explicit calls.
// The caller must supply a trusted ancestor path and protect a new directory
// before creating sensitive contents. Busy or unowned objects fail closed.
func ProtectPath(path string) error {
	if err := validateStatePath(path); err != nil {
		return err
	}
	path16, err := windows.UTF16PtrFromString(strings.ReplaceAll(path, "/", `\`))
	if err != nil {
		return err
	}
	// SetSecurityInfo does not propagate ACEs through a MAXIMUM_ALLOWED
	// handle. Exclusive sharing alone did propagate on Windows Server 2025.
	// OPEN_REPARSE_POINT avoids following the final path component; all
	// mutations use this exact non-inheritable opened object.
	handle, err := windows.CreateFile(path16, windows.MAXIMUM_ALLOWED,
		0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("open native state path for ACL: %w", err)
	}
	defer windows.CloseHandle(handle)
	kind, err := windows.GetFileType(handle)
	if err != nil || kind != windows.FILE_TYPE_DISK {
		return fmt.Errorf("native state path is not a disk file or directory: %v", err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect native state path: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("native state path is a reparse point")
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if !isDir && info.NumberOfLinks != 1 {
		return fmt.Errorf("native state file has multiple hard links")
	}
	userSID, err := CurrentUserSID()
	if err != nil {
		return err
	}
	old, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read native state owner: %w", err)
	}
	owner, _, err := old.Owner()
	if err != nil || owner == nil {
		return fmt.Errorf("native state owner unavailable: %v", err)
	}
	if owner.String() != userSID {
		// Elevated administrators may create objects owned by the default
		// token owner (Administrators). Do not change that existing owner.
		defaultOwner, err := currentDefaultOwnerSID()
		if err != nil {
			return err
		}
		if owner.String() != defaultOwner {
			return fmt.Errorf("native state path is not owned by the caller")
		}
	}
	inherit := ""
	if isDir {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + inherit + ";FA;;;" + userSID + ")(A;" + inherit + ";FA;;;SY)")
	if err != nil {
		return fmt.Errorf("build native state ACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("native state ACL unavailable: %v", err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("protect native state ACL: %w", err)
	}
	runtime.KeepAlive(sd)
	verified, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read back native state ACL: %w", err)
	}
	if err := checkRestrictedDACL(verified, userSID, isDir); err != nil {
		return fmt.Errorf("verify native state ACL: %w", err)
	}
	return nil
}

func currentDefaultOwnerSID() (string, error) {
	var length uint32
	token := windows.GetCurrentProcessToken()
	if err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &length); !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return "", fmt.Errorf("query native state token owner size: %v", err)
	}
	word := uint32(unsafe.Sizeof(uintptr(0)))
	if length < word || length > 64*1024 {
		return "", fmt.Errorf("invalid native state token owner size")
	}
	buffer := make([]uintptr, (length+word-1)/word)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, (*byte)(unsafe.Pointer(&buffer[0])), length, &length); err != nil {
		return "", fmt.Errorf("query native state token owner: %w", err)
	}
	sid := *(**windows.SID)(unsafe.Pointer(&buffer[0]))
	if sid == nil {
		return "", fmt.Errorf("native state token owner missing")
	}
	result := sid.String()
	runtime.KeepAlive(buffer)
	return result, nil
}

func checkRestrictedDACL(sd *windows.SECURITY_DESCRIPTOR, userSID string, directory bool) error {
	wantFlags := uint8(0)
	if directory {
		wantFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	return checkDACL(sd, userSID, wantFlags, true)
}

func checkDACL(sd *windows.SECURITY_DESCRIPTOR, userSID string, wantFlags uint8, requireProtected bool) error {
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if requireProtected && control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("DACL still inherits parent permissions")
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil || dacl.AceCount != 2 {
		return fmt.Errorf("expected exactly two explicit ACL entries")
	}
	want := map[string]int{userSID: 1}
	want["S-1-5-18"]++
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != wantFlags ||
			uint32(ace.Mask) != 0x1f01ff && uint32(ace.Mask) != windows.GENERIC_ALL {
			return fmt.Errorf("unexpected ACL access rights or inheritance")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if want[sid] == 0 {
			return fmt.Errorf("unexpected ACL principal")
		}
		want[sid]--
	}
	runtime.KeepAlive(sd)
	return nil
}
