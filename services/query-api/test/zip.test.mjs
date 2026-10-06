// zip.test.mjs — the archive writer, checked structurally: every record is where the format says
// it is, the sizes agree, and the output is deterministic.
import test from 'node:test';
import assert from 'node:assert/strict';

import { zipSync } from '../src/zip.js';

function readU16(buf, off) { return buf.readUInt16LE(off); }
function readU32(buf, off) { return buf.readUInt32LE(off); }

test('an archive holds each entry with the declared sizes and a central directory', () => {
  const entries = [
    { name: 'manifest.json', data: Buffer.from('{"a":1}', 'utf8') },
    { name: 'prompts/0001.txt', data: Buffer.from('hello world', 'utf8') },
  ];
  const zip = zipSync(entries);

  // The file starts with the first local header.
  assert.equal(readU32(zip, 0), 0x04034b50);

  // Walk local headers, collecting names and data offsets.
  let off = 0;
  const seen = [];
  for (let i = 0; i < entries.length; i += 1) {
    assert.equal(readU32(zip, off), 0x04034b50, `local header ${i}`);
    const nameLen = readU16(zip, off + 26);
    const extraLen = readU16(zip, off + 28);
    const compSize = readU32(zip, off + 18);
    const uncompSize = readU32(zip, off + 22);
    const crc = readU32(zip, off + 14);
    const name = zip.slice(off + 30, off + 30 + nameLen).toString('utf8');
    const data = zip.slice(off + 30 + nameLen + extraLen, off + 30 + nameLen + extraLen + compSize);
    seen.push({ name, data, crc, compSize, uncompSize });
    assert.equal(compSize, uncompSize, `${name}: store method compresses nothing`);
    off += 30 + nameLen + extraLen + compSize;
  }

  // Central directory follows the locals.
  assert.equal(readU32(zip, off), 0x02014b50, 'central directory signature');
  const names = seen.map((e) => e.name).sort();
  assert.deepEqual(names, ['manifest.json', 'prompts/0001.txt']);
  assert.ok(seen[0].data.toString('utf8') === '{"a":1}');
  assert.ok(seen[1].data.toString('utf8') === 'hello world');
});

test('an archive is deterministic for the same entries', () => {
  const entries = [{ name: 'a.txt', data: Buffer.from('one') }, { name: 'b.txt', data: Buffer.from('two') }];
  const first = zipSync(entries);
  const second = zipSync(entries);
  assert.deepEqual(first, second);
});

test('an empty archive has an end-of-central-directory record only', () => {
  const zip = zipSync([]);
  // Locals + central directory are empty; the file is just the 22-byte EOCD.
  assert.equal(zip.length, 22);
  assert.equal(readU32(zip, 0), 0x06054b50);
});
