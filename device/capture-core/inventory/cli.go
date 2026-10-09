package inventory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// maxConfigFile caps each package.json and .npmrc read; a larger file is not read.
const maxConfigFile = 1 << 20

// Profile is one user profile the CLI scanner reads.
type Profile struct {
	User hostinfo.User
	// Dir is the profile folder (%USERPROFILE%).
	Dir string
	// Loaded is whether the user's hive is loaded under HKEY_USERS, so their own PATH can be read.
	Loaded bool
}

// CLIHost is the read-only view of the machine the CLI scanner needs besides the files in the
// profile folders and on the PATH, which it reads directly.
type CLIHost interface {
	// Profiles lists the user profiles under the profiles folder that have a loaded hive or an
	// NTUSER.DAT.
	Profiles() ([]Profile, error)
	// UserPath is the user's own Path environment value, unexpanded, or "" when it has none.
	UserPath(sid string) (string, error)
	// MachinePath is the machine's Path environment value, expanded.
	MachinePath() (string, error)
	// Getenv returns a machine environment variable, for expanding a user value that names one.
	Getenv(name string) string
	// FileVersion is an executable's PE file version resource, read as data without running or
	// loading the file for execution; "" when it has none.
	FileVersion(path string) (string, error)
}

// CLIScanner finds the catalog's command-line tools in each user profile: global npm packages,
// the Claude Code native install, pipx venvs, and files on the user's PATH. It reads files and
// metadata only; the files are user-writable and the service runs as SYSTEM, so it never runs,
// loads or asks a discovered program for its version.
type CLIScanner struct {
	host   CLIHost
	person func(hostinfo.User) core.Person
}

// NewCLIScanner returns the CLI scanner over host; person names a profile owner's person.
func NewCLIScanner(host CLIHost, person func(hostinfo.User) core.Person) *CLIScanner {
	return &CLIScanner{host: host, person: person}
}

// Scan implements Scanner.
func (s *CLIScanner) Scan(ctx context.Context, b *policy.Bundle) ([]discovery.Record, []error) {
	return s.ScanCounted(ctx, b, nil)
}

// ScanCounted implements CountedScanner: each file the scan checks counts observed on counters.
func (s *CLIScanner) ScanCounted(ctx context.Context, b *policy.Bundle, counters *core.CounterSet) ([]discovery.Record, []error) {
	sc := &cliScan{s: s, b: b, counters: counters, versions: map[string]string{}}
	machinePath, err := s.host.MachinePath()
	if err != nil {
		sc.fail("reading the machine PATH", err)
	}
	sc.machineDirs = splitPath(machinePath)
	profiles, err := s.host.Profiles()
	if err != nil {
		sc.fail("listing the user profiles", err)
	}
	for _, p := range profiles {
		if ctx.Err() != nil {
			sc.errs = append(sc.errs, ctx.Err())
			break
		}
		sc.profile(p)
	}
	return sc.recs, sc.errs
}

// Finds reports whether a scan finds appKey in any user profile.
func (s *CLIScanner) Finds(ctx context.Context, b *policy.Bundle, appKey string) bool {
	recs, _ := s.Scan(ctx, b)
	return slices.ContainsFunc(recs, func(r discovery.Record) bool { return r.AppKey == appKey })
}

// cliScan is one pass of the CLI scanner.
type cliScan struct {
	s           *CLIScanner
	b           *policy.Bundle
	counters    *core.CounterSet
	machineDirs []string
	versions    map[string]string // FileVersion per path, read once per pass
	recs        []discovery.Record
	errs        []error
}

func (sc *cliScan) fail(what string, err error) {
	sc.errs = append(sc.errs, fmt.Errorf("%s: %w", what, err))
}

func (sc *cliScan) observe() {
	if sc.counters != nil {
		sc.counters.Add(protocol.CounterObserved)
	}
}

// profileFinds is what one profile's scan found so far.
type profileFinds struct {
	person core.Person
	apps   map[string]bool
}

func (sc *cliScan) add(pf *profileFinds, apps []string, ver string) {
	for _, app := range apps {
		pf.apps[app] = true
		person := pf.person
		sc.recs = append(sc.recs, discovery.Record{
			Type:    protocol.DiscoveryTypeCLIInstalled,
			Basis:   protocol.DetectionBasisPackageScan,
			AppKey:  app,
			Version: version(ver),
			Person:  &person,
		})
	}
}

