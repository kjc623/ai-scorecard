# Cost model — docs/05-platform-delivery.md §11, made checkable

This file does not restate the document's numbers: it **recomputes** them from the unit prices in
§11.1 and flags every place the arithmetic and the document disagree. `azure/tools/check-infra.mjs`
parses the block below, recomputes each line from its terms, and compares the result with the
document's own §11.2 table. A figure that does not reconcile fails `node --test azure/tools/`.

**Basis.** Azure list prices, East US, pay-as-you-go, no committed-use discount, no enterprise
agreement, no reservations, no savings plan — exactly §11.1's basis. The unit prices are the
document's; they are **estimates, not measurements**, and they must be re-baselined against the Azure
pricing calculator before any commercial commitment. Nothing in this file was retrieved from Azure:
there is no subscription, and no price was looked up, so **no price here is verified against the
live price list.**

**Sizing.** §11.1's M1 default: 5,000 devices, ~4,000 AI-active users, ~12,000 submission events/day.

## The model

<!-- cost-model:start -->
{
  "basis": "Azure list prices, East US, pay-as-you-go, no discounts, as of 2026-10-02 (docs/05 §11.1)",
  "sizing": "M1 default: 5000 devices, ~12000 submission events/day",
  "docRoundingTolerance": 3,
  "unitPrices": {
    "containerAppsActiveVcpuHour": 0.0864,
    "containerAppsActiveGibHour": 0.0108,
    "containerAppsPerMillionRequests": 0.40,
    "postgresD2dsV5Hour": 0.252,
    "postgresStorageGibMonth": 0.115,
    "postgresGeoBackupGibMonth": 0.10,
    "frontDoorBasePerMonth": 330,
    "frontDoorPer10kRequests": 0.012,
    "frontDoorEgressGib": 0.085,
    "staticWebAppPerMonth": 9,
    "keyVaultPer10kOps": 0.03,
    "logAnalyticsPerGib": 2.76,
    "blobHotGibMonth": 0.0184,
    "managedHsmPerHour": 4.6
  },
  "lines": [
    {
      "name": "Container Apps + jobs",
      "docLabel": "Container apps + jobs (incl. 2 migrations/month)",
      "derivation": "90 vCPU-h active (75 measured-shaped at ~0.14 events/s, +15 for two timed jobs) and 180 GiB-h (2 GiB per vCPU for every app), plus 43.2M requests",
      "terms": [
        { "unit": "containerAppsActiveVcpuHour", "quantity": 90 },
        { "unit": "containerAppsActiveGibHour", "quantity": 180 },
        { "unit": "containerAppsPerMillionRequests", "quantity": 43.2 }
      ],
      "total": 27.00
    },
    {
      "name": "PostgreSQL",
      "docLabel": "PostgreSQL (compute + HA + storage + backup)",
      "derivation": "730 h primary + 730 h zone-redundant standby = 1460 h at the D2ds_v5 rate; 128 GiB provisioned; ~90 GiB geo-redundant backup",
      "terms": [
        { "unit": "postgresD2dsV5Hour", "quantity": 1460 },
        { "unit": "postgresStorageGibMonth", "quantity": 128 },
        { "unit": "postgresGeoBackupGibMonth", "quantity": 90 }
      ],
      "total": 391.64
    },
    {
      "name": "Front Door Premium",
      "docLabel": "Front Door Premium (base + requests + egress)",
      "derivation": "one profile base + 43.2M requests at $0.012/10k + ~4 GiB egress",
      "terms": [
        { "unit": "frontDoorBasePerMonth", "quantity": 1 },
        { "unit": "frontDoorPer10kRequests", "quantity": 4320 },
        { "unit": "frontDoorEgressGib", "quantity": 4 }
      ],
      "total": 382.18
    },
    {
      "name": "Static Web App",
      "docLabel": "Static Web App",
      "derivation": "one Standard static site",
      "terms": [
        { "unit": "staticWebAppPerMonth", "quantity": 1 }
      ],
      "total": 9.00
    },
    {
      "name": "Key Vault",
      "docLabel": "Key Vault",
      "derivation": "~100k operations plus ~30 HSM-backed keys; see finding KV-HSM-KEY-PRICE for why the key charge is not a unit price in the document",
      "terms": [
        { "unit": "keyVaultPer10kOps", "quantity": 10 },
        { "quantity": 30, "price": 0.0667 }
      ],
      "total": 2.30
    },
    {
      "name": "Log Analytics",
      "docLabel": "Log Analytics",
      "derivation": "~5 GiB/month of metadata-only telemetry at 10% trace sampling (§10.4)",
      "terms": [
        { "unit": "logAnalyticsPerGib", "quantity": 5 }
      ],
      "total": 13.80
    },
    {
      "name": "Blob — events and operational data",
      "docLabel": "Blob — events and operational data",
      "derivation": "~40 GiB hot",
      "terms": [
        { "unit": "blobHotGibMonth", "quantity": 40 }
      ],
      "total": 0.74
    }
  ],
  "subtotal": 826.66,
  "sharedRegional": {
    "derivation": "docs/05 §11.3: Container Apps environment ~$50 + ACR Premium and geo-replication ~$60 + workspace overhead ~$25 + budgets and diagnostics ~$10",
    "total": 145,
    "perTenantAt100": 1.45,
    "perTenantAt20": 7.25
  },
  "estimatePerTenantPerMonth": { "low": 828, "high": 840 },
  "m3Addition": {
    "derivation": "docs/05 §11.4: attachment ciphertext at 50–500 GB/year, plus ~$3 of grant, retrieval and log operations",
    "blobHot12MonthRetention": { "low": 0.92, "high": 9.20 },
    "tieredHotCoolArchive": { "low": 0.24, "high": 2.35 },
    "operations": 3.00
  },
  "managedHsmPerMonth": 3358,
  "findings": [
    {
      "id": "FD-SHARED-OR-PER-TENANT",
      "section": "§2 inventory, §11.2, §11.3, §11.6",
      "affectsLines": ["Front Door Premium"],
      "statement": "§11.2 charges the $330 Front Door Premium base per tenant, and §11.6 leans on it ('$330, or 40% of the tenant … the reason adding tenants is accretive'), but §2 describes one regional environment per residency region with resources shared across all tenants in it, and §11.3's shared-regional list does not mention Front Door. This deployment has exactly ONE Front Door profile per regional environment (main.bicep instantiates frontdoor.bicep once), so the base is a regional fixed cost, not a per-tenant one.",
      "impact": "If the base is shared, the per-tenant figure is $826.66 - $330 + $1.45 shared allocation = about $498 at 100 tenants, not $830–840. The document's own 'ASSUMPTION' that its figure is 'materially above the $300–700/month band quoted in master §1.4' is then wrong: at 100 tenants the model lands inside that band, and the master's correction from ≈$300–700 to ≈$830–840 is only correct if each tenant gets its own Front Door profile. Either §2 must say the profile is per tenant or §11.2/§11.6 must move the base into §11.3."
    },
    {
      "id": "KV-HSM-KEY-PRICE",
      "section": "§11.1, §11.2",
      "affectsLines": ["Key Vault"],
      "statement": "§11.1's unit table says HSM-backed keys 'carry an additional per-key monthly charge' but gives no price, and §11.2's line assumes ~30 HSM-backed keys cost $2 in total — about $0.067 per key per month. This model reproduces the document's $2 so the reconciliation is exact, but the figure is not derived from a stated unit price.",
      "impact": "If the per-key charge is a real per-key rate (the usual shape of a Premium-vault key charge, historically around $5 per HSM-protected key per month), the line is $150 rather than $3 and the per-tenant estimate rises by about $147 to ≈$973. §11.1 must state the per-key price before this line can be trusted."
    },
    {
      "id": "REQUESTS-DERIVATION",
      "section": "§11.2",
      "affectsLines": ["Container Apps + jobs", "Front Door Premium"],
      "statement": "43.2M requests/month appears as a given in two lines. It is exactly 5,000 devices x 288 polls/day x 30 days — a device health poll every five minutes — not 12,000 events/day, which is 0.36M/month. The document does not show this derivation.",
      "impact": "The request lines are driven by the health-poll interval, not by event volume: lengthening the poll interval to 15 minutes would cut both request lines by about $35/month per tenant. This is stated so the derivation is reviewable rather than guessed."
    },
    {
      "id": "EVENT-VOLUME-CONSISTENT",
      "section": "§11.2",
      "affectsLines": [],
      "statement": "12,000 events/day x 1.5 KiB = 17.6 MiB/day = 528 MiB/month = 6.19 GiB/year, inside brief §3.1's 5–9 GB/year band. The three-year store plus indexes and ops rows lands at about 45 GiB of the 128 GiB provisioned disk, which matches §11.2's storage line.",
      "impact": "None: this is the one line where the document's arithmetic is fully shown and this model reproduces it exactly."
    }
  ],
  "notModelled": [
    "engineering and support headcount (§11.5 excludes it deliberately)",
    "reserved capacity or savings plan discounts, 20–40% off compute (§2.2 declines them in v1)",
    "archive retrieval charges at M3 (§11.4 says so)",
    "cross-region egress for a second residency region (§11.3's baseline is per region, multiplicative)",
    "Managed HSM in the per-tenant estimate: §11.6 prices it at +$3,358/month and the deployment instantiates it only per contract"
  ]
}
<!-- cost-model:end -->

