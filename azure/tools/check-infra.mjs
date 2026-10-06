// check-infra.mjs — security properties of azure/ that must never regress, checked from source.
//
// `az bicep build` proves the templates compile; this proves they still say what the design depends
// on: no public path to the database or the vault, content-vault and query-api internal-only, the
// device edge limited to the device API, no secret in a parameter file. Run it directly for a report
// (exit 1 on a finding) or through check-infra.test.mjs.

import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

export const AZURE_ROOT = fileURLToPath(new URL('..', import.meta.url));

export function loadTree(root = AZURE_ROOT) {
  const read = (rel) => ({ rel, text: readFileSync(join(root, rel), 'utf8') });
  const modules = readdirSync(join(root, 'modules')).filter((f) => f.endsWith('.bicep')).sort();
  const params = readdirSync(join(root, 'params')).filter((f) => f.endsWith('.bicepparam')).sort();
  return {
    main: read('main.bicep'),
    modules: Object.fromEntries(modules.map((f) => [f, read(`modules/${f}`)])),
    params: params.map((f) => read(`params/${f}`)),
  };
}

/** The text of `module <symbol> … { … }` in main.bicep, up to its matching brace. */
export function moduleBlock(text, symbol) {
  const start = text.search(new RegExp(`^module ${symbol} `, 'm'));
  if (start < 0) return '';
  let depth = 0;
  for (let i = text.indexOf('{', start); i < text.length; i++) {
    if (text[i] === '{') depth++;
    else if (text[i] === '}' && --depth === 0) return text.slice(start, i + 1);
  }
  return text.slice(start);
}

function missing(text, expectations) {
  return expectations.filter(([re]) => !re.test(text)).map(([, message]) => message);
}

function arrayVar(text, name) {
  const body = text.match(new RegExp(`var ${name} = \\[([^\\]]*)\\]`))?.[1] ?? '';
  return [...body.matchAll(/'([^']+)'/g)].map((m) => m[1]);
}

const rules = [
  {
    id: 'modules-referenced',
    check: (t) => Object.keys(t.modules)
      .filter((f) => !t.main.text.includes(`'modules/${f}'`))
      .map((f) => `modules/${f} is not used by main.bicep`),
  },
  {
    id: 'internal-only-apps',
    check: (t) => [['vaultApp', 'content-vault'], ['queryApp', 'query-api']]
      .filter(([symbol]) => !/ingress: 'internal'/.test(moduleBlock(t.main.text, symbol)))
      .map(([, app]) => `${app} must have ingress: 'internal'`),
  },
  {
    id: 'environment-internal',
    check: (t) => missing(t.modules['container-apps-env.bicep']?.text ?? '', [
      [/internal: true/, 'the Container Apps environment must have an internal load balancer'],
    ]),
  },
  {
    id: 'postgres-private-entra-only',
    check: (t) => missing(t.modules['postgres.bicep']?.text ?? '', [
      [/publicNetworkAccess: 'Disabled'/, 'PostgreSQL must disable public network access'],
      [/passwordAuth: 'Disabled'/, 'PostgreSQL must disable password authentication'],
      [/delegatedSubnetResourceId/, 'PostgreSQL must be VNet-integrated'],
    ]),
  },
  {
    id: 'vault-locked',
    check: (t) => missing(t.modules['keyvault.bicep']?.text ?? '', [
      [/enableRbacAuthorization: true/, 'Key Vault must use RBAC authorization'],
      [/enablePurgeProtection: true/, 'Key Vault must enable purge protection'],
      [/defaultAction: 'Deny'/, 'Key Vault network ACLs must default to Deny'],
    ]),
  },
  {
    id: 'absent-resources',
    check: (t) => {
      const banned = [
        ['Microsoft.DBforPostgreSQL/flexibleServers/firewallRules', 'a PostgreSQL firewall rule'],
        ['Microsoft.Storage/storageAccounts', 'a storage account (content is stored in PostgreSQL)'],
        ['Microsoft.KeyVault/managedHSMs', 'a Managed HSM'],
      ];
      return [t.main, ...Object.values(t.modules)].flatMap((f) =>
        banned.filter(([type]) => f.text.includes(type)).map(([, what]) => `${f.rel} declares ${what}`));
    },
  },
  {
    id: 'waf-prevention',
    check: (t) => ['application-gateway.bicep', 'frontdoor.bicep'].filter((f) => {
      const modes = [...(t.modules[f]?.text ?? '').matchAll(/\bmode: '(\w+)'/g)].map((m) => m[1]);
      return !modes.length || modes.some((m) => m !== 'Prevention');
    }).map((f) => `${f}: every WAF policy must run in Prevention mode`),
  },
  {
    id: 'device-edge-allow-list',
    check: (t) => {
      const agw = t.modules['application-gateway.bicep']?.text ?? '';
      const prefixes = arrayVar(agw, 'allowedPrefixes');
      const routed = ['ingestPaths', 'controlPaths', 'contentPaths'].flatMap((name) => arrayVar(agw, name));
      const out = [];
      if (!prefixes.length) out.push('the device edge has no path allow-list');
      for (const path of routed) {
        if (!prefixes.some((p) => path.replace(/\*$/, '').startsWith(p))) out.push(`routed path ${path} is not in the WAF allow-list`);
      }
      return out.concat(missing(agw, [
        [/defaultBackendAddressPool: \{ id: '\$\{id\}\/backendAddressPools\/unrouted' \}/, 'unmatched device paths must go to the empty pool, not an app'],
        [/headerName: 'X-Client-Cert', headerValue: '\{var_client_certificate\}'/, 'the device edge must forward the client certificate in X-Client-Cert'],
      ]));
    },
  },
  {
    id: 'no-secrets-in-params',
    check: (t) => t.params.flatMap((p) => p.text.split(/\r?\n/)
      .map((line, i) => [line, i + 1])
      .filter(([line]) => /^param\s+\w*(password|secret|key|token)\w*\s*=\s*'[^']+'/i.test(line) && !/KeyId\s*=/.test(line))
      .map(([, n]) => `${p.rel}:${n} assigns a secret-looking literal`)),
  },
];

export const ruleIds = rules.map((r) => r.id);

export function check(tree = loadTree()) {
  return rules.flatMap((r) => r.check(tree).map((message) => ({ rule: r.id, message })));
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const findings = check();
  for (const f of findings) console.log(`FAIL ${f.rule}: ${f.message}`);
  console.log(findings.length ? `${findings.length} finding(s)` : `ok: ${ruleIds.length} rules hold`);
  process.exit(findings.length ? 1 : 0);
}
