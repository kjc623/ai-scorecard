# Marketing materials

Drafts for review. Every price, plan name and minimum is a proposal, not a decision.

| File | What it is |
|---|---|
| `sundial-product-sheet.pdf` | Two-page Letter product sheet: the four outcomes, coverage, rules, privacy, deployment, proposed plans. |
| `demo-video-script.md` | Script and shot list for the 2:13 demo video and its two cut-downs. |
| `sundial-demo.mp4` | The recorded demo video, 2:13, 1920×1080 (16:9), captions burned in. It has no audio: the captions carry every line. |
| `sundial-demo-no-captions.mp4` | The same video without captions, for a voiceover or another language later. |
| `sundial-demo-teaser.mp4` | The 24-second teaser: the logo card, the redirect, the end card. |
| `sundial-demo-policy.mp4` | The 43-second policy clip: the logo card, rules, the redirect, the end card. |
| `source/deck/` | The 13-slide deck: its slides (HTML at 1920×1080, for a Claude Slides artifact, which serves the images at `/_blob/<id>`) and the images in `assets/`. |

## Changing the name, domain, contact or prices

Everything the sheet says about the brand and the plans comes from `source/brand.json`. Edit it,
then rebuild. The demo console replaces "Sundial" in the console's pages with `product`, so the
video follows the same file. The deck's slides are written out from the same values but are not
rebuilt automatically: the name appears in its slides' text, and the plans and close slides carry
the brand values, so edit those slides in the deck.

## Rebuilding

Needs Node 22 and this folder's packages with Playwright's Chromium; the video also needs ffmpeg
with libx264.

```sh
cd source
npm ci && npx playwright install chromium
node render-sheet.mjs                        # writes ../<product>-product-sheet.pdf

cd demo && node server.mjs 8790 &            # the demo console on http://127.0.0.1:8790/
cd ../video
WORK=/tmp/sundial-video node record.mjs      # all scenes: the video, the teaser and the policy clip
WORK=/tmp/sundial-video node record.mjs 5    # one scene, for checking
CAPTIONS=0 WORK=/tmp/sundial-video-nc node record.mjs   # the full video without captions
```

`source/video/scenes.mjs` holds the scenes and, in `CUTS`, which scenes each cut-down joins.

The demo console serves this repository's dashboard (`services/dashboard/`) and redirect page
(`device/extension/splash/`) with the fictional sample data in `source/demo/demo-data.mjs`. It never
talks to a real tenant or the lab. `DEMO_ENV=preprod` would show pre-production pages; the
materials use the production page set only. The sample data follows the product's behaviour: in
the browser a rule acts on the tool and every prompt is logged; rules on sensitive data act only in
coding agents.

## What the materials leave out on purpose

- The monthly report, Insights and Policy › Activity: not in the production console.
- Ollama and other local models: capture is off by default and likely misses the `ollama` command
  itself, which reads the same address setting the agent moves.
- An Intune portal screenshot: not taken from a real tenant.
- Desktop app capture (Claude Desktop, ChatGPT Desktop): it needs TLS inspection, is off by
  default, and has not been confirmed on a real device.
- Customer branding on the redirect page: the page carries the Sundial name and mark and the
  customer's message.

## Before any of this goes out

Decisions for the owner:

- **Prices are placeholders.** Observe $5, Govern $9, Enterprise $14 per monthly active user,
  billed annually, with minimums of $6,000, $10,800 and $16,800 a year. The pilot cap of 50 users
  is in the deck's speaker notes only.
- **Contact details.** The sheet, deck and end card carry a placeholder or the domain. Add a name,
  email and booking link.
- **The domain.** sundial.solutions is on the sheet, deck and end card, but it is not confirmed
  as owned, and the trademark question on the name is open. Swap both in `source/brand.json` and
  rebuild; the deck needs a hand edit.
- **The installer name.** The console's Settings › Devices still names the installer and its
  tenant file `ShadowAICapture.msi` and `ShadowAICapture.tenant.env`. The video keeps that section
  off screen, but a customer sees it on day one.
- **The monthly report and Insights.** Observe in the pricing work lists a monthly report; it and
  Insights exist only in pre-production. The sheet and deck say "Search and CSV export" instead.
  Decide whether to ship them before promising them.

Also true today:

- **Planned, not built:** the EU data region and the uptime SLA, marked "Planned, not yet
  available" under the Enterprise plan.
- **Beta:** the ChatGPT Enterprise, Claude Enterprise and Microsoft 365 Copilot connectors, tested
  only against simulated provider APIs. They read prompt text from the provider into the service to
  check it for sensitive data, even when device capture keeps text on the device.
- **Capture rests mostly on replay tests.** ChatGPT on the web and Claude Code are the only tools
  seen working on the test VM. Claude, Gemini and Perplexity on the web, Codex CLI, Cursor, GitHub
  Copilot and both Microsoft Copilots are tested by replaying recorded or hand-written requests
  through the product's code. Edge is supported by the same extension but not exercised in CI.
- **Microsoft Copilot and Microsoft 365 Copilot are recorded, not blocked**, and so is GitHub
  Copilot.

Product issues the materials work around, which a customer would also see:

- In the browser, a rule cannot act on sensitive data: the extension decides before the prompt is
  classified (`device/extension/src/pipeline.js:533-535`). Those prompts also never count in the
  Overview's Sensitive prompts tile, which counts only prompts a label rule matched
  (`services/jobs/internal/rollup/rollup.go:178-181`).
- The query API lets an analyst list one tool's users ordered by prompts:
  `mart.agg_tool_user_period` lacks `rankable: false` (`services/query-api/src/registry.js:367-404`).
  The console has no such list, so the materials say "no leaderboards" of the console only.
- Opening a prompt from a text search shows the person as a reference and the device as "not
  recorded" (`services/dashboard/src/explore-render.js:506-515`).
- The audit trail's table is too wide for a 1440-pixel window, so actor emails break mid-word, and
  its Hash links and Anchored head cards speak to engineers ("the reconciler's job"). The video
  shows one reviewer's entries in a wider window.
- Rule names are identifiers such as `redirect_chatgpt_to_copilot`, which is how the console shows
  them everywhere.
- The redirect page and the console spell "organisation", while the sheet and deck use American
  spelling.
