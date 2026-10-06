package state

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenProtectsTheDirectory(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := CheckFile(d.Root()); err != nil {
		t.Fatalf("the state directory is not protected: %v", err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(d.Root())
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o700 {
			t.Fatalf("mode = %o, want 0700", st.Mode().Perm())
		}
	}
	// Files created inside without the helper inherit the protection.
	inner := d.Path("inherited")
	if err := os.WriteFile(inner, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(inner); err != nil {
		t.Fatalf("a file created inside the state directory is not protected: %v", err)
	}
}

func TestOpenNarrowsAnExistingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := CheckFile(d.Root()); err != nil {
		t.Fatalf("an existing directory was not narrowed: %v", err)
	}
}

func TestReadOrCreateKeyCreatesOnceAndReuses(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k1, err := d.Key(SpoolKeyFile)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if len(k1) != KeySize {
		t.Fatalf("key is %d bytes, want %d", len(k1), KeySize)
	}
	if err := CheckFile(d.Path(SpoolKeyFile)); err != nil {
		t.Fatalf("the key file is not protected: %v", err)
	}
	k2, err := d.Key(SpoolKeyFile)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatalf("second read: err=%v equal=%v; the key must be reused", err, bytes.Equal(k1, k2))
	}
	other, err := d.Key(ContentKeyFile)
	if err != nil || bytes.Equal(k1, other) {
		t.Fatalf("a second key file must hold its own key (err=%v)", err)
	}
}

// A key file truncated by a crash during creation never encrypted anything, so it is replaced.
func TestReadOrCreateKeyReplacesATruncatedKey(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(d.Path(SpoolKeyFile), []byte("short")); err != nil {
		t.Fatal(err)
	}
	k, err := d.Key(SpoolKeyFile)
	if err != nil || len(k) != KeySize {
		t.Fatalf("Key after truncation: len=%d err=%v", len(k), err)
	}
}

func TestWriteFileReplacesAtomicallyAndProtects(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := d.Path(CredentialFile)
	for _, content := range []string{"first", "second"} {
		if err := WriteFile(path, []byte(content)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("read back %q (err %v), want %q", got, err, content)
		}
	}
	if err := CheckFile(path); err != nil {
		t.Fatalf("the written file is not protected: %v", err)
	}
	entries, err := os.ReadDir(d.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the directory holds %d entries, want only the file (no temporary left behind)", len(entries))
	}
}

func TestCheckFileRefusesAFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by TestCheckSDDL and TestReadOrCreateKeyReprotectsAReadableKey on Windows")
	}
	path := filepath.Join(t.TempDir(), "open")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(path); err == nil {
		t.Fatal("a world-readable file passed the check")
	}
}
