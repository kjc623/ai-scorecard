//go:build !windows

package hostinfo

import "net/netip"

// OwnerOfLocalTCP is not supported here: the platform has no owner-annotated TCP table reader.
func OwnerOfLocalTCP(local, remote netip.AddrPort) (uint32, error) { return 0, ErrUnsupported }

// ProcessInfo is not supported here.
func ProcessInfo(pid uint32) (Process, error) { return Process{}, ErrUnsupported }

// ListenersOn is not supported here.
func ListenersOn(port int) ([]uint32, error) { return nil, ErrUnsupported }
