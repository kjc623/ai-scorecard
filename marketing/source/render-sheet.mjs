// Prints product-sheet.html to ../<product>-product-sheet.pdf, filled from brand.json.
//
//   node render-sheet.mjs
//
// Needs this folder's packages (npm ci) and Playwright's Chromium.

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { chromium } from 'playwright';

const here = dirname(fileURLToPath(import.meta.url));

const brand = JSON.parse(readFileSync(join(here, 'brand.json'), 'utf8'));
const slug = brand.product.toLowerCase().replace(/[^a-z0-9]+/g, '-');
const out = join(here, '..', `${slug}-product-sheet.pdf`);

const browser = await chromium.launch({ args: ['--allow-file-access-from-files'] });
const page = await browser.newPage();
await page.goto(pathToFileURL(join(here, 'product-sheet.html')).href, { waitUntil: 'networkidle' });
await page.evaluate((b) => window.fill(b), brand);
await page.evaluate(() => document.fonts.ready);
const overflow = await page.evaluate(() => [...document.querySelectorAll('.page')].map((p) => p.scrollHeight > p.clientHeight + 4));
if (overflow.some(Boolean)) console.warn('warning: content overflows page', overflow.map((o, i) => (o ? i + 1 : null)).filter(Boolean).join(', '));
await page.pdf({ path: out, format: 'Letter', printBackground: true, preferCSSPageSize: true });
await browser.close();
console.log(out);