## What this model says, and what it does not

**The lines reconcile.** Every line reproduces the document's figure within the one-dollar rounding
the document itself uses, and the subtotal lands at **$826.66** against the document's **$828**
(a $1.34 difference from rounding seven lines to whole dollars). The document's estimate of
**$830–840** per tenant per month therefore follows from §11.2's own arithmetic plus the $1–9 shared
allocation and $1–3 unmodelled growth it names.

**Two findings change the number, and one of them changes the master document's correction.**

1. **Front Door's $330 base is regional in this deployment, not per-tenant** (`FD-SHARED-OR-PER-TENANT`).
   The architecture in `main.bicep` creates one profile per regional environment, shared by every
   tenant in the region — which is what §2 describes and what §11.3's "allocated rather than charged"
   list implies, but not what §11.2's per-tenant table charges. At 100 tenants the per-tenant figure
   is **≈$498**, not $830–840, and the master document's correction from ≈$300–700 to ≈$830–840 is
   only right if each tenant receives its own Front Door profile.
2. **The Key Vault key charge has no unit price** (`KV-HSM-KEY-PRICE`). Reproducing the document's $2
   for 30 HSM-backed keys hides a missing rate; at a $5/key/month shape the line is $150 and the
   estimate rises by about $147.

**What this model cannot say.** Every figure is a list-price estimate, not a measurement. Nothing here
was checked against the Azure pricing calculator or against a real invoice, because this host has no
network and no subscription. The deployable parameters in `azure/params/` set the *sizing* (SKUs, HA mode,
retention, replicas, redundancy), which is what determines which lines apply; they do not validate the
prices.
