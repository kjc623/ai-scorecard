#!/usr/bin/env node
// Build the lab images.
//
// Why this exists rather than a plain `docker build .`: this host has no network, and the only
// cached base images are alpine, postgres, azurite and minio. There is no golang image, so the
// multi-stage Dockerfile's `build` stage cannot run here — but the Go toolchain is on the host, and
// the `lab` stage of each Dockerfile packages a host-compiled binary into a minimal image with no
// toolchain image involved. That is the whole trick, and it is why the lab is runnable offline at
// all.
//
// What this does NOT do: build the `production` stage (distroless), which needs a networked host
// or two cached base images. `node localdev/build.mjs --production` names the exact commands instead of
// pretending.
//
// Usage:
//   node localdev/build.mjs                 # compile + build the lab images
//   node localdev/build.mjs --skip-docker   # compile only (for a quick check)
//   node localdev/build.mjs --production    # print what a networked host would run instead

import { spawnSync } from 'node:child_process';
import { mkdirSync, existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
const SKIP_DOCKER = ARGS.includes('--skip-docker');
const PRODUCTION = ARGS.includes('--production');

// Keep every scratch path inside the workspace. The file sandbox denies writes to the platform temp
// directory, and a tool that writes there fails with "Access is denied" for a reason that has
// nothing to do with the code.
const TMP = join(ROOT, '.testtmp');
mkdirSync(TMP, { recursive: true });

const GO_ENV = {
  ...process.env,
  GOCACHE: join(ROOT, '.tools', 'gocache'),
  GOPROXY: 'off',
  GOFLAGS: '-mod=mod',
  GOTOOLCHAIN: 'local',
  // The host is Windows and the container is Linux, so the binary is cross-compiled. CGO_ENABLED=0
  // is what makes that free: with no cgo there is no cross toolchain to install, and the resulting
  // static binary runs in alpine and in distroless/static alike.
  GOOS: process.env.LAB_GOOS ?? 'linux',
  GOARCH: process.env.LAB_GOARCH ?? 'amd64',
  CGO_ENABLED: '0',
  TMP,
  TEMP: TMP,
  TMPDIR: TMP,
};

const SERVICES = [
  { name: 'ingest-api', dir: 'ingestion/ingest-api', pkg: './cmd/ingest-api', kind: 'go' },
  { name: 'content-vault', dir: 'vault/content-vault', pkg: './cmd/content-vault', kind: 'go' },
  // query-api is Node, so there is nothing to cross-compile: the image carries the source and the
  // runtime, which works because the package has zero dependencies. Its `lab` stage is the whole
  // build, and skipping the compile step is not an omission — there is no compile step to run.
  { name: 'query-api', dir: 'query/query-api', kind: 'node' },
];

function run(cmd, args, opts = {}) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', env: GO_ENV, ...opts });
  if (res.status !== 0) {
    console.error(`\n$ ${cmd} ${args.join(' ')}\n${res.stdout ?? ''}${res.stderr ?? ''}`);
    process.exit(res.status ?? 1);
  }
  return `${res.stdout ?? ''}${res.stderr ?? ''}`.trim();
}

if (PRODUCTION) {
  console.log('The production stage needs base images this host does not have cached.');
  console.log('On a host with a network or a warm cache:\n');
  for (const s of SERVICES) {
    console.log(`  docker build -f ${s.dir}/Dockerfile -t sac/${s.name}:prod .`);
  }
  console.log('\nBases: golang:1.27-alpine (build), gcr.io/distroless/static-debian12:nonroot (run).');
  console.log('Then:  docker run --rm -p 8080:8080 --env SAC_ROLE=ingest-api sac/ingest-api:prod');
  process.exit(0);
}

console.log(`lab: building from ${ROOT}`);
console.log(`lab: temp and build output stay inside the workspace (${join('.testtmp')}, */bin)\n`);

// 1. Compile on the host, for the container's platform. Node services have nothing to compile.
for (const s of SERVICES) {
  if (s.kind === 'node') {
    console.log(`skip compile ${s.name} (node: the image carries the source)`);
    continue;
  }
  const outDir = join(ROOT, s.dir, 'bin');
  mkdirSync(outDir, { recursive: true });
  // No .exe: the file is a Linux binary and the name it has inside the image is this one.
  const out = join(outDir, s.name);
  process.stdout.write(`compile ${s.name} (${GO_ENV.GOOS}/${GO_ENV.GOARCH}) … `);
  run('go', ['build', '-trimpath', '-ldflags=-s -w', '-o', out, s.pkg], { cwd: join(ROOT, s.dir) });
  console.log('ok');
}
console.log('');

if (SKIP_DOCKER) {
  console.log('--skip-docker: compiled only.');
  process.exit(0);
}

// 2. Package. The build context is the repository root; the Dockerfiles say why.
for (const s of SERVICES) {
  process.stdout.write(`docker build sac/${s.name}:lab … `);
  const out = run('docker', [
    'build',
    '--target', 'lab',
    '-f', join(s.dir, 'Dockerfile'),
    '-t', `sac/${s.name}:lab`,
    '.',
  ], { cwd: ROOT });
  console.log(out.split('\n').filter((l) => /DONE|naming to|ERROR/.test(l)).slice(-1)[0]?.trim() ?? 'ok');
}

console.log('\nImages:');
console.log(run('docker', ['images', '--format', '{{.Repository}}:{{.Tag}} {{.Size}}', 'sac/*']));
console.log('\nNext: node localdev/run.mjs   (or: docker compose -f localdev/docker-compose.yml up -d)');
