package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// configEnvFlags maps the SAC_* names in the enrolment profile to the capture-core flag each one
// sets. It is the same catalogue installer/manifest.mjs declares, and installer/verify.mjs fails if
// the two disagree. --config-file reads a file in this vocabulary, so a Windows service can be
// configured by a file the MSI installs instead of by a command line built from it: paths with
// spaces are safe, and reconfiguration is a file edit rather than an MSI rebuild.
var configEnvFlags = map[string]string{
	"SAC_TENANT_ID":            "--tenant-id",
	"SAC_DEVICE_ID":            "--device-id",
	"SAC_USER_REF":             "--user-ref",
	"SAC_POPULATION":           "--population",
	"SAC_SPOOL_DIR":            "--spool-dir",
	"SAC_SPOOL_KEY":            "--spool-key",
	"SAC_SPOOL_BOUNDS":         "--spool-bounds",
	"SAC_RETENTION":            "--retention",
	"SAC_HEALTH_FILE":          "--health-file",
	"SAC_BUNDLE":               "--bundle",
	"SAC_POLICY_KEY":           "--policy-key",
	"SAC_POLICY_KEY_ID":        "--policy-key-id",
	"SAC_CLASSIFIER_ADDRESS":   "--classifier-address",
	"SAC_CLASSIFIER_BUDGET":    "--classifier-budget",
	"SAC_CLASSIFIER_RELEASE":   "--classifier-release",
	"SAC_CLASSIFIER_PUBKEY":    "--classifier-pubkey",
	"SAC_CONTENT_DIR":          "--content-dir",
	"SAC_CONTENT_KEY":          "--content-key",
	"SAC_PROXY_TLS":            "--proxy-tls",
	"SAC_PROXY_TLS_LISTEN":     "--proxy-tls-listen",
	"SAC_PROXY_TLS_CANARY":     "--proxy-tls-canary",
	"SAC_PROXY_LOOPBACK":       "--proxy-loopback",
	"SAC_PROC_DETECT":          "--proc-detect",
	"SAC_DRAIN_DEADLINE":       "--drain-deadline",
	"SAC_TRUST_INSTALL":        "--trust-install",
	"SAC_TRUST_STORE":          "--trust-store",
	"SAC_TRUST_REMOVE_ON_STOP": "--trust-remove-on-stop",
	"SAC_CA_KEY":               "--ca-key",
	"SAC_CA_CERT":              "--ca-cert",
	"SAC_CLI_SHIM":             "--cli-shim",
	"SAC_SHIM_DIR":             "--shim-dir",
	"SAC_ATTACHMENT_CAP":       "--attachment-cap",
	"SAC_DEVICE_ENDPOINT":      "--device-endpoint",
	"SAC_AUTH_MODE":            "--auth-mode",
	"SAC_CREDENTIAL_FILE":      "--credential-file",
	"SAC_ENROLMENT_TOKEN":      "--enrolment-token",
	"SAC_CA_FILE":              "--ca-file",
	"SAC_MDM_ID":               "--mdm-id",
	"SAC_BACKOFF_BASE":         "--backoff-base",
	"SAC_BACKOFF_CAP":          "--backoff-cap",
	"SAC_LOG_LEVEL":            "--log-level",
	"SAC_LOG_FORMAT":           "--log-format",
}

// configBoolEnv is the subset of configEnvFlags whose flag is a Go bool. installer/verify.mjs fails
// if this set and the manifest's `kind: bool` entries disagree.
var configBoolEnv = map[string]bool{
	"SAC_PROXY_TLS":            true,
	"SAC_PROXY_LOOPBACK":       true,
	"SAC_PROC_DETECT":          true,
	"SAC_TRUST_INSTALL":        true,
	"SAC_TRUST_REMOVE_ON_STOP": true,
	"SAC_CLI_SHIM":             true,
}

// configArgsFromFile reads a KEY=VALUE profile and returns the equivalent flag list. Blank lines and
// lines starting with '#' are ignored. An unknown key is an error rather than a silent no-op, so a
// typo cannot look configured; the value is everything after the first '=', taken as-is so a secret
// or a path with spaces needs no quoting.
func configArgsFromFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text()) // also strips a trailing CR from a CRLF file
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		key, value, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("config file %s:%d: line is not KEY=VALUE", path, line)
		}
		trimmed := strings.TrimSpace(key)
		flag, known := configEnvFlags[trimmed]
		if !known {
			return nil, fmt.Errorf("config file %s:%d: unknown key %q", path, line, trimmed)
		}
		// A Go bool flag does not consume a separate token, so it must be one `--flag=value` argument;
		// `--flag value` would leave the value as a positional argument and fail the parse.
		if configBoolEnv[trimmed] {
			out = append(out, flag+"="+value)
		} else {
			out = append(out, flag, value)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	return out, nil
}

// prescanFlagValue finds a flag's value before the flag set is built, so --config-file can be read
// and its flags placed ahead of the command line (which then wins on any duplicate). It understands
// --name value, --name=value and the single-dash spellings.
func prescanFlagValue(args []string, name string) string {
	for i, a := range args {
		for _, prefix := range []string{"--", "-"} {
			if a == prefix+name {
				if i+1 < len(args) {
					return args[i+1]
				}
				return ""
			}
			if strings.HasPrefix(a, prefix+name+"=") {
				return strings.TrimPrefix(a, prefix+name+"=")
			}
		}
	}
	return ""
}
