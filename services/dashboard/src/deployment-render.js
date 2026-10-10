// deployment-render.js — Settings → Deployment, state to HTML. Pure: no DOM, no I/O, no state.
//
// The same honesty rules as every other screen: a count the server did not send is "not reported",
// never 0; a revoked key is not an expired one; nothing is a share of the fleet, and the key count is
// shown with its denominator. Every string from the API is escaped. The setup steps are short and
// say only what this product needs; they are not a copy of Microsoft's documentation.

import { escapeHtml, renderTile } from './render.js';
import { formatCount, formatInstant } from './format.js';
import { DEPLOYMENT_FORMATS, deploymentKeyState, verificationAvailable } from './deployment.js';

const DEP_INSTALL_COMMAND = 'msiexec /i ShadowAICapture.msi /qn';

export function depInstant(iso, absent = 'never') {
  return iso ? `<span class="v-time" title="${escapeHtml(iso)}">${escapeHtml(formatInstant(iso))}</span>` : `<span class="v-absent">${escapeHtml(absent)}</span>`;
}

export function depChip(value) {
  const text = String(value ?? 'unknown');
  return `<span class="v-vocab v-vocab-${escapeHtml(text.replace(/[^a-z_]/gi, ''))}">${escapeHtml(text)}</span>`;
}

export function depProblem(problem, lead = 'Not done.') {
  if (!problem) return '';
  return `<p class="dp-problem" role="alert"><strong>${escapeHtml(lead)}</strong> ${escapeHtml(problem.message ?? '')}`
    + (problem.code ? ` <code>${escapeHtml(problem.code)}</code>` : '') + '</p>';
}

export function depButton(label, data, { kind = '', disabled = false, small = false, type = 'button' } = {}) {
  const attrs = Object.entries(data).map(([k, v]) => ` data-${k}="${escapeHtml(v)}"`).join('');
  const cls = ['btn', kind ? `btn-${kind}` : '', small ? 'btn-small' : ''].filter(Boolean).join(' ');
  return `<button type="${type}" class="${cls}"${attrs}${disabled ? ' disabled' : ''}>${escapeHtml(label)}</button>`;
}

export function depCard(title, body, { extra = '', wide = false, id = '' } = {}) {
  return `<section class="card dp-card${wide ? ' dp-wide' : ''}"${id ? ` id="${id}"` : ''}><div class="card-core">`
    + `<header class="block-head"><h3>${escapeHtml(title)}</h3>${extra}</header>`
    + `<div class="dp-body">${body}</div></div></section>`;
}

function depHeader(eyebrow) {
  return '<header class="screen-head"><div class="screen-title">'
    + (eyebrow ? `<span class="eyebrow">${escapeHtml(eyebrow)}</span>` : '')
    + '<h2>Deployment</h2>'
    + '<p class="question">Put the capture agent on your organisation\'s Windows devices, and keep people in sync from your identity provider.</p>'
    + '</div></header>';
}

// ---------------------------------------------------------------------------------------------
// States before the data
// ---------------------------------------------------------------------------------------------

function depSkeleton() {
  const lines = (widths) => widths.map((w) => `<span class="dp-skel" style="width:${w}%"></span>`).join('');
  const tile = (w) => `<div class="tile card"><div class="card-core">${lines([42, w])}</div></div>`;
  const card = (widths) => `<section class="card dp-card"><div class="card-core"><div class="dp-body dp-skels">${lines(widths)}</div></div></section>`;
  return `<p class="sr" role="status">Loading the deployment settings…</p><div class="tiles" aria-hidden="true">${tile(30)}${tile(55)}${tile(40)}${tile(35)}</div>`
    + `<div class="dp-grid" aria-hidden="true">${card([36, 78, 64])}${card([44, 70, 58])}</div>`
    + `<div aria-hidden="true">${card([30, 92, 84, 61])}</div>`;
}

