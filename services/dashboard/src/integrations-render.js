// integrations-render.js — Settings → Integrations, state to HTML. Pure: no DOM, no I/O, no state.
//
// One card per provider the admin api lists, drawn from its row alone: its name, vendor and tools,
// how it is connected (a key and an account, the tenant's Entra consent, or an intake token) and
// the labels its fields carry. The same rules as the other settings pages: a count the server did
// not send is "not reported", never 0, a failed pull or delivery says which failure it was and who
// can fix it, and every string from an API is escaped. The key field is never given a value; an
// intake token appears only in the card of the action that issued it.

import { escapeHtml, renderPageHead } from './render.js';
import { formatCount } from './format.js';
import { depButton, depCard, depChip, depInstant, depProblem } from './deployment-render.js';
import { DELIVERY_ERRORS, INTAKE_SETUP, PULL_ERRORS, connectKind, isConnected } from './integrations.js';

function intHeader(head) {
  return renderPageHead({
    title: 'Integrations',
    ...head,
    subtitle: 'Connect the AI providers whose records Sundial can read: from a provider\'s admin API every hour, or from the log a provider writes in your cloud, which your cloud sends to Sundial as it is written. Either way Sundial reads each provider\'s own record of the prompts your people sent, with no device in between.',
  });
}

function intRefused(problem) {
  const status = problem?.status;
  const title = status === 401 ? 'Your session has ended'
    : status === 403 ? 'Only an admin can manage integrations'
      : status === 0 ? 'The admin API could not be reached'
        : 'The integrations could not be read';
  const text = status === 401 ? 'Sign in again to continue.' : problem?.message ?? 'Nothing was read.';
  const action = status === 401 ? '<a class="btn" href="/signin">Sign in again</a>' : status === 403 ? '' : depButton('Try again', { int: 'retry' });
  return '<section class="card dp-card"><div class="card-core dp-state" role="alert">'
    + `<h3>${escapeHtml(title)}</h3><p>${escapeHtml(text)}</p>`
    + (problem?.code ? `<p class="dp-code">${escapeHtml(problem.code)}${status ? ` · ${escapeHtml(String(status))}` : ''}</p>` : '')
    + (action ? `<div class="dp-actions">${action}</div>` : '')
    + '</div></section>';
}

/** Where a provider stands: connected, disconnected, never connected, or not readable here. */
function intChip(provider) {
  const [key, text] = isConnected(provider) ? ['active', 'connected']
    : !provider.available ? ['unknown', 'not available']
      : provider.connection ? ['unknown', 'disconnected'] : ['unknown', 'not connected'];
  return `<span class="v-vocab v-vocab-${key}">${text}</span>`;
}

const by = (actor) => (actor ? ` <span class="dp-sub">by ${escapeHtml(actor)}</span>` : '');

/** A pull that started after the last one finished is still running. */
function pullRunning(c) {
  return Boolean(c.last_started_at) && (!c.last_completed_at || Date.parse(c.last_started_at) > Date.parse(c.last_completed_at));
}

function intLastPull(c) {
  if (pullRunning(c)) return `${depInstant(c.last_started_at)} <span class="dp-sub">started, still running</span>`;
  if (c.last_completed_at) return `${depInstant(c.last_completed_at)}${c.last_status ? ` ${depChip(c.last_status)}` : ''}`;
  return '<span class="v-absent">not run yet</span>';
}

function intLastDelivery(c) {
  if (c.last_completed_at) return `${depInstant(c.last_completed_at)}${c.last_status ? ` ${depChip(c.last_status)}` : ''}`;
  return '<span class="v-absent">none yet</span>';
}

function intRecords(c) {
  if (c.records_read === null) return '<span class="v-absent">not reported</span>';
  return `<span class="v-number">${escapeHtml(formatCount(c.records_read))}</span>`
    + (c.records_new === null ? '' : ` <span class="dp-sub">${escapeHtml(formatCount(c.records_new))} of them new to Sundial</span>`);
}

