// The demo video's scenes, in order, matching ../../demo-video-script.md. Each scene has its number,
// its length in seconds, and run(d), which drives the page with the helpers in record.mjs while it
// is being captured; setup(d), when present, runs before capture starts. CUTS names the shorter
// clips made from the same scenes; their end card is held for END_IN_CUTS seconds.

const FONTS = '<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Newsreader:opsz,wght@6..72,400;6..72,500&display=swap">';
const PAPER = `
  html, body { margin: 0; height: 100%; }
  body { background: radial-gradient(60% 50% at 100% 0%, rgb(75 71 201 / .08), transparent 70%),
    radial-gradient(55% 45% at 0% 100%, rgb(202 138 4 / .07), transparent 70%), #f6f5f1;
    color: #131312; font-family: "Inter", system-ui, sans-serif; display: grid; place-items: center; }
  .in { opacity: 0; transform: translateY(10px); animation: rise .9s cubic-bezier(.32,.72,0,1) forwards; }
  @keyframes rise { to { opacity: 1; transform: none; } }`;
const MARK = (size) => `<svg width="${size}" height="${size}" viewBox="0 0 22 22" aria-hidden="true"><defs><linearGradient id="m" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#fff"/><stop offset=".5" stop-color="#e4e2dc"/><stop offset=".5" stop-color="#d6d4cd"/><stop offset="1" stop-color="#f3f1ec"/></linearGradient></defs><rect x=".5" y=".5" width="21" height="21" rx="6" fill="url(#m)" stroke="rgb(17 17 17 / .18)" stroke-width=".6"/></svg>`;
const escape = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

function card(body, extraCss = '') {
  return `<!doctype html><html><head><meta charset="utf-8">${FONTS}<style>${PAPER}${extraCss}</style></head><body>${body}</body></html>`;
}

async function showCard(d, html) {
  await d.page.setContent(html, { waitUntil: 'networkidle' });
  await d.page.evaluate(() => document.fonts.ready);
  await d.page.evaluate(() => window.__demo?.cursor(-50, -50));
}

/** A plain browser window: an address bar over a frame. Nothing in it imitates a real site. */
function browserFrame() {
  return card(`
    <div class="win">
      <div class="bar"><span class="dots"><i></i><i></i><i></i></span><div class="addr"><span class="lock">⌁</span><span id="addr"></span><span id="caret"></span></div></div>
      <iframe id="view" src="about:blank"></iframe>
    </div>`, `
    body { place-items: stretch; padding: 36px 56px 150px; box-sizing: border-box; }
    .win { display: flex; flex-direction: column; border-radius: 14px; overflow: hidden; background: #fff;
      box-shadow: 0 1px 2px rgb(0 0 0 / .06), 0 30px 60px -30px rgb(0 0 0 / .35); border: 1px solid rgb(17 17 17 / .1); }
    .bar { display: flex; align-items: center; gap: 18px; padding: 12px 16px; background: #ecebe6; border-bottom: 1px solid rgb(17 17 17 / .08); }
    .dots i { display: inline-block; width: 11px; height: 11px; border-radius: 50%; background: #d3d1ca; margin-right: 7px; }
    .addr { flex: 1; display: flex; align-items: center; gap: 8px; height: 34px; padding: 0 14px; border-radius: 9px; background: #fff;
      font: 400 16px/1 "Inter", system-ui, sans-serif; color: #2f3437; }
    .lock { color: #a3a19b; }
    #caret { width: 1.5px; height: 18px; background: #4b47c9; animation: blink 1s steps(1) infinite; }
    @keyframes blink { 50% { opacity: 0; } }
    iframe { flex: 1; border: 0; width: 100%; background: #f6f5f1; }`);
}

