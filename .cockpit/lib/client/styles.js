/**
 * Project Cockpit — the panel's stylesheet.
 *
 * Kept as one string so the client bundle stays a single self-contained file
 * with no build step. Every colour resolves through a host theme token with a
 * literal fallback, so the panel follows light/dark like the rest of the shell
 * and still renders if a token is renamed.
 */

/** Short prefix for every class, so two plugins cannot collide in the page. */
const P = 'pcx'

export const CLASS = {
  root: `${P}-root`,
  header: `${P}-header`,
  title: `${P}-title`,
  subtitle: `${P}-subtitle`,
  chipRow: `${P}-chips`,
  chip: `${P}-chip`,
  chipDot: `${P}-chipdot`,
  actions: `${P}-actions`,
  button: `${P}-button`,
  buttonPrimary: `${P}-button-primary`,
  body: `${P}-body`,
  tabs: `${P}-tabs`,
  tab: `${P}-tab`,
  tabActive: `${P}-tab-active`,
  tabCount: `${P}-tabcount`,
  view: `${P}-view`,
  section: `${P}-section`,
  sectionTitle: `${P}-section-title`,
  muted: `${P}-muted`,
  grid: `${P}-grid`,
  card: `${P}-card`,
  cardHeader: `${P}-cardheader`,
  cardTitle: `${P}-cardtitle`,
  cardMeta: `${P}-cardmeta`,
  bar: `${P}-bar`,
  barSeg: `${P}-barseg`,
  legend: `${P}-legend`,
  legendItem: `${P}-legenditem`,
  swatch: `${P}-swatch`,
  layer: `${P}-layer`,
  layerHead: `${P}-layerhead`,
  layerName: `${P}-layername`,
  layerRule: `${P}-layerrule`,
  nodes: `${P}-nodes`,
  node: `${P}-node`,
  nodeActive: `${P}-node-active`,
  nodeTop: `${P}-nodetop`,
  nodeName: `${P}-nodename`,
  nodeKind: `${P}-nodekind`,
  nodeDeps: `${P}-nodedeps`,
  badge: `${P}-badge`,
  dot: `${P}-dot`,
  mono: `${P}-mono`,
  table: `${P}-table`,
  row: `${P}-row`,
  rowClick: `${P}-rowclick`,
  cell: `${P}-cell`,
  cols: `${P}-cols`,
  col: `${P}-col`,
  list: `${P}-list`,
  listItem: `${P}-listitem`,
  missing: `${P}-missing`,
  present: `${P}-present`,
  warn: `${P}-warn`,
  warnBox: `${P}-warnbox`,
  empty: `${P}-empty`,
  agent: `${P}-agent`,
  agentRow: `${P}-agentrow`,
  agentCard: `${P}-agentcard`,
  agentName: `${P}-agentname`,
  agentRole: `${P}-agentrole`,
  emoji: `${P}-emoji`,
  task: `${P}-task`,
  taskCard: `${P}-taskcard`,
  taskHead: `${P}-taskhead`,
  taskBody: `${P}-taskbody`,
  deps: `${P}-deps`,
  dep: `${P}-dep`,
  arrow: `${P}-arrow`,
  kpi: `${P}-kpi`,
  kpiValue: `${P}-kpivalue`,
  kpiLabel: `${P}-kpilabel`,
  tree: `${P}-tree`,
  treeRow: `${P}-treerow`,
  treeName: `${P}-treename`,
  treeMeta: `${P}-treemeta`,
  footer: `${P}-footer`,
  spin: `${P}-spin`,
  err: `${P}-err`,
  ok: `${P}-ok`,
  invariants: `${P}-invariants`,
  inv: `${P}-inv`,
  invId: `${P}-invid`,
  tag: `${P}-tag`,
  scroll: `${P}-scroll`,
  details: `${P}-details`,
  summary: `${P}-summary`,
  heading: `${P}-heading`,
  headingL2: `${P}-heading-l2`,
  headingL3: `${P}-heading-l3`,
}

/**
 * The stylesheet text.
 * @returns CSS.
 */
