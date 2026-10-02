// check-infra.test.mjs — the suite behind the acceptance property: the Azure deployment is
// checkable without an Azure subscription.
//
// Two kinds of test. The first kind runs the checker over the real infra/ tree and docs/05, so a
// regression in the Bicep or a drift in the document fails here. The second kind feeds the rules
// synthetic sources, so a rule that silently stops detecting something fails here too — a check that
// cannot fail is not a check.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';

import {
  INFRA_ROOT,
  DEFAULT_DOC,
  loadInfraTree,
  loadInventory,
  checkParams,
  checkNoHardcodedRegion,
  checkNoHardcodedSku,
  checkNoSecrets,
  checkArchitecturalProperties,
  checkInventory,
  checkEnvironments,
  checkCostModel,
  parseDocInventory,
  parseDocAbsent,
  normaliseLabel,
  parseCostModel,
  parseParams,
  runAll,
  stripComments,
} from './check-infra.mjs';

const docText = readFileSync(DEFAULT_DOC, 'utf8');
const files = loadInfraTree();
const inventory = loadInventory();
const byRel = new Map(files.map((f) => [f.rel, f]));

/** Build a synthetic Bicep file record, so rule tests never touch the disk. */
function file(rel, text) {
  return { path: join(INFRA_ROOT, rel), rel, text, lines: text.split(/\r?\n/) };
}

// ---------------------------------------------------------------------------------------------
// The real tree

test('the infra tree passes every rule', () => {
  const { findings } = runAll({});
  assert.deepEqual(
    findings.map((f) => `${f.rule} ${f.file}:${f.line} ${f.message}`),
    [],
    'infra must satisfy every static rule',
  );
});

test('the §3.3 module layout is complete', () => {
  const onDisk = files.filter((f) => f.rel.startsWith('modules/')).map((f) => f.rel);
  for (const mod of [
    'modules/network.bicep',
    'modules/private-endpoints.bicep',
    'modules/log-analytics.bicep',
    'modules/registry.bicep',
    'modules/postgres.bicep',
    'modules/storage-ciphertext.bicep',
    'modules/storage-exports.bicep',
    'modules/keyvault.bicep',
    'modules/managed-hsm.bicep',
    'modules/container-apps-env.bicep',
    'modules/container-app.bicep',
    'modules/container-app-job.bicep',
    'modules/frontdoor.bicep',
    'modules/waf.bicep',
    'modules/static-web-app.bicep',
    'modules/monitoring.bicep',
    'modules/budget.bicep',
  ]) {
    assert.ok(onDisk.includes(mod), `${mod} must exist`);
  }
  assert.ok(existsSync(join(INFRA_ROOT, 'main.bicep')), 'main.bicep must exist');
});

// ---------------------------------------------------------------------------------------------
// The two architectural properties (docs/05 §2, §2.1, C15, D7)

test('content-vault is instantiated with internal ingress only', () => {
  const main = byRel.get('main.bicep').text;
  const block = main.match(/module\s+\w*content\w*vault\w*\s+'[^']*container-app\.bicep'[\s\S]*?\n\}/i);
  assert.ok(block, 'main.bicep must instantiate the content-vault container app');
  assert.match(block[0], /ingress\s*:\s*'internal'/, 'content-vault must be internal-only (D7)');
  assert.doesNotMatch(block[0], /ingress\s*:\s*'external'/, 'content-vault must never take external ingress');
});

test('Front Door has no route to content-vault', () => {
  const fd = byRel.get('modules/frontdoor.bicep').text;
  assert.doesNotMatch(stripComments(fd), /content[-_]?vault/i, 'Front Door must not reference content-vault (C15, D7)');
  const routes = fd.match(/patternsToMatch:\s*\[[\s\S]*?\]/g) ?? [];
  assert.ok(routes.length >= 2, 'the device and analyst routes must both exist');
  for (const route of routes) {
    assert.doesNotMatch(route, /content[-_]?vault/i);
  }
});

test('content-vault has no public endpoint anywhere in the composition', () => {
  const main = byRel.get('main.bicep').text;
  const vaultBlocks = [...main.matchAll(/module\s+\w*content\w*vault\w*[\s\S]*?\n\}/gi)];
  assert.equal(vaultBlocks.length, 1, 'exactly one content-vault instantiation');
  for (const dir of ['frontdoor.bicep', 'static-web-app.bicep']) {
    assert.doesNotMatch(stripComments(byRel.get(`modules/${dir}`).text), /content[-_]?vault/i, `${dir} must not reach content-vault`);
  }
  // The URL is handed only to query-api, and it is an internal FQDN, not a Front Door hostname.
  assert.match(main, /SAC_CONTENT_VAULT_URL',\s*value:\s*'http:\/\/\$\{contentVaultApp\.outputs\.fqdn\}/);
});

