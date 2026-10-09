package toolconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/shadow-ai-capture/device/capture-core/discovery"
	"github.com/shadow-ai-capture/device/capture-core/state"
	"github.com/shadow-ai-capture/device/protocol"
)

// CopilotTool is GitHub Copilot's endpoint.tools key, and its backup folder's name.
const CopilotTool = "copilot"

// Copilot's catalog fingerprints: the Copilot extension in VS Code, and the Copilot CLI.
const (
	CopilotFingerprint    = "app:github_copilot"
	CopilotCLIFingerprint = "app:copilot_cli"
)

// The registry keys under HKEY_LOCAL_MACHINE the agent writes values in: VS Code's machine policies,
// which lock the Copilot extension's OTel settings, and the machine environment, which the Copilot
// CLI reads its OTel settings from.
const (
	vsCodePolicyKey = `SOFTWARE\Policies\Microsoft\VSCode`
	machineEnvKey   = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
)

// copilotPolicyVersion is the first VS Code release that reads the Copilot OTel policies.
var copilotPolicyVersion = [2]int{1, 127}

// The registry value kinds the agent writes or keeps.
const (
	regSZ       uint32 = 1
	regExpandSZ uint32 = 2
	regBinary   uint32 = 3
	regDWORD    uint32 = 4
	regMultiSZ  uint32 = 7
	regQWORD    uint32 = 11
)

// regValue is one registry value: its kind and its data, in the field for that kind. A kind with no
// field of its own keeps its raw bytes in Binary.
type regValue struct {
	Kind    uint32   `json:"kind"`
	String  string   `json:"string,omitempty"`
	Strings []string `json:"strings,omitempty"`
	Integer uint64   `json:"integer,omitempty"`
	Binary  []byte   `json:"binary,omitempty"`
}

func stringValue(s string) regValue { return regValue{Kind: regSZ, String: s} }

func dwordValue(v uint32) regValue { return regValue{Kind: regDWORD, Integer: uint64(v)} }

func (v regValue) equal(w regValue) bool {
	return v.Kind == w.Kind && v.String == w.String && slices.Equal(v.Strings, w.Strings) &&
		v.Integer == w.Integer && slices.Equal(v.Binary, w.Binary)
}

// machineRegistry is HKEY_LOCAL_MACHINE's 64-bit view. Tests replace it.
type machineRegistry interface {
	// get returns a value, or false when the key or the value does not exist.
	get(key, name string) (regValue, bool, error)
	// set writes a value, creating the key when it does not exist.
	set(key, name string, v regValue) error
	// remove deletes a value; a missing key or value is not an error.
	remove(key, name string) error
	// environmentChanged broadcasts WM_SETTINGCHANGE for "Environment", so programs that rebuild
	// their environment from the registry, Explorer among them, read the new machine environment.
	environmentChanged()
	// watch calls changed whenever a value in key is set or deleted, or the key itself is created
	// or deleted, until stop is closed.
	watch(key string, stop <-chan struct{}, changed func())
}

// copilotSurface is one part of Copilot and the registry values the agent owns for it.
type copilotSurface struct {
	key   string
	names []string
	// values is what the agent writes for d, in names' order.
	values func(d Desired) ([]regValue, error)
}

// copilotVSCode is the Copilot extension's OTel settings, locked by VS Code's machine policies. With
// any of them set, the extension takes its whole OTel configuration from the policies and ignores the
// user's settings.
var copilotVSCode = copilotSurface{
	key:   vsCodePolicyKey,
	names: []string{"CopilotOtelEnabled", "CopilotOtelEndpoint", "CopilotOtelHeaders", "CopilotOtelCaptureContent"},
	values: func(d Desired) ([]regValue, error) {
		headers, err := json.Marshal(map[string]string{"Authorization": "Bearer " + d.Token})
		if err != nil {
			return nil, err
		}
		capture := uint32(0)
		if d.LogPrompts {
			capture = 1
		}
		return []regValue{
			dwordValue(1),
			stringValue("http://" + d.HTTPListen),
			stringValue(string(headers)),
			dwordValue(capture),
		}, nil
	},
}

