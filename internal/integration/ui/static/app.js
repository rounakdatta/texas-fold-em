// Shared page behaviour (list, editor, add). The review deck has its own
// script; this one stays small and works without it.
(() => {
  'use strict';

  // <time datetime="RFC3339"> — dates the way people say them, in IST
  // (every statement is in IST, and so is the editor's date field).
  const tz = 'Asia/Kolkata';
  const now = new Date();
  const dayKey = d => new Intl.DateTimeFormat('en-CA', { timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit' }).format(d);
  const today = dayKey(now);
  const yesterday = dayKey(new Date(now.getTime() - 864e5));
  const yearOf = d => new Intl.DateTimeFormat('en', { timeZone: tz, year: 'numeric' }).format(d);
  for (const el of document.querySelectorAll('time[datetime]:not([data-ago])')) {
    const d = new Date(el.getAttribute('datetime'));
    if (isNaN(d)) continue;
    const k = dayKey(d);
    let day;
    if (k === today) day = 'Today';
    else if (k === yesterday) day = 'Yesterday';
    else {
      const opts = { timeZone: tz, weekday: 'short', day: 'numeric', month: 'short' };
      if (yearOf(d) !== yearOf(now)) opts.year = 'numeric';
      day = new Intl.DateTimeFormat('en-GB', opts).format(d).replace(',', '');
    }
    const clock = new Intl.DateTimeFormat('en-US', { timeZone: tz, hour: 'numeric', minute: '2-digit' }).format(d).toLowerCase();
    el.textContent = el.hasAttribute('data-day-only') ? day : day + ' · ' + clock;
    el.title = d.toLocaleString('en-IN', { timeZone: tz }) + ' IST';
  }

  // Tag suggestions on the editor: tap to add.
  for (const el of document.querySelectorAll('[data-tag-chip]')) {
    el.addEventListener('click', e => {
      e.preventDefault();
      const input = document.querySelector('input[name="tags"]');
      if (!input) return;
      const t = el.getAttribute('data-tag-chip');
      const parts = input.value.split(',').map(p => p.trim()).filter(Boolean);
      if (!parts.includes(t)) parts.push(t);
      input.value = parts.join(', ');
      input.focus();
    });
  }

  // "Select all" in the list.
  const all = document.getElementById('select-all');
  if (all) {
    const boxes = () => document.querySelectorAll('input[name="fold_uuids"]');
    const sync = () => {
      const n = [...boxes()].filter(b => b.checked).length;
      const bar = document.getElementById('bulk-count');
      if (bar) bar.textContent = n ? n + ' selected' : '';
      const btn = document.getElementById('bulk-go');
      if (btn) btn.disabled = n === 0;
    };
    all.addEventListener('change', () => { for (const b of boxes()) b.checked = all.checked; sync(); });
    for (const b of boxes()) b.addEventListener('change', sync);
    sync();
  }

  // A row of tabs too wide for the screen scrolls sideways: it starts with
  // the current tab in view, and fades at an edge that has more past it.
  for (const seg of document.querySelectorAll('.segmented')) {
    if (seg.scrollWidth <= seg.clientWidth) continue;
    const cur = seg.querySelector('[aria-current="true"]');
    if (cur) {
      const right = cur.offsetLeft + cur.offsetWidth - seg.offsetLeft;
      if (right > seg.clientWidth) seg.scrollLeft = right - seg.clientWidth + 28;
    }
    const fade = () => {
      seg.classList.toggle('fade-l', seg.scrollLeft > 2);
      seg.classList.toggle('fade-r', seg.scrollLeft + seg.clientWidth < seg.scrollWidth - 2);
    };
    seg.addEventListener('scroll', fade, { passive: true });
    fade();
  }

  // A select that filters submits itself.
  for (const s of document.querySelectorAll('select[data-autosubmit]')) s.addEventListener('change', () => s.form.submit());

  // ---- accounts from Firefly -----------------------------------------------------
  // fold keeps a copy of Firefly's accounts, refreshed on every sync, and every
  // picker offers what the copy has: an account made in Firefly a minute ago
  // isn't there yet. Any [data-sync-accounts] button refreshes it in place —
  // no reload, so a half-edited transaction survives — and every
  // [data-sync-status] on the page follows along: syncing, then what arrived.
  // The deck's pickers call foldAccounts.run() too, and listen for
  // "fold:accounts" to redraw their lists.
  const ago = at => {
    const t = new Date(at);
    if (isNaN(t)) return '';
    const s = (Date.now() - t) / 1000; // always rounded down: 59½ min is not "1 hour"
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 7200) return '1 hour ago';
    if (s < 86400) return Math.floor(s / 3600) + ' hours ago';
    const k = dayKey(t);
    if (k === dayKey(new Date())) return 'today';
    if (k === dayKey(new Date(Date.now() - 864e5))) return 'yesterday';
    const opts = { timeZone: tz, weekday: 'short', day: 'numeric', month: 'short' };
    if (yearOf(t) !== yearOf(new Date())) opts.year = 'numeric';
    return 'on ' + new Intl.DateTimeFormat('en-GB', opts).format(t).replace(',', '');
  };
  const emit = (phase, result, error) => document.dispatchEvent(new CustomEvent('fold:accounts', { detail: { phase, result, error } }));
  const accounts = window.foldAccounts = {
    available: document.body.dataset.canSync === '1',
    at: document.body.dataset.syncedAt || '',
    running: null,
    added: new Set(), // names (lower case) that arrived while this page was open
    ago,
    lastLine() { return this.at ? 'Last synced ' + ago(this.at) : 'Not synced from Firefly yet'; },
    isNew(name) { return this.added.has(String(name || '').trim().toLowerCase()); },
    run() {
      if (this.running) return this.running;
      emit('start');
      this.running = fetch('/admin/ui/api/sync-accounts', { method: 'POST', headers: { 'X-Fold-UI': '1', Accept: 'application/json' } })
        .then(async r => {
          const d = await r.json().catch(() => ({}));
          if (!r.ok) throw new Error(d.message || 'Couldn’t reach Firefly — try again in a moment.');
          return d;
        })
        .then(d => {
          if (d.syncedAt) this.at = d.syncedAt;
          for (const a of d.added || []) { this.added.add(a.name.toLowerCase()); this.added.add(a.short.toLowerCase()); }
          emit('done', d);
          return d;
        }, e => {
          if (!(e instanceof Error) || e.name === 'TypeError') e = new Error('Couldn’t reach fold — check the connection and try again.');
          emit('fail', null, e);
          throw e;
        })
        .finally(() => { this.running = null; });
      return this.running;
    },
  };
  // a click, not a form post; without JavaScript the button posts the form
  document.addEventListener('click', e => {
    const b = e.target.closest('[data-sync-accounts]');
    if (!b || !accounts.available) return;
    e.preventDefault();
    accounts.run().catch(() => {});
  });
  document.addEventListener('fold:accounts', e => {
    const { phase, result, error } = e.detail;
    for (const b of document.querySelectorAll('[data-sync-accounts]')) {
      b.disabled = phase === 'start';
      b.classList.toggle('is-busy', phase === 'start');
    }
    for (const el of document.querySelectorAll('[data-sync-status]')) {
      el.textContent = phase === 'start' ? 'Syncing with Firefly…' : phase === 'done' ? result.message : error.message;
      el.classList.toggle('is-err', phase === 'fail');
      el.classList.toggle('is-done', phase === 'done');
    }
    if (phase === 'done') refreshLists();
  });
  // The editor's and the Add form's suggestion lists, from the fresh copy.
  async function refreshLists() {
    const lists = { 'own-options': 'accounts', 'payee-options': 'payees', 'payer-options': 'payers' };
    if (!Object.keys(lists).some(id => document.getElementById(id))) return;
    let o;
    try { o = await (await fetch('/admin/ui/api/options?v=' + encodeURIComponent(accounts.at), { headers: { Accept: 'application/json' } })).json(); } catch (_) { return; }
    for (const [id, key] of Object.entries(lists)) {
      const dl = document.getElementById(id);
      if (!dl || !Array.isArray(o[key])) continue;
      dl.replaceChildren(...o[key].map(x => { const opt = document.createElement('option'); opt.value = typeof x === 'string' ? x : x.name; return opt; }));
    }
  }
  // "Last synced 14 min ago" stays true while the page is open.
  setInterval(() => { for (const t of document.querySelectorAll('time[data-ago]')) t.textContent = ago(t.getAttribute('datetime')); }, 30000);
})();