function depRefused(problem) {
  const status = problem?.status;
  const title = status === 401 ? 'Your session has ended'
    : status === 403 ? 'Only an admin can open the deployment settings'
      : status === 0 ? 'The admin API could not be reached'
        : 'The deployment settings could not be read';
  const text = status === 401 ? 'Sign in again to continue.'
    : status === 403 ? 'Your role does not include deployment. Ask an admin of your organisation.'
      : problem?.message ?? 'Nothing was read.';
  const action = status === 401
    ? '<a class="btn" href="/signin">Sign in again</a>'
    : status === 403 ? '' : depButton('Try again', { dep: 'retry' });
  return '<section class="card dp-card"><div class="card-core dp-state" role="alert">'
    + `<h3>${escapeHtml(title)}</h3><p>${escapeHtml(text)}</p>`
    + (problem?.code ? `<p class="dp-code">${escapeHtml(problem.code)}${status ? ` · ${escapeHtml(String(status))}` : ''}</p>` : '')
    + (action ? `<div class="dp-actions">${action}</div>` : '')
    + '</div></section>';
}

// ---------------------------------------------------------------------------------------------
// The sections
// ---------------------------------------------------------------------------------------------

function depTiles(data, now) {
  const keys = data.deployment_keys;
  const states = keys.map((k) => deploymentKeyState(k, now));
  const active = states.filter((s) => s === 'active').length;
  const revoked = states.filter((s) => s === 'revoked').length;
  const expired = states.filter((s) => s === 'expired').length;
  const number = (n) => (n === null ? { kind: 'absent' } : { kind: 'number', value: n, text: formatCount(n) });
  const tiles = [
    {
      label: 'Devices enrolled', value: number(data.devices.enrolled),
      note: data.devices.enrolled === null ? 'not reported by the server' : data.devices.last_enrolled_at ? `last enrolled ${formatInstant(data.devices.last_enrolled_at)}` : 'none enrolled yet',
    },
    {
      label: 'Active deployment keys',
      value: keys.length === 0 ? { kind: 'number', value: 0, text: '0' } : { kind: 'number', value: active, text: `${formatCount(active)} of ${formatCount(keys.length)}` },
      note: keys.length === 0 ? 'none created yet' : [`${formatCount(revoked)} revoked`, expired > 0 ? `${formatCount(expired)} expired` : null].filter(Boolean).join(' · '),
    },
    {
      label: 'People provisioned', value: number(data.scim.users),
      note: data.scim.users === null ? 'not reported by the server'
        : `${data.scim.groups === null ? 'groups not reported' : `${formatCount(data.scim.groups)} group${data.scim.groups === 1 ? '' : 's'}`} · ${data.scim.last_provisioned_at ? `last ${formatInstant(data.scim.last_provisioned_at)}` : 'never provisioned'}`,
    },
    {
      label: 'Agent release', value: data.release?.version ? { kind: 'number', value: 0, text: data.release.version } : { kind: 'absent' },
      note: data.release?.product_code ? `product code ${data.release.product_code}` : 'not reported by the server',
    },
  ];
  return `<div class="tiles">${tiles.map(renderTile).join('')}</div>`;
}

function depSignIn(data) {
  const c = data.connection;
  if (!c) {
    return depCard('Sign-in', '<p class="dp-empty">No identity provider is linked yet. It is linked from the onboarding link your vendor sent: the person who opens it chooses Microsoft Entra ID or another OpenID Connect provider and signs in once.</p>');
  }
  const provider = c.provider === 'entra' ? 'Microsoft Entra ID' : c.provider === 'oidc' ? 'OpenID Connect provider' : 'unknown';
  const where = c.provider === 'entra'
    ? `<div><dt>Entra tenant</dt><dd>${c.entra_tenant_id ? `<code>${escapeHtml(c.entra_tenant_id)}</code>` : '<span class="v-absent">not reported</span>'}</dd></div>`
    : `<div><dt>Issuer</dt><dd>${c.issuer ? `<code>${escapeHtml(c.issuer)}</code>` : '<span class="v-absent">not reported</span>'}</dd></div>`;
  const sentence = c.status === 'active' ? 'People in your organisation sign in through it.'
    : c.status === 'pending' ? 'Linked, not yet active: the first sign-in through it, by the person who opened the onboarding link, activates it.'
      : c.status === 'disabled' ? 'Turned off: nobody can sign in through it.'
        : 'Its state was not reported.';
  return depCard('Sign-in', `<dl class="dp-kv"><div><dt>Provider</dt><dd>${escapeHtml(provider)}</dd></div>${where}<div><dt>Status</dt><dd>${depChip(c.status ?? 'unknown')}</dd></div></dl>`
    + `<p class="dp-note">${escapeHtml(sentence)}</p>`);
}

