package policy

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestBundleValidateProxyAndCLIShim(t *testing.T) {
	// Accept: a well-formed proxy listen address and shim target with a closed-set runtime list.
	b := testBundle("42")
	b.Interception.ProxyListen = "127.0.0.1:8843"
	b.Interception.ProxyCanary = "api.anthropic.com:443"
	b.CLIShim = CLIShimPolicy{ProxyAddr: "127.0.0.1:8843", Runtimes: []string{"go", "node", "python"}}
	if err := b.Validate(); err != nil {
		t.Fatalf("a well-formed proxy/shim was rejected: %v", err)
	}

	// Reject: a proxy_canary that is not host:port (it would silently degrade the probe).
	badCanary := testBundle("42")
	badCanary.Interception.ProxyCanary = "no-port-here"
	if err := badCanary.Validate(); err == nil {
		t.Fatal("a malformed proxy_canary was accepted")
	}

	// Reject: a proxy_listen that is not host:port.
	badListen := testBundle("42")
	badListen.Interception.ProxyListen = "not-a-host-port"
	if err := badListen.Validate(); err == nil {
		t.Fatal("a malformed proxy_listen was accepted")
	}

	// Accept: a well-formed pac_listen, and reject a malformed one (it is written into each
	// user's Internet Settings, so it must be a usable loopback host:port).
	okPAC := testBundle("42")
	okPAC.Interception.PacListen = "127.0.0.1:8350"
	if err := okPAC.Validate(); err != nil {
		t.Fatalf("a well-formed pac_listen was rejected: %v", err)
	}
	badPAC := testBundle("42")
	badPAC.Interception.PacListen = "not-a-host-port"
	if err := badPAC.Validate(); err == nil {
		t.Fatal("a malformed pac_listen was accepted")
	}

	// Reject: a cli_shim proxy_addr that is not host:port.
	badAddr := testBundle("42")
	badAddr.CLIShim.ProxyAddr = "no-port-here"
	if err := badAddr.Validate(); err == nil {
		t.Fatal("a malformed cli_shim proxy_addr was accepted")
	}

	// Reject: a shim runtime outside the closed set.
	badRuntime := testBundle("42")
	badRuntime.CLIShim.Runtimes = []string{"go", "ruby"}
	if err := badRuntime.Validate(); err == nil {
		t.Fatal("a runtime outside {go,node,python} was accepted")
	}

	// Reject: an empty runtime name (a bare comma).
	emptyRuntime := testBundle("42")
	emptyRuntime.CLIShim.Runtimes = []string{"go", ""}
	if err := emptyRuntime.Validate(); err == nil {
		t.Fatal("an empty runtime name was accepted")
	}
}

// endpointSection is an endpoint section in control-api's spelling, with values off the defaults
// where a decode could otherwise pass by accident.
const endpointSection = `{
  "inventory": {"enabled": true, "interval_minutes": 360},
  "processes": {"enabled": true},
  "flows":     {"enabled": false},
  "otel":      {"enabled": true, "http_listen": "127.0.0.1:47318", "grpc_listen": "127.0.0.1:47317"},
  "hooks":     {"enabled": true, "managed_only": true},
  "tools": {
    "claude_code": {"otel": true,  "hooks": true},
    "codex":       {"otel": true,  "hooks": false},
    "copilot":     {"otel": true,  "hooks": false},
    "cursor":      {"otel": false, "hooks": true}
  },
  "discovery_daily_budget": 200
}`

