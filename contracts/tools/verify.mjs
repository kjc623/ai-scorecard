// contracts/tools/verify.mjs
//
// Verification suite for the generated envelope binding.
//
//   node contracts/tools/verify.mjs          (direct)
//   node --test contracts/tools/             (the directory form; see index.js)
//
// It is deliberately not a mirror of the generator:
//   (a) it renders the output into a temporary directory and fails if the committed files differ,
//   (b) it checks the kind registry and every enum in the Go output against the schema,
//   (c) it re-derives required/optional/forbidden field sets from the schema and checks the
//       generated structs field by field, in both directions,
//   (d) it checks that every variant derived from the schema is present and reachable from the
//       decoder,
//   (e) it checks the endpoint kinds against their contract, and checks valid and invalid records
//       of every kind against the field sets and declarations it read from the schema,
//   and it runs the generated package through gofmt, go vet and its Go tests.
//
// Every expectation is re-derived from contracts/event-envelope.schema.json here, so a
// generator bug that produced matching-but-wrong output still fails.

import test, { after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";

import { CONTRACTS_DIR, GO_REL, REPO_ROOT, SCHEMA_COPY_REL, renderAll } from "./generate.mjs";

const SCHEMA_PATH = path.join(CONTRACTS_DIR, "event-envelope.schema.json");
const GO_MODULE_REL = "contracts/generated/go";

const tempDirs = [];
function makeTemp(label) {
  const dir = mkdtempSync(path.join(os.tmpdir(), `sac-verify-${label}-`));
  tempDirs.push(dir);
  return dir;
}
after(() => {
  for (const dir of tempDirs) rmSync(dir, { recursive: true, force: true });
});

function normalize(text) {
  return text.replace(/\r\n/g, "\n");
}

function run(command, args, options = {}) {
  return spawnSync(command, args, { encoding: "utf8", ...options });
}

function describeResult(result) {
  return `exit=${result.status}${result.signal ? ` signal=${result.signal}` : ""}\nstdout:\n${result.stdout ?? ""}\nstderr:\n${result.stderr ?? ""}`;
}

// ---------------------------------------------------------------------------------------
// (a) regeneration: temp directory, byte comparison
// ---------------------------------------------------------------------------------------

test("(a) a fresh generation into a temp directory is byte-identical to the committed files", () => {
  const dir = makeTemp("regen");
  const rendered = renderAll({ contractsDir: CONTRACTS_DIR });
  assert.deepEqual(
    rendered.map((file) => file.rel).sort(),
    [GO_REL, SCHEMA_COPY_REL].sort(),
    "the generator must emit exactly the Go binding and the embedded schema copy",
  );
  const problems = [];
  for (const { rel, content } of rendered) {
    const target = path.join(dir, rel);
    mkdirSync(path.dirname(target), { recursive: true });
    writeFileSync(target, content, "utf8");
    const committedPath = path.join(REPO_ROOT, rel);
    if (!existsSync(committedPath)) {
      problems.push(`${rel}: missing from the working tree`);
      continue;
    }
    const committed = normalize(readFileSync(committedPath, "utf8")).split("\n");
    const fresh = normalize(readFileSync(target, "utf8")).split("\n");
    for (let i = 0; i < Math.max(committed.length, fresh.length); i += 1) {
      if (committed[i] !== fresh[i]) {
        problems.push(`${rel}: committed line ${i + 1} is not what a fresh generation writes\n    committed: ${committed[i] ?? "<eof>"}\n    generated: ${fresh[i] ?? "<eof>"}`);
        break;
      }
    }
  }
  assert.deepEqual(problems, [], `generated files have drifted from the schema:\n${problems.join("\n")}\nrun: node contracts/tools/generate.mjs`);
});

test("(a) the embedded schema copy is the contract document", () => {
  assert.equal(
    normalize(readFileSync(path.join(REPO_ROOT, SCHEMA_COPY_REL), "utf8")),
    normalize(readFileSync(SCHEMA_PATH, "utf8")),
    "the Go package must embed exactly contracts/event-envelope.schema.json",
  );
});

test("(a) generate.mjs --check exits 0 on the committed files", () => {
  const result = run(process.execPath, [path.join(CONTRACTS_DIR, "tools", "generate.mjs"), "--check"], { cwd: REPO_ROOT });
  assert.equal(result.status, 0, `generate.mjs --check must exit 0\n${describeResult(result)}`);
  assert.match(result.stdout, /match/, `expected a success line, got: ${result.stdout}`);
});

test("(a) drift detection fails with the file name and runnable command when a generated file is edited", () => {
  const dir = makeTemp("drift");
  const copy = path.join(dir, "contracts");
  cpSync(CONTRACTS_DIR, copy, { recursive: true });
  const tampered = path.join(copy, "generated", "go", "envelope", "envelope.go");
  writeFileSync(tampered, `${readFileSync(tampered, "utf8")}// hand edit that the schema does not produce\n`, "utf8");
  const result = run(process.execPath, [path.join(copy, "tools", "generate.mjs"), "--check"], { cwd: dir });
  assert.notEqual(result.status, 0, `--check must exit non-zero after a hand edit\n${describeResult(result)}`);
  const output = `${result.stdout}${result.stderr}`;
  assert.match(output, /envelope\.go/, `the failure must name the drifted file, got:\n${output}`);
  assert.match(output, /generate\.mjs/, `the failure must say how to regenerate, got:\n${output}`);
});

// ---------------------------------------------------------------------------------------
// independent reading of the schema
// ---------------------------------------------------------------------------------------

function pascalLocal(value) {
  return String(value)
    .split(/[^A-Za-z0-9]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join("");
}

function forbiddenNames(not) {
  if (!not) return [];
  if (Array.isArray(not.required)) return [...not.required];
  return (not.anyOf ?? []).flatMap((entry) => entry.required ?? []);
}

/**
 * Derives the contract's expectations straight from the schema: the closed kind registry, the
 * enums, the core fields, and one entry per (kind, mode) device variant with its required,
 * optional and forbidden field sets.
 */
function readExpectations() {
  const schema = JSON.parse(readFileSync(SCHEMA_PATH, "utf8"));
  const defs = schema.$defs;
  const core = defs.envelopeCore;
  const properties = Object.keys(core.properties);
  const coreRequired = [...core.required];
  const kinds = [...core.properties.kind.enum];
  const modes = [...core.properties.collection_mode.enum];

  const kindRules = new Map();
  const modeRules = new Map();
  const merge = (rule, then) => {
    rule.required.push(...(then.required ?? []));
    rule.forbidden.push(...forbiddenNames(then.not));
    for (const [name, value] of Object.entries(then.properties ?? {})) rule.pinned[name] = value.const;
  };
  for (const branch of core.allOf ?? []) {
    const ifProperties = branch.if?.properties ?? {};
    const kind = ifProperties.kind?.const;
    const then = branch.then;
    if (!kind || !then) throw new Error("verify: unexpected schema branch without kind const or then");
    if (ifProperties.collection_mode) {
      const branchModes = ifProperties.collection_mode.const ? [ifProperties.collection_mode.const] : [...ifProperties.collection_mode.enum];
      if (!modeRules.has(kind)) modeRules.set(kind, new Map());
      for (const mode of branchModes) {
        const rule = modeRules.get(kind).get(mode) ?? { required: [], forbidden: [], pinned: {} };
        merge(rule, then);
        modeRules.get(kind).set(mode, rule);
      }
      continue;
    }
    const rule = kindRules.get(kind) ?? { required: [], forbidden: [], pinned: {} };
    merge(rule, then);
    kindRules.set(kind, rule);
  }

  const variants = [];
  for (const kind of kinds) {
    const kindRule = kindRules.get(kind) ?? { required: [], forbidden: [], pinned: {} };
    const perKindModes = modeRules.get(kind);
    for (const mode of perKindModes ? modes : [null]) {
      const modeRule = (mode && perKindModes?.get(mode)) || { required: [], forbidden: [], pinned: {} };
      const required = new Set([...coreRequired, ...kindRule.required, ...modeRule.required]);
      const forbidden = new Set([...kindRule.forbidden, ...modeRule.forbidden, "received_at"]);
      const optional = new Set(properties.filter((name) => !required.has(name) && !forbidden.has(name)));
      variants.push({
        name: `Device${pascalLocal(kind)}${mode ? pascalLocal(mode) : ""}`,
        kind,
        mode,
        required: [...required],
        optional: [...optional],
        forbidden: [...forbidden],
        pinned: { ...kindRule.pinned, ...modeRule.pinned, kind, ...(mode ? { collection_mode: mode } : {}) },
      });
    }
  }

  const objects = [
    { name: "Label", def: defs.label },
    { name: "PolicyDecision", def: defs.policyDecision },
    { name: "Attachment", def: defs.attachment },
    { name: "Excerpt", def: defs.excerpt },
  ].map(({ name, def }) => ({
    name,
    required: [...(def.required ?? [])],
    optional: Object.keys(def.properties).filter((property) => !(def.required ?? []).includes(property)),
  }));

  return {
    schema,
    kinds,
    // The core type carries exactly the universally required fields: everything else belongs to
    // a variant, so a non-core property in the core would give a kind a field it must not carry.
    core: {
      required: coreRequired,
      optional: [],
      forbidden: properties.filter((name) => !coreRequired.includes(name)),
    },
    variants,
    objects,
    enums: [
      { goType: "Kind", goAll: "AllKinds", values: kinds },
      { goType: "Route", goAll: "AllRoutes", values: [...defs.route.enum] },
      { goType: "Direction", goAll: "AllDirections", values: [...core.properties.direction.enum] },
      { goType: "CollectionMode", goAll: "AllCollectionModes", values: modes },
      { goType: "Confidence", goAll: "AllConfidences", values: [...core.properties.confidence.enum] },
      { goType: "PolicyAction", goAll: "AllPolicyActions", values: [...defs.policyDecision.properties.action.enum] },
      { goType: "ExcerptKind", goAll: "AllExcerptKinds", values: [...defs.excerpt.properties.kind.enum] },
      { goType: "DetectionBasis", goAll: "AllDetectionBases", values: [...core.properties.detection_basis.enum] },
      { goType: "PromptKind", goAll: "AllPromptKinds", values: [...core.properties.prompt_kind.enum] },
      { goType: "DiscoveryType", goAll: "AllDiscoveryTypes", values: [...core.properties.discovery_type.enum] },
      { goType: "ActivityType", goAll: "AllActivityTypes", values: [...core.properties.activity_type.enum] },
      { goType: "Outcome", goAll: "AllOutcomes", values: [...core.properties.outcome.enum] },
    ],
  };
}

// ---------------------------------------------------------------------------------------
// parser for the generated Go
// ---------------------------------------------------------------------------------------

function parseGo(text) {
  const lines = text.split("\n");
  const structs = new Map();
  const consts = new Map();
  const funcs = new Map();
  const assertions = new Map();
  for (let i = 0; i < lines.length; i += 1) {
    let match;
    if ((match = /^type (\w+) struct \{$/.exec(lines[i]))) {
      const fields = new Map();
      let embedded = null;
      for (i += 1; i < lines.length && lines[i] !== "}"; i += 1) {
        const field = /^\t(\w+)\s+(\S+)\s+`json:"([^"]+)"`$/.exec(lines[i]);
        if (field) {
          const [jsonName, option] = field[3].split(",");
          fields.set(jsonName, { type: field[2], optional: option === "omitempty" });
          continue;
        }
        const inline = /^\t(\w+)$/.exec(lines[i]);
        if (inline) embedded = inline[1];
      }
      structs.set(match[1], { embedded, fields });
      continue;
    }
    if ((match = /^var _ (\w+) = \(\*(\w+)\)\(nil\)$/.exec(lines[i]))) {
      if (!assertions.has(match[1])) assertions.set(match[1], []);
      assertions.get(match[1]).push(match[2]);
      continue;
    }
    if (lines[i] === "const (") {
      for (i += 1; i < lines.length && lines[i] !== ")"; i += 1) {
        const entry = /^\t(\w+)\s+(\w+) = "([^"]*)"$/.exec(lines[i]);
        if (!entry) continue;
        if (!consts.has(entry[2])) consts.set(entry[2], new Map());
        consts.get(entry[2]).set(entry[1], entry[3]);
      }
      continue;
    }
    if (/^func .*\}$/.test(lines[i])) continue; // a one-line function has no body to collect
    if ((match = /^func \((\w+) \*?(\w+)\) (\w+)\(/.exec(lines[i]))) {
      const body = [];
      for (i += 1; i < lines.length && lines[i] !== "}"; i += 1) body.push(lines[i]);
      funcs.set(`${match[2]}.${match[3]}`, body.join("\n"));
      continue;
    }
    if ((match = /^func (\w+)\(/.exec(lines[i]))) {
      const body = [];
      for (i += 1; i < lines.length && lines[i] !== "}"; i += 1) body.push(lines[i]);
      funcs.set(match[1], body.join("\n"));
    }
  }
  return { structs, consts, funcs, assertions };
}

function compareFieldSets(label, actual, expected) {
  const problems = [];
  for (const name of expected.required) {
    if (!actual.has(name)) problems.push(`${label}: required field ${name} is missing`);
    else if (actual.get(name).optional) problems.push(`${label}: ${name} is optional but the schema requires it`);
  }
  for (const name of expected.optional) {
    if (!actual.has(name)) problems.push(`${label}: permitted field ${name} is missing`);
    else if (!actual.get(name).optional) problems.push(`${label}: ${name} is required but the schema only permits it`);
  }
  for (const name of expected.forbidden ?? []) {
    if (actual.has(name)) problems.push(`${label}: ${name} is forbidden for this kind and mode but is declared`);
  }
  const known = new Set([...expected.required, ...expected.optional, ...(expected.forbidden ?? [])]);
  for (const name of actual.keys()) {
    if (!known.has(name)) problems.push(`${label}: ${name} is not a property of the schema`);
  }
  return problems;
}

function stripComments(text) {
  return text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "");
}

const expectations = readExpectations();
const goText = readFileSync(path.join(REPO_ROOT, GO_REL), "utf8");
const goParsed = parseGo(normalize(goText));

// ---------------------------------------------------------------------------------------
// (b) registries and enums
// ---------------------------------------------------------------------------------------

test("(b) every enum, the kind registry among them, equals the schema's closed set", () => {
  const problems = [];
  for (const enumDef of expectations.enums) {
    const goValues = goParsed.consts.get(enumDef.goType);
    if (!goValues) {
      problems.push(`no const block of type ${enumDef.goType}`);
      continue;
    }
    if (JSON.stringify([...goValues.values()].sort()) !== JSON.stringify([...enumDef.values].sort())) {
      problems.push(`${enumDef.goType} constants are ${JSON.stringify([...goValues.values()])}, schema has ${JSON.stringify(enumDef.values)}`);
    }
    const all = goParsed.funcs.get(enumDef.goAll);
    if (!all) problems.push(`${enumDef.goAll}() is missing`);
    else {
      const returned = [...all.matchAll(/\b(\w+)\b/g)].map((entry) => entry[1]).filter((name) => goValues.has(name)).map((name) => goValues.get(name));
      if (JSON.stringify(returned) !== JSON.stringify(enumDef.values)) {
        problems.push(`${enumDef.goAll}() returns ${JSON.stringify(returned)}, schema order is ${JSON.stringify(enumDef.values)}`);
      }
    }
    const valid = goParsed.funcs.get(`${enumDef.goType}.Valid`);
    if (!valid) problems.push(`${enumDef.goType}.Valid() is missing`);
    else {
      const cases = [...valid.matchAll(/case ([^:]+):/g)].flatMap((entry) => entry[1].split(",").map((name) => name.trim()));
      const caseValues = [...new Set(cases.filter((name) => goValues.has(name)).map((name) => goValues.get(name)))];
      if (JSON.stringify(caseValues.sort()) !== JSON.stringify([...enumDef.values].sort())) {
        problems.push(`${enumDef.goType}.Valid() accepts ${JSON.stringify(caseValues)}, schema has ${JSON.stringify(enumDef.values)}`);
      }
    }
  }
  assert.deepEqual(problems, [], `enum drift:\n${problems.join("\n")}`);
});

// ---------------------------------------------------------------------------------------
// (c) required / optional / forbidden
// ---------------------------------------------------------------------------------------

test("(c) every struct's fields match the schema's required, permitted and forbidden sets", () => {
  const problems = [];
  const coreStruct = goParsed.structs.get("EnvelopeCore");
  assert.ok(coreStruct, "EnvelopeCore must be declared");
  problems.push(...compareFieldSets("EnvelopeCore", coreStruct.fields, expectations.core));
  for (const [name, meta] of coreStruct.fields) {
    if (meta.type.startsWith("*")) problems.push(`EnvelopeCore: required field ${name} is a pointer`);
  }
  for (const variant of expectations.variants) {
    const struct = goParsed.structs.get(variant.name);
    if (!struct) {
      problems.push(`struct ${variant.name} is missing`);
      continue;
    }
    if (struct.embedded !== "EnvelopeCore") problems.push(`${variant.name} must embed EnvelopeCore, found ${JSON.stringify(struct.embedded)}`);
    // A variant that re-declares a core field would shadow the embedded one in JSON decoding.
    for (const name of struct.fields.keys()) {
      if (coreStruct.fields.has(name)) problems.push(`${variant.name}: re-declares core field ${name} instead of inheriting it`);
    }
    problems.push(...compareFieldSets(variant.name, new Map([...coreStruct.fields, ...struct.fields]), variant));
    for (const [name, meta] of struct.fields) {
      if (!meta.optional && meta.type.startsWith("*")) problems.push(`${variant.name}: required field ${name} is a pointer`);
      if (meta.optional && !meta.type.startsWith("*")) problems.push(`${variant.name}: optional field ${name} must be a pointer so presence survives a round trip`);
    }
  }
  for (const object of expectations.objects) {
    const struct = goParsed.structs.get(object.name);
    if (!struct) {
      problems.push(`struct ${object.name} is missing`);
      continue;
    }
    problems.push(...compareFieldSets(object.name, struct.fields, object));
    for (const [name, meta] of struct.fields) {
      if (!meta.optional && meta.type.startsWith("*")) problems.push(`${object.name}: required field ${name} is a pointer`);
      if (meta.optional && !meta.type.startsWith("*")) problems.push(`${object.name}: optional field ${name} must be a pointer`);
    }
  }
  assert.deepEqual(problems, [], `requiredness drift:\n${problems.join("\n")}`);
});

// ---------------------------------------------------------------------------------------
// (d) union cases
// ---------------------------------------------------------------------------------------

test("(d) every variant the schema implies is in the union and reachable from the decoder", () => {
  const problems = [];
  const expected = expectations.variants.map((variant) => variant.name).sort();
  const members = [...(goParsed.assertions.get("DeviceSubmission") ?? [])].sort();
  if (JSON.stringify(members) !== JSON.stringify(expected)) {
    problems.push(`DeviceSubmission is ${JSON.stringify(members)}, schema implies ${JSON.stringify(expected)}`);
  }
  const decodeBody = goParsed.funcs.get("DecodeDeviceSubmission");
  assert.ok(decodeBody, "DecodeDeviceSubmission must exist");
  for (const name of expected) {
    if (!decodeBody.includes(`&${name}{}`)) problems.push(`DecodeDeviceSubmission cannot reach ${name}`);
  }
  const kindConsts = goParsed.consts.get("Kind") ?? new Map();
  for (const kind of expectations.kinds) {
    const kindConst = [...kindConsts.entries()].find(([, value]) => value === kind)?.[0];
    if (!kindConst) problems.push(`no Kind constant for ${kind}`);
    else if (!decodeBody.includes(`case ${kindConst}:`)) problems.push(`DecodeDeviceSubmission does not dispatch kind ${kind}`);
  }
  assert.deepEqual(problems, [], `union drift:\n${problems.join("\n")}`);
});

test("(d) the output uses no any / interface{} escape hatch", () => {
  const code = stripComments(goText);
  assert.doesNotMatch(code, /\bany\b/, "the generated Go uses `any` in a type position");
  assert.doesNotMatch(code, /interface\s*\{\s*\}/, "the generated Go uses the empty interface in a type position");
});

// ---------------------------------------------------------------------------------------
// (e) the endpoint kinds, and records of every kind
// ---------------------------------------------------------------------------------------

const CONTENT_FIELDS = ["content_digest", "labels", "classifier_version", "content_excerpt", "confidence", "attachments", "prompt_kind"];
const WINDOW_FIELDS = ["window_start", "window_end", "submission_count", "bytes_total"];
const DISCOVERY_FIELDS = ["discovery_type", "app_version", "publisher", "host_app", "destination_host", "model_names"];
const ACTIVITY_FIELDS = ["activity_type", "model", "input_tokens", "output_tokens", "duration_ms", "tool_name", "outcome"];

// The endpoint part of the contract, written out so a schema edit that loosens it fails here.
const ENDPOINT_CONTRACT = {
  routes: ["tool.hook", "tool.otel", "inv.scan", "net.flow"],
  kinds: ["prompt", "usage_rollup", "discovery", "agent_activity"],
  detectionBases: ["installed_scan", "package_scan", "extension_scan", "process_event", "model_store", "port_listen", "flow_metadata"],
  fields: {
    discovery_type: { enum: ["app_installed", "app_running", "cli_installed", "ide_extension", "local_model", "inference_connection"] },
    app_version: { type: "string", minLength: 1, maxLength: 64 },
    publisher: { type: "string", minLength: 1, maxLength: 200 },
    host_app: { type: "string", minLength: 1, maxLength: 128 },
    destination_host: { type: "string", minLength: 1, maxLength: 253 },
    model_names: { type: "array", maxItems: 64, items: { type: "string", minLength: 1, maxLength: 200 } },
    activity_type: { enum: ["model_request", "tool_call"] },
    model: { type: "string", minLength: 1, maxLength: 128 },
    input_tokens: { type: "integer", minimum: 0 },
    output_tokens: { type: "integer", minimum: 0 },
    duration_ms: { type: "integer", minimum: 0 },
    tool_name: { type: "string", minLength: 1, maxLength: 128 },
    outcome: { enum: ["success", "error", "denied"] },
  },
  variants: {
    DeviceDiscovery: {
      required: ["discovery_type", "detection_basis"],
      direction: "none",
      forbidden: [...CONTENT_FIELDS, "policy_decision", "size_bytes", ...WINDOW_FIELDS, ...ACTIVITY_FIELDS],
    },
    DeviceAgentActivity: {
      required: ["activity_type"],
      direction: "none",
      forbidden: [...CONTENT_FIELDS, "policy_decision", ...WINDOW_FIELDS, "detection_basis", ...DISCOVERY_FIELDS],
    },
  },
  // The kinds that existed before keep their rules and also forbid every new field.
  alsoForbidNewFields: ["DevicePromptM0", "DevicePromptM1", "DevicePromptM2", "DevicePromptM3", "DeviceUsageRollup"],
};

test("(e) the routes, kinds, detection bases, endpoint fields and endpoint kind rules are the contract's", () => {
  const { schema } = expectations;
  const core = schema.$defs.envelopeCore;
  const problems = [];
  for (const route of ENDPOINT_CONTRACT.routes) {
    if (!schema.$defs.route.enum.includes(route)) problems.push(`route ${route} is missing`);
  }
  if (JSON.stringify(core.properties.kind.enum) !== JSON.stringify(ENDPOINT_CONTRACT.kinds)) {
    problems.push(`kind is ${JSON.stringify(core.properties.kind.enum)}, expected ${JSON.stringify(ENDPOINT_CONTRACT.kinds)}`);
  }
  if (JSON.stringify(core.properties.detection_basis.enum) !== JSON.stringify(ENDPOINT_CONTRACT.detectionBases)) {
    problems.push(`detection_basis is ${JSON.stringify(core.properties.detection_basis.enum)}`);
  }
  for (const [name, want] of Object.entries(ENDPOINT_CONTRACT.fields)) {
    const prop = core.properties[name];
    if (!prop) {
      problems.push(`${name} is not declared in envelopeCore`);
      continue;
    }
    for (const [key, value] of Object.entries(want)) {
      if (JSON.stringify(key === "items" ? { type: prop.items?.type, minLength: prop.items?.minLength, maxLength: prop.items?.maxLength } : prop[key]) !== JSON.stringify(value)) {
        problems.push(`${name}.${key} is ${JSON.stringify(prop[key])}, expected ${JSON.stringify(value)}`);
      }
    }
    if (core.required.includes(name)) problems.push(`${name} must not be required on every kind`);
  }
  if (!new RegExp(core.properties.destination_host.pattern, "u").test("api.openai.com") || new RegExp(core.properties.destination_host.pattern, "u").test("API.OpenAI.com")) {
    problems.push("destination_host must accept a lower-case host name and refuse an upper-case one");
  }
  for (const [name, want] of Object.entries(ENDPOINT_CONTRACT.variants)) {
    const variant = expectations.variants.find((v) => v.name === name);
    if (!variant) {
      problems.push(`no variant ${name}`);
      continue;
    }
    for (const field of want.required) if (!variant.required.includes(field)) problems.push(`${name} must require ${field}`);
    if (variant.pinned.direction !== want.direction) problems.push(`${name} must pin direction ${want.direction}, pins ${variant.pinned.direction}`);
    const forbidden = variant.forbidden.filter((field) => field !== "received_at").sort();
    if (JSON.stringify(forbidden) !== JSON.stringify([...want.forbidden].sort())) {
      problems.push(`${name} forbids ${JSON.stringify(forbidden)}, expected ${JSON.stringify([...want.forbidden].sort())}`);
    }
  }
  for (const name of ENDPOINT_CONTRACT.alsoForbidNewFields) {
    const variant = expectations.variants.find((v) => v.name === name);
    for (const field of [...DISCOVERY_FIELDS, ...ACTIVITY_FIELDS, "detection_basis"]) {
      if (!variant?.forbidden.includes(field)) problems.push(`${name} must forbid ${field}`);
    }
  }
  assert.deepEqual(problems, [], `the endpoint contract drifted:\n${problems.join("\n")}`);
});

/**
 * Checks a record against one variant: the field sets and pins re-derived above, and each field's
 * declaration in envelopeCore (type, enum, const, length, pattern, minimum, array bounds). Nested
 * objects are checked for type only. Returns the problems, each naming its field.
 */
function check(record, variant) {
  const { schema } = expectations;
  const core = schema.$defs.envelopeCore;
  const problems = [];
  const permitted = new Set([...variant.required, ...variant.optional]);
  for (const name of variant.required) if (!(name in record)) problems.push(`${name}: required`);
  for (const name of Object.keys(record)) {
    if (variant.forbidden.includes(name)) problems.push(`${name}: forbidden for ${variant.name}`);
    else if (!permitted.has(name)) problems.push(`${name}: not a property`);
  }
  for (const [name, value] of Object.entries(variant.pinned)) {
    if (name in record && record[name] !== value) problems.push(`${name}: must be ${JSON.stringify(value)}`);
  }
  const resolve = (prop) => (prop.$ref ? { ...schema.$defs[prop.$ref.slice("#/$defs/".length)], ...prop, $ref: undefined } : prop);
  const checkValue = (name, prop, value) => {
    if (prop.const !== undefined && value !== prop.const) problems.push(`${name}: const ${JSON.stringify(prop.const)}`);
    if (prop.enum && !prop.enum.includes(value)) problems.push(`${name}: not one of ${JSON.stringify(prop.enum)}`);
    const type = prop.type ?? (prop.enum || prop.pattern ? "string" : undefined);
    if (type === "string") {
      if (typeof value !== "string") return problems.push(`${name}: not a string`);
      if (prop.minLength !== undefined && value.length < prop.minLength) problems.push(`${name}: shorter than ${prop.minLength}`);
      if (prop.maxLength !== undefined && value.length > prop.maxLength) problems.push(`${name}: longer than ${prop.maxLength}`);
      if (prop.pattern !== undefined && !new RegExp(prop.pattern, "u").test(value)) problems.push(`${name}: does not match ${prop.pattern}`);
    } else if (type === "integer") {
      if (!Number.isInteger(value)) return problems.push(`${name}: not an integer`);
      if (prop.minimum !== undefined && value < prop.minimum) problems.push(`${name}: below ${prop.minimum}`);
    } else if (type === "array") {
      if (!Array.isArray(value)) return problems.push(`${name}: not an array`);
      if (prop.maxItems !== undefined && value.length > prop.maxItems) problems.push(`${name}: more than ${prop.maxItems} items`);
      if (prop.items && !prop.items.$ref) value.forEach((item, i) => checkValue(`${name}[${i}]`, prop.items, item));
    } else if (type === "object" && (typeof value !== "object" || value === null || Array.isArray(value))) {
      problems.push(`${name}: not an object`);
    }
  };
  for (const [name, value] of Object.entries(record)) {
    if (core.properties[name]) checkValue(name, resolve(core.properties[name]), value);
  }
  return problems;
}

const sha = (c) => `sha256:${c.repeat(64)}`;

// A value for every declared property, valid against its declaration.
const SAMPLE = {
  received_at: "2026-10-02T13:00:01Z",
  subject_name: "Ada",
  prompt_kind: "user",
  confidence: "high",
  size_bytes: 42,
  content_digest: sha("a"),
  labels: [{ class: "credential", score: 0.9 }],
  classifier_version: "classifier-1",
  content_excerpt: { kind: "match_span", text: "abc" },
  attachments: [],
  policy_decision: { rule_id: "R-1", action: "logged", decided_locally: true },
  window_start: "2026-10-02T12:55:00Z",
  window_end: "2026-10-02T13:00:00Z",
  submission_count: 3,
  bytes_total: 4096,
  detection_basis: "installed_scan",
  discovery_type: "app_installed",
  app_version: "1.2.3",
  publisher: "Vendor, Inc.",
  host_app: "app:vscode",
  destination_host: "api.openai.com",
  model_names: ["llama3.2:3b"],
  activity_type: "tool_call",
  model: "claude-sonnet-4",
  input_tokens: 1200,
  output_tokens: 300,
  duration_ms: 2100,
  tool_name: "Bash",
  outcome: "success",
};

function baseRecord(fields) {
  return {
    schema_version: "1.0",
    event_id: "11111111-1111-4111-8111-111111111111",
    tenant_id: "22222222-2222-4222-8222-222222222222",
    device_id: "33333333-3333-4333-8333-333333333333",
    user_ref: "user-1",
    tool_fingerprint: "app:claude_code",
    occurred_at: "2026-10-02T13:00:00Z",
    monotonic_offset_ms: 12,
    dedup_key: sha("b"),
    ...fields,
  };
}

const classified = { confidence: "high", content_digest: sha("a"), labels: SAMPLE.labels, classifier_version: "classifier-1" };
const promptRecord = (mode, extra = {}) =>
  baseRecord({ kind: "prompt", direction: "egress", source: "tool.hook", collection_mode: mode, size_bytes: 42, policy_decision: SAMPLE.policy_decision, ...extra });

// One valid record per variant, and one per discovery type and activity type.
const VALID = [
  ["DevicePromptM0", promptRecord("m0")],
  ["DevicePromptM1", promptRecord("m1", classified)],
  ["DevicePromptM2", promptRecord("m2", { ...classified, content_excerpt: SAMPLE.content_excerpt })],
  ["DevicePromptM3", promptRecord("m3", { ...classified, source: "tool.otel" })],
  ["DeviceUsageRollup", baseRecord({ kind: "usage_rollup", direction: "none", source: "proc.detect", collection_mode: "m1", window_start: SAMPLE.window_start, window_end: SAMPLE.window_end, submission_count: 3, bytes_total: 4096 })],
  ...[
    { discovery_type: "app_installed", source: "inv.scan", detection_basis: "installed_scan", tool_fingerprint: "app:cursor", user_ref: "unattributed", app_version: "0.48.1", publisher: "Anysphere, Inc." },
    { discovery_type: "app_running", source: "proc.detect", detection_basis: "process_event", tool_fingerprint: "app:chatgpt_desktop" },
    { discovery_type: "cli_installed", source: "inv.scan", detection_basis: "package_scan", tool_fingerprint: "app:codex", app_version: "0.40.0" },
    { discovery_type: "ide_extension", source: "inv.scan", detection_basis: "extension_scan", tool_fingerprint: "app:github_copilot", host_app: "app:vscode" },
    { discovery_type: "local_model", source: "inv.scan", detection_basis: "model_store", tool_fingerprint: "app:ollama", model_names: ["llama3.2:3b", "qwen2.5-coder:7b"] },
    { discovery_type: "inference_connection", source: "net.flow", detection_basis: "flow_metadata", tool_fingerprint: "exe:0123456789abcdef", destination_host: "api.anthropic.com" },
  ].map((fields) => ["DeviceDiscovery", baseRecord({ kind: "discovery", direction: "none", collection_mode: "m0", ...fields })]),
  ...[
    { activity_type: "model_request", source: "tool.otel", model: "claude-sonnet-4", input_tokens: 1200, output_tokens: 300, duration_ms: 2100, outcome: "success" },
    { activity_type: "tool_call", source: "tool.hook", tool_name: "Bash", duration_ms: 15, outcome: "denied" },
  ].map((fields) => ["DeviceAgentActivity", baseRecord({ kind: "agent_activity", direction: "none", collection_mode: "m1", ...fields })]),
];

const variantNamed = (name) => expectations.variants.find((variant) => variant.name === name);

test("(e) one valid record per kind, mode, discovery type and activity type is accepted", () => {
  const problems = [];
  for (const [name, record] of VALID) {
    const found = check(record, variantNamed(name));
    if (found.length > 0) problems.push(`${name} ${record.discovery_type ?? record.activity_type ?? record.collection_mode}: ${found.join("; ")}`);
  }
  const core = expectations.schema.$defs.envelopeCore;
  const covered = (field) => new Set(VALID.map(([, record]) => record[field]).filter(Boolean));
  for (const value of core.properties.discovery_type.enum) if (!covered("discovery_type").has(value)) problems.push(`no valid record for discovery_type ${value}`);
  for (const value of core.properties.activity_type.enum) if (!covered("activity_type").has(value)) problems.push(`no valid record for activity_type ${value}`);
  for (const variant of expectations.variants) if (!VALID.some(([name]) => name === variant.name)) problems.push(`no valid record for ${variant.name}`);
  assert.deepEqual(problems, [], problems.join("\n"));
});

test("(e) one invalid record per forbidden field per kind is refused, naming the field", () => {
  const problems = [];
  for (const variant of expectations.variants) {
    const [, valid] = VALID.find(([name]) => name === variant.name);
    for (const field of variant.forbidden) {
      assert.ok(field in SAMPLE, `no sample value for ${field}`);
      const found = check({ ...valid, [field]: SAMPLE[field] }, variant);
      if (!found.some((problem) => problem.startsWith(`${field}: forbidden`))) problems.push(`${variant.name} accepted ${field}`);
    }
  }
  assert.deepEqual(problems, [], problems.join("\n"));
});

test("(e) the endpoint kinds pin direction none and refuse out-of-contract values", () => {
  const discovery = VALID.find(([name]) => name === "DeviceDiscovery")[1];
  const activity = VALID.find(([name]) => name === "DeviceAgentActivity")[1];
  const cases = [
    ["DeviceDiscovery", { ...discovery, direction: "egress" }, "direction"],
    ["DeviceDiscovery", { ...discovery, discovery_type: "browser_tab" }, "discovery_type"],
    ["DeviceDiscovery", { ...discovery, detection_basis: "process_scan" }, "detection_basis"],
    ["DeviceDiscovery", { ...discovery, destination_host: "API.openai.com" }, "destination_host"],
    ["DeviceDiscovery", { ...discovery, destination_host: "api.openai.com/v1/chat" }, "destination_host"],
    ["DeviceDiscovery", { ...discovery, app_version: "x".repeat(65) }, "app_version"],
    ["DeviceDiscovery", { ...discovery, model_names: Array.from({ length: 65 }, (_, i) => `m${i}`) }, "model_names"],
    ["DeviceDiscovery", { ...discovery, model_names: [""] }, "model_names[0]"],
    ["DeviceDiscovery", (({ discovery_type, ...rest }) => rest)(discovery), "discovery_type"],
    ["DeviceAgentActivity", { ...activity, direction: "egress" }, "direction"],
    ["DeviceAgentActivity", { ...activity, outcome: "blocked" }, "outcome"],
    ["DeviceAgentActivity", { ...activity, input_tokens: -1 }, "input_tokens"],
    ["DeviceAgentActivity", { ...activity, model: "" }, "model"],
    ["DeviceAgentActivity", (({ activity_type, ...rest }) => rest)(activity), "activity_type"],
  ];
  const problems = [];
  for (const [name, record, field] of cases) {
    const found = check(record, variantNamed(name));
    if (!found.some((problem) => problem.startsWith(`${field}:`))) problems.push(`${name}: a bad ${field} was accepted (${found.join("; ") || "no problems"})`);
  }
  assert.deepEqual(problems, [], problems.join("\n"));
});

// ---------------------------------------------------------------------------------------
// the generated Go package: formatting, vet, and its tests
// ---------------------------------------------------------------------------------------

test("(acceptance) the generated Go package is gofmt-clean, vets, and passes its tests", () => {
  const moduleDir = path.join(REPO_ROOT, GO_MODULE_REL);
  const formatted = run("gofmt", ["-l", "."], { cwd: moduleDir });
  assert.equal(formatted.status, 0, `gofmt failed to run\n${describeResult(formatted)}`);
  assert.equal(formatted.stdout.trim(), "", `gofmt reports unformatted files:\n${formatted.stdout}`);

  const vet = run("go", ["vet", "./..."], { cwd: moduleDir });
  assert.equal(vet.status, 0, `go vet ./... failed in ${GO_MODULE_REL}\n${describeResult(vet)}`);

  const tested = run("go", ["test", "./..."], { cwd: moduleDir });
  assert.equal(tested.status, 0, `go test ./... failed in ${GO_MODULE_REL}\n${describeResult(tested)}`);
});