test('the database is not reachable from outside the VNet', () => {
  const pg = byRel.get('modules/postgres.bicep').text;
  assert.match(pg, /publicNetworkAccess\s*:\s*'Disabled'/, 'publicNetworkAccess must be Disabled');
  assert.match(pg, /delegatedSubnetResourceId/, 'the server must join the delegated subnet');
  assert.match(pg, /privateDnsZoneArmResourceId/, 'the server must resolve only through the private DNS zone');
  assert.doesNotMatch(stripComments(pg), /firewallRules/, 'the server must have no firewall rules at all (§2.1)');
  for (const f of files.filter((x) => x.rel.startsWith('modules/'))) {
    assert.doesNotMatch(f.text, /flexibleServers\/firewallRules/, `${f.rel} must not create a firewall rule`);
  }
});

test('no PaaS resource accepts a public connection', () => {
  for (const rel of ['modules/postgres.bicep', 'modules/storage-ciphertext.bicep', 'modules/storage-exports.bicep', 'modules/keyvault.bicep', 'modules/registry.bicep', 'modules/network.bicep']) {
    assert.doesNotMatch(byRel.get(rel).text, /publicNetworkAccess\s*:\s*'Enabled'/, `${rel} must not enable public network access`);
  }
  const account = byRel.get('modules/storage-ciphertext.bicep').text;
  assert.match(account, /allowSharedKeyAccess\s*:\s*false/);
  assert.match(account, /allowBlobPublicAccess\s*:\s*false/);
  const vault = byRel.get('modules/keyvault.bicep').text;
  assert.match(vault, /enablePurgeProtection\s*:\s*(true|enablePurgeProtection)/);
  assert.match(vault, /enableRbacAuthorization\s*:\s*true/);
});

test('the Container Apps environment uses an internal load balancer', () => {
  const env = byRel.get('modules/container-apps-env.bicep').text;
  assert.match(env, /internal\s*:\s*internalLoadBalancer/, 'the environment LB must be internal');
  assert.match(env, /param internalLoadBalancer bool = true/, 'internalLoadBalancer must default to true');
});

test('unwrap is held by exactly one identity, and it is content-vault', () => {
  const main = byRel.get('main.bicep').text;
  const unwrap = main.match(/unwrapPrincipalIds:\s*\[([\s\S]*?)\]/);
  assert.ok(unwrap, 'main.bicep must pass unwrapPrincipalIds');
  const ids = unwrap[1].split(',').map((s) => s.trim()).filter(Boolean);
  assert.equal(ids.length, 1, 'exactly one principal may unwrap per-tenant KEKs (C15, D7)');
  assert.match(ids[0], /identityVault\.properties\.principalId/);
  const vaultMod = byRel.get('modules/keyvault.bicep').text;
  assert.equal((stripComments(vaultMod).match(/roleKeyVaultCryptoUser/g) ?? []).length, 2, 'the unwrap role definition is used in one resource and one variable declaration');
});

// ---------------------------------------------------------------------------------------------
// Parameterisation, secrets, environments

test('every module parameter is typed and described', () => {
  for (const f of files.filter((x) => x.rel.endsWith('.bicep'))) {
    for (const p of parseParams(f.text)) {
      assert.ok(p.type, `${f.rel}:${p.line} ${p.name} has no type`);
      assert.ok(p.hasDescription, `${f.rel}:${p.line} ${p.name} has no @description`);
    }
  }
});

test('no region or SKU is hard-coded in a module', () => {
  assert.deepEqual(checkNoHardcodedRegion(files), []);
  assert.deepEqual(checkNoHardcodedSku(files), []);
});

test('the environment ladder has a parameter file per environment, with no credential in any of them', () => {
  assert.deepEqual(checkEnvironments(files, docText), []);
  const params = files.filter((f) => f.rel.startsWith('params/'));
  assert.ok(params.length >= 3);
  for (const p of params) {
    assert.match(p.text, /^using '\.\.\/main\.bicep'/m, `${p.rel} must use the composition`);
    assert.match(p.text, /param location =/);
    assert.match(p.text, /param environment =/);
    assert.doesNotMatch(stripComments(p.text), /(password|secret|accountKey|connectionString)/i);
  }
});