function intFacts(provider) {
  const c = provider.connection;
  const rows = [['Covers', provider.tools.length > 0 ? escapeHtml(provider.tools.map((t) => t.name).join(', ')) : '<span class="v-absent">no tool listed</span>']];
  const intake = provider.credential === 'intake';
  if (isConnected(provider)) {
    if (c.account) rows.push([provider.account_label ?? 'Account', `<code>${escapeHtml(c.account)}</code>`]);
    if (intake) rows.push(['Endpoint', c.intake_url ? `<code class="dp-url">${escapeHtml(c.intake_url)}</code>` : '<span class="v-absent">not reported</span>']);
    rows.push(['Connected', `${depInstant(c.created_at, 'not reported')}${by(c.created_by)}`]);
    if (c.updated_at && c.updated_at !== c.created_at) rows.push(['Last changed', `${depInstant(c.updated_at)}${by(c.updated_by)}`]);
    rows.push(intake ? ['Last delivery', intLastDelivery(c)] : ['Last pull', intLastPull(c)]);
    rows.push(['Records read', intRecords(c)]);
  } else if (c) {
    rows.push(['Disconnected', `${depInstant(c.updated_at, 'not reported')}${by(c.updated_by)}`]);
  }
  return `<dl class="dp-kv">${rows.map(([label, html]) => `<div><dt>${escapeHtml(label)}</dt><dd>${html}</dd></div>`).join('')}</dl>`;
}

function intPullFailed(provider) {
  const c = provider.connection;
  if (!isConnected(provider) || c.last_status !== 'failed') return '';
  const [what, words] = provider.credential === 'intake'
    ? ['delivery', DELIVERY_ERRORS[c.last_error] ?? 'The delivery could not be recorded.']
    : ['pull', PULL_ERRORS[c.last_error] ?? 'The provider could not be read.'];
  return `<p class="dp-problem" role="alert"><strong>The last ${what} failed.</strong> ${escapeHtml(words)}`
    + (c.last_error ? ` <code>${escapeHtml(c.last_error)}</code>` : '') + '</p>';
}

/** What the admin's last action on this provider did. */
function intOutcome(provider, action) {
  const forgotten = { api_key: ' and has forgotten its key', intake: ' and has forgotten its token' }[provider.credential] ?? '';
  const sentence = provider.credential === 'intake' ? {
    connect: 'Connected. Sundial now receives what is sent with the token below.',
    replace: 'Token replaced. The previous token no longer works.',
    disconnect: `Disconnected. Sundial no longer receives ${provider.display_name}${forgotten}.`,
  }[action.done] : {
    connect: action.queued ? 'Connected. The first pull has started: it reads the last 7 days.' : 'Connected. The first pull runs within the hour and reads the last 7 days.',
    replace: action.queued ? 'Key replaced. A pull has started with it.' : 'Key replaced. The next pull uses it.',
    disconnect: `Disconnected. Sundial no longer reads ${provider.display_name}${forgotten}.`,
    run: action.queued ? 'A pull has started.' : 'A pull is already running.',
  }[action.done];
  if (!sentence) return '';
  return `<p class="${action.done === 'run' ? 'dp-note' : 'dp-done'}" role="status">${escapeHtml(sentence)}</p>`;
}

/** How a provider read with the tenant's Entra consent is connected, and what it needs first. */
function intConsentNote(state, provider) {
  if (isConnected(provider)) return '';
  if (!state.data.entra_connected) {
    return `<p class="dp-note">Connecting ${escapeHtml(provider.display_name)} needs an active Microsoft Entra ID connection for your organisation, and there is none. `
      + 'It is linked from the onboarding link your vendor sent; its state is on <a href="#directory">Identity Providers</a>.</p>';
  }
  return `<p class="dp-note">${escapeHtml(provider.display_name)} is read with the consent your organisation gave the Sundial application in Microsoft Entra ID: there is no key to paste.</p>`;
}

function intCopied(action, what) {
  if (action.copied?.what !== what) return '';
  return action.copied.ok
    ? '<span class="dp-copied" role="status">Copied</span>'
    : '<span class="dp-copied dp-copied-no" role="status">This browser did not allow copying: select the text and copy it.</span>';
}