export function cockpitCss() {
  return `
.${CLASS.root} {
  --pcx-bg: var(--dsw-alias-bg-base, #16171a);
  --pcx-surface: var(--dsw-alias-bg-layer-1, #1d1f23);
  --pcx-surface2: var(--dsw-alias-bg-layer-2, #24272c);
  --pcx-overlay: var(--dsw-alias-bg-overlay, #2a2e34);
  --pcx-line: var(--dsw-alias-border-l1, #31353c);
  --pcx-line2: var(--dsw-alias-border-l2, #3d434c);
  --pcx-text: var(--dsw-alias-label-primary, #e8eaed);
  --pcx-text2: var(--dsw-alias-label-secondary, #a2a9b4);
  --pcx-brand: var(--dsw-alias-brand-primary, #4d8dff);
  --pcx-ok: var(--dsw-alias-state-success-primary, #3fb950);
  --pcx-warn: var(--dsw-alias-state-warn-primary, #d29922);
  --pcx-err: var(--dsw-alias-state-error-primary, #f85149);
  --pcx-idle: var(--dsw-alias-state-idle-primary, #8b949e);
  --pcx-radius: 10px;
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  background: var(--pcx-bg);
  color: var(--pcx-text);
  font-size: 13px;
  line-height: 1.45;
}

.${CLASS.header} {
  flex: none;
  padding: 16px 20px 0;
  border-bottom: 1px solid var(--pcx-line);
  background: var(--pcx-surface);
}

.${CLASS.title} {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  letter-spacing: -0.01em;
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
}

.${CLASS.subtitle} {
  color: var(--pcx-text2);
  font-size: 12.5px;
  margin: 4px 0 0;
  max-width: 90ch;
}

.${CLASS.chipRow} {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin: 10px 0 0;
}

.${CLASS.chip} {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 3px 9px;
  border: 1px solid var(--pcx-line);
  border-radius: 999px;
  background: var(--pcx-surface2);
  color: var(--pcx-text2);
  font-size: 11.5px;
  white-space: nowrap;
}

.${CLASS.chipDot} {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  flex: none;
}

.${CLASS.actions} {
  display: flex;
  gap: 8px;
  align-items: center;
  margin: 12px 0 0;
}

.${CLASS.button} {
  appearance: none;
  border: 1px solid var(--pcx-line2);
  background: var(--pcx-surface2);
  color: var(--pcx-text);
  border-radius: 7px;
  padding: 4px 11px;
  font-size: 12px;
  font-family: inherit;
  cursor: pointer;
}

.${CLASS.button}:hover { border-color: var(--pcx-brand); }
.${CLASS.button}:disabled { opacity: 0.55; cursor: default; }

.${CLASS.buttonPrimary} {
  background: var(--pcx-brand);
  border-color: var(--pcx-brand);
  color: #fff;
}

.${CLASS.tabs} {
  display: flex;
  gap: 2px;
  margin-top: 12px;
  overflow-x: auto;
  scrollbar-width: none;
}

.${CLASS.tabs}::-webkit-scrollbar { display: none; }

.${CLASS.tab} {
  appearance: none;
  border: 0;
  border-bottom: 2px solid transparent;
  background: transparent;
  color: var(--pcx-text2);
  font-family: inherit;
  font-size: 12.5px;
  padding: 7px 11px;
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  white-space: nowrap;
}

.${CLASS.tab}:hover { color: var(--pcx-text); }
.${CLASS.tabActive} { color: var(--pcx-text); border-bottom-color: var(--pcx-brand); }

.${CLASS.tabCount} {
  font-variant-numeric: tabular-nums;
  font-size: 11px;
  color: var(--pcx-text2);
  background: var(--pcx-surface2);
  border-radius: 999px;
  padding: 0 6px;
}

.${CLASS.body} {
  flex: 1 1 auto;
  min-height: 0;
  overflow-y: auto;
  padding: 18px 20px 32px;
  scrollbar-gutter: stable;
}

.${CLASS.view} { display: flex; flex-direction: column; gap: 18px; }

.${CLASS.section} {
  border: 1px solid var(--pcx-line);
  border-radius: var(--pcx-radius);
  background: var(--pcx-surface);
  padding: 14px 16px;
}

.${CLASS.sectionTitle} {
  margin: 0 0 10px;
  font-size: 12px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--pcx-text2);
}

.${CLASS.kpi} {
  display: flex;
  gap: 22px;
  flex-wrap: wrap;
}

.${CLASS.kpiValue} {
  font-size: 21px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.${CLASS.kpiLabel} {
  font-size: 11px;
  color: var(--pcx-text2);
  text-transform: uppercase;
  letter-spacing: 0.05em;
}

.${CLASS.bar} {
  display: flex;
  height: 12px;
  border-radius: 999px;
  overflow: hidden;
  background: var(--pcx-surface2);
  border: 1px solid var(--pcx-line);
}

.${CLASS.barSeg} { height: 100%; min-width: 2px; }

.${CLASS.legend} {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 10px;
}

.${CLASS.legendItem} {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 11.5px;
  color: var(--pcx-text2);
}

.${CLASS.swatch} { width: 9px; height: 9px; border-radius: 3px; flex: none; }

.${CLASS.layer} { display: flex; flex-direction: column; gap: 10px; }

.${CLASS.layerHead} { display: flex; align-items: center; gap: 10px; }

.${CLASS.layerName} {
  font-size: 12px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.07em;
  color: var(--pcx-text2);
  white-space: nowrap;
}

.${CLASS.layerRule} { flex: 1; height: 1px; background: var(--pcx-line); }

.${CLASS.nodes} {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(238px, 1fr));
  gap: 10px;
}

.${CLASS.node} {
  border: 1px solid var(--pcx-line);
  border-left-width: 3px;
  border-radius: 8px;
  background: var(--pcx-surface2);
  padding: 9px 11px;
  cursor: pointer;
  text-align: left;
  font-family: inherit;
  color: inherit;
  display: flex;
  flex-direction: column;
  gap: 5px;
  min-width: 0;
}

.${CLASS.node}:hover { border-color: var(--pcx-line2); background: var(--pcx-overlay); }
.${CLASS.nodeActive} { box-shadow: 0 0 0 1px var(--pcx-brand) inset; }

.${CLASS.nodeTop} { display: flex; align-items: center; gap: 7px; min-width: 0; }

.${CLASS.nodeName} {
  font-weight: 600;
  font-size: 13px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  flex: 1;
}

.${CLASS.nodeKind} { font-size: 11px; color: var(--pcx-text2); font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }

.${CLASS.nodeDeps} { font-size: 11px; color: var(--pcx-text2); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.${CLASS.badge} {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  border-radius: 999px;
  padding: 1px 8px;
  font-size: 11px;
  border: 1px solid var(--pcx-line2);
  color: var(--pcx-text2);
  white-space: nowrap;
}

.${CLASS.dot} { width: 8px; height: 8px; border-radius: 50%; flex: none; }

.${CLASS.mono} { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 11.5px; }

.${CLASS.grid} {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 12px;
}

.${CLASS.card} {
  border: 1px solid var(--pcx-line);
  border-radius: var(--pcx-radius);
  background: var(--pcx-surface);
  padding: 12px 14px;
  min-width: 0;
}

.${CLASS.cardHeader} { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }

.${CLASS.cardTitle} { font-weight: 600; font-size: 13.5px; display: flex; align-items: center; gap: 7px; }

.${CLASS.cardMeta} { color: var(--pcx-text2); font-size: 11.5px; margin-top: 3px; }

.${CLASS.cols} { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 14px; }

.${CLASS.col} { min-width: 0; display: flex; flex-direction: column; gap: 8px; }

.${CLASS.list} { display: flex; flex-direction: column; gap: 5px; }

.${CLASS.listItem} {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  min-width: 0;
}

.${CLASS.treeRow} {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 2px 6px;
  border-radius: 5px;
  font-size: 12px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  min-width: 0;
}

.${CLASS.treeRow}:hover { background: var(--pcx-surface2); }
.${CLASS.treeName} { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.${CLASS.treeMeta} { color: var(--pcx-text2); font-size: 11px; white-space: nowrap; }

.${CLASS.present} { color: var(--pcx-ok); }
.${CLASS.missing} { color: var(--pcx-err); }
.${CLASS.warn} { color: var(--pcx-warn); }
.${CLASS.ok} { color: var(--pcx-ok); }
.${CLASS.err} { color: var(--pcx-err); }
.${CLASS.muted} { color: var(--pcx-text2); }

.${CLASS.warnBox} {
  border: 1px solid var(--pcx-warn);
  border-radius: 8px;
  background: color-mix(in srgb, var(--pcx-warn) 9%, transparent);
  padding: 9px 12px;
  font-size: 12px;
}

.${CLASS.err} { color: var(--pcx-err); }

.${CLASS.empty} {
  border: 1px dashed var(--pcx-line2);
  border-radius: var(--pcx-radius);
  padding: 26px 18px;
  text-align: center;
  color: var(--pcx-text2);
  font-size: 12.5px;
}

.${CLASS.agentRow} {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 7px 9px;
  border: 1px solid var(--pcx-line);
  border-radius: 8px;
  background: var(--pcx-surface2);
  cursor: pointer;
  font-family: inherit;
  color: inherit;
  text-align: left;
  width: 100%;
}

.${CLASS.agentRow}:hover { border-color: var(--pcx-line2); }
.${CLASS.emoji} { font-size: 16px; flex: none; }
.${CLASS.agentName} { font-weight: 600; font-size: 12.5px; }
.${CLASS.agentRole} { color: var(--pcx-text2); font-size: 11.5px; }

.${CLASS.deps} { display: flex; flex-wrap: wrap; gap: 5px; align-items: center; }
.${CLASS.dep} { border: 1px solid var(--pcx-line2); border-radius: 5px; padding: 0 6px; font-size: 11px; font-family: ui-monospace, monospace; color: var(--pcx-text2); }
.${CLASS.arrow} { color: var(--pcx-text2); font-size: 11px; }

.${CLASS.taskCard} {
  border: 1px solid var(--pcx-line);
  border-left-width: 3px;
  border-radius: 8px;
  background: var(--pcx-surface);
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 7px;
  min-width: 0;
}

.${CLASS.taskHead} { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.${CLASS.taskBody} { font-size: 12px; color: var(--pcx-text2); }

.${CLASS.inv} {
  display: flex;
  gap: 9px;
  align-items: flex-start;
  padding: 6px 0;
  border-top: 1px solid var(--pcx-line);
  font-size: 12px;
}

.${CLASS.invId} {
  flex: none;
  font-family: ui-monospace, monospace;
  font-size: 11px;
  color: var(--pcx-text2);
  min-width: 54px;
}

.${CLASS.tag} {
  font-size: 10.5px;
  border: 1px solid var(--pcx-line2);
  border-radius: 4px;
  padding: 0 5px;
  color: var(--pcx-text2);
}

.${CLASS.details} { font-size: 12px; }
.${CLASS.summary} { cursor: pointer; color: var(--pcx-text2); }
.${CLASS.summary}:hover { color: var(--pcx-text); }

.${CLASS.heading} { font-size: 14px; font-weight: 600; margin: 0; }
.${CLASS.headingL2} { margin: 12px 0 4px; font-size: 12.5px; font-weight: 600; }
.${CLASS.headingL3} { margin: 10px 0 4px; font-size: 12px; font-weight: 600; color: var(--pcx-text2); }

.${CLASS.footer} {
  flex: none;
  border-top: 1px solid var(--pcx-line);
  padding: 7px 20px;
  font-size: 11px;
  color: var(--pcx-text2);
  display: flex;
  gap: 14px;
  flex-wrap: wrap;
  background: var(--pcx-surface);
}

.${CLASS.table} { display: flex; flex-direction: column; gap: 2px; }
.${CLASS.row} { display: flex; gap: 10px; align-items: center; padding: 3px 6px; border-radius: 5px; font-size: 12px; min-width: 0; }
.${CLASS.rowClick} { cursor: pointer; font-family: inherit; color: inherit; border: 0; background: transparent; text-align: left; width: 100%; }
.${CLASS.rowClick}:hover { background: var(--pcx-surface2); }
.${CLASS.cell} { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
`
}