export const SCENES = [
  {
    n: 1, seconds: 8,
    setup: (d) => showCard(d, card(`
      <div style="text-align:center;max-width:1100px">
        <div class="in" style="font:400 76px/1.05 Newsreader, Georgia, serif;letter-spacing:-.02em">Your people already use AI at work.</div>
        <div class="in" style="animation-delay:2.2s;margin-top:26px;font:400 30px/1.35 Inter, sans-serif;color:#6c6b66">Do you know which tools, and what goes into them?</div>
      </div>`)),
    run: (d) => d.wait(8200),
  },
  {
    n: 2, seconds: 8,
    setup: (d) => showCard(d, card(`
      <div style="text-align:center;max-width:1240px">
        <div class="in" style="display:flex;align-items:center;justify-content:center;gap:22px;font:600 64px/1 Inter, sans-serif;letter-spacing:-.02em">${MARK(72)}<span>${escape(d.brand.product)}</span></div>
        <div class="in" style="animation-delay:.9s;margin-top:34px;font:400 44px/1.1 Newsreader, Georgia, serif;letter-spacing:-.01em">See it. Steer it. Prove it.</div>
        <div class="in" style="animation-delay:2.2s;margin-top:22px;font:400 26px/1.4 Inter, sans-serif;color:#6c6b66">${escape(d.brand.product)} shows which AI tools your people use, and steers them to the ones you approve.</div>
      </div>`)),
    run: (d) => d.wait(8200),
  },
  {
    n: 3, seconds: 24,
    setup: (d) => d.goto('index.html#posture'),
    async run(d) {
      await d.wait(400);
      await d.caption('The AI tools people use, in the browser and in coding tools.');
      await d.move('text=PROMPTS', 900);
      await d.wait(1600);
      await d.move('text=USERS', 900);
      await d.wait(1600);
      await d.move('text=DEVICES REPORTING', 900);
      await d.wait(1200);
      await d.caption('How many people use each one.');
      await d.scroll('text=Tools in use', 1400);
      await d.move('text=Tools in use', 700);
      await d.wait(2000);
      await d.caption('Sanctioned and unsanctioned, side by side.');
      await d.click('role=link[name="Usage"]', 900);
      await d.wait(800);
      await d.scroll('text=Tools by prompts', 1400);
      await d.wait(3000);
    },
  },
  {
    n: 4, seconds: 19,
    setup: (d) => d.goto('index.html#posture'),
    async run(d) {
      await d.wait(300);
      await d.caption('Sensitive data is detected on the device.');
      await d.move('text=SENSITIVE PROMPTS', 700);
      await d.wait(2200);
      await d.caption('Rules warn on, block or redirect a tool. In coding agents, they also stop sensitive prompts.');
      await d.click('role=link[name="Policy"]', 700);
      await d.wait(1600);
      await d.click('text=redirect_chatgpt_to_copilot', 800);
      await d.wait(800);
      // The rule's panel scrolls to its action, message and link.
      await d.page.locator('#rl-message').evaluate((el) => el.scrollIntoView({ behavior: 'smooth', block: 'center' }));
      await d.wait(1100);
      await d.move('.dialog-panel button[data-value="redirect"]', 600);
      await d.wait(2600);
    },
  },
  {
    n: 5, seconds: 12,
    splash: {
      message: "ChatGPT isn't approved for company work. Use Microsoft Copilot instead.",
      link: 'https://copilot.microsoft.com/',
    },
    async setup(d) {
      await showCard(d, browserFrame());
      d.splashUrl = new URL('splash/redirect.html?rule=redirect_chatgpt&from=chatgpt.com', process.env.DEMO_URL ?? 'http://127.0.0.1:8790/').href;
    },
    async run(d) {
      await d.caption('People get your message, and one click to the tool you approve.');
      await d.wait(500);
      for (const ch of 'chatgpt.com') {
        await d.page.evaluate((c) => { document.getElementById('addr').textContent += c; }, ch);
        await d.wait(85);
      }
      await d.wait(500);
      await d.page.evaluate((src) => {
        document.getElementById('caret').remove();
        document.getElementById('view').src = src;
      }, d.splashUrl);
      const view = d.page.frameLocator('#view');
      await view.locator('#sp-go:not([hidden])').waitFor();
      // The button would leave for the real site; the demo shows where it goes instead.
      await d.page.frames().find((f) => f.url().includes('/splash/')).evaluate(() => {
        document.getElementById('sp-go').addEventListener('click', (e) => { e.preventDefault(); parent.postMessage('go', '*'); });
      });
      await d.page.evaluate(() => addEventListener('message', () => { document.getElementById('addr').textContent = 'copilot.microsoft.com'; }));
      await d.wait(2600);
      await d.click(view.locator('#sp-go'), 1100);
      await d.wait(5000);
    },
  },
  {
    n: 6, seconds: 15,
    setup: (d) => d.goto('explore.html'),
    async run(d) {
      await d.caption('Search finds the prompt.');
      await d.type('#x-text-query', 'customer', 90);
      await d.click('#x-text-form button[type=submit]', 600);
      await d.wait(1600);
      await d.click('button.x-hit:has-text("Turn these call notes")', 900);
      await d.caption('Opening it takes a specific role.');
      await d.move('text=Everything captured for this request', 900);
      await d.wait(4600);
    },
  },
  {
    // One reader's entries, so the table holds no record IDs and fits the window.
    n: 7, seconds: 9,
    async setup(d) {
      await d.goto('index.html#audit?actor=arjun.mehta%40northwind.example');
      await d.page.locator('text=Audit entries').evaluate((el) => window.scrollTo(0, el.getBoundingClientRect().top + window.scrollY - 40));
    },
    async run(d) {
      await d.caption('Every search and decision is in a tamper-evident audit trail.');
      await d.move('table >> text=export.list', 900);
      await d.wait(1500);
      await d.move('table >> text=tool.sanction', 800);
      await d.wait(2600);
    },
  },
  {
    n: 8, seconds: 15,
    setup: (d) => d.goto('index.html#tools'),
    async run(d) {
      await d.caption('See which tools people already use before you choose one.');
      await d.scroll('text=Tools by prompts', 1000);
      await d.click('role=link[name="Unsanctioned"]', 900);
      await d.wait(1800);
      await d.click('role=link[name="No decision yet"]', 800);
      await d.wait(1800);
      await d.scroll(0, 700);
      await d.click('role=link[name="Teams"]', 900);
      await d.caption('And which teams use them.');
      await d.wait(3000);
    },
  },
  {
    n: 9, seconds: 15,
    setup: (d) => d.goto('index.html#settings'),
    async run(d) {
      await d.caption('By default, prompt text stays on the device.');
      await d.move('text=M1 · digest & labels', 700);
      await d.wait(1500);
      await d.caption('Or choose full capture: encrypted and searchable.');
      await d.move('text=M3 · prompt', 600);
      await d.wait(500);
      await d.move('text=Store and search prompts', 600);
      await d.wait(1300);
      await d.click('role=link[name="Devices"]', 800);
      await d.caption('Devices enroll through Intune, and quiet ones are flagged.');
      await d.page.locator('text=Need attention').first().waitFor();
      await d.wait(300);
      await d.move('text=Need attention', 700);
      await d.wait(2600);
    },
  },
  {
    n: 10, seconds: 8,
    setup: (d) => showCard(d, card(`
      <div style="text-align:center">
        <div class="in" style="display:flex;align-items:center;justify-content:center;gap:20px;font:600 56px/1 Inter, sans-serif;letter-spacing:-.02em">${MARK(62)}<span>${escape(d.brand.product)}</span></div>
        <div class="in" style="animation-delay:.6s;margin-top:30px;font:400 40px/1.15 Newsreader, Georgia, serif">See what AI your people use, on your own devices.</div>
        <div class="in" style="animation-delay:1.2s;margin-top:34px;display:inline-block;padding:14px 26px;border-radius:14px;background:#131312;color:#f6f5f1;font:600 26px/1 Inter, sans-serif">${escape(d.brand.domain)}</div>
      </div>`)),
    run: (d) => d.wait(8200),
  },
];

/** Shorter clips cut from the same scenes: a teaser and a clip on rules and redirects. */
export const CUTS = { teaser: [2, 5, 10], policy: [2, 4, 5, 10] };
export const END_IN_CUTS = 4;
