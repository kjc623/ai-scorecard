package toolconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// fakeRegistry is HKEY_LOCAL_MACHINE in memory, value names case-insensitive as Windows has them.
// A set or remove notifies the key's watches, the agent's own writes included, as Windows does.
type fakeRegistry struct {
	mu         sync.Mutex
	values     map[string]regValue // key + "\\" + lower-case name
	broadcasts int
	failSet    bool
	watches    map[string]map[*func()]bool
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{values: map[string]regValue{}, watches: map[string]map[*func()]bool{}}
}

func regPath(key, name string) string { return key + `\` + strings.ToLower(name) }

func (r *fakeRegistry) get(key, name string) (regValue, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.values[regPath(key, name)]
	return v, ok, nil
}

func (r *fakeRegistry) set(key, name string, v regValue) error {
	r.mu.Lock()
	if r.failSet {
		r.mu.Unlock()
		return errors.New("access is denied")
	}
	r.values[regPath(key, name)] = v
	r.mu.Unlock()
	r.notify(key)
	return nil
}

func (r *fakeRegistry) remove(key, name string) error {
	r.mu.Lock()
	delete(r.values, regPath(key, name))
	r.mu.Unlock()
	r.notify(key)
	return nil
}

func (r *fakeRegistry) environmentChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broadcasts++
}

func (r *fakeRegistry) watch(key string, stop <-chan struct{}, changed func()) {
	r.mu.Lock()
	if r.watches[key] == nil {
		r.watches[key] = map[*func()]bool{}
	}
	r.watches[key][&changed] = true
	r.mu.Unlock()
	<-stop
	r.mu.Lock()
	delete(r.watches[key], &changed)
	r.mu.Unlock()
}

// watching is the number of watches on key.
func (r *fakeRegistry) watching(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.watches[key])
}

func (r *fakeRegistry) notify(key string) {
	r.mu.Lock()
	var fns []func()
	for f := range r.watches[key] {
		fns = append(fns, *f)
	}
	r.mu.Unlock()
	for _, f := range fns {
		f()
	}
}

// value is a value's content, or false when it is absent.
func (r *fakeRegistry) value(t *testing.T, key, name string) (regValue, bool) {
	t.Helper()
	v, ok, _ := r.get(key, name)
	return v, ok
}

// snapshot copies every value.
func (r *fakeRegistry) snapshot() map[string]regValue {
	out := make(map[string]regValue, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out
}

func sameValues(a, b map[string]regValue) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || !v.equal(w) {
			return false
		}
	}
	return true
}

// everywhere is Copilot in VS Code at a release that reads the policies, and the CLI.
var everywhere = CopilotInstall{Extension: true, Policy: true, CLI: true}

// newCopilotTestWriter is a Copilot writer over a fake registry and a state directory in the test's
// temporary directory, with Copilot installed as *in says.
func newCopilotTestWriter(t *testing.T, in *CopilotInstall) (*Copilot, *fakeRegistry, string) {
	t.Helper()
	root := t.TempDir()
	dir, err := state.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	reg := newFakeRegistry()
	w := NewCopilotWriter(dir, func() CopilotInstall { return *in })
	w.reg = reg
	return w, reg, dir.Path(backupDir, CopilotTool, "original")
}

// copilotDesired is the OTel export with VS Code's and the CLI's prompt logging as given.
func copilotDesired(vsCode, cli bool) Desired {
	return Desired{OTel: true, HTTPListen: "127.0.0.1:47318", Token: testToken, LogPrompts: vsCode, LogCLIPrompts: cli}
}

// The values the agent writes for copilotDesired.
const (
	wantEndpoint   = "http://127.0.0.1:47318"
	wantPolicyHdrs = `{"Authorization":"Bearer ` + testToken + `"}`
	wantEnvHeaders = "Authorization=Bearer " + testToken
)

func checkValue(t *testing.T, reg *fakeRegistry, key, name string, want regValue) {
	t.Helper()
	got, ok := reg.value(t, key, name)
	if !ok {
		t.Fatalf(`HKLM\%s\%s is absent, want %+v`, key, name, want)
	}
	if !got.equal(want) {
		t.Fatalf(`HKLM\%s\%s = %+v, want %+v`, key, name, got, want)
	}
}

func checkAbsent(t *testing.T, reg *fakeRegistry, key, name string) {
	t.Helper()
	if v, ok := reg.value(t, key, name); ok {
		t.Fatalf(`HKLM\%s\%s = %+v, want it absent`, key, name, v)
	}
}

// checkCopilotValues checks every owned value for d.
func checkCopilotValues(t *testing.T, reg *fakeRegistry, vsCodeCapture uint32, cliCapture string) {
	t.Helper()
	checkValue(t, reg, vsCodePolicyKey, "CopilotOtelEnabled", dwordValue(1))
	checkValue(t, reg, vsCodePolicyKey, "CopilotOtelEndpoint", stringValue(wantEndpoint))
	checkValue(t, reg, vsCodePolicyKey, "CopilotOtelHeaders", stringValue(wantPolicyHdrs))
	checkValue(t, reg, vsCodePolicyKey, "CopilotOtelCaptureContent", dwordValue(vsCodeCapture))
	checkValue(t, reg, machineEnvKey, "COPILOT_OTEL_ENABLED", stringValue("true"))
	checkValue(t, reg, machineEnvKey, "OTEL_EXPORTER_OTLP_ENDPOINT", stringValue(wantEndpoint))
	checkValue(t, reg, machineEnvKey, "OTEL_EXPORTER_OTLP_HEADERS", stringValue(wantEnvHeaders))
	checkValue(t, reg, machineEnvKey, "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", stringValue(cliCapture))
}

func TestCopilotInstallFrom(t *testing.T) {
	ext := func(host string) discovery.Record {
		return discovery.Record{Type: protocol.DiscoveryTypeIDEExtension, AppKey: "github_copilot", Version: "0.70.0", HostApp: host}
	}
	vsCode := func(version string) discovery.Record { return discovery.Record{AppKey: "vscode", Version: version} }
	cli := discovery.Record{AppKey: "copilot_cli", Version: "1.0.94"}
	cases := []struct {
		name string
		recs []discovery.Record
		want CopilotInstall
	}{
		{"nothing", nil, CopilotInstall{}},
		{"VS Code without Copilot", []discovery.Record{vsCode("1.140.0")}, CopilotInstall{}},
		{"the CLI only", []discovery.Record{cli}, CopilotInstall{CLI: true}},
		{"in VS Code", []discovery.Record{ext("app:vscode"), vsCode("1.140.2"), cli}, everywhere},
		{"in VS Code of an unread version", []discovery.Record{ext("app:vscode")}, CopilotInstall{Extension: true, Policy: true}},
		{"in VS Code 1.127", []discovery.Record{ext("app:vscode"), vsCode("1.127.0")}, CopilotInstall{Extension: true, Policy: true}},
		{"in VS Code 1.126", []discovery.Record{ext("app:vscode"), vsCode("1.126.3")}, CopilotInstall{Extension: true}},
		{"in Cursor", []discovery.Record{ext("app:cursor")}, CopilotInstall{Extension: true}},
		{"in VS Code and Windsurf", []discovery.Record{ext("app:vscode"), ext("app:windsurf"), cli}, CopilotInstall{Extension: true, CLI: true}},
	}
	for _, c := range cases {
		if got := CopilotInstallFrom(c.recs); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Apply writes VS Code's four policy values and the CLI's four machine variables, touches no other
// value in either key, and broadcasts the environment change once; applying the same again writes
// and broadcasts nothing.
func TestCopilotApplyWritesBothParts(t *testing.T) {
	in := everywhere
	w, reg, _ := newCopilotTestWriter(t, &in)
	reg.values[regPath(vsCodePolicyKey, "UpdateMode")] = stringValue("manual")
	reg.values[regPath(machineEnvKey, "Path")] = regValue{Kind: regExpandSZ, String: `%SystemRoot%\system32`}
	if !w.Installed() {
		t.Fatal("not installed")
	}
	if err := w.Apply(copilotDesired(true, true)); err != nil {
		t.Fatal(err)
	}
	checkCopilotValues(t, reg, 1, "true")
	checkValue(t, reg, vsCodePolicyKey, "UpdateMode", stringValue("manual"))
	checkValue(t, reg, machineEnvKey, "Path", regValue{Kind: regExpandSZ, String: `%SystemRoot%\system32`})
	if len(reg.values) != 10 {
		t.Fatalf("%d values, want the customer's 2 and the agent's 8", len(reg.values))
	}
	if reg.broadcasts != 1 {
		t.Fatalf("%d environment broadcasts, want 1", reg.broadcasts)
	}
	if ok, err := w.Holds(copilotDesired(true, true)); err != nil || !ok {
		t.Fatalf("Holds = %v, %v", ok, err)
	}
	if ok, _ := w.Holds(copilotDesired(true, false)); ok {
		t.Fatal("Holds with the CLI's content capture differing")
	}

	before := reg.snapshot()
	if err := w.Apply(copilotDesired(true, true)); err != nil {
		t.Fatal(err)
	}
	if !sameValues(before, reg.values) || reg.broadcasts != 1 {
		t.Fatalf("a second apply changed the registry or broadcast again (%d)", reg.broadcasts)
	}
}

// Only the parts that are installed are written; one installed later is written at the next apply,
// and a change to the VS Code policies alone does not broadcast.
func TestCopilotApplyWritesInstalledPartsOnly(t *testing.T) {
	in := CopilotInstall{CLI: true}
	w, reg, _ := newCopilotTestWriter(t, &in)
	w.Installed()
	if err := w.Apply(copilotDesired(true, true)); err != nil {
		t.Fatal(err)
	}
	for _, name := range copilotVSCode.names {
		checkAbsent(t, reg, vsCodePolicyKey, name)
	}
	checkValue(t, reg, machineEnvKey, "COPILOT_OTEL_ENABLED", stringValue("true"))

	in = everywhere
	w.Installed()
	if ok, _ := w.Holds(copilotDesired(true, true)); ok {
		t.Fatal("Holds while the newly installed extension's policies are not written")
	}
	broadcasts := reg.broadcasts
	if err := w.Apply(copilotDesired(true, true)); err != nil {
		t.Fatal(err)
	}
	checkCopilotValues(t, reg, 1, "true")
	if reg.broadcasts != broadcasts {
		t.Fatal("writing VS Code's policies broadcast an environment change")
	}
}

// Each value the agent replaces is backed up once, before its first write, and Remove puts back
// what was there, with its kind, deletes what was not, and drops the backup. A later apply with a
// new token does not overwrite the backup.
func TestCopilotBackupAndRestore(t *testing.T) {
	in := everywhere
	w, reg, backupPath := newCopilotTestWriter(t, &in)
	reg.values[regPath(vsCodePolicyKey, "CopilotOtelEnabled")] = dwordValue(0)
	reg.values[regPath(vsCodePolicyKey, "CopilotOtelEndpoint")] = stringValue("https://otel.example.com")
	reg.values[regPath(machineEnvKey, "otel_exporter_otlp_endpoint")] = regValue{Kind: regExpandSZ, String: "%COLLECTOR%"}
	reg.values[regPath(machineEnvKey, "Path")] = regValue{Kind: regExpandSZ, String: `%SystemRoot%\system32`}
	customer := reg.snapshot()

	w.Installed()
	if err := w.Apply(copilotDesired(false, false)); err != nil {
		t.Fatal(err)
	}
	checkCopilotValues(t, reg, 0, "false")
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("no backup: %v", err)
	}
	newToken := copilotDesired(false, false)
	newToken.Token = "0123"
	if err := w.Apply(newToken); err != nil {
		t.Fatal(err)
	}
	checkValue(t, reg, machineEnvKey, "OTEL_EXPORTER_OTLP_HEADERS", stringValue("Authorization=Bearer 0123"))

	broadcasts := reg.broadcasts
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if !sameValues(customer, reg.values) {
		t.Fatalf("after Remove the registry is %+v, want the customer's %+v", reg.values, customer)
	}
	if reg.broadcasts != broadcasts+1 {
		t.Fatal("restoring the machine environment did not broadcast")
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("the backup is still there after Remove: %v", err)
	}
	if err := w.Remove(); err != nil {
		t.Fatalf("Remove without a backup: %v", err)
	}
	if !sameValues(customer, reg.values) {
		t.Fatal("a second Remove changed the registry")
	}
}

// A value the agent writes only once a part is installed is backed up then, as it is at that time.
func TestCopilotBackupTakesEachValueBeforeItsFirstWrite(t *testing.T) {
	in := CopilotInstall{CLI: true}
	w, reg, _ := newCopilotTestWriter(t, &in)
	w.Installed()
	if err := w.Apply(copilotDesired(true, true)); err != nil {
		t.Fatal(err)
	}
	reg.values[regPath(vsCodePolicyKey, "CopilotOtelHeaders")] = stringValue(`{"x-team":"a"}`)
	in = everywhere
	w.Installed()
	if err := w.Apply(copilotDesired(true, true)); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if want := map[string]regValue{regPath(vsCodePolicyKey, "CopilotOtelHeaders"): stringValue(`{"x-team":"a"}`)}; !sameValues(want, reg.values) {
		t.Fatalf("after Remove the registry is %+v, want only the customer's headers policy", reg.values)
	}
}

// A desired configuration without OTel is a Remove.
func TestCopilotApplyWithoutOTelRemoves(t *testing.T) {
	in := everywhere
	w, reg, _ := newCopilotTestWriter(t, &in)
	w.Installed()
	if err := w.Apply(copilotDesired(true, true)); err != nil {
		t.Fatal(err)
	}
	if err := w.Apply(Desired{}); err != nil {
		t.Fatal(err)
	}
	if len(reg.values) != 0 {
		t.Fatalf("values left: %+v", reg.values)
	}
}

// copilotBundle switches Copilot's OTel export on, at a tenant default mode.
func copilotBundle(mode protocol.CollectionMode) policy.Bundle {
	return policy.Bundle{
		Version:       "1",
		TenantDefault: mode,
		Endpoint: policy.EndpointPolicy{
			OTel:  policy.EndpointOTel{Enabled: true, HTTPListen: "127.0.0.1:47318", GRPCListen: "127.0.0.1:47317"},
			Tools: map[string]policy.EndpointTool{"copilot": {OTel: true}},
		},
	}
}

// supportedCopilot is Copilot on a platform the agent writes its configuration on.
var supportedCopilot = func() tool { t := copilotTool; t.supported = true; return t }()

var copilotConfig = Config{Token: func() string { return testToken }}

// tool_config_copilot follows endpoint.otel.enabled and endpoint.tools.copilot.otel; the hooks
// switches do nothing for it.
func TestCopilotProviderNameAndSwitch(t *testing.T) {
	p := NewCopilot(&Copilot{}, Config{})
	if p.Name() != protocol.CollectorToolConfigCopilot {
		t.Fatalf("Name = %s", p.Name())
	}
	b := copilotBundle(protocol.ModeM1)
	if !p.Enabled(&b) {
		t.Fatal("not enabled with the receiver and Copilot's OTel on")
	}
	if p.Enabled(nil) {
		t.Fatal("enabled with no bundle in force")
	}
	off := copilotBundle(protocol.ModeM1)
	off.Endpoint.OTel.Enabled = false
	if p.Enabled(&off) {
		t.Fatal("enabled with the receiver off")
	}
	hooks := copilotBundle(protocol.ModeM1)
	hooks.Endpoint.OTel.Enabled = false
	hooks.Endpoint.Hooks.Enabled = true
	hooks.Endpoint.Tools["copilot"] = policy.EndpointTool{Hooks: true}
	if p.Enabled(&hooks) {
		t.Fatal("enabled by the hooks switches")
	}
}

// Content capture follows the resolved mode for each app: on at m1 and above, off at m0, and VS
// Code's and the CLI's apart when their tool modes differ. A mode change rewrites the values.
func TestCopilotProviderModeSwitching(t *testing.T) {
	in := everywhere
	w, reg, _ := newCopilotTestWriter(t, &in)
	p := newProvider(supportedCopilot, w, copilotConfig)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyPolicy(copilotBundle(protocol.ModeM0)); err != nil {
		t.Fatal(err)
	}
	checkCopilotValues(t, reg, 0, "false")

	if err := p.ApplyPolicy(copilotBundle(protocol.ModeM2)); err != nil {
		t.Fatal(err)
	}
	checkCopilotValues(t, reg, 1, "true")

	split := copilotBundle(protocol.ModeM1)
	split.ToolModes = map[string]protocol.CollectionMode{CopilotFingerprint: protocol.ModeM0}
	if err := p.ApplyPolicy(split); err != nil {
		t.Fatal(err)
	}
	checkCopilotValues(t, reg, 0, "true")

	split.ToolModes = map[string]protocol.CollectionMode{CopilotCLIFingerprint: protocol.ModeM0}
	if err := p.ApplyPolicy(split); err != nil {
		t.Fatal(err)
	}
	checkCopilotValues(t, reg, 1, "false")
	if h := p.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("row = %s/%s, want healthy", h.State, h.Detail)
	}
}

// Under the registry, switching Copilot's OTel on writes the values and reports healthy; switching
// it off restores the customer's values, without a restart.
func TestCopilotProviderFollowsThePolicyToggle(t *testing.T) {
	in := everywhere
	w, fake, _ := newCopilotTestWriter(t, &in)
	fake.values[regPath(machineEnvKey, "OTEL_EXPORTER_OTLP_ENDPOINT")] = stringValue("http://localhost:4318")
	customer := fake.snapshot()
	reg := core.NewRegistry(nil, nil)
	if err := reg.Add(newProvider(supportedCopilot, w, copilotConfig)); err != nil {
		t.Fatal(err)
	}
	reg.StartCollectors(context.Background())
	t.Cleanup(func() { reg.StopAll(context.Background()) })

	reg.ApplyPolicy(copilotBundle(protocol.ModeM1))
	checkCopilotValues(t, fake, 1, "true")
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCopilot); h.State != protocol.StateHealthy {
		t.Fatalf("row = %s/%s, want healthy", h.State, h.Detail)
	}
	off := copilotBundle(protocol.ModeM1)
	off.Endpoint.Tools["copilot"] = policy.EndpointTool{}
	reg.ApplyPolicy(off)
	if !sameValues(customer, fake.values) {
		t.Fatalf("after the switch went off the registry is %+v, want the customer's", fake.values)
	}
	if h, _ := reg.HealthFor(protocol.CollectorToolConfigCopilot); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
		t.Fatalf("row = %s/%s, want absent/disabled_by_policy", h.State, h.Detail)
	}
}

// The row's states: absent/tool_not_installed without Copilot; degraded/tool_version_unsupported
// while the extension is in an IDE without the policies, even with the CLI configured;
// degraded/config_write_failed when a write fails or a value is lost; healthy otherwise.
func TestCopilotProviderHealth(t *testing.T) {
	start := func(t *testing.T, in *CopilotInstall) (*Provider, *fakeRegistry) {
		t.Helper()
		w, reg, _ := newCopilotTestWriter(t, in)
		p := newProvider(supportedCopilot, w, copilotConfig)
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return p, reg
	}
	check := func(t *testing.T, p *Provider, state protocol.CollectorState, detail protocol.Detail) {
		t.Helper()
		if h := p.Health(); h.State != state || h.Detail != detail {
			t.Fatalf("row = %s/%s, want %s/%s", h.State, h.Detail, state, detail)
		}
	}

	t.Run("not installed", func(t *testing.T) {
		in := CopilotInstall{}
		p, reg := start(t, &in)
		_ = p.ApplyPolicy(copilotBundle(protocol.ModeM1))
		check(t, p, protocol.StateAbsent, protocol.DetailToolNotInstalled)
		if len(reg.values) != 0 {
			t.Fatal("wrote values with Copilot not installed")
		}
	})
	t.Run("the extension where no policy reaches it", func(t *testing.T) {
		in := CopilotInstall{Extension: true, CLI: true}
		p, reg := start(t, &in)
		if err := p.ApplyPolicy(copilotBundle(protocol.ModeM1)); err != nil {
			t.Fatal(err)
		}
		checkCopilotValues(t, reg, 1, "true")
		check(t, p, protocol.StateDegraded, protocol.DetailToolVersionUnsupported)
	})
	t.Run("the CLI alone", func(t *testing.T) {
		in := CopilotInstall{CLI: true}
		p, _ := start(t, &in)
		_ = p.ApplyPolicy(copilotBundle(protocol.ModeM1))
		check(t, p, protocol.StateHealthy, protocol.DetailNone)
	})
	t.Run("everywhere", func(t *testing.T) {
		in := everywhere
		p, reg := start(t, &in)
		_ = p.ApplyPolicy(copilotBundle(protocol.ModeM1))
		check(t, p, protocol.StateHealthy, protocol.DetailNone)
		delete(reg.values, regPath(machineEnvKey, "OTEL_EXPORTER_OTLP_HEADERS"))
		check(t, p, protocol.StateDegraded, protocol.DetailConfigWriteFailed)
	})
	t.Run("a failed write", func(t *testing.T) {
		in := everywhere
		p, reg := start(t, &in)
		reg.failSet = true
		err := p.ApplyPolicy(copilotBundle(protocol.ModeM1))
		if err == nil {
			t.Fatal("no error from a failed write")
		}
		if strings.Contains(err.Error(), testToken) {
			t.Fatalf("the error carries the token: %v", err)
		}
		check(t, p, protocol.StateDegraded, protocol.DetailConfigWriteFailed)
	})
}