test('PostgreSQL sizing differs per environment, as §3.1 requires', () => {
  const dev = byRel.get('params/dev.bicepparam').text;
  const staging = byRel.get('params/staging.bicepparam').text;
  const prod = byRel.get('params/prod.eastus.bicepparam').text;
  assert.match(dev, /postgresSkuName = 'B_Standard_B2s'/);
  assert.match(dev, /postgresHaMode = 'Disabled'/);
  assert.match(staging, /postgresSkuName = 'D2ds_v5'/);
  assert.match(staging, /postgresHaMode = 'Disabled'/);
  assert.match(prod, /postgresSkuName = 'D2ds_v5'/);
  assert.match(prod, /postgresHaMode = 'ZoneRedundant'/);
  assert.match(prod, /postgresBackupRetentionDays = 35/);
  assert.match(prod, /wafMode = 'Prevention'/);
  assert.match(dev, /wafMode = 'Detection'/);
});

test('every container app and job runs as a user-assigned managed identity', () => {
  const main = byRel.get('main.bicep').text;
  const calls = [...main.matchAll(/module\s+\w+\s+'[^']*container-app(?:-job)?\.bicep'/g)];
  const bindings = [...main.matchAll(/userAssignedIdentityId\s*:/g)];
  assert.ok(calls.length >= 7, 'four services and three jobs are instantiated');
  assert.ok(bindings.length >= calls.length, `${calls.length} calls need ${calls.length} identity bindings`);
  assert.doesNotMatch(stripComments(main), /=\s*'[^']*(password|AccountKey|connectionString)/i, 'no credential literal may appear in the composition (§5.1)');
});

// ---------------------------------------------------------------------------------------------
// The inventory diff, both directions

test('every §2 inventory row is implemented and nothing undocumented is deployed', () => {
  const docRows = parseDocInventory(docText);
  assert.ok(docRows.length >= 18, `the §2 table should have at least 18 rows, found ${docRows.length}`);
  assert.deepEqual(checkInventory(files, docText, inventory), []);
});

test('the §2.2 deliberately-absent list stays absent', () => {
  const absent = parseDocAbsent(docText);
  assert.ok(absent.length >= 8, `the §2.2 table should have at least 8 rows, found ${absent.length}`);
  for (const entry of inventory.deliberatelyAbsent) {
    assert.ok(
      absent.some((r) => r.key.startsWith(normaliseLabel(entry.resource).slice(0, 24))),
      `"${entry.resource}" must match a §2.2 row`,
    );
    for (const type of entry.forbiddenTypes) {
      for (const f of files) {
        assert.ok(!f.text.includes(type), `${f.rel} must not declare ${type} (§2.2)`);
      }
    }
  }
});

test('a document row with no implementation is reported, not ignored', () => {
  const docWithExtraRow = `${docText.slice(0, docText.indexOf('### 2.1'))}\n| **A resource nobody built** | n/a | Because |\n\n${docText.slice(docText.indexOf('### 2.1'))}`;
  const findings = checkInventory(files, docWithExtraRow, inventory);
  assert.ok(findings.some((f) => f.rule === 'inventory-missing' && /A resource nobody built/.test(f.message)));
});

test('an inventory entry with no document row is reported too', () => {
  const docWithoutFrontDoor = docText.replace(/\| \*\*Front Door Premium\*\* \+ WAF policy \|[^\n]*\n/, '');
  const findings = checkInventory(files, docWithoutFrontDoor, inventory);
  assert.ok(findings.some((f) => f.rule === 'inventory-extra' && /Front Door/.test(f.message)));
});

test('an undocumented resource is caught by the layout check', () => {
  const withExtra = [...files, file('modules/queue.bicep', "param location string = 'eastus'\n")];
  const findings = checkInventory(withExtra, docText, inventory);
  assert.ok(findings.some((f) => f.rule === 'layout-extra' && /queue\.bicep/.test(f.message)));
});

test('a module named by the layout but missing from disk is caught', () => {
  const withoutWaf = files.filter((f) => f.rel !== 'modules/waf.bicep');
  const findings = checkInventory(withoutWaf, docText, inventory);
  assert.ok(findings.some((f) => f.rule === 'layout-missing' && /waf\.bicep/.test(f.message)));
});

