package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// defaultTLSListen is the "ask the OS for a port" default. A service pins a real port in its
// enrolment profile; a test leaves this and gets an ephemeral one. It is a named constant because
// the bundle's interception.proxy_listen only applies when the flag was left at this value.
const defaultTLSListen = "127.0.0.1:0"

// Config is everything the binary resolves before it starts anything. Every field is either a flag
// or a value read from the signed bundle; none of it is policy the binary invents, and none of it
// has a default that widens what the agent may do.
type Config struct {
	// Identity and storage.
	SpoolDir           string
	SpoolKey           string
	SpoolBoundsProfile string
	TenantID           string
	DeviceID           string
	UserRef            string
	Population         string
	Retention          string

	// Device identity (ADR 0021). Hostname and SubjectName are the clear identity fields; empty
	// means "resolve from the operating system". ManagedState is the agent's report, because there
	// is no MDM resolver in this build. DeviceIdentity is the tenant setting the device acts on:
	// 'clear' sends the hostname and subject name, 'hashed' sends neither. It defaults to 'clear'
	// (the product default) and is refreshed from the server's enrolment and health responses.
	Hostname       string
	SubjectName    string
	ManagedState   string
	DeviceIdentity string

	// Policy.
	BundlePath  string
	PolicyKey   string
	PolicyKeyID string

	// Classifier host (§3.4).
	ClassifierAddress string
	ClassifierBudget  time.Duration
	// ClassifierRelease/ClassifierPubkey make the agent run the classifier host itself, as a child
	// on stdio, when no ClassifierAddress names a host someone else runs.
	ClassifierRelease string
	ClassifierPubkey  string

	// The M3 local content store (§11.3). Empty ContentDir means the device holds no content, and
	// an M3 observation is refused rather than emitted without the content it says it holds.
	ContentDir string
	ContentKey string

	// Providers.
	EnableTLS        bool
	TLSListen        string
	TLSCanary        string
	EnableLoopback   bool
	EnableProcDetect bool
	DrainDeadline    time.Duration

	// Trust/CA and the CLI trust shim (docs/01-collectors.md §4.5, §5.2, §14).
	// TrustInstall defaults false: the agent never touches the OS trust store unless the
	// enrolment profile asks it to (§5.2's wrong-store rule makes a silent install worse than
	// none). CAKeyFile/CACertFile pin the per-device CA so it survives a restart and the trust
	// entry stays valid; with neither, the interceptor generates an ephemeral CA as before.
	TrustInstall      bool
	TrustStore        string // windows: root | enterprise
	TrustRemoveOnStop bool
	CAKeyFile         string
	CACertFile        string
	CLIShim           bool
	ShimDir           string

	// Health channel.
	HealthFile     string
	HealthInterval time.Duration

	// Native messaging.
	AttachmentCap int64

	// Device-to-cloud drain (ADR 0020). Empty DeviceEndpoint means the drain is disabled: the
	// shutdown drain reports what is still spooled and stops at its deadline, exactly as before.
	DeviceEndpoint string
	AuthMode       string // "x509" | "dpop"; empty when the drain is disabled
	CredentialFile string // path to the sealed device credential
	EnrolmentToken string // single-use bootstrap token for POST /v1/enrol
	CAFile         string // PEM CA set the edge is pinned to (empty = system roots)
	MDMID          string // MDM-delivered device identifier, the preferred hardware-identity seed
	BackoffBase    time.Duration
	BackoffCap     time.Duration
	DrainInterval  time.Duration // background drain poll interval (default 1s; the selftest lengthens it)

	// Modes and misc.
	WorkDir string
	// ServiceName is the Windows service name used with --service. It must match the name the
	// installer registered: the SCM dispatcher table and the control handler are keyed by it.
	ServiceName string

	// KeepWorkDir leaves the selftest's work directory in place for inspection. The default is to
	// remove it: a self test leaves the tree as it found it, including on the failure path.
	KeepWorkDir bool
	LogFormat   string
	LogLevel    string
	DryRun      bool
}

