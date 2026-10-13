package appcapture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

var schemaPath = filepath.Join("..", "..", "..", "services", "database", "schema.sql")

// catalogue is the app catalogue seeded in schema.sql.
type catalogue struct {
	apps []policy.CatalogApp
	// toolApp is each tool_catalogue fingerprint's app_key, with every app's own app:<key> row.
	toolApp map[string]string
	// tlsHosts is the distinct hosts of the tls rows, the bundle's interception seed.
	tlsHosts []string
	// webApps is the chat assistants known by their inference domains alone, the apps a person uses
	// in the browser itself, which the extension's replay covers.
	webApps map[string]bool
}

var (
	appRow    = regexp.MustCompile(`\(\s*'([^']+)'\s*,\s*'[^']+'\s*,\s*'[^']*'\s*,\s*'([^']+)'\s*,\s*'[^']*'\s*\)`)
	signalRow = regexp.MustCompile(`\(\s*'([^']+)'\s*,\s*'([^']+)'\s*,\s*'([^']+)'\s*,\s*'([^']*)'\s*\)`)
	toolRow   = regexp.MustCompile(`\(\s*'([^']+)'\s*,\s*'[^']+'\s*,\s*'[^']*'\s*,\s*'([a-z]+)'\s*,\s*'([^']*)'\s*,\s*'([^']+)'\s*\)`)
)

// seed is the statement of source that starts with head, up to its semicolon.
func seed(t *testing.T, source, head string) string {
	t.Helper()
	start := strings.Index(source, head)
	if start == -1 {
		t.Fatalf("schema.sql has no %s", head)
	}
	return source[start : start+strings.IndexByte(source[start:], ';')]
}

func loadCatalogue(t *testing.T) catalogue {
	t.Helper()
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	c := catalogue{toolApp: map[string]string{}, webApps: map[string]bool{}}
	index := map[string]int{}
	for _, m := range appRow.FindAllStringSubmatch(seed(t, source, "INSERT INTO ref.app ("), -1) {
		index[m[1]] = len(c.apps)
		c.apps = append(c.apps, policy.CatalogApp{AppKey: m[1], Category: m[2]})
		c.toolApp["app:"+m[1]] = m[1]
	}
	for _, m := range signalRow.FindAllStringSubmatch(seed(t, source, "INSERT INTO ref.app_signal"), -1) {
		i, ok := index[m[1]]
		if !ok {
			t.Fatalf("ref.app_signal names %s, which ref.app does not", m[1])
		}
		c.apps[i].Signals = append(c.apps[i].Signals, policy.CatalogSignal{Platform: m[2], Kind: m[3], Value: m[4]})
	}
	for _, m := range toolRow.FindAllStringSubmatch(seed(t, source, "INSERT INTO ref.tool_catalogue"), -1) {
		c.toolApp[m[1]] = m[4]
		switch m[2] {
		case "tls":
			var ev struct{ Host string }
			if err := json.Unmarshal([]byte(m[3]), &ev); err != nil {
				t.Fatalf("tool_catalogue row %s: %v", m[1], err)
			}
			if h := strings.ToLower(ev.Host); h != "" && !slices.Contains(c.tlsHosts, h) {
				c.tlsHosts = append(c.tlsHosts, h)
			}
		}
	}
	for _, a := range c.apps {
		web := a.Category == "chat_assistant" && len(a.Signals) > 0
		for _, sig := range a.Signals {
			web = web && sig.Kind == policy.SignalInferenceDomain
		}
		c.webApps[a.AppKey] = web
	}
	if len(c.apps) == 0 || len(c.tlsHosts) == 0 {
		t.Fatal("schema.sql's catalogue seed did not parse")
	}
	slices.Sort(c.tlsHosts)
	return c
}

// app is the app a fingerprint is catalogued under, or "" for none.
func (c catalogue) app(fingerprint string) string { return c.toolApp[fingerprint] }

// bundle is the policy bundle control-api composes for a tenant at m3 that has switched on TLS
// inspection and Ollama's loopback broker, with every other endpoint setting at the default
// SQLEndpointSettings serves: the OTel receiver and the hook relay on, Claude Code's and Codex's
// telemetry and hooks, Copilot's telemetry and Cursor's hooks. The loopback broker holds Ollama's
// listen_port only, as app:ollama, because Ollama is the one runtime control-api brokers.
func (c catalogue) bundle(t *testing.T) *policy.Bundle {
	t.Helper()
	b := &policy.Bundle{
		Version:       "replay",
		TenantDefault: protocol.ModeM3,
		Interception:  policy.Interception{Enabled: true, SeedHosts: c.tlsHosts, Ports: []int{443}},
		Endpoint: policy.EndpointPolicy{
			OTel:  policy.EndpointOTel{Enabled: true},
			Hooks: policy.EndpointHooks{Enabled: true},
			Tools: map[string]policy.EndpointTool{
				"claude_code": {OTel: true, Hooks: true},
				"codex":       {OTel: true, Hooks: true},
				"copilot":     {OTel: true},
				"cursor":      {Hooks: true},
			},
		},
		Catalog: c.apps,
	}
	for _, a := range c.apps {
		if a.AppKey != "ollama" {
			continue
		}
		for _, s := range a.Signals {
			if s.Kind == policy.SignalListenPort {
				port, err := strconv.Atoi(s.Value)
				if err != nil {
					t.Fatalf("ollama's listen_port %q", s.Value)
				}
				b.Loopback.Ports = append(b.Loopback.Ports, policy.LoopbackPort{ToolFingerprint: "app:ollama", Port: port, Mode: protocol.ModeM3})
			}
		}
	}
	if len(b.Loopback.Ports) != 1 {
		t.Fatalf("ollama has %d listen_port signals, want 1", len(b.Loopback.Ports))
	}
	return b
}

// appByExe is the service's lookup of the catalog app a Windows process image belongs to: the
// first app whose windows_exe is the image's base name.
func appByExe(b *policy.Bundle, image string) (string, bool) {
	base := strings.ToLower(image[strings.LastIndexAny(image, `\/`)+1:])
	keys := b.AppByExe("windows", base)
	if len(keys) == 0 {
		return "", false
	}
	return keys[0], true
}
