package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// signedCatalogBundle signs a bundle holding catalog.
func signedCatalogBundle(t *testing.T, priv ed25519.PrivateKey, version string, catalog []policy.CatalogApp) []byte {
	t.Helper()
	raw, err := policy.Sign("policy-key-1", priv, &policy.Bundle{
		Version:       version,
		EffectiveAt:   time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
		TenantDefault: protocol.ModeM1,
		Catalog:       catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// catalogService is a service whose bundle in force holds a catalog: claude.exe on this platform,
// Cursor.exe on another platform and ollama.exe on any. apply signs a bundle and puts it in force.
func catalogService(t *testing.T) (*service, func(version string, catalog []policy.CatalogApp)) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	here := catalogPlatform(runtime.GOOS)
	other := "windows"
	if here == other {
		other = "linux"
	}
	catalog := []policy.CatalogApp{
		{AppKey: "claude_code", Category: "coding_agent", Signals: []policy.CatalogSignal{{Platform: here, Kind: policy.SignalWindowsExe, Value: "claude.exe"}}},
		{AppKey: "cursor", Category: "ide", Signals: []policy.CatalogSignal{{Platform: other, Kind: policy.SignalWindowsExe, Value: "Cursor.exe"}}},
		{AppKey: "ollama", Category: "local_runtime", Signals: []policy.CatalogSignal{{Platform: "any", Kind: policy.SignalWindowsExe, Value: "ollama.exe"}}},
	}
	cloud := startFakeCloud(t, signedCatalogBundle(t, priv, "5", catalog))
	svc, err := newService(context.Background(), testConfig(t, cloud, pub), testLogger(t))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	if b := svc.currentBundle(); b == nil || b.Version != "5" || len(b.Catalog) != len(catalog) {
		t.Fatalf("bundle in force = %+v, want version 5 with the catalog", b)
	}
	return svc, func(version string, catalog []policy.CatalogApp) {
		t.Helper()
		res := svc.store.Apply(signedCatalogBundle(t, priv, version, catalog))
		if res.Err != nil || res.Outcome != policy.OutcomeAccepted {
			t.Fatalf("bundle not accepted: %+v", res)
		}
		svc.policyFetched(res)
	}
}

// assertAppByExe checks a seam the service wired against the catalog of catalogService's bundle,
// then against a bundle with no catalog, which names no app.
func assertAppByExe(t *testing.T, appByExe func(string) (string, bool), apply func(string, []policy.CatalogApp)) {
	t.Helper()
	if appByExe == nil {
		t.Fatal("the service left the AppByExe seam nil")
	}
	for base, want := range map[string]string{
		"claude.exe": "claude_code", // the lower-case image base name the callers pass
		"CLAUDE.EXE": "claude_code",
		"ollama.exe": "ollama", // a signal for any platform
		"cursor.exe": "",       // a signal for another platform
		"python.exe": "",
		"":           "",
	} {
		key, ok := appByExe(base)
		if key != want || ok != (want != "") {
			t.Errorf("AppByExe(%q) = %q, %t; want %q", base, key, ok, want)
		}
	}

	apply("6", nil)
	if key, ok := appByExe("claude.exe"); ok {
		t.Errorf("under a bundle with no catalog AppByExe(claude.exe) = %q", key)
	}
}

// catalogPlatform gives every platform the agent builds for a name in the catalog's platform set.
func TestCatalogPlatform(t *testing.T) {
	for goos, want := range map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux"} {
		if got := catalogPlatform(goos); got != want || !slices.Contains(policy.CatalogPlatforms, got) {
			t.Errorf("catalogPlatform(%q) = %q, want %q", goos, got, want)
		}
	}
}

// proxy.tls's native-first exclusion names a connecting process's app from the catalog of the
// bundle in force.
func TestProxyNamesItsClientsAppFromTheCatalogInForce(t *testing.T) {
	svc, apply := catalogService(t)
	cfg := svc.proxyConfig(svc.currentBundle(), nil, nil)
	assertAppByExe(t, cfg.AppByExe, apply)
}

// The GenAI normalizer names a resolved sender's app from the catalog of the bundle in force.
func TestOTelNormalizersNameTheSendersAppFromTheCatalogInForce(t *testing.T) {
	svc, apply := catalogService(t)
	deps := svc.normalizerDeps(core.NewCounterSet(time.Now()))
	assertAppByExe(t, deps.AppByExe, apply)
}
