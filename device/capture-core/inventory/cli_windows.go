//go:build windows

package inventory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
)

// The registry locations the CLI scanner reads: the profile of each account (its folder in
// ProfileImagePath), and the machine's environment. A user's own environment is the Environment
// key of their hive.
const (
	profileListPath    = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`
	machineEnvPath     = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	userEnvKey         = `Environment`
	maxVersionResource = 1 << 20
)

// SystemCLIHost is the machine the service runs on, read only.
func SystemCLIHost() CLIHost { return winCLIHost{} }

// CLIInstalled reports whether the CLI scanner finds appKey, by b's catalog, in any user profile.
func CLIInstalled(b *policy.Bundle, appKey string) bool {
	s := NewCLIScanner(SystemCLIHost(), func(hostinfo.User) core.Person { return core.Person{} })
	return s.Finds(context.Background(), b, appKey)
}

type winCLIHost struct{}

// Profiles lists the people's profiles whose folder is directly under the profiles folder
// (FOLDERID_UserProfiles, C:\Users) and whose hive is loaded or whose NTUSER.DAT exists.
func (winCLIHost) Profiles() ([]Profile, error) {
	root, err := windows.KnownFolderPath(windows.FOLDERID_UserProfiles, 0)
	if err != nil {
		return nil, fmt.Errorf("finding the profiles folder: %w", err)
	}
	sids, err := subKeys(registry.LOCAL_MACHINE, profileListPath, registry.WOW64_64KEY)
	if err != nil {
		return nil, err
	}
	var out []Profile
	var errs []error
	for _, sid := range sids {
		if !userSID(sid) {
			continue
		}
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileListPath+`\`+sid, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if notFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("opening the profile of %s: %w", sid, err))
			continue
		}
		dir := stringValue(k, "ProfileImagePath")
		k.Close()
		if expanded, err := registry.ExpandString(dir); err == nil {
			dir = expanded
		}
		if dir == "" || !samePath(filepath.Dir(filepath.Clean(dir)), root) {
			continue
		}
		loaded := hiveLoaded(sid)
		if !loaded {
			if _, err := os.Stat(filepath.Join(dir, "NTUSER.DAT")); err != nil {
				continue
			}
		}
		out = append(out, Profile{User: hostinfo.User{SID: sid, Account: accountOf(sid)}, Dir: dir, Loaded: loaded})
	}
	return out, errors.Join(errs...)
}

func hiveLoaded(sid string) bool {
	k, err := registry.OpenKey(registry.USERS, sid, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

// accountOf is the SID's DOMAIN\user, or "" when Windows cannot name it.
func accountOf(sid string) string {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return ""
	}
	account, domain, _, err := s.LookupAccount("")
	if err != nil {
		return ""
	}
	if domain != "" {
		return domain + `\` + account
	}
	return account
}

func (winCLIHost) UserPath(sid string) (string, error) {
	return pathValue(registry.USERS, sid+`\`+userEnvKey)
}

func (winCLIHost) MachinePath() (string, error) {
	v, err := pathValue(registry.LOCAL_MACHINE, machineEnvPath)
	if err != nil || v == "" {
		return v, err
	}
	// The machine's variables are the service's own, so its environment expands them.
	return registry.ExpandString(v)
}

// pathValue reads a key's Path value unexpanded; a missing key or value is "".
func pathValue(root registry.Key, path string) (string, error) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if notFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s\\Path: %w", path, err)
	}
	return v, nil
}

func (winCLIHost) Getenv(name string) string { return os.Getenv(name) }

// FileVersion reads the fixed file version (major.minor.build.revision) from the file's version
// resource. GetFileVersionInfo maps the file as a data file: nothing in it runs.
func (winCLIHost) FileVersion(path string) (string, error) {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if errors.Is(err, windows.ERROR_RESOURCE_TYPE_NOT_FOUND) || errors.Is(err, windows.ERROR_RESOURCE_DATA_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_RESOURCE_NAME_NOT_FOUND) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if size == 0 || size > maxVersionResource {
		return "", fmt.Errorf("a version resource of %d bytes", size)
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return "", err
	}
	var fixed *windows.VS_FIXEDFILEINFO
	n := uint32(unsafe.Sizeof(*fixed))
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &n); err != nil || fixed == nil ||
		n < uint32(unsafe.Sizeof(*fixed)) || fixed.Signature != 0xFEEF04BD {
		return "", nil
	}
	return fmt.Sprintf("%d.%d.%d.%d", fixed.FileVersionMS>>16, fixed.FileVersionMS&0xffff,
		fixed.FileVersionLS>>16, fixed.FileVersionLS&0xffff), nil
}
