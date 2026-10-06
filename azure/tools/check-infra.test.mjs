import { test } from 'node:test';
import assert from 'node:assert/strict';

import { check, loadTree, ruleIds } from './check-infra.mjs';

const clone = (tree) => structuredClone(tree);

function mutate(tree, file, from, to) {
  const target = file === 'main.bicep' ? tree.main : tree.modules[file];
  assert.ok(target.text.includes(from), `${file} no longer contains ${JSON.stringify(from)}; update the test`);
  target.text = target.text.replace(from, to);
  return tree;
}

const rulesFailing = (tree) => new Set(check(tree).map((f) => f.rule));

test('the templates satisfy every rule', () => {
  assert.deepEqual(check(), []);
});

// Each perturbation must trip exactly the rule that guards it, so a rule that silently stopped
// matching would fail here.
const perturbations = [
  ['internal-only-apps', 'main.bicep', "appName: 'content-vault'\n    environmentId: environmentModule.outputs.environmentId\n    identity: id.vault\n    ingress: 'internal'", "appName: 'content-vault'\n    environmentId: environmentModule.outputs.environmentId\n    identity: id.vault\n    ingress: 'external'"],
  ['environment-internal', 'container-apps-env.bicep', 'internal: true', 'internal: false'],
  ['postgres-private-entra-only', 'postgres.bicep', "passwordAuth: 'Disabled'", "passwordAuth: 'Enabled'"],
  ['vault-locked', 'keyvault.bicep', 'enablePurgeProtection: true', 'enablePurgeProtection: false'],
  ['absent-resources', 'postgres.bicep', "resource database 'Microsoft.DBforPostgreSQL/flexibleServers/databases", "resource database 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules"],
  ['waf-prevention', 'frontdoor.bicep', "mode: 'Prevention'", "mode: 'Detection'"],
  ['device-edge-allow-list', 'application-gateway.bicep', "backendAddressPools/unrouted' }\n          defaultBackendHttpSettings", "backendAddressPools/apps' }\n          defaultBackendHttpSettings"],
  ['device-edge-allow-list', 'application-gateway.bicep', "var allowedPrefixes = ['/v1/events', ", "var allowedPrefixes = ["],
];

for (const [rule, file, from, to] of perturbations) {
  test(`${rule} catches a change to ${file}`, () => {
    const failing = rulesFailing(mutate(clone(loadTree()), file, from, to));
    assert.deepEqual([...failing], [rule]);
  });
}

test('an unused module is reported', () => {
  const tree = clone(loadTree());
  tree.modules['orphan.bicep'] = { rel: 'modules/orphan.bicep', text: '' };
  assert.deepEqual([...rulesFailing(tree)], ['modules-referenced']);
});

test('a secret literal in a parameter file is reported', () => {
  const tree = clone(loadTree());
  tree.params.push({ rel: 'params/bad.bicepparam', text: "param adminPassword = 'hunter2'\n" });
  assert.deepEqual([...rulesFailing(tree)], ['no-secrets-in-params']);
});

test('every rule is exercised by a perturbation', () => {
  const covered = new Set([...perturbations.map(([rule]) => rule), 'modules-referenced', 'no-secrets-in-params']);
  assert.deepEqual(ruleIds.filter((id) => !covered.has(id)), []);
});
