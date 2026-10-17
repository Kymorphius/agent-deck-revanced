//go:build windows

// matrix/hardened: ConPTY sessions have an unnamed, non-inheritable Job Object
// and exact native handles; teardown cannot target unrelated processes.
package conpty

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var kernel32 = windows.NewLazySystemDLL("kernel32.dll")
var createPseudoConsole = kernel32.NewProc("CreatePseudoConsole")
var updateProcThreadAttribute = kernel32.NewProc("UpdateProcThreadAttribute")

// Process owns one native terminal and its process tree. Clients may detach by
// stopping their own input/output forwarding, while the host keeps this object
// and continuously consumes Output. Close stops the tree and releases handles.
// A host crash also stops the tree through JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
type Process struct {
	pid          uint32
	input        *os.File
	nativeOutput *os.File
	output       *outputBuffer
	mu           sync.Mutex
	console      windows.Handle
	job          windows.Handle
	closed       bool
	closeOnce    sync.Once
	closeErr     error
	pumpDone     chan struct{}
	waitDone     chan struct{}
	exitCode     uint32
	waitErr      error
}

// Start creates the child suspended, assigns its job, then resumes it. No child
// code can run before process-tree ownership has been established.
func Start(options Options) (*Process, error) {
	return start(options, nil)
}

// matrix/hardened: per-call checkpoints let opt-in native tests exercise each
// startup cleanup owner. Start always supplies nil; there are no mutable global
// hooks, exported injection settings, or environment-triggered failure paths.
type launchStage uint8

const (
	launchConsoleCreated launchStage = iota
	launchChildCreated
	launchJobAssigned
	launchResuming
)

type launchCheckpoint func(launchStage, uint32) error

