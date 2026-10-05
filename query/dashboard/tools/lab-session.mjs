// lab-session.mjs — sign in to a lab dashboard as one account and print its session cookie.
//
// The same path a browser takes, over plain HTTP: the dashboard's /signin/start with the work
// email (control-api begins and names the identity provider), the lab stand-in IdP's /authorize
// (it picks the account from login_hint, or lists its accounts and this follows the one asked
// for), and the dashboard's /callback, which sets `sac_session`. Lab only: the stand-in signs
// anyone in without a password, which is what makes this possible.
//
//   node tools/lab-session.mjs viewer.sample@lab.test --dashboard http://dashboard-sample:8787
//   node tools/observe.mjs 'http://dashboard-sample:8787/index.html?transport=live' --session <printed value>
//   curl -b "sac_session=<printed value>" http://dashboard-sample:8787/session
//
// The value is a credential for that session until it ends (control-api: 8 h, or 1 h idle).
// Zero dependencies.

const args = process.argv.slice(2);
const account = args.find((a) => !a.startsWith('--') && args[args.indexOf(a) - 1] !== '--dashboard');
const dashboard = new URL(args.includes('--dashboard') ? args[args.indexOf('--dashboard') + 1] : (process.env.SAC_DASHBOARD_URL ?? 'http://127.0.0.1:8787/'));

function cookieFrom(res, name) {
  const line = res.headers.getSetCookie().find((c) => c.startsWith(`${name}=`));
  return line ? line.slice(name.length + 1).split(';')[0] : null;
}

async function step(url, init = {}) {
  const res = await fetch(url, { redirect: 'manual', ...init });
  return res;
}

async function main() {
  if (!account) throw new Error('give the account to sign in as, for example viewer.sample@lab.test');
  const start = new URL('/signin/start', dashboard);
  start.searchParams.set('email', account);
  const begun = await step(start);
  const attempt = cookieFrom(begun, 'sac_signin');
  if (begun.status !== 302 || !attempt) throw new Error(`the dashboard did not begin a sign-in (${begun.status}); is it connected to control-api?`);

  let at = new URL(begun.headers.get('location'));
  let provider = await step(at);
  if (provider.status === 200) {
    // The stand-in's account chooser: follow the account's own link, as a person would click it.
    const page = await provider.text();
    const link = [...page.matchAll(/href="([^"]+)"[^>]*>([^<]+)</g)].find((m) => m[2].trim().toLowerCase() === account.toLowerCase());
    if (!link) throw new Error(`the identity provider at ${at.origin} does not list ${account}`);
    at = new URL(link[1].replace(/&amp;/g, '&'), at);
    provider = await step(at);
  }
  const callback = provider.headers.get('location');
  if (provider.status !== 302 || !callback) throw new Error(`the identity provider did not return to the dashboard (${provider.status})`);

  const done = await step(new URL(callback, dashboard), { headers: { cookie: `sac_signin=${attempt}` } });
  const session = cookieFrom(done, 'sac_session');
  if (!session) {
    const reason = /<strong>([^<]+)<\/strong>/.exec(await done.text())?.[1] ?? `status ${done.status}`;
    throw new Error(`the dashboard did not sign ${account} in: ${reason}`);
  }
  process.stdout.write(`${session}\n`);
}

main().catch((error) => {
  console.error(`lab-session: ${error.message}`);
  process.exitCode = 1;
});
