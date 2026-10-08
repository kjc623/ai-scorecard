package hostinfo

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"testing"
	"time"
)

// tcp4Table builds a MIB_TCPTABLE_OWNER_PID as Windows lays it out: ports in network byte order in
// the low 16 bits of their DWORD, with garbage in the high bits the reader must ignore.
func tcp4Table(rows ...tcpRow) []byte {
	buf := make([]byte, 4+len(rows)*tcp4RowSize)
	binary.LittleEndian.PutUint32(buf, uint32(len(rows)))
	for i, r := range rows {
		b := buf[4+i*tcp4RowSize:]
		binary.LittleEndian.PutUint32(b[0:], 5) // MIB_TCP_STATE_ESTAB
		l, rm := r.local.Addr().As4(), r.remote.Addr().As4()
		copy(b[4:8], l[:])
		binary.BigEndian.PutUint16(b[8:], r.local.Port())
		b[10], b[11] = 0xAA, 0xBB
		copy(b[12:16], rm[:])
		binary.BigEndian.PutUint16(b[16:], r.remote.Port())
		b[18], b[19] = 0xCC, 0xDD
		binary.LittleEndian.PutUint32(b[20:], r.pid)
	}
	return buf
}

// tcp6Table builds a MIB_TCP6TABLE_OWNER_PID.
func tcp6Table(rows ...tcpRow) []byte {
	buf := make([]byte, 4+len(rows)*tcp6RowSize)
	binary.LittleEndian.PutUint32(buf, uint32(len(rows)))
	for i, r := range rows {
		b := buf[4+i*tcp6RowSize:]
		l, rm := r.local.Addr().As16(), r.remote.Addr().As16()
		copy(b[0:16], l[:])
		binary.BigEndian.PutUint16(b[20:], r.local.Port())
		copy(b[24:40], rm[:])
		binary.BigEndian.PutUint16(b[44:], r.remote.Port())
		binary.LittleEndian.PutUint32(b[48:], 5)
		binary.LittleEndian.PutUint32(b[52:], r.pid)
	}
	return buf
}

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// A loopback connection has two rows, one per end. Given the server's view, the client's row is
// the answer, never the server's own.
func TestClientRowPicksTheDiallingEnd(t *testing.T) {
	server, client := ap("127.0.0.1:8443"), ap("127.0.0.1:50123")
	table := tcp4Table(
		tcpRow{local: ap("0.0.0.0:8443"), remote: ap("0.0.0.0:0"), pid: 100},    // the listener
		tcpRow{local: server, remote: client, pid: 100},                         // the server's end
		tcpRow{local: ap("127.0.0.1:50999"), remote: server, pid: 0},            // TIME_WAIT
		tcpRow{local: client, remote: server, pid: 4242},                        // the client's end
		tcpRow{local: ap("10.0.0.5:50123"), remote: ap("1.2.3.4:443"), pid: 77}, // unrelated
	)
	rows, err := parseTCP4Table(table)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 5 || rows[3] != (tcpRow{local: client, remote: server, pid: 4242}) {
		t.Fatalf("rows = %+v", rows)
	}
	if pid, ok := clientRow(rows, server, client); !ok || pid != 4242 {
		t.Fatalf("clientRow = %d, %v; want 4242", pid, ok)
	}
	if _, ok := clientRow(rows, server, ap("127.0.0.1:50999")); ok {
		t.Fatal("a TIME_WAIT row with PID 0 named a process")
	}
	if _, ok := clientRow(rows, server, ap("127.0.0.1:1")); ok {
		t.Fatal("a connection the table does not hold was found")
	}
}

