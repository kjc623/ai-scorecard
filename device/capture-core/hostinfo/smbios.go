package hostinfo

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// SystemInfo is what SMBIOS structure type 1 (System Information) states: the serial number the
// MDM inventories (Win32_BIOS.SerialNumber and Intune's serialNumber read this field) and the
// system UUID.
type SystemInfo struct {
	Serial string
	UUID   string
}

// smbiosEndOfTable is the type that closes the structure table.
const smbiosEndOfTable = 127

// ParseSMBIOS walks the structure table and returns the System Information fields. The table is
// read directly rather than through WMI so the agent needs no COM and no new dependency; WMI's
// Win32_BIOS.SerialNumber is this same field.
func ParseSMBIOS(raw RawSMBIOS) (SystemInfo, error) {
	t := raw.Table
	for off := 0; off+4 <= len(t); {
		typ, length := t[off], int(t[off+1])
		if length < 4 || off+length > len(t) {
			return SystemInfo{}, fmt.Errorf("structure at offset %d declares length %d past the table", off, length)
		}
		// The formatted area is followed by its string set: NUL-terminated strings ending in a
		// double NUL (just the double NUL when the structure has no strings).
		strStart := off + length
		end := bytes.Index(t[strStart:], []byte{0, 0})
		if end < 0 {
			return SystemInfo{}, fmt.Errorf("structure at offset %d has an unterminated string set", off)
		}
		strs := t[strStart : strStart+end]
		next := strStart + end + 2
		if typ == 1 {
			return systemInfo(t[off:off+length], strs, raw.Major, raw.Minor), nil
		}
		if typ == smbiosEndOfTable {
			break
		}
		off = next
	}
	return SystemInfo{}, errors.New("no System Information (type 1) structure")
}

func systemInfo(formatted, strs []byte, major, minor byte) SystemInfo {
	var out SystemInfo
	// Offset 7 is the serial number's string index (1-based; 0 means none).
	if len(formatted) > 7 {
		out.Serial = smbiosString(strs, formatted[7])
	}
	// Offsets 8..23 are the UUID (SMBIOS 2.1+).
	if len(formatted) >= 24 {
		out.UUID = formatSMBIOSUUID(formatted[8:24], major, minor)
	}
	return out
}

func smbiosString(strs []byte, index byte) string {
	if index == 0 || len(strs) == 0 {
		return ""
	}
	parts := bytes.Split(strs, []byte{0})
	if int(index) > len(parts) {
		return ""
	}
	return strings.TrimSpace(string(parts[index-1]))
}

// formatSMBIOSUUID renders the UUID the way Windows does (Win32_ComputerSystemProduct.UUID),
// lower-cased. From SMBIOS 2.6 the first three fields are little-endian; earlier tables are read as
// network order. All-zero and all-FF mean "not present" and "not set"; one widely shipped board
// UUID is a constant, so it is not an identity either.
func formatSMBIOSUUID(u []byte, major, minor byte) string {
	if bytes.Equal(u, make([]byte, 16)) || bytes.Equal(u, bytes.Repeat([]byte{0xff}, 16)) {
		return ""
	}
	b := append([]byte(nil), u...)
	if major > 2 || (major == 2 && minor >= 6) {
		b[0], b[1], b[2], b[3] = b[3], b[2], b[1], b[0]
		b[4], b[5] = b[5], b[4]
		b[6], b[7] = b[7], b[6]
	}
	s := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	if s == "03000200-0400-0500-0006-000700080009" {
		return ""
	}
	return s
}

// placeholderSerials are the serial strings firmware ships when the OEM never set one. They are
// the same on every such machine, so they must never seed an identity.
var placeholderSerials = map[string]bool{
	"to be filled by o.e.m.": true, "to be filled by oem": true, "default string": true,
	"system serial number": true, "chassis serial number": true, "serial number": true,
	"not specified": true, "not applicable": true, "not available": true, "none": true,
	"n/a": true, "na": true, "oem": true, "invalid": true, "123456789": true, "0123456789": true,
	"1234567890": true,
}

// PlaceholderSerial reports whether a serial is an OEM placeholder: a known filler string, or one
// character repeated (all zeros, all spaces, all X).
func PlaceholderSerial(s string) bool {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" || placeholderSerials[v] {
		return true
	}
	return strings.Count(v, v[:1]) == len(v)
}
