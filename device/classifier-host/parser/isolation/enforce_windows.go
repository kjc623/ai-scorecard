package isolation

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// setProcessAttributes keeps a console window from appearing for the child.
func setProcessAttributes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// jobEnforcer places the child in a job object that is killed when its handle closes and, when a
// memory limit is set, caps each process's commit charge. The job's peak process memory is the
// sample: it cannot miss a spike between two polls.
type jobEnforcer struct {
	job windows.Handle
}

func newEnforcer(pid int, memoryBytes int64) (enforcer, error) {
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return nil, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(proc)
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if memoryBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.ProcessMemoryLimit = uintptr(memoryBytes)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return &jobEnforcer{job: job}, nil
}

func (e *jobEnforcer) sample() (int64, bool) {
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	err := windows.QueryInformationJobObject(e.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	if err != nil {
		return 0, false
	}
	return int64(info.PeakProcessMemoryUsed), true
}

func (e *jobEnforcer) peak() int64 {
	n, _ := e.sample()
	return n
}

func (e *jobEnforcer) kill() error { return windows.TerminateJobObject(e.job, 1) }

func (e *jobEnforcer) close() { windows.CloseHandle(e.job) }
