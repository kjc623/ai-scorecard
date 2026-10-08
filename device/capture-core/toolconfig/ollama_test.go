package toolconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/state"
)

// fakeEnv is a machine environment in memory that records each write.
type fakeEnv struct {
	vars   map[string]string
	writes []string
	err    error
}

func newFakeEnv() *fakeEnv { return &fakeEnv{vars: map[string]string{}} }

func (e *fakeEnv) Lookup(name string) (string, bool, error) {
	if e.err != nil {
		return "", false, e.err
	}
	v, ok := e.vars[name]
	return v, ok, nil
}

func (e *fakeEnv) Set(name, value string) error {
	if e.err != nil {
		return e.err
	}
	e.vars[name] = value
	e.writes = append(e.writes, "set "+name+"="+value)
	return nil
}

func (e *fakeEnv) Unset(name string) error {
	if e.err != nil {
		return e.err
	}
	delete(e.vars, name)
	e.writes = append(e.writes, "unset "+name)
	return nil
}

func testStateDir(t *testing.T) state.Dir {
	t.Helper()
	dir, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// An unset OLLAMA_HOST is pointed at the upstream port and, on restore, unset again; the backup is
// kept only while the variable is relocated.
func TestOllamaRelocatesAndRestoresAnUnsetHost(t *testing.T) {
	dir := testStateDir(t)
	env := newFakeEnv()
	o := NewOllama(dir, env)
	backupPath := dir.Path("toolconfig", "ollama", "original")

	if err := o.Relocate(21434); err != nil {
		t.Fatal(err)
	}
	if got := env.vars["OLLAMA_HOST"]; got != "127.0.0.1:21434" {
		t.Fatalf("OLLAMA_HOST = %q, want 127.0.0.1:21434", got)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("no backup at %s: %v", backupPath, err)
	}
	// A second relocation finds the variable already set and writes nothing.
	if err := o.Relocate(21434); err != nil {
		t.Fatal(err)
	}
	if len(env.writes) != 1 {
		t.Fatalf("writes = %v, want one", env.writes)
	}

	if err := o.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.vars["OLLAMA_HOST"]; ok {
		t.Fatalf("OLLAMA_HOST = %q after restore, want it unset", env.vars["OLLAMA_HOST"])
	}
	if _, err := os.Stat(backupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the backup outlived the restore: %v", err)
	}
	// Without a record a restore changes nothing.
	n := len(env.writes)
	if err := o.Restore(); err != nil || len(env.writes) != n {
		t.Fatalf("restore without a record: %v, writes %v", err, env.writes)
	}
}

// A value the customer set is recorded once, survives a change of upstream port and a new process
// (the record is in the state directory), and is put back exactly.
func TestOllamaRestoresTheCustomersValue(t *testing.T) {
	dir := testStateDir(t)
	env := newFakeEnv()
	env.vars["OLLAMA_HOST"] = "0.0.0.0:11434"

	if err := NewOllama(dir, env).Relocate(21434); err != nil {
		t.Fatal(err)
	}
	if err := NewOllama(dir, env).Relocate(31434); err != nil {
		t.Fatal(err)
	}
	if got := env.vars["OLLAMA_HOST"]; got != "127.0.0.1:31434" {
		t.Fatalf("OLLAMA_HOST = %q, want the new upstream", got)
	}
	if err := NewOllama(dir, env).Restore(); err != nil {
		t.Fatal(err)
	}
	if got := env.vars["OLLAMA_HOST"]; got != "0.0.0.0:11434" {
		t.Fatalf("OLLAMA_HOST = %q after restore, want the customer's 0.0.0.0:11434", got)
	}
}

// A failure to read the environment changes nothing and keeps no record; a failure to write is
// reported.
func TestOllamaReportsEnvironmentFailures(t *testing.T) {
	dir := testStateDir(t)
	env := newFakeEnv()
	env.err = errors.New("access denied")
	o := NewOllama(dir, env)
	if err := o.Relocate(21434); err == nil {
		t.Fatal("relocation over an unreadable environment succeeded")
	}
	if _, err := os.Stat(dir.Path("toolconfig", "ollama", "original")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a record was kept: %v", err)
	}
	if err := o.Relocate(0); err == nil {
		t.Fatal("port 0 was accepted")
	}
}