/** The endpoint and the token an intake connection was just given: the only time the token is shown. */
function intIssued(provider, action) {
  const issued = action.issued;
  if (!issued) return '';
  const id = escapeHtml(provider.provider);
  return '<div class="dp-secret">'
    + '<p><strong>Copy this token now. It will not be shown again.</strong> Sundial keeps only a hash of it. If it is lost, replace it.</p>'
    + (issued.url
      ? `<label class="dp-label" for="int-url-${id}">Endpoint</label><div class="dp-secret-row">`
        + `<input class="dp-input dp-mono" id="int-url-${id}" readonly value="${escapeHtml(issued.url)}">`
        + depButton('Copy endpoint', { int: 'copy', what: 'url' }) + '</div>' + intCopied(action, 'url')
      : '')
    + `<label class="dp-label" for="int-token-${id}">Token</label><div class="dp-secret-row">`
    + `<input class="dp-input dp-mono" id="int-token-${id}" readonly value="${escapeHtml(issued.token)}">`
    + depButton('Copy token', { int: 'copy', what: 'token' }, { kind: 'primary' }) + '</div>' + intCopied(action, 'token')
    + `<div class="dp-actions">${depButton('I have copied it', { int: 'cancel' }, { kind: 'quiet' })}</div>`
    + '</div>';
}

function intForm(state, provider) {
  const id = provider.provider;
  const replacing = isConnected(provider);
  const pending = state.action.pending === 'connect';
  const off = pending ? ' disabled' : '';
  const field = (name) => escapeHtml(`int-${name}-${id}`);
  const account = provider.account_label
    ? `<label class="dp-label" for="${field('account')}">${escapeHtml(provider.account_label)}</label>`
      + `<input class="dp-input dp-mono" id="${field('account')}" name="account" maxlength="200" required spellcheck="false" data-int-draft="account" value="${escapeHtml(state.drafts.account)}"${off}>`
    : '';
  const key = `<label class="dp-label" for="${field('key')}">${escapeHtml(provider.credential_label ?? 'Key')}</label>`
    + `<input class="dp-input dp-mono" id="${field('key')}" name="key" type="password" maxlength="4096" required spellcheck="false" data-int-draft="key"${off}>`;
  const submit = pending ? (replacing ? 'Replacing…' : 'Connecting…') : (replacing ? 'Replace key' : 'Connect');
  return `<form class="dp-form" data-int-form="${escapeHtml(id)}" autocomplete="off">${account}${key}`
    + (replacing ? '<p class="dp-note">The new key replaces the one Sundial holds, which is forgotten.</p>' : '')
    + `<div class="dp-actions">${depButton(submit, {}, { kind: 'primary', type: 'submit', disabled: pending })}${depButton('Cancel', { int: 'cancel' }, { kind: 'quiet', disabled: pending })}</div>`
    + '</form>';
}

function intActions(state, provider, mine) {
  const id = provider.provider;
  const kind = connectKind(provider);
  const connected = isConnected(provider);
  const busy = Boolean(state.action.pending);
  if (mine?.confirm) {
    const [verb, forget] = provider.credential === 'intake' ? ['receiving', ' and forget its token'] : ['reading', provider.credential === 'api_key' ? ' and forget its key' : ''];
    return `<div class="dp-actions"><span class="dp-confirm">Stop ${verb} ${escapeHtml(provider.display_name)}${forget}? `
      + depButton('Disconnect', { int: 'confirm-disconnect', provider: id }, { kind: 'danger', small: true })
      + depButton('Cancel', { int: 'cancel' }, { kind: 'quiet', small: true }) + '</span></div>';
  }
  if (mine?.replace) {
    return `<div class="dp-actions"><span class="dp-confirm">Issue a new token? The current one stops working at once, so what ${escapeHtml(provider.display_name)} sends is refused until its sender has the new one. `
      + depButton('Replace token', { int: 'confirm-replace', provider: id }, { kind: 'danger', small: true })
      + depButton('Cancel', { int: 'cancel' }, { kind: 'quiet', small: true }) + '</span></div>';
  }
  if (mine?.form) return intForm(state, provider);
  const buttons = [];
  if (connected && (kind === 'key' || kind === 'consent')) buttons.push(depButton(mine?.pending === 'run' ? 'Starting…' : 'Pull now', { int: 'run', provider: id }, { kind: 'primary', disabled: busy }));
  if (kind === 'key') buttons.push(depButton(connected ? 'Replace key' : 'Connect', { int: 'open', provider: id }, { kind: connected ? '' : 'primary', disabled: busy }));
  if (kind === 'consent' && !connected) {
    buttons.push(depButton(mine?.pending === 'connect' ? 'Connecting…' : 'Connect', { int: 'connect', provider: id }, { kind: 'primary', disabled: busy || !state.data.entra_connected }));
  }
  if (kind === 'intake') {
    const pending = mine?.pending === 'connect';
    buttons.push(connected
      ? depButton(pending ? 'Replacing…' : 'Replace token', { int: 'replace', provider: id }, { disabled: busy })
      : depButton(pending ? 'Connecting…' : 'Connect', { int: 'connect', provider: id }, { kind: 'primary', disabled: busy }));
  }
  if (connected) buttons.push(depButton(mine?.pending === 'disconnect' ? 'Disconnecting…' : 'Disconnect', { int: 'disconnect', provider: id }, { kind: 'quiet', disabled: busy }));
  return buttons.length > 0 ? `<div class="dp-actions">${buttons.join('')}</div>` : '';
}