func (sc *cliScan) profile(p Profile) {
	pf := &profileFinds{person: sc.s.person(p.User), apps: map[string]bool{}}
	env := sc.profileEnv(p.Dir)
	owner := "profile " + p.Dir
	sc.native(pf, owner, p.Dir)
	sc.npm(pf, owner, p.Dir, env)
	sc.pipx(pf, owner, p.Dir)
	var dirs []string
	if p.Loaded {
		raw, err := sc.s.host.UserPath(p.User.SID)
		if err != nil {
			sc.fail("reading the PATH of user "+p.User.SID, err)
		}
		dirs = splitPath(expandPercent(raw, env))
	}
	sc.path(pf, append(dirs, sc.machineDirs...))
}

// profileEnv resolves the variables a user's own values name: the profile's folders first, then the
// machine's.
func (sc *cliScan) profileEnv(home string) func(string) (string, bool) {
	own := map[string]string{
		"USERPROFILE":  home,
		"APPDATA":      filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
	}
	return func(name string) (string, bool) {
		if v, ok := own[strings.ToUpper(name)]; ok {
			return v, true
		}
		v := sc.s.host.Getenv(name)
		return v, v != ""
	}
}

// native finds the Claude Code native install: the launcher %USERPROFILE%\.local\bin\claude.exe,
// matched by the catalog's executable and command names. Its version is the newest release in
// %USERPROFILE%\.local\share\claude\versions, named by version, or else the launcher's own file
// version.
func (sc *cliScan) native(pf *profileFinds, owner, home string) {
	exe := filepath.Join(home, ".local", "bin", "claude.exe")
	fi, err := os.Stat(exe)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		sc.fail("checking the native install in "+owner, err)
		return
	}
	if !fi.Mode().IsRegular() {
		return
	}
	sc.observe()
	apps := slices.Concat(sc.b.AppByExe(platformWindows, "claude.exe"), sc.b.AppByCLI("claude"))
	if len(apps) == 0 {
		return
	}
	slices.Sort(apps)
	apps = slices.Compact(apps)
	ver, err := newestVersion(filepath.Join(home, ".local", "share", "claude", "versions"))
	if err != nil {
		sc.fail("reading the native install's versions in "+owner, err)
	}
	if ver == "" {
		ver = sc.fileVersion(exe)
	}
	sc.add(pf, apps, ver)
}

// npm finds the global packages under npm's default Windows prefix, %APPDATA%\npm, and under the
// prefix the user's .npmrc sets. Global packages are in <prefix>\node_modules, scoped ones one
// folder deeper.
func (sc *cliScan) npm(pf *profileFinds, owner, home string, env func(string) (string, bool)) {
	prefixes := []string{filepath.Join(home, "AppData", "Roaming", "npm")}
	prefix, err := npmrcPrefix(filepath.Join(home, ".npmrc"), home, env)
	if err != nil {
		sc.fail("reading the .npmrc of "+owner, err)
	}
	if prefix != "" && !samePath(prefix, prefixes[0]) {
		prefixes = append(prefixes, prefix)
	}
	for _, pfx := range prefixes {
		modules := filepath.Join(pfx, "node_modules")
		entries, err := readDir(modules)
		if err != nil {
			sc.fail("reading "+modules, err)
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || e.Type().IsRegular() {
				continue
			}
			if !strings.HasPrefix(name, "@") {
				sc.npmPackage(pf, filepath.Join(modules, name))
				continue
			}
			scoped, err := readDir(filepath.Join(modules, name))
			if err != nil {
				sc.fail("reading "+filepath.Join(modules, name), err)
			}
			for _, s := range scoped {
				sc.npmPackage(pf, filepath.Join(modules, name, s.Name()))
			}
		}
	}
}

func (sc *cliScan) npmPackage(pf *profileFinds, dir string) {
	file := filepath.Join(dir, "package.json")
	data, err := readCapped(file)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		sc.fail("reading "+file, err)
		return
	}
	sc.observe()
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		sc.fail("decoding "+file, err)
		return
	}
	if pkg.Name == "" {
		return
	}
	sc.add(pf, sc.b.AppByNPM(pkg.Name), pkg.Version)
}

