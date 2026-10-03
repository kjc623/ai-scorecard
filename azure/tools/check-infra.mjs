// check-infra.mjs — static validation of azure/*.bicep against docs/05-platform-delivery.md.
//
// Why this exists: there is no Azure subscription and no Bicep compiler on the build host, so the
// deployment cannot be checked by deploying it. What *can* be checked is the set of properties the
// document makes architectural rather than cosmetic:
//
//   - content-vault has internal ingress only, and Front Door has no route to it (D7, C15);
//   - the database is not reachable from outside the VNet (publicNetworkAccess Disabled, no
//     firewall rules);
//   - no storage account, vault or PostgreSQL server accepts a public connection;
//   - no secret is written into the repository, and every parameter is typed and described;
//   - no region or SKU is hard-coded outside a parameter file;
//   - every row of the document's §2 inventory is implemented, and nothing undocumented is
//     deployed;
//   - the §11 cost model's arithmetic matches its own stated unit prices and the document's totals.
//
// It is deliberately zero-dependency and line-based rather than a Bicep parser: it must run with
// `node --test` on a host with no network, and a reviewer must be able to read the rule and check
// it by eye. A rule that cannot be stated plainly is a rule that cannot be reviewed.

import { readFileSync, readdirSync, existsSync, statSync } from 'node:fs';
import { join, relative, sep, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

export const INFRA_ROOT = fileURLToPath(new URL('..', import.meta.url));
export const REPO_ROOT = fileURLToPath(new URL('../..', import.meta.url));
export const DEFAULT_DOC = join(REPO_ROOT, 'docs', '05-platform-delivery.md');

// ---------------------------------------------------------------------------------------------
// Loading

export function listFiles(dir, filter = () => true, depth = 0) {
  if (depth > 4 || !existsSync(dir)) return [];
  const out = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name === '.git') continue;
    const p = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...listFiles(p, filter, depth + 1));
    else if (filter(entry.name)) out.push(p);
  }
  return out.sort();
}

/** The Bicep tree as { path, rel, text, lines }, with `rel` relative to azure/ and slash-separated. */
export function loadInfraTree(root = INFRA_ROOT) {
  return listFiles(root, (n) => n.endsWith('.bicep') || n.endsWith('.bicepparam')).map((path) => {
    const text = readFileSync(path, 'utf8');
    return { path, rel: relative(root, path).split(sep).join('/'), text, lines: text.split(/\r?\n/) };
  });
}

export function loadInventory(root = INFRA_ROOT) {
  return JSON.parse(readFileSync(join(root, 'inventory.json'), 'utf8'));
}

// ---------------------------------------------------------------------------------------------
// Findings

export function finding(rule, file, line, message) {
  return { rule, file, line, message };
}

/** Strip // comments: an invariant stated in a comment is documentation, not a reference. */
export function stripComments(text) {
  return text
    .split(/\r?\n/)
    .map((l) => {
      const i = l.indexOf('//');
      if (i < 0) return l;
      // Keep it simple and honest: a URL inside a string would be mis-stripped, and no module here
      // contains one. The checker is line-based by design, and a rule a reviewer cannot read is a
      // rule a reviewer cannot trust.
      return l.slice(0, i);
    })
    .join('\n');
}

function scan(text, regex) {
  const out = [];
  const lines = text.split(/\r?\n/);
  lines.forEach((l, i) => {
    const re = new RegExp(regex.source, regex.flags.includes('g') ? regex.flags : regex.flags + 'g');
    let m;
    while ((m = re.exec(l)) !== null) {
      out.push({ line: i + 1, text: l, match: m[0] });
      if (m.index === re.lastIndex) re.lastIndex++;
    }
  });
  return out;
}

// ---------------------------------------------------------------------------------------------
// Rule 1: every parameter has a type and a description.