test('a deliberately-absent resource type in the tree fails the build', () => {
  const withAks = [...files, file('modules/k8s.bicep', "resource aks 'Microsoft.ContainerService/managedClusters@2024-01-01' = {\n  name: 'x'\n}\n")];
  const findings = checkInventory(withAks, docText, inventory);
  assert.ok(findings.some((f) => f.rule === 'deliberately-absent' && /managedClusters/.test(f.message)));
});

// ---------------------------------------------------------------------------------------------
// Rule-level tests: a check that cannot fail is not a check

test('an undescribed parameter is reported', () => {
  const findings = checkParams([file('modules/x.bicep', 'param location string\n')]);
  assert.ok(findings.some((f) => f.rule === 'param-description'));
});

test('a hard-coded region is reported', () => {
  const findings = checkNoHardcodedRegion([file('modules/x.bicep', "resource r 'Microsoft.X/y@2024-01-01' = {\n  location: 'eastus'\n}\n")]);
  assert.ok(findings.some((f) => f.rule === 'region-hardcoded'));
  // ...but an @allowed list naming regions is a constraint, not a hard-coded choice.
  const allowed = checkNoHardcodedRegion([file('modules/y.bicep', "@allowed(['eastus', 'westus'])\nparam location string\n")]);
  assert.deepEqual(allowed, []);
});

test('a hard-coded SKU is reported', () => {
  const findings = checkNoHardcodedSku([file('modules/x.bicep', "resource r 'Microsoft.X/y@2024-01-01' = {\n  properties: {\n    tier: 'Premium'\n  }\n}\n")]);
  assert.ok(findings.some((f) => f.rule === 'sku-hardcoded'));
});

test('plaintext credential material is reported', () => {
  for (const text of [
    "param adminPassword string = 'hunter2hunter2'\n",
    "var key = 'AccountKey=abcdefghijklmnop=='\n",
    "var pem = '-----BEGIN PRIVATE KEY-----'\n",
    `var token = '${'A'.repeat(80)}'\n`,
  ]) {
    const findings = checkNoSecrets([file('modules/x.bicep', text)]);
    assert.ok(findings.some((f) => f.rule.startsWith('secret')), `should flag: ${text.slice(0, 30)}`);
  }
});

test('an external content-vault fails the architectural check', () => {
  const fakeMain = file('main.bicep', [
    "module contentVaultApp 'modules/container-app.bicep' = {",
    '  name: "content-vault"',
    '  params: {',
    "    ingress: 'external'",
    '  }',
    '}',
  ].join('\n'));
  const findings = checkArchitecturalProperties([fakeMain, ...files.filter((f) => f.rel !== 'main.bicep')]);
  assert.ok(findings.some((f) => f.rule === 'content-vault-ingress'));
});

test('a Front Door route that mentions content-vault fails', () => {
  const fd = file('modules/frontdoor.bicep', (byRel.get('modules/frontdoor.bicep').text + "\nvar leakedOrigin = 'content-vault.internal'\n"));
  const findings = checkArchitecturalProperties([...files.filter((f) => f.rel !== 'modules/frontdoor.bicep'), fd]);
  assert.ok(findings.some((f) => f.rule === 'content-vault-route'));
});

test('a PostgreSQL firewall rule fails', () => {
  const pg = file('modules/postgres.bicep', (byRel.get('modules/postgres.bicep').text + "\nresource fw 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules@2024-08-01' = {\n  name: 'allow-all'\n}\n"));
  const findings = checkArchitecturalProperties([...files.filter((f) => f.rel !== 'modules/postgres.bicep'), pg]);
  assert.ok(findings.some((f) => f.rule === 'postgres-firewall'));
});

test('a public database fails the architectural check', () => {
  const pg = file('modules/postgres.bicep', byRel.get('modules/postgres.bicep').text.replace(/publicNetworkAccess\s*:\s*'Disabled'/, "publicNetworkAccess: 'Enabled'"));
  const findings = checkArchitecturalProperties([...files.filter((f) => f.rel !== 'modules/postgres.bicep'), pg]);
  assert.ok(findings.some((f) => f.rule === 'private-only' && /PostgreSQL/.test(f.message)));
});

test('a vault without purge protection fails', () => {
  const kv = file('modules/keyvault.bicep', byRel.get('modules/keyvault.bicep').text.replace(/enablePurgeProtection\s*:\s*enablePurgeProtection/, 'enablePurgeProtection: false'));
  const findings = checkArchitecturalProperties([...files.filter((f) => f.rel !== 'modules/keyvault.bicep'), kv]);
  assert.ok(findings.some((f) => f.rule === 'private-only' && /purge protection/.test(f.message)));
});

