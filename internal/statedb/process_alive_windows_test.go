//go:build windows

// matrix/hardened: Windows owner liveness acceptance checks only self and an explicitly owned child.
package statedb

import (
	"io"
	"os"
	"os/exec"
	"testing"
)

func TestWindowsPIDAliveChild(t *testing.T) {
	if os.Getenv("AGENTDECK_LIVENESS_CHILD") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestWindowsPIDAliveSelfChildAndExited(t *testing.T) {
	if os.Getenv("AGENTDECK_NATIVE_TEST") != "1" {
		t.Skip("set AGENTDECK_NATIVE_TEST=1 for owned Windows child liveness acceptance")
	}
	if !pidAlive(os.Getpid()) || pidAlive(0) || pidAlive(-1) || pidAlive(2147483647) {
		t.Fatal("Windows self/invalid PID liveness mismatch")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestWindowsPIDAliveChild$")
	child.Env = append(os.Environ(), "AGENTDECK_LIVENESS_CHILD=1")
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		input.Close()
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	}()
	if !pidAlive(child.Process.Pid) {
		t.Fatal("live owned child considered dead")
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if pidAlive(child.Process.Pid) {
		t.Fatal("exited owned child considered alive")
	}
}
