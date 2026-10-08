package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/hostinfo"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

const (
	adaSID     = "S-1-12-1-1111111111-2222222222-3333333333-4444444444"
	graceSID   = "S-1-5-21-1004336348-1177238915-682003330-1001"
	testDevice = "5f0f0c1e-0000-4000-8000-0000000000d1"
)

// testCatalog holds the catalog's shapes for the apps the fixtures install. The ChatGPT package
// family is a fixture value, not a catalog fact.
func testCatalog() *policy.Bundle {
	return &policy.Bundle{
		Endpoint: policy.EndpointPolicy{
			Inventory:            policy.EndpointInventory{Enabled: true, IntervalMinutes: 360},
			DiscoveryDailyBudget: 200,
		},
		Catalog: []policy.CatalogApp{
			{AppKey: "chatgpt_desktop", Category: "chat_assistant", Signals: []policy.CatalogSignal{
				{Platform: "windows", Kind: policy.SignalWindowsAppx, Value: "OpenAI.ChatGPT-Desktop_2p2nqsd0c76g0"},
			}},
			{AppKey: "claude_desktop", Category: "chat_assistant", Signals: []policy.CatalogSignal{
				{Platform: "windows", Kind: policy.SignalPublisher, Value: "Anthropic, PBC"},
				{Platform: "windows", Kind: policy.SignalWindowsAppx, Value: "Claude_pzs8sxrjxfjjc"},
			}},
			{AppKey: "jetbrains", Category: "ide", Signals: []policy.CatalogSignal{
				{Platform: "windows", Kind: policy.SignalPublisher, Value: "JetBrains s.r.o."},
				{Platform: "windows", Kind: policy.SignalWindowsUninstallName, Value: "IntelliJ IDEA"},
			}},
			{AppKey: "vscode", Category: "ide", Signals: []policy.CatalogSignal{
				{Platform: "windows", Kind: policy.SignalPublisher, Value: "Microsoft Corporation"},
				{Platform: "windows", Kind: policy.SignalWindowsExe, Value: "Code.exe"},
			}},
		},
	}
}

// fakeRegistry is the three reads, with an error per read where a test asks for one.
type fakeRegistry struct {
	users     []hostinfo.User
	usersErr  error
	uninstall map[string][]UninstallEntry
	packages  map[string][]string
	errs      map[string]error // "uninstall:<sid>" or "packages:<sid>"
}

func (r *fakeRegistry) Users() ([]hostinfo.User, error) { return r.users, r.usersErr }

func (r *fakeRegistry) Uninstall(sid string) ([]UninstallEntry, error) {
	return r.uninstall[sid], r.errs["uninstall:"+sid]
}

func (r *fakeRegistry) Packages(sid string) ([]string, error) {
	return r.packages[sid], r.errs["packages:"+sid]
}

