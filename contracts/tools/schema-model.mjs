// contracts/tools/schema-model.mjs
//
// Reads contracts/event-envelope.schema.json (JSON Schema draft 2020-12) and derives the
// model that both generated languages are rendered from: the closed `kind` registry, the
// per-kind and per-collection-mode field rules carried by the `if`/`then` branches inside
// $defs/envelopeCore.allOf, the two shapes (deviceSubmission and storedEnvelope), and the
// enum/object definitions.
//
// Zero external dependencies; this host has no network access, so nothing is installed.
//
// The parser is deliberately strict: a schema construct it does not understand is a hard
// error, because silently ignoring a constraint would generate types that are wrong in a
// way no test would catch.

import { readFileSync } from "node:fs";

export const SCHEMA_REL_PATH = "contracts/event-envelope.schema.json";

const DEFS_PREFIX = "#/$defs/";
const DRAFT = "2020-12";

// $defs -> emitted type names. A definition the generator does not know is a hard error,
// so that adding one to the schema forces a look at the generated types.
const STRING_DEF_TYPES = { sha256: "Sha256" };
const ENUM_DEF_TYPES = { route: "Route" };
const OBJECT_DEF_TYPES = {
  label: "Label",
  policyDecision: "PolicyDecision",
  attachment: "Attachment",
  excerpt: "Excerpt",
};

// Inline string enums, keyed "<object def>.<property>". Every inline enum in the schema
// must appear here; a new one is a hard error rather than a silently open string.
const INLINE_ENUM_TYPES = {
  "envelopeCore.kind": "Kind",
  "envelopeCore.direction": "Direction",
  "envelopeCore.confidence": "Confidence",
  "envelopeCore.collection_mode": "CollectionMode",
  "envelopeCore.detection_basis": "DetectionBasis",
  "envelopeCore.prompt_kind": "PromptKind",
  "policyDecision.action": "PolicyAction",
  "excerpt.kind": "ExcerptKind",
};

// Emission order of the enum sections in both languages (stable, not schema order).
const ENUM_ORDER = [
  "Kind",
  "Route",
  "Direction",
  "CollectionMode",
  "Confidence",
  "PolicyAction",
  "ExcerptKind",
  "DetectionBasis",
  "PromptKind",
];

// The two shapes the contract defines on top of the common core.
const SHAPES = ["device", "stored"];
export const SHAPE_UNION_NAME = { device: "DeviceSubmission", stored: "StoredEnvelope" };
const RECEIVED_AT = "received_at";

// Go identifiers that must keep their conventional casing.
const GO_INITIALISMS = new Set([
  "id", "ids", "ms", "tls", "cli", "dom", "etw", "url", "api", "json", "uuid", "http", "https", "utc",
]);

