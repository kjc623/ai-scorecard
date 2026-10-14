//go:build windows

package procmon

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
