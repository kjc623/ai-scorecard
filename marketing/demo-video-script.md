# Sundial demo video: script and shot list

A 2:13 product overview for the website, LinkedIn and sales follow-up emails, 1920×1080 (16:9).
It follows the four things the product sheet promises, in the same order (Know, Protect, Prove,
Decide), then privacy and deployment. Every console shot is the shipping console with fictional
sample data; one shot shows an employee's browser.

The video has no audio. It is made to play muted, as it will in feeds and email previews: the
captions carry every line, kept short so the console carries the message. A version without captions is kept for a voiceover or another language
later.

Cut-downs from the same recording, each opening on the logo card and closing on a 4-second end card:
- **24-second teaser:** scenes 2, 5 and 10. The logo card says what the product does; the
  redirect shows it.
- **43-second policy clip:** scenes 2, 4, 5 and 10.

## Before recording

- Use the demo console (`source/demo/`), never a customer's tenant or the owner's lab. Every name,
  team and figure on screen is fictional.
- Light theme, browser window 1760×990 for every scene, scaled to 1920×1080, the 7-day window.
- The redirect rule sends ChatGPT to Microsoft Copilot, and the redirect page says so.
- Show only pages the shipping console has: Overview, Usage, Search, Policy and Settings. Insights
  and Policy › Activity are pre-production pages and stay out of the video.
- Keep the installer's file names and product code off screen: Settings › Devices is shown from
  its top, not its install section.

## Script

| # | Time | On screen | Caption |
|---|---|---|---|
| 1 | 0:00–0:08 | Title card on the warm paper background. | Your people already use AI at work. Do you know which tools, and what goes into them? |
| 2 | 0:08–0:16 | Logo card: the Sundial mark, "See it. Steer it. Prove it." and one line on what it does. | Sundial shows which AI tools your people use, and steers them to the ones you approve. |
| 3 | 0:16–0:40 | **Know.** Overview: the Prompts, Users and Devices reporting tiles, then Tools in use. Click Usage; the Tools list by prompts. | The AI tools people use, in the browser and in coding tools. / How many people use each one. / Sanctioned and unsanctioned, side by side. |
| 4 | 0:40–0:59 | **Protect.** Overview's Sensitive prompts tile. Click Policy; open the `redirect_chatgpt_to_copilot` rule and scroll its panel to the action, message and link. | Sensitive data is detected on the device. / Rules warn on, block or redirect a tool. In coding agents, they also stop sensitive prompts. |
| 5 | 0:59–1:11 | Employee's browser (a plain browser frame): type chatgpt.com; the Sundial redirect page appears; click "Continue to copilot.microsoft.com". | People get your message, and one click to the tool you approve. |
| 6 | 1:11–1:26 | **Prove.** Search: type "customer", run it, open one prompt. | Search finds the prompt. / Opening it takes a specific role. |
| 7 | 1:26–1:35 | Settings › Audit trail, filtered to one reviewer's entries: a list export and a sanction decision. | Every search and decision is in a tamper-evident audit trail. |
| 8 | 1:35–1:50 | **Decide.** Usage › Tools, filter Unsanctioned, then No decision yet. Then Teams. | See which tools people already use before you choose one. / And which teams use them. |
| 9 | 1:50–2:05 | Settings › Collection: the default (M1) and this company's choice (M3, store and search prompts). Then Settings › Devices: Need attention. | By default, prompt text stays on the device. / Or choose full capture: encrypted and searchable. / Devices enroll through Intune, and quiet ones are flagged. |
| 10 | 2:05–2:13 | End card, held still: the Sundial mark, the line, and sundial.solutions. Add a contact or booking link once there is one. | See what AI your people use, on your own devices. |

## Shots to capture

1. Console: Overview, Usage (Tools, the sanction filter, Teams), Search with a text search and a
   prompt opened, Policy › Rules with the redirect rule opened, Settings › Audit trail filtered to
   one person, Settings › Collection, Settings › Devices (scenes 3, 4, 6, 7, 8, 9).
2. Employee browser: the redirect page for chatgpt.com and the Continue click, inside a plain
   browser frame (scene 5). No third-party site is shown.
3. Title, logo and end cards (scenes 1, 2, 10), in the product sheet's look.

## Notes

- Every claim here matches the product sheet and was checked against the code on main. In the
  browser, rules act on the tool, so the video shows the redirect on ChatGPT; rules on sensitive
  data act in coding agents. Microsoft Copilot prompts are recorded, not blocked.
- Prices stay out of the video until the plans are approved.
