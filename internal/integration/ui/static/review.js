// The review deck. One card at a time: swipe right (or →) to send it to
// Firefly, left (or ←) to look at it later. Sending waits out a short undo
// window before the request goes, because Firefly is the ledger of record
// and a mis-swipe must cost nothing. Everything else — titles, categories,
// payees — is edited in place and saved as you go.
(() => {
  'use strict';

  const app = document.getElementById('deck-app');
  if (!app) return;
  const $ = (sel, root = document) => root.querySelector(sel);
  const stackEl = $('#stack');
  const stateEl = $('#deck-state');
  const sheet = $('#sheet');
  const toastsEl = $('#toasts');
  const btnSend = $('#btn-send');
  const btnLater = $('#btn-later');
  const btnMore = $('#btn-more');
  const sendLabel = $('#send-label');
  const hintEl = $('#deck-hint');
  const upnextEl = $('#upnext');
  const scopeBtn = $('#scope-btn');
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const UNDO_MS = 5000;
  let accounts = [];
  try { accounts = JSON.parse(app.dataset.accounts || '[]'); } catch (_) { accounts = []; }

  // Per-viewer conveniences only (the order you like, a hint you've seen).
  // Everything that matters lives on the server.
  const store = {
    get(k, d) { try { const v = localStorage.getItem('fold.' + k); return v === null ? d : JSON.parse(v); } catch (_) { return d; } },
    set(k, v) { try { localStorage.setItem('fold.' + k, JSON.stringify(v)); } catch (_) { /* private mode */ } },
  };

  const params = new URLSearchParams(location.search);
  const state = {
    pile: params.get('pile') === 'later' ? 'later' : 'review',
    account: params.has('account') ? params.get('account') : String(store.get('account', '')),
    order: store.get('order', 'newest') === 'oldest' ? 'oldest' : 'newest',
    cards: [],
    next: '',
    counts: { review: 0, later: 0 },
    loaded: false,
    loading: false,
    error: '',
    pending: [],     // decisions not yet settled with the server (see decide)
    last: null,      // the decision U / ⌘Z takes back
    perAccount: null, // {all, by: {accountId: n}} for this pile, for the account picker
    options: null,   // {categories, payees}, fetched once
    decided: store.get('decided', 0),
  };
  if (!accounts.some(a => String(a.id) === state.account)) state.account = '';

  // ---- api -------------------------------------------------------------------
  async function api(path, body, opts = {}) {
    const init = body === undefined
      ? { headers: { Accept: 'application/json' } }
      : { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Fold-UI': '1' }, body: JSON.stringify(body), keepalive: !!opts.keepalive };
    const r = await fetch('/admin/ui/api/' + path, init);
    let data = {};
    try { data = await r.json(); } catch (_) { /* empty body */ }
    if (!r.ok) {
      const e = new Error(data.message || ('Something went wrong (' + r.status + ')'));
      e.status = r.status; e.data = data;
      throw e;
    }
    return data;
  }

  // ---- tiny DOM builder ----------------------------------------------------------
  function h(tag, attrs, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === false || v === null || v === undefined) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else if (k === 'dataset') Object.assign(el.dataset, v);
      else el.setAttribute(k, v === true ? '' : v);
    }
    for (const kid of kids.flat()) {
      if (kid === null || kid === undefined || kid === false) continue;
      el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
    }
    return el;
  }
  const SVG = {
    card: '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="3" y="5.5" width="18" height="13" rx="2.5" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M3 10h18" stroke="currentColor" stroke-width="1.8"/></svg>',
    bank: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 9.5 12 5l8 4.5M5.5 10v7M9.8 10v7M14.2 10v7M18.5 10v7M4 19h16" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    pen: '<svg class="pen" viewBox="0 0 20 20" aria-hidden="true"><path d="m13.5 3.5 3 3L7 16H4v-3z" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"/></svg>',
    alert: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M10 3.5 18 17H2z" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linejoin="round"/><path d="M10 8.5v3.6M10 14.4v.1" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"/></svg>',
    refund: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M7.5 5 4 8.5 7.5 12M4.5 8.5H12a4 4 0 0 1 0 8h-2" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    swap: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 8.5h13.5M15 5l3.5 3.5L15 12M19 15.5H5.5M9 12l-3.5 3.5L9 19" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    close: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="m5 5 10 10M15 5 5 15" stroke="currentColor" stroke-width="1.9" stroke-linecap="round"/></svg>',
    done: '<svg viewBox="0 0 64 64" aria-hidden="true"><circle cx="32" cy="32" r="27" fill="none" stroke="currentColor" stroke-width="3" opacity=".35"/><path d="m21 33 7.5 7.5L44 25" fill="none" stroke="currentColor" stroke-width="4" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  };
  const icon = name => { const t = document.createElement('template'); t.innerHTML = SVG[name]; return t.content.firstChild; };

  // ---- what the card needs next ----------------------------------------------------
  // The primary button always names the next step: "Send" when the card is
  // ready, otherwise the one thing standing in the way.
  function nextStep(c) {
    const b = c.blockers || [];
    // (stamp: what the card says as it is dragged right)
    if (b.includes('hold')) return { kind: 'skip', label: 'Skip it', stamp: 'Skip' };
    // who before what: the title suggestions are the payee's past titles
    if (b.includes('payee')) return { kind: 'payee', label: 'Who was paid?', stamp: 'Add payee' };
    if (b.includes('source')) return { kind: 'editor', label: 'Pick the account', stamp: 'Pick account' };
    if (b.includes('title-empty')) return { kind: 'title', label: 'Add a title', stamp: 'Add title' };
    if (b.includes('title-blank')) return { kind: 'title', label: 'Fill in the blank', stamp: 'Fill in' };
    return { kind: 'send', label: 'Send', stamp: 'Send' };
  }

  // A blank's hint reads the word before it: "from ___" asks where, "with ___" who.
  function blankHint(before) {
    const w = (before.trim().split(/\s+/).pop() || '').toLowerCase();
    if (w === 'from' || w === 'to' || w === 'at' || w === 'in') return 'where?';
    if (w === 'with' || w === 'for' && /share/i.test(before)) return 'who?';
    return 'what?';
  }

  function titleNodes(title) {
    if (!title.trim()) return [h('span', { text: 'Add a title' })];
    const parts = title.split('___');
    const out = [];
    parts.forEach((p, i) => {
      if (p) out.push(document.createTextNode(p));
      if (i < parts.length - 1) out.push(h('span', { class: 'blank', 'data-hint': blankHint(parts.slice(0, i + 1).join(' ')), 'aria-label': 'blank' }));
    });
    return out;
  }

  function spoken(c) {
    const who = c.direction === 'in' ? 'from ' + (c.from.name || 'someone') : 'to ' + (c.to.name || 'someone');
    return `${c.amount} ${who}, ${c.day} ${c.clock}`;
  }

  // ---- card -----------------------------------------------------------------
  function buildCard(c) {
    const el = h('article', { class: 'card dir-' + c.direction, 'aria-label': spoken(c), dataset: { uuid: c.uuid } });
    // dragged right, a card that can't go yet says what comes next instead
    const step = nextStep(c);
    el.append(h('div', { class: 'stamp stamp-send stamp-' + step.kind, 'aria-hidden': 'true', text: step.stamp }),
              h('div', { class: 'stamp stamp-later', 'aria-hidden': 'true', text: state.pile === 'later' ? 'Not yet' : 'Later' }));
    const scroll = h('div', { class: 'card-scroll' });
    el.append(scroll);

    // Whose account, and when — the small print a person reads last.
    const mine = c.direction === 'in' ? c.to : c.from;
    const acct = c.direction === 'transfer'
      ? h('span', { class: 'acct' }, icon('swap'), h('span', { text: 'Transfer' }))
      : h('span', { class: 'acct', title: mine.full || mine.name || '' },
          icon(/card$/i.test(mine.name || '') ? 'card' : 'bank'), h('span', { text: mine.name || 'Which account?' }));
    scroll.append(h('header', { class: 'card-top' }, acct, h('span', { class: 'when' }, c.day, ' · ', c.clock)));

    // The money and who it went to.
    const hero = h('div', { class: 'hero' });
    hero.append(avatar(c));
    hero.append(h('div', { class: 'amount num' }, c.direction === 'in' ? '+' + c.amount : c.amount));
    const notes = [];
    if (c.foreign) notes.push(c.foreign + ' abroad');
    if (c.foldAmount) notes.push('from the statement · the alert said ' + c.foldAmount);
    if (notes.length) hero.append(h('div', { class: 'amount-note' }, notes.join(' · ')));
    const who = h('div', { class: 'who' });
    if (c.direction === 'transfer') {
      who.append(h('span', { class: 'who-name', text: c.from.name }), h('span', { class: 'transfer-arrow', 'aria-label': 'to', text: '→' }), h('span', { class: 'who-name', text: c.to.name }));
    } else if (c.direction === 'in') {
      who.append(h('span', { class: 'who-pre', text: 'from' }), h('span', { class: 'who-name', text: c.from.name || 'someone' }));
      if (c.from.place) who.append(h('span', { class: 'who-place', text: c.from.place }));
    } else {
      const missing = (c.blockers || []).includes('payee');
      who.append(h('span', { class: 'who-pre', text: 'to' }),
        h('button', { type: 'button', class: 'who-name' + (missing ? ' is-missing' : ''), 'data-act': 'payee', text: missing ? 'Who was paid?' : c.to.name }));
      if (c.to.place && !missing) who.append(h('span', { class: 'who-place', text: c.to.place }));
      // with no payee, the name on the alert is the likely answer: one tap takes it
      if (missing && c.to.name) who.append(h('span', { class: 'who-break', 'aria-hidden': 'true' }), h('button', { type: 'button', class: 'who-guess', 'data-act': 'guess', 'aria-label': 'Paid to ' + c.to.name + '? Use this name' },
        h('span', { text: c.to.name + '?' })));
    }
    hero.append(who);
    scroll.append(hero);

    // What it was: the title and category push will send.
    const what = h('div', { class: 'what' });
    what.append(h('button', { type: 'button', class: 'title-btn' + (c.title.trim() ? '' : ' is-empty'), 'data-act': 'title', 'aria-label': 'Title: ' + (c.title || 'none') + '. Edit' },
      h('span', { class: 't' }, ...titleNodes(c.title)), icon('pen')));
    const meta = h('div', { class: 'meta-row' });
    meta.append(c.category
      ? h('button', { type: 'button', class: 'chip', 'data-act': 'category', 'aria-label': 'Category: ' + c.category + '. Change', text: c.category })
      : h('button', { type: 'button', class: 'chip chip-quiet', 'data-act': 'category', text: '+ Category' }));
    for (const t of c.tags || []) meta.append(h('span', { class: 'tag', text: '#' + t }));
    what.append(meta);
    scroll.append(what);

    // Only what is exceptional.
    const attn = h('div', { class: 'attn' });
    if (c.error) attn.append(banner('alert', h('span', {}, h('b', { text: 'Not sent. ' }), c.error),
      h('button', { type: 'button', class: 'btn btn-sm btn-quiet', 'data-act': 'editor', text: 'Open the editor' })));
    if (c.hold) attn.append(banner('alert', h('span', {}, h('b', { text: 'On hold: ' }), sentence(c.hold), ' It won’t be sent.')));
    if (c.duplicate) attn.append(banner('alert', 'fold.money thinks this may be a duplicate alert.',
      h('button', { type: 'button', class: 'btn btn-sm btn-quiet', 'data-act': 'skip', text: 'Skip it' })));
    if (c.refund && c.refund.needsPick && refundChoices(c).length) attn.append(banner('refund', 'A refund. Which purchase did it come back for?',
      h('button', { type: 'button', class: 'btn btn-sm btn-quiet', 'data-act': 'refund', text: 'Pick the purchase' })));
    if (attn.childNodes.length) scroll.append(attn);

    // The context in the user's own words, then what the bank said.
    const foot = h('footer', { class: 'foot' });
    if (c.note) foot.append(h('p', { class: 'note-quote', text: c.note }));
    if (c.refund && !c.refund.needsPick && c.refund.label) foot.append(h('p', { class: 'bank' }, h('b', { text: 'Refund of' }), h('span', { text: c.refund.label })));
    if (c.manual) foot.append(h('p', { class: 'bank', title: c.narration }, h('b', { text: 'From the statement' }), h('span', { text: c.bankSaid.replace(/^Statement: /, '') })));
    else if (c.bankSaid && addsInfo(c.bankSaid, c)) foot.append(h('p', { class: 'bank', title: c.narration }, h('b', { text: /^CARD\//.test(c.narration) ? 'On the alert' : 'On the bank line' }), h('span', { text: c.bankSaid })));
    if (foot.childNodes.length) scroll.append(foot);

    attachDrag(el);
    return el;
  }

  // Someone's note as a sentence: it ends with a stop, whatever they typed.
  function sentence(t) { t = (t || '').trim(); return /[.!?…]$/.test(t) ? t : t + '.'; }

  // The bank's name for the payee is worth a line only when it says
  // something the payee's name doesn't ("Ramesh S" behind "Chai Corner").
  function addsInfo(said, c) {
    const norm = x => (x || '').toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim();
    const s = norm(said), who = norm([c.to.name, c.to.place, c.from.name].join(' '));
    if (!s) return false;
    return !s.split(' ').every(w => w.length < 3 || who.includes(w));
  }

  // A payee's initials on a colour of its own, the same colour every time:
  // the deck reads faster when Zomato always looks like Zomato.
  function avatar(c) {
    const p = c.direction === 'in' ? c.from : c.to;
    if (c.direction === 'transfer') { const a = h('div', { class: 'avatar is-mine', 'aria-hidden': 'true' }); a.append(icon('bank')); return a; }
    if ((c.blockers || []).includes('payee') || !p.name) return h('div', { class: 'avatar is-missing', 'aria-hidden': 'true', text: '?' });
    const words = p.name.replace(/[^\p{L}\p{N} ]+/gu, ' ').split(/\s+/).filter(Boolean);
    const letters = (words.length > 1 ? words[0][0] + words[1][0] : (words[0] || '?')[0]).toUpperCase();
    let hue = 0;
    for (const ch of p.name.toLowerCase()) hue = (hue * 31 + ch.codePointAt(0)) % 360;
    return h('div', { class: 'avatar', 'aria-hidden': 'true', style: '--h:' + hue, text: letters });
  }

  function banner(iconName, body, action) {
    // amber for what could go wrong (a hold, a duplicate, a failed send);
    // a plain question is just a question
    return h('div', { class: 'banner ' + (iconName === 'refund' ? 'banner-info' : 'banner-attn') }, icon(iconName),
      h('div', { class: 'banner-body' }, body, action ? h('div', { class: 'banner-actions' }, action) : null));
  }

  // ---- rendering ------------------------------------------------------------
  const nodes = new Map(); // uuid -> element, for the cards on screen
  let flying = new Set();  // elements animating out; left alone by render

  function fmtCount(n) { return n > 0 ? String(n) : ''; }

  function render() {
    for (const el of document.querySelectorAll('[data-count]')) {
      const n = state.counts[el.dataset.count] || 0;
      el.textContent = fmtCount(n);
    }
    const navCount = document.querySelector('.nav a[aria-current="page"] .count');
    if (navCount && state.loaded && state.pile === 'review' && !state.account) {
      navCount.textContent = String(state.counts.review || 0);
      navCount.hidden = !state.counts.review;
    }
    for (const b of document.querySelectorAll('.pile')) b.setAttribute('aria-selected', String(b.dataset.pile === state.pile));
    const acct = accounts.find(a => String(a.id) === state.account);
    $('#scope-label').textContent = (acct ? acct.short : 'All accounts') + (state.order === 'oldest' ? ' · oldest first' : '');

    const shown = state.cards.slice(0, 3);
    const keep = new Set(shown.map(c => c.uuid));
    for (const [id, el] of nodes) {
      if (!keep.has(id) && !flying.has(el)) { el.remove(); nodes.delete(id); }
    }
    shown.forEach((c, i) => {
      let el = nodes.get(c.uuid);
      if (!el || el.dataset.v !== c._v) {
        const fresh = buildCard(c);
        fresh.dataset.v = c._v || '';
        if (el) { fresh.className = el.className; el.replaceWith(fresh); } else stackEl.append(fresh);
        el = fresh;
        nodes.set(c.uuid, el);
      }
      el.classList.remove('is-top', 'is-next', 'is-after', 'is-hidden');
      el.classList.add(['is-top', 'is-next', 'is-after'][i]);
      el.style.transform = '';
      const inner = el.querySelector('.card-scroll');
      if (inner) inner.style.opacity = '';
      el.setAttribute('aria-hidden', i === 0 ? 'false' : 'true');
      el.inert = i !== 0;
    });

    const top = state.cards[0];
    const hasTop = !!top;
    for (const b of [btnSend, btnLater, btnMore]) b.disabled = !hasTop;
    // an empty (or failed) deck has nothing to decide: the bar steps aside
    app.classList.toggle('is-empty', !hasTop && (state.loaded || !!state.error));
    if (top) {
      const step = nextStep(top);
      sendLabel.textContent = state.pile === 'later' && step.kind === 'send' ? 'Send' : step.label;
      btnSend.classList.toggle('is-step', step.kind !== 'send' && step.kind !== 'skip');
      btnSend.classList.toggle('is-skip', step.kind === 'skip');
      btnSend.setAttribute('aria-label', step.kind === 'send' ? 'Send to Firefly (right arrow)' : step.label);
      setSendIcon(step.kind);
    } else {
      sendLabel.textContent = 'Send';
      btnSend.classList.remove('is-step', 'is-skip');
      setSendIcon('send');
    }
    btnLater.querySelector('span').textContent = state.pile === 'later' ? 'Not yet' : 'Later';
    fitSend();
    hintEl.hidden = !(hasTop && state.decided < 3);
    renderState();
    renderUpNext();
    report();
  }

  // The deck reports what it holds, for tests and anyone curious: cards
  // loaded, and decisions not yet settled with the server (waiting out the
  // undo window, or in flight).
  function report() {
    app.dataset.cards = String(state.cards.length);
    app.dataset.pending = String(state.pending.filter(unsettled).length);
    app.dataset.loaded = String(state.loaded);
  }

  // The primary button's label is the one text in the bar that must never be
  // cut: when it won't fit, the icon goes first, then a little size.
  function fitSend() {
    btnSend.classList.remove('is-tight', 'is-tighter');
    const over = () => sendLabel.scrollWidth > sendLabel.clientWidth + 0.5;
    if (!over()) return;
    btnSend.classList.add('is-tight');
    if (over()) btnSend.classList.add('is-tighter');
  }
  if (window.ResizeObserver) new ResizeObserver(() => fitSend()).observe(btnSend);

  const STEP_ICON = {
    send: '<path d="m5 12.5 4.2 4.2L19 7" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/>',
    step: '<path d="m15.5 4.5 4 4L9 19H5v-4z" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linejoin="round"/>',
    skip: '<path d="M6 6l12 12M18 6 6 18" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round"/>',
  };
  function setSendIcon(kind) {
    const k = kind === 'send' ? 'send' : kind === 'skip' ? 'skip' : 'step';
    const svg = btnSend.querySelector('svg');
    if (svg.dataset.k !== k) { svg.innerHTML = STEP_ICON[k]; svg.dataset.k = k; }
  }

  function renderState() {
    stateEl.replaceChildren();
    let content = null;
    if (!state.loaded && !state.error) {
      content = skeleton();
    } else if (state.error && !state.cards.length) {
      content = h('div', { class: 'empty' },
        h('h2', { text: 'Couldn’t load your transactions' }), h('p', { text: state.error }),
        h('div', { class: 'actions' }, h('button', { type: 'button', class: 'btn', onclick: () => load(true), text: 'Try again' })));
    } else if (state.loaded && !state.cards.length && !state.loading) {
      content = emptyState();
    }
    stateEl.hidden = !content;
    if (content) stateEl.append(content);
  }

  function skeleton() {
    const s = h('div', { class: 'card skeleton', 'aria-hidden': 'true' });
    const inner = h('div', { class: 'card-scroll' },
      h('div', { class: 'sk', style: 'height:14px;width:55%' }),
      h('div', { class: 'sk', style: 'height:44px;width:40%;margin-top:18px' }),
      h('div', { class: 'sk', style: 'height:20px;width:70%' }),
      h('div', { class: 'sk', style: 'height:18px;width:85%;margin-top:22px' }),
      h('div', { class: 'sk', style: 'height:28px;width:28%' }));
    s.append(inner);
    const wrap = h('div', { style: 'position:absolute;inset:0' }, s);
    return wrap;
  }

  function emptyState() {
    const acct = accounts.find(a => String(a.id) === state.account);
    const actions = h('div', { class: 'actions' });
    let title, line;
    if (state.pile === 'later') {
      title = 'Nothing saved for later';
      line = 'Cards you swipe left wait here.';
      actions.append(h('button', { type: 'button', class: 'btn', onclick: () => switchPile('review'), text: 'Back to review' }));
    } else if (acct) {
      title = 'Nothing to review on ' + acct.short;
      line = 'Every transaction on it is decided.';
      actions.append(h('button', { type: 'button', class: 'btn', onclick: () => setScope('', state.order), text: 'Show all accounts' }));
    } else {
      title = 'All caught up';
      line = 'Nothing is waiting to go to Firefly.';
      if (state.counts.later) actions.append(h('button', { type: 'button', class: 'btn', onclick: () => switchPile('later'),
        text: state.counts.later === 1 ? '1 card saved for later' : state.counts.later + ' cards saved for later' }));
    }
    actions.append(h('a', { href: '/admin/ui/?status=all', text: 'See every transaction' }));
    const art = h('div', { class: 'empty-art' }); art.append(icon('done'));
    return h('div', { class: 'empty' }, art, h('h2', { text: title }), h('p', { text: line }), actions);
  }

  // Two lines per card in "up next": who and how much, then what it needs
  // (or, when it needs nothing, what it was) — never the same words twice.
  function upNextLines(c) {
    const norm = x => (x || '').toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim();
    const plain = t => t.replace(/_{3}/g, '…').trim();
    const payee = c.direction === 'in' ? c.from.name : c.direction === 'transfer' ? [c.from.name, c.to.name].filter(Boolean).join(' → ') : c.to.name;
    const step = nextStep(c);
    const blocked = step.kind !== 'send';
    const head = payee || (c.title.trim() ? plain(c.title) : '') || c.bankSaid || 'Someone';
    let sub;
    if (blocked) sub = step.label;
    else if (c.title.trim() && norm(plain(c.title)) !== norm(head)) sub = plain(c.title);
    else sub = c.category || 'Ready to send';
    return { head, sub, blocked };
  }

  function renderUpNext() {
    if (!upnextEl || getComputedStyle(upnextEl.parentElement).display === 'none') return;
    upnextEl.replaceChildren();
    for (const c of state.cards.slice(1, 8)) {
      const l = upNextLines(c);
      upnextEl.append(h('li', {}, h('button', { type: 'button', onclick: () => bringToTop(c.uuid), 'aria-label': 'Review next: ' + spoken(c) },
        h('span', { class: 'u-who', text: l.head }), h('span', { class: 'u-amt num', text: (c.direction === 'in' ? '+' : '') + c.amount }),
        h('span', { class: 'u-title' + (l.blocked ? ' u-attn' : ''), text: l.sub }), h('span', { class: 'u-day', text: c.day }))));
    }
    if (!upnextEl.childNodes.length) upnextEl.append(h('li', { class: 'muted', text: 'Nothing else in this pile.' }));
  }

  function bringToTop(uuid) {
    const i = state.cards.findIndex(c => c.uuid === uuid);
    if (i > 0) { const [c] = state.cards.splice(i, 1); state.cards.unshift(c); render(); }
  }

  // ---- loading ----------------------------------------------------------------
  async function load(reset) {
    if (state.loading) return;
    state.loading = true;
    if (reset) {
      state.cards = []; state.next = ''; state.loaded = false; state.error = '';
      for (const el of nodes.values()) el.remove();
      nodes.clear();
      render();
    }
    const q = new URLSearchParams({ pile: state.pile, order: state.order });
    if (state.account) q.set('account', state.account);
    if (!reset && state.next) q.set('after', state.next);
    try {
      const d = await api('deck?' + q.toString());
      // a decision not yet settled with the server may still be listed by it
      const skip = new Set(state.pending.filter(unsettled).map(p => p.card.uuid));
      const have = new Set(state.cards.map(c => c.uuid));
      for (const c of d.cards) if (!skip.has(c.uuid) && !have.has(c.uuid)) state.cards.push(c);
      state.next = d.next || '';
      if (d.counts.byAccount) state.perAccount = { all: d.counts.all || 0, by: d.counts.byAccount };
      state.counts = d.counts;
      // …and isn't in its counts yet
      for (const p of state.pending) if (unsettled(p) && (p.kind === 'send' || p.kind === 'skip')) state.counts[p.pile] = Math.max(0, (state.counts[p.pile] || 0) - 1);
      state.loaded = true;
      state.error = '';
    } catch (e) {
      state.error = e.message;
    }
    state.loading = false;
    render();
  }

  function maybePrefetch() {
    if (state.next && state.cards.length < 10 && !state.loading) load(false);
  }

  function switchPile(pile) {
    if (pile === state.pile) return;
    state.pile = pile;
    const u = new URL(location.href);
    if (pile === 'later') u.searchParams.set('pile', 'later'); else u.searchParams.delete('pile');
    history.replaceState(null, '', u);
    load(true);
  }

  function setScope(account, order) {
    state.account = account; state.order = order;
    store.set('account', account); store.set('order', order);
    load(true);
  }

  // ---- decisions --------------------------------------------------------------
  // A decision is {card, kind, pile, done, inflight, timer}. A send or skip
  // waits out UNDO_MS before its request goes (done = false until then); a
  // Later goes at once. `state.last` is the one decision U / ⌘Z takes back.
  function top() { return state.cards[0]; }
  const unsettled = p => !p.done || p.inflight;

  function decide(kind) {
    const c = top();
    if (!c) return;
    if (kind === 'send' || kind === 'right') {
      const step = nextStep(c);
      if (step.kind !== 'send') {
        if (step.kind === 'skip') return decide('skip');
        return blocked(c, step);
      }
      kind = 'send';
    }
    if (kind === 'later' && state.pile === 'later') {
      // "Not yet": back of the Later pile, nothing to save.
      flyOut(c, 'left', () => { state.cards.push(state.cards.shift()); render(); });
      return;
    }
    flyOut(c, kind === 'later' ? 'left' : kind === 'skip' ? 'down' : 'right', () => {
      state.cards.shift();
      const p = { card: c, kind, pile: state.pile, done: false, inflight: false, timer: null };
      state.pending.push(p);
      state.last = p;
      state.counts[state.pile] = Math.max(0, (state.counts[state.pile] || 0) - 1);
      if (kind === 'later') {
        state.counts.later = (state.counts.later || 0) + 1;
        p.done = true; p.inflight = true;
        p.request = api('rows/' + c.uuid + '/later', { later: true })
          .then(() => { p.inflight = false; state.pending = state.pending.filter(x => x !== p); report(); },
                e => failed(p, e));
        toast('Saved for later · ' + short(c), { undo: () => undo(p) });
      } else {
        p.timer = setTimeout(() => run(p), UNDO_MS);
        toast((kind === 'send' ? 'Sent · ' : 'Skipped · ') + short(c), { undo: () => undo(p), ms: UNDO_MS });
      }
      state.decided += 1; store.set('decided', state.decided);
      if (navigator.vibrate) navigator.vibrate(8);
      render();
      maybePrefetch();
    });
  }

  function short(c) {
    const who = c.direction === 'in' ? c.from.name : c.to.name;
    return c.amount + (who ? ' ' + who : '');
  }

  async function run(p, keepalive) {
    if (p.done) return;
    p.done = true; p.inflight = true;
    clearTimeout(p.timer);
    report();
    try {
      const d = await api('rows/' + p.card.uuid + '/' + p.kind, {}, { keepalive });
      if (d.fireflyUrl) p.card.fireflyUrl = d.fireflyUrl;
      p.inflight = false;
      state.pending = state.pending.filter(x => x !== p);
      report();
    } catch (e) {
      p.inflight = false;
      failed(p, e);
    }
  }

  // A decision the server refused comes back to the top of its pile, saying why.
  function failed(p, e) {
    state.pending = state.pending.filter(x => x !== p);
    if (state.last === p) state.last = null;
    const c = (e.data && e.data.card) || p.card;
    c.error = e.message;
    c._v = String(Date.now());
    if (p.kind === 'later') state.counts.later = Math.max(0, state.counts.later - 1);
    state.counts[p.pile] = (state.counts[p.pile] || 0) + 1;
    if (p.pile === state.pile) {
      state.cards = state.cards.filter(x => x.uuid !== c.uuid);
      state.cards.unshift(c);
    }
    toast(e.message, { error: true });
    render();
  }

  function undo(p) {
    p = p || state.last;
    if (!p) return;
    if (state.last === p) state.last = null;
    if (!p.done) {
      clearTimeout(p.timer);
      p.done = true;
    } else if (p.kind === 'later') {
      // after the Later request, never racing it
      Promise.resolve(p.request).catch(() => {}).then(() => api('rows/' + p.card.uuid + '/later', { later: false }))
        .catch(e => toast(e.message, { error: true }));
      state.counts.later = Math.max(0, state.counts.later - 1);
    } else {
      toast(p.kind === 'send' ? 'That one’s already in Firefly — change it from Transactions.' : 'That one’s already skipped — Transactions › Skipped can bring it back.');
      return;
    }
    state.pending = state.pending.filter(x => x !== p);
    state.counts[p.pile] = (state.counts[p.pile] || 0) + 1;
    delete p.card.error;
    clearToasts();
    if (p.pile !== state.pile) { render(); toast('Undone — it’s back in ' + (p.pile === 'later' ? 'Later' : 'To review')); return; }
    state.cards = state.cards.filter(x => x.uuid !== p.card.uuid);
    state.cards.unshift(p.card);
    render();
    const el = nodes.get(p.card.uuid);
    if (el && !reduceMotion) {
      el.animate([{ transform: 'translateY(-18px) scale(1.02)', opacity: 0 }, { transform: 'none', opacity: 1 }], { duration: 220, easing: 'cubic-bezier(.2,.8,.2,1)' });
    }
  }

  // Before the page goes away, anything still waiting out its undo window is
  // sent now: the swipe was the decision.
  function flushPending() {
    for (const p of state.pending) if (!p.done) run(p, true);
  }
  window.addEventListener('pagehide', flushPending);
  document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'hidden') flushPending(); });

  function blocked(c, step) {
    const el = nodes.get(c.uuid);
    if (el) {
      el.style.transform = '';
      el.classList.remove('shake');
      void el.offsetWidth;
      el.classList.add('shake');
      setTimeout(() => el.classList.remove('shake'), 420);
    }
    doStep(c, step);
  }

  function doStep(c, step) {
    if (step.kind === 'title') openTitle(c);
    else if (step.kind === 'payee') openPayee(c);
    else if (step.kind === 'editor') location.href = c.editUrl;
    else if (step.kind === 'skip') decide('skip');
  }

  // ---- gestures ---------------------------------------------------------------
  function flyOut(c, dir, then) {
    const el = nodes.get(c.uuid);
    if (!el || reduceMotion) { if (el) { el.remove(); nodes.delete(c.uuid); } then(); return; }
    flying.add(el);
    nodes.delete(c.uuid);
    const w = window.innerWidth;
    const x = dir === 'right' ? w * 1.1 : dir === 'left' ? -w * 1.1 : 0;
    const y = dir === 'down' ? window.innerHeight : 40;
    const rot = dir === 'right' ? 18 : dir === 'left' ? -18 : 0;
    el.classList.remove('is-dragging');
    el.style.transition = 'transform .32s cubic-bezier(.3,.7,.3,1), opacity .32s';
    el.style.transform = `translate(${x}px, ${y}px) rotate(${rot}deg)`;
    el.style.opacity = '0';
    setTimeout(() => { el.remove(); flying.delete(el); }, 340);
    then();
  }

  function paint(el, dx) {
    el.style.transform = `translateX(${dx}px) rotate(${Math.max(-14, Math.min(14, dx * 0.045))}deg)`;
    const t = Math.min(1, Math.abs(dx) / 110);
    el.querySelector('.stamp-send').style.opacity = dx > 0 ? String(t) : '0';
    el.querySelector('.stamp-later').style.opacity = dx < 0 ? String(t) : '0';
    const next = stackEl.querySelector('.card.is-next');
    if (next) {
      next.style.transform = `translateY(${11 - 11 * t}px) scale(${0.955 + 0.045 * t})`;
      const inner = next.querySelector('.card-scroll');
      if (inner) inner.style.opacity = String(t);
    }
  }

  function settle(el) {
    el.style.transform = '';
    el.querySelector('.stamp-send').style.opacity = '0';
    el.querySelector('.stamp-later').style.opacity = '0';
    const next = stackEl.querySelector('.card.is-next');
    if (next) {
      next.style.transform = '';
      const inner = next.querySelector('.card-scroll');
      if (inner) inner.style.opacity = '';
    }
  }

  function attachDrag(el) {
    let id = null, sx = 0, sy = 0, dx = 0, dragging = false, lastX = 0, lastT = 0, vx = 0, swallowClick = false;
    el.addEventListener('pointerdown', e => {
      swallowClick = false;
      if (e.button !== 0 || sheet.open || !el.classList.contains('is-top')) return;
      if (e.target.closest('input, textarea, select, a')) return;
      id = e.pointerId; sx = lastX = e.clientX; sy = e.clientY; dx = 0; vx = 0; lastT = performance.now(); dragging = false;
    });
    el.addEventListener('pointermove', e => {
      if (e.pointerId !== id) return;
      dx = e.clientX - sx;
      const dy = e.clientY - sy;
      if (!dragging) {
        if (Math.abs(dx) > 10 && Math.abs(dx) > Math.abs(dy) * 1.2) {
          dragging = true;
          try { el.setPointerCapture(id); } catch (_) { /* pointer already gone */ }
          el.classList.add('is-dragging');
        } else if (Math.abs(dy) > 12) { id = null; return; } // a scroll, not a swipe
        else return;
      }
      const now = performance.now();
      vx = (e.clientX - lastX) / Math.max(1, now - lastT);
      lastX = e.clientX; lastT = now;
      paint(el, dx);
    });
    const finish = e => {
      if (e.pointerId !== id) return;
      id = null;
      if (!dragging) return;
      dragging = false;
      swallowClick = true;
      el.classList.remove('is-dragging');
      const th = Math.min(130, el.offsetWidth * 0.3);
      const c = top();
      if (dx > th || (vx > 0.55 && dx > 40)) { settle(el); decide('right'); }
      else if (dx < -th || (vx < -0.55 && dx < -40)) { settle(el); decide('later'); }
      else settle(el);
      if (c && nodes.get(c.uuid) === el && !flying.has(el)) settle(el);
    };
    el.addEventListener('pointerup', finish);
    el.addEventListener('pointercancel', e => {
      if (e.pointerId !== id) return;
      id = null;
      if (dragging) { dragging = false; el.classList.remove('is-dragging'); settle(el); }
    });
    // A drag ends in a click on whatever was under the finger; that click
    // was part of the swipe, not a tap on the title.
    el.addEventListener('click', e => {
      if (swallowClick) { e.preventDefault(); e.stopPropagation(); swallowClick = false; return; }
      const act = e.target.closest('[data-act]');
      if (!act) return;
      const c = state.cards.find(x => x.uuid === el.dataset.uuid);
      if (!c) return;
      runAct(act.dataset.act, c);
    }, true);
  }

  function runAct(act, c) {
    if (act === 'title') openTitle(c);
    else if (act === 'category') openCategory(c);
    else if (act === 'payee') openPayee(c);
    else if (act === 'refund') openRefund(c);
    else if (act === 'skip') { bringToTop(c.uuid); decide('skip'); }
    else if (act === 'editor') location.href = c.editUrl;
    else if (act === 'guess') edit(c, { payee: c.to.name }).catch(() => {});
  }

  // ---- saving an edit -------------------------------------------------------------
  async function edit(c, fields) {
    try {
      const d = await api('rows/' + c.uuid + '/edit', fields);
      if (d.card) replaceCard(d.card);
      if (!d.ok && d.message) toast(d.message, { error: true });
      return d;
    } catch (e) {
      toast(e.message, { error: true });
      throw e;
    }
  }

  function replaceCard(fresh) {
    fresh._v = String(Date.now());
    const i = state.cards.findIndex(x => x.uuid === fresh.uuid);
    if (i >= 0) state.cards[i] = fresh;
    render();
  }

  // ---- sheets -------------------------------------------------------------------
  let sheetDone = null;
  function openSheet(title, body, foot) {
    // (replaceChildren would print a null as the text "null")
    sheet.replaceChildren(...[
      h('div', { class: 'sheet-grab', 'aria-hidden': 'true' }),
      h('div', { class: 'sheet-head' }, h('h2', { id: 'sheet-title', text: title }),
        h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Close', onclick: closeSheet }, icon('close'))),
      h('div', { class: 'sheet-body' }, body),
      foot ? h('div', { class: 'sheet-foot' }, foot) : null].filter(Boolean));
    if (!sheet.open) sheet.showModal();
    const first = sheet.querySelector('textarea, input');
    if (first) setTimeout(() => first.focus(), 30);
  }
  function closeSheet() { if (sheet.open) sheet.close(); }
  sheet.addEventListener('click', e => { if (e.target === sheet) closeSheet(); });
  sheet.addEventListener('close', () => { if (sheetDone) { const f = sheetDone; sheetDone = null; f(); } });

  function pickButton(label, hint, onpick, selected, stacked) {
    return h('button', { type: 'button', class: 'pick' + (stacked ? ' pick-stacked' : ''), 'aria-selected': selected ? 'true' : false, onclick: onpick },
      h('span', { class: 'pick-main', text: label }), hint ? h('span', { class: 'pick-hint', text: hint }) : null);
  }

  async function suggestions(c) {
    try { return await api('rows/' + c.uuid + '/suggest'); } catch (_) { return { titles: [], categories: [] }; }
  }
  async function options() {
    if (!state.options) {
      try { state.options = await api('options'); } catch (_) { state.options = { categories: [], payees: [] }; }
    }
    return state.options;
  }

  function openTitle(c) {
    const ta = h('textarea', { rows: '2', 'aria-label': 'Title', placeholder: 'What was it? e.g. Masala dosa for breakfast' });
    ta.value = c.title;
    const past = h('div', { class: 'pick-list' });
    const save = async value => {
      value = value.trim();
      if (!value) { ta.focus(); return; }
      closeSheet();
      if (value !== c.title) await edit(c, { title: value }).catch(() => {});
    };
    ta.addEventListener('keydown', e => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); save(ta.value); } });
    openSheet(c.title.includes('___') ? 'Fill in the blank' : 'Title',
      h('div', { class: 'fields' }, ta, past),
      [h('button', { type: 'button', class: 'btn btn-ghost', onclick: closeSheet, text: 'Cancel' }),
       h('button', { type: 'button', class: 'btn', onclick: () => save(ta.value), text: 'Save' })]);
    // put the caret on the first blank, selected, so typing replaces it
    setTimeout(() => {
      const i = ta.value.indexOf('___');
      if (i >= 0) ta.setSelectionRange(i, i + 3); else ta.setSelectionRange(ta.value.length, ta.value.length);
    }, 40);
    suggestions(c).then(s => {
      if (!s.titles.length) return;
      past.append(h('div', { class: 'pick-group', text: 'Before at ' + (c.to.name || 'this payee') }));
      for (const t of s.titles.slice(0, 6)) past.append(pickButton(t.value, t.hint, () => save(t.value), false, true));
    });
  }

  async function openCategory(c) {
    const input = h('input', { type: 'search', placeholder: 'Search categories', 'aria-label': 'Search categories', autocomplete: 'off' });
    const list = h('div', { class: 'pick-list', role: 'listbox' });
    const choose = async v => { closeSheet(); if (v !== c.category) await edit(c, { category: v }).catch(() => {}); };
    openSheet('Category', h('div', { class: 'fields' }, input, list));
    const [s, o] = await Promise.all([suggestions(c), options()]);
    const paint = () => {
      const q = input.value.trim().toLowerCase();
      list.replaceChildren();
      if (!q && s.categories.length) {
        list.append(h('div', { class: 'pick-group', text: 'Used before here' }));
        for (const x of s.categories) list.append(pickButton(x.value, x.hint, () => choose(x.value), x.value === c.category));
        list.append(h('div', { class: 'pick-group', text: 'All categories' }));
      }
      const suggested = new Set(q ? [] : s.categories.map(x => x.value));
      const all = o.categories.filter(x => (!q || x.toLowerCase().includes(q)) && !suggested.has(x)).slice(0, q ? 60 : 200);
      for (const x of all) list.append(pickButton(x, '', () => choose(x), x === c.category));
      if (q && !all.length) list.append(h('p', { class: 'hint', text: 'No category called that. Categories come from Firefly.' }));
      if (c.category && !q) list.append(pickButton('No category', '', () => choose('')));
    };
    input.addEventListener('input', paint);
    input.addEventListener('keydown', e => { if (e.key === 'Enter') { const b = list.querySelector('.pick'); if (b) { e.preventDefault(); b.click(); } } });
    paint();
  }

  async function openPayee(c) {
    const input = h('input', { type: 'search', placeholder: 'Who was paid?', 'aria-label': 'Payee', autocomplete: 'off' });
    const missing = (c.blockers || []).includes('payee');
    const current = missing ? '' : [c.to.name, c.to.place].filter(Boolean).join(', ');
    input.value = current;
    const list = h('div', { class: 'pick-list', role: 'listbox' });
    const choose = async v => { closeSheet(); if (v !== current) await edit(c, { payee: v }).catch(() => {}); };
    openSheet('Paid to', h('div', { class: 'fields' }, input, h('p', { class: 'hint', text: 'Pick one of your payees, or type a new name. Firefly creates it when this is sent.' }), list));
    setTimeout(() => input.select(), 40); // typing replaces the current name
    const [o, s] = await Promise.all([options(), suggestions(c)]);
    const paint = () => {
      const q = input.value.trim();
      const ql = q.toLowerCase();
      list.replaceChildren();
      if (!ql || q === current) {
        const past = (s.payees || []).filter(x => x.value !== c.to.name);
        if (missing && c.to.name) {
          list.append(h('div', { class: 'pick-group', text: 'On the alert' }));
          const known = o.payees.find(p => p.toLowerCase() === c.to.name.toLowerCase());
          list.append(pickButton(known || c.to.name, known ? '' : 'new payee', () => choose(known || c.to.name)));
        }
        if (past.length) {
          list.append(h('div', { class: 'pick-group', text: 'Before, for ' + (c.bankSaid || 'this bank name') }));
          for (const x of past) list.append(pickButton(x.value, x.hint, () => choose(x.value), x.value === current));
        }
        return;
      }
      const starts = [], has = [];
      for (const p of o.payees) {
        const pl = p.toLowerCase();
        if (pl.startsWith(ql)) starts.push(p); else if (pl.includes(ql)) has.push(p);
        if (starts.length > 30) break;
      }
      const hits = starts.concat(has).slice(0, 30);
      const exact = hits.some(p => p.toLowerCase() === ql);
      if (!exact) list.append(pickButton('Use “' + q + '”', 'new payee', () => choose(q)));
      for (const p of hits) list.append(pickButton(p, '', () => choose(p)));
    };
    input.addEventListener('input', paint);
    input.addEventListener('keydown', e => { if (e.key === 'Enter') { const b = list.querySelector('.pick'); if (b) { e.preventDefault(); b.click(); } } });
    paint();
  }

  // The purchases a refund could be for (not the "none of these" choice).
  function refundChoices(c) { return ((c.refund && c.refund.options) || []).filter(o => o.value !== 'none'); }

  function openRefund(c) {
    const list = h('div', { class: 'pick-list' });
    const choose = async v => { closeSheet(); await edit(c, { refundOf: v }).catch(() => {}); };
    for (const o of refundChoices(c)) list.append(pickButton(o.label, '', () => choose(o.value), o.value === c.refund.current));
    if (!list.childNodes.length) list.append(h('p', { class: 'hint', text: 'No purchase on this card matches. You can still send it — it just won’t be linked.' }));
    list.append(pickButton('It isn’t a refund of anything here', '', () => choose('none')));
    openSheet('Refund of', h('div', { class: 'fields' }, h('p', { class: 'hint', text: 'Firefly will link the two, so the purchase shows it was refunded.' }), list));
  }

  function openMore(c) {
    const list = h('div', { class: 'pick-list' });
    const add = (label, hint, fn) => list.append(pickButton(label, hint, () => { closeSheet(); fn(); }));
    add('Edit the title', 'T', () => openTitle(c));
    add('Change the category', 'C', () => openCategory(c));
    if (c.direction === 'out') add('Change who was paid', 'P', () => openPayee(c));
    if (c.refund) add('Which purchase it refunds', '', () => openRefund(c));
    add('Open the full editor', 'O', () => { location.href = c.editUrl; });
    if (state.pile === 'later') add('Move back to review', '', () => moveBack(c));
    if (c.hold) add('Release the hold', '', () => setHold(c, ''));
    else add('Put on hold…', '', () => openHold(c));
    list.append(h('div', { class: 'pick-group', text: '' }));
    list.append(h('button', { type: 'button', class: 'pick', onclick: () => { closeSheet(); decide('skip'); } },
      h('span', { class: 'pick-main', style: 'color:var(--danger)', text: 'Skip — never send this one' }), h('span', { class: 'pick-hint', text: 'you can undo' })));
    openSheet(short(c), list);
  }

  function openHold(c) {
    const input = h('input', { type: 'text', placeholder: 'e.g. never billed by the bank', 'aria-label': 'Why is it on hold?' });
    const save = () => { const v = input.value.trim(); if (!v) { input.focus(); return; } closeSheet(); setHold(c, v); };
    input.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); save(); } });
    openSheet('Put on hold', h('div', { class: 'fields' }, h('p', { class: 'hint', text: 'A held transaction can’t be sent until the hold is released. Say why, for whoever looks next.' }), input),
      [h('button', { type: 'button', class: 'btn btn-ghost', onclick: closeSheet, text: 'Cancel' }), h('button', { type: 'button', class: 'btn', onclick: save, text: 'Hold it' })]);
  }

  async function setHold(c, reason) {
    try {
      const d = await api('rows/' + c.uuid + '/hold', { reason });
      if (d.card) replaceCard(d.card);
    } catch (e) { toast(e.message, { error: true }); }
  }

  async function moveBack(c) {
    try {
      await api('rows/' + c.uuid + '/later', { later: false });
      state.cards = state.cards.filter(x => x.uuid !== c.uuid);
      state.counts.later = Math.max(0, state.counts.later - 1);
      state.counts.review = (state.counts.review || 0) + 1;
      toast('Back in review · ' + short(c));
      render();
    } catch (e) { toast(e.message, { error: true }); }
  }

  function openScope() {
    const list = h('div', { class: 'pick-list' });
    let account = state.account, order = state.order;
    list.append(h('div', { class: 'pick-group', text: state.pile === 'later' ? 'Account · saved for later' : 'Account · waiting' }));
    const opts = [{ id: '', short: 'All accounts' }].concat(accounts);
    const pa = state.perAccount;
    for (const a of opts) {
      const n = !pa ? null : a.id === '' ? pa.all : (pa.by[String(a.id)] || 0);
      const b = pickButton(a.short, n === null ? '' : n ? n.toLocaleString('en-IN') : 'none', () => { account = String(a.id); apply(); }, String(a.id) === state.account);
      if (a.name && a.name !== a.short) b.title = a.name;
      list.append(b);
    }
    list.append(h('div', { class: 'pick-group', text: 'Order' }));
    list.append(pickButton('Newest first', '', () => { order = 'newest'; apply(); }, state.order === 'newest'));
    list.append(pickButton('Oldest first', 'the way statements run', () => { order = 'oldest'; apply(); }, state.order === 'oldest'));
    function apply() { closeSheet(); setScope(account, order); }
    openSheet('Show', list);
  }

  // ---- toasts -------------------------------------------------------------------
  function clearToasts() { toastsEl.replaceChildren(); }
  function toast(text, opts = {}) {
    clearToasts();
    const t = h('div', { class: 'toast' + (opts.error ? ' toast-err' : '') }, h('span', { class: 'toast-text', text }));
    if (opts.undo) t.append(h('button', { type: 'button', class: 'btn btn-sm btn-quiet', onclick: () => opts.undo(), text: 'Undo' }));
    const ms = opts.ms || (opts.error ? 6000 : 3500);
    if (opts.ms && !reduceMotion) {
      const bar = h('div', { class: 'toast-bar' });
      t.append(bar);
      bar.animate([{ transform: 'scaleX(1)' }, { transform: 'scaleX(0)' }], { duration: ms, easing: 'linear', fill: 'forwards' });
    }
    toastsEl.append(t);
    setTimeout(() => t.remove(), ms + 150);
  }

  // ---- buttons & keys --------------------------------------------------------------
  btnSend.addEventListener('click', () => decide('right'));
  btnLater.addEventListener('click', () => decide('later'));
  btnMore.addEventListener('click', () => { const c = top(); if (c) openMore(c); });
  scopeBtn.addEventListener('click', openScope);
  for (const b of document.querySelectorAll('.pile')) b.addEventListener('click', () => switchPile(b.dataset.pile));

  document.addEventListener('keydown', e => {
    if (sheet.open || e.altKey) return;
    if (e.target.closest('input, textarea, select, [contenteditable]')) return;
    const c = top();
    const k = e.key.toLowerCase();
    if ((e.metaKey || e.ctrlKey) && k === 'z') { e.preventDefault(); undo(); return; }
    if (e.metaKey || e.ctrlKey) return;
    if (!c && k !== 'u') return;
    switch (k) {
      case 'arrowright': e.preventDefault(); decide('right'); break;
      case 'arrowleft': e.preventDefault(); decide('later'); break;
      case 't': case 'e': e.preventDefault(); openTitle(c); break;
      case 'c': e.preventDefault(); openCategory(c); break;
      case 'p': if (c.direction === 'out') { e.preventDefault(); openPayee(c); } break;
      case 'o': e.preventDefault(); location.href = c.editUrl; break;
      case 'm': e.preventDefault(); openMore(c); break;
      case 'u': e.preventDefault(); undo(); break;
    }
  });

  render();
  load(true);
})();
