#!/usr/bin/env node
/* Regression harness for the admin drives page (internal/admin/web/assets/).
 *
 * Runs the REAL portal scripts (api.js, mock.js, ui.js, pages-main.js,
 * pages-drives.js, app.js) in their ?mock=1 mode inside a minimal
 * tree-building DOM stub, under a deterministic virtual clock — the same
 * approach as scripts/test-relay-page.cjs. Production code paths are
 * exercised unmodified; no real network, storage, popup or cookie is touched.
 *
 * Covers the upstream-connect must-haves:
 *   T1  single 添加网盘 entry opens the choice modal (网盘登录 / API 上游
 *       连接); the modal has NO inputs — upstream URL + key are entered only
 *       in OpenList, never in the portal
 *   T2  clicking a choice mints the handoff: window.open fires synchronously
 *       (before the POST resolves), the opener is severed, POST
 *       /connect/handoff carries the wizard target and the blank window is
 *       navigated to the returned ticket URL (targets /new/goapi and /)
 *   T3  重新授权 mints /reauth/<storage_id> per click — nothing at render
 *   T4  old module (connect_handoff_available false): plain manual links
 *       (connectLink href, 手动登录 OpenList), no handoff POSTs; overview
 *       mirrors it and still lists the goapi platform
 *   T5  blocked popup: the fallback modal appears with a manual link — never
 *       a silent no-op
 *   T6  failed mint (502 with fallback in the envelope): blank window closed,
 *       explicit fallback shown, no automatic retry
 *   T7  wizard return: #/drives?connected=<id> runs ONE sync, opens the adopt
 *       dialog with the suggested ID pre-filled, and adopting POSTs the
 *       mapping and re-renders with the new drive
 *   T8  stale generation: a superseded drives render must not render into its
 *       detached root; navigating away mid-sync must not clobber the new
 *       route's page (app.js lifecycle generation)
 *   T9  mock contract: unknown handoff target → 400; old-module handoff →
 *       200 with empty url and a fallback_url
 *
 * Usage: node scripts/test-drives-page.cjs
 */
'use strict';
const fs = require('fs');
const path = require('path');

const DIR = path.resolve(__dirname, '..', 'internal', 'admin', 'web', 'assets');
const SCRIPTS = ['api.js', 'mock.js', 'ui.js', 'pages-main.js', 'pages-drives.js', 'app.js'];

let failed = 0;
function ok(cond, msg) { console.log((cond ? 'PASS ' : 'FAIL ') + msg); if (!cond) failed++; }

/* ---------- virtual clock (installed BEFORE the scripts load) ---------- */
const realSetTimeout = setTimeout;
const realClearTimeout = clearTimeout;
let vnow = 0;
let timerSeq = 0;
const timers = []; // {id, at, fn}
global.setTimeout = function (fn, ms, ...args) {
  timerSeq += 1;
  timers.push({ id: timerSeq, at: vnow + (ms || 0), fn: () => fn(...args) });
  return timerSeq;
};
global.clearTimeout = function (id) {
  const i = timers.findIndex((t) => t.id === id);
  if (i >= 0) timers.splice(i, 1);
};
async function yieldMicro() {
  for (let i = 0; i < 12; i++) await Promise.resolve();
  await new Promise((r) => realSetTimeout.call(null, r, 0));
}
/** Advance the virtual clock until cond() or the horizon is reached. */
async function pumpUntil(cond, horizonMs, label) {
  const horizon = vnow + (horizonMs || 30000);
  let idle = 0;
  for (;;) {
    if (cond()) return;
    timers.sort((a, b) => a.at - b.at || a.id - b.id);
    const t = timers[0];
    if (!t) {
      // no fireable timer right now: pending promise chains may still register
      // one, so yield the real event loop a bounded number of times first
      idle++;
      if (idle > 60) break;
      await new Promise((r) => realSetTimeout(r, 0));
      continue;
    }
    idle = 0;
    const idx = timers.indexOf(t);
    if (idx >= 0) timers.splice(idx, 1); // one-shot: a fired timer must never re-fire
    vnow = Math.max(vnow, t.at);
    t.fn();
    await yieldMicro();
  }
  await yieldMicro();
  if (!cond()) throw new Error('pumpUntil: condition not met within horizon (' + (label || '') + '), vnow=' + vnow);
}

process.on('unhandledRejection', (e) => {
  console.log('FAIL unhandled rejection: ' + (e && e.stack || e));
  failed++;
});

