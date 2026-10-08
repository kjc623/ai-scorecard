package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// cliCatalog holds the catalog's signals for the four CLIs. aider and its pipx package are fixture
// values, not catalog facts: the catalog names no pipx package.
func cliCatalog() *policy.Bundle {
	sig := func(platform, kind, value string) policy.CatalogSignal {
		return policy.CatalogSignal{Platform: platform, Kind: kind, Value: value}
	}
	return &policy.Bundle{
		Endpoint: policy.EndpointPolicy{
			Inventory:            policy.EndpointInventory{Enabled: true, IntervalMinutes: 360},
			DiscoveryDailyBudget: 200,
		},
		Catalog: []policy.CatalogApp{
			{AppKey: "aider", Category: "coding_agent", Signals: []policy.CatalogSignal{
				sig("any", policy.SignalCLIBinary, "aider"), sig("any", policy.SignalPipxPackage, "aider-chat"),
			}},
			{AppKey: "claude_code", Category: "coding_agent", Signals: []policy.CatalogSignal{
				sig("any", policy.SignalCLIBinary, "claude"), sig("any", policy.SignalNPMPackage, "@anthropic-ai/claude-code"),
				sig("windows", policy.SignalWindowsExe, "claude.exe"),
			}},
			{AppKey: "codex", Category: "coding_agent", Signals: []policy.CatalogSignal{
				sig("any", policy.SignalCLIBinary, "codex"), sig("any", policy.SignalNPMPackage, "@openai/codex"),
				sig("windows", policy.SignalWindowsExe, "codex.exe"),
			}},
			{AppKey: "copilot_cli", Category: "coding_agent", Signals: []policy.CatalogSignal{
				sig("any", policy.SignalCLIBinary, "copilot"), sig("any", policy.SignalNPMPackage, "@github/copilot"),
			}},
			{AppKey: "gemini_cli", Category: "coding_agent", Signals: []policy.CatalogSignal{
				sig("any", policy.SignalCLIBinary, "gemini"), sig("any", policy.SignalNPMPackage, "@google/gemini-cli"),
			}},
		},
	}
}

// fakeCLIHost is the profiles, the registry's PATH values and the file versions of a fixture tree.
type fakeCLIHost struct {
	profiles    []Profile
	profilesErr error
	userPath    map[string]string
	userPathErr map[string]error
	machinePath string
	machineErr  error
	env         map[string]string
	versions    map[string]string // file version per path
}

func (h *fakeCLIHost) Profiles() ([]Profile, error) { return h.profiles, h.profilesErr }

func (h *fakeCLIHost) UserPath(sid string) (string, error) {
	return h.userPath[sid], h.userPathErr[sid]
}

func (h *fakeCLIHost) MachinePath() (string, error) { return h.machinePath, h.machineErr }

func (h *fakeCLIHost) Getenv(name string) string { return h.env[strings.ToUpper(name)] }

func (h *fakeCLIHost) FileVersion(path string) (string, error) { return h.versions[path], nil }

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func packageJSON(name, version string) string {
	b, _ := json.Marshal(map[string]any{"name": name, "version": version, "bin": map[string]string{"x": "x.js"}})
	return string(b)
}

