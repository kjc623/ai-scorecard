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
// What this file does NOT decide: the tenant's values. The installed configuration is two files read
// in order by repeated --config-file, the later winning: capture-core.env, the vendor-wide file the
// package installs (layout, trust anchors, product defaults; GENERIC below), and tenant.env, which
// the package copies from ShadowAICapture.tenant.env beside it (TENANT_PACKAGE below; control-api
// writes it per download). The committed render carries layout and placeholders only, never a
// tenant, a token or a key.

/** The installed layout, per platform. Absolute paths only: a service manager resolves nothing. */
export const LAYOUT = {
  linux: {
    prefix: '/opt/shadow-ai-capture',
    bindir: '/opt/shadow-ai-capture/bin',
    configdir: '/etc/shadow-ai-capture',
    statedir: '/var/lib/shadow-ai-capture',
    logdir: '/var/log/shadow-ai-capture',
    configFile: '/etc/shadow-ai-capture/capture-core.env',
    tenantFile: '/etc/shadow-ai-capture/tenant.env',
    classifierdir: '/opt/shadow-ai-capture/classifier',
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
    tenantFile: '/usr/local/etc/shadow-ai-capture/tenant.env',
    classifierdir: '/usr/local/opt/shadow-ai-capture/classifier',
    serviceFile: '/Library/LaunchDaemons/com.shadowaicapture.capture-core.plist',
    serviceName: 'com.shadowaicapture.capture-core',
  },
  windows: {
    prefix: 'C:\\Program Files\\ShadowAICapture',
    bindir: 'C:\\Program Files\\ShadowAICapture\\bin',
    configdir: 'C:\\ProgramData\\ShadowAICapture',
    statedir: 'C:\\ProgramData\\ShadowAICapture\\state',
    logdir: 'C:\\ProgramData\\ShadowAICapture\\log',
    // Both configuration files live in profile\, beside the files a lab profile points at.
    profiledir: 'C:\\ProgramData\\ShadowAICapture\\profile',
    configFile: 'C:\\ProgramData\\ShadowAICapture\\profile\\capture-core.env',
    tenantFile: 'C:\\ProgramData\\ShadowAICapture\\profile\\tenant.env',
    classifierdir: 'C:\\Program Files\\ShadowAICapture\\classifier',
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
 * The configuration schema. Every entry is a capture-core flag. The order here is the order in the
 * generated env file, the wrapper, and any documentation, so a reader comparing platforms is
 * comparing the same list.
 *
 *   env       the installer's variable name (deployment vocabulary, SAC_*)
 *   flag      the capture-core flag it becomes; parsed from main.go by verify.mjs so a typo fails
 *   kind      string | path | bool | duration | int | enum | url | secret
 *   default   what the generated env file carries when the value is inert
 *   required  a value the agent refuses to start without (mirrors Config.validate)
 *   secret    written 0600 and never echoed
 *   scope     who supplies the value. Absent means vendor: the generic capture-core.env the package
 *             installs may carry it. 'tenant' comes only from the tenant package file. 'lab' is an
 *             optional override (a lab profile, a local run) that neither the generic file nor a
 *             tenant package ever carries; the agent resolves it itself when it is empty.
 *   platforms all three unless restricted
 */
export const CONFIG = [
  // Identity and storage.
  { env: 'SAC_TENANT_ID', flag: '--tenant-id', kind: 'string', default: '', required: true, mustFill: true, scope: 'tenant', desc: 'tenant stamped on every envelope (from enrolment, docs/01 §13.1)' },
  { env: 'SAC_DEVICE_ID', flag: '--device-id', kind: 'string', default: '', scope: 'lab', desc: 'local/offline fallback only: with a drain the issued credential is the device identity' },
  { env: 'SAC_USER_REF', flag: '--user-ref', kind: 'string', default: '', scope: 'lab', desc: 'pseudonymous subject reference; empty derives it per user from the enrolled tenant key (a set value wins)' },
  { env: 'SAC_HOSTNAME', flag: '--hostname', kind: 'string', default: '', desc: 'clear machine name; empty resolves the OS hostname (ADR 0021)' },
  { env: 'SAC_SUBJECT_NAME', flag: '--subject-name', kind: 'string', default: '', desc: "clear account name on each submission; empty resolves the console user's UPN, else DOMAIN\\user (ADR 0021)" },
  { env: 'SAC_MANAGED_STATE', flag: '--managed-state', kind: 'enum', values: ['managed', 'unmanaged', 'unknown'], default: '', desc: 'whether the device is under MDM; empty reports managed when an Intune enrolment is found, else unknown' },
  { env: 'SAC_DEVICE_IDENTITY', flag: '--device-identity', kind: 'enum', values: ['clear', 'hashed'], default: 'clear', desc: "tenant identity setting to act on; 'hashed' sends no clear hostname or name (ADR 0021)" },
  { env: 'SAC_POPULATION', flag: '--population', kind: 'string', default: '', desc: 'user population for scope resolution (may be empty)' },
  { env: 'SAC_SPOOL_DIR', flag: '--spool-dir', kind: 'path', default: '<statedir>/spool', required: true, desc: "the agent's only durable store; opened before any provider starts" },
  { env: 'SAC_SPOOL_KEY', flag: '--spool-key', kind: 'path', default: '<statedir>/spool.key', required: true, desc: 'spool key file; must be OUTSIDE the spool directory' },
  { env: 'SAC_SPOOL_BOUNDS', flag: '--spool-bounds', kind: 'enum', values: ['default', 'dev'], default: 'default', desc: 'spool bound profile; dev is small so drop-oldest is reachable' },
  { env: 'SAC_RETENTION', flag: '--retention', kind: 'duration', default: '720h', desc: 'device-side retention for spooled observations' },
  { env: 'SAC_HEALTH_FILE', flag: '--health-file', kind: 'path', default: '<statedir>/health.jsonl', desc: 'append the health channel to this file as JSON lines; empty disables it' },

  // Policy.
  { env: 'SAC_BUNDLE', flag: '--bundle', kind: 'path', default: '', scope: 'lab', desc: 'a local signed policy bundle; empty fetches the bundle from the control plane after enrolment' },
  { env: 'SAC_POLICY_KEY', flag: '--policy-key', kind: 'string', default: '', desc: 'hex-encoded Ed25519 public key the bundle must verify under (the vendor trust anchor)' },
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
  { env: 'SAC_CA_KEY', flag: '--ca-key', kind: 'path', default: '', scope: 'lab', desc: 'per-device CA private key PEM (0600); empty lets the agent mint the device CA itself' },
  { env: 'SAC_CA_CERT', flag: '--ca-cert', kind: 'path', default: '', scope: 'lab', desc: 'per-device CA public cert PEM; empty, with SAC_CA_KEY empty, lets the agent mint the device CA itself' },
  { env: 'SAC_CLI_SHIM', flag: '--cli-shim', kind: 'bool', default: 'false', desc: 'run cli.shim (managed shell trust/proxy environment for CLI runtimes)' },
  { env: 'SAC_SHIM_DIR', flag: '--shim-dir', kind: 'path', default: '', desc: 'directory cli.shim writes the CA bundle and profile into; empty uses a platform default' },

  // Device-to-cloud drain (ADR 0020). Empty SAC_DEVICE_ENDPOINT disables it.
  { env: 'SAC_DEVICE_ENDPOINT', flag: '--device-endpoint', kind: 'url', default: '', mustFill: true, scope: 'tenant', desc: 'device ingress base URL, e.g. https://ingest.eu.example.com; empty disables the drain' },
  { env: 'SAC_AUTH_MODE', flag: '--auth-mode', kind: 'enum', values: ['x509', 'dpop'], default: '', desc: 'device credential mode for the drain' },
  { env: 'SAC_CREDENTIAL_FILE', flag: '--credential-file', kind: 'path', default: '<statedir>/credential.sealed', desc: 'sealed device credential issued by POST /v1/enrol; must NOT be inside SAC_SPOOL_DIR' },
  { env: 'SAC_DEPLOYMENT_KEY', flag: '--deployment-key', kind: 'secret', secret: true, default: '', mustFill: true, scope: 'tenant', desc: "the tenant's multi-use deployment key from the package (sacdk_...); enrols the device" },
  { env: 'SAC_ENROLMENT_TOKEN', flag: '--enrolment-token', kind: 'secret', secret: true, default: '', scope: 'lab', desc: 'a single-use bootstrap token (lab); exclusive with SAC_DEPLOYMENT_KEY, which replaces it in a tenant package' },
  { env: 'SAC_STATE_DIR', flag: '--state-dir', kind: 'path', default: '', desc: 'where the agent keeps what it fetches or generates (cached policy bundle, per-device CA); empty uses the credential file directory' },
  { env: 'SAC_CA_FILE', flag: '--ca-file', kind: 'path', default: '', scope: 'lab', desc: 'PEM CA set the edge is pinned to; empty uses the system roots' },
  { env: 'SAC_MDM_ID', flag: '--mdm-id', kind: 'string', default: '', scope: 'lab', desc: 'a set hardware-identity seed; empty reads the device attestation from the OS' },
  { env: 'SAC_BACKOFF_BASE', flag: '--backoff-base', kind: 'duration', default: '1s', desc: 'drain retry backoff base (full jitter)' },
  { env: 'SAC_BACKOFF_CAP', flag: '--backoff-cap', kind: 'duration', default: '300s', desc: 'drain retry backoff cap' },

  // Logging.
  { env: 'SAC_LOG_LEVEL', flag: '--log-level', kind: 'enum', values: ['debug', 'info', 'warn', 'error'], default: 'info', desc: 'log level' },
  { env: 'SAC_LOG_FORMAT', flag: '--log-format', kind: 'enum', values: ['json', 'text'], default: 'json', desc: 'log format' },
];

/**
 * The tenant package file: what control-api writes beside the generic MSI (or pkg/script) for one
 * tenant, and the only tenant-specific input a device takes. The package copies it to
 * LAYOUT[os].tenantFile; the agent reads it after the generic file, so it wins on any key both carry.
 */
export const TENANT_PACKAGE = {
  fileName: 'ShadowAICapture.tenant.env',
  keys: ['SAC_TENANT_ID', 'SAC_DEVICE_ENDPOINT', 'SAC_DEPLOYMENT_KEY', 'SAC_AUTH_MODE'],
};

/**
 * The generic, vendor-wide capture-core.env: every vendor-scope entry at its default, with these
 * product values over it. The classifier release ships in the package (docs/05 §6.1 as built), M3
 * content has somewhere to be held, and the credential is a DPoP key so no PKI is needed. With no
 * SAC_BUNDLE the agent fetches its policy from the control plane and verifies it under the pinned
 * SAC_POLICY_KEY.
 */
export const GENERIC = {
  SAC_CLASSIFIER_RELEASE: '<classifierdir>',
  // The agent mints the per-device interception CA only when something must trust it, so the
  // product turns both on: the root goes into the machine store, and the CLI shim points CLI
  // runtimes at the proxy. Remove-on-stop is what makes an uninstall take the root and the
  // environment entries back out (docs/05 §6.4 as built).
  SAC_TRUST_INSTALL: 'true',
  SAC_TRUST_REMOVE_ON_STOP: 'true',
  SAC_CLI_SHIM: 'true',
  SAC_CONTENT_DIR: '<statedir>/content',
  SAC_CONTENT_KEY: '<statedir>/content.key',
  SAC_AUTH_MODE: 'dpop',
  SAC_LOG_LEVEL: 'info',
};

/** Supplied per build, never committed: the public keys a vendor release pins. */
export const TRUST_ANCHORS = ['SAC_POLICY_KEY', 'SAC_POLICY_KEY_ID', 'SAC_CLASSIFIER_PUBKEY'];

/**
 * The generic capture-core.env as ordered [env, value] pairs. No tenant- or lab-scope key appears,
 * so the file cannot carry a tenant, a key or a token, nor pin a value the agent should resolve
 * itself (SAC_USER_REF). `anchors` supplies TRUST_ANCHORS; an absent anchor keeps its default.
 */
export function genericProfile(os, anchors = {}) {
  const layout = LAYOUT[os];
  const out = [];
  for (const c of configFor(os)) {
    if (c.scope === 'tenant' || c.scope === 'lab') continue;
    let v = c.env in GENERIC ? expandDefault({ default: GENERIC[c.env] }, layout) : expandDefault(c, layout);
    if (TRUST_ANCHORS.includes(c.env) && anchors[c.env] !== undefined) v = anchors[c.env];
    out.push([c.env, v]);
  }
  return out;
}

/** Replace the layout placeholders an entry's default may carry, in the layout's native separators. */
export function expandDefault(entry, layout) {
  const value = String(entry.default).replace('<statedir>', layout.statedir).replace('<classifierdir>', layout.classifierdir);
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
  // WiX generates a new ProductCode for every build, so every version has its own (Intune's
  // detection rule keys on it); this is the UpgradeCode, which is stable across versions so major
  // upgrades replace rather than stack (docs/05 §6.1). release-msi.mjs reads both back out of the
  // built package into release.json.
  upgradeCode: '7E9C2B7A-6D0E-4C6A-9F2B-1A6E6C2D44A1',
  version: '0.1.0',
};
