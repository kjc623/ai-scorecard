#!/usr/bin/env node
// Builds every image the lab runs: the production images from their own Dockerfiles, and the lab's
// edge, identity provider, smoke tool and job scheduler. Each is tagged sac/<name>:lab.
//
//   node localdev/build.mjs              all of them
//   node localdev/build.mjs control-api  only the named ones

import { spawnSync } from 'node:child_process';

import { IMAGES, ROOT, buildArgs, imageTag } from './lab.mjs';

const names = process.argv.slice(2);
const unknown = names.filter((n) => !IMAGES.some((i) => i.name === n));
if (unknown.length) {
  console.error(`build: unknown image(s) ${unknown.join(', ')}; the lab's images are ${IMAGES.map((i) => i.name).join(', ')}`);
  process.exit(2);
}

for (const image of IMAGES.filter((i) => names.length === 0 || names.includes(i.name))) {
  console.log(`build: ${imageTag(image.name)} from ${image.dockerfile}`);
  const r = spawnSync('docker', buildArgs(image), { cwd: ROOT, stdio: 'inherit' });
  if (r.status !== 0) {
    console.error(`build: ${imageTag(image.name)} failed`);
    process.exit(1);
  }
}
console.log('build: done. Next: node localdev/run.mjs');