func (c Config) validate(mode runMode) error {
	// The native-messaging mode still needs identity (it mints envelopes) and storage (it writes
	// them); --print-config and --version do not. --selftest supplies its own work directory, spool,
	// bundle and identity, so it validates only what it was given.
	needsStorage := mode.nativeHost || mode.nativeFrames != "" || (!mode.showVersion && !mode.printConfig && !mode.selftest)
	if needsStorage {
		if strings.TrimSpace(c.SpoolDir) == "" {
			return errors.New("--spool-dir is required: a provider with nowhere to write must not start (§3.5 step 2)")
		}
		if strings.TrimSpace(c.SpoolKey) == "" {
			return errors.New("--spool-key is required: the spool never generates a key inside its own directory (§12)")
		}
		// The flags are the identity only for a local/offline run (no --device-endpoint): with a
		// drain configured the sealed credential (or a bounded enrolment) is authoritative, and the
		// flags are a validated assertion rather than a requirement.
		if strings.TrimSpace(c.DeviceEndpoint) == "" {
			if strings.TrimSpace(c.DeviceID) == "" {
				return errors.New("--device-id is required for a local run (no --device-endpoint): every envelope carries a device id and health is keyed by it")
			}
			if strings.TrimSpace(c.TenantID) == "" {
				return errors.New("--tenant-id is required for a local run (no --device-endpoint): a device with no tenant cannot attribute an observation")
			}
		}
		if _, err := time.ParseDuration(c.Retention); err != nil {
			return fmt.Errorf("--retention %q: %w", c.Retention, err)
		}
	}
	if (c.BundlePath == "") != (c.PolicyKey == "") {
		return errors.New("--bundle and --policy-key go together: a bundle with no pinned key cannot be verified, and a key with no bundle verifies nothing")
	}
	if c.PolicyKey != "" {
		if _, err := hex.DecodeString(strings.TrimSpace(c.PolicyKey)); err != nil {
			return fmt.Errorf("--policy-key must be hex-encoded Ed25519 public key bytes: %w", err)
		}
	}
	if (c.ClassifierRelease == "") != (c.ClassifierPubkey == "") {
		return errors.New("--classifier-release and --classifier-pubkey go together: a release with no pinned key cannot be verified, and a key with no release verifies nothing")
	}
	if (c.ContentDir == "") != (c.ContentKey == "") {
		return errors.New("--content-dir and --content-key go together: held content is sealed, and the key must live outside the directory it seals")
	}
	if mode.nativeFrames != "" {
		if st, err := os.Stat(mode.nativeFrames); err != nil || !st.IsDir() {
			return fmt.Errorf("--native-frames %q is not a directory", mode.nativeFrames)
		}
	}
	if c.AttachmentCap <= 0 {
		return errors.New("--attachment-cap must be positive")
	}
	if err := c.validateDrain(); err != nil {
		return err
	}
	switch c.TrustStore {
	case "", "root", "enterprise":
	default:
		return fmt.Errorf("--trust-store %q must be root or enterprise", c.TrustStore)
	}
	return nil
}

// validateDrain checks the device-to-cloud drain configuration. An empty --device-endpoint means
// the drain is disabled and nothing else is required; a non-empty one must be a usable https URL
// with a credential file, a closed auth mode, and sane backoff bounds.
func (c Config) validateDrain() error {
	if strings.TrimSpace(c.DeviceEndpoint) == "" {
		return nil
	}
	u, err := url.Parse(c.DeviceEndpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("--device-endpoint %q must be an https URL with a host", c.DeviceEndpoint)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("--device-endpoint %q must not carry a path: the gateway route map and the DPoP htu are the bare host", c.DeviceEndpoint)
	}
	switch c.AuthMode {
	case "x509", "dpop":
	default:
		return fmt.Errorf("--auth-mode %q must be x509 or dpop", c.AuthMode)
	}
	if strings.TrimSpace(c.CredentialFile) == "" {
		return errors.New("--credential-file is required when --device-endpoint is set: the issued credential has nowhere to be sealed")
	}
	if strings.TrimSpace(c.SpoolDir) != "" {
		if absCred, err := filepath.Abs(c.CredentialFile); err == nil {
			if absSpool, err := filepath.Abs(c.SpoolDir); err == nil && withinDir(absSpool, absCred) {
				return errors.New("--credential-file must not be inside --spool-dir: a credential beside the spool it shares a key with is not sealing at rest")
			}
		}
	}
	if c.BackoffBase <= 0 {
		return errors.New("--backoff-base must be positive")
	}
	if c.BackoffCap <= 0 {
		return errors.New("--backoff-cap must be positive")
	}
	if c.BackoffBase > c.BackoffCap {
		return fmt.Errorf("--backoff-base %s exceeds --backoff-cap %s", c.BackoffBase, c.BackoffCap)
	}
	return nil
}

