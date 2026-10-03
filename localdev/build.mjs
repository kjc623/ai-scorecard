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
// The default build is the memory lab: ingest-api, content-vault, query-api, the control-api image
// and the edge image, all from the standard-library-only sources. `--auth` additionally compiles
// the two services with `-tags sac_sql_driver` (the real PostgreSQL driver) and packages them as
// sac/{ingest-api,control-api}:lab-auth, which localdev/authlab.compose.yaml runs. That tagged
// build needs github.com/jackc/pgx/v5 v5.11.0 in the host module cache and GOPROXY=off; see
// "The module-cache precondition" in localdev/README.md. It is opt-in so a host without the cache
// can still build and run the default lab.
//
// Usage:
//   node localdev/build.mjs                 # compile + build the default lab images
//   node localdev/build.mjs --auth          # also build the tagged SQL images for the auth lab
//   node localdev/build.mjs --skip-docker   # compile only (for a quick check)
//   node localdev/build.mjs --production    # print what a networked host would run instead

import { spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
const SKIP_DOCKER = ARGS.includes('--skip-docker');
const PRODUCTION = ARGS.includes('--production');
const AUTH = ARGS.includes('--auth');

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

// The host GOOS/GOARCH, for the one tool that runs on the host rather than in a container.
const HOST_GOOS = process.platform === 'win32' ? 'windows' : process.platform === 'darwin' ? 'darwin' : 'linux';
const HOST_GOARCH = process.arch === 'arm64' ? 'arm64' : 'amd64';
const EXE = HOST_GOOS === 'windows' ? '.exe' : '';

// The images the default lab builds.
const IMAGES = [
  { name: 'ingest-api', dir: 'ingestion/ingest-api', pkg: './cmd/ingest-api', kind: 'go' },
  { name: 'content-vault', dir: 'vault/content-vault', pkg: './cmd/content-vault', kind: 'go' },
  // query-api is Node, so there is nothing to cross-compile: the image carries the source and the
  // runtime, which works because the package has zero dependencies. Its `lab` stage is the whole
  // build, and skipping the compile step is not an omission — there is no compile step to run.
  { name: 'query-api', dir: 'query/query-api', kind: 'node' },
  // control-api joined the lab with ADR 0020: it is the enrolment and token authority the device
  // path needs, and the image exists even in the memory lab so the checker can see its vocabulary.
  { name: 'control-api', dir: 'control/control-api', pkg: './cmd/control-api', kind: 'go' },
  // edge is the Application Gateway stand-in: a standard-library-only program with its own tiny
  // image, so the lab can prove the forwarded-certificate contract without a real gateway.
  { name: 'edge', dir: 'localdev/edge', pkg: '.', kind: 'go' },
];

// Binaries that run on the host, not in a container. authlab generates the dev PKI and drives the
// end-to-end checks; it is not shipped in any image.
const TOOLS = [
  { name: 'authlab', dir: 'localdev/authlab', pkg: '.', binary: 'localdev/authlab/bin/authlab' },
];

// The tagged SQL images `--auth` adds. The binaries are compiled with the sac_sql_driver build tag,
// which is the only thing that links a PostgreSQL driver into either service. The Dockerfile `lab`
// stage takes the binary path as a build arg, so one Dockerfile serves both images.
const SQL_IMAGES = [
  {
    name: 'ingest-api',
    dir: 'ingestion/ingest-api',
    pkg: './cmd/ingest-api',
    binary: 'ingestion/ingest-api/bin/ingest-api-sql',
    image: 'sac/ingest-api:lab-auth',
    dockerfile: 'ingestion/ingest-api/Dockerfile',
  },
  {
    name: 'control-api',
    dir: 'control/control-api',
    pkg: './cmd/control-api',
    binary: 'control/control-api/bin/control-api-sql',
    image: 'sac/control-api:lab-auth',
    dockerfile: 'control/control-api/Dockerfile',
  },
];

// Images with no host-compiled binary: a plain Dockerfile built at package time. The schema image
// bakes database/schema.sql in with a build-time COPY rather than a runtime bind, so it works on
// daemons that replace a single-file bind with an empty directory (which one did).
const PLAIN_IMAGES = [
  { name: 'schema', dockerfile: 'localdev/schema/Dockerfile' },
];

function run(cmd, args, opts = {}) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', ...opts, env: { ...GO_ENV, ...(opts.env ?? {}) } });
  if (res.status !== 0) {
    console.error(`\n$ ${cmd} ${args.join(' ')}\n${res.stdout ?? ''}${res.stderr ?? ''}`);
    process.exit(res.status ?? 1);
  }
  return `${res.stdout ?? ''}${res.stderr ?? ''}`.trim();
}

