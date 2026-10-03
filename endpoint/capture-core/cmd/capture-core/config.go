package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
)

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

	// Policy.
	BundlePath  string
	PolicyKey   string
	PolicyKeyID string

	// Classifier host (§3.4).
	ClassifierAddress string
	ClassifierBudget  time.Duration

	// Providers.
	EnableTLS        bool
	TLSListen        string
	TLSCanary        string
	EnableLoopback   bool
	EnableProcDetect bool
	DrainDeadline    time.Duration

	// Health channel.
	HealthFile     string
	HealthInterval time.Duration

	// Native messaging.
	AttachmentCap int64

	// Modes and misc.
	WorkDir string

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
		if strings.TrimSpace(c.DeviceID) == "" {
			return errors.New("--device-id is required: every envelope carries a device id and health is keyed by it")
		}
		if strings.TrimSpace(c.TenantID) == "" {
			return errors.New("--tenant-id is required: a device with no tenant cannot attribute an observation")
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
	if mode.nativeFrames != "" {
		if st, err := os.Stat(mode.nativeFrames); err != nil || !st.IsDir() {
			return fmt.Errorf("--native-frames %q is not a directory", mode.nativeFrames)
		}
	}
	if c.AttachmentCap <= 0 {
		return errors.New("--attachment-cap must be positive")
	}
	return nil
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
func (c Config) identity() core.Identity {
	return core.Identity{TenantID: c.TenantID, DeviceID: c.DeviceID, UserRef: c.UserRef}
}

func (c Config) scopeQuery(tool string) core.ScopeQuery {
	return core.ScopeQuery{
		ToolFingerprint: tool,
		Population:      c.Population,
		DeviceID:        c.DeviceID,
		UserRef:         c.UserRef,
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
	fmt.Printf("identity:        tenant=%s device=%s user=%s population=%q\n", cfg.TenantID, cfg.DeviceID, cfg.UserRef, cfg.Population)
	fmt.Printf("spool:           dir=%s key=%s bounds=%s retention=%s\n", cfg.SpoolDir, cfg.SpoolKey, cfg.SpoolBoundsProfile, cfg.Retention)

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

	if cfg.ClassifierAddress == "" {
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
		cfg.EnableTLS, cfg.TLSListen, cfg.TLSCanary, cfg.EnableLoopback, cfg.EnableProcDetect)
	if !cfg.EnableProcDetect {
		fmt.Printf("named gap:       proc.detect is not started; the route has no coverage row and §4.4 detection does not run\n")
	}
	fmt.Printf("native messaging: 4-byte little-endian length prefix on stdin/stdout (Chromium's framing, NOT protocol's local-socket framing)\n")
	fmt.Printf("health channel:  file=%q interval=%s\n", cfg.HealthFile, cfg.HealthInterval)
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