/* ---------- tree-building DOM stub ---------- */
function mkEl(tag) {
  const el = {
    nodeType: 1, tagName: tag, className: '', children: [], _listeners: {}, attrs: {},
    style: {}, dataset: {}, disabled: false, checked: false, selected: false,
    hidden: false, open: false, textContent: '', innerHTML: '', files: null,
    parentNode: null,
    get firstChild() { return this.children[0] || null; },
    get isConnected() {
      let n = this;
      while (n && n.parentNode) n = n.parentNode;
      return !!(n && n._root);
    },
    append(...cs) { for (const c of cs) if (c && c.nodeType) { c.parentNode = this; this.children.push(c); } },
    prepend(c) { if (c && c.nodeType) { c.parentNode = this; this.children.unshift(c); } },
    appendChild(c) { if (c && c.nodeType) { c.parentNode = this; this.children.push(c); } return c; },
    insertBefore(c, ref) {
      if (!c || !c.nodeType) return c;
      c.parentNode = this;
      const i = ref ? this.children.indexOf(ref) : -1;
      if (i >= 0) this.children.splice(i, 0, c); else this.children.push(c);
      return c;
    },
    removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) this.children.splice(i, 1); c.parentNode = null; return c; },
    remove() { if (this.parentNode) this.parentNode.removeChild(this); },
    addEventListener(t, fn) { (this._listeners[t] = this._listeners[t] || []).push(fn); },
    removeEventListener(t, fn) { const l = this._listeners[t] || []; const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1); },
    setAttribute(k, v) {
      this.attrs[k] = v === undefined ? '' : String(v);
      if (k === 'class') this.className = String(v);
      // mirror the attribute→property reflection real browsers perform
      else if (k === 'checked') this.checked = true;
      else if (k === 'disabled') this.disabled = true;
      else if (k === 'selected') this.selected = true;
      else if (k === 'value') this.value = String(v);
    },
    getAttribute(k) { return this.attrs[k] ?? null; },
    removeAttribute(k) { delete this.attrs[k]; },
    querySelector() { return mkEl('span'); },
    querySelectorAll(sel) {
      const cls = String(sel).replace(/^\.([^ .]+).*$/, '$1');
      return findAll(this, (e) => String(e.className).split(' ').includes(cls));
    },
    closest() { return null; },
    focus() {},
    select() {},
    scrollIntoView() {},
    getBoundingClientRect() { return { height: 120, width: 320, top: 0, bottom: 120, left: 0, right: 320 }; },
    fire(t) { for (const fn of (this._listeners[t] || []).slice()) fn({ target: this, preventDefault() {} }); },
  };
  Object.defineProperty(el, 'textContent', {
    get() { return this._textContent || ''; },
    set(value) { this._textContent = value == null ? '' : String(value); this.children.length = 0; },
  });
  el.classList = {
    add() {}, remove() {}, toggle() {}, contains() { return false; },
  };
  Object.defineProperty(el, 'value', {
    get() {
      if (this.tagName === 'select') {
        const chosen = this.children.find((o) => o.tagName === 'option' && o.selected);
        if (chosen) return chosen.attrs.value ?? '';
        const first = this.children.find((o) => o.tagName === 'option');
        if (first) return first.attrs.value ?? '';
        return this._value ?? '';
      }
      return this._value ?? '';
    },
    set(v) {
      this._value = String(v);
      if (this.tagName === 'select') {
        let hit = false;
        for (const o of this.children) {
          if (o.tagName !== 'option') continue;
          if (!hit && o.attrs.value === String(v)) { o.selected = true; hit = true; } else o.selected = false;
        }
        if (!hit) { const first = this.children.find((o) => o.tagName === 'option'); if (first) first.selected = true; }
      }
    },
  });
  return el;
}
function textOf(el) {
  if (el.children && el.children.length) {
    let out = '';
    for (const c of el.children) {
      if (typeof c === 'string') out += c;
      else if (c.nodeType === 3) out += c.text;
      else if (c.nodeType === 1 && !['input', 'select', 'textarea'].includes(c.tagName)) out += textOf(c);
    }
    return out;
  }
  return el.textContent || '';
}
function findAll(root, pred, out) {
  out = out || [];
  (function walk(el) {
    if (!el || typeof el !== 'object' || el.nodeType !== 1) return;
    try { if (pred(el)) out.push(el); } catch (e) { /* malformed stub node */ }
    for (const c of el.children || []) walk(c);
  }(root));
  return out;
}
function byText(root, tag, text) {
  return findAll(root, (e) => e.tagName === tag && textOf(e).trim() === text);
}