function depVerification(state) {
  const data = state.data;
  const available = verificationAvailable(data);
  const current = data.device_verification;
  const pending = state.verification.pending;
  const option = (value, label, disabled) => `<button type="button" class="seg-item" data-dep="verification" data-value="${value}" aria-pressed="${current === value}"${disabled ? ' disabled' : ''}>${escapeHtml(label)}</button>`;
  const sentence = pending ? 'Saving…'
    : !available ? 'Intune verification needs an active Microsoft Entra ID connection, because the check runs in your Entra tenant.'
      : current === 'intune' ? 'A device enrols only with a deployment key, and only if your Intune manages it with a matching serial number.'
        : 'A device enrols with the deployment key in its package. With Intune on, your Intune must also manage it.';
  return depCard('Device verification', `<div class="seg dp-seg" role="group" aria-label="Device verification">`
    + option('none', 'Deployment key only', Boolean(pending))
    + option('intune', 'Key and Intune', Boolean(pending) || !available)
    + '</div>'
    + `<p class="dp-note"${pending ? ' role="status"' : ''}>${escapeHtml(sentence)}</p>`
    + depProblem(state.verification.problem, 'Not changed.'));
}

function depHowTo(data) {
  const code = data.release?.product_code ?? '{product code}';
  const version = data.release?.version ?? 'the release version';
  const unknown = !data.release?.product_code || !data.release?.version
    ? '<p class="dp-note">The server did not report the release, so the product code and version are shown as placeholders. README.txt in the .zip package carries both.</p>'
    : '';
  return '<details class="dp-howto"><summary>Deploy with Microsoft Intune</summary><ol class="dp-steps">'
    + '<li>In the Microsoft Intune admin center, go to <b>Apps</b> → <b>Windows</b> → <b>Add</b> and choose <b>Windows app (Win32)</b>.</li>'
    + '<li>Select the downloaded <code>.intunewin</code> file as the app package file.</li>'
    + `<li>Install command: <code>${escapeHtml(DEP_INSTALL_COMMAND)}</code>. Install behavior: <b>System</b>.</li>`
    + `<li>Uninstall command: <code>msiexec /x ${escapeHtml(code)} /qn</code></li>`
    + `<li>Detection rule: rule type <b>MSI</b>, product code <code>${escapeHtml(code)}</code>, with the product version check on: greater than or equal to <code>${escapeHtml(version)}</code>.</li>`
    + '<li>Assign the app as <b>Required</b> to the device groups that should run the agent.</li>'
    + '</ol>' + unknown + '</details>'
    + '<details class="dp-howto"><summary>Deploy with Configuration Manager, Group Policy or another tool</summary><ol class="dp-steps">'
    + '<li>Unzip the <code>.zip</code> package and keep <code>ShadowAICapture.msi</code> and <code>ShadowAICapture.tenant.env</code> in the same folder: the installer reads the tenant file from beside itself.</li>'
    + `<li>Run <code>${escapeHtml(DEP_INSTALL_COMMAND)}</code> from that folder as SYSTEM or an administrator.</li>`
    + `<li>Detect the installation by the MSI product code <code>${escapeHtml(code)}</code> and version <code>${escapeHtml(version)}</code>. The README.txt in the package says the same.</li>`
    + '</ol></details>';
}

