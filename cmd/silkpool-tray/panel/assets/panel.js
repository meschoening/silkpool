// The tray panel's script, beside the shared live-update script
// (package ui's live.js), which it needs loaded first:
//
// - The app pushes the parts that changed as "panel" Wails events,
//   since WebView2 holds server-sent events back until their response
//   ends. They go to silkpool.apply, which morphs them in.
// - The window is as tall as the panel: its height goes to the app as a
//   "panel:height" event whenever it changes, as when the buttons wrap
//   or the editor opens.
// - A link that opens a new window, such as a job's run or the
//   dashboard, opens in the browser instead.
(() => {
  'use strict';

  let wails = null;
  let height = 0;
  const panel = document.querySelector('.pnl');

  function fit() {
    const h = Math.ceil(panel.getBoundingClientRect().height);
    if (!wails || h === height) return;
    height = h;
    wails.Events.Emit('panel:height', h);
  }

  document.addEventListener('click', (e) => {
    const a = e.target instanceof Element && e.target.closest('a[target="_blank"]');
    if (!a) return;
    e.preventDefault();
    const url = new URL(a.href);
    if (wails && (url.protocol === 'https:' || url.protocol === 'http:')) wails.Browser.OpenURL(url.href);
  });

  // The runtime is Wails' own, which its asset server serves beside the
  // panel's files.
  import('/wails/runtime.js').then((rt) => {
    wails = rt;
    rt.Events.On('panel', (e) => window.silkpool.apply(e.data));
    new ResizeObserver(fit).observe(panel);
    fit();
  });
})();
