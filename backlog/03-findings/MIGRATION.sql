-- In-place migration: findings read the current rule definition.
-- Applies the 03-findings schema change to an existing database without touching ingest data.
-- mart is derived and rebuildable and ref.rule is empty, so this is additive and reversible.
BEGIN;

-- 1. mart.finding keeps the match, not the rule's attributes. The view depends on the two columns,
--    so it is dropped first and recreated below with its grant.
DROP VIEW IF EXISTS mart.v_finding;
ALTER TABLE mart.finding DROP COLUMN IF EXISTS class_code;
ALTER TABLE mart.finding DROP COLUMN IF EXISTS severity;

COMMENT ON TABLE mart.finding IS
  'Derived. Review state is NOT here -- it is in ops.finding_review, keyed by the same natural key, so that DROP and rebuild of this schema cannot destroy an analyst''s judgement. class_code, severity and title come from the current ref.rule row at read time, not from a snapshot.';

-- 2. The view reads class_code/severity/title from the current ref.rule. It was dropped above and is
--    recreated here with its grant.
CREATE VIEW mart.v_finding
WITH (security_invoker = true) AS
SELECT f.tenant_id,
       f.submission_id,
       f.rule_id,
       r.title        AS rule_title,
       r.class_code,
       r.severity,
       f.detected_at,
       f.decided_locally,
       f.collection_mode,
       s.user_ref,
       s.tool_fingerprint,
       s.policy_action,
       coalesce(fr.review_state, 'open') AS review_state,
       fr.reviewed_by,
       fr.reviewed_at
  FROM mart.finding f
  JOIN ref.rule r ON r.rule_id = f.rule_id
  JOIN ingest.submission s
    ON s.tenant_id = f.tenant_id AND s.submission_id = f.submission_id
  LEFT JOIN ops.finding_review fr
    ON fr.tenant_id = f.tenant_id
   AND fr.submission_id = f.submission_id
   AND fr.rule_id = f.rule_id;

COMMENT ON VIEW mart.v_finding IS
  'Brief §3.6 question 5: filtered list with severity and review state. class_code, severity and title are present-tense (ref.rule), so a rule edit shows on every finding that names it; the review coalesce to open is a rendering of "nobody has reviewed this", not an assertion that it was reviewed and found unremarkable.';

GRANT SELECT ON mart.v_finding TO sac_query;

-- 3. The rule catalogue the classifier publishes. Idempotent so a re-applied migration is a no-op.
INSERT INTO ref.rule (rule_id, class_code, detector_kind, severity, title, description, introduced_in) VALUES
  ('PCI_PAN_PATTERN',       'payment_card',     'deterministic', 'critical', 'Payment card number in prompt',   'A card-number-shaped run of digits whose Luhn checksum holds and which is not preceded by "test".',       '2026.01.0-shadow'),
  ('PAYMENT_CARD_PAN',      'payment_card',     'deterministic', 'critical', 'Payment card number in prompt',   'Card-number pattern validated by Luhn, as published by the endpoint classifier. Alias of the rule above under its wire id.', '2026.01.0-shadow'),
  ('SECRET_API_KEY',        'credential',       'deterministic', 'critical', 'API key or access token',         'A credential-shaped string: private-key headers and provider key prefixes.',                             '2026.01.0-shadow'),
  ('GOV_ID_NUMBER',         'government_id',    'deterministic', 'high',     'Government identifier',           'A national identifier or tax number matching a jurisdiction rule set.',                                   '2026.01.0-shadow'),
  ('PII_CUSTOMER_RECORD',   'customer_pii',     'deterministic', 'high',     'Customer personal data',          'Names, addresses or contact details identifying a customer.',                                             '2026.01.0-shadow'),
  ('SRC_INTERNAL_REPO',     'source_code',      'deterministic', 'high',     'Proprietary source code',         'Source declarations or repository detail indicating proprietary implementation.',                          '2026.01.0-shadow'),
  ('PHI_CLINICAL_TERM',     'health',           'deterministic', 'high',     'Health information',              'Clinical vocabulary and anything suggesting a medical condition.',                                        '2026.01.0-shadow'),
  ('LEGAL_CONTRACT_TERMS',  'legal_commercial', 'deterministic', 'medium',   'Contract or commercial terms',    'Contractual or commercially sensitive language.',                                                         '2026.01.0-shadow')
ON CONFLICT (rule_id) DO NOTHING;

COMMIT;