// pipx finds the venvs in pipx's home, chosen as pipx chooses it on Windows: %USERPROFILE%\.local\pipx
// or %USERPROFILE%\pipx when either exists, else %LOCALAPPDATA%\pipx\pipx. A venv is named after its
// package; the version is that package's <name>-<version>.dist-info in the venv's site-packages.
func (sc *cliScan) pipx(pf *profileFinds, owner, home string) {
	pipxHome := filepath.Join(home, "AppData", "Local", "pipx", "pipx")
	for _, legacy := range []string{filepath.Join(home, ".local", "pipx"), filepath.Join(home, "pipx")} {
		if fi, err := os.Stat(legacy); err == nil && fi.IsDir() {
			pipxHome = legacy
			break
		}
	}
	venvs := filepath.Join(pipxHome, "venvs")
	entries, err := readDir(venvs)
	if err != nil {
		sc.fail("reading the pipx venvs of "+owner, err)
	}
	for _, e := range entries {
		apps := sc.b.AppByPipx(e.Name())
		if len(apps) == 0 {
			continue
		}
		site := filepath.Join(venvs, e.Name(), "Lib", "site-packages")
		infos, err := readDir(site)
		if err != nil {
			sc.fail("reading "+site, err)
		}
		ver := ""
		for _, info := range infos {
			name, v, ok := distInfo(info.Name())
			if ok && pythonName(name) == pythonName(e.Name()) {
				sc.observe()
				ver = v
				break
			}
		}
		sc.add(pf, apps, ver)
	}
}

// path finds the catalog's command names among the files in the PATH's folders: a file named
// <name>.exe, <name>.cmd or <name>.ps1. The first match of an app wins, as it is the one that runs,
// and an app already found in the profile's package locations is not reported again. Only an .exe
// has a version: its PE file version.
func (sc *cliScan) path(pf *profileFinds, dirs []string) {
	seen := map[string]bool{}
	for _, dir := range dirs {
		key := strings.ToLower(filepath.Clean(dir))
		if seen[key] {
			continue
		}
		seen[key] = true
		entries, err := readDir(dir)
		if err != nil {
			sc.fail("reading PATH folder "+dir, err)
		}
		for _, e := range entries {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if ext != ".exe" && ext != ".cmd" && ext != ".ps1" {
				continue
			}
			var apps []string
			for _, app := range sc.b.AppByCLI(strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))) {
				if !pf.apps[app] {
					apps = append(apps, app)
				}
			}
			if len(apps) == 0 {
				continue
			}
			file := filepath.Join(dir, e.Name())
			fi, err := os.Stat(file)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			sc.observe()
			ver := ""
			if ext == ".exe" {
				ver = sc.fileVersion(file)
			}
			sc.add(pf, apps, ver)
		}
	}
}

func (sc *cliScan) fileVersion(file string) string {
	if v, ok := sc.versions[file]; ok {
		return v
	}
	v, err := sc.s.host.FileVersion(file)
	if err != nil {
		sc.fail("reading the file version of "+file, err)
	}
	sc.versions[file] = v
	return v
}

