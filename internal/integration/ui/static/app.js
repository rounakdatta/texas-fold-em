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
  for (const el of document.querySelectorAll('time[datetime]')) {
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
})();
