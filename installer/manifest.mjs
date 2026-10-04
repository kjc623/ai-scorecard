// installer/manifest.mjs - the single source of truth for the development installer.
//
// The installer packages the endpoint exactly the way docs/05-platform-delivery.md §6 describes it:
// one signed artefact per platform carrying code only, with the per-tenant association delivered as
// configuration by the customer's MDM. This file is the *code* half - what is in the payload and
// which flags the agent resolves - so the Windows MSI, the macOS PKG and the Linux package cannot
// disagree about the binary name, the wrapper, or a configuration variable.
//
// Why a manifest rather than three hand-written installers: the three platform descriptions were
// three chances to typo `--device-endpoint` into `--deviceEndpoint`, and the acceptance pattern in
// this repository is "one source, generated output, a --check that fails on drift"
// (contracts/tools/generate.mjs is the model). `node installer/render.mjs --check` is the same idea
// applied to the service definitions.
//
// What this file does NOT decide: the values. A value is a per-tenant fact (the docs call it the
// enrolment profile) and is delivered by the customer's MDM, not baked into the artefact. The
// committed render therefore carries only placeholders and layout, never a tenant, a token or a key.

/** The installed layout, per platform. Absolute paths only: a service manager resolves nothing. */
export const LAYOUT = {
  linux: {
    prefix: '/opt/shadow-ai-capture',
    bindir: '/opt/shadow-ai-capture/bin',
    configdir: '/etc/shadow-ai-capture',
    statedir: '/var/lib/shadow-ai-capture',
    logdir: '/var/log/shadow-ai-capture',
    configFile: '/etc/shadow-ai-capture/capture-core.env',
    serviceFile: '/etc/systemd/system/shadow-ai-capture.service',
    serviceName: 'shadow-ai-capture.service',
  },
  darwin: {
    prefix: '/usr/local/opt/shadow-ai-capture',
    bindir: '/usr/local/opt/shadow-ai-capture/bin',
    configdir: '/usr/local/etc/shadow-ai-capture',
    statedir: '/var/db/shadow-ai-capture',
    logdir: '/var/log/shadow-ai-capture',
    configFile: '/usr/local/etc/shadow-ai-capture/capture-core.env',
    serviceFile: '/Library/LaunchDaemons/com.shadowaicapture.capture-core.plist',
    serviceName: 'com.shadowaicapture.capture-core',
  },
  windows: {
    prefix: 'C:\\Program Files\\ShadowAICapture',
    bindir: 'C:\\Program Files\\ShadowAICapture\\bin',
    configdir: 'C:\\ProgramData\\ShadowAICapture',
    statedir: 'C:\\ProgramData\\ShadowAICapture\\state',
    logdir: 'C:\\ProgramData\\ShadowAICapture\\log',
    configFile: 'C:\\ProgramData\\ShadowAICapture\\capture-core.env',
    serviceFile: 'ShadowAICapture.msi',
    serviceName: 'ShadowAICapture',
  },
};

/**
 * The payload: the two device binaries, the wrapper each service manager invokes, and the
 * configuration template. `dest` is relative to `bindir` (or `configdir` when stated), and
 * `from` is relative to the repository root once `build.mjs` has staged it. The classifier host is
 * a second binary, not a library, because ADR 0016 compiles one Go source to native and wasm.
 */
export const PAYLOAD = [
  { name: 'capture-core', from: 'endpoint/capture-core/bin/capture-core', dest: 'bin', exe: true },
  { name: 'classifier-host', from: 'endpoint/classifier-host/bin/classifier-host', dest: 'bin', exe: true },
  { name: 'capture-core-run', from: '<generated>', dest: 'bin', exe: true, generated: true },
  { name: 'capture-core.env.example', from: '<generated>', dest: 'etc', generated: true },
];

/** Go modules built into the payload. `out` is where build.mjs leaves the binary. */
export const GO_BINARIES = [
  { name: 'capture-core', dir: 'endpoint/capture-core', pkg: './cmd/capture-core' },
  { name: 'classifier-host', dir: 'endpoint/classifier-host', pkg: './cmd/classifier-host' },
];

/**
 * The configuration schema. Every entry is a capture-core flag whose value is a per-tenant
 * enrolment-profile fact. The order here is the order in the generated env file, the wrapper, and
 * any documentation, so a reader comparing platforms is comparing the same list.
 *
 *   env       the installer's variable name (deployment vocabulary, SAC_*)
 *   flag      the capture-core flag it becomes; parsed from main.go by verify.mjs so a typo fails
 *   kind      string | path | bool | duration | int | enum | url | secret
 *   default   what the generated env file carries when the value is inert
 *   required  a value the agent refuses to start without (mirrors Config.validate)
 *   secret    written 0600 and never echoed; the enrolment token is the only one today
 *   platforms all three unless restricted
 */