func TestClientRowReadsTheIPv6Table(t *testing.T) {
	server, client := ap("[::1]:8443"), ap("[::1]:50123")
	rows, err := parseTCP6Table(tcp6Table(
		tcpRow{local: server, remote: client, pid: 100},
		tcpRow{local: client, remote: server, pid: 4242},
	))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pid, ok := clientRow(rows, server, client); !ok || pid != 4242 {
		t.Fatalf("clientRow = %d, %v; want 4242", pid, ok)
	}
	// An IPv4 connection on a dual-stack socket is listed with v4-mapped addresses.
	mServer, mClient := ap("[::ffff:127.0.0.1]:8443"), ap("[::ffff:127.0.0.1]:50123")
	rows, err = parseTCP6Table(tcp6Table(tcpRow{local: mClient, remote: mServer, pid: 9}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pid, ok := clientRow(rows, mServer, mClient); !ok || pid != 9 {
		t.Fatalf("clientRow over v4-mapped rows = %d, %v; want 9", pid, ok)
	}
}

func TestTCPTableRejectsATruncatedBuffer(t *testing.T) {
	full := tcp4Table(tcpRow{local: ap("127.0.0.1:1"), remote: ap("127.0.0.1:2"), pid: 1})
	for _, buf := range [][]byte{nil, full[:3], full[:len(full)-1]} {
		if _, err := parseTCP4Table(buf); err == nil {
			t.Errorf("a %d-byte table parsed", len(buf))
		}
	}
	six := tcp6Table(tcpRow{local: ap("[::1]:1"), remote: ap("[::1]:2"), pid: 1})
	if _, err := parseTCP6Table(six[:len(six)-1]); err == nil {
		t.Error("a truncated IPv6 table parsed")
	}
}

// The cache answers for one process: the same PID with another creation time is a different
// process and misses, an answer older than the TTL misses, and the cache never grows past its cap.
func TestProcessCacheKeysOnPIDAndStart(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	started := now.Add(-time.Hour)
	c := newProcessCache(10*time.Minute, 3)
	c.put(Process{PID: 7, Image: `C:\a.exe`, Started: started, User: &User{SID: "S-1-5-21-1"}}, now)

	got, ok := c.get(7, started, now.Add(9*time.Minute))
	if !ok || got.Image != `C:\a.exe` {
		t.Fatalf("get within the TTL = %+v, %v", got, ok)
	}
	got.User.SID = "changed"
	if again, _ := c.get(7, started, now); again.User.SID != "S-1-5-21-1" {
		t.Fatal("a caller's change reached the cached answer")
	}
	if _, ok := c.get(7, started.Add(time.Second), now); ok {
		t.Fatal("a reused PID returned the previous process's answer")
	}
	if _, ok := c.get(7, started, now.Add(10*time.Minute)); ok {
		t.Fatal("an answer older than the TTL was returned")
	}
}

func TestProcessCacheIsBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newProcessCache(10*time.Minute, 3)
	for i := range 3 {
		c.put(Process{PID: uint32(i + 1), Image: fmt.Sprint(i + 1), Started: now}, now.Add(time.Duration(i)*time.Second))
	}
	c.put(Process{PID: 4, Started: now}, now.Add(time.Minute))
	if len(c.entries) != 3 {
		t.Fatalf("cache holds %d entries, want at most 3", len(c.entries))
	}
	if _, ok := c.get(1, now, now.Add(time.Minute)); ok {
		t.Fatal("the oldest entry was kept past the cap")
	}
	for _, pid := range []uint32{2, 3, 4} {
		if _, ok := c.get(pid, now, now.Add(time.Minute)); !ok {
			t.Fatalf("PID %d was evicted instead of the oldest", pid)
		}
	}
	// Expired entries go first: after the TTL a full cache makes room without evicting a live one.
	later := now.Add(time.Minute + 10*time.Minute)
	c.put(Process{PID: 5, Started: now}, later)
	c.put(Process{PID: 6, Started: now}, later)
	if len(c.entries) > 3 {
		t.Fatalf("cache holds %d entries, want at most 3", len(c.entries))
	}
	for _, pid := range []uint32{5, 6} {
		if _, ok := c.get(pid, now, later); !ok {
			t.Fatalf("PID %d was evicted while expired entries remained", pid)
		}
	}
}
