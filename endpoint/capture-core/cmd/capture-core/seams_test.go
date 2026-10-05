package main

import (
	"os"
	"sync"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// The service reads the machine's attestation and console user through two seams. Every test in
// this package replaces them, so a run on an Intune-enrolled or domain-joined machine behaves like
// one on a build host; hostinfo's own tests cover the real readers.
func TestMain(m *testing.M) {
	collectHostFacts = func() hostinfo.Facts { return hostinfo.Facts{} }
	userSources = func() hostinfo.UserSources { return (&fakeConsole{err: hostinfo.ErrNoConsoleUser}).sources() }
	os.Exit(m.Run())
}

// fakeConsole is a console session whose user a test changes.
type fakeConsole struct {
	mu   sync.Mutex
	user hostinfo.User
	err  error
}

func (c *fakeConsole) set(u hostinfo.User, err error) {
	c.mu.Lock()
	c.user, c.err = u, err
	c.mu.Unlock()
}

func (c *fakeConsole) sources() hostinfo.UserSources {
	return hostinfo.UserSources{Console: func() (hostinfo.User, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.user, c.err
	}}
}

// withHostFacts replaces the attestation seam for one test.
func withHostFacts(t *testing.T, f hostinfo.Facts) {
	t.Helper()
	prev := collectHostFacts
	collectHostFacts = func() hostinfo.Facts { return f }
	t.Cleanup(func() { collectHostFacts = prev })
}

// withConsole replaces the console-user seam for one test.
func withConsole(t *testing.T, c *fakeConsole) {
	t.Helper()
	prev := userSources
	userSources = c.sources
	t.Cleanup(func() { userSources = prev })
}