// cliTree is the fixture device under root:
//   - Ada (hive loaded) installed the four CLIs with npm under the default prefix, beside a non-AI
//     package; npm put its command shims in the prefix, which is on her PATH.
//   - Grace (hive not loaded, NTUSER.DAT present) has the Claude Code native install, a pipx venv
//     of aider and a non-AI one, and Gemini CLI under the prefix her .npmrc sets.
//   - The machine PATH holds a copilot.exe with a file version, and non-AI executables.
//
// Every executable is a script that would leave a marker if anything ran it.
func cliTree(t *testing.T) (*fakeCLIHost, string) {
	t.Helper()
	root := t.TempDir()
	users := filepath.Join(root, "Users")
	ada, grace := filepath.Join(users, "ada"), filepath.Join(users, "grace")
	marker := filepath.Join(root, "ran")
	script := "#!/bin/sh\ntouch " + marker + "\n"

	npm := filepath.Join(ada, "AppData", "Roaming", "npm")
	modules := filepath.Join(npm, "node_modules")
	put(t, filepath.Join(modules, "@anthropic-ai", "claude-code", "package.json"), packageJSON("@anthropic-ai/claude-code", "2.1.295"))
	put(t, filepath.Join(modules, "@openai", "codex", "package.json"), packageJSON("@openai/codex", "0.162.0"))
	put(t, filepath.Join(modules, "@google", "gemini-cli", "package.json"), packageJSON("@google/gemini-cli", "0.63.0"))
	put(t, filepath.Join(modules, "@github", "copilot", "package.json"), packageJSON("@github/copilot", "1.0.94"))
	put(t, filepath.Join(modules, "left-pad", "package.json"), packageJSON("left-pad", "1.3.0"))
	put(t, filepath.Join(modules, ".package-lock.json"), "{}")
	for _, shim := range []string{"claude", "claude.cmd", "claude.ps1", "codex.cmd", "gemini.cmd", "copilot.cmd", "left-pad.cmd"} {
		put(t, filepath.Join(npm, shim), script)
	}

	put(t, filepath.Join(grace, "NTUSER.DAT"), "")
	put(t, filepath.Join(grace, ".local", "bin", "claude.exe"), script)
	for _, v := range []string{"2.1.10", "2.1.290", "2.1.295-rc.1", "2.1.295"} {
		put(t, filepath.Join(grace, ".local", "share", "claude", "versions", v), script)
	}
	put(t, filepath.Join(grace, ".local", "share", "claude", "versions", "2.1.300.tmp"), "")
	venvs := filepath.Join(grace, ".local", "pipx", "venvs")
	mkdir(t, filepath.Join(venvs, "aider-chat", "Lib", "site-packages", "aider_chat-0.86.1.dist-info"))
	mkdir(t, filepath.Join(venvs, "aider-chat", "Lib", "site-packages", "litellm-1.74.0.dist-info"))
	put(t, filepath.Join(venvs, "aider-chat", "Scripts", "aider.exe"), script)
	mkdir(t, filepath.Join(venvs, "black", "Lib", "site-packages", "black-25.1.0.dist-info"))
	put(t, filepath.Join(grace, ".npmrc"), "; Grace's npm settings\nregistry=https://registry.npmjs.org/\nprefix = \"${APPDATA}"+string(filepath.Separator)+"npm-global\"\n")
	put(t, filepath.Join(grace, "AppData", "Roaming", "npm-global", "node_modules", "@google", "gemini-cli", "package.json"), packageJSON("@google/gemini-cli", "0.63.0"))
	put(t, filepath.Join(grace, "AppData", "Roaming", "npm-global", "node_modules", "typescript", "package.json"), packageJSON("typescript", "5.9.3"))

	machineBin := filepath.Join(root, "Program Files", "GitHub Copilot")
	put(t, filepath.Join(machineBin, "copilot.exe"), script)
	system := filepath.Join(root, "Windows", "System32")
	put(t, filepath.Join(system, "notepad.exe"), script)
	put(t, filepath.Join(system, "where.exe"), script)

	sep := string(filepath.Separator)
	h := &fakeCLIHost{
		profiles: []Profile{
			{User: hostinfo.User{SID: adaSID, Account: `AzureAD\AdaLovelace`}, Dir: ada, Loaded: true},
			{User: hostinfo.User{SID: graceSID, Account: `CONTOSO\grace`}, Dir: grace},
		},
		userPath: map[string]string{
			adaSID: "%APPDATA%" + sep + "npm;%USERPROFILE%" + sep + ".local" + sep + "bin;;%LOCALAPPDATA%" + sep + "Microsoft" + sep + "WindowsApps;relative" + sep + "bin;%UNSET%" + sep + "bin",
		},
		machinePath: system + `;"` + machineBin + `";` + system + sep + `;\\fileserver\tools`,
		env:         map[string]string{"SYSTEMROOT": filepath.Join(root, "Windows")},
		versions: map[string]string{
			filepath.Join(machineBin, "copilot.exe"):            "1.0.94.0",
			filepath.Join(grace, ".local", "bin", "claude.exe"): "2.1.295.0",
		},
	}
	return h, marker
}

type cliFound struct {
	app, version, userRef string
}

