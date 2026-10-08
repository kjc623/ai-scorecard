// device/installer/manifest.mjs - the single source the three platform packages are generated from.
//
// A device reads two KEY=VALUE files with repeated --config-file, the later winning:
//   capture-core.env  the vendor-wide file every package installs and an upgrade replaces: the
//                     state directory, the classifier release and the trust anchors (genericProfile);
//   tenant.env        copied at install from ShadowAICapture.tenant.env beside the package, which
//                     control-api writes per download (TENANT_PACKAGE).
// render.mjs turns this file into device/installer/generated/**, and verify.mjs checks the catalogue against
// the flags capture-core actually registers.

/** The installed layout, per platform. Absolute paths only: a service manager resolves nothing. */
export const LAYOUT = {
  linux: {
    bindir: '/opt/shadow-ai-capture/bin',
    classifierdir: '/opt/shadow-ai-capture/classifier',
    configdir: '/etc/shadow-ai-capture',
    statedir: '/var/lib/shadow-ai-capture',
    configFile: '/etc/shadow-ai-capture/capture-core.env',
    tenantFile: '/etc/shadow-ai-capture/tenant.env',
    serviceName: 'shadow-ai-capture.service',
  },
  darwin: {
    bindir: '/usr/local/opt/shadow-ai-capture/bin',
    classifierdir: '/usr/local/opt/shadow-ai-capture/classifier',
    configdir: '/usr/local/etc/shadow-ai-capture',
    // Beneath /var/db/shadow-ai-capture, which the CLI shim writes for every user to read; the
    // state directory itself is readable by root only.
    statedir: '/var/db/shadow-ai-capture/state',
    logdir: '/var/log/shadow-ai-capture',
    configFile: '/usr/local/etc/shadow-ai-capture/capture-core.env',
    tenantFile: '/usr/local/etc/shadow-ai-capture/tenant.env',
    serviceName: 'com.shadowaicapture.capture-core',
  },
  windows: {
    bindir: 'C:\\Program Files\\ShadowAICapture\\bin',
    classifierdir: 'C:\\Program Files\\ShadowAICapture\\classifier',
    configdir: 'C:\\ProgramData\\ShadowAICapture',
    statedir: 'C:\\ProgramData\\ShadowAICapture\\state',
    profiledir: 'C:\\ProgramData\\ShadowAICapture\\profile',
    configFile: 'C:\\ProgramData\\ShadowAICapture\\profile\\capture-core.env',
    tenantFile: 'C:\\ProgramData\\ShadowAICapture\\profile\\tenant.env',
    serviceName: 'ShadowAICapture',
  },
};

/** The Go commands compiled into every package; classifier-host runs as capture-core's child. */
export const GO_BINARIES = [
  { name: 'capture-core', dir: 'device/capture-core', pkg: './cmd/capture-core' },
  { name: 'classifier-host', dir: 'device/classifier-host', pkg: './cmd/classifier-host' },
];

/**
 * The browser extension's native messaging host. Chrome and Edge start capture-core with the
 * extension's origin as its argument, and capture-core relays the extension's messages to the
 * running service. The allowed origin is the id pinned by device/extension/manifest.json's `key`.
 */
export const NATIVE_HOST = {
  name: 'com.shadowaicapture.capture_core',
  description: 'Shadow AI Capture',
  // Where each platform's browsers look for a system-wide host manifest. On Windows the MSI
  // registers the manifest under these HKLM keys instead of copying it.
  dirs: {
    linux: ['/etc/opt/chrome/native-messaging-hosts', '/etc/opt/edge/native-messaging-hosts'],
    darwin: ['/Library/Google/Chrome/NativeMessagingHosts', '/Library/Microsoft Edge/NativeMessagingHosts'],
  },
  registryKeys: [
    'SOFTWARE\\Google\\Chrome\\NativeMessagingHosts\\com.shadowaicapture.capture_core',
    'SOFTWARE\\Microsoft\\Edge\\NativeMessagingHosts\\com.shadowaicapture.capture_core',
  ],
};

/**
 * The Windows Start-menu shortcut that gives the agent's notifications their identity. Windows shows
 * a desktop app's toast only under an AppUserModelID that a Start-menu shortcut carries as its
 * System.AppUserModel.ID property, and capture-core's user-session helper shows its toasts under
 * this one (AppUserModelID in device/capture-core/userhelper). Opening the shortcut runs
 * capture-core with `arguments`, which only prints the version.
 */
export const START_MENU_SHORTCUT = {
  name: 'Shadow AI Capture',
  description: 'Shows the notifications of the Shadow AI Capture agent.',
  arguments: '--version',
  appUserModelId: 'ShadowAICapture.Agent',
};

/** The host manifest for one platform. Windows resolves `path` against the manifest's own folder. */
export function nativeHostManifest(os, extensionId) {
  return {
    name: NATIVE_HOST.name,
    description: NATIVE_HOST.description,
    path: os === 'windows' ? 'capture-core.exe' : `${LAYOUT[os].bindir}/capture-core`,
    type: 'stdio',
    allowed_origins: [`chrome-extension://${extensionId}/`],
  };
}

