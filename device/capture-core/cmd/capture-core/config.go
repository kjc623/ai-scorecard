package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/shadow-ai-capture/device/protocol"
)

// Config is everything the agent is configured with. Everything else (what to collect, at which
// mode, where the proxy listens) comes from the signed policy bundle.
type Config struct {
	// StateDir holds the spool, the keys, the device credential, held content, the cached policy
	// bundle and the interception CA. It is protected so only the service can read it.
	StateDir string
	// TenantID, DeviceEndpoint and DeploymentKey come from the tenant file the MDM delivers.
	TenantID       string
	DeviceEndpoint string
	DeploymentKey  string
	// CAFile names a PEM CA set trusted for the device endpoint in addition to the system roots.
	CAFile string
	// PolicyKey is the hex-encoded Ed25519 public key the policy bundle must verify under, and
	// PolicyKeyID the key id it must name. With no key the device runs at M0.
	PolicyKey   string
	PolicyKeyID string
	// ClassifierRelease is the signed classifier release directory and ClassifierPubkey the
	// hex-encoded Ed25519 key it must verify under. With neither, classification is rules-only.
	ClassifierRelease string
	ClassifierPubkey  string
	// DeviceIdentity is the tenant's identity setting to act on until the server states it:
	// clear sends the hostname and account name, hashed sends neither.
	DeviceIdentity string
	LogLevel       string
}

// configKeys maps each configuration-file key to the flag it sets. The installer generates the
// vendor and tenant configuration files in this vocabulary.
var configKeys = map[string]string{
	"SAC_STATE_DIR":          "state-dir",
	"SAC_TENANT_ID":          "tenant-id",
	"SAC_DEVICE_ENDPOINT":    "device-endpoint",
	"SAC_DEPLOYMENT_KEY":     "deployment-key",
	"SAC_CA_FILE":            "ca-file",
	"SAC_POLICY_KEY":         "policy-key",
	"SAC_POLICY_KEY_ID":      "policy-key-id",
	"SAC_CLASSIFIER_RELEASE": "classifier-release",
	"SAC_CLASSIFIER_PUBKEY":  "classifier-pubkey",
	"SAC_DEVICE_IDENTITY":    "device-identity",
	"SAC_LOG_LEVEL":          "log-level",
}

// runMode is what the command line asked for besides running the service.
type runMode struct {
	showVersion bool
	printConfig bool
}

// parseFlags reads the configuration files named by --config-file, in order, then the remaining
// flags. A later file wins over an earlier one, and a flag on the command line wins over both.
func parseFlags(args []string) (Config, runMode, error) {
	cfg := Config{PolicyKeyID: "policy-key-1", DeviceIdentity: string(protocol.DeviceIdentityClear), LogLevel: "info"}
	var mode runMode
	var configFiles []string

	fs := flag.NewFlagSet("capture-core", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "protected state directory (spool, keys, credential, content, policy cache, device CA)")
	fs.StringVar(&cfg.TenantID, "tenant-id", cfg.TenantID, "the tenant this device belongs to")
	fs.StringVar(&cfg.DeviceEndpoint, "device-endpoint", cfg.DeviceEndpoint, "device edge base URL, https with no path")
	fs.StringVar(&cfg.DeploymentKey, "deployment-key", cfg.DeploymentKey, "the tenant's deployment key; enrols the device")
	fs.StringVar(&cfg.CAFile, "ca-file", cfg.CAFile, "PEM CA set trusted for the device endpoint in addition to the system roots")
	fs.StringVar(&cfg.PolicyKey, "policy-key", cfg.PolicyKey, "hex-encoded Ed25519 public key the policy bundle must verify under")
	fs.StringVar(&cfg.PolicyKeyID, "policy-key-id", cfg.PolicyKeyID, "key id the policy bundle must name")
	fs.StringVar(&cfg.ClassifierRelease, "classifier-release", cfg.ClassifierRelease, "signed classifier release directory")
	fs.StringVar(&cfg.ClassifierPubkey, "classifier-pubkey", cfg.ClassifierPubkey, "hex-encoded Ed25519 public key the classifier release must verify under")
	fs.StringVar(&cfg.DeviceIdentity, "device-identity", cfg.DeviceIdentity, "device identity setting until the server states it: clear | hashed")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "debug | info | warn | error")
	fs.Func("config-file", "KEY=VALUE configuration file (repeatable; a later file wins, command-line flags win over all)", func(v string) error {
		configFiles = append(configFiles, v)
		return nil
	})
	fs.BoolVar(&mode.showVersion, "version", false, "print the version and exit")
	fs.BoolVar(&mode.printConfig, "print-config", false, "print the resolved configuration and the device's state, then exit")

	if err := fs.Parse(args); err != nil {
		return cfg, mode, err
	}
	if fs.NArg() > 0 {
		return cfg, mode, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// Apply the files, then re-apply the command line so it wins.
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for _, path := range configFiles {
		values, err := readConfigFile(path)
		if err != nil {
			return cfg, mode, err
		}
		for _, kv := range values {
			if explicit[kv[0]] {
				continue
			}
			if err := fs.Set(kv[0], kv[1]); err != nil {
				return cfg, mode, fmt.Errorf("config file %s: %s: %w", path, kv[0], err)
			}
		}
	}
	if mode.showVersion {
		return cfg, mode, nil
	}
	return cfg, mode, cfg.validate()
}