func summariseCLI(t *testing.T, recs []discovery.Record) []cliFound {
	t.Helper()
	var out []cliFound
	for _, r := range recs {
		if r.Type != protocol.DiscoveryTypeCLIInstalled || r.Basis != protocol.DetectionBasisPackageScan {
			t.Fatalf("record %+v is not a package scan's cli_installed", r)
		}
		if r.Person == nil || r.UserRef != "" {
			t.Fatalf("record %+v is not attributed to its profile's owner", r)
		}
		out = append(out, cliFound{r.AppKey, r.Version, r.Person.UserRef})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].userRef != out[j].userRef {
			return out[i].userRef < out[j].userRef
		}
		return out[i].app < out[j].app
	})
	return out
}

func TestCLIScannerFindsTheCatalogsCLIs(t *testing.T) {
	host, marker := cliTree(t)
	counters := core.NewCounterSet(time.Now())
	recs, errs := NewCLIScanner(host, person).ScanCounted(context.Background(), cliCatalog(), counters)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	want := []cliFound{
		// Grace: the native install's newest release, the pipx venv, the .npmrc prefix, and the
		// machine PATH's copilot.exe with its file version.
		{"aider", "0.86.1", "u_1001"},
		{"claude_code", "2.1.295", "u_1001"},
		{"copilot_cli", "1.0.94.0", "u_1001"},
		{"gemini_cli", "0.63.0", "u_1001"},
		// Ada: the four npm packages. Their shims on her PATH, and the machine's copilot.exe, are
		// the same apps and are not reported again.
		{"claude_code", "2.1.295", "u_4444"},
		{"codex", "0.162.0", "u_4444"},
		{"copilot_cli", "1.0.94", "u_4444"},
		{"gemini_cli", "0.63.0", "u_4444"},
	}
	if got := summariseCLI(t, recs); !reflect.DeepEqual(got, want) {
		t.Fatalf("found\n%v\nwant\n%v", got, want)
	}
	for _, r := range recs {
		if want := map[string]string{"u_4444": `AzureAD\AdaLovelace`, "u_1001": `CONTOSO\grace`}[r.Person.UserRef]; r.Person.SubjectName != want {
			t.Errorf("%s is attributed to %+v, want %s", r.AppKey, r.Person, want)
		}
	}
	// Files checked: Ada's five package.json files; Grace's launcher, aider's dist-info, two
	// package.json files and copilot.exe.
	if c := counters.Cumulative(); c[protocol.CounterObserved] != 10 || c[protocol.CounterErrors] != 0 {
		t.Fatalf("counters %v, want 10 observed", c)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the scan ran a discovered file")
	}
}

// Without a versions folder the native install's version is the launcher's file version, and
// without that it is empty.
func TestCLIScannerNativeVersionFallsBackToTheFileVersion(t *testing.T) {
	host, _ := cliTree(t)
	grace := host.profiles[1].Dir
	if err := os.RemoveAll(filepath.Join(grace, ".local", "share")); err != nil {
		t.Fatal(err)
	}
	recs, _ := NewCLIScanner(host, person).Scan(context.Background(), cliCatalog())
	if v := versionOf(recs, "claude_code", "u_1001"); v != "2.1.295.0" {
		t.Fatalf("version %q, want the launcher's file version", v)
	}
	host.versions = nil
	recs, _ = NewCLIScanner(host, person).Scan(context.Background(), cliCatalog())
	if v := versionOf(recs, "claude_code", "u_1001"); v != "" {
		t.Fatalf("version %q, want none", v)
	}
}

func versionOf(recs []discovery.Record, app, userRef string) string {
	for _, r := range recs {
		if r.AppKey == app && r.Person.UserRef == userRef {
			return r.Version
		}
	}
	return "missing"
}