/**
 * The configuration catalogue: every SAC_* key capture-core reads from a --config-file, in the order
 * the generated files list them. It must equal capture-core's own catalogue (verify.mjs). What to
 * collect, at which mode and through which collectors comes from the signed policy, not from here.
 *
 *   env       the key in a configuration file
 *   flag      the capture-core flag it sets
 *   default   the value the generic file carries; <statedir> and <classifierdir> expand per layout
 *   required  capture-core refuses to start without it
 *   secret    never given a default and never echoed
 *   scope     absent: vendor-wide, written into the generic file. 'tenant': only from the tenant
 *             file. 'override': an optional per-device setting that neither the generic file nor
 *             the tenant package carries.
 */
export const CONFIG = [
  { env: 'SAC_STATE_DIR', flag: '--state-dir', default: '<statedir>', required: true, desc: 'protected state directory: spool, keys, device credential, held content, cached policy, interception CA' },
  { env: 'SAC_TENANT_ID', flag: '--tenant-id', default: '', required: true, scope: 'tenant', desc: 'the tenant this device belongs to' },
  { env: 'SAC_DEVICE_ENDPOINT', flag: '--device-endpoint', default: '', required: true, scope: 'tenant', desc: 'device edge base URL, https with no path' },
  { env: 'SAC_DEPLOYMENT_KEY', flag: '--deployment-key', default: '', secret: true, scope: 'tenant', desc: "the tenant's deployment key; a first enrolment presents it" },
  { env: 'SAC_CA_FILE', flag: '--ca-file', default: '', scope: 'override', desc: 'PEM CA set trusted for the device endpoint in addition to the system roots' },
  { env: 'SAC_POLICY_KEY', flag: '--policy-key', default: '', desc: 'hex Ed25519 public key every policy bundle must verify under' },
  { env: 'SAC_POLICY_KEY_ID', flag: '--policy-key-id', default: 'policy-key-1', desc: 'key id every policy bundle must name' },
  { env: 'SAC_CLASSIFIER_RELEASE', flag: '--classifier-release', default: '<classifierdir>', desc: 'signed classifier release directory' },
  { env: 'SAC_CLASSIFIER_PUBKEY', flag: '--classifier-pubkey', default: '', desc: 'hex Ed25519 public key the classifier release must verify under' },
  { env: 'SAC_DEVICE_IDENTITY', flag: '--device-identity', default: 'clear', desc: "identity setting to act on until the server states the tenant's: clear or hashed" },
  { env: 'SAC_LOG_LEVEL', flag: '--log-level', default: 'info', desc: 'debug, info, warn or error' },
];

/** The tenant file control-api writes beside the generic package, and the only tenant input. */
export const TENANT_PACKAGE = {
  fileName: 'ShadowAICapture.tenant.env',
  keys: ['SAC_TENANT_ID', 'SAC_DEVICE_ENDPOINT', 'SAC_DEPLOYMENT_KEY'],
};

/** The public keys a release pins in the generic file; build inputs, never committed. */
export const TRUST_ANCHORS = ['SAC_POLICY_KEY', 'SAC_POLICY_KEY_ID', 'SAC_CLASSIFIER_PUBKEY'];

/** The generic capture-core.env as ordered [key, value] pairs: every vendor-scope key, nothing else. */
export function genericProfile(os, anchors = {}) {
  const layout = LAYOUT[os];
  return CONFIG.filter((c) => !c.scope).map((c) => {
    if (TRUST_ANCHORS.includes(c.env) && anchors[c.env] !== undefined) return [c.env, anchors[c.env]];
    return [c.env, expand(c.default, layout)];
  });
}

/** The generic file's text, in the platform's line endings. */
export function genericEnv(os, anchors, version) {
  const lines = [
    `# ${PRODUCT.displayName} ${version} - vendor-wide configuration. An upgrade replaces this file.`,
    `# ${TENANT_PACKAGE.fileName}, installed beside it as tenant.env, is read after it and wins.`,
    ...genericProfile(os, anchors).map(([k, v]) => `${k}=${v}`),
    '',
  ];
  return lines.join(os === 'windows' ? '\r\n' : '\n');
}

/** Expand the layout placeholders in a default, in the layout's own path separator. */
export function expand(value, layout) {
  const v = String(value).replace('<statedir>', layout.statedir).replace('<classifierdir>', layout.classifierdir);
  return layout.statedir.includes('\\') ? v.replace(/\//g, '\\') : v;
}

export const PRODUCT = {
  name: 'ShadowAICapture',
  displayName: 'Shadow AI Capture',
  manufacturer: 'Shadow AI Capture',
  // Stable across versions so a newer MSI replaces an older one (major upgrade); every build gets its
  // own ProductCode, which release-msi.mjs reads back into release.json.
  upgradeCode: '7E9C2B7A-6D0E-4C6A-9F2B-1A6E6C2D44A1',
};
