//go:build windows

package procmon

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// snapshot lists the running processes (CreateToolhelp32Snapshot).
func snapshot() ([]proc, error) {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(h)
	var out []proc
	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(h, &e); err == nil; err = windows.Process32Next(h, &e) {
		out = append(out, proc{pid: e.ProcessID, parent: e.ParentProcessID, base: windows.UTF16ToString(e.ExeFile[:])})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("Process32Next: %w", err)
	}
	return out, nil
}

// vsFixedFileInfoSignature is VS_FIXEDFILEINFO's dwSignature.
const vsFixedFileInfoSignature = 0xFEEF04BD

// fileVersion is the file version in the image's version resource (VS_FIXEDFILEINFO), as
// major.minor.build.revision. The image is read as data, never loaded or run.
func fileVersion(image string) string {
	size, err := windows.GetFileVersionInfoSize(image, nil)
	if err != nil || size == 0 {
		return ""
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(image, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return ""
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var n uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &n); err != nil ||
		fixed == nil || n < uint32(unsafe.Sizeof(windows.VS_FIXEDFILEINFO{})) || fixed.Signature != vsFixedFileInfoSignature {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", fixed.FileVersionMS>>16, fixed.FileVersionMS&0xffff,
		fixed.FileVersionLS>>16, fixed.FileVersionLS&0xffff)
}