func start(options Options, checkpoint launchCheckpoint) (*Process, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	if err := createPseudoConsole.Find(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	dir := options.Dir
	if dir != "" {
		dir, err = filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("ConPTY working directory: %w", err)
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			if err == nil {
				err = errors.New("not a directory")
			}
			return nil, fmt.Errorf("ConPTY working directory: %w", err)
		}
	}
	executable := options.Executable
	if dir != "" && strings.ContainsAny(executable, `\/`) && !filepath.IsAbs(executable) {
		executable = filepath.Join(dir, executable)
	}
	executable, err = exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("ConPTY executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("ConPTY executable path: %w", err)
	}
	if extension := strings.ToLower(filepath.Ext(executable)); extension == ".cmd" || extension == ".bat" {
		return nil, errors.New("ConPTY requires an executable; launch batch scripts with an explicit shell interpreter")
	}
	line, err := commandLine(executable, options.Args)
	if err != nil {
		return nil, err
	}
	application, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, err
	}
	command, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return nil, err
	}
	var directory *uint16
	if dir != "" {
		directory, err = windows.UTF16PtrFromString(dir)
		if err != nil {
			return nil, err
		}
	}
	var environment []uint16
	var environmentPointer *uint16
	if options.Env != nil {
		environment, err = environmentBlock(options.Env)
		if err != nil {
			return nil, err
		}
		environmentPointer = &environment[0]
	}

	var inputRead, inputWrite, outputRead, outputWrite windows.Handle
	var console, job windows.Handle
	var child windows.ProcessInformation
	// matrix/hardened: all resources remain local until complete ownership is
	// transferred. Failed launches kill only the exact suspended child.
	defer func() {
		if child.Thread != 0 {
			_ = windows.CloseHandle(child.Thread)
		}
		if child.Process != 0 {
			_ = windows.TerminateProcess(child.Process, 1)
			_, _ = windows.WaitForSingleObject(child.Process, windows.INFINITE)
			_ = windows.CloseHandle(child.Process)
		}
		if job != 0 {
			_ = windows.CloseHandle(job)
		}
		for _, handle := range []windows.Handle{inputRead, inputWrite, outputWrite} {
			if handle != 0 {
				_ = windows.CloseHandle(handle)
			}
		}
		if console != 0 {
			// Even startup failure must drain shutdown frames on older Windows.
			reader := os.NewFile(uintptr(outputRead), "conpty-startup-output")
			drained := make(chan struct{})
			go func() {
				_, _ = io.Copy(io.Discard, reader)
				_ = reader.Close()
				close(drained)
			}()
			windows.ClosePseudoConsole(console)
			<-drained
			outputRead = 0
		}
		if outputRead != 0 {
			_ = windows.CloseHandle(outputRead)
		}
	}()
	if err = windows.CreatePipe(&inputRead, &inputWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("ConPTY input pipe: %w", err)
	}
	if err = windows.CreatePipe(&outputRead, &outputWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("ConPTY output pipe: %w", err)
	}
	if err = windows.CreatePseudoConsole(windows.Coord{X: int16(options.Cols), Y: int16(options.Rows)}, inputRead, outputWrite, 0, &console); err != nil {
		return nil, fmt.Errorf("create ConPTY: %w", err)
	}
	if checkpoint != nil {
		if err = checkpoint(launchConsoleCreated, 0); err != nil {
			return nil, err
		}
	}
	job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("ConPTY job: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, fmt.Errorf("ConPTY job limits: %w", err)
	}
	runtime.KeepAlive(limits)
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, fmt.Errorf("ConPTY process attributes: %w", err)
	}
	defer attributes.Delete()
	// This attribute takes the opaque HPCON value itself, not &console. A
	// native call avoids disguising an integer handle as a Go pointer, which
	// would violate checkptr when Windows integration tests run with -race.
	updated, _, attributeErr := updateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(attributes.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(console), unsafe.Sizeof(console), 0, 0)
	runtime.KeepAlive(attributes)
	if updated == 0 {
		return nil, fmt.Errorf("ConPTY console attribute: %w", attributeErr)
	}
	startup := windows.StartupInfoEx{ProcThreadAttributeList: attributes.List()}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	// matrix/hardened: Windows otherwise duplicates a redirected parent's
	// stdio even with inheritHandles=false, bypassing the attached ConPTY.
	// Request explicit NULL standard handles so console initialization creates
	// this terminal's handles, as used by Windows Terminal's ConPTY backend.
	// https://github.com/microsoft/terminal/discussions/15814
	startup.Flags = windows.STARTF_USESTDHANDLES
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	err = windows.CreateProcess(application, command, nil, nil, false, flags, environmentPointer, directory, &startup.StartupInfo, &child)
	runtime.KeepAlive(environment)
	runtime.KeepAlive(attributes)
	if err != nil {
		return nil, fmt.Errorf("create ConPTY child: %w", err)
	}
	if checkpoint != nil {
		if err = checkpoint(launchChildCreated, child.ProcessId); err != nil {
			return nil, err
		}
	}
	if err = windows.AssignProcessToJobObject(job, child.Process); err != nil {
		return nil, fmt.Errorf("assign ConPTY child to owned job: %w", err)
	}
	if checkpoint != nil {
		if err = checkpoint(launchJobAssigned, child.ProcessId); err != nil {
			return nil, err
		}
	}
	if err = windows.CloseHandle(inputRead); err != nil {
		return nil, fmt.Errorf("release ConPTY input endpoint: %w", err)
	}
	inputRead = 0
	if err = windows.CloseHandle(outputWrite); err != nil {
		return nil, fmt.Errorf("release ConPTY output endpoint: %w", err)
	}
	outputWrite = 0
	p := &Process{
		pid: child.ProcessId, input: os.NewFile(uintptr(inputWrite), "conpty-input"),
		nativeOutput: os.NewFile(uintptr(outputRead), "conpty-output"),
		output:       newOutputBuffer(outputBacklogLimit), console: console, job: job,
		pumpDone: make(chan struct{}), waitDone: make(chan struct{}),
	}
	processHandle := child.Process
	// Ownership is complete before the suspended thread can execute.
	inputWrite, outputRead, console, job, child.Process = 0, 0, 0, 0, 0
	go p.pumpOutput()
	go p.waitProcess(processHandle)
	if checkpoint != nil {
		if err = checkpoint(launchResuming, p.pid); err != nil {
			return nil, errors.Join(err, p.Close())
		}
	}
	if _, err = windows.ResumeThread(child.Thread); err != nil {
		return nil, errors.Join(fmt.Errorf("resume ConPTY child: %w", err), p.Close())
	}
	return p, nil
}

