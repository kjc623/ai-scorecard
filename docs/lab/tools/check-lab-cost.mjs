// check-lab-cost.mjs — recompute every figure in docs/lab/LAB-COST.md from its own terms.
//
// The document's whole value is that its arithmetic is checkable. This script recomputes each
// table from the unit prices it cites, so a typo in the document fails here rather than in a
// budget. Not a test of Azure: a test of the document.
//
//   node docs/lab/tools/check-lab-cost.mjs

const HOURS = 730; // a month
const DAYS = HOURS / 24; // 30.4167
const round = (n, d = 2) => Math.round(n * 10 ** d) / 10 ** d;

/** Unit prices exactly as the document states them, with the citation it gives. */
const UNIT = {
  acaVcpuHour: [0.0864, '§11.1'],
  acaGibHour: [0.0108, '§11.1'],
  acaPerMillionRequests: [0.40, '§11.1'],
  pgB1msHour: [0.0170, 'EST'],
  pgB2sHour: [0.0340, 'EST'],
  pgD2dsV5Hour: [0.252, '§11.1'],
  pgStorageGibMonth: [0.115, '§11.1'],
  acrBasicMonth: [5.00, 'EST'],
  acrPremiumGeoMonth: [60.00, '§11.3'],
  keyVaultPer10kOps: [0.03, '§11.1'],
  blobHotGibMonth: [0.0184, '§11.1'],
  privateEndpointHour: [0.01, 'EST'],
  privateDnsZoneMonth: [0.50, 'EST'],
  logAnalyticsPerGib: [2.76, '§11.1'],
  alertRuleMonth: [0.50, 'EST'],
  frontDoorPremiumBase: [330, '§11.1'],
  staticWebAppStandard: [9, '§11.1'],
  managedHsmHour: [4.60, '§11.1'],
};

const lines = [];
const add = (name, value, note = '') => lines.push({ name, value: round(value), note });

// ── Part B: the recommended lab ───────────────────────────────────────────────────────────────
add('PG compute B1ms', UNIT.pgB1msHour[0] * HOURS);
add('PG storage 32 GiB', UNIT.pgStorageGibMonth[0] * 32);
add('PG backup (within free allowance)', 0);
add('Container Apps environment', 0);
const acaUsage = 30 * UNIT.acaVcpuHour[0] + 60 * UNIT.acaGibHour[0] + 0.5 * UNIT.acaPerMillionRequests[0];
const acaGrant = 50 * UNIT.acaVcpuHour[0] + 100 * UNIT.acaGibHour[0];
add('Container Apps compute (30 vCPU-h, 60 GiB-h, 0.5M req)', acaUsage);
add('Container Apps free grant offset', -Math.min(acaGrant, acaUsage));
add('ACR Basic', UNIT.acrBasicMonth[0]);
add('Key Vault (50k ops)', 5 * UNIT.keyVaultPer10kOps[0]);
add('Blob storage + transactions', 1 * UNIT.blobHotGibMonth[0] + 0.10);
add('Private endpoint (blob)', UNIT.privateEndpointHour[0] * HOURS);
add('Private DNS zones x3', 3 * UNIT.privateDnsZoneMonth[0]);
add('Log Analytics 0.2 GiB', 0.2 * UNIT.logAnalyticsPerGib[0]);
add('Container Apps Job (schema apply)', 10 * (2 / 60) * UNIT.acaVcpuHour[0]);
add('Alerts x2', 2 * UNIT.alertRuleMonth[0]);

const labTotal = lines.reduce((s, l) => s + l.value, 0);

console.log('=== Part B: recommended lab ===');
for (const l of lines) console.log(`  ${l.name.padEnd(52)} $${l.value.toFixed(2)}`);
console.log(`  ${'TOTAL / month'.padEnd(52)} $${round(labTotal).toFixed(2)}`);
console.log(`  per day  $${round(labTotal / DAYS).toFixed(2)}`);
console.log(`  per week $${round((labTotal / DAYS) * 7).toFixed(2)}`);

// floors
const floorIdle = UNIT.pgB1msHour[0] * HOURS + UNIT.pgStorageGibMonth[0] * 32 + UNIT.acrBasicMonth[0]
  + UNIT.privateEndpointHour[0] * HOURS + 3 * UNIT.privateDnsZoneMonth[0] + 0.27;
const floorStopped = UNIT.pgStorageGibMonth[0] * 32 + UNIT.acrBasicMonth[0]
  + UNIT.privateEndpointHour[0] * HOURS + 3 * UNIT.privateDnsZoneMonth[0] + 0.27;
console.log(`\n  floor, deployed and unused        $${round(floorIdle).toFixed(2)}/month  ($${round((floorIdle / DAYS) * 7).toFixed(2)}/week)`);
console.log(`  floor, PostgreSQL stopped         $${round(floorStopped).toFixed(2)}/month  ($${round((floorStopped / DAYS) * 7).toFixed(2)}/week)`);

// a session: 35 hours of use a month
const session = 35 * UNIT.pgB1msHour[0] + 10 * (2 / 60) * UNIT.acaVcpuHour[0] + 35 * (1 / 60) * UNIT.logAnalyticsPerGib[0];
const sessionAca = 35 * UNIT.acaVcpuHour[0] + 70 * UNIT.acaGibHour[0];
console.log(`\n  a 35-hour month of USE:           $${round(session).toFixed(2)} (metered) + $${round(sessionAca).toFixed(2)} of Container Apps compute, covered by the free grant`);