// machine is the fixture device: a machine-wide VS Code and IntelliJ IDEA (the latter in the
// 32-bit view), Ada's per-user Squirrel install of Claude Desktop and her Store install of ChatGPT,
// and non-AI apps in each place.
func machine() *fakeRegistry {
	return &fakeRegistry{
		users: []hostinfo.User{{SID: adaSID, Account: `AzureAD\AdaLovelace`}, {SID: graceSID, Account: `CONTOSO\grace`}},
		uninstall: map[string][]UninstallEntry{
			"": {
				{DisplayName: "Microsoft Visual Studio Code", DisplayVersion: "1.140.0", Publisher: "Microsoft Corporation",
					DisplayIcon: `C:\Program Files\Microsoft VS Code\Code.exe`, InstallLocation: `C:\Program Files\Microsoft VS Code\`},
				{DisplayName: "IntelliJ IDEA 2026.2.1", DisplayVersion: "262.8653.112", Publisher: "JetBrains s.r.o.",
					DisplayIcon: `C:\Program Files\JetBrains\IntelliJ IDEA 2026.2.1\bin\idea64.exe`},
				// Non-AI apps: a publisher that also publishes a catalog app does not make them that app.
				{DisplayName: "Microsoft Edge", DisplayVersion: "141.0.3537.57", Publisher: "Microsoft Corporation",
					DisplayIcon: `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe,0`},
				{DisplayName: "7-Zip 25.01 (x64)", DisplayVersion: "25.01", Publisher: "Igor Pavlov", DisplayIcon: `C:\Program Files\7-Zip\7zFM.exe`},
			},
			adaSID: {
				// Squirrel installs per user under %LOCALAPPDATA% with an icon file, not an executable.
				{DisplayName: "Claude", DisplayVersion: "0.14.10", Publisher: "Anthropic, PBC",
					DisplayIcon: `C:\Users\ada\AppData\Local\AnthropicClaude\app.ico`, InstallLocation: `C:\Users\ada\AppData\Local\AnthropicClaude`},
				{DisplayName: "Spotify", DisplayVersion: "1.2.74.477", Publisher: "Spotify AB", DisplayIcon: `C:\Users\ada\AppData\Roaming\Spotify\Spotify.exe`},
			},
		},
		packages: map[string][]string{
			"": {"Microsoft.WindowsCalculator_11.2405.2.0_x64__8wekyb3d8bbwe"},
			adaSID: {
				"OpenAI.ChatGPT-Desktop_1.2025.112.0_x64__2p2nqsd0c76g0",
				"Microsoft.WindowsNotepad_11.2507.26.0_x64__8wekyb3d8bbwe",
				"not a package full name",
			},
		},
	}
}

// person names a user by SID, as the service's peer lookup does.
func person(u hostinfo.User) core.Person {
	return core.Person{UserRef: "u_" + u.SID[len(u.SID)-4:], SubjectName: u.Account}
}

type found struct {
	app, version, userRef string
}

func summarise(recs []discovery.Record) []found {
	var out []found
	for _, r := range recs {
		if r.Type != protocol.DiscoveryTypeAppInstalled || r.Basis != protocol.DetectionBasisInstalledScan {
			panic(fmt.Sprintf("record %+v is not an installed scan", r))
		}
		ref := r.UserRef
		if r.Person != nil {
			if ref != "" {
				panic(fmt.Sprintf("record %+v has both a user_ref and a person", r))
			}
			ref = r.Person.UserRef
		}
		out = append(out, found{r.AppKey, r.Version, ref})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].app < out[j].app })
	return out
}

func TestAppScannerFindsTheCatalogsApps(t *testing.T) {
	recs, errs := NewAppScanner(machine(), person).Scan(context.Background(), testCatalog())
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	want := []found{
		{"chatgpt_desktop", "1.2025.112.0", "u_4444"}, // an MSIX package, per user
		{"claude_desktop", "0.14.10", "u_4444"},       // a per-user Squirrel install, by its publisher
		{"jetbrains", "262.8653.112", "unattributed"}, // a machine install, by its name
		{"vscode", "1.140.0", "unattributed"},         // a machine install, by its executable
	}
	if got := summarise(recs); !reflect.DeepEqual(got, want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	for _, r := range recs {
		if r.Person != nil && r.Person.SubjectName != `AzureAD\AdaLovelace` {
			t.Errorf("%s is attributed to %+v, want Ada", r.AppKey, r.Person)
		}
	}
}

// A non-AI app is not emitted, wherever it is installed.
func TestAppScannerIgnoresOtherApps(t *testing.T) {
	reg := machine()
	reg.uninstall[""] = reg.uninstall[""][2:]
	reg.uninstall[adaSID] = reg.uninstall[adaSID][1:]
	reg.packages[adaSID] = reg.packages[adaSID][1:]
	recs, errs := NewAppScanner(reg, person).Scan(context.Background(), testCatalog())
	if len(errs) != 0 || len(recs) != 0 {
		t.Fatalf("found %v with errors %v, want nothing", summarise(recs), errs)
	}
	// Without a catalog nothing matches.
	if recs, _ := NewAppScanner(machine(), person).Scan(context.Background(), nil); len(recs) != 0 {
		t.Fatalf("a scan without a catalog found %v", summarise(recs))
	}
}

// Once the catalog names an app's executable or uninstall name, its publisher alone no longer
// identifies it.
func TestAppScannerPublisherIsTheLastResort(t *testing.T) {
	b := testCatalog()
	b.Catalog[1].Signals = append(b.Catalog[1].Signals, policy.CatalogSignal{Platform: "windows", Kind: policy.SignalWindowsUninstallName, Value: "Claude Desktop"})
	recs, _ := NewAppScanner(machine(), person).Scan(context.Background(), b)
	for _, r := range recs {
		if r.AppKey == "claude_desktop" {
			t.Fatalf("an entry named Claude matched by publisher alone: %+v", r)
		}
	}
}

// A hive or key that cannot be read is one error each; what could be read is still found.
func TestAppScannerReportsWhatItCouldNotRead(t *testing.T) {
	reg := machine()
	reg.errs = map[string]error{
		"uninstall:" + adaSID: errors.New("access denied"),
		"packages:":           errors.New("access denied"),
	}
	recs, errs := NewAppScanner(reg, person).Scan(context.Background(), testCatalog())
	if len(errs) != 2 {
		t.Fatalf("errors = %v, want 2", errs)
	}
	got := summarise(recs)
	if len(got) != 4 {
		t.Fatalf("found %v, want all four apps (the failing reads still returned their entries)", got)
	}
	reg.usersErr = errors.New("HKEY_USERS unreadable")
	reg.users = nil
	recs, errs = NewAppScanner(reg, person).Scan(context.Background(), testCatalog())
	if len(errs) != 2 || len(recs) != 2 {
		t.Fatalf("with no users: %v, errors %v; want the two machine installs and two errors", summarise(recs), errs)
	}
}

func TestExeBase(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Program Files\Microsoft VS Code\Code.exe`:           "Code.exe",
		`"C:\Program Files\Microsoft VS Code\Code.exe",0`:       "Code.exe",
		`C:\Program Files\Microsoft VS Code\Code.exe,-1`:        "Code.exe",
		`"C:\Program Files\Ollama\ollama app.exe"`:              "ollama app.exe",
		`C:\Users\ada\AppData\Local\AnthropicClaude\app.ico`:    "",
		`C:\Users\ada\AppData\Local\AnthropicClaude\`:           "",
		`C:\Users\ada\AppData\Local\Programs\cursor\Cursor.EXE`: "Cursor.EXE",
		``: "",
	} {
		if got := exeBase(in); got != want {
			t.Errorf("exeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePackageFullName(t *testing.T) {
	family, ver, ok := parsePackageFullName("Microsoft.Windows.Photos_2020.20090.1002.0_x64__8wekyb3d8bbwe")
	if !ok || family != "Microsoft.Windows.Photos_8wekyb3d8bbwe" || ver != "2020.20090.1002.0" {
		t.Fatalf("got %q %q %v", family, ver, ok)
	}
	for _, bad := range []string{"", "Microsoft.Windows.Photos_8wekyb3d8bbwe", "a_b_c_d_e_f", "_1.0.0.0_x64__8wekyb3d8bbwe"} {
		if _, _, ok := parsePackageFullName(bad); ok {
			t.Errorf("%q parsed as a package full name", bad)
		}
	}
}

func TestVersionOverTheEnvelopeLimitIsLeftOut(t *testing.T) {
	long := ""
	for len(long) <= maxVersionChars {
		long += "1."
	}
	if version(long) != "" || version(" 1.2.3 ") != "1.2.3" {
		t.Fatal("version is not bounded by the envelope's limit")
	}
}

func TestUserSID(t *testing.T) {
	for name, want := range map[string]bool{
		adaSID:                  true,
		graceSID:                true,
		"S-1-11-96-3623454863":  true,
		adaSID + "_Classes":     false,
		"S-1-5-18":              false,
		"S-1-5-19":              false,
		".DEFAULT":              false,
		"S-1-5-80-123456-12345": false,
	} {
		if got := userSID(name); got != want {
			t.Errorf("userSID(%q) = %v, want %v", name, got, want)
		}
	}
}

// recordingPipeline mints each fact through core.BuildEnvelope, as core.Pipeline does.
type recordingPipeline struct {
	mu  sync.Mutex
	raw [][]byte
}

func (p *recordingPipeline) Record(_ context.Context, f core.Fact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := core.Identity{TenantID: "tenant-1", DeviceID: testDevice, UserRef: "u_console"}
	if f.Person != nil {
		id.UserRef, id.SubjectName = f.Person.UserRef, f.Person.SubjectName
	}
	raw, err := core.BuildEnvelope(core.EnvelopeInput{
		Identity:          id,
		EventID:           fmt.Sprintf("00000000-0000-4000-8000-%012d", len(p.raw)+1),
		Kind:              f.Kind,
		Route:             f.Route,
		Mode:              protocol.ModeM0,
		ToolFingerprint:   f.ToolFingerprint,
		OccurredAt:        f.OccurredAt,
		MonotonicOffsetMS: f.MonotonicOffsetMS,
		DedupKey:          f.DedupKey,
		FactFields:        f.FactFields,
	})
	if err != nil {
		return err
	}
	p.raw = append(p.raw, raw)
	return nil
}

// A scan through the real emitter mints one valid discovery envelope per app, attributed as found,
// and a second scan the same day emits nothing more.
func TestScanEmitsDiscoveryEnvelopes(t *testing.T) {
	dir, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b := testCatalog()
	pipe := &recordingPipeline{}
	em, err := discovery.New(discovery.Config{Pipeline: pipe, Dir: dir, Clock: clock, Bundles: func() *policy.Bundle { return b }, DeviceID: testDevice})
	if err != nil {
		t.Fatal(err)
	}
	ticks := newTicks()
	p := New(Config{Scanners: []Scanner{NewAppScanner(machine(), person)}, Emitter: em, Bundles: func() *policy.Bundle { return b }, Clock: clock, After: ticks.after})
	if err := p.ApplyPolicy(*b); err != nil {
		t.Fatal(err)
	}
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
		Direction       string `json:"direction"`
	}
	got := map[string]envelope{}
	for _, raw := range pipe.raw {
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		if e.Kind != "discovery" || e.Route != "inv.scan" || e.DiscoveryType != "app_installed" || e.DetectionBasis != "installed_scan" {
			t.Errorf("envelope %+v is not an installed-app discovery on inv.scan", e)
		}
		got[e.ToolFingerprint] = e
	}
	for fp, want := range map[string][2]string{
		"app:chatgpt_desktop": {"1.2025.112.0", "u_4444"},
		"app:claude_desktop":  {"0.14.10", "u_4444"},
		"app:jetbrains":       {"262.8653.112", "unattributed"},
		"app:vscode":          {"1.140.0", "unattributed"},
	} {
		if e, ok := got[fp]; !ok || e.AppVersion != want[0] || e.UserRef != want[1] {
			t.Errorf("%s: %+v, want version %s for %s", fp, e, want[0], want[1])
		}
	}
	if len(pipe.raw) != 4 {
		t.Fatalf("%d envelopes, want 4", len(pipe.raw))
	}
	if c := p.Counters().Cumulative(); c[protocol.CounterEmitted] != 4 || c[protocol.CounterErrors] != 0 {
		t.Fatalf("counters %v, want 4 emitted", c)
	}

	ticks.tick(t)
	ticks.waitFor(t, 2)
	if len(pipe.raw) != 4 {
		t.Fatalf("a second scan the same day emitted %d more", len(pipe.raw)-4)
	}
}