export const CONFIG = [
  // Identity and storage.
  { env: 'SAC_TENANT_ID', flag: '--tenant-id', kind: 'string', default: '', required: true, mustFill: true, desc: 'tenant stamped on every envelope (from enrolment, docs/01 §13.1)' },
  { env: 'SAC_DEVICE_ID', flag: '--device-id', kind: 'string', default: '', required: true, mustFill: true, desc: 'device stamped on every envelope; must match the issued credential' },
  { env: 'SAC_USER_REF', flag: '--user-ref', kind: 'string', default: 'device-user', desc: 'pseudonymous subject reference, never a name or e-mail' },
  { env: 'SAC_POPULATION', flag: '--population', kind: 'string', default: '', desc: 'user population for scope resolution (may be empty)' },
  { env: 'SAC_SPOOL_DIR', flag: '--spool-dir', kind: 'path', default: '<statedir>/spool', required: true, desc: "the agent's only durable store; opened before any provider starts" },
  { env: 'SAC_SPOOL_KEY', flag: '--spool-key', kind: 'path', default: '<statedir>/spool.key', required: true, desc: 'spool key file; must be OUTSIDE the spool directory' },
  { env: 'SAC_SPOOL_BOUNDS', flag: '--spool-bounds', kind: 'enum', values: ['default', 'dev'], default: 'default', desc: 'spool bound profile; dev is small so drop-oldest is reachable' },
  { env: 'SAC_RETENTION', flag: '--retention', kind: 'duration', default: '720h', desc: 'device-side retention for spooled observations' },
  { env: 'SAC_HEALTH_FILE', flag: '--health-file', kind: 'path', default: '<statedir>/health.jsonl', desc: 'append the health channel to this file as JSON lines; empty disables it' },

  // Policy.
  { env: 'SAC_BUNDLE', flag: '--bundle', kind: 'path', default: '', desc: 'signed policy bundle; empty runs at M0 (metadata only)' },
  { env: 'SAC_POLICY_KEY', flag: '--policy-key', kind: 'string', default: '', desc: 'hex-encoded Ed25519 public key the bundle must verify under' },
  { env: 'SAC_POLICY_KEY_ID', flag: '--policy-key-id', kind: 'string', default: 'policy-key-1', desc: 'key id the bundle must name' },

  // Classifier host.
  { env: 'SAC_CLASSIFIER_ADDRESS', flag: '--classifier-address', kind: 'string', default: '', desc: 'transport:path of the classifier host; empty means rules-only' },
  { env: 'SAC_CLASSIFIER_BUDGET', flag: '--classifier-budget', kind: 'duration', default: '2s', desc: 'budget for one classification' },
  { env: 'SAC_CLASSIFIER_RELEASE', flag: '--classifier-release', kind: 'path', default: '', desc: 'signed classifier release directory; with no SAC_CLASSIFIER_ADDRESS the agent runs the installed classifier-host as a child' },
  { env: 'SAC_CLASSIFIER_PUBKEY', flag: '--classifier-pubkey', kind: 'string', default: '', desc: 'hex-encoded Ed25519 public key the classifier release must verify under' },

  // The M3 local content store (docs/01 §11.3). Empty by default: a device holds content only when
  // its enrolment profile gives it somewhere to hold it.
  { env: 'SAC_CONTENT_DIR', flag: '--content-dir', kind: 'path', default: '', desc: 'M3 local content store; empty means the device holds no content and refuses M3 observations' },
  { env: 'SAC_CONTENT_KEY', flag: '--content-key', kind: 'path', default: '', desc: 'key file the content store is sealed under; must be OUTSIDE SAC_CONTENT_DIR' },

  // Providers.
  { env: 'SAC_PROXY_TLS', flag: '--proxy-tls', kind: 'bool', default: 'true', desc: 'run proxy.tls (the egress interceptor)' },
  { env: 'SAC_PROXY_TLS_LISTEN', flag: '--proxy-tls-listen', kind: 'string', default: '127.0.0.1:0', desc: 'proxy.tls listen address' },
  { env: 'SAC_PROXY_TLS_CANARY', flag: '--proxy-tls-canary', kind: 'string', default: '', desc: 'canary host:port for the end-to-end probe; empty reports degraded' },
  { env: 'SAC_PROXY_LOOPBACK', flag: '--proxy-loopback', kind: 'bool', default: 'true', desc: "run proxy.loopback using the bundle's port map" },
  { env: 'SAC_PROC_DETECT', flag: '--proc-detect', kind: 'bool', default: 'false', desc: 'run proc.detect (needs an enumerator)' },
  { env: 'SAC_DRAIN_DEADLINE', flag: '--drain-deadline', kind: 'duration', default: '30s', desc: 'bounded spool drain at shutdown' },
  { env: 'SAC_ATTACHMENT_CAP', flag: '--attachment-cap', kind: 'int', default: '67108864', desc: 'policy cap for one attachment manifest, checked before any byte moves' },

  // Trust/CA and the CLI trust shim (docs/01 §4.5, §5.2, §14). TrustInstall defaults false: the
  // agent never touches the OS trust store unless the enrolment profile asks it to, because the
  // wrong store fails silently (E7).
  { env: 'SAC_TRUST_INSTALL', flag: '--trust-install', kind: 'bool', default: 'false', desc: 'install the per-device root CA into the platform trust store' },
  { env: 'SAC_TRUST_STORE', flag: '--trust-store', kind: 'enum', values: ['root', 'enterprise'], default: 'root', desc: 'Windows trust store; other platforms ignore it' },
  { env: 'SAC_TRUST_REMOVE_ON_STOP', flag: '--trust-remove-on-stop', kind: 'bool', default: 'false', desc: 'remove the root CA on shutdown (uninstall or kill switch)' },
  { env: 'SAC_CA_KEY', flag: '--ca-key', kind: 'path', default: '', desc: 'per-device CA private key PEM (0600); empty generates an ephemeral CA' },
  { env: 'SAC_CA_CERT', flag: '--ca-cert', kind: 'path', default: '', desc: "per-device CA public cert PEM; empty uses the bundle's interception.root_ca_pem" },
  { env: 'SAC_CLI_SHIM', flag: '--cli-shim', kind: 'bool', default: 'false', desc: 'run cli.shim (managed shell trust/proxy environment for CLI runtimes)' },
  { env: 'SAC_SHIM_DIR', flag: '--shim-dir', kind: 'path', default: '', desc: 'directory cli.shim writes the CA bundle and profile into; empty uses a platform default' },

  // Device-to-cloud drain (ADR 0020). Empty SAC_DEVICE_ENDPOINT disables it.
  { env: 'SAC_DEVICE_ENDPOINT', flag: '--device-endpoint', kind: 'url', default: '', desc: 'device ingress base URL, e.g. https://ingest.eu.example.com; empty disables the drain' },
  { env: 'SAC_AUTH_MODE', flag: '--auth-mode', kind: 'enum', values: ['x509', 'dpop'], default: '', desc: 'device credential mode for the drain' },
  { env: 'SAC_CREDENTIAL_FILE', flag: '--credential-file', kind: 'path', default: '<statedir>/credential.sealed', desc: 'sealed device credential issued by POST /v1/enrol; must NOT be inside SAC_SPOOL_DIR' },
  { env: 'SAC_ENROLMENT_TOKEN', flag: '--enrolment-token', kind: 'secret', default: '', mustFill: true, desc: 'single-use bootstrap token from the MDM enrolment profile' },
  { env: 'SAC_CA_FILE', flag: '--ca-file', kind: 'path', default: '', desc: 'PEM CA set the edge is pinned to; empty uses the system roots' },
  { env: 'SAC_MDM_ID', flag: '--mdm-id', kind: 'string', default: '', desc: 'MDM-delivered device identifier; the preferred hardware-identity seed' },
  { env: 'SAC_BACKOFF_BASE', flag: '--backoff-base', kind: 'duration', default: '1s', desc: 'drain retry backoff base (full jitter)' },
  { env: 'SAC_BACKOFF_CAP', flag: '--backoff-cap', kind: 'duration', default: '300s', desc: 'drain retry backoff cap' },

  // Logging.
  { env: 'SAC_LOG_LEVEL', flag: '--log-level', kind: 'enum', values: ['debug', 'info', 'warn', 'error'], default: 'info', desc: 'log level' },
  { env: 'SAC_LOG_FORMAT', flag: '--log-format', kind: 'enum', values: ['json', 'text'], default: 'json', desc: 'log format' },
];

/** Replace the layout placeholders an entry's default may carry, in the layout's native separators. */
export function expandDefault(entry, layout) {
  const value = String(entry.default).replace('<statedir>', layout.statedir);
  // The defaults are written with forward slashes (`<statedir>/spool`); a Windows layout expands to
  // `C:\ProgramData\...`, so normalise the separators once here rather than emitting a mixed path.
  return layout.statedir.includes('\\') ? value.replace(/\//g, '\\') : value;
}

/** The flags a wrapper renders, in CONFIG order. Secrets default to empty and are never required. */
export function configFor(os) {
  return CONFIG.filter((c) => !c.platforms || c.platforms.includes(os));
}

/** A stable, human-readable product version, distinct from the agent's compiled version. */
export const PRODUCT = {
  name: 'ShadowAICapture',
  displayName: 'Shadow AI Capture',
  manufacturer: 'Shadow AI Capture',
  // The MSI ProductCode is regenerated per release by Build-Msi.ps1; this is the UpgradeCode, which
  // is stable across versions so major upgrades replace rather than stack (docs/05 §6.1).
  upgradeCode: '7E9C2B7A-6D0E-4C6A-9F2B-1A6E6C2D44A1',
  version: '0.1.0',
};