/** Parse `param name type` declarations with the decorators immediately above them. */
export function parseParams(text) {
  const lines = text.split(/\r?\n/);
  const params = [];
  let decorators = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const dec = line.match(/^\s*@(description|allowed|secure|minLength|maxLength|minValue|maxValue|metadata)\s*\(/);
    if (dec) {
      decorators.push({ name: dec[1], line: i + 1, text: line });
      continue;
    }
    const p = line.match(/^\s*param\s+([A-Za-z_][A-Za-z0-9_]*)\s+([A-Za-z][A-Za-z0-9_?[\]]*)\s*(=.*)?$/);
    if (p) {
      params.push({
        name: p[1],
        type: p[2],
        hasDefault: Boolean(p[3]),
        line: i + 1,
        decorators,
        hasDescription: decorators.some((d) => d.name === 'description'),
        secure: decorators.some((d) => d.name === 'secure'),
      });
      decorators = [];
      continue;
    }
    if (/^\s*(param|var|resource|module|output|targetScope|type)\b/.test(line) || line.trim() === '') {
      if (line.trim() === '') continue;
      decorators = [];
    } else if (!/^\s*(\/\/|\*|\/\*)/.test(line)) {
      decorators = [];
    }
  }
  return params;
}

export function checkParams(files) {
  const findings = [];
  for (const f of files.filter((x) => x.rel.endsWith('.bicep'))) {
    const params = parseParams(f.text);
    for (const p of params) {
      if (!p.type || p.type.length === 0) {
        findings.push(finding('param-type', f.rel, p.line, `parameter ${p.name} has no type`));
      }
      if (!p.hasDescription) {
        findings.push(finding('param-description', f.rel, p.line, `parameter ${p.name} has no @description`));
      }
    }
    if (params.length === 0 && !f.rel.endsWith('main.bicep')) {
      findings.push(finding('param-none', f.rel, 1, 'module declares no parameters; every module takes its inputs explicitly (§3.3)'));
    }
  }
  return findings;
}

// ---------------------------------------------------------------------------------------------
// Rule 2: no hard-coded region or SKU outside a parameter file.

const REGION_LITERALS = [
  'eastus', 'eastus2', 'centralus', 'westus', 'westus2', 'westus3', 'northeurope', 'westeurope',
  'uksouth', 'swedencentral', 'australiaeast', 'southeastasia', 'japaneast', 'canadacentral',
  'brazilsouth', 'southafricanorth', 'uaenorth', 'centralindia', 'koreacentral', 'francecentral',
];

// SKUs and tiers that must be parameters, not decisions baked into a module.
const SKU_LITERALS = [
  'Premium', 'Standard', 'Basic', 'Consumption', 'D2ds_v5', 'D2s_v5', 'B2s', 'B_Standard_B2s',
  'GPv3', 'GPv2', 'RA-GRS', 'ZRS', 'LRS', 'GRS', 'A_Standard', 'P1v3', 'E10',
];

