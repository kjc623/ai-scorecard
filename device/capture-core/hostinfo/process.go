package hostinfo

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// Process is a running process as the operating system describes it: which executable, run by whom,
// signed by which publisher.
type Process struct {
	PID uint32
	// Image is the full path of the process's executable.
	Image string
	// Publisher is the common name of the image's Authenticode signer, when the signature verifies;
	// empty for an unsigned image or one whose signature does not verify.
	Publisher string
	// User is the account the process runs as; nil when its token cannot be read.
	User *User
	// Started is the process's creation time. A PID is reused once its process ends; the pair
	// (PID, Started) names one process.
	Started time.Time
}

// ProcessInfo answers are kept this long, for at most this many processes.
const (
	processCacheTTL = 10 * time.Minute
	processCacheMax = 1024
)

type processKey struct {
	pid     uint32
	started int64
}

type processEntry struct {
	p  Process
	at time.Time
}

// processCache remembers ProcessInfo answers by (PID, creation time), so a reused PID misses
// rather than returning the previous process's answer.
type processCache struct {
	ttl time.Duration
	max int

	mu      sync.Mutex
	entries map[processKey]processEntry
}

func newProcessCache(ttl time.Duration, max int) *processCache {
	return &processCache{ttl: ttl, max: max, entries: map[processKey]processEntry{}}
}

func (c *processCache) get(pid uint32, started, now time.Time) (Process, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := processKey{pid, started.UnixNano()}
	e, ok := c.entries[k]
	if !ok {
		return Process{}, false
	}
	if now.Sub(e.at) >= c.ttl {
		delete(c.entries, k)
		return Process{}, false
	}
	return e.p.clone(), true
}

// put stores p. A full cache first drops what has expired, then the oldest entry.
func (c *processCache) put(p Process, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := processKey{p.PID, p.Started.UnixNano()}
	if _, ok := c.entries[k]; !ok && len(c.entries) >= c.max {
		var oldest processKey
		var oldestAt time.Time
		for ek, e := range c.entries {
			if now.Sub(e.at) >= c.ttl {
				delete(c.entries, ek)
				continue
			}
			if oldestAt.IsZero() || e.at.Before(oldestAt) {
				oldest, oldestAt = ek, e.at
			}
		}
		if len(c.entries) >= c.max {
			delete(c.entries, oldest)
		}
	}
	c.entries[k] = processEntry{p: p.clone(), at: now}
}

// clone copies p so a caller never shares the cached User.
func (p Process) clone() Process {
	if p.User != nil {
		u := *p.User
		p.User = &u
	}
	return p
}

// tcpRow is one connection in the system's TCP table: its two endpoints and the owning process.
type tcpRow struct {
	local, remote netip.AddrPort
	pid           uint32
}

// The row sizes of GetExtendedTcpTable's TCP_TABLE_OWNER_PID_* tables: MIB_TCPROW_OWNER_PID (six
// DWORDs) and MIB_TCP6ROW_OWNER_PID (two 16-byte addresses and six DWORDs). Each table is a DWORD
// row count followed by the rows.
const (
	tcp4RowSize = 24
	tcp6RowSize = 56
)

// parseTCP4Table reads a MIB_TCPTABLE_OWNER_PID. Addresses are in network byte order; a port is
// the low 16 bits of its DWORD, also in network byte order.
func parseTCP4Table(buf []byte) ([]tcpRow, error) {
	rows, err := tableRows(buf, tcp4RowSize)
	if err != nil {
		return nil, err
	}
	out := make([]tcpRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, tcpRow{
			local:  netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[4:8])), binary.BigEndian.Uint16(r[8:10])),
			remote: netip.AddrPortFrom(netip.AddrFrom4([4]byte(r[12:16])), binary.BigEndian.Uint16(r[16:18])),
			pid:    binary.LittleEndian.Uint32(r[20:24]),
		})
	}
	return out, nil
}

// parseTCP6Table reads a MIB_TCP6TABLE_OWNER_PID. The scope ids are not needed to match a
// connection on one host and are left out.
func parseTCP6Table(buf []byte) ([]tcpRow, error) {
	rows, err := tableRows(buf, tcp6RowSize)
	if err != nil {
		return nil, err
	}
	out := make([]tcpRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, tcpRow{
			local:  netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[0:16])), binary.BigEndian.Uint16(r[20:22])),
			remote: netip.AddrPortFrom(netip.AddrFrom16([16]byte(r[24:40])), binary.BigEndian.Uint16(r[44:46])),
			pid:    binary.LittleEndian.Uint32(r[52:56]),
		})
	}
	return out, nil
}

func tableRows(buf []byte, size int) ([][]byte, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("hostinfo: TCP table of %d bytes has no row count", len(buf))
	}
	n := int(binary.LittleEndian.Uint32(buf[0:4]))
	if n < 0 || n > (len(buf)-4)/size {
		return nil, fmt.Errorf("hostinfo: TCP table claims %d rows in %d bytes", n, len(buf))
	}
	rows := make([][]byte, n)
	for i := range rows {
		rows[i] = buf[4+i*size : 4+(i+1)*size]
	}
	return rows, nil
}

// clientRow finds the client's half of a connection given the server's view of it: the row whose
// local endpoint is the server's remote and whose remote endpoint is the server's local. The
// server's own row has the endpoints the other way round and is never the answer. A row with PID 0
// (a connection in TIME_WAIT) names no process.
func clientRow(rows []tcpRow, local, remote netip.AddrPort) (uint32, bool) {
	for _, r := range rows {
		if sameEndpoint(r.local, remote) && sameEndpoint(r.remote, local) && r.pid != 0 {
			return r.pid, true
		}
	}
	return 0, false
}

func sameEndpoint(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().WithZone("") == b.Addr().WithZone("")
}
