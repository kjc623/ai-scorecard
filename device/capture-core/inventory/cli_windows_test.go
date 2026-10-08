//go:build windows

package inventory

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

var fixedVersion = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

// The file version comes from the PE version resource: Windows' own executables have one, and the
// test binary, like any Go binary, has none.
func TestFileVersion(t *testing.T) {
	host := SystemCLIHost()
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kernel32.dll", "cmd.exe"} {
		v, err := host.FileVersion(filepath.Join(system, name))
		if err != nil || !fixedVersion.MatchString(v) {
			t.Errorf("%s: version %q, %v", name, v, err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if v, err := host.FileVersion(self); v != "" || err != nil {
		t.Errorf("the test binary: version %q, %v; want none", v, err)
	}
	if _, err := host.FileVersion(filepath.Join(t.TempDir(), "missing.exe")); err == nil {
		t.Error("a missing file has a version")
	}
}

// pathOnlyHost is one profile whose PATH is a fixture folder, with the machine's real file version
// reader.
type pathOnlyHost struct {
	CLIHost
	home, dir string
}

func (h pathOnlyHost) Profiles() ([]Profile, error) {
	return []Profile{{User: hostinfo.User{SID: adaSID}, Dir: h.home, Loaded: true}}, nil
}
func (h pathOnlyHost) UserPath(string) (string, error) { return h.dir, nil }
func (h pathOnlyHost) MachinePath() (string, error)    { return "", nil }

// A PATH hit on an .exe carries its PE file version: a copy of cmd.exe named claude.exe reports
// cmd.exe's version, read without running it.
func TestCLIScannerPathHitWithAFileVersion(t *testing.T) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	want, err := SystemCLIHost().FileVersion(filepath.Join(system, "cmd.exe"))
	if err != nil || want == "" {
		t.Fatalf("cmd.exe version %q, %v", want, err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(system, "cmd.exe"), filepath.Join(dir, "claude.exe"))
	host := pathOnlyHost{CLIHost: SystemCLIHost(), home: filepath.Join(root, "home"), dir: dir}
	recs, errs := NewCLIScanner(host, person).Scan(context.Background(), cliCatalog())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(recs) != 1 || recs[0].AppKey != "claude_code" || recs[0].Version != want {
		t.Fatalf("found %+v, want claude_code %s", recs, want)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// The machine's own reads, read only: the profiles are people's folders under the profiles folder,
// and the machine PATH holds the system folder.
func TestSystemCLIHostReads(t *testing.T) {
	host := SystemCLIHost()
	root, err := windows.KnownFolderPath(windows.FOLDERID_UserProfiles, 0)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := host.Profiles()
	if err != nil {
		t.Fatalf("profiles: %v", err)
	}
	for _, p := range profiles {
		if !userSID(p.User.SID) || !samePath(filepath.Dir(p.Dir), root) {
			t.Errorf("profile %+v is not a person's folder under %s", p, root)
		}
		if !p.Loaded {
			continue
		}
		// Another person's hive may be closed to an unelevated test run; the service runs as
		// LocalSystem.
		if _, err := host.UserPath(p.User.SID); err != nil {
			t.Logf("%s PATH: %v", p.User.SID, err)
		}
	}
	path, err := host.MachinePath()
	if err != nil {
		t.Fatalf("machine PATH: %v", err)
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(path), strings.ToLower(system)) || strings.Contains(path, "%SystemRoot%") {
		t.Fatalf("machine PATH %q does not hold %s expanded", path, system)
	}
}