/* ---------- browser globals ---------- */
const byId = {};
const E = (id) => { if (!byId[id]) { const n = mkEl('div'); n.id = id; byId[id] = n; return n; } return byId[id]; };
function makeStorage() { return { _m: {}, getItem(k) { return Object.prototype.hasOwnProperty.call(this._m, k) ? this._m[k] : null; }, setItem(k, v) { this._m[k] = String(v); }, removeItem(k) { delete this._m[k]; } }; }
global.localStorage = makeStorage();
global.sessionStorage = makeStorage();

const winListeners = {};
function fireWindow(t) { for (const fn of (winListeners[t] || []).slice()) fn({}); }

const locationObj = { origin: 'http://127.0.0.1:8080', pathname: '/admin/', search: '?mock=1', href: 'http://127.0.0.1:8080/admin/' };
Object.defineProperty(locationObj, 'hash', {
  get() { return this._hash || ''; },
  set(v) {
    this._hash = String(v);
    this.href = this.origin + this.pathname + this._hash; // mirror real href
    fireWindow('hashchange'); // a hash assignment navigates → app.js re-routes
  },
});
global.location = locationObj;
global.history = { replaceStateCalls: [], replaceState(...a) { this.replaceStateCalls.push(a); } };

/* window.open stub: 'normal' hands out blank windows, 'blocked' returns null */
const openLog = [];   // {url, name}
const openWins = [];  // blank window stubs
let openMode = 'normal';
function makeWin() {
  const win = { closed: false, opener: { severed: false }, navigated: null };
  win.location = {};
  Object.defineProperty(win.location, 'href', {
    get() { return win.navigated || 'about:blank'; },
    set(v) { win.navigated = String(v); },
  });
  win.close = function () { win.closed = true; };
  return win;
}
global.window = global;
global.open = function (url, name) {
  openLog.push({ url: String(url), name: String(name) });
  if (openMode === 'blocked') return null;
  const w = makeWin();
  openWins.push(w);
  return w;
};
global.addEventListener = (t, fn) => { (winListeners[t] = winListeners[t] || []).push(fn); };
global.removeEventListener = (t, fn) => { const l = winListeners[t] || []; const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1); };
global.matchMedia = () => ({ matches: true }); // reduced motion: instant overlay removal
Object.defineProperty(global, 'navigator', { value: {}, configurable: true });

const documentListeners = [];
global.document = {
  _root: true,
  readyState: 'complete', // boot() runs synchronously at script load
  addEventListener(t, fn) { documentListeners.push([t, fn]); },
  removeEventListener() {},
  createTextNode(t) { return { nodeType: 3, text: String(t), parentNode: null }; },
  getElementById(id) { const n = E(id); return n; },
  querySelector: () => null,
  querySelectorAll: () => [],
  body: mkEl('body'),
  documentElement: mkEl('html'),
  activeElement: null,
  createElement: (tag) => mkEl(tag),
};
global.document.body._root = true;

/* no real network may ever happen — fail loudly if a page leaves mock mode */
global.fetch = () => { throw new Error('REAL NETWORK FETCH in mock mode'); };
global.XMLHttpRequest = function () { throw new Error('REAL XHR in mock mode'); };

/* ---------- load the REAL portal scripts ---------- */
global.window.C2A = global.window.C2A || {};
for (const name of SCRIPTS) {
  // eslint-disable-next-line no-eval
  (0, eval)(fs.readFileSync(path.join(DIR, name), 'utf8'));
}
E('page')._root = false;

/* ---------- scenario helpers ---------- */
const page = E('page');
const modalRoot = E('modal-root');
const toastRoot = E('toast-root');

/** Fresh mock instance with a clean db + a call recorder; flags per scenario. */
function freshMock(flags) {
  const m = new window.C2AMockBackend();
  Object.assign(m.flags, flags || {});
  const orig = m.handle.bind(m);
  m.calls = [];
  m.handle = async (method, p, body) => { m.calls.push({ method, path: p, body }); return orig(method, p, body); };
  window.C2A.API.mock = m;
  return m;
}
/** Drop overlays a previous scenario left open — modals only close on user
 * action, and a stale fallback modal would leak into the next assertions. */