function intCard(state, provider) {
  const mine = state.action.provider === provider.provider ? state.action : null;
  const kind = connectKind(provider);
  const docs = kind && provider.source_url
    ? `<p class="dp-note"><a href="${escapeHtml(provider.source_url)}" target="_blank" rel="noopener noreferrer">${{ key: 'How to issue a key', consent: 'How it is read', intake: 'How to turn on its log' }[kind]}</a></p>`
    : '';
  const setup = kind === 'intake' && INTAKE_SETUP[provider.intake] ? `<p class="dp-note">${escapeHtml(INTAKE_SETUP[provider.intake])}</p>` : '';
  const body = intFacts(provider)
    + intPullFailed(provider)
    + (kind ? '' : '<p class="dp-note">Not available on this deployment.</p>')
    + (kind === 'consent' ? intConsentNote(state, provider) : '')
    + (mine ? intOutcome(provider, mine) : '')
    + (mine ? intIssued(provider, mine) : '')
    + setup
    + intActions(state, provider, mine)
    + (mine ? depProblem(mine.problem) : '')
    + docs;
  const extra = `<span class="table-note">${provider.vendor ? escapeHtml(provider.vendor) : ''}${intChip(provider)}</span>`;
  return depCard(provider.display_name, body, { extra });
}

function intProviders(state) {
  const providers = state.data.providers;
  if (providers.length === 0) {
    return depCard('Providers', '<p class="dp-empty">No provider is listed yet. Each AI provider whose admin API Sundial can read appears here once it is added.</p>', { wide: true });
  }
  return `<div class="dp-grid">${providers.map((p) => intCard(state, p)).join('')}</div>`;
}

const INT_NOTES = Object.freeze([
  'Every connection, replaced key or token, disconnection and pull you start is written to the audit trail under your name.',
  'A key is sent once, when you connect, and kept sealed. Sundial never shows it again: to change it, replace it with a new one.',
  'Sundial pulls each provider connected with a key or consent every hour and right after it is connected. A connection\'s first pull reads the last 7 days.',
  'A provider with no admin API writes its log in your cloud, and your cloud sends it to the endpoint shown with the token issued when you connect. Sundial keeps only a hash of the token and shows it once.',
  'A prompt a device also captured is counted once: the provider\'s record joins the device\'s.',
]);

/**
 * The whole page for one state.
 *
 * @param {object} state  createIntegrations().state
 * @param {object} [options]
 * @param {object} [options.head]  the page's title, eyebrow and tabs
 */
export function renderIntegrations(state, { head = {} } = {}) {
  const top = intHeader(head);
  if (state.status === 'idle' || state.status === 'loading') {
    return `<article class="screen dp" aria-busy="true">${top}<p class="sr" role="status">Loading the integrations…</p></article>`;
  }
  const refresh = state.refreshProblem
    ? `<div class="banner banner-warning" role="status"><strong>Not refreshed</strong><span>${escapeHtml(state.refreshProblem.message ?? '')} What is shown is from the last read.</span></div>`
    : '';
  const notes = `<details class="notes"><summary>About these settings<span class="notes-count">${INT_NOTES.length}</span></summary><ul>${INT_NOTES.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`;
  const body = state.status === 'refused' || !state.data ? intRefused(state.problem) : `${refresh}${intProviders(state)}`;
  return `<article class="screen dp">${top}${body}${notes}</article>`;
}
