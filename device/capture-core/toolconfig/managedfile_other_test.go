//go:build !windows

package toolconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// A managed file the agent rewrites stays readable by everyone and writable by its owner only.
func TestClaudeCodeWriteLeavesUsersReadOnly(t *testing.T) {
	w, path := newTestWriter(t)
	writeFile(t, path, []byte(customerFile))
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(testDesired(false)); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 0644", st.Mode().Perm())
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(testDesired(true)); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want the file's own 0640 kept", st.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".managed-*")); len(m) != 0 {
		t.Fatalf("temporary files left behind: %v", m)
	}
}
