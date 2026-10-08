package component

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sysProcAttr adds nothing: the job object ties the child to the service.
func sysProcAttr() *syscall.SysProcAttr { return nil }

// jobObject holds the child in a job that is killed when its last handle closes. The service holds
// the only handle, so the child, and anything it starts, dies with the service however it ends.
type jobObject struct{ h windows.Handle }

func contain(p *os.Process) (containment, error) {
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return nil, fmt.Errorf("OpenProcess(%d): %w", p.Pid, err)
	}
	defer windows.CloseHandle(proc)
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return &jobObject{h: job}, nil
}

func (j *jobObject) kill() error { return windows.TerminateJobObject(j.h, 1) }

// release closes the job, which kills anything the child left running.
func (j *jobObject) release() { _ = windows.CloseHandle(j.h) }
