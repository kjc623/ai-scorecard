# DSH browser/client plugin API — verified reference (runtime 0.2.0-rc.2, cordis 4.0.4)

All facts below were read out of real files. Paths are given in full or as `<SRC>` / `<ASAR>`:

- `<SRC>` = `C:\Users\kyle\Downloads\architecture\.tools\dsh-src\dsh\node_modules\@deepseek-ai\`
- `<ASAR>` = `C:\Users\kyle\Downloads\architecture\.research\asar-all\` — 64 `dsh-client-*` / `dsh-experimental-client-*` packages extracted out of
  `C:\Users\kyle\AppData\Local\Programs\DeepSeek Harness\resources\app.asar` → `dsh/node_modules/@deepseek-ai/…`
  (these are the packages the running desktop harness actually boots; `asar-all/<pkg>/lib/client.js` is the real built bundle)
- `<MKT>` = `C:\Users\kyle\.dsh\profiles\desktop\node_modules\dshmarket\`

Anything I could not find is marked **NOT FOUND**.

---

## 1. Client plugin bundle format

### 1.1 The wrapper — exact shape

Every built client bundle is one classic (non-module) script whose entire body is a single
`window.__ModuleLoader__.load({ id, factory })` call.

Verbatim head of `<MKT>\client\client.js` (line 1):

```js
window.__ModuleLoader__.load({ id: "dshmarket", factory: (require) => {
```

Verbatim head of `<SRC>dsh-experimental-client-ui-agent-team\lib\client.js` (lines 1-10):

```js
window.__ModuleLoader__.load({
	id: "@deepseek-ai/dsh-experimental-client-ui-agent-team",
	factory: (require) => {
		var module = { exports: {} };
		var exports = module.exports;
		Object.defineProperty(exports, Symbol.toStringTag, { value: "Module" });
		let react_jsx_runtime = require("react/jsx-runtime");
		let react = require("react");
		let react_dom = require("react-dom");
		let _deepseek_ai_dsh_client_ui_primitives = require("@deepseek-ai/dsh-client-ui-primitives");
```

Verbatim tail of `<MKT>\client\client.js`:

```js
		exports.REQUIRED_PRIMITIVES = REQUIRED_PRIMITIVES;
		exports.apply = apply;
		exports.inject = inject;
		exports.missingPrimitives = missingPrimitives;
		exports.name = name;
		return module.exports;
	}
});
```

**The registration object is exactly `{ id, factory }` in every bundle I read.** There is a third optional
field, `chunk`, used only by tsdown-split sub-chunks (`<SRC>dsh-client-modules\lib\client.js:571`):

```js
register(registration) {
	const ownerId = stripClientSuffix(registration.id);
	if (registration.chunk !== void 0 && !CLIENT_CHUNK.test(registration.chunk)) throw new Error(`client-modules: invalid package-local chunk ${JSON.stringify(registration.chunk)}`);
	const id = registration.chunk === void 0 ? ownerId : chunkId(ownerId, registration.chunk);
```

`CLIENT_CHUNK = /^client\.[A-Za-z0-9][A-Za-z0-9._-]*\.js$/` (`<SRC>dsh-client-modules\lib\index.js:169`).

### 1.2 How `id` is used

`stripClientSuffix()` (`<SRC>dsh-client-modules\lib\client.js:98-100`) removes a trailing `/client`:

```js
function stripClientSuffix(spec) {
	return spec.endsWith("/client") ? spec.slice(0, -7) : spec;
}
```

so `"dshmarket"`, `"dshmarket/client"`, and the bare package name are **the same module identity**.
`register()` throws on a duplicate registration:
`client-modules: duplicate factory registration for "…" (bundle executed twice without invalidate?)`.
`id` must equal the installing package's `package.json` `name` (`ClientModuleRegistry.resolveMeta`,
`<SRC>dsh-client-modules\lib\index.js:713-727`).

**Exports shape matters:** the loader does `registered.factory(this.makeRequire(ownerId, edges))` and uses the
**return value** as the module exports (`<SRC>dsh-client-modules\lib\client.js:683`). Both styles work:
`exports.apply = apply` (CJS-ish, what all real bundles use) or `export default`, because the host reads the
resolved entry as a cordis plugin object. Every real bundle uses named exports `apply` + `inject`.

### 1.3 `require()` resolution — exact module table

```js
makeRequire(ownerId, edges) {
	const require = (spec) => {
		edges.add(spec);
		if (this.seed.has(spec)) return this.seed.get(spec);
		const id = stripClientSuffix(spec);
		const record = this.loadCache.get(id);
		if (record !== void 0) return record.exports;
		if (this.factories.has(id)) return this.materialize(id).exports;
		throw new Error(`client-modules: require("${spec}") missed the module table — not a platform seed word, not a materialized module, and no registered package factory (a build-time externals drift, or a dynamic dependency that did not arrive)`);
	};
	require.async = async (spec) => { … };
	return require;
}
```
(`<SRC>dsh-client-modules\lib\client.js:697-715`)

**Resolution is exact-string.** There is no prefix/subpath fallback except the single `stripClientSuffix`
rule above. `require.async("./client.<name>.js")` is the only relative form (package-local chunks).

**The frozen platform seed table (the complete list), verbatim from the shipped web shell**
`<ASAR>dsh-web-frontend\dist\assets\index-5SrrfWpU.js` (function `rM()`):

```js
function rM(){return{
  react:Ef,
  "react/jsx-runtime":If,
  "react-dom":Rf,
  "react-dom/client":Df,
  "@deepseek-ai/cordis":sf,
  "@deepseek-ai/dsh-client-store":lh,
  "@deepseek-ai/dsh-client-ui-slots":hh,
  "@deepseek-ai/dsh-client-ui-primitives":sE,
  "@deepseek-ai/dsh-client-ui-dockkit":XS}}
```

> **IMPORTANT — two conflicts a hand-written plugin must know about:**
> 1. `@deepseek-ai/dsh-client-ui-slots` **is** a seed word, but `@deepseek-ai/dsh-client-ui-theme`,
>    `@deepseek-ai/dsh-client-locale`, `@deepseek-ai/dsh-client-connection` and
>    `@deepseek-ai/dsh-client-ui-renderer` are **NOT** — they are dynamic rows, so requiring them works only
>    if the row already arrived. `dshmarket` requires exactly four specifiers and nothing else:
>    `@deepseek-ai/dsh-client-ui-primitives`, `react`, `react/jsx-runtime`, `react-dom`.
> 2. `@deepseek-ai/dsh-client-ui-primitives` **is not a seed word either** — it is a dynamic package row. It
>    resolves because the consumer's `dsh.client.external` names it (see §1.5) and the host orders it first.
> 3. `@deepseek-ai/dsh-client-runtime` (a `devDependencies` entry in `<MKT>\package.json:85`) is
>    **NOT FOUND** in the boot graph or the seed table — it is a type/dev-only name, not a runtime module.

`dshmarket`'s primitive typing file states the same, `<MKT>\src\client\primitives.d.ts:2-6`:
> "provided at runtime by the host's frozen platform module table (packages/client/web/src/platform.ts). The
> published npm package is only a dev-time mirror used by the jsdom test lane, so the browser bundle stays
> external (see tsdown.config.ts CLIENT_EXTERNALS)"

### 1.4 CSS injection — exact mechanism

**CSS is a plain string inside the factory closure, injected with a `<style>` tag.** No CSS-module loader,
no `adoptedStyleSheets`, no loader-side CSS API. Verbatim from
`<SRC>dsh-experimental-client-ui-agent-team\lib\client.js:11-20`:

```js
		//#region \0dsh-css:D:\develop\dsh-harness-windows-x64\packages\experimental\client-ui-agent-team\src\client\TeamAction.module.css.mjs
		const css = ".EBLgjq_root{position:relative}.EBLgjq_trigger{…}";
		const tagId = "@deepseek-ai/dsh-experimental-client-ui-agent-team/TeamAction.module.css";
		if (typeof document !== "undefined" && document.querySelector("style[data-plugin-css=" + JSON.stringify(tagId) + "]") === null) {
			const tag = document.createElement("style");
			tag.dataset.plugin = "@deepseek-ai/dsh-experimental-client-ui-agent-team";
			tag.dataset.pluginCss = tagId;
			tag.textContent = css;
			document.head.appendChild(tag);
		}
		var TeamAction_module_css_default = {
			"body": "EBLgjq_body",
			"panel": "EBLgjq_panel",
			…
		};
```

`<MKT>\client\client.js:2851-2861` is character-for-character the same pattern with
`tag.dataset.plugin = "dshmarket"` and `tagId = "dshmarket/Market.module.css"`.

The host only *adopts* these tags for HMR bookkeeping — `<SRC>dsh-client-modules\lib\client.js:492-498`:

```js
const claimStyles = (id) => {
	if (typeof document === "undefined") return [];
	for (const el of document.querySelectorAll("style:not([data-plugin])")) el.setAttribute("data-plugin", id);
	const owned = [];
	for (const el of document.querySelectorAll(`style[data-plugin=${JSON.stringify(id)}]`)) owned.push(el.getAttribute("data-plugin-css") ?? id);
	return owned;
};
```
and removes them on unload/replace via `removeOwnedStyles(id)` (line 194-197):
`document.querySelectorAll("style[data-plugin]")` where `data-plugin === id`.

**So the two hard requirements are:** the tag carries `data-plugin="<package name>"`, and the class names in
the string match the lookup object you use at render time. Everything else is free.

### 1.5 `dsh.client` in `package.json` — every field the host reads

Validator `parseDshClient()` — `<SRC>dsh-client-modules\lib\client.js:61-75`:

```js
function parseDshClient(pkgName, value) {
	if (value === void 0) return void 0;
	if (typeof value !== "object" || value === null) throw new Error(`client-modules: ${pkgName} has a non-object dsh.client declaration`);
	const decl = value;
	if (typeof decl.platform !== "string") throw new Error(`client-modules: ${pkgName} dsh.client.platform must be a string`);
	const inject = optionalStringArray(pkgName, "dsh.client.inject", decl.inject);
	const external = optionalStringArray(pkgName, "dsh.client.external", decl.external);
	if (decl.immediately !== void 0 && typeof decl.immediately !== "boolean") throw new Error(`client-modules: ${pkgName} dsh.client.immediately must be a boolean`);
	return {
		platform: decl.platform,
		...inject !== void 0 ? { inject } : {},
		...external !== void 0 ? { external } : {},
		...decl.immediately !== void 0 ? { immediately: decl.immediately } : {}
	};
}
```

Consumption — `<SRC>dsh-client-modules\lib\index.js:712-727`:

```js
const dsh = pkg.dsh;
const decl = parseDshClient(packageName, dsh !== null && typeof dsh === "object" ? dsh.client : void 0);
if (decl === void 0 || decl.platform !== "web") { this.pkgMeta.set(sourceKey, null); return null; }
const clientRel = clientExportOf(packageName, pkg.exports);
if (clientRel === void 0) throw new Error(`client-modules: ${packageName} declares dsh.client but exports no "./client" bundle`);
const resolved = {
	packageName,
	meta: {
		clientPath: join(dirname(pkgPath), clientRel),
		...decl.inject !== void 0 ? { inject: decl.inject } : {},
		external: decl.external ?? [],
		immediately: decl.immediately === true
	}
};
```

| field | type | meaning | default | read at |
|---|---|---|---|---|
| `dsh.client.platform` | string, **must be `"web"`** | gate: any other value = not a client package | required | client.js:65 / index.js:714 |
| `dsh.client.inject` | `string[]` | **package names** of other client plugins that must arrive and load before this one | `[]` | client.js:66, index.js:724 |
| `dsh.client.external` | `string[]` | exact extra `require()` specifiers this bundle uses; each must be another client package row (`<pkg>` or `<pkg>/client`) or an exact static-table key | `[]` | client.js:67, index.js:725 |
| `dsh.client.immediately` | boolean | schedule this row in the earliest batch | `false` | client.js:68, index.js:726 |
| `dsh.bundle.patch` | string | path to a cordis patch file (host half) | — | see `<MKT>\package.json:48-50` |

**`entry` and `export`-mapping fields inside `dsh.client` DO NOT EXIST** — **NOT FOUND**. The bundle path is
taken *only* from `exports["./client"]`:

```js
function clientExportOf(pkgName, exportsField) {
	if (typeof exportsField !== "object" || exportsField === null) return void 0;
	const client = exportsField["./client"];
	if (client === void 0) return void 0;
	if (typeof client === "string") return client;
	if (typeof client === "object" && client !== null) {
		const fallback = client.default;
		if (typeof fallback === "string") return fallback;
	}
	throw new Error(`client-modules: ${pkgName} exports["./client"] must be a string or an object with a string default`);
}
```
(`<SRC>dsh-client-modules\lib\index.js:171-181`)

Real examples:

`<MKT>\package.json:47-59` (hand-written bundle, no tsdown client types):
```json
"dsh": {
  "bundle": { "patch": "./cordis.patch.yml" },
  "client": {
    "inject": [
      "@deepseek-ai/dsh-client-locale",
      "@deepseek-ai/dsh-client-ui-settings",
      "@deepseek-ai/dsh-client-ui-theme"
    ],
    "platform": "web"
  }
}
```
`<MKT>\package.json:60-73`:
```json
"exports": {
  ".": { "types": "./lib/types/index.d.ts", "default": "./lib/index.js" },
  "./client": "./client/client.js",
  "./cordis.patch.yml": "./cordis.patch.yml",
  "./package.json": "./package.json",
  "./locale/*.json": "./locale/*.json"
}
```

`<SRC>dsh-experimental-client-ui-agent-team\package.json`:
```json
"dsh": {
  "client": {
    "inject": [
      "@deepseek-ai/dsh-api-session-controller",
      "@deepseek-ai/dsh-client-locale",
      "@deepseek-ai/dsh-client-ui-conversation",
      "@deepseek-ai/dsh-client-ui-primitives",
      "@deepseek-ai/dsh-client-ui-workspace"
    ],
    "platform": "web"
  }
}
```
with `"./client": { "types": "./lib/types/client/index.d.ts", "default": "./lib/client.js" }`.

Note `dshmarket` deliberately mixes the two injection vocabularies: `dsh.client.inject` names **packages**
(boot-graph ordering), while `export const inject = ['slots','locale','theme']` inside the bundle names
**client services** (cordis DI). They are unrelated.

### 1.6 Serving under `/plugins`

`PLUGIN_ROUTE = "/plugins"` (`<SRC>dsh-client-modules\lib\index.js:201`). Route registrations
(`<SRC>dsh-client-modules\lib\index.js:545-552`):

```js
const registerWebCarrier = (webCtx) => {
	webCtx.effect(() => webCtx.webServer.register({
		kind: "prefix",
		path: PLUGIN_ROUTE,
		handler: this.serveBundle
	}), "client-modules: bundle route");
};
ctx.inject(["webServer"], registerWebCarrier);
ctx.on("webserver/index-inject", (table) => { table.push(...bootInjections(this.composed)); });
```

Exact URL forms:

| what | URL | source |
|---|---|---|
| one plugin, one resource | `/plugins/<pkgId>/client.js?rev=<rev>` | `buildCombo([record], …, record.entry.rev)`, index.js:670 |
| combo (startup batch, N plugins) | `/plugins/??<id1>/client.js,<id2>/client.js&rev=<rev>` | `comboSearch()`, index.js:203-204 |
| source map | same + `.map` before `&rev=` | index.js:204 |
| package-local chunk | `/plugins/<pkgId>/client.<name>.js?rev=<rev>` | `chunkUrl()`, index.js:220-222 |

Headers on a hit (`bundleResource`, index.js:963-970):
`content-type: text/javascript; charset=utf-8` (or `application/json` for maps) and
`cache-control: public, max-age=31536000, immutable`. Unknown URL/revision → 404; non-GET/HEAD → 405.
Revision `rev` = 12-hex sha1 over `mtimeMs`, `ctimeMs`, `size` (`artifactRevision`, index.js:193-198) — so
**rewriting `client.js` alone changes the rev**.

### 1.7 Can a hand-written (not tsdown-built) `client.js` be valid? — YES

The loader parses only the `__ModuleLoader__.load({...})` call. There is **no build-artifact check**, no
sourcemap requirement (`SOURCE_MAP_TRAILER` is only *stripped* when composing combos), and no ESM syntax
requirement — the script is loaded with `<script async src>` and plain `document.head.append`
(`defaultLoadBundle`, client.js:451-464).

Minimum valid file, derived strictly from the loader code:

```js
window.__ModuleLoader__.load({
  id: "my-plugin",                       // MUST equal package.json "name"
  factory: (require) => {
    var module = { exports: {} };
    var exports = module.exports;
    let react = require("react");
    let jsx = require("react/jsx-runtime");
    let P = require("@deepseek-ai/dsh-client-ui-primitives");   // only if declared in dsh.client.external

    const css = ".myThing{color:var(--dsw-alias-label-primary)}";
    if (typeof document !== "undefined" &&
        document.querySelector('style[data-plugin-css="my-plugin/x.css"]') === null) {
      const tag = document.createElement("style");
      tag.dataset.plugin = "my-plugin";
      tag.dataset.pluginCss = "my-plugin/x.css";
      tag.textContent = css;
      document.head.appendChild(tag);
    }

    function apply(ctx) { /* ctx.slots.inject(...) etc. */ }
    const inject = ["slots"];             // client SERVICE names, not package names

    exports.apply = apply;
    exports.inject = inject;
    return module.exports;
  }
});
```

Requirements: (a) `id` equals the package `name`; (b) the factory **returns** the exports object;
(c) every `require()` specifier is a seed word or an exactly-named `dsh.client.external` entry;
(d) `exports.apply` and (optionally) `exports.inject` exist. `dshmarket` is itself proof: its
`client/client.js` is a tsdown output, but `scripts/normalize-client-banner.mjs` post-processes the banner —
the wrapper is the only contract.

---

## 2. Client plugin entry

### 2.1 Signature

```js
export function apply(ctx) { … }
export const inject = ['slots', 'locale', 'theme']   // optional; client service names
export const name = 'dsh-market'                     // optional
```

`<MKT>\src\client\index.ts:95-100`:

```ts
export const name = 'dsh-market'
// 'theme' is safe to require: ui-layout (mandatory in every web composition)
// already hard-depends on it. This cordis's object-form inject means
// intercept config, NOT {required,optional} — do not use it here.
export const inject = ['slots', 'locale', 'theme']
export function apply(ctx: MarketClientContext): void {
```

There is **no `default` export** in any real client bundle.

From `<SRC>dsh-experimental-client-ui-agent-team\lib\client.js:506-512`:

```js
function apply(ctx) {
	registerAgentTeamUi(ctx);
}
…
exports.apply = apply;
exports.inject = inject;
```

### 2.2 How `ctx` is typed

**There is NO published `declare module` augmentation that gives `ctx.slots` / `ctx.connection` /
`ctx.sessions`.** I verified this three ways:

- `<SRC>dsh-client-ui-slots\lib\` contains **only `index.js`** — no `.d.ts`.
- The asar's `dsh-client-ui-slots` package ships `lib/index.js` only (no `lib/types`), and neither does
  `dsh-client-ui-renderer` (`lib/client.js`, `lib/index.js`, `lib/invariant.js`).
- `<SRC>dsh-client-ui-slots\README.md:56` says the map is "declared empty here and merged by consumers via
  `declare module` augmentation" — but those consumer `.d.ts` files are **not shipped in the npm packages or
  the asar**.

**What real plugin authors do instead:** declare the *structural subset* they touch. `<MKT>\src\client\index.ts:78-93`:

```ts
/** The subset of the slots service this plugin touches. */
interface SlotsService {
  inject(slot: string, register: () => unknown): void
  register(meta: Record<string, unknown>, component: () => unknown): unknown
}

/** The client cordis context shape this plugin relies on (structural: the
 * host provides the real Context; typing the touched surface keeps this
 * external package free of monorepo-internal type dependencies). */
interface MarketClientContext {
  effect(callback: () => unknown, label?: string): void
  on(event: string, callback: () => void): () => void
  locale: LocaleService
  slots: SlotsService
  theme: ThemeService
}
```

and separately re-declare the primitives module: `<MKT>\src\client\primitives.d.ts:10`
`declare module '@deepseek-ai/dsh-client-ui-primitives' { … }` (comment lines 7-9 say "keep these signatures
in sync with deepseek-harness/packages/client/ui-primitives/src/ @0.1.0-rc.6. Only the members this plugin
uses are declared.").

Also `<MKT>\src\client\globals.d.ts:3-13`:

```ts
declare module '*.module.css' {
  const classes: Record<string, string>
  export default classes
}

interface Window {
  /** Boot manifest written by the host page for bundle-layer plugins. */
  __DSH_BOOT__?: { entries?: Array<{ id: string }> }
}
```

**Conclusion: the services are obtained by `inject: [...]` at runtime, and typed by hand.** The real service
names in use are (grepped from all extracted client bundles):
`loader, slots, sessions, uiSession, uiConversation, uiWorkspace, uiRenderer, locale, theme, connection,
remote, remote.<namespace>, layout, shortcuts, configForms, settingsScope (≤0.1.6) / settings (0.1.7+),
clientModules/modules, sidebarRight, panelNavigation, commandUi, jobs, webTerminals, productAnalytics`.

### 2.2a Live client Inspect providers exist (but were unreachable here)

`cordis_inspect_list` reports four **client** providers that would answer these questions against the live
page: `client/Slots.listSubTree` ("Progressive live Slot inspection with explicit Slot and Factory topology
nodes"), `client/Service.listService`, `client/Event.listEvents`, `client/Builtin.listBuiltins`
("Plain-JavaScript symbols available to a dynamic Client half"), and `client/Theme.listTokens`
("Current theme token names and light/dark override requirements"). **Every query timed out after 10000 ms
with `Open or reconnect the Harness page, then retry`**, so the live slot tree could not be read in this
session. When the Harness page is open, `client/Slots.listSubTree` is the authoritative way to enumerate the
real slot catalog on a given host — much better than the reconstruction in §3.

### 2.3 The exact `inject` / `register` calls

`ctx.slots.inject(slotKey, register)` — *wait for the slot to be declared, then run `register`*. The
callback's return value is used as the disposer (`subscribeDeclaration` semantics), and a **generator
function** is also accepted for registering several entries.

`ctx.slots.register(meta, Component)` — *claim one cell of the slot*. `meta` is
`{ name, id?, key?, order?, priority?, label?, locale?, inject?, children?, store?, select? }`.

From `<SRC>dsh-experimental-client-ui-agent-team\lib\client.js:457-497` (complete, verbatim):

```js
		const inject = [
			"sessions",
			"uiWorkspace",
			"slots",
			"locale"
		];
		/**
		* Register the Team locale dictionaries and the conversation-header action.
		* The panel reads the Lead Session's `agentTeam` projection from the shared
		* Session store; this registration performs no Team RPC.
		* @param ctx - Client Context carrying the injected navigation, locale, slot, and Session services.
		*/
		function registerAgentTeamUi(ctx) {
			ctx.effect(() => ctx.locale.register(NS, {
				zh,
				en
			}), "client-ui-agent-team: dictionaries");
			const sessions = ctx.sessions;
			const leadSessionId = (sessionId) => {
				return (sessions.binding(sessionId)?.session.getSnapshot().subagent?.address)?.parentSessionId ?? sessionId;
			};
			const actions = { openTeammate(sessionId, childSessionId) {
				const parentSessionId = leadSessionId(sessionId);
				if ((sessions.retainInfo(sessionId).getSnapshot().retainedBy.mainView ?? 0) === 0) return;
				if (childSessionId === parentSessionId) {
					ctx.uiWorkspace.openSession(parentSessionId);
					return;
				}
				ctx.uiWorkspace.openSession({
					parentSessionId,
					childSessionId,
					mode: "continuable"
				});
			} };
			ctx.slots.inject("conversation.session.header.actions", () => ctx.slots.register({
				name: "conversation.session.header.actions",
				id: "agent-team",
				order: -20,
				locale: NS,
				inject: () => actions
			}, TeamAction));
		}
```

From `<MKT>\src\client\index.ts` — the four real injection shapes:

```ts
// 1. list slot, root scope, label + order
const sectionGate = createSectionGate(() => {
  const off = ctx.slots.register({
    name: 'settings.section',
    id: 'market',
    order: 40,
    label: () => t('nav'),
    locale: NS,
    inject: () => ({ t }),
  }, (ownerProps: { preferredSubsectionId?: string } = {}) => buildMarketElement(ownerProps))
  return typeof off === 'function' ? off as () => void : () => {}
})
ctx.slots.inject('settings.section', () => { sectionGate.available() })
```
```ts
// 2. a NESTED cordis inject on the client context, then a keyed slot
settingsCtx.inject(['settingsScope'], (scoped) => {
  scoped.slots.inject('settings.plugin.item', () => scoped.slots.register({
    name: 'settings.plugin.item',
    key: NS,
    locale: NS,
    inject: () => ({ t }),
  }, () => h(SettingsCard, { t, onRemoved: () => { sectionGate.retire() } })))
})
```
```ts
// 3. keyed slot whose key is the package name
bundleConfigCtx.slots.inject('plugins.bundle.config', () => bundleConfigCtx.slots.register({
  name: 'plugins.bundle.config',
  key: MARKET_PACKAGE_NAME,          // 'dshmarket'
  locale: NS,
  inject: () => ({ t }),
}, (ownerProps: { view?: string } = {}) => ownerProps.view === 'summary' ? null : h(SettingsCard, {...})))
```
```ts
// 4. overlay list slot, no locale, no inject
const Toast = () => h(InstallToast, { t })
ctx.slots.inject('shell.overlay', () => ctx.slots.register({
  name: 'shell.overlay',
  id: 'dsh-market-toast',
  label: () => 'dsh-market',
}, Toast))
```

Real generator form (multiple entries at once), `<ASAR>dsh-client-ui-workspace\lib\client.js`:

```js
ctx.slots.inject("sidebar.workspaces.session.menu.item", function* () {
	yield ctx.slots.register({ name: "sidebar.workspaces.session.menu.item", id: "pin",     order: 100, locale: NS, inject: pinInjected     }, PinSessionMenuItem);
	yield ctx.slots.register({ name: "sidebar.workspaces.session.menu.item", id: "rename",  order: 200, locale: NS, inject: renameInjected  }, RenameSessionMenuItem);
	yield ctx.slots.register({ name: "sidebar.workspaces.session.menu.item", id: "fork",    order: 300, locale: NS, inject: forkInjected    }, ForkSessionMenuItem);
	yield ctx.slots.register({ name: "sidebar.workspaces.session.menu.item", id: "archive", order: 400, locale: NS, inject: archiveInjected }, ArchiveSessionMenuItem);
});
```

**Component props come from two places:** the `inject` callback (scope `(sessionId)` → arbitrary object,
optionally `{ hooks: {...}, ...actions }`), merged with the framework's standard kit. So
`function MyPanel({ sessionId, useSession, useSessions, t, openTeammate }) {…}`.

**Validating host availability before registering** — `<MKT>\src\client\index.ts:31-35,108-117`:

```ts
export const REQUIRED_PRIMITIVES = ['Menu', 'DisclosureRow', 'Tooltip', 'Toast'] as const
export function missingPrimitives(mod: Record<string, unknown>, required: readonly string[] = REQUIRED_PRIMITIVES): string[] {
  return required.filter(name => mod[name] === undefined)
}
…
  const mod = primitives as unknown as Record<string, unknown>
  const gaps = missingPrimitives(mod)
  if (gaps.length > 0) {
    console.warn('[dsh-market] host ui-primitives missing ' + gaps.join(', ') + ' — market section disabled (dsh web >= 0.1.0-rc.6 required)')
    return
  }
```

### 2.4 `register` validation rules (what throws)

`SlotCore.register` (`<SRC>dsh-client-ui-slots\lib\index.js:163-190`):

```js
register(options, component) {
	const rec = this.records.get(options.name);
	if (!rec?.spec) throw new Error(`slot "${options.name}" is not declared (a parent entry's children table must declare it)`);
	…
	case "single": { if (occupant) throw new Error(`single slot "${options.name}" already has a registration …`); }
	case "keyed":  { if (options.key === void 0) throw new Error(`keyed slot "${options.name}" requires options.key`); … }
	case "list":   { if (options.id === void 0) throw new Error(`list slot "${options.name}" requires options.id`); … }
	case "chain":  { if (options.select === void 0) throw new Error(`chain slot "${options.name}" requires options.select`); … }
```

So: **`single` → no `id`/`key`; `list` → `id` required; `keyed` → `key` required; `chain` → `select`
required.** Lists sort by `(priority, order)` ascending; for non-list kinds only `priority` matters and a
later registration at the same priority throws (use a different `priority` to shadow — "lowest renders").

Slot scope is **not** passed in `register`; it comes from the declaration (`scope: "root" | "session" |
"session-maybe"`). And `ctx.slots.inject(key, …)` is required to have arrived: registering into an
undeclared slot throws.

---

## 3. Slot catalog

**Source of truth problem:** `<SRC>dsh-client-ui-slots\lib\index.js` declares an **empty** `SlotMap`
(augmented by consumers whose `.d.ts` files are not shipped). The real catalog is therefore reconstructed
from `children:` tables in the shipped client bundles. The table below is complete for the packages present
in the running desktop harness's `app.asar` (64 `dsh-client-*` packages).

Legend: **kind** = single / list / keyed / chain · **scope** = root / session / session-maybe.

### 3.1 The root frame — `<ASAR>dsh-client-ui-layout\lib\client.js`

```js
ctx.slots.register({
	name: "root",
	locale: "common",
	children: {
		"sidebar":       { kind: "single", scope: "root" },
		"main":          { kind: "keyed",  scope: "root" },
		"rightbar":      { kind: "single", scope: "root" },
		"shell.overlay": { kind: "list",   scope: "root" },
		"shell.leading": { kind: "single", scope: "root" }
	},
	store
}, AppFrame);
```

| key | kind | scope | declared in | purpose |
|---|---|---|---|---|
| `root` | single | root | ui-layout | the whole app frame; sole occupant |
| `main` | keyed | root | ui-layout | main panel; key `"conversation"` is taken by ui-conversation |
| `sidebar` | single | root | ui-sidebar | left sidebar column |
| `rightbar` | single | root | ui-layout (occupant: ui-sidebar-right) | right sidebar column |
| `shell.overlay` | list | root | ui-layout | **full-viewport overlay layer — dialogs, toasts, portals** |
| `shell.leading` | single | root | ui-layout (occupant: ui-sidebar HeaderLeadingControls) | top-left header area |

**Occupied SINGLE slots are still injectable.** `dsh-client-ui-plugin-manager` and
`dsh-client-ui-sidebar-right` both claim `main`, and `dsh-client-ui-sidebar-right` claims `rightbar`, using
the *generator* form of `slots.inject` (verified call sites: `slots.inject("rightbar", function* () {…})`
and `slots.inject("main", function* () {…})`). So `children:` declarations can be published into a slot
even when its single cell is already taken — you just cannot `register()` into that cell yourself.

### 3.2 Sidebar — `<ASAR>dsh-client-ui-sidebar\lib\client.js`

```js
children: {
	"sidebar.brand.mark":    { kind: "single", scope: "root" },
	"sidebar.brand.name":    { kind: "single", scope: "root" },
	"sidebar.toggle.badge":  { kind: "single", scope: "root" },
	"sidebar.panellist":     { kind: "list",   scope: "root" },
	"sidebar.workspaces":    { kind: "single", scope: "root" },
	"sidebar.settings":      { kind: "single", scope: "root" },
	"sidebar.footer.action": { kind: "list",   scope: "root" }
}
```

`sidebar.workspaces` is itself declared further by `<ASAR>dsh-client-ui-workspace\lib\client.js`:

```js
children: {
	"sidebar.workspaces.directoryFlow":          { kind: "single", scope: "root" },
	"sidebar.workspaces.session.menu.item":      { kind: "list",   scope: "root", inject: { hooks: { menuOpenState: menuOpenStateFactory, shortcuts: ctx.shortcuts.catalog } } },
	"sidebar.workspaces.session.row.action":     { kind: "list",   scope: "root" },
	"sidebar.session.row.leading":               { kind: "list",   scope: "root" },
	"sidebar.session.row.hover":                 { kind: "list",   scope: "root" }
}
```

### 3.3 Right sidebar (the big panel host) — `<ASAR>dsh-client-ui-sidebar-right\lib\client.js`

```js
ctx.slots.register({
	name: "rightbar",
	children: { "rightbar.session": { kind: "single", scope: "session" } },
	inject: () => ({ hooks: { views: views.source }, mountView: (reference) => views.mount(reference) })
}, …)
```

```js
children: {
	"sidebar.right.pane.tab":        { kind: "keyed", scope: "session" },
	"sidebar.right.pane.tab.title":  { kind: "keyed", scope: "session" },
	"sidebar.right.tab.guide":       { kind: "chain", scope: "session" },
	"sidebar.right.tab.guide.entry": { kind: "keyed", scope: "session" },
	"sidebar.right.tab.menu.item":   { kind: "list",  scope: "session" }
}
```

### 3.4 Conversation — `<ASAR>dsh-client-ui-conversation\lib\client.js`

```js
// registered into main slot with key "conversation"
slots.register({ name: "main", key: "conversation",
  children: { "main.conversation": { kind: "single", scope: "session-maybe" } } }, ConversationPanel);

slots.register({ name: "main.conversation",
  children: { "conversation.header": { kind: "single", scope: "session-maybe" } } }, ConversationRoot);

// NOTE: a FACTORY, not an ordinary slot
slots.registerFactory({ name: "conversation.content", scope: "session-maybe", locale: NS,
  children: {
	"conversation.session":            { kind: "single", scope: "session" },
	"conversation.composer":           { kind: "chain",  scope: "session" },
	"conversation.composer.bar":       { kind: "single", scope: "session-maybe" },
	"conversation.input.dock":         { kind: "list",   scope: "session" },
	"conversation.hero.brand.mark":    { kind: "single", scope: "root" },
	"conversation.hero.workspace":     { kind: "single", scope: "root" },
	"conversation.hero.agentPreset":   { kind: "single", scope: "session-maybe" }
  },
  slots: { views: { scope: "session" }, widthControls: { scope: "root" } },
  inject: (sessionId) => ({ … })
});

slots.register({ name: "conversation.session",
  children: { "conversation.view": { kind: "list", scope: "session" } },
  store: conversationStore, inject: (sessionId, actions) => ({…}) }, ConversationSession);

slots.register({ name: "conversation.header",
  children: {
	"conversation.header.leading":    { kind: "single", scope: "root" },
	"conversation.session.header":    { kind: "single", scope: "session" }
  } }, ConversationHeader);

slots.register({ name: "conversation.session.header", locale: NS,
  children: {
	"conversation.session.header.lineage":   { kind: "single", scope: "session" },
	"conversation.session.header.actions":   { kind: "list",   scope: "session" },
	"conversation.session.header.utilities": { kind: "list",   scope: "session" },
	"conversation.session.header.corner":    { kind: "single", scope: "session" }
  },
  store: conversationStore, inject: (sessionId, actions) => ({…}) }, ConversationSessionHeader);

slots.register({ name: "conversation.composer.bar", locale: NS,
  children: {
	"conversation.input.attachments": { kind: "single", scope: "session-maybe" },
	"conversation.input.overlay":     { kind: "list",   scope: "session" },
	"conversation.input.permission":  { kind: "single", scope: "session" },
	"conversation.input.left":        { kind: "list",   scope: "session" },
	"conversation.input.plan":        { kind: "single", scope: "session" },
	"conversation.input.right":       { kind: "list",   scope: "session" },
	"conversation.input.model":       { kind: "single", scope: "session" },
	"conversation.input.activity":    { kind: "single", scope: "session" },
	"conversation.composer.dock":     { kind: "list",   scope: "session" }
  }, inject: (sessionId) => ({…}) }, …);
```

### 3.5 Settings — `<ASAR>dsh-client-ui-settings-general\lib\client.js` and `-plugins`

```js
children: {
	"settings.section":      { kind: "list",   scope: "root" },   // sidebar nav items in Settings
	"settings.header":       { kind: "single", scope: "root" },
	"settings.close":        { kind: "single", scope: "root" },
	"settings.trigger":      { kind: "single", scope: "root" },
	"settings.action":       { kind: "list",   scope: "root" },
	"settings.launcher":     { kind: "single", scope: "root" },
	"settings.general.item": { kind: "list",   scope: "root" },
	"settings.onboarding":   { kind: "list",   scope: "root" }
}
```
`<ASAR>dsh-client-ui-settings-plugins\lib\client.js` adds
`"settings.plugins.tab": { kind: "list", scope: "root" }`.

### 3.6 Plugins / bundle pages — `<ASAR>dsh-client-ui-plugin-manager\lib\client.js`

```js
"plugins.item":             { kind: "list",  scope: "root" },
"plugins.bundle.config":    { kind: "keyed", scope: "root" },
"plugins.bundle.activation":{ kind: "keyed", scope: "root" },
"plugins.row.config":       { kind: "keyed", scope: "root" },
"plugins.detail.actions":   { kind: "list",  scope: "root" },
"plugins.detail.badge":     { kind: "list",  scope: "root" },
"plugins.detail.section":   { kind: "list",  scope: "root" }
```

### 3.7 Everything else (complete, from the asar)

| key | kind | scope | declaring package |
|---|---|---|---|
| `conversation.approval.detail` | single | session | dsh-client-ui-approval |
| `conversation.chat.commandview` | keyed | session | dsh-client-ui-chat |
| `conversation.chat.node` | keyed | session | dsh-client-ui-chat |
| `conversation.chat.turnTail` | list | session | dsh-client-ui-chat |
| `conversation.header.leading` | single | root | dsh-client-ui-conversation |
| `conversation.hero.workspace.directoryFlow` | single | root | dsh-client-ui-workspace |
| `conversation.message.images` | single | session | dsh-client-ui-chat |
| `conversation.trajectory.images` | single | session | dsh-client-ui-trajectory |
| `deliverables.file.actions` | list | session | dsh-client-ui-deliverables |
| `deliverables.review.file.actions` | list | session | dsh-client-ui-deliverables |
| `settings.models.footer` | list | root | dsh-client-ui-settings-models |
| `sidebar.chat.conversation` | single | session | dsh-client-ui-subagent |
| `sidebar.right.tab.document` | keyed | session | dsh-client-ui-sidebar-documentpreview |
| `sidebar.right.tab.document.action` | keyed | session | dsh-client-ui-sidebar-documentpreview |
| `sidebar.right.tab.document.actions` | list | session | dsh-client-ui-sidebar-documentpreview |
| `sidebar.right.tab.document.office.pdf` | keyed | session | dsh-client-ui-sidebar-documentpreview |
| `sidebar.right.tab.document.unpreviewable` | list | session | dsh-client-ui-sidebar-documentpreview |
| `sidebar.right.tab.files.actions` | list | session | dsh-client-ui-sidebar-files |
| `tool.call.images` | single | session | dsh-client-ui-tool |
| `tool.call.toolview` | keyed | session | dsh-client-ui-tool |
| `tool.view.cordis` | keyed | session | dsh-client-ui-cordis |
| `dsh.sessions.current` / `dsh.trajectory.*` | — | session | (registered in ui-workspace / ui-trajectory; no children table found) |

### 3.8 Which slot to use — recommendation table

| need | slot | kind | why |
|---|---|---|---|
| **large custom panel/page (settings)** | `settings.section` | list (root) | One nav item in the Settings sidebar; the occupant owns the whole content pane. This is what `dshmarket` uses. `id` + `order` + `label`. |
| **a second way into the same settings page** | `settings.plugins.tab` | list (root) | one tab inside the Plugins settings section; needs `id, order, label` |
| **per-package config page** | `plugins.bundle.config` | **keyed (root), key = package name** | the official seat for a third-party bundle's own configuration; `ownerProps.view === 'summary'` gives you the one-liner row |
| per-package row config | `plugins.row.config` | keyed (root), key = package name | |
| **big right-side panel with tabs** | `sidebar.right.pane.tab` | **keyed (session)**, key = tab id | occupant is the whole tab body; pair with `sidebar.right.pane.tab.title` (same key) for the tab's title |
| **a whole right-pane column** | `rightbar` | single (session) — **occupied** | ui-sidebar-right holds it; do NOT register here |
| **modal / toast / global overlay** | `shell.overlay` | list (root) | full-viewport layer above everything; ids: `workspace.session-rename`, `workspace.row-toast`, `dsh-market-toast` |
| **header action button** | `conversation.session.header.actions` | list (session) | the agent-team precedent; `order: -20` puts it first |
| header utility button | `conversation.session.header.utilities` | list (session) | |
| header corner badge | `conversation.session.header.corner` | single (session) | |
| **sidebar section** | `sidebar.workspaces` | single (root) — occupied | use `sidebar.workspaces.session.menu.item` (list) or `sidebar.workspaces.session.row.action` (list) instead |
| extra sidebar footer button | `sidebar.footer.action` | list (root) | (ui-cordis uses it) |
| sidebar panel-list entry | `sidebar.panellist` | list (root) | (ui-plugin-manager, ui-schedule) |
| **per-session dock above the composer** | `conversation.input.dock` | list (session) | ui-conversation's queue dock + ui-goal both live here |
| composer bar slot | `conversation.input.left` / `.right` | list (session) | |
| big main-area content | `main` | keyed (root), key = panel id | the conversation claims `"conversation"`; you must also register a `sidebar.panellist` entry to be reachable |
| turn footer | `conversation.chat.turnTail` | list (session) | |
| tool-result row | `tool.call.toolview` | keyed (session), key = tool name | |
| conversation view tab | `conversation.view` | list (session) | "Chat" / "Trajectory" style switch |

---

## 4. Session store on the client

### 4.1 The store and its hooks

The store lives on `ctx.sessions.list` (a `createSnapshotStore` instance, provided by
`<ASAR>dsh-api-session-controller\lib\client.js:125740`):

```js
this.list = createSnapshotStore({
	ids: [],
	byId: {},
	phase: "pending",
	projectionsBySession: {}
});
…
rootCtx.reflect.provide("sessions", this, void 0);
```

Its `getSnapshot()` returns (`<ASAR>dsh-api-session-controller\lib\client.js:117416-117425`):

```js
return {
	items: this.itemsCache,
	state: this.listState,
	phase: this.listPhase,
	error: this.listError,
	projectionsBySession: Object.fromEntries([...this.projectionStores].map(([sessionId, store]) => [sessionId, {
		values: store.values(),
		state: "idle",
		error: null,
		...this.projectionLoads.get(sessionId)
	}]))
};
```
plus `ids` and `byId` (both set in the initial shape above).

**Hook names come from the renderer**, not the store. `<ASAR>dsh-client-ui-session\lib\client.js` (the
`UiSession` adapter) installs the Session standard sources:

```js
const BUILTIN_SOURCE = {
	hooks: ["session"],
	keyedHooks: ["projection"],
	props: ["sessionId"],
	resolve: (binding) => ({
		hooks: { session: binding.session },
		keyedHooks: { projection: (key) => binding.session.projections.faceOf(key) },
		props: { sessionId: binding.sessionId }
	})
};

function apply(ctx) {
	const service = new UiSession(ctx, ctx.sessions);
	ctx.slots.provideRoot({
		hooks: {
			sessions: ctx.sessions.list,
			sessionStatus: service.sessionStatus
		},
		keyedHooks: { sessionRetainInfo: (key) => ctx.sessions.retainInfo(key) }
	});
	ctx.slots.installScope("session", service.adapter);
}
```

The renderer turns each declared hook name into a prop named `use<CapitalizedName>`
(`standardHookPropName`, `<SRC>dsh-client-ui-slots\lib\index.js:7-9`). So **every** slot component
(regardless of scope) receives:

| prop | source | selector receives |
|---|---|---|
| `useSessions` | `ctx.sessions.list` | the whole list state `{ ids, byId, phase, projectionsBySession, … }` |
| `useSessionStatus` | `uiSession.sessionStatus` | a `Map<sessionId, {running, pendingInteraction, completionUnread}>` |
| `useSessionRetainInfo` | keyed hook `sessionRetainInfo` | one session's retain info |
| `useSession` | session source (`session` hook), session/session-maybe scope only | the `SessionSnapshot` |
| `useProjection` | keyed hook (`projection`), session scope only | `(key) => snapshot` selector factory |
| `useWorkspaces` | root source (ui-workspace `ctx.slots.provideRoot({ hooks: { workspaces: workspaces.list } })`) | workspace list state |
| `sessionId` | plain prop | — |

### 4.2 Real usage — copied verbatim

Global store + projections, from `<SRC>dsh-experimental-client-ui-agent-team\lib\client.js:88-95`:

```js
function TeamMemberRow({ member, memberCount, sessionId, useSessions, useSessionStatus, openTeammate, onError, t }) {
	const model = useSessions((state) => state.projectionsBySession[member.id]?.values.modelSelection?.next?.model);
	const running = useSessionStatus((state) => state.get(member.id)?.running);
	const summaryRunning = useSessions((state) => state.byId[member.id]?.running);
	const status = member.phase === "active" ? (running ?? summaryRunning) === true ? "running" : "inactive" : member.phase;
	const isCurrent = member.id === sessionId;
```

From `<SRC>dsh-experimental-client-ui-agent-team\lib\client.js:238-241`:

```js
	const leadSessionId = useSession((snapshot) => snapshot.subagent?.address.parentSessionId) ?? sessionId;
	const team = useSessions((state) => state.projectionsBySession[leadSessionId]?.values.agentTeam);
	const opening = useSession((snapshot) => snapshot.openState === "loading");
	const listing = useSessions((state) => state.phase === "pending");
```

Other verified selectors (grepped from every extracted bundle):

```js
useSessions((state) => state.byId[sessionId]?.cwd)                                    // ui-deliverables, ui-open-in-app
useSessions((state) => state.byId[sessionId]?.blank)                                  // ui-subagent
useSessions((state) => state.byId[sessionId]?.origin === "subagent")                  // ui-subagent
useSessions((state) => state.projectionsBySession)                                    // ui-subagent
useSessions((state) => Object.values(state.byId)… )                                   // ui-layout, ui-cordis, ui-settings-general
useSessions((state) => { const value = state.byId[sessionId]?.projectionValues?.agentPreset; return typeof value === "string" ? value : void 0 })   // ui-agent-preset
useSessions((state) => state)                                                         // ui-workspace
```

**`activeSessionId`: NOT FOUND** on the sessions store. The active session comes from the layout /
navigation services instead. What exists instead (from
`<ASAR>dsh-client-ui-layout\lib\client.js`):
`ctx.slots.entries("main").some(entry => entry.options.key === id)` for "is this panel open", and the
`LayoutController` reflected as `ctx.layout`.

### 4.3 Which projection keys exist by default

Two different access paths, both real:

1. **Global list store** — `state.projectionsBySession[sessionId].values.<key>` (values are *all* registered
   keys for every session the client has loaded a baseline for).
2. **Per-session face** — `useProjection(key)` (keyed hook) or, outside React,
   `ctx.sessions.binding(sessionId).session.projections.faceOf("<key>")` (a bare observable source
   `{ getSnapshot, subscribe }`). Verbatim `<ASAR>dsh-client-ui-goal\lib\client.js:24045+`:

```js
const binding = sessions.binding(sessionId);
if (binding === void 0) throw new Error(`ui-goal: session "${sessionId}" is unavailable`);
return {
	hooks: { goalActivation: createGoalActivationSource({
		projection: binding.session.projections.faceOf("goal"),
		session: binding.session,
		…
```

and `<ASAR>dsh-client-ui-trajectory\lib\client.js`: `ctx.uiSession.provide({ hooks: ["trajectory"], resolve: (binding) => ({ hooks: { trajectory: trajectorySource(binding) } }) })`.

Projection keys confirmed in use by shipped client packages:

| key | used by |
|---|---|
| `title` | api-session-controller |
| `subagent` | api-session-controller, ui-subagent |
| `subagentCatalog` | api-session-controller, ui-subagent, ui-workspace, ui-workflow-run |
| `subagentTiming` | ui-subagent |
| `inbox` | api-session-controller, ui-conversation, ui-user-questions |
| `sessionListMetadata` | api-session-controller (rejects stale blank hints) |
| `agentPreset` | api-session-controller, ui-agent-preset |
| `modelSelection` | ui-conversation, ui-model-selection, agent-team |
| `permissions` | ui-permission-presets |
| `plan` | ui-conversation |
| `goal` | ui-conversation, ui-goal |
| `userQuestions` | ui-user-questions |
| `tokenUsage` | ui-subagent |
| `agentTeam` | dsh-experimental-client-ui-agent-team |

Projection reads keep loading/failure state independently of values:
`{ values, state, error }` per session (`<SRC>dsh-api-session-controller\README.md:44`).

`createSnapshotStore` itself (`<ASAR>dsh-client-store\lib\index.js`) returns
`{ getSnapshot(), subscribe(fn), update(draftMutator), set(next) }` and is **zustand + immer + uSES**
based, with an optional `{ flush: "raf", persist: { name } }` second argument. To subscribe outside React,
pass the source straight to `useSyncExternalStore(source.subscribe, source.getSnapshot)`; the host wraps
declared `hooks` sources for you through `observableHook` / `maybeObservableHook` /
`keyedObservableHook` (`<ASAR>dsh-client-ui-renderer\lib\client.js`).

**Declaring your own projection on the host does not automatically produce a client store** — the client
must read it through `projectionsBySession` / `faceOf`. **NOT FOUND**: any client-side registration API for a
brand-new projection key.

---

## 5. Client → Host calls

There are two layers, and for a plugin with a custom host half the **lower one is the practical choice**.

### 5.1 Layer 1 — generic Connection RPC (`@deepseek-ai/dsh-client-connection`)

**Client handle** — `<SRC>dsh-client-connection\lib\client.js:1209-1239`:

```js
function createWebConnectionRpc(doFetch, openStream) {
	const send = doFetch ?? ((input, init) => globalThis.fetch(input, init));
	return {
		async call(channel, endpoint, payload, signal) {
			assertTarget(channel, endpoint);
			const rpcId = RpcId(randomUuid());
			const message = { type: "client-request", rpcId, method: endpoint, payload };
			const response = await send(`${channel}/${endpoint}`.slice(1), {
				method: "POST",
				headers: { "content-type": "application/json" },
				body: JSON.stringify(message),
				...signal === void 0 ? {} : { signal }
			});
			if (!response.ok) throw new Error(`transport failure for ${channel}/${endpoint}: HTTP ${response.status}`);
			const full = …;
			if (full.rpcId !== rpcId) throw new Error(`rpcId mismatch for ${endpoint}: sent ${rpcId}, got ${full.rpcId}`);
			return full.result;
		},
		...openStream === void 0 ? {} : { open(channel, endpoint, payload, signal, uplink) { … } }
	};
}
```

`assertTarget` rules (line 1317-1320): `channel` must match `/^\/[A-Za-z0-9._~-]+$/`, and each `/`-separated
`endpoint` segment must match `/^[A-Za-z0-9_$.-]+$/` and not be `.` or `..`.

The `ctx.connection` handle (`<SRC>dsh-client-connection\lib\client.js:1403-1477`) exposes exactly:
`{ isLoopback, generation: {getSnapshot, subscribe}, state: {getSnapshot, subscribe}, rpc, reconnect(),
registerGenerationSource(source), start(sinks, config) }`.

**Host side registration** — `<SRC>dsh-client-connection\lib\index.js:572-577, 640-656`:

```js
	/** Generic channel registry scoped to the Context reading this service. */
	get rpc() {
		const owner = this.ctx;
		return {
			handle: (channel, handler) => this.register(owner, channel, handler),
			intercept: (channel, matches, handler) => this.registerInterceptor(owner, channel, matches, handler)
		};
	}
…
	register(owner, channel, handler) {
		assertChannel(channel);
		const fetchHandler = rpcFetchHandler(channel, handler, this.operator);
		const route = { … path: channel … };
		return owner.effect(() => owner.webServer.register(route), `client-connection: ${channel} rpc channel`);
	}
```

`assertChannel` (line 755-756): `if (!CHANNEL_PATTERN.test(channel) || channel === "/api") throw new Error(\`connection: invalid or reserved RPC channel …\`)`
— so **`/api` is reserved**; a plugin must pick its own channel, e.g. `/my-plugin`.

The handler signature (from `rpcFetchHandler`, line 673-710):

```js
const result = await handler(endpoint, message.payload, request.signal, peer);
```
where `endpoint` is the path after the channel, `payload` is the client's `payload`, `signal` aborts on
disconnect, and `peer` is the operator `PeerScope`. Return value is wrapped into
`{ type: "server-response", rpcId, result: { ok: true, value } }` (or `{ ok: false, error: { code, message, details } }`).

**This is the pattern I recommend for a custom host half:** host does
`ctx.connection.rpc.handle("/my-plugin", async (endpoint, payload, signal, peer) => {…})` and the client does
`await ctx.connection.rpc.call("/my-plugin", "someMethod", {…}, signal)` → resolves to
`{ ok: true, value } | { ok: false, error: { code, message, details } }` (**it does not reject for a
business failure** — only a transport fault throws).

**NOT FOUND:** any third-party/community plugin in the examined tree that registers a custom Connection RPC
channel. The only real `rpc.call` example is inside the gateway itself:

```js
const result = await connection.rpc.call("/api", endpoint, { args: prepared.args }, prepared.signal);
```
(`<ASAR>dsh-api-gateway\lib\client.js`, `invoke()`).

### 5.2 Layer 2 — `ctx.remote` (Typert generated namespaces)

`ctx.remote` is provided by `<ASAR>dsh-api-gateway\lib\client.js` (`ClientRemoteService extends Service`,
`super(ctx, "remote")`). Its surface (`<ASAR>dsh-api-gateway\README.md:52-66` + the bundle):

| member | meaning |
|---|---|
| `ctx.remote.$mount(contribution)` | validate + register a generated Host-for-Client contribution; returns an async disposer |
| `ctx.remote.$host` | `{ home, isLoopback }` — plain values, **not** a store |
| `ctx.remote.$on(event, listener)` | subscribe to one forwarded Host event; legal keys = the Host's forwarding allowlist |
| `ctx.remote.$stream(options)` | single-consumer `RemoteStream` spanning carrier generations |
| `ctx.remote.<namespace>.<method>(...args)` | one generated namespace child service per descriptor namespace |

How a method becomes callable: the **host** business service extends `TypertRemoteService`
(`@deepseek-ai/dsh-typert-protocol`) and marks the method:

```ts
import { Remote, TypertRemoteService } from '@deepseek-ai/dsh-typert-protocol'

export class GoalService extends TypertRemoteService {
  @Remote
  async someMethod(sessionId: string, value: string) { … }
}
```
(`<SRC>dsh-typert-protocol\README.md:35-49`). A build step generates an `InvocationDescriptor` contribution;
the **client assembly** (`@deepseek-ai/dsh-api-remotes/client`) imports the generated `/remote` artifacts and
mounts them:

```js
const disposeRemote = callerCtx.typert.remotes.register(contribution);
const groups = new Map();
for (const descriptor of contribution.descriptors) { … }
```
(`<ASAR>dsh-api-gateway\lib\client.js`, `mountContribution`).
Namespace service key = `` `remote.${namespace}` ``; endpoint = `` `${descriptor.namespace}/${descriptor.method}` ``;
the wire payload is `{ args: <named-args object> }` over `connection.rpc.call("/api", endpoint, …, signal)`;
the client is created lazily and installed as `this[namespace] = service` — so a plugin consumes it as
`ctx.remote.session.list(...)`, `ctx.remote.commands.execute(sessionId, "/plan off", [])`,
`ctx.remote.workspaceFiles.readBytes(...)`, `ctx.remote.settings.…`, etc.

Real consumer examples found:

```js
// <ASAR>dsh-client-ui-plan\lib\client.js
const result = await ctx.remote.commands.execute(sessionId, "/plan off", []);
if (!result.ok) return `${result.error.message}`;
```
```js
// <ASAR>dsh-client-ui-sidebar-documentpreview\lib\client.js
readRelated: (address, relativePath, signal) => {
	const file = hostFileOf(address);
	return ctx.remote.workspaceFiles.readBytes(file.sessionId, relativePath, { baseFile: file.path }, signal);
}
```
```js
// <ASAR>dsh-experimental-client-ui-voice-input\lib\client.js
transcribe: async (request, signal) => await ctx.remote.speech.transcribe(request, signal),
configure: async (patch) => {
	const result = await ctx.remote.speech.configure(patch);
	if (!result.ok) throw result.error;
}
```
```js
// <ASAR>dsh-client-ui-settings-web-search\lib\client.js
await this.ctx.remote.credentials.set(refOf(this.scope.getSnapshot()), value);
```

**Note the `inject` name for a namespace is the dotted key:** `inject = ["remote", "remote.settings"]`
(`<ASAR>dsh-client-ui-settings\lib\client.js`). `ctx.remote` methods always resolve to
`RemoteResult<T>` = `{ ok: true, value } | { ok: false, error }` and only *throw* for a local assembly
fault; discriminate with `isRemoteFailure(value)`.

**NOT FOUND:** any non-`@deepseek-ai` plugin in the tree that generates and mounts its own
`ctx.remote` contribution. Using `ctx.remote.$mount()` from a third-party bundle would require the
Typert code generator, so Layer 1 is the realistic route.

Requires: `ctx.remote` needs `@deepseek-ai/dsh-api-gateway` + `@deepseek-ai/dsh-api-remotes` in the
composition; `ctx.connection` comes from `@deepseek-ai/dsh-client-connection` (a mandatory web row).

---

## 6. UI primitives — complete export list

Package: `@deepseek-ai/dsh-client-ui-primitives`. The authoritative export statement is the single
`export { … }` at the end of `<SRC>dsh-client-ui-primitives\lib\index.js` (6615 chars). Complete inventory:

### 6.1 Layout / containers / misc
`BrandWordmark`, `FishLogo`, `FISH_LOGO_PATH`, `FISH_LOGO_VIEWBOX`, `SHIELD_OUTLINE_PATH`,
`PathLabel`, `TextShimmer`, `ConnectionIndicator`

### 6.2 Buttons & actions
`Button` (`variant: 'primary' | 'ghost' | 'outline' | 'toolbar'`, `size: 'md' | 'sm'`, `icon`)
`Pill` (selectable capsule: `active`, `onClick`) · `Tag` (`tone`, one of eight palettes) ·
`ShortcutKeys` · `RiskConfirmation` · `Switch` · `Checkbox`

### 6.3 Nav / segmented
`SegmentedControl` (tablist of ≥2 equal segments, one sliding indicator, `id` seeds `<id>-<value>`) ·
`SegmentedTabs` (controlled tabs with ←/→/Home/End) · `MenuGroup` · `observeStickyMenuGroups`

### 6.4 Disclosure / trees
`DisclosureRow` (24px compact disclosure; memoized with shallow compare) ·
`TextShimmer` (row highlight) · `JsonTree` (`collapsedStringLines`, default 3) · `JsonBlock`

### 6.5 Overlays
`Modal` (`open`, `onClose`, `title`, `closeLabel`, `description`, `footer`, `headless`, `backdropBlur`) ·
`HoverCard` (inline + popover, optional copy button) · `ImageLightbox` · `Tooltip`
(`label`, `side`, `gap`, `delayMs`, `focusDelayMs`, `disabled`, `portal`, `openOnClick`) ·
`Toast` (`text`, `icon`, `anchor`, `onDone`, `holdMs`) · `MenuSurface` (`compact`) ·
`isBehindModal` · `closeTopModal(document)` · `useModalLayer` · `focusWithoutRing(element, options?)` ·
`modalSelector` · `useDismissOnOutsidePointer` · `useAnchoredPosition` · `useAnchoredMaxHeight`

### 6.6 Menus
`Menu` (`open`, `anchor` (ReactNode), `items: MenuEntry[]`, `footer`, `selectedId`, `selectedIds`,
`onSelect`, `onClose`, `align: 'start'|'end'`, `side`, `portal`, `closeOnPointerLeave`, `dense`,
`compact`, `listClassName`, `className`) · `MenuItemButton` (`separatorBefore`) ·
types `MenuItem` `MenuSeparator` `MenuLabel` `MenuEntry`

### 6.7 Code / terminal / diff / output cards
`CodeBlock` (`lineNumbers`, `toolbarLabels`, `contentRef`, `showHeader`) ·
`TerminalBlock` (`maxLines`, `copyText`, `runStateDot`) · `ReadBlock` · `DiffBlock` ·
`SearchBlock` · `WebBlock` · `DEFAULT_TERMINAL_MAX_LINES`, `DEFAULT_READ_MAX_LINES`,
`DEFAULT_DIFF_MAX_LINES`, `DEFAULT_SEARCH_MAX_LINES` · `diffTotals`

### 6.8 Markdown
`MarkdownText` (`variant: 'body' | 'compact'`, `pathImages`, `fileImages`) ·
`MarkdownDelegateProvider` (`openExternalLink`, `openFile`) · `extractMarkdownPlainText` ·
`projectUserText` · `languageForPath` · `CODE_HIGHLIGHT_EXTENSIONS` · `useCodeHighlighter`

### 6.9 Form inputs & settings kit
`Input` · `Checkbox` · `Switch` · `SettingsForm` · `SettingsValueField` · `SettingsSecretField` ·
`SettingsFormModel` · `settingsNumberField` · `settingsTextField`

### 6.10 File / link / permission artwork
`FileTypeIcon` · `classifyFileType` · `fileExtension` · `classifyLinkPath` ·
`PluginArtworkTerminal` `PluginArtworkLoop` `PluginArtworkSubagent` `PluginArtworkSearch`
`PluginArtworkDefault` · `GuideArtworkBrowser` `GuideArtworkFiles` ·
`PermissionIconReadOnlyRegular`/`Medium` · `PermissionIconWorkspaceWriteRegular`/`Medium` ·
`PermissionIconFullAccessRegular`/`Medium` · `ReferenceIconRegular`/`Medium` · `LinkIconRegular`/`Medium`

### 6.11 Utilities
`writeClipboard` · `relativeTime` · `fileSizeText` · `rankByName` · `observeComposition` ·
`pointerModality` · `isDarwinDesktop` · `ICON_REGULAR_STROKE` · `ICON_MEDIUM_STROKE`

### 6.12 Icons — naming convention

**Weight names, not size names:** `<Name><Regular|Medium>`, e.g. `IconChevronDownOutlineRegular` /
`IconChevronDownOutlineMedium`; `Regular` = 1px product stroke, `Medium` = same geometry at 1.3px. Pixel
size is the `size` prop (`IconProps = { size?: number; className?: string }`).
**`Regular`/`Medium` replaced pre-0.1.7 size-suffixed names** (`IconChevronDownOutline14`,
`IconCheckOutline16`, `IconSparkle16`) **with no alias**, per `<MKT>\src\client\primitives.d.ts:116-119`:
> "Pre-0.1.7 size suffixes. Host 0.1.7-alpha.1 renamed these to weight names (below) with no alias; the
> market resolves Regular first, then the size-suffixed spelling, via icons.ts (#671)."

Complete icon list (each in both `…Regular` and `…Medium` unless noted):
`IconAgentPresetOutline`, `IconAlarmClockOutline`, `IconApiOutline`, `IconArchiveCheckOutline`,
`IconArchiveOffOutline`, `IconArchiveOutline`, `IconBranchOutline`, `IconBrowseOutline`,
`IconCheckCircleFill`, `IconCheckCircleOutline`, `IconCheckOutline`, `IconChecklistOutline`,
`IconChevronDownOutline`, `IconChevronLeftOutline`, `IconChevronRightOutline`, `IconChevronUpOutline`,
`IconChevronsUpDownOutline`, `IconClockOutline`, `IconCloseCircleFill`, `IconCloseFill`, `IconCloseOutline`,
`IconCodeOutline`, `IconCompactOutline`, `IconCompareSplitOutline`, `IconContextInjectionOutline`,
`IconCopyOutline`, `IconCordisPluginOutline`, `IconDarkOutline`, `IconDataOutline`, `IconDatabaseOutline`,
`IconDeliverDoc`, `IconDislikeFill`, `IconDislikeOutline`, `IconDownloadOutline`, `IconEditOutline`,
`IconEllipsisOutline`, `IconEnhanceOutline`, `IconFlatListOutline`, `IconFolderClose`,
`IconFolderOpen`, `IconFolderOpenOutline`, `IconFollowsystemOutline`, `IconFullscreenOutline`,
`IconGaugeOutline`, `IconGlobeOutline`, `IconGoalOutline`, `IconInfoOutline`, `IconInspectOutline`,
`IconLightOutline`, `IconLikeFill`, `IconLikeOutline`, `IconLinkOutline`, `IconListPenOutline`,
`IconLoadingOutline`, `IconMicrophoneOutline`, `IconNewChatOutline`, `IconNowrapFill`,
`IconPanelLeftOutline`, `IconPaperPlaneOutline`, `IconPaperclipOutline`, `IconPauseOutline`,
`IconPersonalizationOutline`, `IconPinFill`, `IconPinOutline`, `IconPlanOutline`, `IconPlayOutline`,
`IconPluginPinwheelOutline`, `IconPlusOutline`, `IconProjectAddOutline`, `IconQuestionOutline`,
`IconQueueOutline`, `IconRefreshOutline`, `IconRightUpOutline`, `IconSearchOutline`, `IconSendOutline`,
`IconSettingsOutline`, `IconShareOutline`, `IconShieldOutline`, `IconSkillOutline`,
`IconSlidersTwoOutline`, `IconSparkle`, `IconStopFill`, `IconThinkOutline`, `IconTrashOutline`,
`IconTreeCorner`, `IconTriangleRightFill`, `IconUnarchiveOutline`, `IconUserOutline`, `IconUsersOutline`,
`IconWarningOutline` (circle), `IconWarningTriangleOutline` (rounded triangle),
`IconWorkspaceTreeOutline`, `IconWrapFill`, `IconWrapLinesOutline`

**Runtime existence check:** yes, and it is the documented version-tolerance pattern —
`<MKT>\src\client\index.ts:31-35` `missingPrimitives(mod, required)` plus `icons.ts`'s
`missingIcons(mod)`; log the gaps and either skip registration entirely (hard-required primitives) or
render without them (icons).

---

## 7. Theme tokens & CSS

### 7.1 Where the tokens live

`@deepseek-ai/dsh-client-ui-theme` — `<SRC>dsh-client-ui-theme\lib\client.js` contains the complete token
sheets: **899 occurrences of `--dsw-*`**. Its README (line 12) says: "The package also ships the `--dsw-*`
token stylesheets and injects a synchronous bootstrap so the selected palette and font size apply before the
shell loads. Third-party themes can register alias-token overrides through `ctx.theme`."
README lines 58: eight sheets imported in order — `base.css`, `corner-shape.css`, `design-platform.css`,
`focus.css`, `onboarding.css`, `scrollbar.css`, `gradient-shadow-text.css`, `shiki.css` — "compiled and
injected as plugin-owned global styles".

### 7.2 Dark / light selection (exact mechanism)

`<SRC>dsh-client-ui-theme\README.md:40`:
> "Head CSS selects the document canvas color scheme before any script runs, including a
> `prefers-color-scheme` query for the `system` preference. A body script then sets
> `body[data-ds-dark-theme]` and `--dsh-content-font-size` before the loading page and application scripts"

So: **`body[data-ds-dark-theme]`** is the dark-mode hook (`color-scheme` on `:root`/`html` for the
canvas), plus `html[data-input-modality='pointer'|'keyboard']` for focus-ring behavior
(README lines 60-62). **Write your CSS against the tokens, never against a `.dark` class.**

### 7.3 Class-name convention in built bundles

The built `.module.css` files are **authored with plain local names** and shipped unhashed — see
`<SRC>dsh-client-ui-primitives\lib\Pill.module.css` verbatim:

```css
.pill {
  display: inline-flex; align-items: center; gap: 4px; height: 24px; padding: 0 8px;
  border: none; border-radius: 999px; corner-shape: round; font-size: 12px; line-height: 18px;
  color: var(--dsw-alias-label-secondary);
  background: var(--dsw-alias-bg-layer-2);
}
.interactive { cursor: pointer; }
.interactive:hover { background: var(--dsw-alias-interactive-bg-hover); }
.active {
  color: var(--dsw-alias-label-primary);
  background: var(--dsw-alias-button-ghost-active-fill);
  box-shadow: inset 0 0 0 1px var(--dsw-alias-button-ghost-active-border);
}
```

**The hashed prefix is a *build-time* transform, applied by the tsdown CSS-module plugin, not a convention
you must copy.** The hash is a 6-char base62 prefix chosen per module (`nUhMVa_`, `EBLgjq_`, `mP3ANa_`) and
the class in the injected string is `<prefix>_<localName>`. The plugin's own CSS is the *source* file; the
bundle contains both the flattened string and the lookup map.

For a **hand-written** bundle, you have two valid options:
1. **Namespace-manually (recommended):** use your own literal prefix, e.g.
   `.dshmypanel_root { … }`, and inject one `<style data-plugin="<pkg>" data-plugin-css="<pkg>/x.css">`.
   Nothing in the host reads the class names.
2. Keep unhashed local names — safe as long as they cannot collide with other plugins' global CSS. Since the
   host injects every style tag into `document.head` **globally**, prefer option 1.

Global rules the theme enforces (`<SRC>dsh-client-ui-theme\README.md`):
- menus/popovers/dialogs use `--dsw-menu-surface-fill` + `--dsw-menu-backdrop-filter` via `MenuSurface` or
  `--dsw-specific-menu` (lines 47, 78);
- elevated surfaces set `border: 0` and use `--dsw-elevation-stroke` / `--dsw-elevation-panel` /
  `--dsw-elevation-prominent` / `--dsw-elevation-soft` (line 78);
- focus rings: `var(--dsw-focus-ring-color, var(--dsw-alias-state-business-primary))` +
  `var(--dsw-focus-ring-width)` (2px) — never `outline-style` (line 60);
- radii: `--dsw-radius-xs|sm|md|lg|xl|panel`, with `--dsw-corner-shape` already applied globally
  (line 76) — **do not** add `corner-shape` yourself except to pair `round` with `border-radius: 50%`
  and pill radii;
- scrollbars: rebind `--dsh-scrollbar-thumb` / `--dsh-scrollbar-thumb-hover` on your scroll container
  (note the `--dsh-` prefix, not `--dsw-`), optionally `--dsh-scrollbar-width`,
  `--dsh-scrollbar-thumb-border`, `--dsh-scrollbar-track-margin` (line 86).

### 7.4 The token list

The complete set of names appearing in the shipped theme bundle, grouped by prefix. (899 name occurrences;
the list below is deduplicated across the whole asar.)

**Alias — surfaces & layers**
`--dsw-alias-bg-base` · `--dsw-alias-bg-layer-1` · `--dsw-alias-bg-layer-2` · `--dsw-alias-bg-layer-3` ·
`--dsw-alias-bg-layer-4` · `--dsw-alias-bg-overlay` · `--dsw-alias-bg-mask-1` · `--dsw-alias-bg-mask-2` ·
`--dsw-alias-bg-mask-3` · `--dsw-alias-bg-mask-drop` · `--dsw-alias-bg-mask-photo` ·
`--dsw-alias-bg-module-platform` · `--dsw-alias-bg-multi-select` · `--dsw-alias-bg-skeleton` ·
`--dsw-alias-bg-document-preview` · `--dsw-alias-bg-document-selection`

**Alias — borders**
`--dsw-alias-border-l1` · `--dsw-alias-border-l2` · `--dsw-alias-border-l3` · `--dsw-alias-border-l4` ·
`--dsw-alias-border-l2-darkmode-thin` · `--dsw-alias-border-inverted` · `--dsw-alias-border-inverted2`

**Alias — labels**
`--dsw-alias-label-primary` · `--dsw-alias-label-primary-dimmed` · `--dsw-alias-label-primary-inverted` ·
`--dsw-alias-label-primary-foreground` · `--dsw-alias-label-primary-bluish` · `--dsw-alias-label-secondary` ·
`--dsw-alias-label-tertiary` · `--dsw-alias-label-caption` · `--dsw-alias-label-dimmed` ·
`--dsw-alias-label-inverted` · `--dsw-alias-label-shimmer` · `--dsw-alias-label-deep-diving` ·
`--dsw-alias-label-deep-diving-shimmer` · `--dsw-alias-label-document-preview` · `--dsw-alias-label-error`

**Alias — state (semantic)**
`--dsw-alias-state-business-primary` · `--dsw-alias-state-business-tertiary` ·
`--dsw-alias-state-success-primary` · `--dsw-alias-state-success-secondary` ·
`--dsw-alias-state-success-tertiary` · `--dsw-alias-state-warn-primary` ·
`--dsw-alias-state-warn-secondary` · `--dsw-alias-state-warn-tertiary` · `--dsw-alias-state-warn-label` ·
`--dsw-alias-state-error-primary` · `--dsw-alias-state-error-secondary` · `--dsw-alias-state-idle-primary`

**Alias — interactive**
`--dsw-alias-interactive-bg-hover` · `--dsw-alias-interactive-bg-active` ·
`--dsw-alias-interactive-bg-hover-accent` · `--dsw-alias-interactive-bg-hover-danger` ·
`--dsw-alias-interactive-bg-hover-solid`

**Alias — buttons**
`--dsw-alias-button-primary-fill` · `--dsw-alias-button-primary-hover` · `--dsw-alias-button-primary-dimmed` ·
`--dsw-alias-button-ghost-active-fill` · `--dsw-alias-button-ghost-active-border` ·
`--dsw-alias-button-ghost-active-hover` · `--dsw-alias-button-tool-bar-fill` ·
`--dsw-alias-button-tool-bar-fill-invisible` · `--dsw-alias-button-tool-bar-hover` ·
`--dsw-alias-button-elevated-fill` · `--dsw-alias-button-floating-fill` · `--dsw-alias-button-floating-hover` ·
`--dsw-alias-button-contrast-fill` · `--dsw-alias-button-info-fill` · `--dsw-alias-button-info-hover`

**Alias — brand / link**
`--dsw-alias-brand-primary` · `--dsw-alias-brand-primary-invert` · `--dsw-alias-brand-text` ·
`--dsw-alias-link`

**Alias — markdown & code**
`--dsw-alias-markdown-code-block` · `--dsw-alias-markdown-code-block-banner` ·
`--dsw-alias-markdown-inline-code` · `--dsw-alias-markdown-citation` · `--dsw-alias-markdown-placeholder` ·
`--dsw-alias-markdown-tag` · `--dsw-alias-markdown-code-segment-selected` ·
`--dsw-alias-markdown-code-segment-unselected` · `--dsw-alias-code-diff-added` ·
`--dsw-alias-code-diff-deleted` · `--dsw-alias-file-diff-added-bg` · `--dsw-alias-file-diff-added-gutter` ·
`--dsw-alias-file-diff-added-marker` · `--dsw-alias-file-diff-deleted-bg` ·
`--dsw-alias-file-diff-deleted-gutter` · `--dsw-alias-file-diff-deleted-marker`

**Alias — menus, overlays, misc chrome**
`--dsw-alias-menu-icon` · `--dsw-alias-menu-group-header-fill` · `--dsw-alias-tooltip-bg` ·
`--dsw-alias-tooltip-key-bg` · `--dsw-alias-toast-bg` · `--dsw-alias-toast-label` ·
`--dsw-alias-switch-thumb` · `--dsw-alias-turn-trigger-bg` · `--dsw-alias-turn-trigger-bg-hover` ·
`--dsw-alias-scrollbar-bg-l1` · `--dsw-alias-scrollbar-bg-l2` · `--dsw-alias-scrollbar-hover-l1` ·
`--dsw-alias-scrollbar-hover-l2` · `--dsw-alias-settings-card-fill` · `--dsw-alias-settings-card-stroke` ·
`--dsw-alias-onboarding-accent` · `--dsw-alias-onboarding-card-fill` ·
`--dsw-alias-onboarding-checkbox-border` · `--dsw-alias-onboarding-secondary-fill`

**Specific (overlay material)**
`--dsw-specific-menu` · `--dsw-specific-bubble` · `--dsw-specific-bubble-highlight` ·
`--dsw-specific-input-major` · `--dsw-specific-login-input` · `--dsw-specific-selector` ·
`--dsw-specific-sidebar-fill` · `--dsw-specific-sidebar-nav-item-active` ·
`--dsw-specific-sidebar-nav-item-active-accent` · `--dsw-specific-sidebar-nav-item-hover` ·
`--dsw-specific-tip`

**Radii, elevation, focus, masks**
`--dsw-radius-xs` · `--dsw-radius-sm` · `--dsw-radius-md` · `--dsw-radius-lg` · `--dsw-radius-xl` ·
`--dsw-radius-panel` · `--dsw-corner-shape` · `--dsw-shadow-lv1` · `--dsw-shadow-lv1-blur` ·
`--dsw-shadow-lv2` · `--dsw-shadow-lv3` · `--dsw-elevation-stroke` · `--dsw-elevation-stroke-color` ·
`--dsw-elevation-panel` · `--dsw-elevation-prominent` · `--dsw-elevation-soft` ·
`--dsw-focus-ring-color` · `--dsw-focus-ring-width` · `--dsw-menu-surface-fill` ·
`--dsw-menu-backdrop-filter` · `--dsw-mask-blur` · `--dsw-hovercard-bg` ·
`--dsw-linear-gradient-think` · `--dsw-linear-think-select` ·
`--dsw-gradient-onboarding-violet-stops` · `--dsw-gradient-onboarding-blue-stops` ·
`--dsw-gradient-onboarding-cyan-stops`

**Typography — `--dsw-font-family`, `--dsw-font-family-brand`, and step families.**
Each step exposes a shorthand plus five longhands:
`--dsw-font-{xxxs-11,xxs-12,xs-13,s-14,base-16,m-18,l-20,xl-24}` and
`--dsw-font-{xxxs,xxs,xs,s,base}-strong-{11,12,13,14,16}`, each with
`-font-family`, `-font-size`, `-font-style`, `-font-weight`, `-line-height`.
Markdown variants: `--dsw-font-markdown-{base,base-strong,base-italic,base-strong-italic,small,
small-strong,small-italic,small-strong-italic,code,code-block,code-block-small,h1,h2,h3,h4,table,table-head}`
each with the same five longhands; `--dsw-font-markdown-code-font-family` is a standalone token.

**Static palette** (raw steps, for charts/dots/artwork):
`--dsw-static-neutral-{00,50,100,150,200,250,300,400,500,550,600,700,800,850,900,1000}` ·
`--dsw-static-neutral-bluish-{00,50,60,75,100,150,200,300,400,500,600,700,750,800,850,875,900,950,1000}` ·
`--dsw-static-blue-{50,50p,75,100,300,400,450,500,600,800,900,950}` ·
`--dsw-static-deepseek-{50,100,200,300,400,450,500,600,700-delete,800,900}` ·
`--dsw-static-{green-100,green-400,green-500,green-500-a08,green-500-a12,green-900}` ·
`--dsw-static-{amber-100,amber-400,amber-500,amber-600,amber-900}` ·
`--dsw-static-{red-50,red-100,red-400,red-400-a12,red-500,red-600,red-600-a08,red-900}`

**Document-level (`--dsh-` prefix, not `--dsw-`)**
`--dsh-content-font-size` · `--dsh-content-font-delta` · `--dsh-content-font-size-secondary` ·
`--dsh-content-font-delta-secondary` · `--dsh-scrollbar-thumb` · `--dsh-scrollbar-thumb-hover` ·
`--dsh-scrollbar-thumb-border` · `--dsh-scrollbar-width` · `--dsh-scrollbar-track-margin` ·
`--dsh-boot-arc` (boot spinner)

### 7.5 Theme service

```ts
interface ThemeService {
  getTheme(): ThemeSnapshot | null
  setTheme(id: string): void
}
```
(`<MKT>\src\client\index.ts:64-68`). React-friendly subscription, verbatim from
`<MKT>\src\client\index.ts:142-146`:

```ts
themeStore: {
  subscribe: (cb: () => void) => ctx.on('theme/change', cb),
  getSnapshot: () => ctx.theme.getTheme(),
},
```

and from `<ASAR>dsh-client-ui-settings-account\lib\client.js`:

```js
theme: {
	getSnapshot: () => ctx.theme.getTheme(),
	subscribe: (listener) => ctx.on("theme/change", listener)
}
```

`inject: ['theme']` is enough; `theme/change` fires synchronously with registry mutations
(`<SRC>dsh-client-ui-theme\README.md:138`).

---

## 8. Where client plugins get registered / enabled

### 8.1 Discovery

The host half `ClientModuleRegistry` (`<SRC>dsh-client-modules\lib\index.js:506-556`) subscribes to the
cordis loader and scans **incrementally per package**:

```js
ctx.on("internal/plugin", (fiber) => {
	const entryName = fiber.entry?.options.name;
	if (entryName === void 0) return;
	this.dirty.add(entryName);
	…
});
…
for (const entry of ctx.loader.entries()) this.dirty.add(entry.options.name);
this.composed = this.compose();
```

So a package joins the client graph when its **host half** becomes a live loader entry. The host resolves
`package.json` from the loader row (`locatePkgJson` → `nearestPackage`), requires
`dsh.client.platform === "web"`, and requires `exports["./client"]`. Nothing else registers a client half.

### 8.2 Boot order (what the page executes)

`bootInjections(graph)` (`<SRC>dsh-client-modules\lib\index.js:453-498`) injects, into `<head>`:

1. an inline `window.__ModuleLoader__` **queue facade** (`mode:"queue"`, `pendingQueue`, `load()` pushes,
   `create()` pops the bootstrap registration and builds the real system);
2. `<link rel=modulepreload>`-style advisory preloads for every **application** batch combo;
3. parser-blocking `<script src>` for the **bootstrap** batch combo (contains
   `@deepseek-ai/dsh-client-modules`);
4. a `<script>window.__DSH_BOOT__ = <graph></script>` global, before the shell reads it.

`ClientEntries.start()` then creates one loader entry per graph row and awaits activation — **the React
renderer's `mount()` only runs after every client entry has settled**
(`<ASAR>dsh-client-ui-renderer\README.md:94`: "The first application frame waits for every client entry").

### 8.3 Refresh vs. restart vs. hot — precisely

| situation | what happens | evidence |
|---|---|---|
| **Install a new client plugin bundle (new package)** | Host loader gains the entry → `internal/plugin` fires → microtask flush recomposes the graph → `onGraphChanged` → the **live page follows it through the HMR transport** and the new row is imported, materialized and mounted. `dshmarket`'s own README says *"most plugins go live after a page refresh, no restart"*; the client-modules README is stronger: *"An open Web page follows the Host's complete module graph through the HMR transport. Enabling an ordinary plugin adds its Loader entry…"* | `<SRC>dsh-client-modules\README.md:58`; `<SRC>dsh-client-modules\lib\index.js:527-551, 694-700` |
| **A brand-new package requires the host process to have loaded it at all** | the package must be in the profile's cordis composition; `dsh plugin --profile <p> add <pkg>` then a **host restart** for the host half to exist, after which the client half appears without another restart | `<MKT>\README.md:21-24` ("Restart `dsh web`, then open Settings → Plugin Market") |
| **Edit an already-registered bundle's `client.js` on disk** | `rebuilt(id)` recomputes the rev from mtime/ctime/size, `notifyGraphChanged`, `ClientEntries.reload(id, rev)` tears down the entry fiber, `removeOwnedStyles(id)`, re-imports, and `entry.refresh()`. This is the **HMR path** (`ctx.clientHmr` / `dsh-client-hmr`). Unchanged mtime+size keeps the revision, so SSE reconnects do not replace plugins | `<SRC>dsh-client-modules\lib\index.js:607-625`; `lib\client.js:298-327` |
| **Enable/disable a plugin** | disable removes the Loader entry and "waits for its asynchronous effects before evicting unused modules and styles. Re-enabling loads one instance with its styles." | `<SRC>dsh-client-modules\README.md:58` |
| **Replace the bootstrap module (`@deepseek-ai/dsh-client-modules`)** | refused at runtime: `client-modules: replacing bootstrap module <id> requires a page reload` | `lib\client.js:541` |
| **CSS after unload/reload** | `removeOwnedStyles(id)` deletes every `style[data-plugin="<id>"]`; on re-materialization the factory re-injects them. Hand-written CSS must therefore live **inside the factory closure**, not at the top of the file | `lib\client.js:194-197, 690` |
| **Dev-mode client-plugin HMR receiver** | the harness GUI has an active client-plugin HMR receiver; automatic reload without a manual refresh only while the coordinating dev watcher (`pnpm run dev:web` equivalent for the plugin's own build) is also running | DSH harness runtime-context note; `<SRC>dsh-client-modules\README.md:72` ("Bundle content changes reach the graph only through `rebuilt()` (the HMR hook)") |

**Failure reporting:** `ClientEntries.state` is a stable observable `{ syncing, failures: [{id, message}] }`,
rendered in **Settings → Plugins → Plugin list** with a retry button (`retry()`), per
`<SRC>dsh-client-modules\README.md:58`. A bundle that never registers its `id` produces
`… loaded without registering "<id>" via __ModuleLoader__.load`; a missing bundle at startup fails
activation loudly with one build instruction.

---

## Explicit "NOT FOUND" list

1. **No shipped `declare module '@deepseek-ai/dsh-client-*'` augmentation** for `ctx.slots` /
   `ctx.connection` / `ctx.sessions`. Neither `<SRC>dsh-client-ui-slots\lib\` nor the asar copy contains
   `lib/types`. `SlotMap` is declared empty and the merges are internal-only.
2. **No `entry` or `export` field inside `dsh.client`** — the bundle path comes only from `exports["./client"]`.
3. **No loader-provided CSS API** (no `load({css})`, no `injectCSS`, no `adoptedStyleSheets` helper).
4. **`state.activeSessionId` does not exist** on the sessions store. Active session is derived from
   `ctx.slots.entries("main")` / the `layout` service.
5. **No client-side API to declare a new session projection key** — only to read existing ones.
6. **No CI/CD or community precedent for a third-party `ctx.remote.$mount()` contribution** (needs the
   Typert generator); use `ctx.connection.rpc.handle` / `.call`.
7. **No example of any third-party plugin registering a custom Connection RPC channel** in the examined tree.
8. **`@deepseek-ai/dsh-client-runtime`** appears only as a `devDependencies` entry in `<MKT>\package.json`;
   it is not in the platform seed table and not in the boot graph.
