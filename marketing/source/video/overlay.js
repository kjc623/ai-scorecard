// Injected into every recorded page: a visible pointer that follows the mouse (a headless browser
// draws none) and the caption bar. Top frame only. Both are manual popovers, raised again whenever a
// modal dialog opens, so they stay above the dialog's backdrop.
(() => {
  if (window.top !== window) return;
  const css = `
    #demo-cursor, #demo-caption { inset: auto; margin: 0; border: 0; overflow: visible; }
    #demo-cursor { position: fixed; left: 0; top: 0; padding: 0; background: transparent; width: 22px; height: 22px; z-index: 2147483647; pointer-events: none;
      transform: translate(-3px, -2px); transition: opacity .2s; filter: drop-shadow(0 1px 2px rgb(0 0 0 / .35)); }
    #demo-cursor .pulse { position: absolute; left: -11px; top: -12px; width: 30px; height: 30px; border-radius: 50%;
      background: rgb(75 71 201 / .35); transform: scale(0); opacity: 0; }
    #demo-cursor.down .pulse { animation: demo-pulse .45s ease-out; }
    @keyframes demo-pulse { from { transform: scale(.2); opacity: 1; } to { transform: scale(1.4); opacity: 0; } }
    #demo-caption { position: fixed; left: 50%; bottom: 34px; transform: translate(-50%, 8px); max-width: 1040px; z-index: 2147483646;
      pointer-events: none; padding: 14px 24px; border-radius: 14px; background: rgb(19 19 18 / .9); color: #f6f5f1;
      font: 500 21px/1.4 "Inter", system-ui, sans-serif; letter-spacing: -.005em; text-align: center; opacity: 0;
      transition: opacity .35s ease, transform .35s ease; box-shadow: 0 10px 30px -12px rgb(0 0 0 / .45); }
    #demo-caption.on { opacity: 1; transform: translate(-50%, 0); }`;
  const mount = () => {
    if (document.getElementById('demo-cursor')) return;
    const style = document.createElement('style');
    style.textContent = css;
    document.head.append(style);
    const cursor = document.createElement('div');
    cursor.id = 'demo-cursor';
    cursor.innerHTML = '<span class="pulse"></span><svg width="22" height="22" viewBox="0 0 22 22"><path d="M3 2l15 9.2-6.4 1.3 3.8 6.9-2.6 1.4-3.8-6.9L4.2 18.4z" fill="#131312" stroke="#fff" stroke-width="1.4" stroke-linejoin="round"/></svg>';
    const caption = document.createElement('div');
    caption.id = 'demo-caption';
    cursor.popover = 'manual';
    caption.popover = 'manual';
    document.body.append(cursor, caption);
    raise();
  };
  // Showing a popover again puts it on top of everything in the top layer, the cursor last.
  const raise = () => {
    for (const id of ['demo-caption', 'demo-cursor']) {
      const el = document.getElementById(id);
      if (!el?.showPopover) continue;
      if (el.matches(':popover-open')) el.hidePopover();
      el.showPopover();
    }
  };
  new MutationObserver((changes) => {
    if (changes.some((c) => c.target.tagName === 'DIALOG' && c.target.open)) raise();
  }).observe(document, { subtree: true, attributes: true, attributeFilter: ['open'] });
  const place = (x, y) => { mount(); document.getElementById('demo-cursor').style.left = `${x}px`; document.getElementById('demo-cursor').style.top = `${y}px`; };
  window.__demo = {
    cursor: place,
    caption(text) {
      mount();
      const el = document.getElementById('demo-caption');
      if (!text) { el.classList.remove('on'); return; }
      el.textContent = text;
      el.classList.add('on');
    },
  };
  addEventListener('mousemove', (e) => place(e.clientX, e.clientY), true);
  addEventListener('mousedown', () => {
    const el = document.getElementById('demo-cursor');
    if (!el) return;
    el.classList.remove('down'); void el.offsetWidth; el.classList.add('down');
  }, true);
  if (document.readyState === 'loading') addEventListener('DOMContentLoaded', mount); else mount();
})();