function depDownload(state) {
  const data = state.data;
  const d = state.download;
  const busy = Boolean(d.pending);
  const status = d.pending
    ? `<p class="dp-note" role="status">Building the ${escapeHtml(DEPLOYMENT_FORMATS[d.pending]?.label ?? 'package')}…</p>`
    : d.last
      ? `<p class="dp-done" role="status">Saved <strong>${escapeHtml(d.last.filename)}</strong>. ${d.last.keyLabel
        ? `It carries the deployment key <strong>“${escapeHtml(d.last.keyLabel)}”</strong>.`
        : 'It carries a new deployment key, listed below.'}</p>`
      : '';
  const body = '<p>Each download is a package for your organisation: the agent installer (<code>ShadowAICapture.msi</code>) and, beside it, <code>ShadowAICapture.tenant.env</code>, which says where to enrol and carries a new deployment key. Revoke a key below to stop new enrolments from the packages that carry it.</p>'
    + '<form class="dp-form" data-dep-form="none" autocomplete="off">'
    + '<label class="dp-label" for="dp-package-label">Label for the new deployment key <span class="dp-optional">optional</span></label>'
    + `<input class="dp-input" id="dp-package-label" maxlength="120" data-dep-draft="packageLabel" placeholder="For example: Intune, all laptops" value="${escapeHtml(state.drafts.packageLabel)}">`
    + '<div class="dp-actions">'
    + depButton('Download Intune package (.intunewin)', { dep: 'download', format: 'intunewin' }, { kind: 'primary', disabled: busy })
    + depButton('Download package (.zip)', { dep: 'download', format: 'zip' }, { disabled: busy })
    + '</div></form>'
    + status
    + depProblem(d.problem, 'No package was saved.')
    + depHowTo(data);
  const release = data.release?.version ? `<span class="table-note">release ${escapeHtml(data.release.version)}</span>` : '';
  return depCard('Download the agent', body, { extra: release, wide: true });
}

function depKeyAction(key, state, now) {
  if (deploymentKeyState(key, now) !== 'active') return '';
  if (state.keys.pending === key.key_id) return '<span class="dp-sub" role="status">Revoking…</span>';
  if (state.keys.confirm === key.key_id) {
    return '<span class="dp-confirm">Stop new enrolments with this key? '
      + depButton('Revoke', { dep: 'confirm-revoke-key', id: key.key_id }, { kind: 'danger', small: true })
      + depButton('Cancel', { dep: 'cancel-revoke-key' }, { kind: 'quiet', small: true }) + '</span>';
  }
  return depButton('Revoke', { dep: 'revoke-key', id: key.key_id }, { small: true, disabled: Boolean(state.keys.pending) });
}

function depKeys(state, now) {
  const keys = state.data.deployment_keys;
  const active = keys.filter((k) => deploymentKeyState(k, now) === 'active').length;
  const rows = keys.map((k) => {
    const keyState = deploymentKeyState(k, now);
    const expires = keyState === 'active' && k.expires_at ? `<span class="dp-sub">expires ${escapeHtml(formatInstant(k.expires_at))}</span>` : '';
    return '<tr>'
      + `<td>${k.label ? `<span class="v-text">${escapeHtml(k.label)}</span>` : '<span class="v-absent">no label</span>'}</td>`
      + `<td>${depInstant(k.created_at, 'not reported')}</td>`
      + `<td>${k.created_by ? `<span class="v-text">${escapeHtml(k.created_by)}</span>` : '<span class="v-absent">not reported</span>'}</td>`
      + `<td class="num">${k.enrolment_count === null ? '<span class="v-absent">not reported</span>' : `<span class="v-number">${escapeHtml(formatCount(k.enrolment_count))}</span>`}</td>`
      + `<td>${depInstant(k.last_used_at)}</td>`
      + `<td>${depChip(keyState)}${expires}</td>`
      + `<td class="dp-action">${depKeyAction(k, state, now)}</td>`
      + '</tr>';
  }).join('');
  const empty = keys.length === 0 ? '<tr class="row-empty"><td colspan="7">No deployment keys yet. Downloading a package creates one.</td></tr>' : '';
  const count = keys.length > 0 ? `<span class="block-count">${keys.length}</span><p class="table-note">${escapeHtml(formatCount(active))} of ${escapeHtml(formatCount(keys.length))} active</p>` : '';
  return `<section class="table-block card" id="dp-keys"><div class="card-core"><header class="block-head"><h3>Deployment keys</h3>${count}</header>`
    + '<div class="table-scroll"><table><thead><tr><th scope="col">Label</th><th scope="col">Created</th><th scope="col">Created by</th><th scope="col" class="num">Enrolments</th><th scope="col">Last used</th><th scope="col">State</th><th scope="col"><span class="sr">Action</span></th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`
    + (state.keys.problem ? `<div class="dp-body">${depProblem(state.keys.problem, 'Not revoked.')}</div>` : '')
    + '</div></section>';
}