// ---------------------------------------------------------------------------------------------
// The cost model

test('the cost model reconciles with the document', () => {
  const text = readFileSync(join(INFRA_ROOT, 'cost-model.md'), 'utf8');
  assert.deepEqual(checkCostModel(text, docText), []);
});

test('the cost model recomputes the document’s own arithmetic', () => {
  const model = parseCostModel(readFileSync(join(INFRA_ROOT, 'cost-model.md'), 'utf8'));
  const unit = model.unitPrices;
  const subtotal = model.lines.reduce((sum, line) => {
    const computed = line.terms.reduce((s, t) => {
      if (t.unit) return s + t.quantity * unit[t.unit];
      return s + t.quantity * t.price;
    }, 0);
    assert.ok(Math.abs(computed - line.total) < 0.01, `${line.name}: ${computed} vs stated ${line.total}`);
    return sum + computed;
  }, 0);
  assert.ok(Math.abs(subtotal - model.subtotal) < 0.01, `subtotal ${subtotal} vs stated ${model.subtotal}`);
  // The document's estimate is this subtotal plus the shared allocation it names.
  assert.ok(model.estimatePerTenantPerMonth.low >= model.subtotal);
  assert.ok(model.estimatePerTenantPerMonth.high - model.estimatePerTenantPerMonth.low <= 20);
});

test('a cost line whose terms do not sum to its total fails', () => {
  const text = readFileSync(join(INFRA_ROOT, 'cost-model.md'), 'utf8');
  const broken = text.replace('"total": 27.00', '"total": 99.00');
  const findings = checkCostModel(broken, docText);
  assert.ok(findings.some((f) => f.rule === 'cost-model' && /Container Apps \+ jobs/.test(f.message)));
});

test('a cost line that disagrees with the document by more than a dollar fails', () => {
  const text = readFileSync(join(INFRA_ROOT, 'cost-model.md'), 'utf8');
  const broken = text.replace('"total": 391.64', '"total": 291.64');
  const findings = checkCostModel(broken, docText);
  assert.ok(findings.some((f) => f.rule === 'cost-model' && /PostgreSQL/.test(f.message)));
});

test('a subtotal beyond the declared rounding budget fails', () => {
  const text = readFileSync(join(INFRA_ROOT, 'cost-model.md'), 'utf8');
  const broken = text.replace('"subtotal": 826.66', '"subtotal": 700.00');
  const findings = checkCostModel(broken, docText);
  assert.ok(findings.some((f) => f.rule === 'cost-model' && /subtotal/.test(f.message)));
});

test('a documented finding without the fields that make it checkable fails', () => {
  const text = readFileSync(join(INFRA_ROOT, 'cost-model.md'), 'utf8');
  // Remove the `impact` field from one finding: a finding that states a problem without an impact
  // cannot be reviewed, so the checker refuses it.
  const broken = text.replace(/,\n      "impact": "If the per-key charge[^"]*"/, '');
  assert.notEqual(broken, text, 'the test must actually remove a field');
  const findings = checkCostModel(broken, docText);
  assert.ok(findings.some((f) => f.rule === 'cost-model' && /missing "impact"/.test(f.message)));
});

test('the two material findings are present and reference real lines', () => {
  const model = parseCostModel(readFileSync(join(INFRA_ROOT, 'cost-model.md'), 'utf8'));
  const ids = model.findings.map((f) => f.id);
  assert.ok(ids.includes('FD-SHARED-OR-PER-TENANT'), 'the Front Door sharing finding is the one that changes the master document’s correction');
  assert.ok(ids.includes('KV-HSM-KEY-PRICE'), 'the missing Key Vault key unit price must be recorded');
  for (const f of model.findings) {
    for (const line of f.affectsLines ?? []) {
      assert.ok(model.lines.some((l) => l.name === line), `${f.id} names ${line}`);
    }
  }
});

// ---------------------------------------------------------------------------------------------
// What is deliberately not verified

test('the documentation states that deployment is not verified', () => {
  const readme = readFileSync(join(INFRA_ROOT, 'README.md'), 'utf8');
  assert.match(readme, /NOT VERIFIED/, 'the README must say deployment is not verified');
  assert.match(readme, /az deployment group create/, 'and name the exact command a human would run');
  assert.match(readme, /precondition/i, 'and name the preconditions');
});