// ── the traps ─────────────────────────────────────────────────────────────────────────────────
const devShape =
  UNIT.pgB2sHour[0] * HOURS + UNIT.pgStorageGibMonth[0] * 32            // PG B2s 32 GiB
  + UNIT.frontDoorPremiumBase[0] + 1                                     // Front Door Premium
  + UNIT.staticWebAppStandard[0]                                         // Static Web App Standard
  + (2.5 * UNIT.acaVcpuHour[0] + 5 * UNIT.acaGibHour[0]) * HOURS         // 3 x 0.5 + 1 vCPU, 5 GiB, continuous
  - 5.4                                                                  // the subscription-level free grant
  + UNIT.acrPremiumGeoMonth[0]                                           // ACR Premium + geo
  + 2 + 1 + 1                                                            // Key Vault, storage, logs
  + 5 * UNIT.privateEndpointHour[0] * HOURS                              // 5 private endpoints
  + 8 * UNIT.privateDnsZoneMonth[0]                                      // 8 DNS zones
  + 18 * UNIT.alertRuleMonth[0];                                         // 18 alerts
const prodShape =
  UNIT.pgD2dsV5Hour[0] * 2 * HOURS + UNIT.pgStorageGibMonth[0] * 128 + 90 * 0.10  // HA pair + 128 GiB + geo backup
  + UNIT.frontDoorPremiumBase[0] + 1
  + UNIT.staticWebAppStandard[0]
  + (4 * UNIT.acaVcpuHour[0] + 8 * UNIT.acaGibHour[0]) * HOURS
  + UNIT.acrPremiumGeoMonth[0] + 2 + 1 + 1
  + 5 * UNIT.privateEndpointHour[0] * HOURS
  + 8 * UNIT.privateDnsZoneMonth[0]
  + 18 * UNIT.alertRuleMonth[0];

console.log('\n=== the traps (a lab that is not a lab) ===');
console.log(`  dev.bicepparam deployed as-is     $${round(devShape).toFixed(0)}/month   $${round((devShape / DAYS) * 7).toFixed(0)}/week`);
console.log(`  prod.eastus.bicepparam as-is      $${round(prodShape).toFixed(0)}/month   $${round((prodShape / DAYS) * 7).toFixed(0)}/week`);
console.log(`  Managed HSM alone                 $${round(UNIT.managedHsmHour[0] * HOURS).toFixed(0)}/month   $${round((UNIT.managedHsmHour[0] * HOURS / DAYS) * 7).toFixed(0)}/week`);
console.log(`  production Container Apps floor   $${round((4 * UNIT.acaVcpuHour[0] + 8 * UNIT.acaGibHour[0]) * HOURS).toFixed(0)}/month  (4 vCPU + 8 GiB continuous)`);
console.log(`  dev Container Apps floor          $${round((2.5 * UNIT.acaVcpuHour[0] + 5 * UNIT.acaGibHour[0]) * HOURS).toFixed(0)}/month  (2.5 vCPU + 5 GiB continuous)`);
console.log(`\n  lab vs dev-as-is: ${round(devShape / labTotal, 1)}x cheaper`);
console.log(`  a forgotten week, prod shape ($${round((prodShape / DAYS) * 7).toFixed(0)}) vs a year of the lab ($${round(labTotal * 12).toFixed(0)})`);

// ── the document must agree with the arithmetic ───────────────────────────────────────────────
// A costing document whose own numbers drift from its own terms is worse than no document, so the
// figures are asserted here by string. `--check` is what a reviewer runs; without it this script is
// a readable recomputation.
const DOC = new URL('../LAB-COST.md', import.meta.url);
const expected = [
  ['lab total', `$${round(labTotal).toFixed(2)}`],
  ['lab per day', `$${round(labTotal / DAYS).toFixed(2)}`],
  ['lab per week', `$${round((labTotal / DAYS) * 7).toFixed(2)}`],
  ['floor, unused', `$${round(floorIdle).toFixed(2)}`],
  ['floor, PG stopped', `$${round(floorStopped).toFixed(2)}`],
  ['dev shape, per month', `$${round(devShape).toFixed(0)}`],
  ['dev shape, per week', `$${round((devShape / DAYS) * 7).toFixed(0)}`],
  ['prod shape, per month', Number(round(prodShape).toFixed(0)).toLocaleString('en-US')],
  ['prod shape, per week', `$${round((prodShape / DAYS) * 7).toFixed(0)}`],
  ['Managed HSM, per week', `$${round((UNIT.managedHsmHour[0] * HOURS / DAYS) * 7).toFixed(0)}`],
  ['production Container Apps floor', `$${round((4 * UNIT.acaVcpuHour[0] + 8 * UNIT.acaGibHour[0]) * HOURS).toFixed(0)}`],
  ['dev Container Apps floor', `$${round((2.5 * UNIT.acaVcpuHour[0] + 5 * UNIT.acaGibHour[0]) * HOURS).toFixed(0)}`],
];

const text = await import('node:fs').then((fs) => fs.readFileSync(DOC, 'utf8'));
const missing = expected.filter(([, needle]) => !text.includes(needle.replace('$', '$')));
console.log('\n=== the document agrees with this arithmetic ===');
for (const [label, needle] of expected) {
  const ok = text.includes(needle);
  console.log(`  ${ok ? 'ok  ' : 'MISS'} ${label.padEnd(34)} ${needle}`);
}
if (process.argv.includes('--check') && missing.length > 0) {
  console.error(`\n${missing.length} figure(s) in LAB-COST.md do not match the recomputation: ${missing.map(([l]) => l).join(', ')}`);
  process.exit(1);
}
console.log(missing.length === 0 ? '\nall document figures match' : `\n${missing.length} to reconcile`);
