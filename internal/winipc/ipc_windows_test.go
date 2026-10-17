//go:build windows

// matrix/hardened: Windows acceptance tests are opt-in and operate only on owned temporary objects.
package winipc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func requireNativeTest(t *testing.T) {
	t.Helper()
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 to exercise owned native Windows IPC")
	}
}

func TestWindowsPipeRoundTripAndACL(t *testing.T) {
	requireNativeTest(t)
	path := fmt.Sprintf("%stest-%d-%d", PipePrefix, os.Getpid(), time.Now().UnixNano())
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if duplicate, err := Listen(path); err == nil {
		duplicate.Close()
		t.Fatal("second listener unexpectedly acquired the same name")
	}
	payload := []byte("native IPC 中文 round trip\r\n")
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			result <- err
			return
		}
		buffer := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, buffer); err != nil {
			result <- err
			return
		}
		if !bytes.Equal(buffer, payload) {
			result <- fmt.Errorf("server payload mismatch")
			return
		}
		_, err = conn.Write(buffer)
		result <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	testPipeACL(t, conn)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buffer, payload) {
		t.Fatal("client payload mismatch")
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if unknown, err := Dial(cancelled, path+"-missing"); err == nil {
		unknown.Close()
		t.Fatal("cancelled dial unexpectedly succeeded")
	}
}

func testPipeACL(t *testing.T, conn net.Conn) {
	t.Helper()
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		t.Fatal("named pipe connection has no native handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fd.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := CurrentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRestrictedDACL(sd, sid, false); err != nil {
		t.Fatalf("pipe ACL readback: %v", err)
	}
}

func TestWindowsProtectPathACLAndScope(t *testing.T) {
	requireNativeTest(t)
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	preexisting := filepath.Join(state, "preexisting.txt")
	payload := []byte("preserve native state 中文")
	if err := os.WriteFile(preexisting, payload, 0600); err != nil {
		t.Fatal(err)
	}
	before := readStateACL(t, preexisting).String()
	if err := ProtectPath(state); err != nil {
		t.Fatal(err)
	}
	if after := readStateACL(t, preexisting).String(); before != after {
		t.Fatalf("directory ACL operation changed existing child ACL: before %s, after %s", before, after)
	}
	sid, err := CurrentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRestrictedDACL(readStateACL(t, state), sid, true); err != nil {
		t.Fatal(err)
	}
	future := filepath.Join(state, "future.txt")
	if err := os.WriteFile(future, payload, 0600); err != nil {
		t.Fatal(err)
	}
	// A newly created child has inherited ACE flags until it is explicitly
	// protected. Native readback must show no additional principals.
	if err := checkDACL(readStateACL(t, future), sid, windows.INHERITED_ACE, false); err != nil {
		t.Fatalf("new child inherited unsafe ACL: %v", err)
	}
	for _, path := range []string{preexisting, future} {
		ownerBefore, _, err := readStateACL(t, path).Owner()
		if err != nil {
			t.Fatal(err)
		}
		ownerSID := ownerBefore.String()
		if err := ProtectPath(path); err != nil {
			t.Fatal(err)
		}
		if err := checkRestrictedDACL(readStateACL(t, path), sid, false); err != nil {
			t.Fatal(err)
		}
		ownerAfter, _, err := readStateACL(t, path).Owner()
		if err != nil || ownerAfter.String() != ownerSID {
			t.Fatalf("state owner changed: %v", err)
		}
		if contents, err := os.ReadFile(path); err != nil || !bytes.Equal(contents, payload) {
			t.Fatalf("state data changed: %q, %v", contents, err)
		}
	}
	if err := ProtectPath(filepath.Join(state, "absent")); err == nil {
		t.Fatal("missing state file unexpectedly protected")
	}
}

func TestWindowsProtectPathRejectsAliases(t *testing.T) {
	requireNativeTest(t)
	root := t.TempDir()
	file := filepath.Join(root, "state.txt")
	if err := os.WriteFile(file, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	before := readStateACL(t, file).String()
	hardlink := filepath.Join(root, "hardlink.txt")
	if err := os.Link(file, hardlink); err != nil {
		t.Fatal(err)
	}
	if err := ProtectPath(hardlink); err == nil {
		t.Fatal("hard-linked state file unexpectedly protected")
	}
	if after := readStateACL(t, file).String(); before != after {
		t.Fatal("rejected hard link changed original ACL")
	}
	symlink := filepath.Join(root, "symlink.txt")
	if err := os.Symlink(file, symlink); err != nil {
		t.Logf("symlink subcheck unavailable under this token: %v", err)
		return
	}
	if err := ProtectPath(symlink); err == nil {
		t.Fatal("reparse-point state path unexpectedly protected")
	}
	if after := readStateACL(t, file).String(); before != after {
		t.Fatal("rejected reparse point changed target ACL")
	}
}

func readStateACL(t *testing.T, path string) *windows.SECURITY_DESCRIPTOR {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		t.Fatalf("read state ACL %s: %v", path, err)
	}
	return sd
}

func TestWindowsACLVerificationRejectsBroadAccess(t *testing.T) {
	requireNativeTest(t)
	sid, err := CurrentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, sddl := range []string{
		"D:P(A;;FA;;;" + sid + ")(A;;FA;;;WD)",
		"D:P(A;;FA;;;" + sid + ")",
		"D:(A;;FA;;;" + sid + ")(A;;FA;;;SY)",
		"D:P(A;;FR;;;" + sid + ")(A;;FA;;;SY)",
		"D:P(D;;FA;;;" + sid + ")(A;;FA;;;SY)",
		"D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)",
	} {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkRestrictedDACL(sd, sid, false); err == nil {
			t.Fatalf("unsafe ACL accepted: %s", sddl)
		}
	}
}
