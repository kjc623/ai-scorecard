package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capture-core.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// The config file is the Windows service's whole configuration, so parsing it must handle comments,
// blank lines, CRLF and a value with spaces and no quoting.
func TestConfigArgsFromFile(t *testing.T) {
	path := writeConfig(t, "# a comment\r\n\r\nSAC_TENANT_ID=11111111-1111-4111-8111-111111111111\r\nSAC_PROXY_TLS=false\r\nSAC_CA_FILE=C:\\pki\\dev ca.crt\r\n")
	args, err := configArgsFromFile(path)
	if err != nil {
		t.Fatalf("configArgsFromFile: %v", err)
	}
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"--tenant-id\x0011111111-1111-4111-8111-111111111111",
		"--proxy-tls=false", // a bool must be one --flag=value argument, not `--flag value`
		"--ca-file\x00C:\\pki\\dev ca.crt",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got %q", want, joined)
		}
	}
}

func TestConfigArgsFromFileRejectsUnknownKey(t *testing.T) {
	if _, err := configArgsFromFile(writeConfig(t, "SAC_NOT_A_REAL_KEY=1\n")); err == nil {
		t.Fatal("an unknown key was accepted; a typo must not look configured")
	}
}

func TestConfigArgsFromFileRequiresKeyValue(t *testing.T) {
	if _, err := configArgsFromFile(writeConfig(t, "SAC_TENANT_ID\n")); err == nil {
		t.Fatal("a line without '=' was accepted")
	}
}

// A --config-file profile supplies flags, and an explicitly passed flag wins over it.
func TestConfigFileSuppliesFlagsAndCommandLineWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "capture-core.env")
	body := "SAC_TENANT_ID=11111111-1111-4111-8111-111111111111\n" +
		"SAC_DEVICE_ID=22222222-2222-4222-8222-222222222222\n" +
		"SAC_SPOOL_DIR=" + filepath.Join(dir, "spool") + "\n" +
		"SAC_SPOOL_KEY=" + filepath.Join(dir, "spool.key") + "\n" +
		"SAC_RETENTION=48h\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, _, err := parseFlags([]string{"--config-file", path})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.TenantID != "11111111-1111-4111-8111-111111111111" || cfg.Retention != "48h" {
		t.Fatalf("config file did not supply flags: tenant=%q retention=%q", cfg.TenantID, cfg.Retention)
	}

	cfg2, _, err := parseFlags([]string{"--config-file", path, "--retention", "1h"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg2.Retention != "1h" {
		t.Fatalf("an explicit --retention did not win over the file: %q", cfg2.Retention)
	}
}

// The service is started with the vendor's generic file and then the tenant's: a later file wins,
// and an explicit flag wins over both. The tenant file carries only the four keys of contract §5,
// and nothing in either names a device id.
func TestRepeatedConfigFilesLaterFileWins(t *testing.T) {
	dir := t.TempDir()
	generic := filepath.Join(dir, "capture-core.env")
	tenant := filepath.Join(dir, "tenant.env")
	if err := os.WriteFile(generic, []byte("# vendor-wide values\r\n"+
		"SAC_SPOOL_DIR="+filepath.Join(dir, "spool")+"\r\n"+
		"SAC_SPOOL_KEY="+filepath.Join(dir, "spool.key")+"\r\n"+
		"SAC_CREDENTIAL_FILE="+filepath.Join(dir, "state", "credential.sealed")+"\r\n"+
		"SAC_POLICY_KEY="+strings.Repeat("ab", 32)+"\r\n"+
		"SAC_POLICY_KEY_ID=policy-key-1\r\n"+
		"SAC_TRUST_INSTALL=true\r\n"+
		"SAC_AUTH_MODE=x509\r\n"+
		"SAC_LOG_LEVEL=info\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tenant, []byte("SAC_TENANT_ID=11111111-1111-4111-8111-111111111111\r\n"+
		"SAC_DEVICE_ENDPOINT=https://devices.example.com\r\n"+
		"SAC_DEPLOYMENT_KEY=sacdk_secret\r\n"+
		"SAC_AUTH_MODE=dpop\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := parseFlags([]string{"--config-file", generic, "--config-file=" + tenant})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.AuthMode != "dpop" {
		t.Errorf("auth mode = %q; the tenant file (later) must win over the generic one", cfg.AuthMode)
	}
	if cfg.DeviceEndpoint != "https://devices.example.com" || cfg.DeploymentKey != "sacdk_secret" || cfg.TenantID == "" {
		t.Errorf("tenant values not applied: %+v", cfg)
	}
	if !cfg.TrustInstall || cfg.PolicyKeyID != "policy-key-1" || cfg.LogLevel != "info" {
		t.Errorf("generic values not applied: trust=%v key-id=%q log=%q", cfg.TrustInstall, cfg.PolicyKeyID, cfg.LogLevel)
	}
	if cfg.DeviceID != "" {
		t.Errorf("device id = %q; neither file names one and enrolment issues it", cfg.DeviceID)
	}
	if !cfg.fetchesPolicy() || !cfg.generatesDeviceCA() {
		t.Errorf("fetchesPolicy=%v generatesDeviceCA=%v, want both for a tenant-packaged device", cfg.fetchesPolicy(), cfg.generatesDeviceCA())
	}
	if got, want := cfg.stateDir(), filepath.Join(dir, "state"); got != want {
		t.Errorf("state dir = %q, want the credential's directory %q", got, want)
	}

	cfg, _, err = parseFlags([]string{"--config-file", generic, "--config-file", tenant, "--log-level", "warn"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("log level = %q; an explicit flag must win over every file", cfg.LogLevel)
	}
}

func TestMissingConfigFileNamesThePath(t *testing.T) {
	dir := t.TempDir()
	generic := writeConfig(t, "SAC_LOG_LEVEL=info\n")
	missing := filepath.Join(dir, "ShadowAICapture", "tenant.env")
	_, _, err := parseFlags([]string{"--config-file", generic, "--config-file", missing})
	if err == nil {
		t.Fatal("a missing tenant file was accepted")
	}
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error %q does not name the missing file", err)
	}
}

func TestPrescanFlagValuesKeepsOrder(t *testing.T) {
	got := prescanFlagValues([]string{"--config-file", "a.env", "-config-file=b.env", "--retention", "1h", "--config-file=c.env", "--", "--config-file", "ignored"}, "config-file")
	if strings.Join(got, ",") != "a.env,b.env,c.env" {
		t.Fatalf("prescanFlagValues = %v, want [a.env b.env c.env]", got)
	}
}
