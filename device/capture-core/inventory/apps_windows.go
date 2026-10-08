//go:build windows

package inventory

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
)

// The registry locations of installed software. Uninstall is under HKLM (in both registry views)
// and under each user's hive; the machine's AppX/MSIX packages are the all-user store's
// Applications, and a user's are the package repository in that user's classes.
const (
	uninstallPath       = `Software\Microsoft\Windows\CurrentVersion\Uninstall`
	allUserPackagesPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\Appx\AppxAllUserStore\Applications`
	userPackagesPath    = `Software\Classes\Local Settings\Software\Microsoft\Windows\CurrentVersion\AppModel\Repository\Packages`
)

// Scanners is the platform's scan pass: the installed-app scanner over the registry, then the CLI
// and IDE extension scanners over the user profiles.
func Scanners(person func(hostinfo.User) core.Person) []Scanner {
	return []Scanner{
		NewAppScanner(SystemRegistry(), person),
		NewCLIScanner(SystemCLIHost(), person),
		NewIDEScanner(SystemCLIHost(), person),
	}
}

// SystemRegistry is the machine's registry, read only.
func SystemRegistry() Registry { return winRegistry{} }

type winRegistry struct{}

// Users lists the people whose hives are loaded under HKEY_USERS, named by SID and, where Windows
// can say, DOMAIN\user.
func (winRegistry) Users() ([]hostinfo.User, error) {
	k, err := registry.OpenKey(registry.USERS, "", registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, fmt.Errorf("opening HKEY_USERS: %w", err)
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("reading HKEY_USERS: %w", err)
	}
	var out []hostinfo.User
	for _, n := range names {
		if !userSID(n) {
			continue
		}
		u := hostinfo.User{SID: n}
		if sid, err := windows.StringToSid(n); err == nil {
			if account, domain, _, err := sid.LookupAccount(""); err == nil {
				u.Account = account
				if domain != "" {
					u.Account = domain + `\` + account
				}
			}
		}
		out = append(out, u)
	}
	return out, nil
}

func (winRegistry) Uninstall(sid string) ([]UninstallEntry, error) {
	if sid == "" {
		entries, err64 := uninstallEntries(registry.LOCAL_MACHINE, uninstallPath, registry.WOW64_64KEY)
		more, err32 := uninstallEntries(registry.LOCAL_MACHINE, uninstallPath, registry.WOW64_32KEY)
		return append(entries, more...), errors.Join(err64, err32)
	}
	return uninstallEntries(registry.USERS, sid+`\`+uninstallPath, 0)
}

func (winRegistry) Packages(sid string) ([]string, error) {
	if sid == "" {
		return subKeys(registry.LOCAL_MACHINE, allUserPackagesPath, registry.WOW64_64KEY)
	}
	return subKeys(registry.USERS, sid+`\`+userPackagesPath, 0)
}

// subKeys lists a key's subkey names; a missing key has none.
func subKeys(root registry.Key, path string, view uint32) ([]string, error) {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS|view)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return names, nil
}

// uninstallEntries reads each subkey of an Uninstall key. A subkey that cannot be read is an
// error; the others are still returned.
func uninstallEntries(root registry.Key, path string, view uint32) ([]UninstallEntry, error) {
	names, err := subKeys(root, path, view)
	if err != nil {
		return nil, err
	}
	var out []UninstallEntry
	var errs []error
	for _, n := range names {
		k, err := registry.OpenKey(root, path+`\`+n, registry.QUERY_VALUE|view)
		if notFound(err) {
			continue // removed since the listing
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("opening %s\\%s: %w", path, n, err))
			continue
		}
		out = append(out, UninstallEntry{
			DisplayName:     stringValue(k, "DisplayName"),
			DisplayVersion:  stringValue(k, "DisplayVersion"),
			Publisher:       stringValue(k, "Publisher"),
			DisplayIcon:     stringValue(k, "DisplayIcon"),
			InstallLocation: stringValue(k, "InstallLocation"),
		})
		k.Close()
	}
	return out, errors.Join(errs...)
}

// stringValue reads a REG_SZ or REG_EXPAND_SZ value unexpanded; an absent value, or one of another
// type, is "".
func stringValue(k registry.Key, name string) string {
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

func notFound(err error) bool {
	return errors.Is(err, registry.ErrNotExist) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