// A CLI found only on the PATH is reported once per profile, from the first folder that has it; a
// .cmd or .ps1 has no version.
func TestCLIScannerPathHits(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "Users", "ada")
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	put(t, filepath.Join(first, "Codex.CMD"), "")
	put(t, filepath.Join(second, "codex.exe"), "")
	put(t, filepath.Join(second, "gemini.ps1"), "")
	put(t, filepath.Join(second, "copilot"), "")      // no extension Windows runs
	put(t, filepath.Join(second, "claude.bat"), "")   // not one of the three
	mkdir(t, filepath.Join(second, "claude.exe"))     // a folder
	put(t, filepath.Join(second, "claude-x.exe"), "") // another name
	host := &fakeCLIHost{
		profiles:    []Profile{{User: hostinfo.User{SID: adaSID}, Dir: home, Loaded: true}},
		userPath:    map[string]string{adaSID: first},
		machinePath: second,
		versions:    map[string]string{filepath.Join(second, "codex.exe"): "0.162.0.0"},
	}
	recs, errs := NewCLIScanner(host, person).Scan(context.Background(), cliCatalog())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	want := []cliFound{{"codex", "", "u_4444"}, {"gemini_cli", "", "u_4444"}}
	if got := summariseCLI(t, recs); !reflect.DeepEqual(got, want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	// Without a loaded hive the user's own PATH is not read.
	host.profiles[0].Loaded = false
	recs, _ = NewCLIScanner(host, person).Scan(context.Background(), cliCatalog())
	want = []cliFound{{"codex", "0.162.0.0", "u_4444"}, {"gemini_cli", "", "u_4444"}}
	if got := summariseCLI(t, recs); !reflect.DeepEqual(got, want) {
		t.Fatalf("without the hive: found %v, want %v", got, want)
	}
}

// Packages, venvs and PATH files that are not the catalog's are not emitted.
func TestCLIScannerIgnoresOtherPackages(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "Users", "ada")
	put(t, filepath.Join(home, "AppData", "Roaming", "npm", "node_modules", "left-pad", "package.json"), packageJSON("left-pad", "1.3.0"))
	put(t, filepath.Join(home, "AppData", "Roaming", "npm", "node_modules", "@types", "node", "package.json"), packageJSON("@types/node", "24.0.0"))
	// A folder named after a catalog package holds another package: the name inside decides.
	put(t, filepath.Join(home, "AppData", "Roaming", "npm", "node_modules", "@openai", "codex", "package.json"), packageJSON("codex-lookalike", "1.0.0"))
	mkdir(t, filepath.Join(home, ".local", "pipx", "venvs", "black", "Lib", "site-packages", "black-25.1.0.dist-info"))
	put(t, filepath.Join(home, ".local", "bin", "black.exe"), "")
	put(t, filepath.Join(home, ".local", "bin", "uv.exe"), "")
	host := &fakeCLIHost{
		profiles: []Profile{{User: hostinfo.User{SID: adaSID}, Dir: home, Loaded: true}},
		userPath: map[string]string{adaSID: filepath.Join(home, ".local", "bin")},
	}
	recs, errs := NewCLIScanner(host, person).Scan(context.Background(), cliCatalog())
	if len(errs) != 0 || len(recs) != 0 {
		t.Fatalf("found %v with errors %v, want nothing", recs, errs)
	}
	// Without a catalog nothing matches.
	full, _ := cliTree(t)
	if recs, _ := NewCLIScanner(full, person).Scan(context.Background(), nil); len(recs) != 0 {
		t.Fatalf("a scan without a catalog found %v", recs)
	}
}

// A place that cannot be read is one error each, and everything else is still found. A
// package.json over 1 MiB is not read.
func TestCLIScannerReportsWhatItCouldNotRead(t *testing.T) {
	host, _ := cliTree(t)
	ada := host.profiles[0].Dir
	modules := filepath.Join(ada, "AppData", "Roaming", "npm", "node_modules")
	put(t, filepath.Join(modules, "huge", "package.json"), `{"name":"huge","pad":"`+strings.Repeat("x", maxConfigFile)+`"}`)
	put(t, filepath.Join(modules, "broken", "package.json"), `{"name":`)
	host.userPathErr = map[string]error{adaSID: errors.New("access denied")}
	host.machineErr = errors.New("access denied")
	host.machinePath = ""
	counters := core.NewCounterSet(time.Now())
	recs, errs := NewCLIScanner(host, person).ScanCounted(context.Background(), cliCatalog(), counters)
	if len(errs) != 4 {
		t.Fatalf("errors = %v, want 4 (huge, broken, Ada's PATH, the machine PATH)", errs)
	}
	got := summariseCLI(t, recs)
	if len(got) != 7 {
		t.Fatalf("found %v, want everything but Grace's PATH hit", got)
	}
	for _, f := range got {
		if f.app == "copilot_cli" && f.userRef == "u_1001" {
			t.Fatalf("found %v from a machine PATH that could not be read", f)
		}
	}

	host.profilesErr = errors.New("ProfileList unreadable")
	host.profiles = nil
	recs, errs = NewCLIScanner(host, person).Scan(context.Background(), cliCatalog())
	if len(recs) != 0 || len(errs) != 2 {
		t.Fatalf("with no profiles: %v, errors %v", recs, errs)
	}
}

