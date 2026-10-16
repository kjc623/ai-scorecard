// Records the demo video from the demo console and cuts it into one MP4.
//
//   node record.mjs                 every scene, then ../../<product>-demo.mp4 and the cut-downs
//   node record.mjs 3 4             only those scenes (kept in the work folder), no final cut
//   CAPTIONS=0 node record.mjs      without burned-in captions (the full video only)
//
// The demo console (../demo/server.mjs) must be running; DEMO_URL names it. Scenes and the
// cut-downs are in scenes.mjs. Each scene is captured with Chrome's screencast at 1920×1080, padded
// or trimmed to its length, faded in and out, and the scenes are joined in order. A cut-down joins
// some of the same scenes, with the end card shortened. Needs ffmpeg with libx264, and this
// folder's packages (npm ci) with Playwright's Chromium.

import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { chromium } from 'playwright';

import { CUTS, END_IN_CUTS, SCENES } from './scenes.mjs';

const here = dirname(fileURLToPath(import.meta.url));

const brand = JSON.parse(readFileSync(join(here, '..', 'brand.json'), 'utf8'));
const slug = brand.product.toLowerCase().replace(/[^a-z0-9]+/g, '-');
const DEMO_URL = process.env.DEMO_URL ?? 'http://127.0.0.1:8790/';
const CAPTIONS = process.env.CAPTIONS !== '0';
const WORK = process.env.WORK ?? join(tmpdir(), `${slug}-video`);
const suffix = CAPTIONS ? '' : '-no-captions';
const OUT = join(here, '..', '..', `${slug}-demo${suffix}.mp4`);
// Every scene's browser window, scaled to 1920×1080. It is wide enough for the console's widest
// tables (Search, the audit trail) to fit without clipping.
const VIEW = { width: 1760, height: 990 };
const SIZE = { width: 1920, height: 1080 };
const FPS = 30;
const FADE = 0.3;
const PAPER = '0xf6f5f1';

const overlay = readFileSync(join(here, 'overlay.js'), 'utf8');

/** Helpers a scene drives the page with. Times are in milliseconds. */
function director(page, view) {
  let at = { x: view.width * 0.62, y: view.height * 0.55 };
  let shown = '';
  const wait = (ms) => page.waitForTimeout(ms);
  const box = async (target) => {
    const loc = typeof target === 'string' ? page.locator(target).first() : target;
    await loc.scrollIntoViewIfNeeded();
    const b = await loc.boundingBox();
    if (!b) throw new Error(`no box for ${target}`);
    return b;
  };
  const d = {
    page,
    wait,
    brand,
    caption(text) {
      shown = text;
      return page.evaluate(([t, on]) => window.__demo?.caption(on ? t : ''), [text, CAPTIONS]);
    },
    async goto(path) {
      await page.goto(new URL(path, DEMO_URL).href, { waitUntil: 'networkidle' });
      await page.evaluate(() => document.fonts.ready);
      await page.evaluate(([x, y]) => window.__demo?.cursor(x, y), [at.x, at.y]);
      if (shown) await d.caption(shown);
    },
    async move(target, ms = 700) {
      const b = typeof target === 'object' && 'x' in target ? { ...target, width: 0, height: 0 } : await box(target);
      const to = { x: b.x + b.width / 2, y: b.y + b.height / 2 };
      const steps = Math.max(2, Math.round(ms / 16));
      for (let i = 1; i <= steps; i += 1) {
        const k = i / steps;
        const e = k < 0.5 ? 4 * k * k * k : 1 - (-2 * k + 2) ** 3 / 2;
        const x = at.x + (to.x - at.x) * e; const y = at.y + (to.y - at.y) * e;
        await page.mouse.move(x, y);
        // Over an iframe the top page gets no mousemove, so the pointer is placed directly too.
        await page.evaluate(([px, py]) => window.__demo?.cursor(px, py), [x, y]);
        await wait(10);
      }
      at = to;
    },
    async click(target, ms = 700) {
      await d.move(target, ms);
      await wait(150);
      await page.mouse.down();
      await wait(90);
      await page.mouse.up();
      await page.waitForLoadState('networkidle');
    },
    async type(target, text, perChar = 70) {
      await d.click(target);
      for (const ch of text) { await page.keyboard.type(ch); await wait(perChar); }
    },
    /** Scroll the page so `target` (a selector or a y offset) sits near the top, smoothly. */
    async scroll(target, ms = 900) {
      const y = typeof target === 'number' ? target
        : await page.locator(target).first().evaluate((el) => el.getBoundingClientRect().top + window.scrollY - 40);
      await page.evaluate(([to, dur]) => new Promise((done) => {
        const from = window.scrollY; const t0 = performance.now();
        const step = (now) => {
          const k = Math.min(1, (now - t0) / dur); const e = k < 0.5 ? 2 * k * k : 1 - (-2 * k + 2) ** 2 / 2;
          window.scrollTo(0, from + (to - from) * e);
          if (k < 1) requestAnimationFrame(step); else done();
        };
        requestAnimationFrame(step);
      }), [y, ms]);
    },
  };
  return d;
}

