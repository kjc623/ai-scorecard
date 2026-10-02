// contracts/tools/generate.mjs
//
// Generates the TypeScript and Go types for the event envelope from
// contracts/event-envelope.schema.json. The schema is the only source of field names;
// this file contains no contract knowledge of its own beyond how to render it.
//
//   node contracts/tools/generate.mjs           write the generated files
//   node contracts/tools/generate.mjs --check   fail (exit 1) if the committed files differ
//
// Zero external dependencies: this host has no network access.

import { readFileSync, writeFileSync, mkdirSync, existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { SCHEMA_REL_PATH, SHAPE_UNION_NAME, goPascal, loadModel, pascal } from "./schema-model.mjs";

const HERE = path.dirname(fileURLToPath(import.meta.url));
export const CONTRACTS_DIR = path.resolve(HERE, "..");
export const REPO_ROOT = path.resolve(CONTRACTS_DIR, "..");

const TS_REL = "contracts/generated/typescript/envelope.ts";
const GO_REL = "contracts/generated/go/envelope/envelope.go";

const GENERATED_BY = "contracts/tools/generate.mjs";
const REGENERATE = "node contracts/tools/generate.mjs";
const VERIFY = "node --test contracts/tools/";

// ---------------------------------------------------------------------------------------
// small text helpers
// ---------------------------------------------------------------------------------------

function wrap(text, width = 96) {
  const words = String(text ?? "").split(" ").filter(Boolean);
  const lines = [];
  let line = "";
  for (const word of words) {
    if (line.length === 0) line = word;
    else if (line.length + 1 + word.length <= width) line += ` ${word}`;
    else {
      lines.push(line);
      line = word;
    }
  }
  if (line.length > 0) lines.push(line);
  return lines.length > 0 ? lines : [""];
}

function tsDoc(text, indent = "") {
  const lines = Array.isArray(text) ? text : wrap(text);
  const out = [`${indent}/**`];
  for (const line of lines) out.push(line.length > 0 ? `${indent} * ${line}` : `${indent} *`);
  out.push(`${indent} */`);
  return out;
}

function goComment(text, indent = "") {
  const lines = Array.isArray(text) ? text : wrap(text);
  return lines.map((line) => (line.length > 0 ? `${indent}// ${line}` : `${indent}//`));
}

/** Pads every column except the last to the widest cell in that column plus one space. */
function alignRows(rows) {
  const widths = [];
  for (const row of rows) {
    row.forEach((cell, index) => {
      widths[index] = Math.max(widths[index] ?? 0, cell.length);
    });
  }
  return rows.map((row) => row.map((cell, index) => (index === row.length - 1 ? cell : cell.padEnd(widths[index] + 1))).join(""));
}

/**
 * Renders struct fields the way gofmt does: a comment line ends the alignment run that
 * precedes it and the field it documents opens the next run. Fields in one run are padded
 * to that run's widest cell in each column.
 * entries: [{ comments: string[] | null, name, type, tag }]
 */
function renderStructFields(entries, indent = "\t") {
  const out = [];
  let run = [];
  const flush = () => {
    if (run.length === 0) return;
    out.push(...alignRows(run.map((entry) => [`${indent}${entry.name}`, entry.type, entry.tag])));
    run = [];
  };
  for (const entry of entries) {
    if (entry.comments && entry.comments.length > 0) {
      flush();
      out.push(...entry.comments);
    }
    run.push(entry);
  }
  flush();
  return out;
}

function upperSnake(name) {
  return name
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
    .toUpperCase();
}

function enumConstName(typeName, value) {
  return `${typeName}${goPascal(value)}`;
}

// ---------------------------------------------------------------------------------------
// language types
// ---------------------------------------------------------------------------------------

const TS_FORMAT_TYPES = { uuid: "Uuid", "date-time": "DateTime" };
const GO_FORMAT_TYPES = { uuid: "UUID", "date-time": "DateTime" };

function tsTypeName(field) {
  if (field.type === "string") {
    if (field.typeName) return field.typeName;
    if (field.enumType) return field.enumType;
    if (field.format && TS_FORMAT_TYPES[field.format]) return TS_FORMAT_TYPES[field.format];
    return "string";
  }
  if (field.type === "integer" || field.type === "number") return "number";
  if (field.type === "boolean") return "boolean";
  if (field.type === "object") return field.objectType;
  if (field.type === "array") return `readonly ${field.itemsType}[]`;
  throw new Error(`generate: no TypeScript type for ${field.owner}.${field.name}`);
}

function tsType(field) {
  if (field.pinnedValue !== undefined) return JSON.stringify(field.pinnedValue);
  if (field.const !== undefined) return JSON.stringify(field.const);
  return tsTypeName(field);
}

function goTypeName(field) {
  if (field.type === "string") {
    if (field.typeName) return field.typeName;
    if (field.enumType) return field.enumType;
    if (field.format && GO_FORMAT_TYPES[field.format]) return GO_FORMAT_TYPES[field.format];
    return "string";
  }
  if (field.type === "integer") return "int64";
  if (field.type === "number") return "float64";
  if (field.type === "boolean") return "bool";
  if (field.type === "object") return field.objectType;
  if (field.type === "array") return `[]${field.itemsType}`;
  throw new Error(`generate: no Go type for ${field.owner}.${field.name}`);
}

function goType(field, required) {
  const base = goTypeName(field);
  return required ? base : `*${base}`;
}

function goZeroChecks(field) {
  // Literal for a pinned/const value, used in equality checks.
  if (typeof field.pinnedValue === "string") return JSON.stringify(field.pinnedValue);
  if (typeof field.const === "string") return JSON.stringify(field.const);
  return null;
}

function regexVarName(field) {
  return field.def ? `re${goPascal(field.def)}` : `re${goPascal(field.owner)}${goPascal(field.name)}`;
}

// ---------------------------------------------------------------------------------------
// TypeScript
// ---------------------------------------------------------------------------------------

function renderTsHeader(model) {
  const lines = [
    `// Code generated by ${GENERATED_BY} from ${SCHEMA_REL_PATH}. DO NOT EDIT.`,
    `// Regenerate: ${REGENERATE}    Drift is a test failure: ${VERIFY}`,
    "//",
    ...wrap(`Contract: ${model.title}. ${model.description}`, 100).map((l) => `// ${l}`),
    "//",
    `// Source of truth: ${model.schemaId}`,
    "// ADR 0010: `kind` is a closed registry and the envelope is a discriminated union on it.",
    "// Every field declared without `?` is required by the schema; `?` marks a field the schema",
    "// permits but does not require; a field the schema forbids for a kind and mode is absent",
    "// from that interface entirely. Observations are immutable, so every field is `readonly`.",
  ];
  return lines;
}

function renderTsEnums(model) {
  const out = [];
  for (const enumDef of model.enums) {
    const constName = `${upperSnake(enumDef.typeName)}S`;
    out.push(...tsDoc(`The closed ${enumDef.typeName} set from ${enumDef.source}. ${enumDef.description}`));
    out.push(`export const ${constName} = [${enumDef.values.map((v) => JSON.stringify(v)).join(", ")}] as const;`);
    out.push("");
    out.push(...tsDoc([`A member of ${constName}: the union is derived from the runtime list, so a value`,"cannot be added to the type without being added to the closed set."]));
    out.push(`export type ${enumDef.typeName} = (typeof ${constName})[number];`);
    out.push("");
  }
  return out;
}

function renderTsField(field, required) {
  const out = [];
  const tags = [];
  if (field.const !== undefined) tags.push(`@constant ${JSON.stringify(field.const)}`);
  if (field.pinnedValue !== undefined) tags.push(`@constant ${JSON.stringify(field.pinnedValue)}`);
  if (field.enumValues) tags.push(`@enum ${field.enumValues.map((v) => JSON.stringify(v)).join(" | ")}`);
  if (field.format) tags.push(`@format ${field.format}`);
  if (field.pattern) tags.push(`@pattern ${field.pattern}`);
  if (field.minLength !== undefined) tags.push(`@minLength ${field.minLength}`);
  if (field.maxLength !== undefined) tags.push(`@maxLength ${field.maxLength}`);
  if (field.minimum !== undefined) tags.push(`@minimum ${field.minimum}`);
  if (field.maximum !== undefined) tags.push(`@maximum ${field.maximum}`);
  if (field.maxItems !== undefined) tags.push(`@maxItems ${field.maxItems}`);
  out.push(...tsDoc([...wrap(field.description), ...tags], "  "));
  out.push(`  readonly ${field.name}${required ? "" : "?"}: ${tsType(field)};`);
  return out;
}

function renderTsObject(objectDef) {
  const out = [];
  out.push(...tsDoc(`${objectDef.typeName}: ${objectDef.description}`));
  out.push(`export interface ${objectDef.typeName} {`);
  for (const field of objectDef.fields) {
    out.push(...renderTsField(field, objectDef.required.includes(field.name)));
  }
  out.push("}");
  out.push("");
  return out;
}

function renderTsVariant(variant) {
  const out = [];
  const where = variant.mode ? ` at collection mode \`${variant.mode}\`` : "";
  const shape = variant.shape === "stored" ? "The envelope as stored" : "What a device is permitted to send";
  const doc = [
    `${shape} for kind \`${variant.kind}\`${where}.`,
    "",
    `Required by the schema: the common core${variant.narrowedCore.length > 0 ? ` (with \`${variant.narrowedCore.map((f) => f.name).join("`, `")}\` pinned)` : ""}${
      variant.ownFields.filter((f) => f.required).length > 0 ? `, plus \`${variant.ownFields.filter((f) => f.required).map((f) => f.name).join("`, `")}\`` : ""
    }.`,
    variant.optional.length > 0
      ? `Permitted but not required: \`${variant.optional.join("`, `")}\`.`
      : "Permitted but not required: nothing beyond the required fields.",
    variant.forbidden.length > 0
      ? `Must not carry, so absent from this interface: \`${variant.forbidden.join("`, `")}\`.`
      : "No field is forbidden for this kind.",
  ];
  out.push(...tsDoc(doc));
  out.push(`export interface ${variant.name} extends EnvelopeCore {`);
  for (const field of variant.narrowedCore) out.push(...renderTsField(field, true));
  for (const field of variant.ownFields) out.push(...renderTsField(field, field.required));
  out.push("}");
  out.push("");
  return out;
}

function renderTypeScript(model) {
  const out = [...renderTsHeader(model), ""];
  out.push(...tsDoc(`The only schema_version this contract describes; a new value is a new contract.`));
  out.push(`export const SCHEMA_VERSION = ${JSON.stringify(model.schemaVersion)} as const;`);
  out.push("");
  out.push(...tsDoc(`The document version this record validates against.`));
  out.push("export type SchemaVersion = typeof SCHEMA_VERSION;");
  out.push("");
  out.push(...tsDoc(`Lowercase hex SHA-256 with the \`sha256:\` prefix; never truncated or base64.`));
  out.push("export type Sha256 = string;");
  out.push("");
  out.push(...tsDoc(`A UUID as it appears on the wire.`));
  out.push("export type Uuid = string;");
  out.push("");
  out.push(...tsDoc(`An RFC 3339 date-time as it appears on the wire.`));
  out.push("export type DateTime = string;");
  out.push("");
  out.push(...renderTsEnums(model));
  for (const objectDef of model.objects) out.push(...renderTsObject(objectDef));

  out.push(...tsDoc(`The fields required on every envelope, whatever its kind. This interface is not by itself a valid record: kind, direction and collection_mode are pinned per variant.`));
  out.push("export interface EnvelopeCore {");
  for (const field of model.coreFields) out.push(...renderTsField(field, field.required));
  out.push("}");
  out.push("");

  for (const variant of model.variants) out.push(...renderTsVariant(variant));

  for (const shape of ["device", "stored"]) {
    const unionName = SHAPE_UNION_NAME[shape];
    const members = model.variants.filter((v) => v.shape === shape).map((v) => v.name);
    const doc =
      shape === "device"
        ? "The closed discriminated union on `kind` of what a device is permitted to send. `received_at` is absent from every member: a device-supplied receive time would be neither of the two clocks the contract allows."
        : "The closed discriminated union on `kind` of the envelope as stored, which is the device submission plus the server-assigned `received_at`.";
    out.push(...tsDoc(doc));
    out.push(`export type ${unionName} =`);
    out.push(members.map((name) => `  | ${name}`).join("\n") + ";");
    out.push("");
  }

  return `${out.join("\n").trimEnd()}\n`;
}

// ---------------------------------------------------------------------------------------
// Go
// ---------------------------------------------------------------------------------------

function renderGoHeader(model) {
  const lines = [
    `// Code generated by ${GENERATED_BY} from ${SCHEMA_REL_PATH}. DO NOT EDIT.`,
    `// Regenerate: ${REGENERATE}    Drift is a test failure: ${VERIFY}`,
    "//",
    ...wrap(`Contract: ${model.title}. ${model.description}`, 100).map((l) => `// ${l}`),
    "//",
    `// Source of truth: ${model.schemaId}`,
    "// ADR 0010: `kind` is a closed registry and the envelope is a discriminated union on it.",
    "//",
    "// Encoding of the contract in Go:",
    "//   - a field the schema requires is a value field with no `omitempty`, so it is always",
    "//     marshalled; a field the schema permits but does not require is a pointer with",
    "//     `omitempty` (an array is a pointer-to-slice so that present-but-empty survives a",
    "//     round trip); a field the schema forbids for a kind and mode is absent from that",
    "//     variant's struct entirely, so it cannot be compiled into a record.",
    "//   - the closed unions are the interfaces below, one implementation per variant, each",
    "//     asserted at compile time; Decode* refuses an unknown kind or mode instead of",
    "//     defaulting, and refuses a missing required field or a forbidden field by name.",
    "//   - `format` is asserted only as non-emptiness; the schema's `pattern` constraints are",
    "//     enforced as written.",
  ];
  const permitted = model.variants
    .map((v) => (v.optional.length > 0 ? `${v.name}: ${v.optional.join(", ")}` : null))
    .filter(Boolean);
  if (permitted.length > 0) {
    lines.push("//");
    lines.push("// Permitted by the schema but not required, so optional in the structs below:");
    for (const entry of permitted) lines.push(`//   ${entry}`);
  }
  for (const warning of model.audit.branchWarnings) {
    lines.push("//");
    lines.push(`// SCHEMA NOTE: ${warning}`);
  }
  return lines;
}

function renderGoEnum(enumDef) {
  const out = [];
  out.push(...goComment(`${enumDef.typeName} is the closed set from ${enumDef.source}. ${enumDef.description}`));
  out.push(`type ${enumDef.typeName} string`);
  out.push("");
  out.push("const (");
  out.push(
    ...alignRows(enumDef.values.map((value) => [`\t${enumConstName(enumDef.typeName, value)}`, `${enumDef.typeName} = ${JSON.stringify(value)}`])),
  );
  out.push(")");
  out.push("");
  out.push(...goComment(`Valid reports whether the value is inside the closed set. A value outside it is`));
  out.push(...goComment(`refused rather than defaulted, which is what keeps the registry closed in practice.`));
  out.push(`func (v ${enumDef.typeName}) Valid() bool {`);
  out.push("\tswitch v {");
  out.push(`\tcase ${enumDef.values.map((value) => enumConstName(enumDef.typeName, value)).join(", ")}:`);
  out.push("\t\treturn true");
  out.push("\t}");
  out.push("\treturn false");
  out.push("}");
  out.push("");
  out.push(...goComment(`All${enumDef.typeName}s returns the closed set in schema order.`));
  out.push(`func All${enumDef.typeName}s() []${enumDef.typeName} {`);
  out.push(`\treturn []${enumDef.typeName}{${enumDef.values.map((value) => enumConstName(enumDef.typeName, value)).join(", ")}}`);
  out.push("}");
  out.push("");
  return out;
}

/** Constraint checks for one field, as Go statements indented one level inside a method. */
function goFieldChecks(ownerType, field, required) {
  const out = [];
  const goName = goPascal(field.name);
  const label = `${ownerType}: ${field.name}`;
  const access = required ? `e.${goName}` : `(*e.${goName})`;

  const checks = [];
  const statement = (condition, message, args = []) => {
    checks.push(`\tif ${condition} {`);
    checks.push(`\t\treturn fmt.Errorf(${[JSON.stringify(message), ...args].join(", ")})`);
    checks.push("\t}");
  };

  if (field.type === "string" && field.enumType) {
    statement(`!${access}.Valid()`, `envelope: ${label} is %q, which is outside the closed set`, [`string(${access})`]);
  } else if (field.type === "string") {
    if (field.minLength !== undefined) {
      statement(`len(${access}) < ${field.minLength}`, `envelope: ${label} must be at least ${field.minLength} character(s), got %d`, [`len(${access})`]);
    }
    if (field.maxLength !== undefined) {
      statement(`len(${access}) > ${field.maxLength}`, `envelope: ${label} must be at most ${field.maxLength} character(s), got %d`, [`len(${access})`]);
    }
    if (field.pattern) {
      statement(`!${regexVarName(field)}.MatchString(${access})`, `envelope: ${label} must match %s`, [regexVarName(field)]);
    }
    if (field.format && field.minLength === undefined && !field.pattern) {
      statement(`${access} == ""`, `envelope: ${label} is required and must not be empty`);
    }
  } else if (field.type === "integer" || field.type === "number") {
    const verb = field.type === "integer" ? "%d" : "%v";
    if (field.minimum !== undefined) {
      statement(`${access} < ${field.minimum}`, `envelope: ${label} must be >= ${field.minimum}, got ${verb}`, [access]);
    }
    if (field.maximum !== undefined) {
      statement(`${access} > ${field.maximum}`, `envelope: ${label} must be <= ${field.maximum}, got ${verb}`, [access]);
    }
  } else if (field.type === "array") {
    if (required) statement(`${access} == nil`, `envelope: ${label} is required and must be present`);
    checks.push(`\tfor i := range ${access} {`);
    checks.push(`\t\tif err := ${access}[i].Validate(); err != nil {`);
    checks.push(`\t\t\treturn fmt.Errorf("envelope: ${label}[%d]: %w", i, err)`);
    checks.push("\t\t}");
    checks.push("\t}");
  } else if (field.type === "object") {
    checks.push(`\tif err := ${access}.Validate(); err != nil {`);
    checks.push(`\t\treturn fmt.Errorf("envelope: ${label}: %w", err)`);
    checks.push("\t}");
  }

  if (checks.length === 0) return out;
  if (required) {
    out.push(...checks);
    return out;
  }
  out.push(`\tif e.${goName} != nil {`);
  out.push(...checks.map((line) => `\t${line}`));
  out.push("\t}");
  return out;
}

function renderGoObject(objectDef) {
  const out = [];
  out.push(...goComment(`${objectDef.typeName}: ${objectDef.description}`));
  out.push(`type ${objectDef.typeName} struct {`);
  out.push(
    ...renderStructFields(
      objectDef.fields.map((field) => {
        const required = objectDef.required.includes(field.name);
        return {
          comments: field.description ? goComment(field.description, "\t") : null,
          name: goPascal(field.name),
          type: goType(field, required),
          tag: `\`json:"${field.name}${required ? "" : ",omitempty"}"\``,
        };
      }),
    ),
  );
  out.push("}");
  out.push("");
  out.push(...goComment(`Validate checks the constraints the schema places on a ${objectDef.typeName}.`));
  out.push(`func (e *${objectDef.typeName}) Validate() error {`);
  for (const field of objectDef.fields) {
    out.push(...goFieldChecks(objectDef.typeName, field, objectDef.required.includes(field.name)));
  }
  out.push("\treturn nil");
  out.push("}");
  out.push("");
  return out;
}

function renderGoCore(model) {
  const out = [];
  out.push(...goComment("EnvelopeCore holds the fields required on every envelope, whatever its kind."));
  out.push(...goComment("It is not by itself a valid record: ValidateCore checks the core constraints, and each"));
  out.push(...goComment("variant pins kind, direction and collection_mode and adds its own required fields."));
  out.push("type EnvelopeCore struct {");
  out.push(
    ...renderStructFields(
      model.coreFields.map((field) => ({
        comments: field.description ? goComment(field.description, "\t") : null,
        name: goPascal(field.name),
        type: goType(field, true),
        tag: `\`json:"${field.name}"\``,
      })),
    ),
  );
  out.push("}");
  out.push("");
  out.push(...goComment("ValidateCore checks the constraints the schema places on the common core alone."));
  out.push("func (e EnvelopeCore) ValidateCore() error {");
  for (const field of model.coreFields) {
    const pin = goZeroChecks(field);
    if (pin !== null) {
      out.push(`\tif e.${goPascal(field.name)} != ${pin} {`);
      out.push(`\t\treturn fmt.Errorf("envelope: EnvelopeCore: ${field.name} must be %s, got %q", ${pin}, e.${goPascal(field.name)})`);
      out.push("\t}");
      continue;
    }
    out.push(...goFieldChecks("EnvelopeCore", field, true));
  }
  out.push("\treturn nil");
  out.push("}");
  out.push("");
  return out;
}

/** The field lookup a pin needs: core fields first, then the variant's own fields. */
function findField(variant, model, name) {
  return model.coreFields.find((f) => f.name === name) ?? variant.ownFields.find((f) => f.name === name) ?? null;
}

function goPinChecks(variant, model) {
  const out = [];
  for (const [name, value] of Object.entries(variant.pinned)) {
    // ValidateCore already checks every field's `const`, schema_version among them.
    const field = findField(variant, model, name);
    if (!field || field.const !== undefined) continue;
    const goName = goPascal(name);
    if (field.enumType) {
      const constName = enumConstName(field.enumType, value);
      out.push(`\tif e.${goName} != ${constName} {`);
      out.push(`\t\treturn fmt.Errorf("envelope: ${variant.name}: ${name} must be %q, got %q", ${constName}, e.${goName})`);
      out.push("\t}");
    } else if (typeof value === "string") {
      out.push(`\tif e.${goName} != ${JSON.stringify(value)} {`);
      out.push(`\t\treturn fmt.Errorf("envelope: ${variant.name}: ${name} must be %q, got %q", ${JSON.stringify(value)}, e.${goName})`);
      out.push("\t}");
    } else {
      out.push(`\tif e.${goName} != ${value} {`);
      out.push(`\t\treturn fmt.Errorf("envelope: ${variant.name}: ${name} must be ${value}, got %v", e.${goName})`);
      out.push("\t}");
    }
  }
  return out;
}

function renderGoVariant(variant, model) {
  const out = [];
  const where = variant.mode ? ` at collection mode ${JSON.stringify(variant.mode)}` : "";
  const requiredOwn = variant.ownFields.filter((f) => f.required).map((f) => f.name);
  out.push(...goComment(`${variant.name} is the ${variant.shape === "stored" ? "stored" : "device"} envelope for kind ${JSON.stringify(variant.kind)}${where}.`));
  out.push("//");
  out.push(...goComment(`Required: the common core${variant.narrowedCore.length > 0 ? ` (${variant.narrowedCore.map((f) => f.name).join(", ")} pinned)` : ""}${requiredOwn.length > 0 ? `, plus ${requiredOwn.join(", ")}` : ""}.`));
  out.push(...goComment(variant.optional.length > 0 ? `Permitted but not required: ${variant.optional.join(", ")}.` : "Permitted but not required: nothing beyond the required fields."));
  out.push(...goComment(variant.forbidden.length > 0 ? `Must not carry, so absent from this struct: ${variant.forbidden.join(", ")}.` : "No field is forbidden for this kind."));
  out.push(`type ${variant.name} struct {`);
  out.push("\tEnvelopeCore");
  if (variant.ownFields.length > 0) {
    out.push("");
    out.push(
      ...renderStructFields(
        variant.ownFields.map((field) => ({
          comments: null,
          name: goPascal(field.name),
          type: goType(field, field.required),
          tag: `\`json:"${field.name}${field.required ? "" : ",omitempty"}"\``,
        })),
      ),
    );
  }
  out.push("}");
  out.push("");

  const marker = variant.shape === "stored" ? "storedEnvelope" : "deviceSubmission";
  out.push(...goComment(`${marker} marks ${variant.name} as a member of the closed ${SHAPE_UNION_NAME[variant.shape]} union.`));
  out.push(`func (*${variant.name}) ${marker}() {}`);
  out.push("");
  out.push(...goComment("Core returns the common core of the envelope."));
  out.push(`func (e *${variant.name}) Core() EnvelopeCore { return e.EnvelopeCore }`);
  out.push("");
  out.push(...goComment(`Validate checks the constraints the schema places on a ${variant.name}.`));
  out.push(`func (e *${variant.name}) Validate() error {`);
  out.push("\tif err := e.ValidateCore(); err != nil {");
  out.push(`\t\treturn fmt.Errorf("envelope: ${variant.name}: %w", err)`);
  out.push("\t}");
  out.push(...goPinChecks(variant, model));
  for (const field of variant.ownFields) {
    out.push(...goFieldChecks(variant.name, field, field.required));
  }
  out.push("\treturn nil");
  out.push("}");
  out.push("");
  return out;
}

function renderGoRules(model) {
  const out = [];
  out.push(...goComment("variantRule is the field set the schema requires and forbids for one variant. It is the"));
  out.push(...goComment("machine-readable half of the contract that Decode* enforces by name, so a rejection"));
  out.push(...goComment("says which field was missing or forbidden rather than just \"invalid\"."));
  out.push("type variantRule struct {");
  out.push(
    ...renderStructFields([
      { comments: null, name: "name", type: "string", tag: "" },
      { comments: null, name: "kind", type: "Kind", tag: "" },
      { comments: null, name: "mode", type: "CollectionMode", tag: "" },
      { comments: null, name: "required", type: "[]string", tag: "" },
      { comments: null, name: "forbidden", type: "[]string", tag: "" },
    ]).map((line) => line.trimEnd()),
  );
  out.push("}");
  out.push("");
  for (const variant of model.variants) {
    const listName = (suffix) => `${suffix}${variant.name}`;
    out.push(...goComment(`Fields the schema requires of ${variant.name}.`));
    out.push(`var ${listName("required")} = []string{`);
    out.push(...renderStringList(variant.required));
    out.push("}");
    out.push("");
    out.push(...goComment(`Fields ${variant.name} must not carry.`));
    out.push(`var ${listName("forbidden")} = []string{`);
    out.push(...renderStringList(variant.forbidden));
    out.push("}");
    out.push("");
  }
  for (const variant of model.variants) {
    const mode = variant.mode ? enumConstName("CollectionMode", variant.mode) : '""';
    out.push(`var rule${variant.name} = variantRule{`);
    out.push(
      ...alignRows([
        ["\tname:", `${JSON.stringify(variant.name)},`],
        ["\tkind:", `${enumConstName("Kind", variant.kind)},`],
        ["\tmode:", `${mode},`],
        ["\trequired:", `required${variant.name},`],
        ["\tforbidden:", `forbidden${variant.name},`],
      ]),
    );
    out.push("}");
    out.push("");
  }
  return out;
}

function renderStringList(values) {
  const out = [];
  let line = "\t";
  for (const value of values) {
    const cell = `${JSON.stringify(value)}, `;
    if (line.length + cell.length > 110) {
      out.push(line.trimEnd());
      line = "\t";
    }
    line += cell;
  }
  if (line.trim().length > 0) out.push(line.trimEnd());
  return out;
}

function renderGoUnionsAndDecoder(model) {
  const out = [];
  for (const shape of ["device", "stored"]) {
    const unionName = SHAPE_UNION_NAME[shape];
    const marker = shape === "stored" ? "storedEnvelope" : "deviceSubmission";
    const members = model.variants.filter((v) => v.shape === shape);
    const doc =
      shape === "stored"
        ? "StoredEnvelope is the closed union of the envelope as stored: one implementation per kind and, for prompts, per collection mode. A new kind is an ADR, not a new implementation."
        : "DeviceSubmission is the closed union of what a device is permitted to send. Every member omits `received_at`: a device-supplied receive time would be neither of the two clocks the contract allows.";
    out.push(...goComment(doc));
    out.push(`type ${unionName} interface {`);
    out.push(`\t${marker}()`);
    out.push("\tCore() EnvelopeCore");
    out.push("}");
    out.push("");
    out.push(...goComment(`Compile-time proof that every variant belongs to ${unionName}, and that no other type does.`));
    for (const variant of members) out.push(`var _ ${unionName} = (*${variant.name})(nil)`);
    out.push("");
  }

  out.push(...goComment("variantTarget is what decodeVariant fills: a pointer to one concrete variant."));
  out.push("type variantTarget interface {");
  out.push("\tCore() EnvelopeCore");
  out.push("\tValidate() error");
  out.push("}");
  out.push("");
  out.push(...goComment("kindProbe reads only the discriminators, so dispatch never guesses a shape."));
  out.push("type kindProbe struct {");
  out.push(
    ...alignRows([
      ["\tKind", "Kind", "`json:\"kind\"`"],
      ["\tCollectionMode", "CollectionMode", "`json:\"collection_mode\"`"],
    ]),
  );
  out.push("}");
  out.push("");
  out.push(...goComment("checkVariantFields enforces the two rules a struct cannot state in Go: a required field"));
  out.push(...goComment("must be present even when its zero value is legal, and a field the kind must not carry"));
  out.push(...goComment("must be absent. Absence is checked against the raw JSON, before decoding."));
  out.push("func checkVariantFields(data []byte, rule variantRule) error {");
  out.push("\tvar object map[string]json.RawMessage");
  out.push("\tif err := json.Unmarshal(data, &object); err != nil {");
  out.push('\t\treturn fmt.Errorf("envelope: %s: expected a JSON object: %w", rule.name, err)');
  out.push("\t}");
  out.push("\tif object == nil {");
  out.push('\t\treturn fmt.Errorf("envelope: %s: expected a JSON object, got null", rule.name)');
  out.push("\t}");
  out.push("\tvar missing []string");
  out.push("\tfor _, name := range rule.required {");
  out.push("\t\tif _, ok := object[name]; !ok {");
  out.push("\t\t\tmissing = append(missing, name)");
  out.push("\t\t}");
  out.push("\t}");
  out.push("\tif len(missing) > 0 {");
  out.push('\t\treturn fmt.Errorf("envelope: %s: missing required field(s): %s", rule.name, strings.Join(missing, ", "))');
  out.push("\t}");
  out.push("\tvar forbidden []string");
  out.push("\tfor _, name := range rule.forbidden {");
  out.push("\t\tif _, ok := object[name]; ok {");
  out.push("\t\t\tforbidden = append(forbidden, name)");
  out.push("\t\t}");
  out.push("\t}");
  out.push("\tif len(forbidden) > 0 {");
  out.push('\t\treturn fmt.Errorf("envelope: %s: field(s) not permitted for this kind and mode: %s", rule.name, strings.Join(forbidden, ", "))');
  out.push("\t}");
  out.push("\treturn nil");
  out.push("}");
  out.push("");
  out.push(...goComment("decodeVariant checks the field set, decodes with unknown fields refused so that a field"));
  out.push(...goComment("outside the contract is an error rather than silently dropped, then validates constraints."));
  out.push("func decodeVariant(data []byte, dst variantTarget, rule variantRule) error {");
  out.push("\tif err := checkVariantFields(data, rule); err != nil {");
  out.push("\t\treturn err");
  out.push("\t}");
  out.push("\tdecoder := json.NewDecoder(bytes.NewReader(data))");
  out.push("\tdecoder.DisallowUnknownFields()");
  out.push("\tif err := decoder.Decode(dst); err != nil {");
  out.push('\t\treturn fmt.Errorf("envelope: %s: %w", rule.name, err)');
  out.push("\t}");
  out.push("\treturn dst.Validate()");
  out.push("}");
  out.push("");

  for (const shape of ["device", "stored"]) {
    const unionName = SHAPE_UNION_NAME[shape];
    const fnName = `Decode${unionName}`;
    const label = shape === "stored" ? "stored envelope" : "device submission";
    out.push(...goComment(`${fnName} decodes the ${label} in data and returns the variant named by kind, and by`));
    out.push(...goComment("collection_mode within kind prompt. It refuses an unknown kind, an unknown mode, a missing"));
    out.push(...goComment("required field, a forbidden field and a field the contract does not declare."));
    out.push(`func ${fnName}(data []byte) (${unionName}, error) {`);
    out.push("\tvar probe kindProbe");
    out.push("\tif err := json.Unmarshal(data, &probe); err != nil {");
    out.push(`\t\treturn nil, fmt.Errorf("envelope: ${label} is not a JSON object: %w", err)`);
    out.push("\t}");
    out.push("\tswitch probe.Kind {");
    for (const kind of model.kinds) {
      const members = model.variants.filter((v) => v.shape === shape && v.kind === kind);
      out.push(`\tcase ${enumConstName("Kind", kind)}:`);
      const hasModes = members.some((v) => v.mode !== null);
      if (hasModes) {
        out.push("\t\tswitch probe.CollectionMode {");
        for (const variant of members.filter((v) => v.mode !== null)) {
          out.push(`\t\tcase ${enumConstName("CollectionMode", variant.mode)}:`);
          out.push(`\t\t\tvar v ${variant.name}`);
          out.push(`\t\t\tif err := decodeVariant(data, &v, rule${variant.name}); err != nil {`);
          out.push("\t\t\t\treturn nil, err");
          out.push("\t\t\t}");
          out.push("\t\t\treturn &v, nil");
        }
        out.push("\t\tdefault:");
        out.push(`\t\t\treturn nil, fmt.Errorf("envelope: collection_mode %q is outside the closed set %v for kind %q", probe.CollectionMode, AllCollectionModes(), probe.Kind)`);
        out.push("\t\t}");
      } else {
        const variant = members[0];
        out.push(`\t\tvar v ${variant.name}`);
        out.push(`\t\tif err := decodeVariant(data, &v, rule${variant.name}); err != nil {`);
        out.push("\t\t\treturn nil, err");
        out.push("\t\t}");
        out.push("\t\treturn &v, nil");
      }
    }
    out.push("\tdefault:");
    out.push(`\t\treturn nil, fmt.Errorf("envelope: kind %q is outside the closed registry %v: a new kind is an ADR, not a code change", probe.Kind, AllKinds())`);
    out.push("\t}");
    out.push("}");
    out.push("");
  }
  return out;
}

function renderGo(model) {
  const imports = ["bytes", "encoding/json", "fmt", "strings"];
  const hasPattern = [...model.coreFields, ...model.objects.flatMap((o) => o.fields)].some((f) => f.pattern);
  if (hasPattern) imports.push("regexp");
  imports.sort();

  const out = [...renderGoHeader(model), "", "package envelope", "", "import ("];
  for (const name of imports) out.push(`\t${JSON.stringify(name)}`);
  out.push(")");
  out.push("");
  out.push(...goComment(`SchemaID is the contract document these types were generated from.`));
  out.push(`const SchemaID = ${JSON.stringify(model.schemaId)}`);
  out.push("");
  out.push(...goComment(`SchemaVersion is the only schema_version this contract describes; a new value is a new contract.`));
  out.push(`const SchemaVersion = ${JSON.stringify(model.schemaVersion)}`);
  out.push("");
  out.push(...goComment("Wire-level string aliases. They are aliases, not defined types, so a value from the"));
  out.push(...goComment("wire can be used without conversion; they exist to name the format in signatures."));
  out.push("type Sha256 = string");
  out.push("");
  out.push("type UUID = string");
  out.push("");
  out.push("type DateTime = string");
  out.push("");

  if (hasPattern) {
    const seen = new Map();
    for (const field of [...model.coreFields, ...model.objects.flatMap((o) => o.fields)]) {
      if (field.pattern && !seen.has(regexVarName(field))) seen.set(regexVarName(field), field.pattern);
    }
    for (const [name, pattern] of seen) {
      out.push(`var ${name} = regexp.MustCompile(\`${pattern}\`)`);
    }
    out.push("");
  }

  for (const enumDef of model.enums) out.push(...renderGoEnum(enumDef));
  for (const objectDef of model.objects) out.push(...renderGoObject(objectDef));
  out.push(...renderGoCore(model));
  for (const variant of model.variants) out.push(...renderGoVariant(variant, model));
  out.push(...renderGoRules(model));
  out.push(...renderGoUnionsAndDecoder(model));

  return `${out.join("\n").trimEnd()}\n`;
}

// ---------------------------------------------------------------------------------------
// generation and CLI
// ---------------------------------------------------------------------------------------

export function renderAll({ contractsDir = CONTRACTS_DIR } = {}) {
  const schemaPath = path.join(contractsDir, "event-envelope.schema.json");
  const model = loadModel(schemaPath);
  return [
    { rel: TS_REL, content: renderTypeScript(model) },
    { rel: GO_REL, content: renderGo(model) },
  ];
}

function normalize(text) {
  return text.replace(/\r\n/g, "\n");
}

function firstDifference(committed, generated) {
  const a = normalize(committed).split("\n");
  const b = normalize(generated).split("\n");
  const limit = Math.max(a.length, b.length);
  for (let i = 0; i < limit; i += 1) {
    if (a[i] !== b[i]) {
      return { line: i + 1, committed: a[i] ?? "<end of file>", generated: b[i] ?? "<end of file>" };
    }
  }
  return null;
}

export function checkGenerated({ repoRoot = REPO_ROOT, contractsDir = CONTRACTS_DIR } = {}) {
  const problems = [];
  for (const { rel, content } of renderAll({ contractsDir })) {
    const absolute = path.join(repoRoot, rel);
    if (!existsSync(absolute)) {
      problems.push(`${rel}: is missing; run \`${REGENERATE}\``);
      continue;
    }
    const committed = readFileSync(absolute, "utf8");
    if (normalize(committed) === normalize(content)) continue;
    const difference = firstDifference(committed, content);
    problems.push(
      `${rel}: differs from a fresh generation of ${SCHEMA_REL_PATH} (first difference at line ${difference.line})\n` +
        `    committed: ${difference.committed}\n` +
        `    generated: ${difference.generated}`,
    );
  }
  return problems;
}

export function writeGenerated({ repoRoot = REPO_ROOT, contractsDir = CONTRACTS_DIR } = {}) {
  const written = [];
  for (const { rel, content } of renderAll({ contractsDir })) {
    const absolute = path.join(repoRoot, rel);
    mkdirSync(path.dirname(absolute), { recursive: true });
    writeFileSync(absolute, content, "utf8");
    written.push(rel);
  }
  return written;
}

function main(argv) {
  const unknown = argv.filter((arg) => arg !== "--check");
  if (unknown.length > 0) {
    process.stderr.write(`generate.mjs: unknown argument(s): ${unknown.join(", ")}\nusage: node ${GENERATED_BY} [--check]\n`);
    return 2;
  }
  if (argv.includes("--check")) {
    const problems = checkGenerated();
    if (problems.length > 0) {
      process.stderr.write(`generate.mjs --check: the committed generated files are not what ${SCHEMA_REL_PATH} produces\n`);
      for (const problem of problems) process.stderr.write(`  ${problem}\n`);
      process.stderr.write(`run: ${REGENERATE}\n`);
      return 1;
    }
    process.stdout.write(`generate.mjs --check: the generated files match ${SCHEMA_REL_PATH}\n`);
    return 0;
  }
  for (const rel of writeGenerated()) {
    process.stdout.write(`generate.mjs: wrote ${rel}\n`);
  }
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = main(process.argv.slice(2));
}