// readDir lists a folder; a missing folder has no entries.
func readDir(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// readCapped reads a regular file of at most maxConfigFile bytes.
func readCapped(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigFile {
		return nil, fmt.Errorf("larger than %d bytes", maxConfigFile)
	}
	return data, nil
}

// npmrcPrefix is the prefix a user's .npmrc sets, or "" when it sets none npm could use: npm reads
// it as an ini file of key = value lines (';' and '#' start a comment), replaces ${VAR}, and
// expands a leading ~ to the home folder. The last top-level prefix wins. A prefix naming an unset
// variable, or a relative one, is not used.
func npmrcPrefix(file, home string, env func(string) (string, bool)) (string, error) {
	data, err := readCapped(file)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	prefix := ""
	lines := bufio.NewScanner(bytes.NewReader(data))
	lines.Buffer(make([]byte, 0, 4096), maxConfigFile)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if strings.HasPrefix(line, "[") {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "prefix" {
			continue
		}
		prefix = unquote(strings.TrimSpace(value))
	}
	if prefix == "" {
		return "", nil
	}
	prefix, ok := expandBraces(prefix, env)
	if !ok {
		return "", nil
	}
	if rest, found := strings.CutPrefix(prefix, "~"); found && (rest == "" || rest[0] == '/' || rest[0] == '\\') {
		prefix = home + rest
	}
	if !filepath.IsAbs(prefix) {
		return "", nil
	}
	return filepath.Clean(prefix), nil
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

var bracesVar = regexp.MustCompile(`\$\{([^${}]+)\}`)

// expandBraces replaces each ${VAR}; ok is false when one is unset.
func expandBraces(v string, env func(string) (string, bool)) (string, bool) {
	ok := true
	out := bracesVar.ReplaceAllStringFunc(v, func(m string) string {
		val, set := env(m[2 : len(m)-1])
		if !set {
			ok = false
		}
		return val
	})
	return out, ok
}

var percentVar = regexp.MustCompile(`%([^%;]+)%`)

// expandPercent replaces each %VAR% that is set, as Windows expands a REG_EXPAND_SZ value; an unset
// one is left as it is.
func expandPercent(v string, env func(string) (string, bool)) string {
	return percentVar.ReplaceAllStringFunc(v, func(m string) string {
		if val, ok := env(m[1 : len(m)-1]); ok {
			return val
		}
		return m
	})
}

// splitPath splits a Windows PATH value into its folders. Quotes around an entry are removed. A
// relative entry (resolved against whatever the current folder is) and a network path, which the
// service would read as the machine account, are left out.
func splitPath(v string) []string {
	var out []string
	for _, d := range strings.Split(v, ";") {
		d = strings.TrimSpace(strings.Trim(strings.TrimSpace(d), `"`))
		if d == "" || strings.HasPrefix(d, `\\`) || strings.HasPrefix(d, "//") || !filepath.IsAbs(d) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// releaseName is a release version as the native installer names its versions entries.
var releaseName = regexp.MustCompile(`^\d+(\.\d+)+([-+][0-9A-Za-z.+-]+)?$`)

// newestVersion is the highest release among a versions folder's entries ("" when none), each
// named by its version, with or without .exe.
func newestVersion(dir string) (string, error) {
	entries, err := readDir(dir)
	best := ""
	for _, e := range entries {
		name := e.Name()
		if strings.EqualFold(filepath.Ext(name), ".exe") {
			name = name[:len(name)-len(".exe")]
		}
		if releaseName.MatchString(name) && (best == "" || compareRelease(name, best) > 0) {
			best = name
		}
	}
	return best, err
}

// compareRelease orders two release names by their numeric parts, then a release above its
// pre-releases, then by text.
func compareRelease(a, b string) int {
	coreA, extA, _ := strings.Cut(strings.ReplaceAll(a, "+", "-"), "-")
	coreB, extB, _ := strings.Cut(strings.ReplaceAll(b, "+", "-"), "-")
	pa, pb := strings.Split(coreA, "."), strings.Split(coreB, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y uint64
		if i < len(pa) {
			x, _ = strconv.ParseUint(pa[i], 10, 64)
		}
		if i < len(pb) {
			y, _ = strconv.ParseUint(pb[i], 10, 64)
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case extA == extB:
		return strings.Compare(a, b)
	case extA == "":
		return 1
	case extB == "":
		return -1
	}
	return strings.Compare(extA, extB)
}

var pythonSeparators = regexp.MustCompile(`[-_.]+`)

// pythonName is a Python package name normalized for comparison: lower case, each run of '-', '_'
// and '.' a single '-'.
func pythonName(n string) string {
	return pythonSeparators.ReplaceAllString(strings.ToLower(n), "-")
}

// distInfo splits a <name>-<version>.dist-info folder name. The name part has its dashes escaped
// as underscores, so the first dash ends it.
func distInfo(dir string) (name, ver string, ok bool) {
	base, found := strings.CutSuffix(dir, ".dist-info")
	if !found {
		return "", "", false
	}
	name, ver, ok = strings.Cut(base, "-")
	if !ok || name == "" || ver == "" {
		return "", "", false
	}
	return name, ver, true
}
