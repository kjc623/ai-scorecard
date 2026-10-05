// index.js — the package's public surface.
//
// A reader who wants to know what this package promises should read, in this order:
//   DSL.md          the frozen grammar and the decisions the document left open
//   registry.js     the allow-list: every identifier that can reach SQL
//   validate.js     the gate: unknown keys, fields, operators and buckets are rejected
//   compile.js      the compiler: values are bound, identifiers are resolved
//   plan.js         the pipeline: validate -> guard -> compile -> audit -> execute -> suppress

export {
  QUERY_VERSION,
  API_VERSION,
  K,
  OPERATORS,
  BUCKETS,
  NATIVE_BUCKETS,
  REDUCTION_BUCKETS,
  SOURCES,
  SOURCE_IDS,
  SOURCE_WATERMARK,
  QUERY_CLASSES,
  MAX_SERIES_POINTS,
  MAX_RESPONSE_BYTES,
  MAX_LIST_WINDOW_DAYS,
  MAX_FILTERS,
  MAX_IN_VALUES,
  MAX_VALUE_LENGTH,
  MAX_DIMENSIONS,
} from './registry.js';

export {
  QueryError,
  RESULT_STATES,
  RESULT_STATE_NAMES,
  UNAVAILABLE_STATES,
  REASON,
  PROHIBITED_FIELDS,
} from './errors.js';

export { validate, canonicalJson } from './validate.js';
export { compile, effectiveOrderKeys, isNativeBucket } from './compile.js';
export { guard, bucketsForWindow } from './guard.js';
export {
  applySuppression,
  anyCellBelowK,
  suppressedCell,
  isTotalRow,
} from './suppress.js';
export {
  encodeCursor,
  decodeCursor,
  createCursorStore,
  isSubjectBearingOrder,
  paginate,
} from './cursor.js';
export {
  AUDIT_ACTIONS,
  auditDecision,
  auditDetail,
  auditPlan,
  auditStatement,
  subjectRefOf,
  verifyAuditPage,
} from './audit.js';
export {
  buildEnvelope,
  coverageBlock,
  freshnessBlock,
  resultStateFor,
  suppressionBlock,
} from './envelope.js';
export {
  REVIEW_STATES,
  FINDING_REVIEW_ACTION,
  validateReviewRequest,
  FINDING_FOR_REVIEW_SQL,
  UPSERT_REVIEW_SQL,
  findingReviewAuditStatement,
} from './review.js';
export {
  classTotalStatement,
  coverageStatement,
  erasureEvidenceStatement,
  flushCheckStatement,
  freshnessStatement,
  newerEventsStatement,
  orgCoverageStatement,
  resolveMissingRecord,
  submissionDetailStatement,
} from './blocks.js';
export { TEMPLATES, TEMPLATE_NAMES, expandTemplate } from './templates.js';
export { detectSpikes, annotateSpikes, medianOf } from './spike.js';
export { plan, executePlan, makeResume, hashOf } from './plan.js';
