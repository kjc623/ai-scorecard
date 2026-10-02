#!/usr/bin/env node
// The JavaScript half of the sac-canon-1 equivalence check.
//
// Reads a corpus file (testdata/corpus.json) and writes, as JSON on stdout, Node's own NFC
// of every case. Go calls this at test time so the check is a live comparison between the two
// implementations §4 requires to agree, not a comparison against a file that could have gone
// stale.
//
// Usage: node gen/nfc_oracle.mjs testdata/corpus.json

import { readFileSync } from 'node:fs';

const path = process.argv[2];
if (!path) {
  console.error('usage: node gen/nfc_oracle.mjs <corpus.json>');
  process.exit(2);
}

const corpus = JSON.parse(readFileSync(path, 'utf8'));
if (!Array.isArray(corpus.cases)) {
  console.error('corpus has no cases array');
  process.exit(2);
}

const expected = corpus.cases.map((c) => c.in.normalize('NFC'));
process.stdout.write(JSON.stringify({
  contract: corpus.contract,
  node: process.versions.node,
  icu: process.versions.icu,
  unicode: process.versions.unicode,
  expected,
}));