/** Strip decorator lines: an @allowed([...]) list is a type constraint, not a hard-coded choice. */
function withoutDecorators(text) {
  return text
    .split(/\r?\n/)
    .filter((l) => !/^\s*@(description|allowed|metadata|secure|minLength|maxLength|minValue|maxValue)\s*\(/.test(l))
    .join('\n');
}

export function checkNoHardcodedRegion(files) {
  const findings = [];
  for (const f of files.filter((x) => x.rel.startsWith('modules/') || x.rel === 'main.bicep')) {
    const body = withoutDecorators(f.text);
    for (const region of REGION_LITERALS) {
      for (const hit of scan(body, new RegExp(`(['"\`])${region}\\1`, 'i'))) {
        findings.push(finding('region-hardcoded', f.rel, hit.line,
          `region '${region}' is a literal in ${f.rel}; region is environment-parameterised (§3.2)`));
      }
    }
  }
  return findings;
}

export function checkNoHardcodedSku(files) {
  const findings = [];
  for (const f of files.filter((x) => x.rel.startsWith('modules/'))) {
    // A parameter default is *inside* a parameter, which is where a SKU belongs; a decorator is a
    // type constraint. Everything else that spells a SKU is a decision baked into a module.
    const body = stripComments(withoutDecorators(f.text));
    for (const sku of SKU_LITERALS) {
      const pattern = new RegExp(`(['"\`])${sku.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\1`);
      for (const hit of scan(body, pattern)) {
        if (/^\s*param\b/.test(hit.text)) continue;
        findings.push(finding('sku-hardcoded', f.rel, hit.line,
          `SKU/tier '${sku}' is a literal in ${f.rel}; SKU is environment-parameterised (§3.2)`));
      }
    }
  }
  return findings;
}

// ---------------------------------------------------------------------------------------------
// Rule 3: no plaintext secret, and no secret-ish parameter without @secure() or Key Vault.

const SECRETISH = /(password|passwd|pwd|secret|accountkey|connectionstring|apikey|api_key|clientsecret|sas ?token)/i;

export function checkNoSecrets(files) {
  const findings = [];
  for (const f of files) {
    const code = stripComments(f.text);
    for (const p of parseParams(f.text)) {
      if (SECRETISH.test(p.name) && !p.secure && !/(uri|url|name|id)$/i.test(p.name)) {
        findings.push(finding('secret-param', f.rel, p.line,
          `parameter ${p.name} looks like a credential but is not @secure() and is not a Key Vault reference`));
      }
    }
    for (const hit of scan(code, /(-----BEGIN [A-Z ]*PRIVATE KEY-----|AccountKey\s*=|SharedAccessSignature\s*=|"password"\s*:|'password'\s*:)/)) {
      findings.push(finding('secret-literal', f.rel, hit.line, `plaintext credential material in ${f.rel}: ${hit.match}`));
    }
    // A long base64-ish literal is how a secret looks when it is committed by accident.
    for (const hit of scan(code, /(['"])[A-Za-z0-9+/]{60,}={0,2}\1/)) {
      findings.push(finding('secret-literal', f.rel, hit.line, 'a 60+ character base64-like literal; credentials never belong in Bicep (§5.1)'));
    }
    // A non-secure parameter whose default is a non-empty string that is not a Key Vault reference.
    for (const p of parseParams(f.text)) {
      const m = f.text.split(/\r?\n/)[p.line - 1]?.match(/=\s*'([^']{8,})'/);
      if (m && SECRETISH.test(p.name) && !p.secure) {
        findings.push(finding('secret-default', f.rel, p.line, `parameter ${p.name} carries a default value that looks like a credential`));
      }
    }
  }
  return findings;
}

// ---------------------------------------------------------------------------------------------
// Rule 4: the architectural properties.

const MUST = [
  { rel: 'modules/postgres.bicep', property: /publicNetworkAccess\s*:\s*'Disabled'/,
    message: 'PostgreSQL must declare publicNetworkAccess Disabled (§2.1, §2 inventory)' },
  { rel: 'modules/storage-ciphertext.bicep', property: /allowSharedKeyAccess\s*:\s*false/,
    message: 'the ciphertext account must disable shared-key access (§2 inventory)' },
  { rel: 'modules/storage-ciphertext.bicep', property: /allowBlobPublicAccess\s*:\s*false/,
    message: 'the ciphertext account must disable public blob access (§2 inventory)' },
  { rel: 'modules/storage-ciphertext.bicep', property: /publicNetworkAccess\s*:\s*'Disabled'/,
    message: 'the ciphertext account must refuse public network access (§2 inventory)' },
  { rel: 'modules/storage-exports.bicep', property: /publicNetworkAccess\s*:\s*'Disabled'/,
    message: 'the exports account must refuse public network access' },
  // Purge protection may come from a parameter whose default is true — that is more reviewable than a
  // literal — but it may never be written as false, and the parameter's default is asserted separately.
  { rel: 'modules/keyvault.bicep', property: /enablePurgeProtection\s*:\s*(?!false)\w+/,
    message: 'purge protection is not optional (§12.2)' },
  { rel: 'modules/keyvault.bicep', property: /enableRbacAuthorization\s*:\s*true/,
    message: 'Key Vault authorization is RBAC, not access policies (§5.3)' },
  { rel: 'modules/keyvault.bicep', property: /publicNetworkAccess\s*:\s*'Disabled'/,
    message: 'the vault is reachable only through its private endpoint (§2 inventory)' },
  { rel: 'modules/container-apps-env.bicep', property: /internal\s*:\s*(true|internalLoadBalancer)/,
    message: 'the Container Apps environment must use an internal load balancer (§2.1)' },
  { rel: 'modules/frontdoor.bicep', property: /privateLink/,
    message: 'Front Door Premium must reach origins over Private Link (§2 inventory)' },
  { rel: 'modules/waf.bicep', property: /mode\s*:\s*(param\.)?\w*wafMode|mode\s*:\s*'Prevention'/,
    message: 'the WAF mode must come from a parameter and default to Prevention in production (§3.2)' },
  { rel: 'modules/application-gateway.bicep', property: /verifyClientAuthMode\s*:\s*'Passthrough'/,
    message: 'the device gateway must request a client certificate in passthrough mode, not require one (ADR 0020 decision 1)' },
  { rel: 'modules/application-gateway.bicep', property: /headerName\s*:\s*'X-Client-Cert'/,
    message: 'the device gateway must forward the client certificate in X-Client-Cert (ADR 0020 decision 2)' },
  { rel: 'modules/application-gateway.bicep', property: /\{var_client_certificate\}/,
    message: 'the X-Client-Cert rewrite must use the {var_client_certificate} server variable' },
  { rel: 'modules/application-gateway.bicep', property: /headerName\s*:\s*'X-Forwarded-Host'/,
    message: 'the device gateway must set X-Forwarded-Host so the origin’s DPoP htu matches the signed URL' },
  { rel: 'modules/application-gateway.bicep', property: /ipAddress\s*:\s*backendStaticIp/,
    message: 'the device gateway backend pool must be the internal Container Apps static IP, not a public hostname (§2.1)' },
  { rel: 'modules/application-gateway.bicep', property: /'WAF_v2'/,
    message: 'the device gateway SKU must be WAF_v2 (§2 inventory)' },
];

export function checkArchitecturalProperties(files) {
  const findings = [];
  const byRel = new Map(files.map((f) => [f.rel, f]));
  for (const rule of MUST) {
    const f = byRel.get(rule.rel);
    if (!f) {
      findings.push(finding('private-only', rule.rel, 0, `missing file: ${rule.rel}`));
      continue;
    }
    if (!rule.property.test(f.text)) {
      findings.push(finding('private-only', rule.rel, 1, rule.message));
    }
  }

  // content-vault: internal ingress in the composition, and no Front Door route to it.
  const main = byRel.get('main.bicep') || { text: '', rel: 'main.bicep' };
  const vaultBlocks = [...main.text.matchAll(/module\s+\w*content\w*vault\w*\s+'[^']*container-app\.bicep'[\s\S]*?\n\}/gi)];
  if (vaultBlocks.length === 0) {
    findings.push(finding('content-vault-ingress', 'main.bicep', 1,
      'main.bicep does not instantiate the content-vault container app module; the internal-ingress property cannot be asserted'));
  }
  for (const block of vaultBlocks) {
    const line = main.text.slice(0, block.index).split(/\r?\n/).length;
    if (!/ingress\s*:\s*'internal'/.test(block[0])) {
      findings.push(finding('content-vault-ingress', 'main.bicep', line,
        'content-vault is not instantiated with ingress: internal (D7: a CI policy check fails the build on external ingress)'));
    }
  }
  const fd = byRel.get('modules/frontdoor.bicep');
  if (fd && /content[-_]?vault/i.test(stripComments(fd.text))) {
    findings.push(finding('content-vault-route', 'modules/frontdoor.bicep', 1,
      'Front Door references content-vault; it has no Front Door route and no public endpoint (C15, D7)'));
  }

  // ADR 0020 decision 1: Front Door is the analyst edge only. The device /v1/* routes moved to
  // Application Gateway, so a /v1/* route here means the two edges overlap on the device surface.
  if (fd && /patternsToMatch\s*:[\s\S]*?\/v1\/\*/.test(stripComments(fd.text))) {
    findings.push(finding('frontdoor-device-route', 'modules/frontdoor.bicep', 1,
      'Front Door must not route /v1/*; device routes are on Application Gateway (ADR 0020 decision 1)'));
  }

  // No firewall rule resource anywhere: the server additionally has no firewall rules at all, so
  // "reachable from the internet" is not a configuration state it can be put into (§2.1).
  for (const f of files.filter((x) => x.rel.startsWith('modules/'))) {
    for (const hit of scan(stripComments(f.text), /Microsoft\.DBforPostgreSQL\/flexibleServers\/firewallRules/)) {
      findings.push(finding('postgres-firewall', f.rel, hit.line,
        'a PostgreSQL firewall rule exists; the server must have no firewall rules at all (§2.1)'));
    }
  }
  return findings;
}

// ---------------------------------------------------------------------------------------------
// Rule 5: the inventory diff, both directions.

/** Normalise a document label: strip bold markers and backticks, collapse whitespace. */
export function normaliseLabel(s) {
  return s.replace(/\*/g, '').replace(/`/g, '').replace(/\s+/g, ' ').trim();
}

/** Parse the §2 inventory table out of docs/05-platform-delivery.md. */
export function parseDocInventory(docText) {
  const start = docText.indexOf('## 2. Azure resource inventory');
  const end = docText.indexOf('### 2.1 Networking, stated once');
  if (start < 0 || end < 0) return [];
  const section = docText.slice(start, end);
  const rows = [];
  for (const line of section.split(/\r?\n/)) {
    if (!line.startsWith('|')) continue;
    const cells = line.split('|').map((c) => c.trim());
    if (cells.length < 4) continue;
    const label = cells[1];
    if (!label || /^-+$/.test(label) || /^Resource$/i.test(label)) continue;
    rows.push({ label, key: normaliseLabel(label) });
  }
  return rows;
}

/** Parse the §2.2 "deliberately absent" table, which must stay absent from the Bicep. */
export function parseDocAbsent(docText) {
  const start = docText.indexOf('### 2.2 Deliberately absent');
  const end = docText.indexOf('## 3. Environments and infrastructure as code');
  if (start < 0 || end < 0) return [];
  const rows = [];
  for (const line of docText.slice(start, end).split(/\r?\n/)) {
    if (!line.startsWith('|')) continue;
    const cells = line.split('|').map((c) => c.trim());
    if (cells.length < 4 || !cells[1] || /^-+$/.test(cells[1]) || /^Not deployed$/i.test(cells[1])) continue;
    rows.push({ label: cells[1], key: normaliseLabel(cells[1]) });
  }
  return rows;
}

export function checkInventory(files, docText, inventory) {
  const findings = [];
  const docRows = parseDocInventory(docText);
  if (docRows.length === 0) {
    findings.push(finding('inventory', 'docs/05-platform-delivery.md', 0, 'could not parse the §2 inventory table'));
    return findings;
  }
  const mapped = new Map(inventory.inventory.map((e) => [normaliseLabel(e.row), e]));

  for (const row of docRows) {
    const entry = mapped.get(row.key);
    if (!entry) {
      findings.push(finding('inventory-missing', 'azure/inventory.json', 0,
        `document row "${row.label}" has no Bicep implementation recorded (documented gap)`));
      continue;
    }
    for (const mod of entry.modules) {
      if (!existsSync(join(INFRA_ROOT, mod))) {
        findings.push(finding('inventory-module', 'azure/inventory.json', 0,
          `row "${row.label}" names ${mod}, which does not exist`));
      }
    }
  }
  for (const entry of inventory.inventory) {
    if (!docRows.some((r) => r.key === normaliseLabel(entry.row))) {
      findings.push(finding('inventory-extra', 'azure/inventory.json', 0,
        `inventory.json maps "${entry.row}", which is not a row in the document's §2 table`));
    }
  }

  // §3.3's layout: every module in the layout exists, and every module on disk is accounted for.
  const layout = [
    'network.bicep', 'private-endpoints.bicep', 'log-analytics.bicep', 'registry.bicep',
    'postgres.bicep', 'storage-ciphertext.bicep', 'storage-exports.bicep', 'keyvault.bicep',
    'managed-hsm.bicep', 'container-apps-env.bicep', 'container-app.bicep', 'container-app-job.bicep',
    'application-gateway.bicep', 'frontdoor.bicep', 'waf.bicep', 'static-web-app.bicep',
    'monitoring.bicep', 'budget.bicep',
  ];
  const onDisk = files.filter((f) => f.rel.startsWith('modules/')).map((f) => f.rel.slice('modules/'.length));
  for (const mod of layout) {
    if (!onDisk.includes(mod)) {
      findings.push(finding('layout-missing', `modules/${mod}`, 0, `§3.3 names modules/${mod}; it is not on disk`));
    }
  }
  for (const mod of onDisk) {
    if (!layout.includes(mod)) {
      findings.push(finding('layout-extra', `modules/${mod}`, 0, `modules/${mod} is not in the §3.3 layout`));
    }
  }

  // §2.2: what the document deliberately does not deploy must not appear in the Bicep.
  const docAbsent = parseDocAbsent(docText);
  const allTypes = files.flatMap((f) => scan(f.text, /Microsoft\.[A-Za-z]+\/[A-Za-z0-9/]+/g).map((h) => ({ file: f.rel, line: h.line, type: h.match })));
  for (const absent of inventory.deliberatelyAbsent ?? []) {
    const documentRow = docAbsent.find((r) => r.key.startsWith(normaliseLabel(absent.resource).slice(0, 24)));
    if (!documentRow) {
      findings.push(finding('absent-unmatched', 'azure/inventory.json', 0,
        `deliberately-absent entry "${absent.resource}" does not match any §2.2 row`));
    }
    for (const t of absent.forbiddenTypes ?? []) {
      for (const hit of allTypes.filter((x) => x.type === t)) {
        findings.push(finding('deliberately-absent', hit.file, hit.line,
          `${t} is deliberately not deployed (§2.2): ${absent.resource}`));
      }
    }
  }
  return findings;
}

// ---------------------------------------------------------------------------------------------
// Rule 6: parameterisation per environment, and no secret in a parameter file.

export function checkEnvironments(files, docText) {
  const findings = [];
  const params = files.filter((f) => f.rel.startsWith('params/'));
  const wanted = ['dev', 'staging', 'prod'];
  for (const w of wanted) {
    if (!params.some((p) => p.rel.toLowerCase().includes(w))) {
      findings.push(finding('env-missing', `params/${w}.bicepparam`, 0,
        `§3.1's environment ladder needs a parameter file for ${w}`));
    }
  }
  for (const p of params) {
    for (const required of ['location', 'environment']) {
      if (!new RegExp(`\\b${required}\\s*=`).test(p.text)) {
        findings.push(finding('env-param', p.rel, 1, `parameter file does not set ${required}`));
      }
    }
    // Comments are prose: a file that says "no secret lives here" is not naming a credential.
    for (const hit of scan(stripComments(p.text), /(password|secret|accountKey|connectionString)/i)) {
      findings.push(finding('env-secret', p.rel, hit.line, 'a parameter file names a credential; secrets never live in the repository (§5.1)'));
    }
  }
  // Container Apps: every app and job instantiation binds a user-assigned identity (§5.1). The
  // check counts parameter bindings rather than the literal token, so a module call that forgot its
  // identity is a finding even when another call carries one.
  const main = files.find((f) => f.rel === 'main.bicep');
  if (main) {
    const calls = [...main.text.matchAll(/module\s+\w+\s+'[^']*container-app(?:-job)?\.bicep'/g)];
    const bindings = [...main.text.matchAll(/userAssignedIdentityId\s*:/g)];
    if (calls.length > 0 && bindings.length < calls.length) {
      findings.push(finding('managed-identity', 'main.bicep', 1,
        `${calls.length} container app/job instantiations but only ${bindings.length} userAssignedIdentityId bindings; every service runs as a user-assigned managed identity (§5.1)`));
    }
  }
  return findings;
}

// ---------------------------------------------------------------------------------------------
// Rule 7: the cost model's arithmetic.

/** Parse the fenced JSON block in azure/cost-model.md delimited by cost-model:start/end. */
export function parseCostModel(text) {
  const m = text.match(/<!--\s*cost-model:start\s*-->([\s\S]*?)<!--\s*cost-model:end\s*-->/);
  if (!m) return null;
  return JSON.parse(m[1]);
}

export function checkCostModel(text, docText) {
  const findings = [];
  let model;
  try {
    model = parseCostModel(text);
  } catch (e) {
    return [finding('cost-model', 'azure/cost-model.md', 0, `cost model block is not valid JSON: ${e.message}`)];
  }
  if (!model) return [finding('cost-model', 'azure/cost-model.md', 0, 'no cost-model:start/end block found')];

  const unit = model.unitPrices ?? {};
  const round2 = (n) => Math.round(n * 100) / 100;
  const totals = {};
  for (const line of model.lines ?? []) {
    let computed = 0;
    for (const term of line.terms ?? []) {
      if (typeof term.unit === 'string') {
        const price = unit[term.unit];
        if (typeof price !== 'number') {
          findings.push(finding('cost-model', 'azure/cost-model.md', 0,
            `line "${line.name}" uses unit "${term.unit}", which is not in unitPrices`));
          continue;
        }
        computed += term.quantity * price;
      } else if (typeof term.fromLine === 'string') {
        const other = (model.lines ?? []).find((l) => l.name === term.fromLine);
        if (!other) {
          findings.push(finding('cost-model', 'azure/cost-model.md', 0,
            `line "${line.name}" references line "${term.fromLine}", which does not exist`));
          continue;
        }
        computed += other.maximumShare ?? other.total ?? 0;
      } else {
        computed += term.quantity * term.price;
      }
    }
    totals[line.name] = round2(computed);
    if (line.total !== undefined && round2(computed) !== round2(line.total)) {
      findings.push(finding('cost-model', 'azure/cost-model.md', 0,
        `line "${line.name}": terms compute to $${round2(computed)}, stated as $${line.total}`));
    }
  }

  const subtotal = round2(Object.values(totals).reduce((a, b) => a + b, 0));
  if (model.subtotal !== undefined && round2(model.subtotal) !== subtotal) {
    findings.push(finding('cost-model', 'azure/cost-model.md', 0,
      `subtotal: line totals sum to $${subtotal}, but the model states a subtotal of $${model.subtotal}`));
  }

  // Reconcile against the document's own §11.2 table. The document rounds each line to the nearest
  // dollar, so a line comparison allows $1 and the subtotal allows the model's declared rounding
  // budget (worst case is $0.50 per line, and the budget is stated rather than assumed).
  const docTable = docText.slice(docText.indexOf('**Per-tenant monthly estimate, M1 default:**'), docText.indexOf('### 11.3 Shared regional costs'));
  const subtotalTolerance = model.docSubtotalTolerance ?? 3;
  for (const line of model.lines ?? []) {
    if (!line.docLabel) continue;
    const re = new RegExp(`\\|\\s*\\**${line.docLabel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\**[^|]*\\|\\s*\\**\\$?([0-9]+)`, 'i');
    const m = docTable.match(re);
    if (!m) {
      findings.push(finding('cost-model', 'azure/cost-model.md', 0,
        `line "${line.name}" claims to reconcile with the document's "${line.docLabel}", which is not in §11.2's table`));
      continue;
    }
    const docValue = Number(m[1]);
    if (Math.abs(docValue - (line.total ?? 0)) > 1) {
      findings.push(finding('cost-model', 'azure/cost-model.md', 0,
        `line "${line.name}": model says $${line.total}, document says $${docValue} — reconcile explicitly rather than adopting either silently`));
    }
  }
  const docSubtotal = docTable.match(/\*\*Subtotal, in-tenant consumption\*\*\s*\|\s*\*\*\$([0-9]+)\*\*/);
  if (docSubtotal && Math.abs(Number(docSubtotal[1]) - subtotal) > subtotalTolerance) {
    findings.push(finding('cost-model', 'azure/cost-model.md', 0,
      `subtotal: model computes $${subtotal}, document states $${docSubtotal[1]} (tolerance $${subtotalTolerance})`));
  }

  // Documented findings are data, not prose: each must name the section it is about and the lines
  // it affects, so a reviewer can check the claim rather than the wording.
  for (const f of model.findings ?? []) {
    for (const required of ['id', 'section', 'statement', 'impact']) {
      if (!f[required]) {
        findings.push(finding('cost-model', 'azure/cost-model.md', 0,
          `a documented cost finding is missing "${required}"; findings are checkable data, not prose`));
      }
    }
    for (const line of f.affectsLines ?? []) {
      if (!(model.lines ?? []).some((l) => l.name === line)) {
        findings.push(finding('cost-model', 'azure/cost-model.md', 0,
          `cost finding "${f.id}" names line "${line}", which does not exist in the model`));
      }
    }
  }
  return findings;
}

// ---------------------------------------------------------------------------------------------

export function runAll({ root = INFRA_ROOT, docPath = DEFAULT_DOC } = {}) {
  const files = loadInfraTree(root);
  const docText = existsSync(docPath) ? readFileSync(docPath, 'utf8') : '';
  const inventory = loadInventory(root);
  const costPath = join(root, 'cost-model.md');
  const costText = existsSync(costPath) ? readFileSync(costPath, 'utf8') : '';
  const findings = [
    ...checkParams(files),
    ...checkNoHardcodedRegion(files),
    ...checkNoHardcodedSku(files),
    ...checkNoSecrets(files),
    ...checkArchitecturalProperties(files),
    ...(docText ? checkInventory(files, docText, inventory) : [finding('inventory', docPath, 0, 'document not found')]),
    ...checkEnvironments(files, docText),
    ...(costText ? checkCostModel(costText, docText) : [finding('cost-model', 'azure/cost-model.md', 0, 'cost model not found')]),
  ];
  return { files, findings, inventory };
}

// ---------------------------------------------------------------------------------------------
// CLI

function main() {
  const args = process.argv.slice(2);
  const json = args.includes('--json');
  const { files, findings } = runAll({});
  if (json) {
    process.stdout.write(JSON.stringify({ fileCount: files.length, findings }, null, 2) + '\n');
  } else {
    process.stdout.write(`infra checker: ${files.length} Bicep/parameter files, ${findings.length} finding(s)\n`);
    for (const f of findings) {
      process.stdout.write(`  ${f.rule}  ${f.file}:${f.line}  ${f.message}\n`);
    }
  }
  process.exitCode = findings.length === 0 ? 0 : 1;
}

if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1].replace(/\\/g, '/')}`).href) {
  main();
}
