// zip.js — a minimal ZIP writer for the subject-export archive.
//
// The archive is a handful of small, already-textual files, so entries are stored uncompressed
// (method 0). The format is small enough to keep self-contained: a local header and data per entry,
// a central directory, and the end-of-central-directory record. CRC-32 is the ZIP standard's.
//
// It is a writer only: nothing here reads an archive back, and the entries it emits are exactly the
// bytes the caller handed over, so there is no deserialisation to get wrong.

/** CRC-32 (IEEE 802.3, reflected), as ZIP requires. */
const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n += 1) {
    let c = n;
    for (let k = 0; k < 8; k += 1) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c >>> 0;
  }
  return table;
})();

function crc32(bytes) {
  let c = 0xffffffff;
  for (let i = 0; i < bytes.length; i += 1) c = CRC_TABLE[(c ^ bytes[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

// A fixed DOS date (1980-01-01) keeps an archive's bytes independent of the clock, so a test can
// assert byte-for-byte output. An entry's timestamp is not information the archive carries.
const DOS_TIME = 0;
const DOS_DATE = 0x21; // 1980-01-01

/**
 * Build a ZIP archive from entries. Each entry's `name` is a `/`-separated path inside the archive
 * and `data` is its exact contents.
 *
 * @param {ReadonlyArray<{name: string, data: Buffer}>} entries
 * @returns {Buffer}
 */
export function zipSync(entries) {
  const locals = [];
  const central = [];
  let offset = 0;

  for (const entry of entries) {
    const name = Buffer.from(entry.name, 'utf8');
    const data = Buffer.from(entry.data);
    const crc = crc32(data);

    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4); // version needed to extract
    local.writeUInt16LE(0, 6); // general-purpose flags
    local.writeUInt16LE(0, 8); // compression method: store
    local.writeUInt16LE(DOS_TIME, 10);
    local.writeUInt16LE(DOS_DATE, 12);
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(data.length, 18); // compressed size
    local.writeUInt32LE(data.length, 22); // uncompressed size
    local.writeUInt16LE(name.length, 26);
    local.writeUInt16LE(0, 28); // extra field length

    locals.push(local, name, data);
    central.push({ name, crc, size: data.length, offset });
    offset += local.length + name.length + data.length;
  }

  const directory = [];
  for (const entry of central) {
    const record = Buffer.alloc(46);
    record.writeUInt32LE(0x02014b50, 0);
    record.writeUInt16LE(20, 4); // version made by
    record.writeUInt16LE(20, 6); // version needed
    record.writeUInt16LE(0, 8);
    record.writeUInt16LE(0, 10); // method: store
    record.writeUInt16LE(DOS_TIME, 12);
    record.writeUInt16LE(DOS_DATE, 14);
    record.writeUInt32LE(entry.crc, 16);
    record.writeUInt32LE(entry.size, 20);
    record.writeUInt32LE(entry.size, 24);
    record.writeUInt16LE(entry.name.length, 28);
    record.writeUInt16LE(0, 30); // extra
    record.writeUInt16LE(0, 32); // comment
    record.writeUInt16LE(0, 34); // disk number start
    record.writeUInt16LE(0, 36); // internal attributes
    record.writeUInt32LE(0, 38); // external attributes
    record.writeUInt32LE(entry.offset, 42);
    directory.push(record, entry.name);
  }
  const directorySize = directory.reduce((sum, b) => sum + b.length, 0);

  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(0, 4); // disk
  end.writeUInt16LE(0, 6); // directory disk
  end.writeUInt16LE(central.length, 8); // entries on this disk
  end.writeUInt16LE(central.length, 10); // total entries
  end.writeUInt32LE(directorySize, 12);
  end.writeUInt32LE(offset, 16);
  end.writeUInt16LE(0, 20); // comment length

  return Buffer.concat([...locals, ...directory, end]);
}