function depCopied(state, what) {
  if (state.copied?.what !== what) return '';
  return state.copied.ok
    ? '<span class="dp-copied" role="status">Copied</span>'
    : '<span class="dp-copied dp-copied-no" role="status">This browser did not allow copying: select the text and copy it.</span>';
}

function depSecret(state) {
  const created = state.scim.created;
  if (!created) return '';
  return '<div class="dp-secret">'
    + '<p><strong>Copy this token now. It will not be shown again.</strong> Shadow AI Capture keeps only a hash of it. If it is lost, create another and revoke this one.</p>'
    + '<div class="dp-secret-row">'
    + `<input class="dp-input dp-mono" id="dp-token" readonly aria-label="The new SCIM token, ${escapeHtml(created.label)}" value="${escapeHtml(created.token)}">`
    + depButton('Copy token', { dep: 'copy', what: 'token' }, { kind: 'primary' })
    + '</div>'
    + depCopied(state, 'token')
    + `<div class="dp-actions">${depButton('I have copied it', { dep: 'dismiss-token' }, { kind: 'quiet' })}</div>`
    + '</div>';
}

function depTokenAction(token, state) {
  if (token.revoked_at) return '';
  if (state.tokens.pending === token.token_id) return '<span class="dp-sub" role="status">Revoking…</span>';
  if (state.tokens.confirm === token.token_id) {
    return '<span class="dp-confirm">Stop provisioning with this token? '
      + depButton('Revoke', { dep: 'confirm-revoke-token', id: token.token_id }, { kind: 'danger', small: true })
      + depButton('Cancel', { dep: 'cancel-revoke-token' }, { kind: 'quiet', small: true }) + '</span>';
  }
  return depButton('Revoke', { dep: 'revoke-token', id: token.token_id }, { small: true, disabled: Boolean(state.tokens.pending) });
}