// copilotCLI is the Copilot CLI's OTel variables in the machine environment. The endpoint, header
// and content variables are the standard OTel ones, which the CLI reads under no name of its own.
var copilotCLI = copilotSurface{
	key:   machineEnvKey,
	names: []string{"COPILOT_OTEL_ENABLED", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"},
	values: func(d Desired) ([]regValue, error) {
		return []regValue{
			stringValue("true"),
			stringValue("http://" + d.HTTPListen),
			stringValue("Authorization=Bearer " + d.Token),
			stringValue(strconv.FormatBool(d.LogCLIPrompts)),
		}, nil
	},
}

// CopilotInstall is where the inventory finds Copilot.
type CopilotInstall struct {
	// Extension: the Copilot extension is installed in an IDE.
	Extension bool
	// Policy: every IDE the extension is installed in is VS Code at a release that reads the Copilot
	// OTel policies.
	Policy bool
	// CLI: the Copilot CLI is installed in a user profile.
	CLI bool
}

// CopilotInstallFrom reads where Copilot is installed from the inventory's records: the IDE
// extension scan's github_copilot records, with the IDE each is in, the installed-app scan's VS Code
// records, with their versions, and the CLI scan's copilot_cli records. A VS Code whose version
// cannot be read is taken to read the policies.
func CopilotInstallFrom(recs []discovery.Record) CopilotInstall {
	in := CopilotInstall{Policy: true}
	for _, r := range recs {
		switch r.AppKey {
		case "github_copilot":
			in.Extension = true
			if r.HostApp != "app:vscode" {
				in.Policy = false
			}
		case "vscode":
			if v, ok := majorMinor(r.Version); ok && (v[0] < copilotPolicyVersion[0] || (v[0] == copilotPolicyVersion[0] && v[1] < copilotPolicyVersion[1])) {
				in.Policy = false
			}
		case "copilot_cli":
			in.CLI = true
		}
	}
	in.Policy = in.Policy && in.Extension
	return in
}

// majorMinor reads the first two numbers of a version such as 1.140.2.
func majorMinor(v string) ([2]int, bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return [2]int{}, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return [2]int{}, false
	}
	return [2]int{major, minor}, true
}

// savedValue is an owned value as it was before the agent first wrote it.
type savedValue struct {
	Key     string   `json:"key"`
	Name    string   `json:"name"`
	Present bool     `json:"present"`
	Value   regValue `json:"value,omitzero"`
}

// Copilot is GitHub Copilot's machine-wide configuration: VS Code's policies for the extension's OTel
// settings, written while the extension is installed in an IDE, and the CLI's OTel variables in the
// machine environment, written while the CLI is installed. The agent owns those values and no others
// in either key. Before it first writes a value it backs the value up (or records that there was
// none); Remove puts every backed-up value back. A part that is not installed is left as it is.
type Copilot struct {
	reg     machineRegistry
	backup  backup
	install func() CopilotInstall

	mu   sync.Mutex
	last CopilotInstall
}

// NewCopilotWriter returns the writer for this machine's registry. install reports where Copilot is
// installed; nil reports nowhere. Its backup is kept in dir.
func NewCopilotWriter(dir state.Dir, install func() CopilotInstall) *Copilot {
	if install == nil {
		install = func() CopilotInstall { return CopilotInstall{} }
	}
	return &Copilot{reg: systemMachineRegistry(), backup: backupFor(dir, CopilotTool), install: install}
}

