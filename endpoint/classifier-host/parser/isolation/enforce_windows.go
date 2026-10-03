//go:build windows && !js

package isolation

import (
	"fmt"
	"syscall"
	"unsafe"
)

// The Windows enforcer is §10's "job object on Windows": a job with
// JOB_OBJECT_LIMIT_PROCESS_MEMORY caps the child's commit charge, and
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE guarantees no orphan survives the parent. It is created and
// the child assigned immediately after CreateProcess, before the document is written to the
// child's stdin.
//
// The job cap is not the only enforcement: the parent also samples the child's commit charge
// (GetProcessMemoryInfo) and terminates on breach, because a job limit is reported to the child
// as an allocation failure — which a compromised or broken parser could catch and continue past —
// while the parent's kill is unconditional.

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitProcessMemory       = 0x0100
	jobObjectLimitKillOnJobClose      = 0x2000
	// PROCESS_TERMINATE and PROCESS_SET_QUOTA; Go's syscall package exports neither on Windows,
	// because only a caller that assigns job objects needs them.
	processTerminate = 0x0001
	processSetQuota  = 0x0100
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	psapi    = syscall.NewLazyDLL("psapi.dll")

	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procGetProcessMemoryInfo      = psapi.NewProc("GetProcessMemoryInfo")
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobBasicLimitInformation struct {
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

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type windowsEnforcer struct {
	proc syscall.Handle
	job  syscall.Handle
}

func newEnforcer(pid int, memCap, residencyCap int64) (enforcer, error) {
	const access = syscall.PROCESS_QUERY_INFORMATION | processSetQuota | processTerminate
	h, err := syscall.OpenProcess(access, false, uint32(pid))
	if err != nil {
		return nil, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	e := &windowsEnforcer{proc: h}
	if memCap <= 0 {
		return e, nil
	}
	job, _, errno := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return e, fmt.Errorf("CreateJobObject: %v", errno)
	}
	e.job = syscall.Handle(job)
	info := jobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitProcessMemory | jobObjectLimitKillOnJobClose
	info.ProcessMemoryLimit = uintptr(memCap)
	if r1, _, errno := procSetInformationJobObject.Call(uintptr(e.job), jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r1 == 0 {
		e.closeJob()
		return e, fmt.Errorf("SetInformationJobObject: %v", errno)
	}
	if r1, _, errno := procAssignProcessToJobObject.Call(uintptr(e.job), uintptr(h)); r1 == 0 {
		e.closeJob()
		return e, fmt.Errorf("AssignProcessToJobObject: %v", errno)
	}
	return e, nil
}

func (e *windowsEnforcer) sample() (int64, bool) {
	var pmc processMemoryCounters
	pmc.Cb = uint32(unsafe.Sizeof(pmc))
	r1, _, _ := procGetProcessMemoryInfo.Call(uintptr(e.proc), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.Cb))
	if r1 == 0 {
		return 0, false
	}
	return int64(pmc.PagefileUsage), true
}

// peak prefers the job's own high-water mark, which cannot miss a spike between samples.
func (e *windowsEnforcer) peak() int64 {
	if e.job != 0 {
		info := jobExtendedLimitInformation{}
		if r1, _, _ := procQueryInformationJobObject.Call(uintptr(e.job), jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0); r1 != 0 {
			if info.PeakProcessMemoryUsed > 0 {
				return int64(info.PeakProcessMemoryUsed)
			}
		}
	}
	n, _ := e.sample()
	return n
}

// kill terminates the whole job when there is one — including any grandchild the parser spawned —
// and falls back to the process itself.
func (e *windowsEnforcer) kill() error {
	if e.job != 0 {
		if r1, _, errno := procTerminateJobObject.Call(uintptr(e.job), 1); r1 == 0 {
			return fmt.Errorf("TerminateJobObject: %v", errno)
		}
		return nil
	}
	if e.proc == 0 {
		return nil
	}
	return syscall.TerminateProcess(e.proc, 1)
}

func (e *windowsEnforcer) closeJob() {
	if e.job != 0 {
		syscall.CloseHandle(e.job)
		e.job = 0
	}
}

func (e *windowsEnforcer) close() {
	e.closeJob()
	if e.proc != 0 {
		syscall.CloseHandle(e.proc)
		e.proc = 0
	}
}
