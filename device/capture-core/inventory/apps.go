package inventory

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// platformWindows is the catalog platform the installed-app scanner matches on.
const platformWindows = "windows"

// maxVersionChars is the envelope's app_version limit; a longer version is left out.
const maxVersionChars = 64

// UninstallEntry is the values of one Uninstall subkey the scanner reads.
type UninstallEntry struct {
	DisplayName     string
	DisplayVersion  string
	Publisher       string
	DisplayIcon     string
	InstallLocation string
}

// Registry is the read-only view of the three places Windows records installed software. A sid
// of "" names the machine (HKLM); any other names a user's loaded hive under HKEY_USERS. A missing
// key is no entries, not an error; an error means a key exists but could not be read, and the
// entries read before it are still returned.
type Registry interface {
	// Users lists the signed-in users whose hives are loaded.
	Users() ([]hostinfo.User, error)
	// Uninstall lists the uninstall entries: for the machine, of both the 64-bit and the 32-bit
	// registry views.
	Uninstall(sid string) ([]UninstallEntry, error)
	// Packages lists the full names of the AppX/MSIX packages the package repository holds.
	Packages(sid string) ([]string, error)
}

// AppScanner finds the catalog's apps among the machine-wide and per-user uninstall entries and
// AppX/MSIX packages. A machine-wide install is unattributed; a per-user one belongs to the person
// whose hive holds it.
type AppScanner struct {
	reg    Registry
	person func(hostinfo.User) core.Person
}

// NewAppScanner returns the installed-app scanner over reg; person names a user's person.
func NewAppScanner(reg Registry, person func(hostinfo.User) core.Person) *AppScanner {
	return &AppScanner{reg: reg, person: person}
}

// Scan implements Scanner.
func (s *AppScanner) Scan(ctx context.Context, b *policy.Bundle) ([]discovery.Record, []error) {
	var recs []discovery.Record
	var errs []error
	read := func(owner string, person *core.Person, sid string) {
		entries, err := s.reg.Uninstall(sid)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the %s uninstall entries: %w", owner, err))
		}
		for _, e := range entries {
			for _, key := range matchUninstall(b, e) {
				recs = append(recs, appRecord(key, version(e.DisplayVersion), person))
			}
		}
		pkgs, err := s.reg.Packages(sid)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the %s package repository: %w", owner, err))
		}
		for _, full := range pkgs {
			family, ver, ok := parsePackageFullName(full)
			if !ok {
				continue
			}
			for _, key := range b.AppByAppx(platformWindows, family) {
				recs = append(recs, appRecord(key, version(ver), person))
			}
		}
	}

	read("machine", nil, "")
	users, err := s.reg.Users()
	if err != nil {
		errs = append(errs, fmt.Errorf("listing the loaded user hives: %w", err))
	}
	for _, u := range users {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		p := s.person(u)
		read("user "+u.SID, &p, u.SID)
	}
	return recs, errs
}

func appRecord(appKey, version string, person *core.Person) discovery.Record {
	r := discovery.Record{
		Type:    protocol.DiscoveryTypeAppInstalled,
		Basis:   protocol.DetectionBasisInstalledScan,
		AppKey:  appKey,
		Version: version,
		Person:  person,
	}
	if person == nil {
		r.UserRef = discovery.UnattributedUserRef
	}
	return r
}

// matchUninstall returns the apps an uninstall entry is: by its name, or by the executable its icon
// or install location names. Failing those, by its publisher, but only for an app the catalog gives
// no name or executable on Windows: a publisher such as Microsoft Corporation ships far more than
// the one app it identifies.
func matchUninstall(b *policy.Bundle, e UninstallEntry) []string {
	keys := b.AppByUninstallName(platformWindows, e.DisplayName)
	for _, path := range []string{e.DisplayIcon, e.InstallLocation} {
		if exe := exeBase(path); exe != "" {
			keys = append(keys, b.AppByExe(platformWindows, exe)...)
		}
	}
	if len(keys) == 0 && strings.TrimSpace(e.Publisher) != "" {
		for _, k := range b.AppByPublisher(platformWindows, strings.TrimSpace(e.Publisher)) {
			if !b.HasSignal(k, platformWindows, policy.SignalWindowsUninstallName) && !b.HasSignal(k, platformWindows, policy.SignalWindowsExe) {
				keys = append(keys, k)
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// exeBase is the executable's base name in a DisplayIcon or InstallLocation value, or "" when the
// value names no .exe. A DisplayIcon may be quoted and may end in ",<icon index>".
func exeBase(v string) string {
	v = strings.TrimSpace(v)
	if rest, ok := strings.CutPrefix(v, `"`); ok {
		v, _, _ = strings.Cut(rest, `"`)
	} else if i := strings.LastIndexByte(v, ','); i >= 0 {
		if _, err := strconv.Atoi(strings.TrimSpace(v[i+1:])); err == nil {
			v = v[:i]
		}
	}
	v = strings.TrimRight(strings.TrimSpace(v), `\/`)
	base := v[strings.LastIndexAny(v, `\/`)+1:]
	if !strings.HasSuffix(strings.ToLower(base), ".exe") {
		return ""
	}
	return base
}

// parsePackageFullName splits a package full name, <Name>_<Version>_<Architecture>_<ResourceId>_<PublisherId>,
// into its family name, <Name>_<PublisherId>, and its version.
func parsePackageFullName(full string) (family, version string, ok bool) {
	parts := strings.Split(full, "_")
	if len(parts) != 5 || parts[0] == "" || parts[1] == "" || parts[4] == "" {
		return "", "", false
	}
	return parts[0] + "_" + parts[4], parts[1], true
}

func version(v string) string {
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) > maxVersionChars {
		return ""
	}
	return v
}

// userSID reports whether a HKEY_USERS subkey is a signed-in person's hive, by the SID prefixes
// winproxy uses: local and domain accounts (S-1-5-21-), Entra accounts (S-1-12-1-) and Microsoft
// accounts (S-1-11-). Service accounts and the per-user class hives (<SID>_Classes) are not people.
func userSID(name string) bool {
	if strings.HasSuffix(name, "_Classes") {
		return false
	}
	u := strings.ToUpper(name)
	return strings.HasPrefix(u, "S-1-5-21-") || strings.HasPrefix(u, "S-1-12-1-") || strings.HasPrefix(u, "S-1-11-")
}