/** PascalCase for TypeScript type names: `usage_rollup` -> `UsageRollup`, `m0` -> `M0`. */
export function pascal(value) {
  return String(value)
    .split(/[^A-Za-z0-9]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join("");
}

/** PascalCase for Go identifiers, keeping Go's initialisms: `ext.web_request` -> `ExtWebRequest`. */
export function goPascal(value) {
  return String(value)
    .split(/[^A-Za-z0-9]+/)
    .filter(Boolean)
    .map((part) =>
      GO_INITIALISMS.has(part.toLowerCase()) ? part.toUpperCase() : part.charAt(0).toUpperCase() + part.slice(1),
    )
    .join("");
}

export function loadSchema(schemaPath) {
  let raw;
  try {
    raw = readFileSync(schemaPath, "utf8");
  } catch (err) {
    throw new Error(`schema-model: cannot read ${schemaPath}: ${err.message}`);
  }
  let schema;
  try {
    schema = JSON.parse(raw);
  } catch (err) {
    throw new Error(`schema-model: ${schemaPath} is not valid JSON: ${err.message}`);
  }
  const draft = String(schema.$schema ?? "");
  if (!draft.includes(DRAFT)) {
    throw new Error(`schema-model: expected a JSON Schema ${DRAFT} document, got $schema=${JSON.stringify(schema.$schema)}`);
  }
  if (schema.$ref !== "#/$defs/storedEnvelope") {
    throw new Error(`schema-model: expected $ref "#/$defs/storedEnvelope" as the document root, got ${JSON.stringify(schema.$ref)}`);
  }
  if (!schema.$defs || typeof schema.$defs !== "object") {
    throw new Error("schema-model: the schema has no $defs object");
  }
  return schema;
}

export function resolveLocalRef(schema, ref) {
  if (typeof ref !== "string" || !ref.startsWith(DEFS_PREFIX)) {
    throw new Error(`schema-model: only local ${DEFS_PREFIX}* references are supported, got ${JSON.stringify(ref)}`);
  }
  const name = ref.slice(DEFS_PREFIX.length);
  if (name.includes("/")) {
    throw new Error(`schema-model: nested JSON pointers are not supported, got ${JSON.stringify(ref)}`);
  }
  const def = schema.$defs[name];
  if (!def) {
    throw new Error(`schema-model: unresolved $ref ${JSON.stringify(ref)}`);
  }
  return { name, def };
}

function clean(text) {
  return String(text ?? "")
    .replace(/\s+/g, " ")
    .replace(/\*\//g, "* /")
    .trim();
}

/** Field descriptor for one property of a $defs object. */
function describeField(schema, ownerDefName, name, prop) {
  const field = { name, owner: ownerDefName, description: clean(prop.description) };
  for (const key of ["const", "pattern", "format", "minLength", "maxLength", "minimum", "maximum", "minItems", "maxItems"]) {
    if (prop[key] !== undefined) field[key] = prop[key];
  }

  if (prop.$ref) {
    const { name: defName, def } = resolveLocalRef(schema, prop.$ref);
    field.def = defName;
    if (STRING_DEF_TYPES[defName]) {
      field.type = "string";
      field.typeName = STRING_DEF_TYPES[defName];
      if (field.pattern === undefined && def.pattern !== undefined) field.pattern = def.pattern;
      if (!field.description) field.description = clean(def.description);
    } else if (ENUM_DEF_TYPES[defName]) {
      if (!Array.isArray(def.enum)) throw new Error(`schema-model: $defs/${defName} is mapped to an enum but has no enum`);
      field.type = "string";
      field.enumType = ENUM_DEF_TYPES[defName];
      field.enumValues = [...def.enum];
      if (!field.description) field.description = clean(def.description);
    } else if (OBJECT_DEF_TYPES[defName]) {
      if (def.type !== "object") throw new Error(`schema-model: $defs/${defName} is mapped to an object but has type ${def.type}`);
      field.type = "object";
      field.objectType = OBJECT_DEF_TYPES[defName];
      if (!field.description) field.description = clean(def.description);
    } else {
      throw new Error(`schema-model: no emitted type is mapped for $defs/${defName}`);
    }
    return field;
  }

  if (Array.isArray(prop.enum)) {
    const typeName = INLINE_ENUM_TYPES[`${ownerDefName}.${name}`];
    if (!typeName) throw new Error(`schema-model: unmapped inline enum at ${ownerDefName}.${name}`);
    field.type = "string";
    field.enumType = typeName;
    field.enumValues = [...prop.enum];
    return field;
  }

  switch (prop.type) {
    case "string":
      field.type = "string";
      return field;
    case "integer":
    case "number":
    case "boolean":
      field.type = prop.type;
      return field;
    case "array": {
      if (!prop.items || !prop.items.$ref) {
        throw new Error(`schema-model: ${ownerDefName}.${name} must declare array items as a local $ref`);
      }
      const { name: defName, def } = resolveLocalRef(schema, prop.items.$ref);
      if (!OBJECT_DEF_TYPES[defName]) {
        throw new Error(`schema-model: array items of ${ownerDefName}.${name} resolve to $defs/${defName}, which is not an object type`);
      }
      if (def.type !== "object") throw new Error(`schema-model: $defs/${defName} is not an object`);
      field.type = "array";
      field.itemsType = OBJECT_DEF_TYPES[defName];
      return field;
    }
    default:
      throw new Error(
        `schema-model: unsupported property ${ownerDefName}.${name} (type=${JSON.stringify(prop.type)}, keys=${Object.keys(prop).join(",")})`,
      );
  }
}

function parseForbidden(not) {
  if (not === undefined) return [];
  if (Array.isArray(not.required)) return [...not.required];
  if (Array.isArray(not.anyOf)) {
    const out = [];
    for (const entry of not.anyOf) {
      if (Array.isArray(entry.required)) out.push(...entry.required);
      else throw new Error(`schema-model: unsupported not.anyOf entry ${JSON.stringify(entry)}`);
    }
    return out;
  }
  throw new Error(`schema-model: unsupported \`not\` shape ${JSON.stringify(not)}`);
}

function parseBranch(index, branch) {
  const ifSchema = branch.if;
  const then = branch.then;
  if (!ifSchema || !then) {
    throw new Error(`schema-model: envelopeCore.allOf[${index}] must have both if and then`);
  }
  const kindProp = ifSchema.properties?.kind;
  if (!kindProp || typeof kindProp.const !== "string") {
    throw new Error(`schema-model: envelopeCore.allOf[${index}].if must pin kind with a const`);
  }
  const ifRequired = ifSchema.required ?? [];
  if (!ifRequired.includes("kind")) {
    throw new Error(`schema-model: envelopeCore.allOf[${index}].if must require "kind"`);
  }
  const modeProp = ifSchema.properties?.collection_mode;
  let level = "kind";
  let modes = null;
  if (modeProp) {
    level = "mode";
    if (!ifRequired.includes("collection_mode")) {
      throw new Error(`schema-model: envelopeCore.allOf[${index}].if constrains collection_mode but does not require it`);
    }
    modes = typeof modeProp.const === "string" ? [modeProp.const] : Array.isArray(modeProp.enum) ? [...modeProp.enum] : null;
    if (!modes) throw new Error(`schema-model: envelopeCore.allOf[${index}].if must pin collection_mode with const or enum`);
  }
  const pinned = {};
  for (const [name, value] of Object.entries(then.properties ?? {})) {
    if (value.const === undefined) {
      throw new Error(`schema-model: envelopeCore.allOf[${index}].then.properties.${name} is not a const pin`);
    }
    pinned[name] = value.const;
  }
  return {
    index,
    level,
    kind: kindProp.const,
    modes,
    comment: clean(branch.comment),
    required: [...(then.required ?? [])],
    forbidden: parseForbidden(then.not),
    pinned,
  };
}

function emptyRule() {
  return { required: [], forbidden: [], pinned: {}, comments: [] };
}

function mergeRule(rule, branch) {
  rule.required.push(...branch.required);
  rule.forbidden.push(...branch.forbidden);
  Object.assign(rule.pinned, branch.pinned);
  if (branch.comment) rule.comments.push(branch.comment);
}

function inSchemaOrder(names, order) {
  const seen = new Set();
  const out = [];
  for (const name of order) {
    if (names.includes(name) && !seen.has(name)) {
      seen.add(name);
      out.push(name);
    }
  }
  for (const name of names) {
    if (!seen.has(name)) throw new Error(`schema-model: ${name} is not a declared property of envelopeCore`);
  }
  return out;
}

export function buildModel(schema) {
  const defs = schema.$defs;
  const knownDefs = new Set([
    ...Object.keys(STRING_DEF_TYPES),
    ...Object.keys(ENUM_DEF_TYPES),
    ...Object.keys(OBJECT_DEF_TYPES),
    "envelopeCore",
    "storedEnvelope",
    "deviceSubmission",
  ]);
  const unknownDefs = Object.keys(defs).filter((name) => !knownDefs.has(name));
  if (unknownDefs.length > 0) {
    throw new Error(`schema-model: unmapped $defs: ${unknownDefs.join(", ")} - the generator must be taught the new definition`);
  }

  // ---- enums -------------------------------------------------------------------------
  const enums = new Map();
  const addEnum = (typeName, values, description, source) => {
    if (enums.has(typeName)) throw new Error(`schema-model: duplicate enum type ${typeName}`);
    enums.set(typeName, { typeName, values: [...values], description: clean(description), source });
  };
  for (const [defName, typeName] of Object.entries(ENUM_DEF_TYPES)) {
    const def = defs[defName];
    if (!def || !Array.isArray(def.enum)) throw new Error(`schema-model: $defs/${defName} must declare an enum`);
    addEnum(typeName, def.enum, def.description, `$defs/${defName}`);
  }

  // ---- nested objects ----------------------------------------------------------------
  const objects = [];
  for (const [defName, typeName] of Object.entries(OBJECT_DEF_TYPES)) {
    const def = defs[defName];
    if (!def || def.type !== "object") throw new Error(`schema-model: $defs/${defName} must be an object`);
    if (def.additionalProperties !== false) {
      throw new Error(`schema-model: $defs/${defName} must set additionalProperties:false to stay closed`);
    }
    const required = [...(def.required ?? [])];
    const fields = Object.entries(def.properties ?? {}).map(([name, prop]) => describeField(schema, defName, name, prop));
    for (const name of required) {
      if (!fields.some((f) => f.name === name)) throw new Error(`schema-model: $defs/${defName} requires undeclared property ${name}`);
    }
    objects.push({ defName, typeName, description: clean(def.description), required, fields });
  }

  // ---- the common core ---------------------------------------------------------------
  const coreDef = defs.envelopeCore;
  if (!coreDef || coreDef.type !== "object") throw new Error("schema-model: $defs/envelopeCore must be an object");
  if (coreDef.additionalProperties !== false) {
    throw new Error("schema-model: $defs/envelopeCore must set additionalProperties:false to stay closed");
  }
  const coreRequired = [...(coreDef.required ?? [])];
  const declaredFields = Object.entries(coreDef.properties ?? {}).map(([name, prop]) => describeField(schema, "envelopeCore", name, prop));
  const propertyOrder = declaredFields.map((f) => f.name);
  const propertyIndex = new Map(propertyOrder.map((name, i) => [name, i]));
  for (const name of coreRequired) {
    if (!propertyIndex.has(name)) throw new Error(`schema-model: envelopeCore.required names undeclared property ${name}`);
  }
  // The core is only the universally required fields; everything else is a per-variant field,
  // because a kind that must not carry a property must not have it in the embedded core.
  const coreFields = declaredFields.filter((f) => coreRequired.includes(f.name)).map((f) => ({ ...f, required: true }));

  // Inline enums declared on the core and on nested objects.
  for (const field of [...declaredFields, ...objects.flatMap((o) => o.fields)]) {
    if (field.enumType) {
      const owner = field.owner;
      const expected = INLINE_ENUM_TYPES[`${owner}.${field.name}`] ?? ENUM_DEF_TYPES[field.def];
      if (expected !== field.enumType) {
        throw new Error(`schema-model: enum ${owner}.${field.name} maps to ${field.enumType}, expected ${expected}`);
      }
      if (!enums.has(field.enumType)) addEnum(field.enumType, field.enumValues, field.description, `${owner}.${field.name}`);
    }
  }

  // ---- kind registry -----------------------------------------------------------------
  const kindField = declaredFields.find((f) => f.name === "kind");
  if (!kindField || !kindField.enumValues) throw new Error("schema-model: envelopeCore.kind must be an inline enum");
  const kinds = [...kindField.enumValues];
  const modeField = declaredFields.find((f) => f.name === "collection_mode");
  if (!modeField || !modeField.enumValues) throw new Error("schema-model: envelopeCore.collection_mode must be an inline enum");
  const collectionModes = [...modeField.enumValues];
  const schemaVersionField = declaredFields.find((f) => f.name === "schema_version");
  if (!schemaVersionField || typeof schemaVersionField.const !== "string") {
    throw new Error("schema-model: envelopeCore.schema_version must pin a string const");
  }
  const schemaVersion = schemaVersionField.const;

  // ---- if/then branches --------------------------------------------------------------
  const branches = (coreDef.allOf ?? []).map((branch, i) => parseBranch(i, branch));
  const kindRules = new Map(kinds.map((kind) => [kind, emptyRule()]));
  const modeRules = new Map(kinds.map((kind) => [kind, new Map()]));
  for (const branch of branches) {
    if (!kindRules.has(branch.kind)) {
      throw new Error(`schema-model: envelopeCore.allOf[${branch.index}] constrains kind ${branch.kind}, which is not in the closed registry`);
    }
    if (branch.level === "kind") {
      mergeRule(kindRules.get(branch.kind), branch);
      continue;
    }
    const perKind = modeRules.get(branch.kind);
    for (const mode of branch.modes) {
      if (!collectionModes.includes(mode)) {
        throw new Error(`schema-model: envelopeCore.allOf[${branch.index}] constrains collection_mode ${mode}, which is not in the enum`);
      }
      if (!perKind.has(mode)) perKind.set(mode, emptyRule());
      mergeRule(perKind.get(mode), branch);
    }
  }
  const branchAudit = [];
  for (const kind of kinds) {
    const rule = kindRules.get(kind);
    if (rule.required.length === 0 && rule.forbidden.length === 0 && Object.keys(rule.pinned).length === 0) {
      branchAudit.push(`kind ${kind} has no if/then branch in envelopeCore.allOf`);
    }
  }

  // ---- variants ----------------------------------------------------------------------
  const variants = [];
  const permittedNotRequired = new Map();
  for (const kind of kinds) {
    const kindRule = kindRules.get(kind);
    const perKindModes = modeRules.get(kind);
    const modes = perKindModes.size > 0 ? collectionModes : [null];
    for (const mode of modes) {
      const modeRule = mode === null ? emptyRule() : perKindModes.get(mode) ?? emptyRule();
      const pinned = { ...kindRule.pinned, ...modeRule.pinned, kind };
      if (mode !== null) pinned.collection_mode = mode;
      pinned.schema_version = schemaVersion;

      const required = inSchemaOrder([...coreRequired, ...kindRule.required, ...modeRule.required], propertyOrder);
      const forbidden = inSchemaOrder([...kindRule.forbidden, ...modeRule.forbidden], propertyOrder);
      const overlap = required.filter((name) => forbidden.includes(name));
      if (overlap.length > 0) {
        throw new Error(
          `schema-model: kind=${kind}${mode ? ` mode=${mode}` : ""} both requires and forbids ${overlap.join(", ")} - the schema is contradictory`,
        );
      }
      for (const name of Object.keys(pinned)) {
        if (!propertyIndex.has(name)) throw new Error(`schema-model: pinned field ${name} is not a declared property`);
      }

      // Own fields are the properties the core does not already hold: the core struct carries
      // the universally required fields, and each variant carries the rest.
      const ownOrder = [RECEIVED_AT, ...propertyOrder.filter((name) => name !== RECEIVED_AT && !coreRequired.includes(name))];
      for (const shape of SHAPES) {
        const shapeRequired = shape === "stored" ? [RECEIVED_AT] : [];
        const shapeForbidden = shape === "device" ? [RECEIVED_AT] : [];
        const allRequired = inSchemaOrder([...required, ...shapeRequired], propertyOrder);
        const allForbidden = inSchemaOrder([...forbidden, ...shapeForbidden], propertyOrder);
        const allOptional = propertyOrder.filter((name) => !allRequired.includes(name) && !allForbidden.includes(name));
        const variantName = pascal(shape) + pascal(kind) + (mode === null ? "" : pascal(mode));
        const ownFields = ownOrder
          .filter((name) => !allForbidden.includes(name))
          .map((name) => {
            const field = declaredFields[propertyIndex.get(name)];
            return { ...field, required: allRequired.includes(name) };
          });
        // Core fields whose value is pinned by a branch, where the pin is narrower than
        // the core's own declaration (schema_version's const is already the pin, so it is
        // not re-declared).
        const narrowedCore = coreFields
          .filter((f) => coreRequired.includes(f.name) && pinned[f.name] !== undefined && pinned[f.name] !== f.const)
          .map((f) => ({ ...f, pinnedValue: pinned[f.name] }));
        variants.push({
          name: variantName,
          shape,
          kind,
          mode,
          required: allRequired,
          forbidden: allForbidden,
          optional: allOptional,
          pinned,
          narrowedCore,
          ownFields,
          comments: [...kindRule.comments, ...modeRule.comments],
        });
        permittedNotRequired.set(variantName, allOptional);
      }
    }
  }

  const enumList = ENUM_ORDER.map((typeName) => {
    const value = enums.get(typeName);
    if (!value) throw new Error(`schema-model: expected enum ${typeName} is missing from the schema`);
    return value;
  });
  if (enumList.length !== enums.size) {
    throw new Error(`schema-model: enum order covers ${enumList.length} of ${enums.size} enums`);
  }

  return {
    schemaId: schema.$id,
    title: clean(schema.title),
    description: clean(schema.description),
    schemaVersion,
    kinds,
    collectionModes,
    coreRequired,
    coreFields,
    declaredFields,
    propertyOrder,
    objects,
    enums: enumList,
    variants,
    branches,
    audit: {
      branchWarnings: branchAudit,
      permittedNotRequired,
      unknownDefs: [],
    },
  };
}

export function loadModel(schemaPath) {
  return buildModel(loadSchema(schemaPath));
}