// withinDir reports whether path is inside dir (or equal to it), comparing cleaned absolute paths.
func withinDir(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if strings.EqualFold(dir, path) {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// resolvedPolicy is the bundle as the agent will enforce it, plus the resolution of the modes an
// operator asks about in --print-config.
type resolvedPolicy struct {
	store   *policy.Store
	bundle  *policy.Bundle
	result  policy.Result
	loadErr error
}

// loadPolicy verifies the bundle at path and puts it in force. A failure is not fatal: §13.3 keeps
// the previous bundle (there is none on a fresh start) and falls to M0, which is a reduction in
// capability and is reported rather than hidden.
func loadPolicy(cfg Config, refs policy.ArtefactResolver) (*policy.Store, policy.Result, error) {
	pub, err := hex.DecodeString(strings.TrimSpace(cfg.PolicyKey))
	if err != nil {
		return nil, policy.Result{}, fmt.Errorf("decoding --policy-key: %w", err)
	}
	verifier, err := policy.NewVerifier(cfg.PolicyKeyID, pub)
	if err != nil {
		return nil, policy.Result{}, err
	}
	store, err := policy.NewStore(verifier, refs)
	if err != nil {
		return nil, policy.Result{}, err
	}
	raw, err := os.ReadFile(cfg.BundlePath)
	if err != nil {
		return nil, policy.Result{}, fmt.Errorf("reading bundle: %w", err)
	}
	return store, store.Apply(raw), nil
}

// spoolBounds maps the profile flag onto the spool's bound. The default is §12's A16 shape
// (~25 MB / ~25,000 rows); "dev" is deliberately small so a local run reaches the bound and shows
// drop-oldest rather than pretending the bound does not exist.
func spoolBounds(profile string) (int64, int) {
	switch profile {
	case "dev":
		return 1 << 20, 200
	default:
		return 25 << 20, 25000
	}
}

// buildIdentity is the enrolment result the agent stamps on envelopes. On a real device it comes
// from §13.1's enrolment; here it is configuration, and an empty tenant is refused by validate.
// SubjectName is present only while the device-identity setting is 'clear' (ADR 0021).
func (c Config) identity() core.Identity {
	return core.Identity{
		TenantID:    c.TenantID,
		DeviceID:    c.DeviceID,
		UserRef:     c.UserRef,
		SubjectName: c.clearSubjectName(),
	}
}

// clearSubjectName is the account name to stamp on envelopes, or empty when the device-identity
// setting is 'hashed'. The setting gates what the device sends; control-api is authoritative and
// drops a clear value a stale device still sends, so the two layers agree.
func (c Config) clearSubjectName() string {
	if c.deviceIdentityMode() != protocol.DeviceIdentityClear {
		return ""
	}
	return c.resolvedSubjectName()
}

// deviceIdentityMode is the setting the device acts on, defaulting to 'clear' (the product default)
// when nothing configured one. Anything other than the exact 'hashed' value is treated as clear and
// the server corrects it on the next response.
func (c Config) deviceIdentityMode() protocol.DeviceIdentity {
	if strings.TrimSpace(c.DeviceIdentity) == string(protocol.DeviceIdentityHashed) {
		return protocol.DeviceIdentityHashed
	}
	return protocol.DeviceIdentityClear
}

// resolvedHostname is the clear machine name: the configured value, else the OS hostname, else
// empty. Empty is reported as absent and never as a placeholder.
func (c Config) resolvedHostname() string {
	if h := strings.TrimSpace(c.Hostname); h != "" {
		return h
	}
	if h, err := os.Hostname(); err == nil {
		return strings.TrimSpace(h)
	}
	return ""
}

// clearHostname is the machine name to send, or empty when the device-identity setting is 'hashed'
// (ADR 0021).
func (c Config) clearHostname() string {
	if c.deviceIdentityMode() != protocol.DeviceIdentityClear {
		return ""
	}
	return c.resolvedHostname()
}

// hostnameHash is the hashed machine name to send when the device-identity setting is 'hashed'.
// Lowercased before hashing so two spellings of one machine collapse, and prefixed like every other
// digest in the system. Empty when the setting is 'clear' or no hostname could be resolved.
func (c Config) hostnameHash() string {
	if c.deviceIdentityMode() != protocol.DeviceIdentityHashed {
		return ""
	}
	h := c.resolvedHostname()
	if h == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(h)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// resolvedSubjectName is the clear account name: the configured value, else the OS user, else a
// conventional environment variable. It is the machine's interactive account, not proof of which
// process opened a given connection; the report says so plainly.
func (c Config) resolvedSubjectName() string {
	if n := strings.TrimSpace(c.SubjectName); n != "" {
		return n
	}
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return strings.TrimSpace(u.Username)
	}
	for _, k := range []string{"USERNAME", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// managedState is the agent's report, defaulting to 'unknown' because there is no MDM resolver in
// this build and 'unknown' is not 'unmanaged'.
func (c Config) managedState() protocol.ManagedState {
	if m := protocol.ManagedState(strings.TrimSpace(c.ManagedState)); m.Valid() {
		return m
	}
	return protocol.ManagedStateUnknown
}

func (c Config) scopeQuery(tool string) core.ScopeQuery {
	return core.ScopeQuery{
		ToolFingerprint: tool,
		Population:      c.Population,
		DeviceID:        c.DeviceID,
		UserRef:         c.UserRef,
		SubjectName:     c.clearSubjectName(),
	}
}

// classifierAddress parses "transport:path" into the address device/protocol expects. It is
// deliberately explicit about the transport: the frame format is identical on both, but a wrong
// choice fails at connect time rather than silently talking to nothing.
func classifierAddress(s string) (classifierlinkAddress, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return classifierlinkAddress{}, errNoClassifierAddress
	}
	for _, transport := range []string{"unix:", "pipe:", "tcp:"} {
		if strings.HasPrefix(s, transport) {
			return classifierlinkAddress{Network: strings.TrimSuffix(transport, ":"), Path: strings.TrimPrefix(s, transport)}, nil
		}
	}
	return classifierlinkAddress{}, fmt.Errorf("--classifier-address %q: expected unix:PATH, pipe:NAME or tcp:127.0.0.1:PORT (loopback only)", s)
}

var errNoClassifierAddress = errors.New("no classifier address configured")

// classifierHostExe is the classifier host the installer lays down beside this binary.
func classifierHostExe() string {
	name := "classifier-host"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	self, err := os.Executable()
	if err != nil {
		return name
	}
	return filepath.Join(filepath.Dir(self), name)
}

// workDirFor returns the selftest's work directory, resolved against the process's working
// directory so it never lands in a system temp path the sandbox may deny.
// workDirFor returns the selftest's work directory. It never writes into the source tree by default:
// a run artifact inside the repository is a file somebody commits by accident (which happened once,
// with a spool key in it). Candidates are tried in order and the first writable one wins, because a
// sandbox can deny the system temp directory while allowing a workspace-scoped one.
func workDirFor(cfg Config) (string, error) {
	if strings.TrimSpace(cfg.WorkDir) != "" {
		return filepath.Abs(cfg.WorkDir)
	}
	candidates := []string{
		filepath.Join(os.TempDir(), "capture-core-selftest"),
		// The repository's own ignored scratch directory: present in .gitignore by policy, so even a
		// hard-killed run cannot leave something a person is tempted to commit.
		filepath.Join(repoRootGuess(), ".tools", "tmp", "capture-core-selftest"),
		// The harness's workspace temp directory, when it has pointed TMP/TEMP here.
		filepath.Join(repoRootGuess(), ".testtmp", "capture-core-selftest"),
		filepath.Join(".", ".selftest"), // last resort: gitignored, and removed on exit either way
	}
	var lastErr error
	for _, candidate := range candidates {
		if err := probeWritable(candidate); err != nil {
			lastErr = err
			continue
		}
		return filepath.Abs(candidate)
	}
	return "", fmt.Errorf("no writable work directory (last error: %w); pass --work-dir", lastErr)
}

// probeWritable creates the directory and a file in it, so the selftest discovers an unwritable
// location before it has started a service rather than half-way through one.
func probeWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// repoRootGuess walks up from the working directory looking for the repository markers the build
// environment uses (.tools, .git). It returns "." when it finds neither, which keeps the candidate
// list honest rather than inventing a path.
func repoRootGuess() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for i := 0; i < 6; i++ {
		for _, marker := range []string{".tools", ".git"} {
			if st, err := os.Stat(filepath.Join(dir, marker)); err == nil && st.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

// describeMode renders a resolved mode and the axes that produced it, so --print-config can explain
// an over-restriction instead of just stating it (§11.1's visibility requirement).
func describeMode(res core.Resolution) string {
	parts := make([]string, 0, len(res.Contributions))
	for _, c := range res.Contributions {
		key := c.Axis
		if c.Key != "" {
			key += "=" + c.Key
		}
		parts = append(parts, fmt.Sprintf("%s:%s", key, c.Mode))
	}
	out := string(res.Mode)
	if len(parts) > 0 {
		out += " (" + strings.Join(parts, ", ") + ")"
	}
	if len(res.Reasons) > 0 {
		out += " reasons=" + strings.Join(res.Reasons, ",")
	}
	return out
}

func modeOfTool(b *policy.Bundle, q core.ScopeQuery) core.Resolution {
	return core.Resolve(b, q)
}

// printConfig resolves everything and prints it. It starts nothing: this is the "what would this
// agent do" question answered without side effects.
func printConfig(cfg Config, logger loggerLike) error {
	fmt.Printf("capture-core %s\n", version)
	// The flags are the identity only for a local/offline run. With a drain configured the sealed
	// credential (or a bounded enrolment) is authoritative and the flags are a validated assertion.
	identitySource := "local"
	if strings.TrimSpace(cfg.DeviceEndpoint) != "" {
		identitySource = "credential"
	}
	fmt.Printf("identity:        tenant=%s device=%s user=%s population=%q source=%s\n", cfg.TenantID, cfg.DeviceID, cfg.UserRef, cfg.Population, identitySource)
	fmt.Printf("spool:           dir=%s key=%s bounds=%s retention=%s\n", cfg.SpoolDir, cfg.SpoolKey, cfg.SpoolBoundsProfile, cfg.Retention)

	var inForce *policy.Bundle
	if cfg.BundlePath == "" {
		fmt.Printf("policy:          no bundle configured -> M0 (metadata only, no content read; §13.3 rule 5)\n")
	} else {
		store, result, err := loadPolicy(cfg, policy.ArtefactResolverFunc(func(policy.ArtefactRef) error { return nil }))
		if err != nil {
			fmt.Printf("policy:          FAILED to load: %v\n", err)
			return err
		}
		fmt.Printf("policy:          bundle=%s key-id=%s outcome=%s cause=%q severity=%s\n", cfg.BundlePath, cfg.PolicyKeyID, result.Outcome, result.Cause, result.Severity)
		if result.Err != nil {
			fmt.Printf("policy error:    %v\n", result.Err)
		}
		if b := store.InForce(); b != nil {
			inForce = b
			fmt.Printf("policy version:  %s effective_at=%s actor=%s\n", b.Version, b.EffectiveAt.UTC().Format(time.RFC3339), b.Actor)
			fmt.Printf("tenant default:  %s\n", b.TenantDefault)
			fmt.Printf("classifier:      release=%s state=%s\n", b.Classifier.ReleaseID, b.Classifier.State)
			if b.RequiredNoticeVersion != "" {
				fmt.Printf("notice gate:     required version %s (users without it resolve to M0)\n", b.RequiredNoticeVersion)
			}
			for _, ks := range b.KillSwitches {
				fmt.Printf("kill switch:     provider=%s mode=%s reason=%s effective_at=%s\n", ks.Provider, ks.Mode, ks.ReasonCode, ks.EffectiveAt.UTC().Format(time.RFC3339))
			}
			fmt.Printf("interception:    tenant hosts=%v seed hosts=%v ports=%v body cap=%d promotion window=%ds\n",
				b.Interception.TenantHosts, b.Interception.SeedHosts, b.Interception.Ports, b.Interception.BodyCapBytes, b.Interception.PromotionWindowSeconds)
			for _, p := range b.Loopback.Ports {
				fmt.Printf("loopback port:   tool=%s holds=%d upstream=%d preflight=%s mode=%s\n", p.ToolFingerprint, p.Port, p.UpstreamPort, p.PreflightPath, p.Mode)
			}
			fmt.Printf("proc.detect:     signature version=%q image sigs=%d module sigs=%d port map=%d min compute=%d\n",
				b.ProcDetect.SignatureVersion, len(b.ProcDetect.ImageSignatures), len(b.ProcDetect.ModuleSignatures), len(b.ProcDetect.PortMap), b.ProcDetect.MinComputePermille)
			fmt.Printf("spool bounds:    max bytes=%d max rows=%d retention hours=%d\n", b.Spool.MaxBytes, b.Spool.MaxRows, b.Spool.DeviceRetentionHours)
			for _, a := range b.Artefacts {
				fmt.Printf("artefact:        %s path=%s digest=%s\n", a.Name, a.Path, a.Digest)
			}
			tools := map[string]bool{}
			for tool := range b.ToolModes {
				tools[tool] = true
			}
			for tool := range b.ClassPriors {
				tools[tool] = true
			}
			for tool := range tools {
				fmt.Printf("effective mode:  %-32s %s\n", tool, describeMode(modeOfTool(b, cfg.scopeQuery(tool))))
			}
		}
	}

	if cfg.ClassifierAddress == "" && cfg.ClassifierRelease != "" {
		fmt.Printf("classifier host: child on stdio exe=%s release=%s budget=%s\n", classifierHostExe(), cfg.ClassifierRelease, cfg.ClassifierBudget)
	} else if cfg.ClassifierAddress == "" {
		fmt.Printf("classifier host: none configured -> rules-only with confidence=degraded (§3.4)\n")
	} else {
		addr, err := classifierAddress(cfg.ClassifierAddress)
		if err != nil {
			fmt.Printf("classifier host: INVALID: %v\n", err)
		} else {
			fmt.Printf("classifier host: transport=%s path=%s budget=%s\n", addr.Network, addr.Path, cfg.ClassifierBudget)
		}
	}

	fmt.Printf("providers:       proxy.tls=%v (listen %s, canary %q) proxy.loopback=%v proc.detect=%v\n",
		cfg.EnableTLS, tlsListen(cfg, inForce), tlsCanary(cfg, inForce), cfg.EnableLoopback, cfg.EnableProcDetect)
	if cfg.TrustInstall {
		fmt.Printf("trust:           install=true store=%s remove_on_stop=%v ca_cert=%q ca_key=%q\n",
			cfg.TrustStore, cfg.TrustRemoveOnStop, cfg.CACertFile, cfg.CAKeyFile)
	} else {
		fmt.Printf("trust:           install=false (the OS trust store is not touched; §5.2)\n")
	}
	fmt.Printf("cli.shim:        enabled=%v dir=%q\n", cfg.CLIShim, cfg.ShimDir)
	if !cfg.EnableProcDetect {
		fmt.Printf("named gap:       proc.detect is not started; the route has no coverage row and §4.4 detection does not run\n")
	}
	fmt.Printf("native messaging: 4-byte little-endian length prefix on stdin/stdout (Chromium's framing, NOT protocol's local-socket framing)\n")
	fmt.Printf("health channel:  file=%q interval=%s\n", cfg.HealthFile, cfg.HealthInterval)
	if strings.TrimSpace(cfg.DeviceEndpoint) == "" {
		fmt.Printf("device drain:    disabled (no --device-endpoint); the shutdown drain reports what is still spooled\n")
	} else {
		fmt.Printf("device drain:    endpoint=%s auth=%s credential=%q ca=%q backoff=%s..%s\n",
			cfg.DeviceEndpoint, cfg.AuthMode, cfg.CredentialFile, cfg.CAFile, cfg.BackoffBase, cfg.BackoffCap)
	}
	fmt.Printf("local content:   M3 content store is not configured on this host; an M3 observation is refused rather than emitted without its content\n")
	_ = logger
	return nil
}

// loggerLike is the narrow logging seam the printout path takes, so printConfig can be called from
// the selftest with the same logger as the service.
type loggerLike interface{ Printf(string, ...any) }

// classifierlinkAddress mirrors classifierlink.Address without importing it into config.go's
// surface: the binary parses the string once, in one place.
type classifierlinkAddress struct {
	Network string
	Path    string
}

// healthReportShape documents the wire shape the health channel uses, so --print-config can state
// it and the selftest can assert it.
func healthReportShape() string {
	return "protocol.HealthReport rows (device_id, collector, state, detail, last_success_at, since, counters[7], version) plus spool stats"
}