// A cancelled scan stops between profiles and says so.
func TestCLIScannerStopsWhenCancelled(t *testing.T) {
	host, _ := cliTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recs, errs := NewCLIScanner(host, person).Scan(ctx, cliCatalog())
	if len(recs) != 0 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("found %v, errors %v", recs, errs)
	}
}

func TestCLIScannerFinds(t *testing.T) {
	host, _ := cliTree(t)
	s := NewCLIScanner(host, person)
	if !s.Finds(context.Background(), cliCatalog(), "claude_code") || !s.Finds(context.Background(), cliCatalog(), "aider") {
		t.Fatal("an installed CLI is not found")
	}
	if s.Finds(context.Background(), cliCatalog(), "cursor") || s.Finds(context.Background(), nil, "claude_code") {
		t.Fatal("found a CLI that is not installed, or without a catalog")
	}
}

func TestNPMRCPrefix(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	env := func(name string) (string, bool) {
		v, ok := map[string]string{"APPDATA": filepath.Join(home, "AppData", "Roaming")}[name]
		return v, ok
	}
	sep := string(filepath.Separator)
	abs := filepath.Join(root, "npm-prefix")
	for name, tc := range map[string]struct {
		content, want string
	}{
		"plain":           {"prefix=" + abs + "\n", abs},
		"spaced, quoted":  {"  prefix = '" + abs + "'\r\n", abs},
		"variable":        {"prefix=${APPDATA}" + sep + "npm-global", filepath.Join(home, "AppData", "Roaming", "npm-global")},
		"unset variable":  {"prefix=${NOPE}" + sep + "npm", ""},
		"home":            {"prefix=~" + sep + "npm", filepath.Join(home, "npm")},
		"relative":        {"prefix=npm-global", ""},
		"comments":        {"; prefix=" + abs + "\n# prefix=" + abs + "\n", ""},
		"last one wins":   {"prefix=" + filepath.Join(root, "a") + "\nprefix=" + abs, abs},
		"other key":       {"prefixes=" + abs, ""},
		"in a section":    {"[section]\nprefix=" + abs, ""},
		"none":            {"registry=https://registry.npmjs.org/", ""},
		"empty value":     {"prefix=", ""},
		"tilde run on":    {"prefix=~npm", ""},
		"before sections": {"prefix=" + abs + "\n[section]\nprefix=" + filepath.Join(root, "b"), abs},
	} {
		file := filepath.Join(root, name+".npmrc")
		put(t, file, tc.content)
		got, err := npmrcPrefix(file, home, env)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, tc.want)
		}
	}
	if got, err := npmrcPrefix(filepath.Join(root, "missing"), home, env); got != "" || err != nil {
		t.Fatalf("a missing .npmrc: %q, %v", got, err)
	}
	big := filepath.Join(root, "big.npmrc")
	put(t, big, strings.Repeat("; padding\n", maxConfigFile/10+1))
	if _, err := npmrcPrefix(big, home, env); err == nil {
		t.Fatal("an .npmrc over 1 MiB was read")
	}
}