function compileGo({ name, dir, pkg, out, tags, env }) {
  mkdirSync(dirname(out), { recursive: true });
  process.stdout.write(`compile ${name} … `);
  const args = ['build', '-trimpath', '-ldflags=-s -w'];
  if (tags) args.push('-tags', tags);
  args.push('-o', out, pkg);
  run('go', args, { cwd: join(ROOT, dir), env });
  console.log('ok');
}

if (PRODUCTION) {
  console.log('The production stage needs base images this host does not have cached.');
  console.log('On a host with a network or a warm cache:\n');
  for (const s of [...IMAGES, ...SQL_IMAGES]) {
    console.log(`  docker build -f ${s.dir}/Dockerfile -t sac/${s.name}:prod .`);
  }
  console.log('\nBases: golang:1.27-alpine (build), gcr.io/distroless/static-debian12:nonroot (run).');
  console.log('Then:  docker run --rm -p 8080:8080 --env SAC_ROLE=ingest-api sac/ingest-api:prod');
  process.exit(0);
}

console.log(`lab: building from ${ROOT}`);
console.log(`lab: temp and build output stay inside the workspace (${join('.testtmp')}, */bin)\n`);

// 1. Compile on the host, for the container's platform. Node services have nothing to compile.
for (const s of IMAGES) {
  if (s.kind === 'node') {
    console.log(`skip compile ${s.name} (node: the image carries the source)`);
    continue;
  }
  // No .exe: the file is a Linux binary and the name it has inside the image is this one.
  compileGo({ name: s.name, dir: s.dir, pkg: s.pkg, out: join(ROOT, s.dir, 'bin', s.name) });
}
for (const t of TOOLS) {
  compileGo({
    name: t.name,
    dir: t.dir,
    pkg: t.pkg,
    out: join(ROOT, t.binary) + EXE,
    env: { GOOS: HOST_GOOS, GOARCH: HOST_GOARCH },
  });
}

// 2. The tagged SQL binaries, only when asked, so a host without the module cache can still build.
if (AUTH) {
  console.log('');
  for (const s of SQL_IMAGES) {
    compileGo({ name: `${s.name} (sac_sql_driver)`, dir: s.dir, pkg: s.pkg, out: join(ROOT, s.binary), tags: 'sac_sql_driver' });
  }
}

console.log('');
if (SKIP_DOCKER) {
  console.log('--skip-docker: compiled only.');
  process.exit(0);
}

// 3. Package. The build context is the repository root; the Dockerfiles say why.
for (const s of IMAGES) {
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
for (const s of PLAIN_IMAGES) {
  process.stdout.write(`docker build sac/${s.name}:lab … `);
  const out = run('docker', [
    'build',
    '-f', join(s.dockerfile),
    '-t', `sac/${s.name}:lab`,
    '.',
  ], { cwd: ROOT });
  console.log(out.split('\n').filter((l) => /DONE|naming to|ERROR/.test(l)).slice(-1)[0]?.trim() ?? 'ok');
}
for (const s of SQL_IMAGES) {
  if (!AUTH) continue;
  process.stdout.write(`docker build ${s.image} … `);
  const out = run('docker', [
    'build',
    '--target', 'lab',
    '--build-arg', `BINARY=${s.binary}`,
    '-f', join(s.dockerfile),
    '-t', s.image,
    '.',
  ], { cwd: ROOT });
  console.log(out.split('\n').filter((l) => /DONE|naming to|ERROR/.test(l)).slice(-1)[0]?.trim() ?? 'ok');
}

console.log('\nImages:');
console.log(run('docker', ['images', '--format', '{{.Repository}}:{{.Tag}} {{.Size}}', 'sac/*']));
console.log('\nNext: node localdev/run.mjs        (default memory lab)');
console.log('      node localdev/run.mjs --auth (opt-in SQL auth lab; needs the --auth build)');
