package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// The service reads the machine's attestation and console user, and changes the trust store, the
// machine environment and a native messaging endpoint. Every test replaces those seams, so a run on
// an enrolled, domain-joined machine behaves like one on a build host and nothing outside the
// test's temporary directories is touched.
func TestMain(m *testing.M) {
	collectHostFacts = func() hostinfo.Facts { return hostinfo.Facts{} }
	userSources = func() hostinfo.UserSources { return (&fakeConsole{err: hostinfo.ErrNoConsoleUser}).sources() }
	shimDir, err := os.MkdirTemp("", "sac-shim-")
	if err != nil {
		panic(err)
	}
	platform = facilities{
		trustStore: func(func(string, ...any)) trustStore { return &fakeTrustStore{} },
		shimRunner: &fakeRunner{},
		shimDir:    shimDir,
		nativeAddr: testNativeAddr(),
	}
	code := m.Run()
	_ = os.RemoveAll(shimDir)
	os.Exit(code)
}

// testNativeAddr is a native messaging endpoint private to this test process.
func testNativeAddr() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	if runtime.GOOS == "windows" {
		return `\\.\pipe\ShadowAICapture.native.test-` + hex.EncodeToString(b[:])
	}
	return filepath.Join(os.TempDir(), "sac-"+hex.EncodeToString(b[:]), "native.sock")
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

// fakeTrustStore records what the service installs and removes.
type fakeTrustStore struct {
	mu        sync.Mutex
	installed []byte
	removes   int
}

func (f *fakeTrustStore) Install(_ context.Context, der []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append([]byte(nil), der...)
	return nil
}

func (f *fakeTrustStore) Verify(_ context.Context, der []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.installed) == string(der), nil
}

func (f *fakeTrustStore) Remove(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes++
	f.installed = nil
	return nil
}

// fakeRunner stands in for the commands the CLI shim runs to set the machine environment.
type fakeRunner struct{}

func (*fakeRunner) Run(context.Context, string, ...string) (string, error) { return "", nil }
