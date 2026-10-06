package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPolicyKey = "5cbf9e42a4ab5b0c4b3e4dc1b42b2f2e3c1e4c5f3a8fb7d2a1e0f9c8b7a6d5e4"

func writeConfigFile(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The installed configuration is two files: the vendor file, then the tenant file, which wins on
// any key both carry. A flag on the command line wins over both.
func TestConfigFilesApplyInOrderAndTheCommandLineWins(t *testing.T) {
	vendor := writeConfigFile(t,
		"# vendor defaults",
		"SAC_STATE_DIR=C:\\ProgramData\\Shadow AI Capture\\state",
		"SAC_POLICY_KEY="+testPolicyKey,
		"SAC_LOG_LEVEL=warn",
		"SAC_DEVICE_ENDPOINT=https://vendor.example.com",
	)
	tenant := writeConfigFile(t,
		"SAC_TENANT_ID=22222222-2222-4222-8222-222222222222",
		"SAC_DEVICE_ENDPOINT=https://devices.contoso.example",
		"SAC_DEPLOYMENT_KEY=sacdk_secret=with=equals",
	)
	cfg, _, err := parseFlags([]string{"--config-file", vendor, "--config-file", tenant, "--log-level", "debug"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.StateDir != `C:\ProgramData\Shadow AI Capture\state` {
		t.Errorf("state dir = %q; a path with spaces must need no quoting", cfg.StateDir)
	}
	if cfg.DeviceEndpoint != "https://devices.contoso.example" {
		t.Errorf("endpoint = %q, want the tenant file's", cfg.DeviceEndpoint)
	}
	if cfg.DeploymentKey != "sacdk_secret=with=equals" {
		t.Errorf("deployment key = %q, want everything after the first '='", cfg.DeploymentKey)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log level = %q, want the command line's", cfg.LogLevel)
	}
	if cfg.PolicyKeyID != "policy-key-1" || cfg.DeviceIdentity != "clear" {
		t.Errorf("defaults = %q %q", cfg.PolicyKeyID, cfg.DeviceIdentity)
	}
}

func TestConfigFileRefusesAnUnknownKeyAndAMalformedLine(t *testing.T) {
	for _, line := range []string{"SAC_AUTH_MODE=x509", "SAC_SPOOL_DIR=/tmp", "not a key value line"} {
		path := writeConfigFile(t, "SAC_STATE_DIR=/var/lib/sac", line)
		if _, _, err := parseFlags([]string{"--config-file", path}); err == nil {
			t.Errorf("%q was accepted", line)
		}
	}
}

func TestConfigFileThatDoesNotExistNamesThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "tenant.env")
	_, _, err := parseFlags([]string{"--config-file", missing})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("err = %v, want one naming %s", err, missing)
	}
}

func TestValidateRequiresTheTenantFileValues(t *testing.T) {
	base := []string{"--state-dir", t.TempDir(), "--tenant-id", "t1", "--device-endpoint", "https://devices.example.com"}
	if _, _, err := parseFlags(base); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	for _, drop := range []string{"--state-dir", "--tenant-id", "--device-endpoint"} {
		var args []string
		for i := 0; i < len(base); i += 2 {
			if base[i] != drop {
				args = append(args, base[i], base[i+1])
			}
		}
		if _, _, err := parseFlags(args); err == nil {
			t.Errorf("a configuration without %s was accepted", drop)
		}
	}
}

func TestValidateRefusesUnusableValues(t *testing.T) {
	base := []string{"--state-dir", t.TempDir(), "--tenant-id", "t1", "--device-endpoint", "https://devices.example.com"}
	for name, extra := range map[string][]string{
		"plain http endpoint":    {"--device-endpoint", "http://devices.example.com"},
		"endpoint with a path":   {"--device-endpoint", "https://devices.example.com/v1"},
		"short policy key":       {"--policy-key", "abcd"},
		"non-hex policy key":     {"--policy-key", strings.Repeat("zz", 32)},
		"release without a key":  {"--classifier-release", "/opt/sac/classifier"},
		"unknown identity":       {"--device-identity", "partial"},
		"unknown log level":      {"--log-level", "chatty"},
		"positional argument":    {"run"},
		"unknown flag":           {"--device-id", "d1"},
		"removed auth mode flag": {"--auth-mode", "x509"},
	} {
		if _, _, err := parseFlags(append(append([]string(nil), base...), extra...)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestVersionNeedsNoConfiguration(t *testing.T) {
	_, mode, err := parseFlags([]string{"--version"})
	if err != nil || !mode.showVersion {
		t.Fatalf("--version: mode %+v, err %v", mode, err)
	}
}

func TestBrowserOriginSelectsTheRelay(t *testing.T) {
	if !isBrowserOrigin("chrome-extension://abcdefghijklmnopabcdefghijklmnop/") {
		t.Fatal("a Chrome/Edge caller origin was not recognised")
	}
	for _, arg := range []string{"--config-file", "chrome-extension", "--parent-window=1234"} {
		if isBrowserOrigin(arg) {
			t.Errorf("%q selected the relay", arg)
		}
	}
}
