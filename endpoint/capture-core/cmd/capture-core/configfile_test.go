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