async function recordScene(browser, scene) {
  const ctx = await browser.newContext({ viewport: VIEW, deviceScaleFactor: SIZE.width / VIEW.width, colorScheme: 'light' });
  await ctx.addInitScript({ content: overlay });
  await ctx.addInitScript(({ message, link }) => {
    // The splash page asks the extension for its rule; here the demo answers in its place.
    if (location.pathname.includes('/splash/')) {
      window.chrome = window.chrome || {};
      window.chrome.runtime = { sendMessage: () => Promise.resolve({ ok: true, message, link }) };
    }
  }, scene.splash ?? { message: '', link: '' });
  await ctx.route(/^https:\/\/(?!fonts\.)/, (route) => route.abort());
  const page = await ctx.newPage();
  const d = director(page, VIEW);
  if (scene.setup) await scene.setup(d);

  const cdp = await ctx.newCDPSession(page);
  const frames = [];
  cdp.on('Page.screencastFrame', async ({ data, metadata, sessionId }) => {
    frames.push({ data, t: metadata.timestamp });
    await cdp.send('Page.screencastFrameAck', { sessionId }).catch(() => {});
  });
  await cdp.send('Page.startScreencast', { format: 'jpeg', quality: 92, maxWidth: SIZE.width, maxHeight: SIZE.height, everyNthFrame: 1 });
  const started = Date.now() / 1000;
  await scene.run(d);
  const stopped = Date.now() / 1000;
  await cdp.send('Page.stopScreencast');
  await ctx.close();

  const dir = join(WORK, `scene-${scene.n}`);
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const lines = ['ffconcat version 1.0'];
  frames.forEach((f, i) => {
    const file = `${String(i).padStart(5, '0')}.jpg`;
    writeFileSync(join(dir, file), Buffer.from(f.data, 'base64'));
    const next = frames[i + 1]?.t ?? stopped;
    const start = i === 0 ? started : f.t;
    lines.push(`file ${file}`, `duration ${Math.max(0.001, next - start).toFixed(4)}`);
  });
  lines.push(`file ${String(frames.length - 1).padStart(5, '0')}.jpg`);
  writeFileSync(join(dir, 'frames.txt'), `${lines.join('\n')}\n`);

  const length = scene.seconds;
  const out = join(WORK, `scene-${scene.n}.mp4`);
  execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-f', 'concat', '-safe', '0', '-i', join(dir, 'frames.txt'),
    '-vf', [`fps=${FPS}`, `scale=${SIZE.width}:${SIZE.height}:flags=lanczos`, `tpad=stop_mode=clone:stop_duration=${length}`, `trim=duration=${length}`,
      `fade=t=in:st=0:d=${FADE}:color=${PAPER}`, `fade=t=out:st=${length - FADE}:d=${FADE}:color=${PAPER}`, 'format=yuv420p'].join(','),
    '-c:v', 'libx264', '-preset', 'slow', '-crf', '18', '-r', String(FPS), out]);
  console.log(`scene ${scene.n}: ${frames.length} frames, ${(stopped - started).toFixed(1)}s captured, ${length}s kept`);
  return out;
}

const only = process.argv.slice(2).map(Number);
const scenes = only.length ? SCENES.filter((s) => only.includes(s.n)) : SCENES;
mkdirSync(WORK, { recursive: true });
const browser = await chromium.launch();
const clips = [];
for (const scene of scenes) clips.push(await recordScene(browser, scene));
await browser.close();

/** Joins encoded clips, listed in `list` in the work folder, into `out`. */
function concat(list, files, out) {
  writeFileSync(join(WORK, list), files.map((c) => `file '${c}'`).join('\n'));
  execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-f', 'concat', '-safe', '0', '-i', join(WORK, list),
    '-c:v', 'libx264', '-preset', 'slow', '-crf', '18', '-movflags', '+faststart', out]);
  console.log(out);
}

if (!only.length) concat('clips.txt', clips, OUT);
if (!only.length && CAPTIONS) {
  // The cut-downs close on the end card held for END_IN_CUTS seconds instead of its full length.
  const clip = new Map(scenes.map((s, i) => [s.n, clips[i]]));
  const last = SCENES.at(-1).n;
  const end = join(WORK, 'end-short.mp4');
  execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-i', clip.get(last),
    '-vf', `trim=duration=${END_IN_CUTS},fade=t=out:st=${END_IN_CUTS - FADE}:d=${FADE}:color=${PAPER},format=yuv420p`,
    '-c:v', 'libx264', '-preset', 'slow', '-crf', '18', '-r', String(FPS), end]);
  for (const [name, ns] of Object.entries(CUTS)) {
    concat(`${name}.txt`, ns.map((n) => (n === last ? end : clip.get(n))), join(here, '..', '..', `${slug}-demo-${name}.mp4`));
  }
}
