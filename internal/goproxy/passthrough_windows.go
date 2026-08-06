//go:build windows

package goproxy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// isExecutable reports whether path is a regular file Windows would run.
// Windows has no execute permission bit, so this is an extension check plus an
// existence check.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}

	ext := strings.ToLower(filepath.Ext(path))
	for _, allowed := range strings.Split(strings.ToLower(os.Getenv("PATHEXT")), ";") {
		if strings.TrimSpace(allowed) == ext {
			return true
		}
	}

	// PATHEXT may be unset or unhelpful; fall back to the defaults that matter
	// for a toolchain binary.
	switch ext {
	case ".exe", ".bat", ".cmd", ".com":
		return true
	}

	return false
}

// Run executes the real Go toolchain as a child process and returns its exit
// code.
//
// Windows has no execve, so this cannot be true process replacement. To behave
// as close to it as possible:
//
//   - os.Stdin/Stdout/Stderr are wired straight to the child (no io.Pipe, no
//     buffering), so streaming output such as `go test -v` is not reordered or
//     delayed.
//   - The child is placed in a job object with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so if turango dies the child cannot
//     be orphaned: closing our last handle to the job kills everything in it.
//   - A console control handler claims Ctrl+C and Ctrl+Break so turango is not
//     torn down before it can reap the child. The child shares our console and
//     receives the same event directly, so it still gets the chance to shut
//     down cleanly, and we propagate whatever exit code it produces.
//   - The exit code is read from *exec.ExitError explicitly. A failure that is
//     not an *exec.ExitError (binary missing, access denied) carries no usable
//     exit code and is reported as a turango-side error instead.
func Run(goBin string, args []string) (int, error) {
	cmd := exec.Command(goBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	// Claim console control events before the child exists, so an interrupt
	// arriving during startup cannot kill us mid-launch.
	holdConsoleCtrlEvents()

	if err := cmd.Start(); err != nil {
		return 1, execError(goBin, err)
	}

	// Best-effort: a failure here only costs us the orphan guarantee, not
	// correctness of the passthrough itself.
	if job, err := assignToKillOnCloseJob(cmd.Process.Pid); err == nil {
		defer syscall.CloseHandle(job)
	}

	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}

		return 1, execError(goBin, err)
	}

	return 0, nil
}

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW     = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJob    = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob   = kernel32.NewProc("AssignProcessToJobObject")
	procSetConsoleCtrlHandle = kernel32.NewProc("SetConsoleCtrlHandler")
)

const (
	jobObjectExtendedLimitInformationClass = 9
	jobObjectLimitKillOnJobClose           = 0x00002000

	processTerminate = 0x0001
	processSetQuota  = 0x0100
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// assignToKillOnCloseJob puts pid into a fresh job object configured to kill
// its members when the job's last handle closes — which happens when turango
// exits, however it exits. The caller owns the returned handle.
func assignToKillOnCloseJob(pid int) (syscall.Handle, error) {
	jobRaw, _, err := procCreateJobObjectW.Call(0, 0)
	job := syscall.Handle(jobRaw)
	if job == 0 {
		return 0, err
	}

	limits := jobObjectExtendedLimitInformation{}
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose

	ok, _, err := procSetInformationJob.Call(
		uintptr(job),
		uintptr(jobObjectExtendedLimitInformationClass),
		uintptr(unsafe.Pointer(&limits)),
		unsafe.Sizeof(limits),
	)
	if ok == 0 {
		syscall.CloseHandle(job)

		return 0, err
	}

	process, oerr := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if oerr != nil {
		syscall.CloseHandle(job)

		return 0, oerr
	}
	defer syscall.CloseHandle(process)

	ok, _, err = procAssignProcessToJob.Call(uintptr(job), uintptr(process))
	if ok == 0 {
		syscall.CloseHandle(job)

		return 0, err
	}

	return job, nil
}

// consoleCtrlHandler must stay reachable for the lifetime of the process: the
// OS holds a raw pointer to the generated callback.
var consoleCtrlHandler = syscall.NewCallback(func(ctrlType uint32) uintptr {
	// Returning TRUE marks the event handled so the default handler does not
	// terminate turango. The child shares this console and receives the same
	// event independently, so it still shuts down; we stay alive only long
	// enough to reap it and report its exit code.
	return 1
})

// holdConsoleCtrlEvents installs consoleCtrlHandler. Failures are ignored: the
// only consequence is the pre-existing default behaviour.
func holdConsoleCtrlEvents() {
	procSetConsoleCtrlHandle.Call(consoleCtrlHandler, 1)
}