// Path implements Writer.
func (c *Copilot) Path() string {
	return `HKLM\` + vsCodePolicyKey + ` and HKLM\` + machineEnvKey
}

// watchRegistry implements watchedRegistry: both keys the agent writes values in.
func (c *Copilot) watchRegistry(stop <-chan struct{}, changed func()) {
	var wg sync.WaitGroup
	for _, key := range []string{vsCodePolicyKey, machineEnvKey} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.reg.watch(key, stop, changed)
		}()
	}
	wg.Wait()
}

// Installed implements Writer: the extension or the CLI is installed. It records where, for Apply,
// Holds and Unenforced.
func (c *Copilot) Installed() bool {
	in := c.install()
	c.mu.Lock()
	c.last = in
	c.mu.Unlock()
	return in.Extension || in.CLI
}

// Unenforced implements partialWriter: the extension is installed in an IDE that does not read the
// policies.
func (c *Copilot) Unenforced() bool {
	in := c.installed()
	return in.Extension && !in.Policy
}

func (c *Copilot) installed() CopilotInstall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// surfaces is the parts installed at the last Installed.
func (c *Copilot) surfaces() []copilotSurface {
	in := c.installed()
	var out []copilotSurface
	if in.Extension {
		out = append(out, copilotVSCode)
	}
	if in.CLI {
		out = append(out, copilotCLI)
	}
	return out
}

// loadSaved returns the backed-up values, none when there is no backup.
func (c *Copilot) loadSaved() ([]savedValue, error) {
	o, ok, err := c.backup.load()
	if err != nil || !ok {
		return nil, err
	}
	var saved []savedValue
	if err := json.Unmarshal(o.Content, &saved); err != nil {
		return nil, fmt.Errorf("toolconfig: the backup %s is unreadable: %w", c.backup.path, err)
	}
	return saved, nil
}

// storeSaved replaces the backup with saved.
func (c *Copilot) storeSaved(saved []savedValue) error {
	content, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(original{Present: true, Content: content})
	if err != nil {
		return err
	}
	if err := state.MkdirProtected(filepath.Dir(c.backup.path)); err != nil {
		return fmt.Errorf("toolconfig: creating %s: %w", filepath.Dir(c.backup.path), err)
	}
	if err := state.WriteFile(c.backup.path, raw); err != nil {
		return fmt.Errorf("toolconfig: writing the backup %s: %w", c.backup.path, err)
	}
	return nil
}

func findSaved(saved []savedValue, key, name string) (savedValue, bool) {
	for _, s := range saved {
		if s.Key == key && strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return savedValue{}, false
}

// Apply implements Writer: each installed part's values are set to what d asks for. A value the
// agent has not written before is backed up first.
func (c *Copilot) Apply(d Desired) error {
	if !d.OTel {
		return c.Remove()
	}
	saved, err := c.loadSaved()
	if err != nil {
		return err
	}
	envChanged := false
	defer func() {
		if envChanged {
			c.reg.environmentChanged()
		}
	}()
	for _, s := range c.surfaces() {
		values, err := s.values(d)
		if err != nil {
			return err
		}
		for i, name := range s.names {
			cur, has, err := c.reg.get(s.key, name)
			if err != nil {
				return fmt.Errorf("toolconfig: reading HKLM\\%s\\%s: %w", s.key, name, err)
			}
			if _, ok := findSaved(saved, s.key, name); !ok {
				saved = append(saved, savedValue{Key: s.key, Name: name, Present: has, Value: cur})
				if err := c.storeSaved(saved); err != nil {
					return err
				}
			}
			if has && cur.equal(values[i]) {
				continue
			}
			if err := c.reg.set(s.key, name, values[i]); err != nil {
				return fmt.Errorf("toolconfig: writing HKLM\\%s\\%s: %w", s.key, name, err)
			}
			envChanged = envChanged || s.key == machineEnvKey
		}
	}
	return nil
}

// Holds implements Writer: each installed part's values are what d asks for.
func (c *Copilot) Holds(d Desired) (bool, error) {
	if !d.OTel {
		return true, nil
	}
	for _, s := range c.surfaces() {
		values, err := s.values(d)
		if err != nil {
			return false, err
		}
		for i, name := range s.names {
			cur, has, err := c.reg.get(s.key, name)
			if err != nil || !has || !cur.equal(values[i]) {
				return false, err
			}
		}
	}
	return true, nil
}

// Remove implements Writer. Each backed-up value is put back, or deleted when there was none, and
// the backup is dropped once all are.
func (c *Copilot) Remove() error {
	saved, err := c.loadSaved()
	if err != nil || saved == nil {
		return err
	}
	envChanged := false
	defer func() {
		if envChanged {
			c.reg.environmentChanged()
		}
	}()
	var errs []error
	for _, s := range saved {
		cur, has, err := c.reg.get(s.Key, s.Name)
		if err != nil {
			errs = append(errs, fmt.Errorf("toolconfig: reading HKLM\\%s\\%s: %w", s.Key, s.Name, err))
			continue
		}
		switch {
		case s.Present && (!has || !cur.equal(s.Value)):
			err = c.reg.set(s.Key, s.Name, s.Value)
		case !s.Present && has:
			err = c.reg.remove(s.Key, s.Name)
		default:
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("toolconfig: restoring HKLM\\%s\\%s: %w", s.Key, s.Name, err))
			continue
		}
		envChanged = envChanged || s.Key == machineEnvKey
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return c.backup.drop()
}

// copilotTool describes Copilot to its provider: OTel only, with the CLI's prompt logging following
// the CLI's own collection mode. 1.0.4 is the first CLI release with an OTel export; the IDE's
// version is checked as the extension's part (Unenforced).
var copilotTool = tool{
	key:            CopilotTool,
	collector:      protocol.CollectorToolConfigCopilot,
	fingerprint:    CopilotFingerprint,
	cliFingerprint: CopilotCLIFingerprint,
	supported:      copilotSupported,
	otel:           true,
	otelOnly:       true,
	versionApp:     "copilot_cli",
	minVersion:     "1.0.4",
}

// NewCopilot returns the tool_config_copilot collector over w.
func NewCopilot(w Writer, cfg Config) *Provider { return newProvider(copilotTool, w, cfg) }
