// contracts/tools/verify.mjs
//
// Verification suite for the generated envelope contract types.
//
//   node contracts/tools/verify.mjs          (direct)
//   node --test contracts/tools/             (the acceptance command; see index.js)
//
// It is deliberately not a mirror of the generator:
//   (a) it renders the types into a temporary directory and fails if the committed files differ,
//   (b) it checks the kind registry in both outputs against the schema, and that both stay closed,
//   (c) it re-derives required/optional/forbidden field sets from the schema and checks the
//       generated TypeScript and Go field by field, in both directions,
//   (d) it checks that every union case derived from the schema is present and reachable,
//   and it runs the generated Go through gofmt, the compiler, and a table of accept/reject cases.
//
// Every expectation is re-derived from contracts/event-envelope.schema.json here, so a
// generator bug that produced matching-but-wrong output still fails.

import test, { after } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { CONTRACTS_DIR, REPO_ROOT, renderAll } from "./generate.mjs";

const SCHEMA_PATH = path.join(CONTRACTS_DIR, "event-envelope.schema.json");
const TS_REL = "contracts/generated/typescript/envelope.ts";
const GO_REL = "contracts/generated/go/envelope/envelope.go";
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

function readRepo(rel) {
  return readFileSync(path.join(REPO_ROOT, rel), "utf8");
}

function run(command, args, options = {}) {
  return spawnSync(command, args, { encoding: "utf8", ...options });
}

function describeResult(result) {
  return `exit=${result.status}${result.signal ? ` signal=${result.signal}` : ""}\nstdout:\n${result.stdout ?? ""}\nstderr:\n${result.stderr ?? ""}`;
}

function goEnv(tempDir) {
  return {
    ...process.env,
    GOCACHE: path.join(tempDir, "gocache"),
    GOPATH: path.join(tempDir, "gopath"),
    GOMODCACHE: path.join(tempDir, "gomodcache"),
    GOPROXY: "off",
    GOTOOLCHAIN: "local",
    GOFLAGS: "-mod=mod",
    GOWORK: "off",
    GO111MODULE: "on",
  };
}

// ---------------------------------------------------------------------------------------
// (a) regeneration: temp directory, byte comparison
// ---------------------------------------------------------------------------------------

