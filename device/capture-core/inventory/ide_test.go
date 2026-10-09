package inventory

import (
	"archive/zip"
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

// ideCatalog holds the catalog's extension ids for the three IDE assistants. The JetBrains plugin ids
// are fixture values, not catalog facts: the catalog names none.
func ideCatalog() *policy.Bundle {
	ext := func(id string) policy.CatalogSignal {
		return policy.CatalogSignal{Platform: "any", Kind: policy.SignalIDEExtensionID, Value: id}
	}
	return &policy.Bundle{
		Endpoint: policy.EndpointPolicy{
			Inventory:            policy.EndpointInventory{Enabled: true, IntervalMinutes: 360},
			DiscoveryDailyBudget: 200,
		},
		Catalog: []policy.CatalogApp{
			{AppKey: "claude_code_vscode", Category: "ide_assistant", Signals: []policy.CatalogSignal{ext("anthropic.claude-code")}},
			{AppKey: "continue", Category: "ide_assistant", Signals: []policy.CatalogSignal{
				ext("continue.continue"), ext("com.github.continuedev.continueintellijextension"),
			}},
			{AppKey: "github_copilot", Category: "ide_assistant", Signals: []policy.CatalogSignal{
				ext("github.copilot-chat"), ext("com.github.copilot"),
			}},
		},
	}
}

// storedExtension is one entry of an extensions.json as VS Code writes it.
func storedExtension(id, version, folder string) map[string]any {
	return map[string]any{
		"identifier":       map[string]string{"id": id, "uuid": "00000000-0000-4000-8000-000000000000"},
		"version":          version,
		"location":         map[string]any{"$mid": 1, "path": "/c:/Users/x/.vscode/extensions/" + folder, "scheme": "file"},
		"relativeLocation": folder,
		"metadata":         map[string]any{"installedTimestamp": 1759900000000, "source": "gallery"},
	}
}

func extensionsJSON(t *testing.T, entries ...map[string]any) string {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pluginXML(id, name, version string) string {
	x := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<idea-plugin require-restart="true">`
	if id != "" {
		x += "\n  <id>" + id + "</id>"
	}
	return x + "\n  <name>" + name + "</name>\n  <version>" + version + "</version>\n" +
		`  <vendor url="https://example.com">Example</vendor>` + "\n  <description><![CDATA[<p>An <b>example</b>.</p>]]></description>\n" +
		`  <depends>com.intellij.modules.platform</depends>` + "\n</idea-plugin>\n"
}

// writeJar builds a jar holding the entries, in order.
func writeJar(t *testing.T, path string, entries ...[2]string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, e := range entries {
		ew, err := w.Create(e[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ew.Write([]byte(e[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// ideTree is the fixture device under root:
//   - Ada has VS Code, whose extensions.json lists Copilot Chat, Claude Code (a platform-specific
//     build) and Continue beside a non-AI extension; the folder also holds an older Copilot Chat that
//     the list no longer names. Her Cursor has no extensions.json, so its folder names count.
//   - Grace has Windsurf, whose list names Continue in another case, and two JetBrains IDEs: IntelliJ
//     IDEA with the Copilot plugin (its descriptor in the second of two jars) and a non-AI plugin, and
//     PyCharm with Continue's plugin, whose descriptor has no <id>.
func ideTree(t *testing.T) *fakeCLIHost {
	t.Helper()
	root := t.TempDir()
	ada, grace := filepath.Join(root, "Users", "ada"), filepath.Join(root, "Users", "grace")

	vscode := filepath.Join(ada, ".vscode", "extensions")
	put(t, filepath.Join(vscode, "extensions.json"), extensionsJSON(t,
		storedExtension("github.copilot-chat", "0.44.0", "github.copilot-chat-0.44.0"),
		storedExtension("anthropic.claude-code", "2.1.295", "anthropic.claude-code-2.1.295-win32-x64"),
		storedExtension("Continue.continue", "1.3.40", "continue.continue-1.3.40"),
		storedExtension("ms-python.python", "2026.18.0", "ms-python.python-2026.18.0"),
	))
	for _, d := range []string{"github.copilot-chat-0.44.0", "github.copilot-chat-0.43.1", "anthropic.claude-code-2.1.295-win32-x64",
		"continue.continue-1.3.40", "ms-python.python-2026.18.0"} {
		put(t, filepath.Join(vscode, d, "package.json"), "{}")
	}
	put(t, filepath.Join(vscode, ".obsolete"), `{"github.copilot-chat-0.43.1":true}`)

	cursor := filepath.Join(ada, ".cursor", "extensions")
	for _, d := range []string{"anthropic.claude-code-2.1.290-win32-x64", "continue.continue-1.3.38", "esbenp.prettier-vscode-11.0.0", "notes"} {
		put(t, filepath.Join(cursor, d, "package.json"), "{}")
	}
	put(t, filepath.Join(cursor, "github.copilot-chat-0.44.0.vsix"), "")

	put(t, filepath.Join(grace, ".windsurf", "extensions", "extensions.json"), extensionsJSON(t,
		storedExtension("Continue.Continue", "1.3.39", "continue.continue-1.3.39"),
	))

	jb := filepath.Join(grace, "AppData", "Roaming", "JetBrains")
	idea := filepath.Join(jb, "IntelliJIdea2026.2", "plugins")
	writeJar(t, filepath.Join(idea, "github-copilot-intellij", "lib", "core.jar"), [2]string{"com/github/copilot/Core.class", "\xca\xfe\xba\xbe"})
	writeJar(t, filepath.Join(idea, "github-copilot-intellij", "lib", "github-copilot-intellij-1.5.62.jar"),
		[2]string{"META-INF/MANIFEST.MF", "Manifest-Version: 1.0\n"},
		[2]string{"META-INF/plugin.xml", pluginXML("com.github.copilot", "GitHub Copilot", "1.5.62-243")})
	writeJar(t, filepath.Join(idea, "IdeaVim", "lib", "IdeaVim.jar"), [2]string{"META-INF/plugin.xml", pluginXML("IdeaVIM", "IdeaVim", "2.27.0")})
	put(t, filepath.Join(idea, "IdeaVim.zip"), "")
	writeJar(t, filepath.Join(jb, "PyCharm2026.1", "plugins", "continue-intellij-extension", "lib", "continue-intellij-extension-1.0.50.jar"),
		[2]string{"META-INF/plugin.xml", pluginXML("", "com.github.continuedev.continueintellijextension", "1.0.50")})
	put(t, filepath.Join(jb, "consentOptions", "accepted"), "")

	return &fakeCLIHost{profiles: []Profile{
		{User: hostinfo.User{SID: adaSID, Account: `AzureAD\AdaLovelace`}, Dir: ada, Loaded: true},
		{User: hostinfo.User{SID: graceSID, Account: `CONTOSO\grace`}, Dir: grace},
	}}
}

type ideFound struct {
	app, version, hostApp, userRef string
}

func summariseIDE(t *testing.T, recs []discovery.Record) []ideFound {
	t.Helper()
	var out []ideFound
	for _, r := range recs {
		if r.Type != protocol.DiscoveryTypeIDEExtension || r.Basis != protocol.DetectionBasisExtensionScan {
			t.Fatalf("record %+v is not an extension scan's ide_extension", r)
		}
		if r.Person == nil || r.UserRef != "" {
			t.Fatalf("record %+v is not attributed to its profile's owner", r)
		}
		out = append(out, ideFound{r.AppKey, r.Version, r.HostApp, r.Person.UserRef})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.userRef+a.hostApp+a.app+a.version < b.userRef+b.hostApp+b.app+b.version
	})
	return out
}

func TestIDEScannerFindsTheCatalogsExtensions(t *testing.T) {
	host := ideTree(t)
	counters := core.NewCounterSet(time.Now())
	recs, errs := NewIDEScanner(host, person).ScanCounted(context.Background(), ideCatalog(), counters)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	want := []ideFound{
		{"continue", "1.0.50", "app:jetbrains", "u_1001"},
		{"github_copilot", "1.5.62-243", "app:jetbrains", "u_1001"},
		{"continue", "1.3.39", "app:windsurf", "u_1001"},
		// Cursor's folder names: the platform suffix is not part of the version.
		{"claude_code_vscode", "2.1.290", "app:cursor", "u_4444"},
		{"continue", "1.3.38", "app:cursor", "u_4444"},
		// VS Code's list: the older Copilot Chat folder it no longer names is not reported.
		{"claude_code_vscode", "2.1.295", "app:vscode", "u_4444"},
		{"continue", "1.3.40", "app:vscode", "u_4444"},
		{"github_copilot", "0.44.0", "app:vscode", "u_4444"},
	}
	if got := summariseIDE(t, recs); !reflect.DeepEqual(got, want) {
		t.Fatalf("found\n%v\nwant\n%v", got, want)
	}
	for _, r := range recs {
		if want := map[string]string{"u_4444": `AzureAD\AdaLovelace`, "u_1001": `CONTOSO\grace`}[r.Person.UserRef]; r.Person.SubjectName != want {
			t.Errorf("%s is attributed to %+v, want %s", r.AppKey, r.Person, want)
		}
	}
	// Read: two extensions.json files, Cursor's three extension folders and three plugin
	// descriptors.
	if c := counters.Cumulative(); c[protocol.CounterObserved] != 8 || c[protocol.CounterErrors] != 0 {
		t.Fatalf("counters %v, want 8 observed", c)
	}
}

// Extensions that are not the catalog's are not emitted, and without a catalog nothing is.
func TestIDEScannerIgnoresOtherExtensions(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "Users", "ada")
	put(t, filepath.Join(home, ".vscode", "extensions", "extensions.json"), extensionsJSON(t,
		storedExtension("ms-python.python", "2026.18.0", "ms-python.python-2026.18.0"),
		storedExtension("github.copilot-chat-lookalike", "1.0.0", "github.copilot-chat-lookalike-1.0.0"),
	))
	put(t, filepath.Join(home, ".cursor", "extensions", "github.copilot-chat-lookalike-1.0.0", "package.json"), "{}")
	writeJar(t, filepath.Join(home, "AppData", "Roaming", "JetBrains", "GoLand2026.2", "plugins", "IdeaVim", "lib", "IdeaVim.jar"),
		[2]string{"META-INF/plugin.xml", pluginXML("IdeaVIM", "IdeaVim", "2.27.0")})
	host := &fakeCLIHost{profiles: []Profile{{User: hostinfo.User{SID: adaSID}, Dir: home, Loaded: true}}}
	recs, errs := NewIDEScanner(host, person).Scan(context.Background(), ideCatalog())
	if len(errs) != 0 || len(recs) != 0 {
		t.Fatalf("found %v with errors %v, want nothing", recs, errs)
	}
	if recs, _ := NewIDEScanner(ideTree(t), person).Scan(context.Background(), nil); len(recs) != 0 {
		t.Fatalf("a scan without a catalog found %v", recs)
	}
}

// A place that cannot be read is one error each, and everything else is still found: an
// extensions.json over 4 MiB or not JSON, a plugin.xml entry over 1 MiB, a jar that is not a zip.
func TestIDEScannerReportsWhatItCouldNotRead(t *testing.T) {
	host := ideTree(t)
	ada, grace := host.profiles[0].Dir, host.profiles[1].Dir
	put(t, filepath.Join(ada, ".windsurf", "extensions", "extensions.json"), `[{"identifier":{"id":"continue.continue"},"version":"1.0.0","pad":"`+
		strings.Repeat("x", maxExtensionsFile)+`"}]`)
	put(t, filepath.Join(grace, ".vscode", "extensions", "extensions.json"), `[{"identifier":`)
	plugins := filepath.Join(grace, "AppData", "Roaming", "JetBrains", "WebStorm2026.2", "plugins")
	writeJar(t, filepath.Join(plugins, "huge", "lib", "huge.jar"), [2]string{"META-INF/plugin.xml",
		pluginXML("com.github.copilot", "Huge", "1.0.0") + "<!--" + strings.Repeat("x", maxPluginXML) + "-->"})
	put(t, filepath.Join(plugins, "broken", "lib", "broken.jar"), "not a zip")
	counters := core.NewCounterSet(time.Now())
	recs, errs := NewIDEScanner(host, person).ScanCounted(context.Background(), ideCatalog(), counters)
	if len(errs) != 4 {
		t.Fatalf("errors = %v, want 4 (Ada's Windsurf list, Grace's VS Code list, huge.jar, broken.jar)", errs)
	}
	if got := summariseIDE(t, recs); len(got) != 8 {
		t.Fatalf("found %v, want the eight of the full tree", got)
	}

	host.profilesErr = errors.New("ProfileList unreadable")
	host.profiles = nil
	recs, errs = NewIDEScanner(host, person).Scan(context.Background(), ideCatalog())
	if len(recs) != 0 || len(errs) != 1 {
		t.Fatalf("with no profiles: %v, errors %v", recs, errs)
	}
}

// A cancelled scan stops between profiles and says so.
func TestIDEScannerStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recs, errs := NewIDEScanner(ideTree(t), person).Scan(ctx, ideCatalog())
	if len(recs) != 0 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("found %v, errors %v", recs, errs)
	}
}

func TestParseExtensionFolder(t *testing.T) {
	for name, want := range map[string][2]string{
		"github.copilot-chat-0.44.0":              {"github.copilot-chat", "0.44.0"},
		"anthropic.claude-code-2.1.295-win32-x64": {"anthropic.claude-code", "2.1.295"},
		"ms-python.python-2026.18.0-win32-arm64":  {"ms-python.python", "2026.18.0"},
		"continue.continue-1.3.40":                {"continue.continue", "1.3.40"},
		"continue.continue":                       {"", ""},
		"nopublisher-1.0.0":                       {"", ""},
		"continue.continue-1.3":                   {"", ""},
		".obsolete":                               {"", ""},
	} {
		id, ver, _ := parseExtensionFolder(name)
		if id != want[0] || ver != want[1] {
			t.Errorf("parseExtensionFolder(%q) = %q %q, want %q", name, id, ver, want)
		}
	}
}

func TestParseExtensionsJSON(t *testing.T) {
	for in, want := range map[string][]extension{
		"":      nil,
		" \r\n": nil,
		"[]":    {},
		`[{"identifier":{"id":"a.b"},"version":"1.0.0"},{"identifier":{},"version":"2.0.0"}]`: {{"a.b", "1.0.0"}},
	} {
		got, err := parseExtensionsJSON([]byte(in))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("parseExtensionsJSON(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{`{}`, `[`, `["a.b"]`} {
		if _, err := parseExtensionsJSON([]byte(in)); err == nil {
			t.Errorf("parseExtensionsJSON(%q) accepted", in)
		}
	}
}

func TestParsePluginXML(t *testing.T) {
	got, err := parsePluginXML([]byte(pluginXML(" com.github.copilot ", "GitHub Copilot", " 1.5.62-243 ")))
	if err != nil || got != (extension{"com.github.copilot", "1.5.62-243"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, _ := parsePluginXML([]byte(pluginXML("", "Named Only", "1.0"))); got.id != "Named Only" {
		t.Fatalf("a descriptor without <id> is %q, want its name", got.id)
	}
	for _, in := range []string{`<plugin><id>a</id></plugin>`, `<idea-plugin><id>a</id>`, `not xml`} {
		if _, err := parsePluginXML([]byte(in)); err == nil {
			t.Errorf("parsePluginXML(%q) accepted", in)
		}
	}
}

// The provider scans through ScanCounted and the real emitter mints one valid ide_extension envelope
// per app, IDE and person, carrying the IDE as host_app.
func TestIDEScanEmitsDiscoveryEnvelopes(t *testing.T) {
	host := ideTree(t)
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b := ideCatalog()
	pipe := &recordingPipeline{}
	em, err := discovery.New(discovery.Config{Pipeline: pipe, Dir: dir, Clock: clock, Bundles: func() *policy.Bundle { return b }, DeviceID: testDevice})
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Scanners: []Scanner{NewIDEScanner(host, person)}, Emitter: em, Bundles: func() *policy.Bundle { return b }, Clock: clock, After: newTicks().after})
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
		HostApp         string `json:"host_app"`
		UserRef         string `json:"user_ref"`
		Direction       string `json:"direction"`
	}
	got := map[string]bool{}
	for _, raw := range pipe.raw {
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind != "discovery" || e.Route != "inv.scan" || e.DiscoveryType != "ide_extension" || e.DetectionBasis != "extension_scan" || e.Direction != "none" {
			t.Errorf("envelope %+v is not an ide_extension discovery on inv.scan", e)
		}
		got[e.ToolFingerprint+" "+e.AppVersion+" "+e.HostApp+" "+e.UserRef] = true
	}
	for _, want := range []string{
		"app:github_copilot 0.44.0 app:vscode u_4444", "app:claude_code_vscode 2.1.295 app:vscode u_4444", "app:continue 1.3.40 app:vscode u_4444",
		"app:claude_code_vscode 2.1.290 app:cursor u_4444", "app:continue 1.3.38 app:cursor u_4444",
		"app:continue 1.3.39 app:windsurf u_1001", "app:github_copilot 1.5.62-243 app:jetbrains u_1001", "app:continue 1.0.50 app:jetbrains u_1001",
	} {
		if !got[want] {
			t.Errorf("no envelope %s among %v", want, got)
		}
	}
	if len(pipe.raw) != 8 {
		t.Fatalf("%d envelopes, want 8", len(pipe.raw))
	}
	c := p.Counters().Cumulative()
	if c[protocol.CounterEmitted] != 8 || c[protocol.CounterObserved] != 8 || c[protocol.CounterErrors] != 0 {
		t.Fatalf("counters %v, want 8 emitted and 8 observed", c)
	}
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health %s/%s, want healthy", h.State, h.Detail)
	}
}
