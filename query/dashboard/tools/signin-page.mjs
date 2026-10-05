// signin-page.mjs — the sign-in page and the sign-in error pages, rendered by the dashboard server.
//
// Plain HTML with no script, styled by the dashboard's own styles.css and signin.css, both served
// without a session. Nothing is fetched from another host. A page that has to say something went
// wrong says it in a sentence a person can act on, with the short code a help desk can search for;
// it never carries an exception, a stack or an upstream body.
//
// The forms POST to /signin/start, which asks control-api to begin the sign-in and sends the browser
// to the identity provider. The lab's development sign-in is offered only when this server runs as
// the development principal, which is the only time it can work.

const escapeHtml = (value) => String(value ?? '')
  .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');

/**
 * What each outcome says. `status` is the HTTP status the page is served with; `level` picks the
 * banner's colour (refusal, warning, info), the same three the dashboard uses.
 */
export const SIGNIN_MESSAGES = Object.freeze({
  no_sso_connection: Object.freeze({
    status: 404, level: 'refusal', title: 'No single sign-on for that address',
    text: 'We could not find your organisation\'s sign-in from that email address. Check the address, or use Sign in with Microsoft if your organisation uses Microsoft Entra ID. If it still does not work, your organisation may not be set up yet: ask your administrator.',
  }),
  tenant_not_onboarded: Object.freeze({
    status: 403, level: 'refusal', title: 'Your organisation is not set up yet',
    text: 'You signed in, but your organisation has not finished setting up Shadow AI Capture. Your administrator completes it from the onboarding link they were sent.',
  }),
  no_role: Object.freeze({
    status: 403, level: 'refusal', title: 'No access has been assigned to you',
    text: 'You signed in, but your account has not been given access to this dashboard. Ask your administrator to assign you a role: viewer, analyst, content reader or admin.',
  }),
  connection_disabled: Object.freeze({
    status: 403, level: 'refusal', title: 'Sign-in is turned off for your organisation',
    text: 'Sign-in through your organisation\'s identity provider has been turned off for this product. Contact your administrator.',
  }),
  user_deactivated: Object.freeze({
    status: 403, level: 'refusal', title: 'Your account is not active',
    text: 'Your organisation\'s directory says your account is deactivated, so you cannot sign in to this product. Contact your administrator.',
  }),
  invite_invalid: Object.freeze({
    status: 400, level: 'refusal', title: 'This onboarding link cannot be used',
    text: 'It has been used already, or it has expired. Ask your vendor for a new onboarding link.',
  }),
  wrong_tenant: Object.freeze({
    status: 403, level: 'refusal', title: 'This dashboard serves another organisation',
    text: 'You signed in to an organisation this dashboard does not serve. Use your own organisation\'s dashboard address.',
  }),
  not_permitted: Object.freeze({
    status: 403, level: 'refusal', title: 'Your role cannot open this page',
    text: 'You are signed in, but this page is not part of what your role can use. Ask your administrator if you need it.',
  }),
  signin_expired: Object.freeze({
    status: 400, level: 'warning', title: 'That sign-in expired',
    text: 'The sign-in took longer than ten minutes, or it was started in another browser. Start again.',
  }),
  idp_refused: Object.freeze({
    status: 400, level: 'refusal', title: 'Your identity provider stopped the sign-in',
    text: 'Your organisation\'s identity provider did not complete the sign-in. Try again, or ask your administrator if it keeps happening.',
  }),
  email_required: Object.freeze({
    status: 400, level: 'warning', title: 'Enter your work email',
    text: 'Enter the email address you use at work, or use Sign in with Microsoft.',
  }),
  signin_failed: Object.freeze({
    status: 502, level: 'refusal', title: 'Sign-in could not be completed',
    text: 'Something went wrong while signing you in. Try again in a moment.',
  }),
  unavailable: Object.freeze({
    status: 503, level: 'refusal', title: 'Sign-in is unavailable',
    text: 'The sign-in service cannot be reached right now. Try again in a minute.',
  }),
  session_ended: Object.freeze({
    status: 200, level: 'info', title: 'Your session has ended',
    text: 'Sign in again to continue.',
  }),
  signed_out: Object.freeze({
    status: 200, level: 'info', title: 'You have signed out',
    text: 'Close this window, or sign in again.',
  }),
});

/** The message for a code, or the generic failure. A code from elsewhere is never shown as text unless it is a plain word. */
export function signinMessage(code) {
  return Object.hasOwn(SIGNIN_MESSAGES, code ?? '') ? SIGNIN_MESSAGES[code] : SIGNIN_MESSAGES.signin_failed;
}

const MICROSOFT_LOGO = '<svg class="si-ms-logo" viewBox="0 0 21 21" aria-hidden="true">'
  + '<rect x="1" y="1" width="9" height="9" fill="#f25022"/><rect x="11" y="1" width="9" height="9" fill="#7fba00"/>'
  + '<rect x="1" y="11" width="9" height="9" fill="#00a4ef"/><rect x="11" y="11" width="9" height="9" fill="#ffb900"/></svg>';

