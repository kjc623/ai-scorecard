package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/drain"
	"github.com/shadow-ai-capture/device/protocol"
)

// The release key every test signs with: the seed 0x01 repeated, whose statement below was signed
// by device/installer/agent-release.mjs, so the build's signer and this verifier agree byte for byte.
var (
	releaseKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	releasePub = releaseKey.Public().(ed25519.PublicKey)
)

const nodeSignedStatement = `{"algorithm":"ed25519","payload":{"type":"agent_release","platform":"windows-amd64","version":"1.0.42","file":"ShadowAICapture.msi","sha256":"abababababababababababababababababababababababababababababababab","size":12345},"signature":"BjVrvjTQpXOryqMrzz9hxWhjc30OOxj5l/iAlbeqbUsfhDYGM1N02B2qO77AztTGzSoAyu9c2HaJToQwzh05Bw=="}`

func signStatement(t *testing.T, payload string) []byte {
	t.Helper()
	raw, err := json.Marshal(protocol.SignedAgentRelease{
		Algorithm: "ed25519",
		Payload:   json.RawMessage(payload),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(releaseKey, []byte(payload))),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func statementPayload(version string, pkg []byte) string {
	sum := sha256.Sum256(pkg)
	st, _ := json.Marshal(protocol.AgentRelease{
		Type: protocol.AgentReleaseType, Platform: protocol.AgentPlatformWindowsAMD64, Version: version,
		File: "ShadowAICapture.msi", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(pkg)),
	})
	return string(st)
}

func TestTheBuildsSignatureVerifies(t *testing.T) {
	st, err := verifyAgentRelease([]byte(nodeSignedStatement), releasePub)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "1.0.42" || st.Platform != protocol.AgentPlatformWindowsAMD64 || st.Size != 12345 {
		t.Fatalf("payload: %+v", st)
	}
}

func TestAStatementIsRefusedUnlessSignedAndWellFormed(t *testing.T) {
	pkg := []byte("msi")
	good := statementPayload("1.0.2", pkg)
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	forged, _ := json.Marshal(protocol.SignedAgentRelease{Algorithm: "ed25519", Payload: json.RawMessage(good),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(other, []byte(good)))})
	for name, raw := range map[string][]byte{
		"another key":   forged,
		"tampered":      bytes.Replace(signStatement(t, good), []byte(`"1.0.2"`), []byte(`"1.0.3"`), 1),
		"a policy":      signStatement(t, `{"type":"policy_bundle","platform":"windows-amd64","version":"1.0.2","file":"a.msi","sha256":"`+strings.Repeat("ab", 32)+`","size":3}`),
		"unknown field": signStatement(t, strings.Replace(good, `"type"`, `"extra":1,"type"`, 1)),
		"a path":        signStatement(t, strings.Replace(good, `"ShadowAICapture.msi"`, `"..\\evil.msi"`, 1)),
		"a version":     signStatement(t, strings.Replace(good, `"1.0.2"`, `"latest"`, 1)),
		"not json":      []byte("release"),
	} {
		if _, err := verifyAgentRelease(raw, releasePub); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestAgentVersionNewer(t *testing.T) {
	for _, c := range []struct {
		candidate, current string
		want               bool
	}{
		{"1.0.43", "1.0.42", true},
		{"1.1", "1.0.99", true},
		{"1.0.10", "1.0.9", true},
		{"1.0.42", "1.0.42", false},
		{"1.0.41", "1.0.42", false},
		{"1.0.42", "dev", false},
		{"x", "1.0.0", false},
	} {
		if got := agentVersionNewer(c.candidate, c.current); got != c.want {
			t.Errorf("agentVersionNewer(%q, %q) = %v", c.candidate, c.current, got)
		}
	}
}

// fakeAgentSource serves one statement and a package, cutting each download after chunk bytes.
type fakeAgentSource struct {
	statement []byte
	err       error
	pkg       []byte
	chunk     int64
	requests  int
	offsets   []int64
}

func (f *fakeAgentSource) FetchAgentRelease(context.Context) ([]byte, error) {
	return f.statement, f.err
}

func (f *fakeAgentSource) DownloadAgentPackage(_ context.Context, w io.Writer, offset, limit int64) (int64, error) {
	f.requests++
	f.offsets = append(f.offsets, offset)
	end := min(offset+min(f.chunk, limit), int64(len(f.pkg)))
	n, err := w.Write(f.pkg[offset:end])
	if err != nil {
		return int64(n), err
	}
	if end < int64(len(f.pkg)) {
		return int64(n), errors.New("drain: transport error: the gateway ended the response")
	}
	return int64(n), nil
}

type installCall struct{ path, log string }

func newTestUpdater(t *testing.T, src agentSource, current string) (*agentUpdater, *[]installCall, string) {
	t.Helper()
	dir := t.TempDir()
	tenant := filepath.Join(dir, "profile", "tenant.env")
	if err := os.MkdirAll(filepath.Dir(tenant), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tenant, []byte("SAC_TENANT_ID=t\r\nSAC_DEPLOYMENT_KEY=k\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []installCall
	u := newAgentUpdater(src, releasePub, current, protocol.AgentPlatformWindowsAMD64, filepath.Join(dir, "update"), tenant,
		func(path, log string) error { calls = append(calls, installCall{path, log}); return nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return u, &calls, dir
}

func TestANewerReleaseIsDownloadedResumedVerifiedAndInstalled(t *testing.T) {
	pkg := bytes.Repeat([]byte("package "), 1000)
	src := &fakeAgentSource{statement: signStatement(t, statementPayload("1.0.43", pkg)), pkg: pkg, chunk: 3000}
	u, calls, _ := newTestUpdater(t, src, "1.0.42")
	u.once(context.Background())

	if st := u.Status(); st.LastAnswer != "installing" || st.OfferedVersion != "1.0.43" || st.LastError != "" {
		t.Fatalf("status: %+v", st)
	}
	if len(*calls) != 1 {
		t.Fatalf("installer started %d times", len(*calls))
	}
	got, err := os.ReadFile((*calls)[0].path)
	if err != nil || !bytes.Equal(got, pkg) || filepath.Base((*calls)[0].path) != "ShadowAICapture.msi" {
		t.Fatalf("installed %s: %d bytes, %v", (*calls)[0].path, len(got), err)
	}
	if want := []int64{0, 3000, 6000}; len(src.offsets) != len(want) || src.offsets[1] != want[1] || src.offsets[2] != want[2] {
		t.Fatalf("download offsets %v, want %v", src.offsets, want)
	}
	tenant, err := os.ReadFile(filepath.Join(filepath.Dir((*calls)[0].path), tenantPackageFile))
	if err != nil || string(tenant) != "SAC_TENANT_ID=t\r\nSAC_DEPLOYMENT_KEY=k\r\n" {
		t.Fatalf("tenant file beside the package: %q, %v", tenant, err)
	}

	// The install has not replaced the agent by the next check: it is not re-run.
	u.once(context.Background())
	if st := u.Status(); st.LastAnswer != "retry_later" || len(*calls) != 1 {
		t.Fatalf("second check: %+v, %d installs", st, len(*calls))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir((*calls)[0].path), tenantPackageFile)); !os.IsNotExist(err) {
		t.Fatalf("the tenant file outlived the install: %v", err)
	}
	// After the retry window it is.
	u.clock = func() time.Time { return time.Now().Add(agentUpdateRetry + time.Minute) }
	u.once(context.Background())
	if len(*calls) != 2 {
		t.Fatalf("after the retry window: %d installs", len(*calls))
	}
}

func TestNoInstallWithoutANewerVerifiedMatchingRelease(t *testing.T) {
	pkg := []byte("the package")
	tampered := append([]byte(nil), pkg...)
	tampered[0] = 'T'
	for name, c := range map[string]struct {
		src    *fakeAgentSource
		answer string
	}{
		"same version":    {&fakeAgentSource{statement: signStatement(t, statementPayload("1.0.42", pkg)), pkg: pkg, chunk: 100}, "current"},
		"older version":   {&fakeAgentSource{statement: signStatement(t, statementPayload("1.0.41", pkg)), pkg: pkg, chunk: 100}, "current"},
		"no release":      {&fakeAgentSource{err: drain.ErrNoAgentRelease}, "no_release"},
		"unreachable":     {&fakeAgentSource{err: errors.New("drain: transport error")}, "error"},
		"unsigned":        {&fakeAgentSource{statement: []byte(nodeSignedStatement[:40] + "}"), pkg: pkg, chunk: 100}, "error"},
		"package differs": {&fakeAgentSource{statement: signStatement(t, statementPayload("1.0.43", pkg)), pkg: tampered, chunk: 100}, "error"},
		"other platform": {&fakeAgentSource{statement: signStatement(t, strings.Replace(statementPayload("1.0.43", pkg),
			protocol.AgentPlatformWindowsAMD64, "darwin-arm64", 1)), pkg: pkg, chunk: 100}, "current"},
	} {
		t.Run(name, func(t *testing.T) {
			u, calls, _ := newTestUpdater(t, c.src, "1.0.42")
			u.once(context.Background())
			if st := u.Status(); st.LastAnswer != c.answer || len(*calls) != 0 {
				t.Fatalf("status %+v, %d installs; want %s and none", st, len(*calls), c.answer)
			}
		})
	}
}

func TestADevelopmentBuildIsNeverReplaced(t *testing.T) {
	pkg := []byte("the package")
	u, calls, _ := newTestUpdater(t, &fakeAgentSource{statement: signStatement(t, statementPayload("9.9.9", pkg)), pkg: pkg, chunk: 100}, "dev")
	u.once(context.Background())
	if len(*calls) != 0 {
		t.Fatal("a development build was replaced")
	}
}

func TestTidyKeepsOnlyWhatAPendingUpdateNeeds(t *testing.T) {
	u, _, _ := newTestUpdater(t, &fakeAgentSource{err: drain.ErrNoAgentRelease}, "1.0.43")
	write := func(rel string) {
		p := filepath.Join(u.dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"1.0.43/ShadowAICapture.msi", "1.0.43/install.log", "1.0.43/" + tenantPackageFile, "1.0.44/ShadowAICapture.msi.part", "1.0.44/" + tenantPackageFile} {
		write(f)
	}
	u.once(context.Background())
	for f, want := range map[string]bool{
		"1.0.43/ShadowAICapture.msi":      false,
		"1.0.43/install.log":              true,
		"1.0.43/" + tenantPackageFile:     false,
		"1.0.44/ShadowAICapture.msi.part": true,
		"1.0.44/" + tenantPackageFile:     false,
	} {
		_, err := os.Stat(filepath.Join(u.dir, f))
		if (err == nil) != want {
			t.Errorf("%s: present %v, want %v", f, err == nil, want)
		}
	}
}

func TestInstalledTenantFileIsTheLastTenantEnv(t *testing.T) {
	cfg := Config{configFiles: []string{filepath.Join("p", "capture-core.env"), filepath.Join("p", "tenant.env")}}
	if p, ok := installedTenantFile(cfg); !ok || !strings.HasSuffix(p, "tenant.env") {
		t.Fatalf("got %q %v", p, ok)
	}
	if _, ok := installedTenantFile(Config{configFiles: []string{"capture-core.env"}}); ok {
		t.Fatal("found a tenant file where none is configured")
	}
}