// signWithEndpoint signs testBundle with section, as raw JSON, for its endpoint section, so the
// payload carries exactly the names under test.
func signWithEndpoint(t *testing.T, priv ed25519.PrivateKey, version, section string) []byte {
	t.Helper()
	raw, err := Sign("policy-key-1", priv, testBundle(version))
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(env["payload"], &payload); err != nil {
		t.Fatal(err)
	}
	payload["endpoint"] = json.RawMessage(section)
	newPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	env["payload"] = newPayload
	env["signature"], _ = json.Marshal(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, newPayload)))
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEndpointSectionDecodes(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	b, err := v.Open(signWithEndpoint(t, priv, "42", endpointSection), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := EndpointPolicy{
		Inventory: EndpointInventory{Enabled: true, IntervalMinutes: 360},
		Processes: EndpointSwitch{Enabled: true},
		Flows:     EndpointSwitch{Enabled: false},
		OTel:      EndpointOTel{Enabled: true, HTTPListen: "127.0.0.1:47318", GRPCListen: "127.0.0.1:47317"},
		Hooks:     EndpointHooks{Enabled: true, ManagedOnly: true},
		Tools: map[string]EndpointTool{
			"claude_code": {OTel: true, Hooks: true},
			"codex":       {OTel: true},
			"copilot":     {OTel: true},
			"cursor":      {Hooks: true},
		},
		DiscoveryDailyBudget: 200,
	}
	if !reflect.DeepEqual(b.Endpoint, want) {
		t.Fatalf("endpoint = %+v\nwant       %+v", b.Endpoint, want)
	}

	// A misspelt name inside the section is an unknown field, so the whole bundle is refused.
	misspelt := strings.Replace(endpointSection, `"managed_only"`, `"managedOnly"`, 1)
	if _, err := v.Open(signWithEndpoint(t, priv, "43", misspelt), nil); CauseOf(err) != CauseSchemaInvalid {
		t.Fatalf("misspelt endpoint field cause = %q, want %q (err=%v)", CauseOf(err), CauseSchemaInvalid, err)
	}

	// A bundle without the section switches every endpoint collector off.
	none := testBundle("44")
	if err := none.Validate(); err != nil || !reflect.DeepEqual(none.Endpoint, EndpointPolicy{}) {
		t.Fatalf("a bundle without an endpoint section = %+v, %v", none.Endpoint, err)
	}
}

// TLS inspection is the bundle's interception.enabled: on when the tenant turned it on, and off for
// a bundle that does not name it.
func TestInterceptionEnabledDecodes(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	on := testBundle("50")
	on.Interception.Enabled = true
	raw, err := Sign("policy-key-1", priv, on)
	if err != nil {
		t.Fatal(err)
	}
	b, err := v.Open(raw, nil)
	if err != nil || !b.Interception.Enabled {
		t.Fatalf("a bundle with interception.enabled true opened as %+v, %v", b, err)
	}

	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	payload := string(env["payload"])
	unnamed := strings.Replace(payload, `"interception":{"enabled":true,`, `"interception":{`, 1)
	if unnamed == payload {
		t.Fatalf("the payload does not open its interception section with enabled: %s", payload)
	}
	env["payload"] = json.RawMessage(unnamed)
	env["signature"], _ = json.Marshal(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(unnamed))))
	raw, err = json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	b, err = v.Open(raw, nil)
	if err != nil || b.Interception.Enabled {
		t.Fatalf("a bundle without interception.enabled opened as %+v, %v; want inspection off", b, err)
	}
}

func TestEndpointSectionValidation(t *testing.T) {
	v, priv := newKeyPair(t, "policy-key-1")
	refused := map[string][2]string{
		"private http_listen":    {`"127.0.0.1:47318"`, `"10.0.0.5:47318"`},
		"wildcard grpc_listen":   {`"127.0.0.1:47317"`, `"0.0.0.0:47317"`},
		"localhost by name":      {`"127.0.0.1:47318"`, `"localhost:47318"`},
		"no port":                {`"127.0.0.1:47317"`, `"127.0.0.1"`},
		"port out of range":      {`"127.0.0.1:47318"`, `"127.0.0.1:70000"`},
		"empty while enabled":    {`"127.0.0.1:47317"`, `""`},
		"unknown tool key":       {`"cursor":`, `"windsurf":`},
		"negative budget":        {`"discovery_daily_budget": 200`, `"discovery_daily_budget": -1`},
		"interval below 15":      {`"interval_minutes": 360`, `"interval_minutes": 14`},
		"interval zero, enabled": {`"interval_minutes": 360`, `"interval_minutes": 0`},
		"interval negative":      {`"interval_minutes": 360`, `"interval_minutes": -360`},
	}
	for name, r := range refused {
		section := strings.Replace(endpointSection, r[0], r[1], 1)
		if section == endpointSection {
			t.Fatalf("%s: the replacement did not apply", name)
		}
		if _, err := v.Open(signWithEndpoint(t, priv, "50", section), nil); CauseOf(err) != CauseSchemaInvalid {
			t.Errorf("%s: cause = %q, want %q (err=%v)", name, CauseOf(err), CauseSchemaInvalid, err)
		}
	}

	accepted := map[string][2]string{
		"interval at 15": {`"interval_minutes": 360`, `"interval_minutes": 15`},
		"IPv6 loopback":  {`"127.0.0.1:47318"`, `"[::1]:47318"`},
		"zero budget":    {`"discovery_daily_budget": 200`, `"discovery_daily_budget": 0`},
	}
	for name, r := range accepted {
		section := strings.Replace(endpointSection, r[0], r[1], 1)
		if _, err := v.Open(signWithEndpoint(t, priv, "60", section), nil); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}

	// A collector that is off may leave its values out, but a value it carries is still checked.
	off := testBundle("70")
	off.Endpoint = EndpointPolicy{Processes: EndpointSwitch{Enabled: true}}
	if err := off.Validate(); err != nil {
		t.Fatalf("inventory and otel off without values was refused: %v", err)
	}
	off.Endpoint.Inventory.IntervalMinutes = 5
	if err := off.Validate(); err == nil {
		t.Fatal("an interval below 15 minutes was accepted with the scanner off")
	}
	off.Endpoint.Inventory.IntervalMinutes = 0
	off.Endpoint.OTel.HTTPListen = "192.0.2.1:47318"
	if err := off.Validate(); err == nil {
		t.Fatal("a non-loopback OTLP address was accepted with the receiver off")
	}
}