func (p *Process) PID() uint32       { return p.pid }
func (p *Process) Input() io.Writer  { return p.input }
func (p *Process) Output() io.Reader { return p.output }

func (p *Process) Resize(cols, rows int) error {
	if err := validateSize(cols, rows); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	return windows.ResizePseudoConsole(p.console, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Wait reports the original child's exit code. It is safe for concurrent calls
// and does not close the terminal or discard pending output. The host must call
// Close after Wait, including when the child has left descendants running.
func (p *Process) Wait() (uint32, error) {
	<-p.waitDone
	return p.exitCode, p.waitErr
}

func (p *Process) waitProcess(handle windows.Handle) {
	defer close(p.waitDone)
	result, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if err != nil {
		p.waitErr = fmt.Errorf("wait for ConPTY child: %w", err)
	} else if result != windows.WAIT_OBJECT_0 {
		p.waitErr = fmt.Errorf("unexpected ConPTY wait result: %d", result)
	} else if err = windows.GetExitCodeProcess(handle, &p.exitCode); err != nil {
		p.waitErr = fmt.Errorf("ConPTY child exit code: %w", err)
	}
	if err = windows.CloseHandle(handle); err != nil {
		p.waitErr = errors.Join(p.waitErr, fmt.Errorf("release ConPTY child handle: %w", err))
	}
}

func (p *Process) pumpOutput() {
	defer close(p.pumpDone)
	defer p.nativeOutput.Close()
	data := make([]byte, 32*1024)
	discard := false
	for {
		n, err := p.nativeOutput.Read(data)
		if n > 0 && !discard {
			if appendErr := p.output.append(data[:n]); appendErr != nil {
				discard = true
				// Keep draining native output while teardown executes elsewhere.
				go func() { _ = p.Close() }()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, windows.ERROR_BROKEN_PIPE) {
				err = nil
			}
			p.output.finish(err)
			if err != nil {
				// A broken transport cannot be a detached live session. Stop
				// only its owned tree while preserving the observed IO error.
				go func() { _ = p.Close() }()
			}
			return
		}
	}
}

// Close terminates only this object's job, releases the pseudoconsole, waits
// for its output pipe to close, and reaps the child handle. Buffered output can
// still be read afterwards. It is safe for concurrent or repeated calls.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		console, job := p.console, p.job
		p.console, p.job = 0, 0
		p.mu.Unlock()
		if err := windows.TerminateJobObject(job, 1); err != nil {
			p.closeErr = errors.Join(p.closeErr, fmt.Errorf("terminate owned ConPTY job: %w", err))
		}
		if err := windows.CloseHandle(job); err != nil {
			p.closeErr = errors.Join(p.closeErr, fmt.Errorf("release ConPTY job: %w", err))
		}
		if err := p.input.Close(); err != nil {
			p.closeErr = errors.Join(p.closeErr, fmt.Errorf("release ConPTY input: %w", err))
		}
		// On pre-24H2 systems this blocks until nativeOutput is drained. The
		// pump above never waits for a client, including after buffer overflow.
		windows.ClosePseudoConsole(console)
		<-p.pumpDone
		<-p.waitDone
		p.closeErr = errors.Join(p.closeErr, p.waitErr)
	})
	return p.closeErr
}