function clearOverlays() {
  while (modalRoot.firstChild) modalRoot.removeChild(modalRoot.firstChild);
}
/** The 打开向导 button of ONE choice row: the row is the button's immediate
 * parent — the modal body contains both choices, so any wider ancestor match
 * would always hit the first row. */
function choiceBtn(name) {
  return byText(modalRoot, 'button', '打开向导')
    .filter((b) => b.parentNode && textOf(b.parentNode).includes(name))[0];
}
function callsOf(m, method, path) {
  return m.calls.filter((c) => c.method === method && c.path === path);
}
/** Direct API call that still runs under the virtual clock: the mock delays
 * every answer with a (virtual) timer, so a bare await would park on a
 * promise the event loop never services. */
async function apiCall(method, path, body) {
  let out, err;
  const p = window.C2A.API.call(method, path, body).then((d) => { out = d; }, (e) => { err = e; });
  await pumpUntil(() => out !== undefined || err !== undefined, 5000, 'api ' + method + ' ' + path);
  if (err) throw err;
  return out;
}
/** Navigate like the SPA does and wait until the route finished rendering. */
async function nav(hash) {
  const before = window.C2A.pageGen ? window.C2A.pageGen() : 0;
  locationObj.hash = hash; // fires hashchange → route() chains
  await pumpUntil(() => !window.C2A.pageGen || window.C2A.pageGen() > before, 5000, 'route ' + hash);
  await pumpUntil(() => page.attrs['aria-busy'] !== 'true' && page.children.length > 0, 5000, 'render ' + hash);
}
function primaryArea() {
  return findAll(page, (e) => String(e.className).includes('page-head-actions'))[0] || null;
}

/** Parse a minted wizard URL per the real contract: ticket + target live in
 * the fragment (#handoff=<43-char base64url>&target=<encoded>), only the
 * public landing return is in the query. */