function depScim(state) {
  const scim = state.data.scim;
  const baseUrl = scim.base_url ?? state.scim.created?.base_url ?? null;
  const tokens = scim.tokens;
  const live = tokens.filter((t) => !t.revoked_at).length;
  const count = (n) => (n === null ? '<span class="v-absent">not reported</span>' : `<span class="v-number">${escapeHtml(formatCount(n))}</span>`);
  const kv = '<dl class="dp-kv">'
    + `<div><dt>SCIM base URL</dt><dd>${baseUrl ? `<code class="dp-url">${escapeHtml(baseUrl)}</code> ${depButton('Copy', { dep: 'copy', what: 'base_url' }, { small: true })} ${depCopied(state, 'base_url')}` : '<span class="v-absent">not reported</span>'}</dd></div>`
    + `<div><dt>Users provisioned</dt><dd>${count(scim.users)}</dd></div>`
    + `<div><dt>Groups</dt><dd>${count(scim.groups)}</dd></div>`
    + `<div><dt>Last provisioned</dt><dd>${depInstant(scim.last_provisioned_at)}</dd></div>`
    + '</dl>';
  const form = '<form class="dp-form dp-inline" data-dep-form="create-token" autocomplete="off">'
    + '<label class="dp-label" for="dp-scim-label">New token label</label>'
    + '<div class="dp-inline-row">'
    + `<input class="dp-input" id="dp-scim-label" maxlength="120" data-dep-draft="scimLabel" placeholder="For example: Entra provisioning" value="${escapeHtml(state.drafts.scimLabel)}">`
    + depButton(state.scim.pending ? 'Creating…' : 'Create token', { dep: 'create-token' }, { disabled: state.scim.pending })
    + '</div></form>';
  const rows = tokens.map((t) => '<tr>'
    + `<td>${t.label ? `<span class="v-text">${escapeHtml(t.label)}</span>` : '<span class="v-absent">no label</span>'}</td>`
    + `<td>${depInstant(t.created_at, 'not reported')}</td>`
    + `<td>${depChip(t.revoked_at ? 'revoked' : 'active')}${t.revoked_at ? `<span class="dp-sub">${escapeHtml(formatInstant(t.revoked_at))}</span>` : ''}</td>`
    + `<td class="dp-action">${depTokenAction(t, state)}</td>`
    + '</tr>').join('');
  const empty = tokens.length === 0 ? '<tr class="row-empty"><td colspan="4">No SCIM tokens. Create one for your identity provider\'s provisioning settings.</td></tr>' : '';
  const table = '<div class="dp-subhead"><h4>Tokens</h4>'
    + (tokens.length > 0 ? `<span class="table-note">${escapeHtml(formatCount(live))} of ${escapeHtml(formatCount(tokens.length))} active</span>` : '') + '</div>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Label</th><th scope="col">Created</th><th scope="col">State</th><th scope="col"><span class="sr">Action</span></th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`;
  const howto = '<details class="dp-howto"><summary>Set up provisioning in Microsoft Entra ID</summary><ol class="dp-steps">'
    + '<li>In the Microsoft Entra admin center, open <b>Enterprise applications</b>, choose <b>New application</b>, then <b>Create your own application</b> and <b>Integrate any other application you don\'t find in the gallery</b>; name it, for example, Shadow AI Capture provisioning. Sign-in keeps using the Shadow AI Capture application you consented to: Entra does not provision through an application added by consent.</li>'
    + '<li>In the new application, open <b>Provisioning</b> and set the provisioning mode to <b>Automatic</b>.</li>'
    + '<li>Tenant URL: the SCIM base URL above. Secret token: a token created here. Choose <b>Test connection</b>, then save.</li>'
    + '<li>Under <b>Mappings</b>, open the user mapping and map <code>objectId</code> to <code>externalId</code>, so a person a device knows by their Entra object id is matched to their directory entry.</li>'
    + '<li>Assign the users or groups to provision, then turn provisioning on.</li>'
    + '</ol></details>'
    + '<details class="dp-howto"><summary>Other identity providers</summary><p>Any SCIM 2.0 client works: the base URL above, with a token created here as its bearer token. <code>userName</code> is the person\'s sign-in name, their UPN or email address.</p></details>';
  const body = '<p>People and groups can be kept in sync by your identity provider over SCIM 2.0. With Microsoft Entra ID, Sundial can instead read them itself: see <a href="#directory">Directory &amp; teams</a>.</p>'
    + kv + depSecret(state) + form
    + depProblem(state.scim.problem, 'No token was created.')
    + table
    + depProblem(state.tokens.problem, 'Not revoked.')
    + howto;
  return depCard('User provisioning (SCIM)', body, { wide: true, id: 'dp-scim' });
}

const DEP_NOTES = Object.freeze([
  'Every change on this page is written to the audit trail under your name.',
  'A deployment key lets a device enrol into your organisation. It does not let anyone read data.',
  'The counts are what the server reports for your organisation. None is a share of your fleet: the product does not know how many devices you have that it has not seen.',
]);

/**
 * The whole page for one state.
 *
 * @param {object} state  createDeployment().state
 * @param {object} [options]
 * @param {string|null} [options.eyebrow]
 * @param {Date} [options.now]
 */
export function renderDeployment(state, { eyebrow = 'Settings', now = new Date() } = {}) {
  const head = depHeader(eyebrow);
  if (state.status === 'idle' || state.status === 'loading') return `<article class="screen dp" aria-busy="true">${head}${depSkeleton()}</article>`;
  if (state.status === 'refused' || !state.data) return `<article class="screen dp">${head}${depRefused(state.problem)}</article>`;
  const refresh = state.refreshProblem
    ? `<div class="banner banner-warning" role="status"><strong>Not refreshed</strong><span>${escapeHtml(state.refreshProblem.message ?? '')} What is shown is from the last read.</span></div>`
    : '';
  const notes = `<details class="notes"><summary>About these settings<span class="notes-count">${DEP_NOTES.length}</span></summary><ul>${DEP_NOTES.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`;
  return `<article class="screen dp">${head}${refresh}${depTiles(state.data, now)}`
    + `<div class="dp-grid">${depSignIn(state.data)}${depVerification(state)}</div>`
    + depDownload(state)
    + depKeys(state, now)
    + depScim(state)
    + notes
    + '</article>';
}
