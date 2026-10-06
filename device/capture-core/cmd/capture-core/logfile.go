package main

import (
	"os"
	"sync"
)

// maxLogBytes is the size at which the service log rolls to capture-core.log.1.
const maxLogBytes = 10 << 20

// rotatingLog is the Windows service's log file: appended to, and rolled once to a single
// previous generation when it reaches maxLogBytes, so it never grows without bound. A write that
// fails is dropped rather than stopping the agent.
type rotatingLog struct {
	path string

	mu   sync.Mutex
	f    *os.File
	size int64
}

func newRotatingLog(path string) *rotatingLog { return &rotatingLog{path: path} }

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.size+int64(len(p)) > maxLogBytes {
		_ = l.f.Close()
		l.f = nil
		_ = os.Rename(l.path, l.path+".1")
	}
	if l.f == nil {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return len(p), nil
		}
		l.f = f
		if st, err := f.Stat(); err == nil {
			l.size = st.Size()
		}
	}
	n, _ := l.f.Write(p)
	l.size += int64(n)
	return len(p), nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
