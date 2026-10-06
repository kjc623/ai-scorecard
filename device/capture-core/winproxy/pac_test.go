package winproxy

import (
	"strings"
	"testing"
)

func TestClassifyHosts(t *testing.T) {
	exact, suffix := classifyHosts([]string{
		"chatgpt.com", ".openai.com", "*.anthropic.com", "API.Example.com",
		"", "*", "chatgpt.com", // duplicates and empties are dropped
	})
	wantExact := []string{"chatgpt.com", "api.example.com"}
	wantSuffix := []string{".openai.com", ".anthropic.com"}
	if strings.Join(exact, "|") != strings.Join(wantExact, "|") {
		t.Fatalf("exact = %v, want %v", exact, wantExact)
	}
	if strings.Join(suffix, "|") != strings.Join(wantSuffix, "|") {
		t.Fatalf("suffix = %v, want %v", suffix, wantSuffix)
	}
}

func TestRenderDirect(t *testing.T) {
	spec := Spec{
		ProxyAddr:      "127.0.0.1:8843",
		InterceptHosts: []string{"chatgpt.com", ".anthropic.com"},
	}
	pac := string(Render(spec))

	for _, want := range []string{
		`return "PROXY 127.0.0.1:8843";`,
		`if (exact[host]) { return true; }`,
		`host.slice(-s.length) === s`,
		`return "DIRECT";`,
	} {
		if !strings.Contains(pac, want) {
			t.Fatalf("rendered PAC is missing %q:\n%s", want, pac)
		}
	}
	if !strings.Contains(pac, `"chatgpt.com":1`) {
		t.Fatalf("exact host not embedded:\n%s", pac)
	}
	if !strings.Contains(pac, `".anthropic.com"`) {
		t.Fatalf("suffix host not embedded:\n%s", pac)
	}
	// No interception when there are no hosts.
	if strings.Contains(pac, "__sacOriginalFindProxyForURL") {
		t.Fatalf("direct PAC must not delegate:\n%s", pac)
	}
}

func TestRenderFixedProxy(t *testing.T) {
	spec := Spec{
		ProxyAddr:      "127.0.0.1:8843",
		InterceptHosts: []string{"chatgpt.com"},
		Original: Original{
			ProxyEnable:   true,
			ProxyServer:   "proxy.corp:8080;backup.corp:8080",
			ProxyOverride: []string{"<local>", "*.corp.example", "intranet"},
		},
	}
	pac := string(Render(spec))

	for _, want := range []string{
		`return "PROXY 127.0.0.1:8843";`,
		`return "PROXY proxy.corp:8080; PROXY backup.corp:8080; DIRECT";`,
		`host.indexOf(".") === -1`,
		`host === "intranet"`,
		`host.slice(-13) === ".corp.example"`,
	} {
		if !strings.Contains(pac, want) {
			t.Fatalf("rendered PAC is missing %q:\n%s", want, pac)
		}
	}
}

func TestRenderDelegating(t *testing.T) {
	origBody := `function FindProxyForURL(url, host) { return "PROXY corp.pac:8080"; }`
	spec := Spec{
		ProxyAddr:      "127.0.0.1:8843",
		InterceptHosts: []string{"chatgpt.com"},
		Original: Original{
			AutoConfigURL:  "http://corp/proxy.pac",
			AutoConfigBody: origBody,
		},
	}
	pac := string(Render(spec))

	for _, want := range []string{
		`function FindProxyForURL(url, host) { return "PROXY corp.pac:8080"; }`,
		`var __sacOriginalFindProxyForURL = FindProxyForURL;`,
		`FindProxyForURL = function(url, host) {`,
		`return __sacOriginalFindProxyForURL(url, host);`,
		`return "PROXY 127.0.0.1:8843";`,
	} {
		if !strings.Contains(pac, want) {
			t.Fatalf("rendered PAC is missing %q:\n%s", want, pac)
		}
	}
}

