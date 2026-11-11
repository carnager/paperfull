// paperfull UI behaviour. Pages work without it; this makes them pleasant.
(() => {
  'use strict';
  let pendingRefresh = false; // a live update waits until the user is done selecting
  const myTasks = new Set(); // uploads from this tab already show their own progress
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const icon = name => `<svg class="i"><use href="#i-${name}"/></svg>`;
  const esc = s => String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const store = {
    get: k => { try { return localStorage.getItem(k); } catch { return null; } },
    set: (k, v) => { try { localStorage.setItem(k, v); } catch {} },
  };
  const typing = () => {
    const el = document.activeElement;
    return el && (/^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName) || el.isContentEditable);
  };

  // ---------- toasts ----------
  function toast(msg, href, kind) {
    const el = document.createElement(href ? 'a' : 'div');
    el.className = 'toast' + (kind ? ' ' + kind : '');
    if (href) el.href = href;
    el.innerHTML = icon(kind === 'error' ? 'x' : kind === 'new' ? 'inbox' : 'check');
    el.append(msg);
    $('#toasts').append(el);
    setTimeout(() => { el.classList.add('hide'); setTimeout(() => el.remove(), 300); }, href ? 6000 : 3200);
  }
  {
    const p = new URLSearchParams(location.search);
    if (p.has('msg')) {
      toast(p.get('msg'));
      p.delete('msg');
      history.replaceState(history.state, '', location.pathname + (p.size ? '?' + p : '') + location.hash);
    }
  }

  // ---------- confirmation dialog ----------
  const confirmDlg = $('#confirm');
  document.addEventListener('click', e => {
    const b = e.target.closest('[data-confirm]');
    if (!b || b.dataset.confirmed) return;
    e.preventDefault();
    $('#confirm-title').textContent = b.dataset.confirmTitle || 'Are you sure?';
    $('#confirm-text').textContent = b.dataset.confirm;
    $('#confirm-ok').textContent = b.dataset.confirmOk || 'Delete';
    confirmDlg.returnValue = '';
    confirmDlg.showModal();
    confirmDlg.addEventListener('close', () => {
      if (confirmDlg.returnValue !== 'ok') return;
      b.dataset.confirmed = '1';
      if (b.form) b.form.requestSubmit(b); else b.click();
      delete b.dataset.confirmed;
    }, { once: true });
  });

  // ---------- modals ----------
  document.addEventListener('click', e => {
    const opener = e.target.closest('[data-open]');
    if (opener) {
      const dlg = document.getElementById(opener.dataset.open);
      if (dlg) { dlg.showModal(); dlg.querySelector('input:not([type=hidden]):not([type=radio])')?.focus(); }
      return;
    }
    if (e.target.closest('[data-close]')) { e.target.closest('dialog')?.close(); return; }
    if (e.target instanceof HTMLDialogElement) {
      const r = e.target.getBoundingClientRect();
      const inside = e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom;
      if (!inside) e.target.close();
    }
  });

  // ---------- mobile navigation ----------
  document.addEventListener('click', e => {
    if (e.target.closest('[data-nav-open]')) document.body.classList.add('nav-open');
    else if (e.target.closest('[data-nav-close]')) document.body.classList.remove('nav-open');
  });

  // ---------- popover menus ----------
  // Native popovers open in the top layer; place them under the button that opened them.
  let lastInvoker = null;
  document.addEventListener('click', e => {
    const inv = e.target.closest('[popovertarget]');
    if (inv) lastInvoker = inv;
  }, true);
  document.addEventListener('beforetoggle', e => {
    const pop = e.target;
    if (!(pop instanceof HTMLElement) || !pop.matches('[popover]')) return;
    if (e.newState === 'open') pop.style.visibility = 'hidden';
  }, true);
  document.addEventListener('toggle', e => {
    const pop = e.target;
    if (!(pop instanceof HTMLElement) || !pop.matches('[popover]') || e.newState !== 'open') return;
    const anchor = e.source || (lastInvoker && lastInvoker.getAttribute('popovertarget') === pop.id ? lastInvoker : document.querySelector(`[popovertarget="${pop.id}"]`));
    if (anchor) {
      const r = anchor.getBoundingClientRect();
      const w = pop.offsetWidth, h = pop.offsetHeight;
      let left = Math.max(8, Math.min(r.left, innerWidth - w - 8));
      let top = r.bottom + 6;
      if (top + h > innerHeight - 8 && r.top - h - 6 > 8) top = r.top - h - 6;
      pop.style.left = left + 'px';
      pop.style.top = Math.max(8, top) + 'px';
    }
    pop.style.visibility = '';
    const find = pop.querySelector('.menu-find, .form-menu input');
    if (find) { find.value = find.classList.contains('menu-find') ? '' : find.value; find.dispatchEvent(new Event('input')); find.focus(); }
  }, true);
  window.addEventListener('scroll', () => $$('[popover]:popover-open').forEach(p => p.hidePopover()), { passive: true });
  window.addEventListener('resize', () => $$('[popover]:popover-open').forEach(p => p.hidePopover()));
  // Type-to-filter inside menus; arrow keys and Enter pick an entry.
  document.addEventListener('input', e => {
    if (!e.target.matches('.menu-find')) return;
    const q = e.target.value.toLowerCase();
    $$('.menu-items > a', e.target.closest('.menu')).forEach(a => { a.hidden = q && !a.textContent.toLowerCase().includes(q); });
  });
  document.addEventListener('keydown', e => {
    const menu = e.target.closest?.('.menu');
    if (!menu) return;
    if (e.target.matches('.form-menu input') && e.key === 'Enter') {
      e.preventDefault();
      menu.querySelector('.btn.primary')?.click();
      return;
    }
    if (e.target.matches('.menu-find') && e.key === 'Enter') {
      e.preventDefault();
      menu.querySelector('.menu-items > a:not([hidden])')?.click();
    }
  });

  // ---------- document list ----------
  const listview = $('#listview');
  const searchForm = $('#searchform');
  const searchInput = $('#search');

  function applyView() {
    const docs = $('#docs');
    const view = store.get('pf-view') || 'grid';
    if (docs) docs.classList.toggle('as-list', view === 'list');
    $$('[data-view]').forEach(b => b.classList.toggle('on', b.dataset.view === view));
  }
  document.addEventListener('click', e => {
    const b = e.target.closest('[data-view]');
    if (!b) return;
    store.set('pf-view', b.dataset.view);
    applyView();
  });
  applyView();

  function updateSelection() {
    const boxes = $$('input[name=ids]');
    const n = boxes.filter(b => b.checked).length;
    const bar = $('#bulkbar');
    if (bar) bar.hidden = n === 0;
    const c = $('#selcount');
    if (c) c.textContent = `${n} selected`;
    const all = $('#selectall');
    if (all) { all.checked = n > 0 && n === boxes.length; all.indeterminate = n > 0 && n < boxes.length; }
    document.body.classList.toggle('selecting', n > 0);
    if (n === 0 && pendingRefresh) refreshNow();
  }
  function clearSelection() {
    $$('input[name=ids]').forEach(b => { b.checked = false; });
    updateSelection();
  }
  let lastChecked = null;
  document.addEventListener('click', e => {
    const box = e.target.closest('input[name=ids]');
    if (box && e.shiftKey && lastChecked && lastChecked !== box) {
      // Shift-click selects a range.
      const boxes = $$('input[name=ids]');
      const [a, b] = [boxes.indexOf(lastChecked), boxes.indexOf(box)].sort((x, y) => x - y);
      boxes.slice(a, b + 1).forEach(x => { x.checked = box.checked; });
    }
    if (box) lastChecked = box;
    if (e.target.closest('[data-clear-selection]')) clearSelection();
  });
  document.addEventListener('change', e => {
    if (e.target.id === 'selectall') $$('input[name=ids]').forEach(b => { b.checked = e.target.checked; });
    if (e.target.matches('input[name=ids], #selectall')) updateSelection();
  });

  // Search as you type.
  let searchTimer, searchCtrl;
  async function refreshList(url) {
    if (!listview) return;
    searchCtrl?.abort();
    searchCtrl = new AbortController();
    try {
      const r = await fetch(url + (url.includes('?') ? '&' : '?') + 'partial=1', { signal: searchCtrl.signal });
      if (!r.ok) return;
      listview.innerHTML = await r.text();
      applyView();
      updateSelection();
    } catch (err) {
      if (err.name !== 'AbortError') console.error(err);
    }
  }
  function searchURL() {
    const p = new URLSearchParams(new FormData(searchForm));
    if (!p.get('text')) p.delete('text');
    const qs = p.toString();
    return searchForm.getAttribute('action') + (qs ? '?' + qs : '');
  }
  if (searchForm && listview) {
    searchInput.addEventListener('input', () => {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => {
        const url = searchURL();
        history.replaceState(null, '', url);
        refreshList(url);
      }, 200);
    });
    searchForm.addEventListener('submit', e => {
      e.preventDefault();
      clearTimeout(searchTimer);
      const url = searchURL();
      history.replaceState(null, '', url);
      refreshList(url);
    });
  }

  // ---------- keyboard ----------
  document.addEventListener('keydown', e => {
    if (e.key === '/' && !typing() && searchInput) {
      e.preventDefault();
      searchInput.focus();
      searchInput.select();
    }
    if (e.key === 'Escape' && !typing() && document.body.classList.contains('selecting')) clearSelection();
  });

  // ---------- uploads ----------
  const fileInput = $('#upload-input');
  const panel = $('#uploads');
  const plist = $('#uploads-list');
  let pending = 0;
  document.addEventListener('click', e => {
    if (e.target.closest('[data-upload]')) { document.body.classList.remove('nav-open'); fileInput.click(); }
    if (e.target.closest('[data-uploads-close]')) { panel.hidden = true; plist.innerHTML = ''; }
  });
  fileInput?.addEventListener('change', () => { upload(fileInput.files); fileInput.value = ''; });

  const states = {
    queued: 'Waiting…',
    processing: 'Reading the text…',
    done: 'Added to your inbox',
    duplicate: 'Already in your archive',
    failed: 'Could not be added',
    password: 'Needs a password',
  };
  function setRow(li, state, text, docId) {
    li.className = state === 'done' || state === 'duplicate' ? 'done' : state === 'failed' ? 'failed' : state === 'password' ? 'locked' : '';
    const busy = !li.className;
    const ic = { failed: 'x', locked: 'lock' }[li.className] || 'check';
    li.querySelector('.u-lead').outerHTML = busy ? '<span class="spinner u-lead"></span>'
      : `<svg class="i u-icon u-lead"><use href="#i-${ic}"/></svg>`;
    li.querySelector('.u-state').textContent = text;
    const link = li.querySelector('.u-link');
    link.innerHTML = docId ? `<a href="/documents/${docId}">Open</a>` : '';
    if (state === 'password') {
      const b = document.createElement('button');
      b.type = 'button';
      b.textContent = 'Unlock';
      b.dataset.unlock = li.dataset.task;
      b.dataset.file = li.dataset.file;
      link.append(b);
    }
  }
  async function upload(files) {
    files = [...files];
    if (!files.length) return;
    panel.hidden = false;
    $('#uploads-title').textContent = 'Adding documents';
    for (const f of files) {
      const li = document.createElement('li');
      li.dataset.file = f.name;
      li.innerHTML = `<span class="spinner u-lead"></span><span class="u-main"><span class="u-name">${esc(f.name)}</span><span class="u-state">Uploading…</span></span><span class="u-link"></span>`;
      plist.prepend(li);
      pending++;
      send(f, li);
    }
  }
  async function send(f, li) {
    const fd = new FormData();
    fd.append('document', f, f.name);
    try {
      const r = await fetch('/api/documents', { method: 'POST', body: fd });
      const body = await r.json();
      if (!r.ok) throw new Error(body.error || r.statusText);
      myTasks.add(body.id);
      li.dataset.task = body.id;
      setRow(li, body.status, states[body.status]);
      poll(body.id, li);
    } catch (err) {
      setRow(li, 'failed', String(err.message || err));
      finished();
    }
  }
  async function poll(id, li) {
    for (;;) {
      await new Promise(r => setTimeout(r, 900));
      let t;
      try { t = await (await fetch('/api/tasks/' + id)).json(); } catch { continue; }
      if (t.status === 'queued' || t.status === 'processing') { setRow(li, t.status, states[t.status]); continue; }
      const text = t.status === 'failed' ? (t.error || states.failed).replace(/^unsupported file type.*/, "This file type isn't supported") : states[t.status];
      setRow(li, t.status, text, t.document_id);
      if (t.status === 'password') askPassword(t);
      finished();
      return;
    }
  }
  async function finished() {
    if (--pending > 0) return;
    $('#uploads-title').textContent = 'All done';
    try {
      const st = await (await fetch('/api/stats')).json();
      updateCounts(st);
    } catch {}
    if (listview) refreshList(location.pathname + location.search);
  }
  function updateCounts(st) {
    const inbox = $('.nav a[href="/inbox"]');
    let c = inbox?.querySelector('.count');
    if (inbox && st.inbox > 0 && !c) { c = document.createElement('span'); c.className = 'count hot'; inbox.append(c); }
    if (c) { c.textContent = st.inbox; c.hidden = st.inbox === 0; }
    const all = $('.nav a[href="/"] .count');
    if (all) all.textContent = st.documents;
  }

  // Drag and drop anywhere.
  const zone = $('#dropzone');
  let depth = 0;
  const hasFiles = e => e.dataTransfer && [...e.dataTransfer.types].includes('Files');
  window.addEventListener('dragenter', e => { if (hasFiles(e)) { depth++; zone.hidden = false; } });
  window.addEventListener('dragleave', e => { if (hasFiles(e) && --depth <= 0) { depth = 0; zone.hidden = true; } });
  window.addEventListener('dragover', e => { if (hasFiles(e)) e.preventDefault(); });
  window.addEventListener('drop', e => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    depth = 0;
    zone.hidden = true;
    upload(e.dataTransfer.files);
  });

  // ---------- document page ----------
  const docform = $('#docform');
  let docState = null;
  if (docform) {
    // Tag input with suggestions.
    const tf = $('[data-tagfield]');
    if (tf) tagField(tf, JSON.parse($('#tagdata')?.textContent || '[]') || []);

    // Grow notes with their content.
    $$('[data-autosize]').forEach(t => {
      const fit = () => { t.style.height = 'auto'; t.style.height = (t.scrollHeight + 2) + 'px'; };
      t.addEventListener('input', fit);
      fit();
    });

    // Warn before leaving with unsaved changes.
    const snapshot = () => new URLSearchParams(new FormData(docform)).toString();
    let initial = snapshot();
    let submitting = false;
    const dirty = () => !submitting && snapshot() !== initial;
    docform.addEventListener('submit', () => { submitting = true; });
    docState = {
      id: Number(docform.getAttribute('action').split('/').pop()),
      busy: () => submitting || snapshot() !== initial || typing(),
    };
    window.addEventListener('beforeunload', e => { if (dirty()) { e.preventDefault(); e.returnValue = ''; } });

    docform.addEventListener('keydown', e => {
      if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
        e.preventDefault();
        docform.requestSubmit($('[data-primary]', docform));
      } else if (e.key === 'Enter' && e.target.matches('input:not(.tf-input)')) {
        // Enter in a field saves without leaving the page.
        e.preventDefault();
        docform.requestSubmit($('[data-save]', docform));
      }
    });

    // Back returns to the list as it was (search, scroll) when we came from there.
    const back = $('[data-back]');
    const goBack = () => {
      let ref = null;
      try { ref = document.referrer && new URL(document.referrer); } catch {}
      if (ref && ref.origin === location.origin && (ref.pathname === '/' || ref.pathname === '/inbox') && history.length > 1) history.back();
      else location.href = back.getAttribute('href');
    };
    back?.addEventListener('click', e => { e.preventDefault(); goBack(); });
    document.addEventListener('keydown', e => {
      if (e.key === 'Escape' && !typing() && !$('dialog[open]') && !$('[popover]:popover-open')) goBack();
    });
  }

  function tagField(root, all) {
    const hidden = root.querySelector('input[name=tags]');
    const known = name => all.find(t => t.name.toLowerCase() === name.toLowerCase());
    let tags = hidden.value.split(',').map(s => s.trim()).filter(Boolean);
    root.classList.add('enhanced');
    const input = document.createElement('input');
    input.className = 'tf-input';
    input.placeholder = 'Add a tag…';
    input.autocomplete = 'off';
    input.setAttribute('aria-label', 'Add a tag');
    const list = document.createElement('div');
    list.className = 'tf-list';
    list.hidden = true;
    root.append(input, list);
    let options = [], active = 0;

    const sync = () => { hidden.value = tags.join(', '); };
    function render() {
      $$('.tag', root).forEach(el => el.remove());
      for (const name of tags) {
        const el = document.createElement('span');
        el.className = 'tag';
        const c = known(name)?.color;
        if (c) el.style.setProperty('--c', c);
        el.append(name);
        const x = document.createElement('button');
        x.type = 'button';
        x.title = `Remove ${name}`;
        x.innerHTML = icon('x');
        x.addEventListener('click', e => { e.stopPropagation(); tags = tags.filter(t => t !== name); render(); sync(); input.focus(); });
        el.append(x);
        root.insertBefore(el, input);
      }
      input.placeholder = tags.length ? 'Add…' : 'Add a tag…';
    }
    function add(name) {
      name = name.replace(/,/g, ' ').trim();
      if (!name) return;
      name = known(name)?.name || name;
      if (!tags.some(t => t.toLowerCase() === name.toLowerCase())) tags.push(name);
      input.value = '';
      render(); sync(); suggest();
    }
    function suggest() {
      const q = input.value.trim().toLowerCase();
      options = all
        .filter(t => !tags.some(x => x.toLowerCase() === t.name.toLowerCase()) && (!q || t.name.toLowerCase().includes(q)))
        .sort((a, b) => (b.name.toLowerCase().startsWith(q) - a.name.toLowerCase().startsWith(q)))
        .slice(0, 8)
        .map(t => ({ name: t.name, color: t.color }));
      if (q && !known(q)) options.push({ name: input.value.trim(), create: true });
      active = 0;
      draw();
    }
    function draw() {
      list.innerHTML = '';
      if (!options.length || document.activeElement !== input) { list.hidden = true; return; }
      options.forEach((o, i) => {
        const b = document.createElement('button');
        b.type = 'button';
        b.tabIndex = -1;
        if (i === active) b.classList.add('active');
        if (o.create) { b.classList.add('create'); b.innerHTML = icon('plus'); b.append(`Create “${o.name}”`); }
        else { b.innerHTML = `<i class="dot"${o.color ? ` style="--c:${esc(o.color)}"` : ''}></i>`; b.append(o.name); }
        b.addEventListener('mousedown', e => { e.preventDefault(); add(o.name); });
        list.append(b);
      });
      list.hidden = false;
    }
    input.addEventListener('input', suggest);
    input.addEventListener('focus', suggest);
    input.addEventListener('blur', () => { if (input.value.trim()) add(input.value); list.hidden = true; });
    input.addEventListener('keydown', e => {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        if (!options.length) return;
        e.preventDefault();
        active = (active + (e.key === 'ArrowDown' ? 1 : -1) + options.length) % options.length;
        draw();
      } else if ((e.key === 'Enter' || e.key === ',' || (e.key === 'Tab' && input.value.trim())) && !e.ctrlKey && !e.metaKey) {
        if (!input.value.trim() && e.key === 'Enter' && list.hidden) return; // let Enter save the form
        e.preventDefault();
        if (e.key === ',' || !options.length) add(input.value);
        else add(options[active].name);
      } else if (e.key === 'Backspace' && !input.value && tags.length) {
        tags.pop(); render(); sync(); suggest();
      } else if (e.key === 'Escape' && !list.hidden) {
        e.stopPropagation();
        list.hidden = true;
      }
    });
    root.addEventListener('click', e => { if (e.target === root) input.focus(); });
    render();
  }

  // ---------- organize ----------
  function syncAlgo(select) {
    const form = select.closest('form');
    const a = select.value;
    const needsText = ['any', 'all', 'literal', 'regex'].includes(a);
    const match = form.querySelector('[data-match]');
    match.hidden = !needsText;
    match.querySelector('input').required = needsText;
    form.querySelector('[data-match-label]').textContent = { any: 'Words', all: 'Words', literal: 'Exact text', regex: 'Pattern' }[a] || 'Words';
    $$('[data-hint-for]', form).forEach(h => { h.hidden = h.dataset.hintFor !== a; });
  }
  $$('[data-algo]').forEach(s => { syncAlgo(s); s.addEventListener('change', () => syncAlgo(s)); });
  $$('[data-filter-list]').forEach(input => {
    const list = $(input.dataset.filterList);
    input.addEventListener('input', () => {
      const q = input.value.toLowerCase();
      $$('li', list).forEach(li => { li.hidden = q && !li.dataset.name.toLowerCase().includes(q); });
    });
  });

  // ---------- AI fill-in ----------
  document.addEventListener('click', async e => {
    const b = e.target.closest('[data-ai]');
    if (!b || b.disabled) return;
    const label = b.querySelector('span');
    const before = label.textContent;
    b.disabled = true;
    b.classList.add('busy');
    label.textContent = 'Reading the document…';
    try {
      const r = await fetch(b.dataset.ai, { method: 'POST' });
      const s = await r.json();
      if (!r.ok) throw new Error(s.error || r.statusText);
      const set = (name, v) => {
        const el = docform?.elements[name];
        if (!el || !v || el.value === v) return;
        el.value = v;
        el.classList.remove('filled'); void el.offsetWidth; el.classList.add('filled');
      };
      set('sender', s.sender);
      set('type', s.type);
      set('title', s.title);
      set('created', s.date);
      toast(s.sender || s.type || s.title ? 'Filled in. Check and save.' : 'The AI found nothing useful.');
    } catch (err) {
      toast(String(err.message || err), null, 'error');
    } finally {
      b.disabled = false;
      b.classList.remove('busy');
      label.textContent = before;
    }
  });

  // ---------- live updates ----------
  // The server pushes an event whenever a document is added, changed or
  // deleted, so every open page stays current without reloading.
  const onActivity = !!$('.tasklist') || location.pathname === '/activity';
  let changeTimer, reloadTimer;

  function refreshNow() {
    pendingRefresh = false;
    if (listview) refreshList(location.pathname + location.search);
  }
  function onChanged(ids) {
    clearTimeout(changeTimer);
    changeTimer = setTimeout(async () => {
      try { updateCounts(await (await fetch('/api/stats')).json()); } catch {}
      if (listview) {
        // Don't pull the rug out while someone is selecting or using a menu.
        if (document.body.classList.contains('selecting') || $('[popover]:popover-open')) pendingRefresh = true;
        else refreshNow();
      }
      if (docState && ids?.includes(docState.id) && !docState.busy()) location.reload();
    }, 350);
  }
  function onTask(t) {
    if (onActivity) scheduleReload();
    if (t.status === 'password') askPassword(t);
    if (t.status === 'unlocked' || t.status === 'discarded') forgetLocked(t.id);
    if (t.source !== 'folder' || myTasks.has(t.id)) return;
    if (t.status === 'done') toast(`New in your inbox: ${t.file}`, `/documents/${t.document_id}`, 'new');
    else if (t.status === 'duplicate') toast(`${t.file} is already in your archive`, `/documents/${t.document_id}`);
    else if (t.status === 'failed') toast(`Couldn't add ${t.file}`, '/activity', 'error');
  }
  function scheduleReload() {
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(() => {
      // Never reload under someone typing a password or confirming something.
      if ($('dialog[open]')) scheduleReload();
      else location.reload();
    }, 300);
  }

  // ---------- locked PDFs ----------
  const unlockDlg = $('#unlock');
  const lockedQueue = [];
  let lockedNow = null;
  const skippedKey = 'pf-skipped-locked';
  const skipped = () => { try { return JSON.parse(sessionStorage.getItem(skippedKey) || '[]'); } catch { return []; } };
  const skip = id => { try { sessionStorage.setItem(skippedKey, JSON.stringify([...skipped(), id])); } catch {} };

  function askPassword(t, force) {
    if (!unlockDlg || (!force && skipped().includes(t.id))) return;
    if (lockedNow?.id === t.id || lockedQueue.some(x => x.id === t.id)) return;
    lockedQueue.push({ id: t.id, file: t.file });
    showNextLocked();
  }
  function showNextLocked() {
    if (lockedNow || !lockedQueue.length || $('dialog[open]')) return;
    lockedNow = lockedQueue.shift();
    $('#unlock-file').textContent = lockedNow.file;
    $('#unlock-error').hidden = true;
    $('#unlock-password').value = '';
    unlockDlg.showModal();
    $('#unlock-password').focus();
  }
  function closeLocked() {
    lockedNow = null;
    if (unlockDlg.open) unlockDlg.close();
    setTimeout(showNextLocked, 150);
  }
  function forgetLocked(id) {
    const i = lockedQueue.findIndex(x => x.id === id);
    if (i >= 0) lockedQueue.splice(i, 1);
    if (lockedNow?.id === id) closeLocked();
  }
  if (unlockDlg) {
    $('#unlock-form').addEventListener('submit', async e => {
      e.preventDefault();
      if (!lockedNow) return;
      const cur = lockedNow, ok = $('#unlock-ok');
      ok.disabled = true;
      try {
        const r = await fetch(`/api/tasks/${cur.id}/unlock`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ password: $('#unlock-password').value }),
        });
        const body = await r.json();
        if (!r.ok) {
          $('#unlock-error').textContent = body.error || r.statusText;
          $('#unlock-error').hidden = false;
          $('#unlock-password').select();
          return;
        }
        myTasks.add(body.id);
        // Keep following it in the upload panel if it was uploaded here.
        const li = $(`#uploads-list li[data-task="${cur.id}"]`);
        if (li) { li.dataset.task = body.id; pending++; setRow(li, 'queued', 'Unlocked, reading the text…'); poll(body.id, li); }
        else toast(`Unlocked “${cur.file}”, it shows up in your inbox in a moment.`);
        closeLocked();
      } catch (err) {
        $('#unlock-error').textContent = String(err.message || err);
        $('#unlock-error').hidden = false;
      } finally {
        ok.disabled = false;
      }
    });
    $('#unlock-later').addEventListener('click', () => { if (lockedNow) skip(lockedNow.id); closeLocked(); });
    $('#unlock-discard').addEventListener('click', async () => {
      if (!lockedNow) return;
      const cur = lockedNow;
      await fetch(`/api/tasks/${cur.id}`, { method: 'DELETE' });
      toast(`Discarded “${cur.file}”`);
      closeLocked();
    });
    unlockDlg.addEventListener('cancel', () => { if (lockedNow) skip(lockedNow.id); lockedNow = null; setTimeout(showNextLocked, 150); });
    document.addEventListener('click', e => {
      const b = e.target.closest('[data-unlock]');
      if (b) askPassword({ id: b.dataset.unlock, file: b.dataset.file }, true);
    });
    // Ask about PDFs that got locked while this page wasn't open.
    if (!document.body.classList.contains('center')) {
      fetch('/api/tasks').then(r => r.ok ? r.json() : []).then(ts => ts.filter(t => t.status === 'password').reverse().forEach(t => askPassword(t))).catch(() => {});
    }
  }

  document.addEventListener('toggle', e => {
    if (e.newState === 'closed' && pendingRefresh && !document.body.classList.contains('selecting')) refreshNow();
  }, true);

  if (window.EventSource && !document.body.classList.contains('center')) {
    const es = new EventSource('/api/events');
    es.onmessage = e => {
      let ev;
      try { ev = JSON.parse(e.data); } catch { return; }
      if (ev.type === 'task' && ev.task) onTask(ev.task);
      else if (ev.type === 'changed') onChanged(ev.documents || []);
    };
  } else if ($('[data-autorefresh]')) {
    setTimeout(() => location.reload(), 2500);
  }
})();