/**
 * The whole page.
 *
 * @param {object} input
 * @param {string|null} [input.code]       an outcome from SIGNIN_MESSAGES, or null for the plain page
 * @param {string|null} [input.detail]     a provider's own error code, shown beside ours
 * @param {string} [input.next]            where to return after signing in (already checked by the caller)
 * @param {string} [input.email]           the address to keep in the field
 * @param {boolean} [input.identity]       whether the identity service is configured (the two sign-in forms)
 * @param {{actor: string, tenant: string}|null} [input.dev]  the development principal, in development mode only
 */
export function renderSigninPage({ code = null, detail = null, next = '/', email = '', identity = true, dev = null } = {}) {
  if (code === 'not_permitted') return renderNotPermitted();
  const message = code ? signinMessage(code) : null;
  const shownCode = code && Object.hasOwn(SIGNIN_MESSAGES, code) && !['signed_out', 'session_ended'].includes(code) ? code : null;
  const providerCode = typeof detail === 'string' && /^[a-z_]{1,64}$/.test(detail) ? detail : null;
  const banner = message
    ? `<div class="banner banner-${message.level === 'info' ? 'restart' : message.level}" role="${message.level === 'info' ? 'status' : 'alert'}">`
      + `<strong>${escapeHtml(message.title)}</strong>`
      + (message.text ? `<span>${escapeHtml(message.text)}</span>` : '')
      + (shownCode ? `<span class="si-code">${escapeHtml(shownCode)}${providerCode ? ` · ${escapeHtml(providerCode)}` : ''}</span>` : '')
      + '</div>'
    : '';
  const nextField = `<input type="hidden" name="next" value="${escapeHtml(next)}">`;
  const forms = identity
    ? '<form class="si-form" method="post" action="/signin/start">'
      + '<input type="hidden" name="provider" value="entra">' + nextField
      + `<button type="submit" class="si-ms">${MICROSOFT_LOGO}<span>Sign in with Microsoft</span></button>`
      + '</form>'
      + '<div class="si-or" role="separator"><span>or</span></div>'
      + '<form class="si-form" method="post" action="/signin/start">' + nextField
      + '<label class="si-label" for="si-email">Work email</label>'
      + `<input class="si-input" id="si-email" name="email" type="email" required autocomplete="username" spellcheck="false" placeholder="you@your-organisation.com" value="${escapeHtml(email)}">`
      + '<button type="submit" class="si-btn">Continue with SSO</button>'
      + '<p class="si-hint">We find your organisation\'s sign-in from the address. Okta, Ping, Google and other OpenID Connect providers sign in this way.</p>'
      + '</form>'
    : '';
  const devPanel = dev
    ? '<section class="si-dev card"><div class="card-core">'
      + '<h3>Lab development sign-in</h3>'
      + '<p>This server is not connected to an identity service. Every request it forwards carries a fixed development principal, which only a query-api started with <code>SAC_DEV_TRUST_PRINCIPAL=1</code> accepts. It is a lab arrangement, not authentication.</p>'
      + `<dl class="si-facts"><div><dt>Actor</dt><dd>${escapeHtml(dev.actor || 'not set')}</dd></div><div><dt>Tenant</dt><dd><code>${escapeHtml(dev.tenant || 'not set')}</code></dd></div></dl>`
      + `<a class="si-btn si-btn-link" href="${escapeHtml(next)}">Continue as the development principal</a>`
      + '</div></section>'
    : '';
  const title = message && !['signed_out', 'session_ended'].includes(code) ? message.title : 'Sign in';
  return pageShell(title,
    '<section class="si-card card"><div class="card-core">'
    + `<h2>${escapeHtml(identity ? 'Sign in' : 'Development mode')}</h2>`
    + `<p class="si-lead">${identity ? 'Use your organisation\'s account.' : 'Sign-in is not configured on this server.'}</p>`
    + banner + forms
    + '</div></section>'
    + devPanel
    + (identity ? '<p class="si-foot">You sign in with your organisation\'s identity provider. Shadow AI Capture never sees your password.</p>' : ''));
}

/** A signed-in person asked for a page their role does not include: a way back, not a sign-in form. */
function renderNotPermitted() {
  const message = SIGNIN_MESSAGES.not_permitted;
  return pageShell(message.title,
    '<section class="si-card card"><div class="card-core">'
    + `<h2>${escapeHtml(message.title)}</h2>`
    + `<p class="si-lead">${escapeHtml(message.text)}</p>`
    + '<a class="si-btn" href="/index.html?transport=live">Back to the dashboard</a>'
    + '<form class="si-form" method="post" action="/signout"><button type="submit" class="si-btn si-btn-quiet">Sign out</button></form>'
    + '</div></section>');
}

function pageShell(title, body) {
  return '<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n'
    + '<meta name="viewport" content="width=device-width, initial-scale=1">\n'
    + '<meta name="robots" content="noindex">\n'
    + `<title>${escapeHtml(title)} | Shadow AI Capture</title>\n`
    + '<link rel="icon" href="data:,">\n<link rel="stylesheet" href="/styles.css">\n<link rel="stylesheet" href="/signin.css">\n</head>\n<body>\n'
    + '<main class="si">'
    + '<div class="brand si-brand"><span class="brand-mark" aria-hidden="true"></span><h1>Shadow AI Capture</h1></div>'
    + body
    + '</main>\n</body>\n</html>\n';
}