func TestNewestVersion(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"2.1.9", "2.1.10", "2.1.10-rc.1", "2.0.300.exe", "latest", "2.1.11.lock", "2.1.11.tmp"} {
		put(t, filepath.Join(dir, n), "")
	}
	if v, err := newestVersion(dir); v != "2.1.10" || err != nil {
		t.Fatalf("newest = %q, %v; want 2.1.10", v, err)
	}
	put(t, filepath.Join(dir, "2.2.0.exe"), "")
	if v, _ := newestVersion(dir); v != "2.2.0" {
		t.Fatalf("newest = %q, want 2.2.0 (named with .exe)", v)
	}
	if v, err := newestVersion(filepath.Join(dir, "missing")); v != "" || err != nil {
		t.Fatalf("a missing folder: %q, %v", v, err)
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2.1.10", "2.1.9", 1}, {"2.1", "2.1.0", -1}, {"2.1.0", "2.1.0-rc.1", 1}, {"2.1.0-rc.2", "2.1.0-rc.1", 1}, {"1.0.0", "1.0.0", 0},
	} {
		if got := compareRelease(tc.a, tc.b); got != tc.want {
			t.Errorf("compareRelease(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestPathValues(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b c")
	if got := splitPath(a + `;;"` + b + `"; relative ;\\server\share;//server/share;` + a); !reflect.DeepEqual(got, []string{a, b, a}) {
		t.Fatalf("splitPath = %v", got)
	}
	env := func(name string) (string, bool) {
		v, ok := map[string]string{"USERPROFILE": root}[strings.ToUpper(name)]
		return v, ok
	}
	if got := expandPercent("%UserProfile%\\bin;%UNSET%\\bin;100%", env); got != root+"\\bin;%UNSET%\\bin;100%" {
		t.Fatalf("expandPercent = %q", got)
	}
}

func TestDistInfo(t *testing.T) {
	for in, want := range map[string][2]string{
		"aider_chat-0.86.1.dist-info": {"aider_chat", "0.86.1"},
		"black-25.1.0.dist-info":      {"black", "25.1.0"},
		"black-25.1.0.egg-info":       {"", ""},
		"black.dist-info":             {"", ""},
		"-1.0.dist-info":              {"", ""},
	} {
		name, ver, _ := distInfo(in)
		if name != want[0] || ver != want[1] {
			t.Errorf("distInfo(%q) = %q %q, want %q", in, name, ver, want)
		}
	}
	if pythonName("Aider_Chat") != pythonName("aider-chat") || pythonName("a.-_b") != "a-b" {
		t.Fatal("python names do not normalize")
	}
}

// The provider scans through ScanCounted and the real emitter mints one valid cli_installed
// envelope per app and person, with the files checked counted as observed.
func TestCLIScanEmitsDiscoveryEnvelopes(t *testing.T) {
	host, _ := cliTree(t)
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b := cliCatalog()
	pipe := &recordingPipeline{}
	em, err := discovery.New(discovery.Config{Pipeline: pipe, Dir: dir, Clock: clock, Bundles: func() *policy.Bundle { return b }, DeviceID: testDevice})
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Scanners: []Scanner{NewCLIScanner(host, person)}, Emitter: em, Bundles: func() *policy.Bundle { return b }, Clock: clock, After: newTicks().after})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())

	type envelope struct {
		Kind            string `json:"kind"`
		Route           string `json:"source"`
		ToolFingerprint string `json:"tool_fingerprint"`
		DiscoveryType   string `json:"discovery_type"`
		DetectionBasis  string `json:"detection_basis"`
		AppVersion      string `json:"app_version"`
		UserRef         string `json:"user_ref"`
	}
	got := map[string]bool{}
	for _, raw := range pipe.raw {
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind != "discovery" || e.Route != "inv.scan" || e.DiscoveryType != "cli_installed" || e.DetectionBasis != "package_scan" {
			t.Errorf("envelope %+v is not a cli_installed discovery on inv.scan", e)
		}
		got[e.ToolFingerprint+" "+e.AppVersion+" "+e.UserRef] = true
	}
	for _, want := range []string{
		"app:claude_code 2.1.295 u_4444", "app:codex 0.162.0 u_4444", "app:gemini_cli 0.63.0 u_4444", "app:copilot_cli 1.0.94 u_4444",
		"app:claude_code 2.1.295 u_1001", "app:copilot_cli 1.0.94.0 u_1001", "app:gemini_cli 0.63.0 u_1001", "app:aider 0.86.1 u_1001",
	} {
		if !got[want] {
			t.Errorf("no envelope %s among %v", want, got)
		}
	}
	if len(pipe.raw) != 8 {
		t.Fatalf("%d envelopes, want 8", len(pipe.raw))
	}
	c := p.Counters().Cumulative()
	if c[protocol.CounterEmitted] != 8 || c[protocol.CounterObserved] != 10 || c[protocol.CounterErrors] != 0 {
		t.Fatalf("counters %v, want 8 emitted and 10 observed", c)
	}
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health %s/%s, want healthy", h.State, h.Detail)
	}
}
