// subject.test.mjs — the subject-export archive and the erasure request, without a database.
import test from 'node:test';
import assert from 'node:assert/strict';

import { validateSubjectRequest, buildSubjectArchive, SUBJECT_SUBMISSIONS_SQL, SUBJECT_OBSERVATIONS_SQL } from '../src/subject.js';
import { rejection } from './helpers.mjs';

test('a subject request body is validated; unknown keys and a tenant are refused', () => {
  assert.deepEqual(validateSubjectRequest({ subject_ref: 'u_4f21' }), { subjectRef: 'u_4f21' });
  assert.deepEqual(validateSubjectRequest({ subject_ref: '  u_4f21  ' }), { subjectRef: 'u_4f21' });

  const tenantErr = rejection(() => validateSubjectRequest({ subject_ref: 'x', tenant_id: 'y' }));
  assert.equal(tenantErr.resultState, 'unsupported_query_shape');
  assert.equal(tenantErr.reason, 'tenant_in_request');

  const keyErr = rejection(() => validateSubjectRequest({ subject_ref: 'x', other: 1 }));
  assert.equal(keyErr.reason, 'unknown_key');

  const emptyErr = rejection(() => validateSubjectRequest({ subject_ref: '' }));
  assert.equal(emptyErr.reason, 'type_mismatch');
});

test('the archive packs a manifest, three CSVs and one file per prompt', () => {
  const archive = buildSubjectArchive({
    subjectRef: 'u_4f21',
    canonicalRef: 'u_4f21',
    displayName: 'Ada Lovelace',
    generatedAt: '2026-10-05T00:00:00Z',
    submissions: [{ submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', received_at: new Date('2026-10-01T00:00:00Z'), subject: 'u_4f21' }],
    observations: [{ event_id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb' }],
    findings: [{ submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', rule: 'PAYMENT_CARD_PAN' }],
    prompts: [{ event_id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', plaintext: Buffer.from('the prompt').toString('base64') }],
  });

  // A ZIP local-header signature leads, and the entries are named inside.
  assert.equal(archive.readUInt32LE(0), 0x04034b50);
  const text = archive.toString('latin1');
  for (const name of ['manifest.json', 'events.csv', 'observations.csv', 'findings.csv', 'prompts/0001.txt']) {
    assert.ok(text.includes(name), `archive names ${name}`);
  }
  // The prompt header names its event and the plaintext bytes travel after it.
  assert.ok(text.includes('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'));
  assert.ok(text.includes('the prompt'));
});

test("the archive names each event's and observation's origin: its device, or the provider that recorded it", () => {
  const archive = buildSubjectArchive({
    subjectRef: 'u_4f21',
    canonicalRef: 'u_4f21',
    displayName: null,
    generatedAt: '2026-10-05T00:00:00Z',
    submissions: [{ submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', device: null, provider: 'Claude Enterprise' }],
    observations: [{ event_id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', device: null, provider: 'Claude Enterprise' }],
    findings: [],
    prompts: [],
  });
  const text = archive.toString('latin1');
  assert.ok(text.includes(',tool_name,device,provider,mode,'), 'events.csv has a provider column beside the device');
  assert.ok(text.includes('event_id,device,provider,subject_name,'), 'observations.csv has a provider column beside the device');
  assert.ok(text.includes('aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa,,,,,,,,Claude Enterprise,'), 'an event with no device names its provider');
  assert.ok(text.includes('bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb,,Claude Enterprise,'), 'an observation with no device names its provider');

  // The provider is reached through the row's connection of the same tenant, by its display name.
  assert.match(SUBJECT_SUBMISSIONS_SQL, /LEFT JOIN ops\.provider_connection pc ON pc\.tenant_id = s\.tenant_id AND pc\.connection_id = s\.connection_id/);
  assert.match(SUBJECT_OBSERVATIONS_SQL, /LEFT JOIN ops\.provider_connection pc ON pc\.tenant_id = o\.tenant_id AND pc\.connection_id = o\.connection_id/);
  for (const sql of [SUBJECT_SUBMISSIONS_SQL, SUBJECT_OBSERVATIONS_SQL]) {
    assert.match(sql, /LEFT JOIN ref\.provider p ON p\.provider = pc\.provider/);
    assert.match(sql, /p\.display_name AS provider/);
  }
});
