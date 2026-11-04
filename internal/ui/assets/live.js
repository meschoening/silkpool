// The live-update script (ADR 0009). The dashboard and the tray app
// render their pages in Go; this morphs what they push into the page,
// so scroll, focus, form values being edited, and the Starting and
// Draining animations carry on.
//
// - Parts are HTML: elements, each with an id. silkpool.apply(html)
//   morphs each into the page's element of the same id. Inside it,
//   children match by data-key, else id, else kind and position.
// - The dashboard: a page whose body is marked data-stream has a stream
//   of server-sent events at its own URL. Each message holds the parts
//   that changed, and a reload event means the page went elsewhere.
// - The tray app hands the parts its Wails events carry to
//   silkpool.apply.
// - A form that posts to this page's origin is posted by fetch, and the
//   answer is morphed in: a refusal's page, or the page a redirect lands
//   on. Other forms, such as the App manifest's to GitHub, and forms
//   marked data-native, are posted by the browser.
// - A button with data-copy copies its text to the clipboard.
(() => {
  'use strict';

  const keyOf = (n) => (n.nodeType === Node.ELEMENT_NODE ? n.getAttribute('data-key') || n.id || null : null);
  const same = (a, b) => a.nodeType === b.nodeType && a.nodeName === b.nodeName && keyOf(a) === keyOf(b);

  // update makes el, which matched want, what want is. It sets only
  // attributes: an input, checkbox or option the user changed keeps the
  // value they gave it, as browsers do when a value attribute changes.
  function update(el, want) {
    if (el.nodeType !== Node.ELEMENT_NODE) {
      if (el.nodeValue !== want.nodeValue) el.nodeValue = want.nodeValue;
      return;
    }
    for (const a of [...el.attributes]) if (!want.hasAttribute(a.name)) el.removeAttribute(a.name);
    for (const a of want.attributes) if (el.getAttribute(a.name) !== a.value) el.setAttribute(a.name, a.value);
    morph(el, want);
  }

  // morph makes from's children to's, keeping each that matches in
  // place: a moved element restarts its animations, so texts and elements
  // the new render drops go instead, and only a reorder moves one.
  function morph(from, to) {
    const wants = [...to.childNodes];
    const kept = new Set(wants.map(keyOf).filter(Boolean));
    const dropped = (n) => keyOf(n) !== null && !kept.has(keyOf(n));
    const spare = (n) => n.nodeType !== Node.ELEMENT_NODE || dropped(n);
    let cur = from.firstChild;
    const drop = () => {
      const gone = cur;
      cur = cur.nextSibling;
      gone.remove();
    };
    for (const want of wants) {
      if (keyOf(want) !== null) {
        let old = cur;
        while (old && !same(old, want)) old = old.nextSibling;
        if (!old) {
          from.insertBefore(want, cur);
          continue;
        }
        while (cur !== old && spare(cur)) drop();
        if (cur === old) cur = cur.nextSibling;
        else from.insertBefore(old, cur);
        update(old, want);
        continue;
      }
      while (cur && dropped(cur)) drop();
      if (cur && same(cur, want)) {
        update(cur, want);
        cur = cur.nextSibling;
      } else {
        from.insertBefore(want, cur);
      }
    }
    while (cur) drop();
  }

  // apply morphs each part in html into the page's element of the same
  // id. A part the page doesn't show is left out.
  function apply(html) {
    const t = document.createElement('template');
    t.innerHTML = html;
    for (const part of [...t.content.children]) {
      const el = part.id && document.getElementById(part.id);
      if (el) update(el, part);
    }
  }

  // The page's stream, open while the page is marked data-stream and
  // shown.
  let stream = null;
  let streamURL = '';
  const here = () => location.href.split('#')[0];
  // shown is the page's path and query, which only a form's redirect
  // changes without loading a page.
  let shown = location.pathname + location.search;
  function connect() {
    const want = document.body.hasAttribute('data-stream') && !document.hidden ? here() : '';
    if (want === streamURL) return;
    if (stream) stream.close();
    stream = null;
    streamURL = want;
    if (!want) return;
    const es = (stream = new EventSource(want));
    let opened = false;
    es.onopen = () => {
      opened = true;
    };
    es.onmessage = (e) => apply(e.data);
    es.addEventListener('reload', () => {
      es.close();
      location.reload();
    });
    // The Hub refused the stream: the page went elsewhere, as when a
    // restart ended the sign-in, so load it again. A page whose stream
    // never opened stays as it is, so the two can't take turns.
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED && opened) location.reload();
    };
  }

  // show morphs res, the answer to form's post, into the page.
  function show(res, text, form) {
    if (!(res.headers.get('Content-Type') || '').startsWith('text/html')) {
      // An answer that isn't a page, such as a failure, shows as the
      // browser shows it.
      const pre = document.createElement('pre');
      pre.textContent = text;
      document.body.replaceChildren(pre);
      document.body.removeAttribute('data-stream');
    } else {
      const page = new DOMParser().parseFromString(text, 'text/html');
      if (res.redirected && res.url !== here()) {
        history.pushState(null, '', res.url);
        shown = location.pathname + location.search;
        scrollTo(0, 0);
      }
      document.title = page.title;
      update(document.body, page.body);
      // The form now shows what the Hub has, not what was typed.
      if (form.isConnected) form.reset();
      const alert = !res.ok && document.querySelector('[role="alert"]');
      if (alert) alert.scrollIntoView({ block: 'nearest' });
    }
    connect();
  }

  const busy = new WeakSet();
  const native = new WeakSet();
  document.addEventListener('submit', async (e) => {
    const form = e.target;
    const by = e.submitter;
    if (native.delete(form) || form.hasAttribute('data-native')) return;
    const attr = (name) => (by && by.getAttribute('form' + name)) || form.getAttribute(name);
    const url = new URL(attr('action') || here(), document.baseURI);
    if ((attr('method') || '').toLowerCase() !== 'post' || url.origin !== location.origin || (attr('target') || '_self') !== '_self') return;
    e.preventDefault();
    if (busy.has(form)) return;
    busy.add(form);
    try {
      let res;
      try {
        res = await fetch(url, { method: 'POST', body: new URLSearchParams(new FormData(form, by)) });
      } catch {
        // The Hub didn't answer: the browser posts the form itself, and
        // shows why it can't.
        native.add(form);
        form.requestSubmit(by);
        return;
      }
      show(res, await res.text(), form);
    } finally {
      busy.delete(form);
    }
  });

  // A Copy button copies the text it carries, and says so for a moment.
  // Where the clipboard is out of reach, the line beside it is selected
  // instead, to copy by hand.
  document.addEventListener('click', async (e) => {
    const b = e.target instanceof Element && e.target.closest('button[data-copy]');
    if (!b) return;
    const label = b.textContent;
    try {
      await navigator.clipboard.writeText(b.getAttribute('data-copy'));
      b.textContent = 'Copied';
      setTimeout(() => {
        b.textContent = label;
      }, 1500);
    } catch {
      const line = b.previousElementSibling;
      if (!line) return;
      const range = document.createRange();
      range.selectNodeContents(line);
      getSelection().removeAllRanges();
      getSelection().addRange(range);
    }
  });

  // Back and forward return to a page a form's redirect pushed: load it.
  addEventListener('popstate', () => {
    if (location.pathname + location.search !== shown) location.reload();
  });
  document.addEventListener('visibilitychange', connect);
  connect();

  window.silkpool = Object.freeze({ apply });
})();