// readConfigFile reads KEY=VALUE lines and returns (flag name, value) pairs in file order. Blank
// lines and lines starting with '#' are ignored; the value is everything after the first '=', so a
// path with spaces needs no quoting. An unknown key is an error, so a typo cannot look configured.
func readConfigFile(path string) ([][2]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	defer f.Close()
	var out [][2]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if line == 1 {
			s = strings.TrimPrefix(s, "\ufeff")
		}
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		key, value, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("config file %s:%d: line is not KEY=VALUE", path, line)
		}
		name, known := configKeys[strings.TrimSpace(key)]
		if !known {
			return nil, fmt.Errorf("config file %s:%d: unknown key %q", path, line, strings.TrimSpace(key))
		}
		out = append(out, [2]string{name, strings.TrimSpace(value)})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config file %s: %w", path, err)
	}
	return out, nil
}

func (c Config) validate() error {
	switch {
	case strings.TrimSpace(c.StateDir) == "":
		return errors.New("--state-dir (SAC_STATE_DIR) is required")
	case strings.TrimSpace(c.TenantID) == "":
		return errors.New("--tenant-id (SAC_TENANT_ID) is required; it comes from the tenant file")
	case strings.TrimSpace(c.DeviceEndpoint) == "":
		return errors.New("--device-endpoint (SAC_DEVICE_ENDPOINT) is required; it comes from the tenant file")
	}
	u, err := url.Parse(c.DeviceEndpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return fmt.Errorf("--device-endpoint %q must be an https URL with a host and no path", c.DeviceEndpoint)
	}
	if c.PolicyKey != "" {
		if k, err := hex.DecodeString(strings.TrimSpace(c.PolicyKey)); err != nil || len(k) != ed25519.PublicKeySize {
			return errors.New("--policy-key must be a hex-encoded 32-byte Ed25519 public key")
		}
	}
	if (c.ClassifierRelease == "") != (c.ClassifierPubkey == "") {
		return errors.New("--classifier-release and --classifier-pubkey go together: a release with no pinned key cannot be verified")
	}
	if !protocol.DeviceIdentity(c.DeviceIdentity).Valid() {
		return fmt.Errorf("--device-identity %q must be clear or hashed", c.DeviceIdentity)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("--log-level %q must be debug, info, warn or error", c.LogLevel)
	}
	return nil
}

// usage is the help text for --help.
func usage(w io.Writer) {
	fmt.Fprint(w, `usage: capture-core --config-file FILE [--config-file FILE ...] [flags]
       capture-core --print-config --config-file FILE ...
       capture-core --version

Runs the Shadow AI Capture agent. Under the Windows Service Control Manager it runs as the
ShadowAICapture service; elsewhere it runs in the foreground until SIGINT or SIGTERM. Started by a
browser as a native-messaging host (with a chrome-extension:// origin argument), it relays the
browser's messages to the running service.

Configuration keys (files and flags):
  SAC_STATE_DIR          --state-dir          protected state directory (required)
  SAC_TENANT_ID          --tenant-id          tenant id (required, tenant file)
  SAC_DEVICE_ENDPOINT    --device-endpoint    device edge base URL (required, tenant file)
  SAC_DEPLOYMENT_KEY     --deployment-key     tenant deployment key (tenant file)
  SAC_CA_FILE            --ca-file            extra CA trusted for the device endpoint
  SAC_POLICY_KEY         --policy-key         Ed25519 policy verification key (hex)
  SAC_POLICY_KEY_ID      --policy-key-id      policy key id (default policy-key-1)
  SAC_CLASSIFIER_RELEASE --classifier-release signed classifier release directory
  SAC_CLASSIFIER_PUBKEY  --classifier-pubkey  Ed25519 classifier release key (hex)
  SAC_DEVICE_IDENTITY    --device-identity    clear | hashed (default clear)
  SAC_LOG_LEVEL          --log-level          debug | info | warn | error (default info)
`)
}