test("(a) a fresh generation into a temp directory is byte-identical to the committed files", () => {
  const dir = makeTemp("regen");
  const rendered = renderAll({ contractsDir: CONTRACTS_DIR });
  assert.deepEqual(
    rendered.map((file) => file.rel).sort(),
    [GO_REL, TS_REL],
    "the generator must emit exactly the two contract files; the file list changed",
  );
  for (const { rel, content } of rendered) {
    const target = path.join(dir, rel);
    mkdirSync(path.dirname(target), { recursive: true });
    writeFileSync(target, content, "utf8");
  }
  const problems = [];
  for (const { rel, content } of rendered) {
    const committedPath = path.join(REPO_ROOT, rel);
    if (!existsSync(committedPath)) {
      problems.push(`${rel}: missing from the working tree`);
      continue;
    }
    const committed = normalize(readFileSync(committedPath, "utf8"));
    const fresh = normalize(readFileSync(path.join(dir, rel), "utf8"));
    if (committed === fresh) continue;
    const a = committed.split("\n");
    const b = fresh.split("\n");
    for (let i = 0; i < Math.max(a.length, b.length); i += 1) {
      if (a[i] !== b[i]) {
        problems.push(`${rel}: committed line ${i + 1} is not what a fresh generation writes\n    committed: ${a[i] ?? "<eof>"}\n    generated: ${b[i] ?? "<eof>"}`);
        break;
      }
    }
  }
  assert.deepEqual(problems, [], `generated files have drifted from the schema:\n${problems.join("\n")}\nrun: node contracts/tools/generate.mjs`);
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
  const tampered = path.join(copy, "generated", "typescript", "envelope.ts");
  writeFileSync(tampered, `${readFileSync(tampered, "utf8")}// hand edit that the schema does not produce\n`, "utf8");
  const result = run(process.execPath, [path.join(copy, "tools", "generate.mjs"), "--check"], { cwd: dir });
  assert.notEqual(result.status, 0, `--check must exit non-zero after a hand edit\n${describeResult(result)}`);
  const output = `${result.stdout}${result.stderr}`;
  assert.match(output, /envelope\.ts/, `the failure must name the drifted file, got:\n${output}`);
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

function pluralizeLocal(name) {
  return /is$/.test(name) ? name.replace(/is$/, "es") : `${name}s`;
}

function upperSnakeLocal(name) {
  return name.replace(/([a-z0-9])([A-Z])/g, "$1_$2").toUpperCase();
}

function forbiddenNames(not) {
  if (!not) return [];
  if (Array.isArray(not.required)) return [...not.required];
  return (not.anyOf ?? []).flatMap((entry) => entry.required ?? []);
}

/**
 * Derives the contract's expectations straight from the schema: the closed kind registry, the
 * enums, the core fields, and one entry per (shape, kind, mode) variant with its required,
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
    const then = branch.then ?? {};
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
    const variantModes = perKindModes ? modes : [null];
    for (const mode of variantModes) {
      const modeRule = (mode && perKindModes?.get(mode)) || { required: [], forbidden: [], pinned: {} };
      for (const shape of ["device", "stored"]) {
        const required = new Set([...coreRequired, ...kindRule.required, ...modeRule.required]);
        const forbidden = new Set([...kindRule.forbidden, ...modeRule.forbidden]);
        if (shape === "stored") required.add("received_at");
        else forbidden.add("received_at");
        const optional = new Set(properties.filter((name) => !required.has(name) && !forbidden.has(name)));
        variants.push({
          name: `${shape === "device" ? "Device" : "Stored"}${pascalLocal(kind)}${mode ? pascalLocal(mode) : ""}`,
          shape,
          kind,
          mode,
          required: [...required],
          optional: [...optional],
          forbidden: [...forbidden],
        });
      }
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
    kinds,
    modes,
    schemaVersion: core.properties.schema_version.const,
    coreRequired,
    properties,
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
      { ts: "KINDS", goType: "Kind", goAll: "AllKinds", values: kinds },
      { ts: "ROUTES", goType: "Route", goAll: "AllRoutes", values: [...defs.route.enum] },
      { ts: "DIRECTIONS", goType: "Direction", goAll: "AllDirections", values: [...core.properties.direction.enum] },
      { ts: "COLLECTION_MODES", goType: "CollectionMode", goAll: "AllCollectionModes", values: modes },
      { ts: "CONFIDENCES", goType: "Confidence", goAll: "AllConfidences", values: [...core.properties.confidence.enum] },
      { ts: "POLICY_ACTIONS", goType: "PolicyAction", goAll: "AllPolicyActions", values: [...defs.policyDecision.properties.action.enum] },
      { ts: "EXCERPT_KINDS", goType: "ExcerptKind", goAll: "AllExcerptKinds", values: [...defs.excerpt.properties.kind.enum] },
      { ts: "DETECTION_BASES", goType: "DetectionBasis", goAll: "AllDetectionBases", values: [...core.properties.detection_basis.enum] },
    ],
    unionNames: { device: "DeviceSubmission", stored: "StoredEnvelope" },
  };
}

// ---------------------------------------------------------------------------------------
// parsers for the generated artifacts
// ---------------------------------------------------------------------------------------

function parseTypeScript(text) {
  const lines = text.split("\n");
  const interfaces = new Map();
  const unions = new Map();
  const registries = new Map();
  for (let i = 0; i < lines.length; i += 1) {
    let match;
    if ((match = /^export interface (\w+)(?: extends (\w+))? \{$/.exec(lines[i]))) {
      const fields = new Map();
      for (i += 1; i < lines.length && lines[i] !== "}"; i += 1) {
        const field = /^ {2}readonly (\w+)(\?)?: .+;$/.exec(lines[i]);
        if (field) fields.set(field[1], { optional: field[2] === "?" });
      }
      interfaces.set(match[1], { extends: match[2] ?? null, fields });
      continue;
    }
    if ((match = /^export type (\w+) =$/.exec(lines[i]))) {
      const members = [];
      for (i += 1; i < lines.length; i += 1) {
        const member = /^ {2}\| (\w+);?$/.exec(lines[i]);
        if (!member) break;
        members.push(member[1]);
      }
      unions.set(match[1], members);
      continue;
    }
    if ((match = /^export const (\w+) = \[(.*)\] as const;$/.exec(lines[i]))) {
      registries.set(match[1], [...match[2].matchAll(/"([^"]*)"/g)].map((entry) => entry[1]));
    }
  }
  return { interfaces, unions, registries };
}

function flattenTypeScriptInterface(parsed, name) {
  const iface = parsed.interfaces.get(name);
  if (!iface) return null;
  const fields = new Map();
  if (iface.extends) {
    const inherited = flattenTypeScriptInterface(parsed, iface.extends);
    if (!inherited) return null;
    for (const [field, meta] of inherited) fields.set(field, meta);
  }
  for (const [field, meta] of iface.fields) fields.set(field, meta);
  return fields;
}

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
  const actualRequired = [...actual.entries()].filter(([, meta]) => !meta.optional).map(([name]) => name);
  const actualOptional = [...actual.entries()].filter(([, meta]) => meta.optional).map(([name]) => name);
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
  const unexpected = [...actual.keys()].filter(
    (name) => !expected.required.includes(name) && !expected.optional.includes(name) && !(expected.forbidden ?? []).includes(name),
  );
  for (const name of unexpected) problems.push(`${label}: ${name} is not a property of the schema`);
  return { problems, actualRequired, actualOptional };
}

function stripComments(text) {
  return text.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "");
}

// ---------------------------------------------------------------------------------------
// (b) registries and enums
// ---------------------------------------------------------------------------------------

const expectations = readExpectations();
const tsText = readRepo(TS_REL);
const goText = readRepo(GO_REL);
const tsParsed = parseTypeScript(tsText);
const goParsed = parseGo(goText);

test("(b) the closed kind registry in the generated TypeScript matches the schema", async () => {
  const module = await import(pathToFileURL(path.join(REPO_ROOT, TS_REL)).href);
  assert.deepEqual([...module.KINDS], expectations.kinds, "the runtime KINDS list must equal the schema's kind enum");
  assert.match(
    tsText,
    /^export type Kind = \(typeof KINDS\)\[number\];$/m,
    "the Kind type must be derived from the runtime list, so the type and the runtime registry cannot diverge",
  );
  assert.deepEqual(tsParsed.registries.get("KINDS"), expectations.kinds, "KINDS must contain every kind and nothing else");
  assert.equal(module.SCHEMA_VERSION, expectations.schemaVersion, "SCHEMA_VERSION must be the schema's const");
});

test("(b) the closed kind registry in the generated Go matches the schema", () => {
  const kindValues = goParsed.consts.get("Kind");
  assert.ok(kindValues, "the generated Go must declare a Kind const block");
  assert.deepEqual([...kindValues.values()].sort(), [...expectations.kinds].sort(), "Kind constants must equal the schema's kind enum");
  const allKinds = goParsed.funcs.get("AllKinds");
  assert.ok(allKinds, "AllKinds() must exist");
  const named = [...allKinds.matchAll(/\b(Kind\w+)\b/g)].map((entry) => entry[1]);
  assert.deepEqual(
    [...new Set(named.map((name) => kindValues.get(name)))].sort(),
    [...expectations.kinds].sort(),
    "AllKinds() must return exactly the closed registry",
  );
  assert.match(allKinds, /return \[\]Kind\{/, "AllKinds() must return a fresh slice of the closed set");
});

test("(b) every enum in both outputs equals the schema's closed set", async () => {
  const module = await import(pathToFileURL(path.join(REPO_ROOT, TS_REL)).href);
  const problems = [];
  for (const enumDef of expectations.enums) {
    const runtime = module[enumDef.ts];
    if (!runtime) problems.push(`TypeScript: ${enumDef.ts} is not exported`);
    else if (JSON.stringify([...runtime]) !== JSON.stringify(enumDef.values)) {
      problems.push(`TypeScript: ${enumDef.ts} is ${JSON.stringify([...runtime])}, schema has ${JSON.stringify(enumDef.values)}`);
    }
    const goValues = goParsed.consts.get(enumDef.goType);
    if (!goValues) {
      problems.push(`Go: no const block of type ${enumDef.goType}`);
      continue;
    }
    if (JSON.stringify([...goValues.values()].sort()) !== JSON.stringify([...enumDef.values].sort())) {
      problems.push(`Go: ${enumDef.goType} constants are ${JSON.stringify([...goValues.values()])}, schema has ${JSON.stringify(enumDef.values)}`);
    }
    const all = goParsed.funcs.get(enumDef.goAll);
    if (!all) problems.push(`Go: ${enumDef.goAll}() is missing`);
    else {
      const returned = [
        ...new Set(
          [...all.matchAll(/\b(\w+)\b/g)]
            .map((entry) => entry[1])
            .filter((name) => goValues.has(name))
            .map((name) => goValues.get(name)),
        ),
      ];
      if (JSON.stringify(returned.sort()) !== JSON.stringify([...enumDef.values].sort())) {
        problems.push(`Go: ${enumDef.goAll}() returns ${JSON.stringify(returned)}, schema has ${JSON.stringify(enumDef.values)}`);
      }
    }
    const valid = goParsed.funcs.get(`${enumDef.goType}.Valid`);
    if (!valid) problems.push(`Go: ${enumDef.goType}.Valid() is missing`);
    else {
      const cases = [...valid.matchAll(/case ([^:]+):/g)].flatMap((entry) => entry[1].split(",").map((name) => name.trim()));
      const caseValues = [...new Set(cases.filter((name) => goValues.has(name)).map((name) => goValues.get(name)))];
      if (JSON.stringify(caseValues.sort()) !== JSON.stringify([...enumDef.values].sort())) {
        problems.push(`Go: ${enumDef.goType}.Valid() accepts ${JSON.stringify(caseValues)}, schema has ${JSON.stringify(enumDef.values)}`);
      }
    }
  }
  assert.deepEqual(problems, [], `enum drift:\n${problems.join("\n")}`);
});

// ---------------------------------------------------------------------------------------
// (c) required / optional / forbidden, both languages
// ---------------------------------------------------------------------------------------

test("(c) every schema-required property is required in the generated TypeScript", () => {
  const problems = [];
  const coreFields = flattenTypeScriptInterface(tsParsed, "EnvelopeCore");
  assert.ok(coreFields, "EnvelopeCore must be declared in the TypeScript output");
  problems.push(...compareFieldSets("TypeScript EnvelopeCore", coreFields, expectations.core).problems);
  for (const variant of expectations.variants) {
    const fields = flattenTypeScriptInterface(tsParsed, variant.name);
    if (!fields) {
      problems.push(`TypeScript: interface ${variant.name} is missing`);
      continue;
    }
    problems.push(...compareFieldSets(`TypeScript ${variant.name}`, fields, variant).problems);
  }
  for (const object of expectations.objects) {
    const fields = flattenTypeScriptInterface(tsParsed, object.name);
    if (!fields) {
      problems.push(`TypeScript: interface ${object.name} is missing`);
      continue;
    }
    problems.push(...compareFieldSets(`TypeScript ${object.name}`, fields, object).problems);
  }
  assert.deepEqual(problems, [], `requiredness drift in TypeScript:\n${problems.join("\n")}`);
});

test("(c) every schema-required property is required in the generated Go", () => {
  const problems = [];
  const coreStruct = goParsed.structs.get("EnvelopeCore");
  assert.ok(coreStruct, "EnvelopeCore must be declared in the Go output");
  problems.push(...compareFieldSets("Go EnvelopeCore", coreStruct.fields, expectations.core).problems);
  for (const [name, meta] of coreStruct.fields) {
    if (meta.type.startsWith("*")) problems.push(`Go EnvelopeCore: required field ${name} is a pointer`);
  }
  for (const variant of expectations.variants) {
    const struct = goParsed.structs.get(variant.name);
    if (!struct) {
      problems.push(`Go: struct ${variant.name} is missing`);
      continue;
    }
    if (struct.embedded !== "EnvelopeCore") {
      problems.push(`Go: ${variant.name} must embed EnvelopeCore, found ${JSON.stringify(struct.embedded)}`);
    }
    // A variant that re-declares a core field would shadow the embedded one in JSON encoding,
    // so the embedded core would never be populated: it must declare only its own fields.
    for (const name of struct.fields.keys()) {
      if (coreStruct.fields.has(name)) problems.push(`Go ${variant.name}: re-declares core field ${name} instead of inheriting it`);
    }
    const fields = new Map([...coreStruct.fields, ...struct.fields]);
    problems.push(...compareFieldSets(`Go ${variant.name}`, fields, variant).problems);
    for (const [name, meta] of struct.fields) {
      if (!meta.optional && meta.type.startsWith("*")) problems.push(`Go ${variant.name}: required field ${name} is a pointer`);
      if (meta.optional && !meta.type.startsWith("*")) problems.push(`Go ${variant.name}: optional field ${name} must be a pointer so presence survives a round trip`);
    }
  }
  for (const object of expectations.objects) {
    const struct = goParsed.structs.get(object.name);
    if (!struct) {
      problems.push(`Go: struct ${object.name} is missing`);
      continue;
    }
    problems.push(...compareFieldSets(`Go ${object.name}`, struct.fields, object).problems);
    for (const [name, meta] of struct.fields) {
      if (!meta.optional && meta.type.startsWith("*")) problems.push(`Go ${object.name}: required field ${name} is a pointer`);
      if (meta.optional && !meta.type.startsWith("*")) problems.push(`Go ${object.name}: optional field ${name} must be a pointer`);
    }
  }
  assert.deepEqual(problems, [], `requiredness drift in Go:\n${problems.join("\n")}`);
});

// ---------------------------------------------------------------------------------------
// (d) union cases
// ---------------------------------------------------------------------------------------

test("(d) every union case in the schema is present in both outputs", () => {
  const problems = [];
  for (const shape of ["device", "stored"]) {
    const expected = expectations.variants.filter((variant) => variant.shape === shape).map((variant) => variant.name).sort();
    const unionName = expectations.unionNames[shape];

    const tsMembers = [...(tsParsed.unions.get(unionName) ?? [])].sort();
    if (JSON.stringify(tsMembers) !== JSON.stringify(expected)) {
      problems.push(`TypeScript ${unionName} is ${JSON.stringify(tsMembers)}, schema implies ${JSON.stringify(expected)}`);
    }
    for (const name of expected) {
      if (!tsParsed.interfaces.has(name)) problems.push(`TypeScript: union member ${name} has no interface`);
    }

    const goMembers = [...(goParsed.assertions.get(unionName) ?? [])].sort();
    if (JSON.stringify(goMembers) !== JSON.stringify(expected)) {
      problems.push(`Go ${unionName} is ${JSON.stringify(goMembers)}, schema implies ${JSON.stringify(expected)}`);
    }

    const decodeName = `Decode${unionName}`;
    const decodeBody = goParsed.funcs.get(decodeName);
    assert.ok(decodeBody, `Go: ${decodeName} must exist`);
    for (const name of expected) {
      if (!decodeBody.includes(`var v ${name}`)) problems.push(`Go: ${decodeName} cannot reach union member ${name}`);
      if (!goParsed.structs.has(name)) problems.push(`Go: union member ${name} has no struct`);
    }
    const kindConsts = goParsed.consts.get("Kind") ?? new Map();
    for (const kind of expectations.kinds) {
      const kindConst = [...kindConsts.entries()].find(([, value]) => value === kind)?.[0];
      if (!kindConst) problems.push(`Go: no Kind constant for ${kind}`);
      else if (!decodeBody.includes(`case ${kindConst}:`)) problems.push(`Go: ${decodeName} does not dispatch kind ${kind}`);
    }
  }
  assert.deepEqual(problems, [], `union drift:\n${problems.join("\n")}`);
});

test("(2) neither output uses an any / interface{} escape hatch", () => {
  const problems = [];
  const tsCode = stripComments(tsText);
  const goCode = stripComments(goText);
  for (const pattern of [/:\s*any\b/, /<any>/, /\bas any\b/, /any\[\]/]) {
    if (pattern.test(tsCode)) problems.push(`TypeScript uses ${pattern} in a type position`);
  }
  if (/\bany\b/.test(goCode)) problems.push("Go uses `any` in a type position");
  if (/interface\s*\{\s*\}/.test(goCode)) problems.push("Go uses the empty interface in a type position");
  assert.deepEqual(problems, [], `escape hatches found:\n${problems.join("\n")}`);
});

// ---------------------------------------------------------------------------------------
// the generated Go package: formatting, compilation, and behaviour
// ---------------------------------------------------------------------------------------

test("(acceptance) the generated Go package is gofmt-clean and compiles", () => {
  const dir = makeTemp("gobuild");
  const goFile = path.join(REPO_ROOT, GO_REL);
  const formatted = run("gofmt", ["-l", goFile], { cwd: REPO_ROOT });
  assert.equal(formatted.status, 0, `gofmt failed to run\n${describeResult(formatted)}`);
  assert.equal(formatted.stdout.trim(), "", `gofmt reports ${goFile} as unformatted:\n${formatted.stdout}`);

  const build = run("go", ["build", "./..."], { cwd: path.join(REPO_ROOT, GO_MODULE_REL), env: goEnv(dir) });
  assert.equal(build.status, 0, `go build ./... failed in ${GO_MODULE_REL}\n${describeResult(build)}`);

  const vet = run("go", ["vet", "./..."], { cwd: path.join(REPO_ROOT, GO_MODULE_REL), env: goEnv(dir) });
  assert.equal(vet.status, 0, `go vet ./... failed in ${GO_MODULE_REL}\n${describeResult(vet)}`);
});

const SHA = (character) => `sha256:${character.repeat(64)}`;

function fixtures() {
  const core = {
    schema_version: "1.0",
    event_id: "11111111-1111-4111-8111-111111111111",
    tenant_id: "22222222-2222-4222-8222-222222222222",
    device_id: "33333333-3333-4333-8333-333333333333",
    user_ref: "user-1",
    tool_fingerprint: "fingerprint-1",
    occurred_at: "2026-10-02T13:00:00Z",
    monotonic_offset_ms: 12,
    source: "ext.web_request",
    dedup_key: SHA("b"),
  };
  const stored = {
    ...core,
    direction: "egress",
    kind: "prompt",
    collection_mode: "m1",
    confidence: "high",
    size_bytes: 42,
    content_digest: SHA("a"),
    labels: [{ class: "credential", score: 0.9 }],
    classifier_version: "classifier-1",
    policy_decision: { rule_id: "R-1", action: "logged", decided_locally: true },
    received_at: "2026-10-02T13:00:01Z",
  };
  const device = { ...stored };
  delete device.received_at;
  const deviceM2 = { ...device, collection_mode: "m2", content_excerpt: { kind: "match_span", text: "abc" } };
  const rollup = {
    ...core,
    direction: "none",
    kind: "usage_rollup",
    collection_mode: "m1",
    source: "proc.detect",
    window_start: "2026-10-02T12:55:00Z",
    window_end: "2026-10-02T13:00:00Z",
    submission_count: 3,
    bytes_total: 4096,
    received_at: "2026-10-02T13:00:01Z",
  };
  const detection = {
    ...core,
    direction: "none",
    kind: "model_detection",
    collection_mode: "m1",
    source: "proc.detect",
    detection_basis: "process_scan",
    received_at: "2026-10-02T13:00:01Z",
  };
  return {
    storedPromptM1: JSON.stringify(stored),
    devicePromptM1: JSON.stringify(device),
    devicePromptM2: JSON.stringify(deviceM2),
    storedRollup: JSON.stringify(rollup),
    storedDetection: JSON.stringify(detection),
  };
}

function goContractTestSource() {
  const data = fixtures();
  const lines = [];
  const push = (...values) => lines.push(...values);
  push("package envelope", "");
  push("import (", '\t"encoding/json"', '\t"strconv"', '\t"strings"', '\t"testing"', ")", "");
  push("const (");
  for (const [name, json] of Object.entries(data)) push(`\t${name} = ` + "`" + json + "`");
  push(")", "");
  push("func withJSONField(t *testing.T, document string, field string, raw string) string {");
  push("\tt.Helper()");
  push("\tvar object map[string]json.RawMessage");
  push("\tif err := json.Unmarshal([]byte(document), &object); err != nil {");
  push("\t\tt.Fatalf(\"fixture is not an object: %v\", err)");
  push("\t}");
  push("\tobject[field] = json.RawMessage(raw)");
  push("\tencoded, err := json.Marshal(object)");
  push("\tif err != nil {");
  push("\t\tt.Fatalf(\"cannot re-encode fixture: %v\", err)");
  push("\t}");
  push("\treturn string(encoded)");
  push("}", "");
  push("func withoutField(t *testing.T, document string, field string) string {");
  push("\tt.Helper()");
  push("\tvar object map[string]json.RawMessage");
  push("\tif err := json.Unmarshal([]byte(document), &object); err != nil {");
  push("\t\tt.Fatalf(\"fixture is not an object: %v\", err)");
  push("\t}");
  push("\tdelete(object, field)");
  push("\tencoded, err := json.Marshal(object)");
  push("\tif err != nil {");
  push("\t\tt.Fatalf(\"cannot re-encode fixture: %v\", err)");
  push("\t}");
  push("\treturn string(encoded)");
  push("}", "");
  push("func requireOK(t *testing.T, err error) {");
  push("\tt.Helper()");
  push("\tif err != nil {");
  push("\t\tt.Fatalf(\"expected the record to be accepted, got: %v\", err)");
  push("\t}");
  push("}", "");
  push("func requireRejected(t *testing.T, err error, want string) {");
  push("\tt.Helper()");
  push("\tif err == nil {");
  push("\t\tt.Fatalf(\"expected a rejection mentioning %q, got nil\", want)");
  push("\t}");
  push("\tif !strings.Contains(err.Error(), want) {");
  push("\t\tt.Fatalf(\"rejection %q does not mention %q\", err.Error(), want)");
  push("\t}");
  push("}", "");
  const cases = [
    {
      name: "AcceptsStoredPromptM1",
      body: [
        "\tenv, err := DecodeStoredEnvelope([]byte(storedPromptM1))",
        "\trequireOK(t, err)",
        "\tif _, ok := env.(*StoredPromptM1); !ok {",
        '\t\tt.Fatalf("decoded %T, want *StoredPromptM1", env)',
        "\t}",
        "\tif env.Core().Kind != KindPrompt || env.Core().CollectionMode != CollectionModeM1 {",
        '\t\tt.Fatalf("core = %+v", env.Core())',
        "\t}",
      ],
    },
    { name: "AcceptsStoredUsageRollup", body: ["\t_, err := DecodeStoredEnvelope([]byte(storedRollup))", "\trequireOK(t, err)"] },
    { name: "AcceptsStoredModelDetection", body: ["\t_, err := DecodeStoredEnvelope([]byte(storedDetection))", "\trequireOK(t, err)"] },
    { name: "AcceptsDevicePromptM1WithoutReceivedAt", body: ["\t_, err := DecodeDeviceSubmission([]byte(devicePromptM1))", "\trequireOK(t, err)"] },
    {
      name: "AcceptsDevicePromptM2WithExcerpt",
      body: [
        "\tenv, err := DecodeDeviceSubmission([]byte(devicePromptM2))",
        "\trequireOK(t, err)",
        "\tif _, ok := env.(*DevicePromptM2); !ok {",
        '\t\tt.Fatalf("decoded %T, want *DevicePromptM2", env)',
        "\t}",
      ],
    },
    {
      name: "RejectsMissingRequiredField",
      body: ["\t_, err := DecodeStoredEnvelope([]byte(withoutField(t, storedPromptM1, \"dedup_key\")))", '\trequireRejected(t, err, "dedup_key")'],
    },
    {
      name: "RejectsForbiddenField",
      body: [
        '\t_, err := DecodeStoredEnvelope([]byte(withJSONField(t, storedPromptM1, "window_start", `"2026-10-02T12:55:00Z"`)))',
        '\trequireRejected(t, err, "window_start")',
      ],
    },
    {
      name: "RejectsUnknownField",
      body: ['\t_, err := DecodeStoredEnvelope([]byte(withJSONField(t, storedPromptM1, "bogus_field", "1")))', '\trequireRejected(t, err, "unknown field")'],
    },
    {
      name: "RejectsUnknownKind",
      body: ['\t_, err := DecodeStoredEnvelope([]byte(withJSONField(t, storedPromptM1, "kind", strconv.Quote("prompt_v2"))))', '\trequireRejected(t, err, "closed registry")'],
    },
    {
      name: "RejectsUnknownCollectionMode",
      body: ['\t_, err := DecodeStoredEnvelope([]byte(withJSONField(t, storedPromptM1, "collection_mode", strconv.Quote("m9"))))', '\trequireRejected(t, err, "closed set")'],
    },
    {
      name: "RejectsSizeBytesOnRollup",
      body: ['\t_, err := DecodeStoredEnvelope([]byte(withJSONField(t, storedRollup, "size_bytes", "7")))', '\trequireRejected(t, err, "size_bytes")'],
    },
    {
      name: "RejectsDetectionWithoutBasis",
      body: ['\t_, err := DecodeStoredEnvelope([]byte(withoutField(t, storedDetection, "detection_basis")))', '\trequireRejected(t, err, "detection_basis")'],
    },
    {
      name: "RejectsReceivedAtOnDeviceSubmission",
      body: ['\t_, err := DecodeDeviceSubmission([]byte(withJSONField(t, devicePromptM1, "received_at", `"2026-10-02T13:00:01Z"`)))', '\trequireRejected(t, err, "received_at")'],
    },
    {
      name: "RejectsOutOfRangeLabelScore",
      body: ['\t_, err := DecodeStoredEnvelope([]byte(withJSONField(t, storedPromptM1, "labels", `[{"class":"credential","score":2}]`)))', '\trequireRejected(t, err, "score")'],
    },
    { name: "RejectsNullDocument", body: ['\t_, err := DecodeStoredEnvelope([]byte("null"))', '\trequireRejected(t, err, "closed registry")'] },
    {
      name: "RejectsArrayDocument",
      body: ['\t_, err := DecodeStoredEnvelope([]byte("[]"))', '\trequireRejected(t, err, "not a JSON object")'],
    },
    {
      name: "RejectsMalformedDigest",
      body: ['\t_, err := DecodeStoredEnvelope([]byte(withJSONField(t, storedPromptM1, "dedup_key", strconv.Quote("sha256:short"))))', '\trequireRejected(t, err, "dedup_key")'],
    },
  ];
  for (const testCase of cases) {
    push(`func Test${testCase.name}(t *testing.T) {`);
    push(...testCase.body);
    push("}", "");
  }
  return lines.join("\n");
}

test("(acceptance) the generated Go decoder accepts, rejects and dispatches per the contract", () => {
  const dir = makeTemp("gotest");
  const moduleDir = path.join(dir, "go");
  cpSync(path.join(REPO_ROOT, GO_MODULE_REL), moduleDir, { recursive: true });
  writeFileSync(path.join(moduleDir, "envelope", "contract_verify_test.go"), goContractTestSource(), "utf8");
  const result = run("go", ["test", "./..."], { cwd: moduleDir, env: goEnv(dir) });
  assert.equal(result.status, 0, `the generated decoder did not behave as the contract requires\n${describeResult(result)}`);
  assert.match(result.stdout, /ok\s+sac-verify|ok\s+shadow-ai-capture|^ok/m, `expected a passing package, got:\n${result.stdout}`);
});