func TestRenderFailOpen(t *testing.T) {
	// The proxy is not in the path: the PAC must reproduce the original route with no interception.
	spec := Spec{
		ProxyAddr:      "",
		InterceptHosts: []string{"chatgpt.com"},
		Original: Original{
			ProxyEnable: true,
			ProxyServer: "proxy.corp:8080",
		},
	}
	pac := string(Render(spec))
	if strings.Contains(pac, "PROXY ") && strings.Contains(pac, "chatgpt.com") && strings.Contains(pac, "__sacIntercept") {
		t.Fatalf("a PAC with no proxy in the path must not intercept:\n%s", pac)
	}
	if !strings.Contains(pac, "PROXY proxy.corp:8080") {
		t.Fatalf("original route not preserved:\n%s", pac)
	}

	// The delegating fail-open path: an existing PAC is returned verbatim when the proxy is out of
	// the path.
	deleg := Spec{
		ProxyAddr: "",
		Original: Original{
			AutoConfigURL:  "http://corp/proxy.pac",
			AutoConfigBody: `function FindProxyForURL(u,h){ return "DIRECT"; }`,
		},
	}
	dpac := string(Render(deleg))
	if strings.Contains(dpac, "__sacIntercept") || strings.Contains(dpac, "__sacOriginalFindProxyForURL") {
		t.Fatalf("delegating fail-open must return the original PAC untouched:\n%s", dpac)
	}
	if !strings.Contains(dpac, `return "DIRECT";`) {
		t.Fatalf("original PAC body lost:\n%s", dpac)
	}
}

func TestRenderDirectNoProxy(t *testing.T) {
	spec := Spec{
		ProxyAddr:      "127.0.0.1:8843",
		InterceptHosts: []string{"chatgpt.com"},
	}
	pac := string(Render(spec))
	if !strings.Contains(pac, `return "DIRECT";`) {
		t.Fatalf("no original proxy must fall back to DIRECT:\n%s", pac)
	}
}

func TestRenderEscapesHost(t *testing.T) {
	// A hostname with characters that are special in JS must still render as a valid literal.
	spec := Spec{ProxyAddr: "127.0.0.1:8843", InterceptHosts: []string{`evil"host`}}
	pac := string(Render(spec))
	if !strings.Contains(pac, `"evil\"host"`) {
		t.Fatalf("host not JSON-escaped:\n%s", pac)
	}
}

func TestDelegatable(t *testing.T) {
	if !delegatable("function FindProxyForURL(url, host) { return \"DIRECT\"; }") {
		t.Fatal("a function declaration should be delegatable")
	}
	if !delegatable("// header\nfunction FindProxyForURL (url, host) { }") {
		t.Fatal("a function declaration with whitespace should be delegatable")
	}
	if delegatable("var FindProxyForURL = function(u, h) { return \"DIRECT\"; };") {
		t.Fatal("an assignment (not a declaration) must not be delegatable")
	}
	if delegatable("") {
		t.Fatal("an empty body must not be delegatable")
	}
}

func TestRenderRefusesNonDelegatableBody(t *testing.T) {
	// A body that is not a function declaration must not be inlined as a delegation: the PAC falls
	// back to the fixed/direct path rather than embedding something it cannot wrap safely.
	spec := Spec{
		ProxyAddr:      "127.0.0.1:8843",
		InterceptHosts: []string{"chatgpt.com"},
		Original: Original{
			AutoConfigURL:  "http://corp/proxy.pac",
			AutoConfigBody: "var FindProxyForURL = function(u,h){ return \"DIRECT\"; };",
		},
	}
	pac := string(Render(spec))
	if strings.Contains(pac, "__sacOriginalFindProxyForURL") {
		t.Fatalf("non-delegatable body was inlined:\n%s", pac)
	}
	if !strings.Contains(pac, "__sacIntercept") {
		t.Fatalf("interception lost on the fallback path:\n%s", pac)
	}
}
