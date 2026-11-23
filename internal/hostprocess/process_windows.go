//go:build windows

package hostprocess

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type processTree struct {
	mu                sync.Mutex
	job               windows.Handle
	executionJob      windows.Handle
	identity          string
	executionIdentity string
}

func (cmd *Command) configure() {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
}

// SetJobIdentity lets workspace acquisition observe an execution still being
// terminated after its worker has died. It must be set before Start.
func (cmd *Command) SetJobIdentity(identity string) { cmd.tree.identity = identity }

// SetExecutionIdentity adds a nested job for one attempt without weakening the
// workspace-wide guard. An orphan can query its own attempt after a new writer
// has legitimately acquired the same workspace.
func (cmd *Command) SetExecutionIdentity(identity string) { cmd.tree.executionIdentity = identity }

func jobName(identity string) (*uint16, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.UTF16PtrFromString(fmt.Sprintf(`Global\Vantry-%s-%x`, user.User.Sid.String(), sha256.Sum256([]byte(identity))))
}

func createJob(identity string) (windows.Handle, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return 0, err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;;GA;;;" + user.User.Sid.String() + ")(A;;GA;;;SY)")
	if err != nil {
		return 0, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	var name *uint16
	if identity != "" {
		name, err = jobName(identity)
		if err != nil {
			return 0, err
		}
	}
	// The x/sys wrapper discards ERROR_ALREADY_EXISTS on a successful
	// handle. Inspect it here: adopting someone else's job is never allowed.
	h, _, callErr := createJobObject.Call(uintptr(unsafe.Pointer(sa)), uintptr(unsafe.Pointer(name)))
	job := windows.Handle(h)
	if job == 0 || errors.Is(callErr, windows.ERROR_ALREADY_EXISTS) {
		if job != 0 {
			windows.CloseHandle(job)
		}
		return 0, fmt.Errorf("create owned process job: %w", callErr)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func (cmd *Command) start() error {
	cmd.tree.mu.Lock()
	defer cmd.tree.mu.Unlock()
	if cmd.tree.job != 0 {
		return errors.New("command was already started")
	}
	job, err := createJob(cmd.tree.identity)
	if err != nil {
		return err
	}
	cmd.tree.job = job
	if cmd.tree.executionIdentity != "" {
		execution, err := createJob(cmd.tree.executionIdentity)
		if err != nil {
			windows.CloseHandle(job)
			cmd.tree.job = 0
			return err
		}
		cmd.tree.executionJob = execution
	}
	fail := func(err error) error {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		windows.CloseHandle(job)
		cmd.tree.job = 0
		if cmd.tree.executionJob != 0 {
			windows.CloseHandle(cmd.tree.executionJob)
			cmd.tree.executionJob = 0
		}
		// Wait must be allowed to finish its context watcher and pipe copies.
		// Its cancellation callback also takes tree.mu.
		if cmd.Process != nil {
			cmd.tree.mu.Unlock()
			_ = cmd.Cmd.Wait()
			cmd.tree.mu.Lock()
		}
		return err
	}
	// No child code runs before assignment, so it cannot spawn a descendant
	// outside this job. Failure to assign is fatal; no breakaway is enabled.
	if err := cmd.Cmd.Start(); err != nil {
		return fail(err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return fail(fmt.Errorf("assign child to owned job: %w", err))
	}
	if cmd.tree.executionJob != 0 {
		if err := windows.AssignProcessToJobObject(cmd.tree.executionJob, process); err != nil {
			return fail(fmt.Errorf("assign child to execution job: %w", err))
		}
	}
	// os/exec closes CreateProcess's initial thread handle. The process is
	// still suspended and has exactly one thread; recover that owned handle.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(cmd.Process.Pid) {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fail(err)
		}
		_, err = windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if err != nil {
			return fail(err)
		}
		return nil
	}
	return fail(fmt.Errorf("find suspended child thread: %w", err))
}

func (cmd *Command) KillTree() error {
	cmd.tree.mu.Lock()
	defer cmd.tree.mu.Unlock()
	if cmd.tree.job == 0 {
		return os.ErrProcessDone
	}
	return windows.TerminateJobObject(cmd.tree.job, 1)
}

type jobAccounting struct {
	TotalUser, TotalKernel, PeriodUser, PeriodKernel                 int64
	PageFaults, TotalProcesses, ActiveProcesses, TerminatedProcesses uint32
}

func activeProcesses(job windows.Handle) (uint32, error) {
	var info jobAccounting
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0, err
	}
	return info.ActiveProcesses, nil
}

func (cmd *Command) EnsureGone() { _ = cmd.KillTree() }

func (cmd *Command) Close() error {
	cmd.tree.mu.Lock()
	defer cmd.tree.mu.Unlock()
	if cmd.tree.job == 0 {
		return nil
	}
	job := cmd.tree.job
	defer func() {
		if cmd.tree.executionJob != 0 {
			windows.CloseHandle(cmd.tree.executionJob)
			cmd.tree.executionJob = 0
		}
		windows.CloseHandle(job)
		cmd.tree.job = 0
	}()
	if err := windows.TerminateJobObject(job, 1); err != nil {
		return err
	}
	for {
		active, err := activeProcesses(job)
		if err != nil || active == 0 {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

var (
	jobKernel       = windows.NewLazySystemDLL("kernel32.dll")
	openJobObject   = jobKernel.NewProc("OpenJobObjectW")
	createJobObject = jobKernel.NewProc("CreateJobObjectW")
)

// JobActive never signals a PID. The named kernel job remains alive while any
// of its processes are terminating, even after its last user handle is closed.
func JobActive(identity string) (bool, error) {
	name, err := jobName(identity)
	if err != nil {
		return false, err
	}
	const jobObjectQuery = 0x0004
	h, _, callErr := openJobObject.Call(jobObjectQuery, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) {
			return false, nil
		}
		return false, callErr
	}
	defer windows.CloseHandle(windows.Handle(h))
	active, err := activeProcesses(windows.Handle(h))
	return active != 0, err
}

func ConfigureDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
}
