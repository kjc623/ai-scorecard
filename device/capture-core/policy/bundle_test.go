package policy

import (
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