function parseMinted(href) {
  const u = new URL(href);
  return { u: u, frag: new URLSearchParams(u.hash.replace(/^#/, '')) };
}

/* ================= scenarios ================= */

(async function main() {
  /* boot: app.js rendered the overview once — drain it before scenarios */
  freshMock({});
  await pumpUntil(() => page.children.length > 0, 5000, 'boot render');

  /* ---------- S1: handoff-capable module (default flags) ---------- */
  {
    freshMock({});
    openLog.length = 0; openWins.length = 0; openMode = 'normal';
    delete window.C2A.pendingDriveSync;
    await nav('#/drives');

    const primary = primaryArea();
    const addBtns = byText(primary, 'button', '添加网盘');
    ok(addBtns.length === 1, 'T1 single 添加网盘 button is the primary entry');
    ok(byText(primary, 'a', '添加网盘').length === 0 && byText(page, 'a', '手动登录 OpenList').length === 0,
      'T1 no static connect anchor replaces the entry when handoff is available');
    // the only manual @connect anchor left is the orphaned-rebuild fallback
    const connectAnchors = findAll(page, (e) => e.tagName === 'a' && String(e.attrs.href || '').includes('/@connect/'));
    ok(connectAnchors.length === 1 && textOf(connectAnchors[0]).includes('在 OpenList 中重建存储'),
      'T1 only the orphaned rebuild link stays a manual @connect anchor');

    addBtns[0].fire('click');
    ok(modalRoot.children.length > 0, 'T1 choice modal opens');
    const mtext = textOf(modalRoot);
    ok(mtext.includes('网盘登录') && mtext.includes('API 上游连接'), 'T1 modal lists 网盘登录 and API 上游连接');
    ok(mtext.includes('GoAPI'), 'T1 modal names the GoAPI upstream target');
    ok(findAll(modalRoot, (e) => ['input', 'textarea'].includes(e.tagName)).length === 0,
      'T1 modal has no inputs: upstream URL/key are entered only in OpenList');

    /* T2a: the goapi choice mints its handoff on click */
    const goapiBtn = choiceBtn('API 上游连接');
    ok(!!goapiBtn, 'T2 goapi choice carries its 打开向导 action');
    const winsBefore = openWins.length;
    goapiBtn.fire('click');
    ok(openLog.length === winsBefore + 1 && openLog[openLog.length - 1].url === 'about:blank',
      'T2 window.open(about:blank) fired synchronously, before any POST');
    const win = openWins[openWins.length - 1];
    ok(win && win.opener === null, 'T2 blank window opener severed immediately');
    const m1 = window.C2A.API.mock;
    await pumpUntil(() => callsOf(m1, 'POST', '/connect/handoff').length === 1, 5000, 'handoff POST');
    ok(callsOf(m1, 'POST', '/connect/handoff')[0].body.target === '/new/goapi',
      'T2 handoff POST target is /new/goapi');
    await pumpUntil(() => win.navigated, 5000, 'wizard navigation');
    const minted = parseMinted(win.navigated);
    ok(minted.frag.get('handoff') === 'A'.repeat(43),
      'T2 handoff ticket rides in the FRAGMENT: 43-char base64url');
    ok(minted.frag.get('target') === '/new/goapi',
      'T2 fragment target decodes to /new/goapi');
    ok(minted.u.searchParams.has('return')
      && Array.from(minted.u.searchParams.keys()).join(',') === 'return'
      && !minted.u.search.includes('otk') && !minted.u.search.includes('handoff'),
      'T2 query carries ONLY the public landing return — no otk/handoff ticket material');
    ok(decodeURIComponent(minted.u.search).includes('/admin/connect-return?storage_id={storage_id}'),
      'T2 minted URL return points at the public landing (Strict-cookie safe hop)');
    await pumpUntil(() => modalRoot.children.length === 0, 5000, 'choice modal closed');
    ok(true, 'T2 choice modal closed after minting');

    /* T2b: the netdisk choice targets the wizard home */
    byText(primary, 'button', '添加网盘')[0].fire('click');
    const netBtn = choiceBtn('网盘登录');
    netBtn.fire('click');
    const win2 = openWins[openWins.length - 1];
    await pumpUntil(() => win2.navigated, 5000, 'netdisk wizard navigation');
    const minted2 = parseMinted(win2.navigated);
    ok(callsOf(m1, 'POST', '/connect/handoff')[1].body.target === '/', 'T2 netdisk choice mints target /');
    ok(minted2.frag.get('handoff') === 'A'.repeat(43) && minted2.frag.get('target') === '/',
      'T2 netdisk ticket URL fragment carries the ticket and target /');
    await pumpUntil(() => modalRoot.children.length === 0, 5000, 'choice modal closed again');

    /* T3: reauth mints per click, never at render. Every connect-capable
     * drive renders a 重新授权 entry — scope to the aliyun card. */
    freshMock({});
    await nav('#/drives');
    const m2 = window.C2A.API.mock;
    ok(callsOf(m2, 'POST', '/connect/handoff').length === 0, 'T3 render issues no handoff POST');
    const aliyunCard = findAll(page, (e) => e.dataset && e.dataset.driveId === 'aliyun-main')[0];
    const reauthBtns = byText(aliyunCard, 'button', '重新授权');
    ok(reauthBtns.length === 1, 'T3 重新授权 renders as a mint-on-click button');
    reauthBtns[0].fire('click');
    const win3 = openWins[openWins.length - 1];
    await pumpUntil(() => callsOf(m2, 'POST', '/connect/handoff').length === 1, 5000, 'reauth handoff POST');
    ok(callsOf(m2, 'POST', '/connect/handoff')[0].body.target === '/reauth/4',
      'T3 reauth handoff target is /reauth/<storage_id> (aliyun storage 4)');
    await pumpUntil(() => win3.navigated, 5000, 'reauth navigation');
    ok(parseMinted(win3.navigated).frag.get('target') === '/reauth/4',
      'T3 reauth ticket URL fragment decodes to /reauth/4');
  }

  /* ---------- S2: old module (no handoff capability) ---------- */
  {
    freshMock({ nohandoff: '1' });
    openLog.length = 0; openWins.length = 0; openMode = 'normal';
    await nav('#/drives');
    const m = window.C2A.API.mock;
    const primary = primaryArea();
    const manual = byText(primary, 'a', '手动登录 OpenList');
    ok(manual.length === 1, 'T4 old module: primary entry is the 手动登录 OpenList link');
    const manualLanding = decodeURIComponent(String(manual[0].attrs.href));
    ok(manualLanding.includes('/@connect/') && manualLanding.includes('return='),
      'T4 manual link keeps the connectLink href with the return parameter');
    ok(manualLanding.includes('/admin/connect-return?storage_id={storage_id}'),
      'T4 manual link return points at the public landing page, not the SPA hash');
    ok(byText(primary, 'button', '添加网盘').length === 0, 'T4 no choice modal entry without handoff');
    const aliyunCardOld = findAll(page, (e) => e.dataset && e.dataset.driveId === 'aliyun-main')[0];
    const reauthA = byText(aliyunCardOld, 'a', '重新授权');
    ok(reauthA.length === 1 && String(reauthA[0].attrs.href).includes('#/reauth/4'),
      'T4 reauth falls back to the connectLink anchor with the reauth fragment');
    ok(callsOf(m, 'POST', '/connect/handoff').length === 0, 'T4 old-module render issues no handoff POST');
    reauthA[0].fire('click'); // anchor navigation is the browser's job; nothing mints
    await pumpUntil(() => timers.length === 0, 3000, 'old-module settle');
    ok(callsOf(m, 'POST', '/connect/handoff').length === 0, 'T4 clicking the fallback link never mints');

    /* overview mirrors the old-module fallback and still shows the goapi platform */
    await nav('#/overview');
    const ovPrimary = primaryArea();
    ok(byText(ovPrimary, 'a', '手动登录 OpenList').length === 1, 'T4 overview falls back to the manual link too');
    ok(textOf(page).includes('GoAPI 上游'), 'T4 overview platform list still names the GoAPI 上游 platform');
    ok(callsOf(m, 'POST', '/connect/handoff').length === 0, 'T4 overview render issues no handoff POST');

    /* T9: contract — old-module handoff answers 200 with an empty url */
    const doc = await apiCall('POST', '/connect/handoff', { target: '/new/goapi' });
    ok(doc.url === '' && String(doc.fallback_url).includes('/@connect/'),
      'T9 old-module handoff resolves with url:"" and a fallback_url (no error)');
    /* T9: unknown targets are rejected */
    let badStatus = 0;
    try { await apiCall('POST', '/connect/handoff', { target: '/new/evil' }); } catch (e) { badStatus = e.status; }
    ok(badStatus === 400, 'T9 unknown handoff target answers 400');
  }

  /* ---------- S3: blocked popup ---------- */
  {
    freshMock({});
    clearOverlays();
    openLog.length = 0; openWins.length = 0; openMode = 'blocked';
    await nav('#/drives');
    byText(primaryArea(), 'button', '添加网盘')[0].fire('click');
    const goapiBtn = choiceBtn('API 上游连接');
    goapiBtn.fire('click');
    const m = window.C2A.API.mock;
    await pumpUntil(() => callsOf(m, 'POST', '/connect/handoff').length === 1, 5000, 'blocked-popup handoff POST');
    await pumpUntil(() => textOf(modalRoot).includes('浏览器拦截了弹出窗口'), 5000, 'fallback modal');
    const fbLink = findAll(modalRoot, (e) => e.tagName === 'a' && String(e.attrs.href || '').includes('/@connect/'));
    ok(fbLink.length === 1, 'T5 blocked popup shows an explicit manual fallback link, never a silent no-op');
    await pumpUntil(() => timers.length === 0, 3000, 'blocked-popup settle');
    openMode = 'normal';
  }

  /* ---------- S4: failed mint (502 + fallback in the envelope) ---------- */
  {
    const m = freshMock({ handofffail: '1' });
    clearOverlays();
    openLog.length = 0; openWins.length = 0; openMode = 'normal';
    await nav('#/drives');
    byText(primaryArea(), 'button', '添加网盘')[0].fire('click');
    const goapiBtn = choiceBtn('API 上游连接');
    goapiBtn.fire('click');
    const win = openWins[openWins.length - 1];
    await pumpUntil(() => win.closed, 5000, 'blank window closed on mint failure');
    ok(win.closed, 'T6 mint failure closes the blank window');
    await pumpUntil(() => textOf(modalRoot).includes('无法自动登录 OpenList'), 5000, 'mint-failure modal');
    const fbLink = findAll(modalRoot, (e) => e.tagName === 'a' && String(e.attrs.href || '').includes('/@connect/#'));
    ok(fbLink.length === 1 && String(fbLink[0].attrs.href).includes('#/new/goapi'),
      'T6 fallback link comes from the 502 envelope fallback_url (same wizard route)');
    ok(callsOf(m, 'POST', '/connect/handoff').length === 1, 'T6 no automatic retry after a failed mint');
  }

  /* ---------- S5: wizard return → sync → adopt with prefilled ID ---------- */
  {
    freshMock({});
    openLog.length = 0; openWins.length = 0;
    delete window.C2A.pendingDriveSync;
    history.replaceStateCalls.length = 0;
    await nav('#/drives?connected=17'); // the wizard saved unmanaged storage 17 (GoAPI)
    const m = window.C2A.API.mock;
    ok(callsOf(m, 'POST', '/drives/sync').length === 1, 'T7 return flow runs exactly one sync');
    ok(callsOf(m, 'POST', '/connect/handoff').length === 0, 'T7 return flow issues no handoff');
    await pumpUntil(() => textOf(modalRoot).includes('纳管存储 #17'), 5000, 'adopt dialog');
    const idInput = findAll(modalRoot, (e) => e.tagName === 'input')[0];
    ok(!!idInput && idInput.value === 'goapi-upstream',
      'T7 adopt dialog pre-fills the suggested drive ID from the mount path');
    const adoptBtn = byText(modalRoot, 'button', '纳管')[0];
    ok(!!adoptBtn, 'T7 adopt dialog offers the 纳管 action');
    adoptBtn.fire('click');
    await pumpUntil(() => callsOf(m, 'POST', '/drives/adopt').length === 1, 5000, 'adopt POST');
    ok(callsOf(m, 'POST', '/drives/adopt')[0].body.storage_id === 17 && callsOf(m, 'POST', '/drives/adopt')[0].body.id === 'goapi-upstream',
      'T7 adopt POST carries storage_id and the confirmed ID');
    await pumpUntil(() => callsOf(m, 'GET', '/drives').length >= 2, 5000, 'page re-rendered after adopt');
    await pumpUntil(() => textOf(page).includes('goapi-upstream'), 5000, 'new drive visible');
    ok(true, 'T7 adopted GoAPI upstream drive appears as a managed drive');
    ok(history.replaceStateCalls.length >= 1, 'T7 connected query is stripped from the URL after the flow');
    await nav('#/overview');
  }

  /* ---------- S6a: overlapping renders — the superseded one never renders ---------- */
  {
    freshMock({});
    clearOverlays();
    openLog.length = 0; openWins.length = 0;
    delete window.C2A.pendingDriveSync;
    // Raw hash writes (no hashchange) keep the real router out of this
    // scenario: exactly two direct page instances race, A supersedes itself.
    locationObj._hash = '#/drives?connected=17'; // A would run the connected flow
    const rootA = mkEl('div');
    const rootB = mkEl('div');
    let doneA = false, doneB = false;
    const pA = window.C2A.pages.drives(rootA).finally(() => { doneA = true; }); // started first…
    locationObj._hash = '#/drives';                                             // …then the route re-entered
    const pB = window.C2A.pages.drives(rootB).finally(() => { doneB = true; });
    await pumpUntil(() => doneA && doneB, 8000, 'overlapping renders'); // drives the mock's virtual delays
    await Promise.allSettled([pA, pB]);
    ok(findAll(rootA, (e) => e.dataset && e.dataset.driveId).length === 0,
      'T8 superseded render leaves no drive cards in its detached root');
    ok(findAll(rootB, (e) => e.dataset && e.dataset.driveId === 'baidu-main').length > 0,
      'T8 newest render owns the page and shows the drives');
    ok(modalRoot.children.length === 0, 'T8 superseded connected flow never opens the adopt dialog');
    locationObj._hash = '#/drives';
  }

  /* ---------- S6b: route change mid-sync — old page must not clobber ---------- */
  {
    freshMock({});
    openLog.length = 0; openWins.length = 0;
    delete window.C2A.pendingDriveSync;
    await nav('#/drives');
    const genDrives = window.C2A.pageGen();
    byText(primaryArea(), 'button', '刷新并纳管')[0].fire('click'); // sync now in flight
    await nav('#/overview'); // navigate away while the sync request is pending
    ok(window.C2A.pageGen() > genDrives, 'T8 route change bumps the app lifecycle generation');
    await pumpUntil(() => timers.length === 0, 8000, 'mid-sync navigation settles');
    const m = window.C2A.API.mock;
    ok(callsOf(m, 'POST', '/drives/sync').length === 1, 'T8 the in-flight sync still completed');
    ok(textOf(page).includes('总览') && !textOf(page).includes('网盘管理'),
      'T8 stale sync continuation did not re-render drives over the overview route');
  }

  console.log(failed === 0 ? '\nALL PASS' : '\nFAILED: ' + failed);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => {
  console.error('HARNESS ERROR', e && e.stack || e);
  process.exit(1);
});
