/**
 * test/attachment-flow.test.mjs — a file the user attached in the page reaches capture-core with
 * the submission: the worker asks the sending tab's content script for its files, the content
 * script transfers each one (manifest, chunks, completion) through the worker to capture-core, and
 * only then is the observation sent, naming each file with its size and digest.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cpSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { createAttachmentCollector } from '../src/attachments/collect.js';
import { TYPE } from '../src/messages.js';
import { chromeRequest } from '../test-support/fake-chrome.mjs';
import { CHAT_BODY } from '../test-support/fixtures.mjs';
import { attachTab, createHarness, settle, waitFor } from '../test-support/harness.mjs';

const CHAT_URL = 'https://chat.example-ai.invalid/v1/chat/completions';
const TAB = 7;
const FILE = {
  name: 'q3-forecast.csv',
  media_type: 'text/csv',
  bytes: new TextEncoder().encode('region,revenue\nnorth,12\nsouth,7\n'.repeat(40)),
};
const sha256 = (bytes) => `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
const observations = (h) => h.core.received.filter((m) => m.type === TYPE.OBSERVATION).map((m) => m.body);

async function started(bundle, core = { chunkBytes: 256 }) {
  const h = createHarness({ core });
  await h.app.start();
  h.app.applyPolicy({ policy_version: bundle.policy_version, bundle });
  await settle();
  h.core.received.length = 0;
  return h;
}

function submit(h, requestId = 'r1') {
  return h.fake.drive(
    'body',
    chromeRequest({ requestId, url: CHAT_URL, tabId: TAB, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }),
  );
}

test('an attached file is transferred to capture-core before the observation that names it', async () => {
  const h = await started({ policy_version: 'b1', default_mode: 'm2' });
  const tab = attachTab(h, TAB, [FILE]);
  await submit(h);
  await waitFor(() => observations(h).length === 1, { label: 'the observation' });

  const types = h.core.received.map((m) => m.type);
  const observationAt = types.indexOf(TYPE.OBSERVATION);
  assert.equal(types[0], TYPE.ATTACHMENT_MANIFEST, 'the manifest goes first');
  assert.equal(types[observationAt - 1], TYPE.ATTACHMENT_COMPLETE, 'the transfer completes before the observation is sent');
  assert.ok(types.filter((t) => t === TYPE.ATTACHMENT_CHUNK).length > 1, 'the file moves in chunks');

  const observation = h.core.received[observationAt].body;
  const manifest = h.core.received[0].body;
  assert.equal(manifest.observation_id, observation.client_id, 'the transfer names the observation it belongs to');
  assert.deepEqual(manifest.descriptor, { name: FILE.name, size_bytes: FILE.bytes.byteLength, media_type: FILE.media_type });

  const moved = Buffer.concat(h.core.transfers.get(manifest.transfer_id).chunks.map((c) => Buffer.from(c, 'base64')));
  assert.deepEqual(new Uint8Array(moved), FILE.bytes, 'the bytes capture-core received are the file');
  assert.deepEqual(observation.attachments, [
    { name: FILE.name, size_bytes: FILE.bytes.byteLength, media_type: FILE.media_type, content_digest: sha256(FILE.bytes) },
  ]);
  assert.equal(observation.page_context_attachments, true);
  assert.deepEqual(tab.asked, ['capture_upload_check', 'capture_upload_send']);
});

test('a file attaches to one submission: the next one does not send it again', async () => {
  const h = await started({ policy_version: 'b1', default_mode: 'm2' });
  attachTab(h, TAB, [FILE]);
  await submit(h, 'r1');
  await waitFor(() => observations(h).length === 1, { label: 'the first observation' });
  await submit(h, 'r2');
  await waitFor(() => observations(h).length === 2, { label: 'the second observation' });

  assert.equal(h.core.received.filter((m) => m.type === TYPE.ATTACHMENT_MANIFEST).length, 1);
  assert.equal(observations(h)[1].attachments, undefined);
  assert.equal(observations(h)[1].page_context_attachments, undefined);
});

test('at M0 the tab is not asked for files and no byte moves', async () => {
  const h = await started({ policy_version: 'b1', default_mode: 'm0' });
  const tab = attachTab(h, TAB, [FILE]);
  await h.fake.drive('metadata', chromeRequest({ url: CHAT_URL, tabId: TAB, headers: { 'content-type': 'application/json' } }));
  await settle();
  assert.equal(observations(h).length, 1, 'the submission is still observed, for identity and volume');
  assert.deepEqual(tab.asked, []);
  assert.equal(h.core.received.filter((m) => m.type.startsWith('attachment_')).length, 0);
});

test('a refused transfer still names the file, without a digest, and the observation is sent', async () => {
  const h = await started({ policy_version: 'b1', default_mode: 'm2' }, { attachmentCapacityBytes: 16 });
  attachTab(h, TAB, [FILE]);
  await submit(h);
  await waitFor(() => observations(h).length === 1, { label: 'the observation' });
  assert.deepEqual(observations(h)[0].attachments, [{ name: FILE.name, media_type: FILE.media_type, size_bytes: FILE.bytes.byteLength }]);
  assert.equal(h.core.received.filter((m) => m.type === TYPE.ATTACHMENT_CHUNK).length, 0, 'no byte moves after a refused manifest');
  assert.equal(h.app.health.report().errors_by_code.core_refused, 1);
});

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..');
const GO_PRESENT = spawnSync('go', ['version'], { encoding: 'utf8' }).status === 0;

test('the transfer decodes with the real Go protocol types and matches the observation', { skip: GO_PRESENT ? false : 'no Go toolchain on PATH' }, async () => {
  const h = await started({ policy_version: 'b1', default_mode: 'm2' });
  attachTab(h, TAB, [FILE]);
  await submit(h);
  await waitFor(() => observations(h).length === 1, { label: 'the observation' });

  const tmp = mkdtempSync(join(tmpdir(), 'sac-attachment-frames-'));
  try {
    cpSync(join(REPO_ROOT, 'endpoint', 'protocol'), join(tmp, 'protocol'), { recursive: true });
    writeFileSync(join(tmp, 'frames.json'), JSON.stringify(h.core.received));
    writeFileSync(
      join(tmp, 'go.mod'),
      'module frames\n\ngo 1.22\n\nrequire github.com/shadow-ai-capture/device/protocol v0.0.0\n\nreplace github.com/shadow-ai-capture/device/protocol => ./protocol\n',
    );
    // Decodes each frame as capture-core does, reassembles the chunks, and checks the observation's
    // descriptor against the bytes that moved.
    writeFileSync(
      join(tmp, 'main.go'),
      `package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/shadow-ai-capture/device/protocol"
)

func main() {
	raw, _ := os.ReadFile("frames.json")
	var frames []protocol.NativeMessage
	if err := json.Unmarshal(raw, &frames); err != nil {
		fmt.Println("FAIL frames:", err)
		return
	}
	var manifest protocol.AttachmentManifest
	var bytes []byte
	var obs protocol.ObservationMessage
	for _, f := range frames {
		var err error
		switch f.Type {
		case protocol.TypeAttachmentManifest:
			err = json.Unmarshal(f.Body, &manifest)
		case protocol.TypeAttachmentChunk:
			var c protocol.AttachmentChunk
			err = json.Unmarshal(f.Body, &c)
			bytes = append(bytes, c.Data...)
		case protocol.TypeAttachmentComplete:
			var c protocol.AttachmentComplete
			err = json.Unmarshal(f.Body, &c)
		case protocol.TypeObservation:
			if err = json.Unmarshal(f.Body, &obs); err == nil {
				err = obs.Validate()
			}
		}
		if err != nil {
			fmt.Println("FAIL", f.Type, err)
			return
		}
	}
	sum := sha256.Sum256(bytes)
	a := obs.Attachments[0]
	fmt.Println(manifest.Observation == obs.ClientID, int64(len(bytes)) == a.SizeBytes, "sha256:"+hex.EncodeToString(sum[:]) == a.ContentDigest, obs.PageContextAttachments)
}
`,
    );
    const env = { ...process.env, GOFLAGS: '-mod=mod', GOWORK: 'off' };
    const run = spawnSync('go', ['run', '.'], { cwd: tmp, encoding: 'utf8', env });
    assert.equal(run.status, 0, run.stderr);
    assert.equal(run.stdout.trim(), 'true true true true', 'manifest names the observation; size and digest describe the bytes that moved');
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
});

test('the collector bounds every step and never throws', async () => {
  const counted = [];
  const counters = { countError: (code) => counted.push(code) };

  const silent = createAttachmentCollector({ adapter: { tabs: { sendMessage: () => new Promise(() => {}) } }, counters, checkTimeoutMs: 5 });
  assert.deepEqual(await silent.collect({ tabId: 3, observationId: 'c1' }), { attachments: [], reachable: false }, 'a tab that never answers holds nothing up');

  const noTab = createAttachmentCollector({ adapter: { tabs: { sendMessage: () => assert.fail('no tab, no message') } }, counters });
  assert.deepEqual(await noTab.collect({ tabId: -1, observationId: 'c1' }), { attachments: [], reachable: false });

  const candidate = { ref_id: 'f1', name: 'big.bin', media_type: '', size_bytes: 128 << 20 };
  const stalls = {
    tabs: {
      sendMessage: (_tab, msg) => (msg.type === 'capture_upload_check' ? Promise.resolve({ ok: true, candidates: [candidate] }) : new Promise(() => {})),
    },
  };
  const slow = createAttachmentCollector({ adapter: stalls, counters, transferTimeoutMs: 5 });
  assert.deepEqual(
    await slow.collect({ tabId: 3, observationId: 'c1' }),
    { attachments: [{ name: 'big.bin' }], reachable: true },
    'a transfer that does not finish keeps the name; a size over the transport cap is left out',
  );
  assert.deepEqual(counted, ['native_timeout']);
});
