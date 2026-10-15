#!/usr/bin/env node
/* Regression harness for the agent-facing onboarding snippets in the keys page
 * (internal/admin/web/assets/pages-keys.js) and the mock backend prompt
 * (internal/admin/web/assets/mock.js).
 *
 * Runs the REAL portal scripts in their ?mock=1 mode inside a minimal
 * tree-building DOM stub — the same approach as scripts/test-key-form.cjs —
 * with the real C2AMockBackend answering, so create / rotate / use-key dialogs
 * are exercised through their production code paths.
 *
 * Covers the basic-onboarding must-haves:
 *   T1  mock onboardingPrompt uses the basic wording (basic file API, me-first,
 *       basic integration instructions, Basic OpenAPI) and never advertises
 *       advanced routes, compat (Gokapi), admin endpoints or /mcp
 *   T2  create flow: result dialog shows the backend prompt, a per-drive
 *       quickstart (default drive chosen, drive as one encoded path segment,
 *       encoded path query, key only in apikey headers), {{BASE}} resolved
 *       from /settings, and no alias/advanced/compat/MCP offers
 *   T3  rotate flow on a key with default_drive=null: quickstart falls back to
 *       the first granted drive, everything else like T2
 *   T4  use-key dialog (relay-permitted key): single basic onboarding with the
 *       masked key placeholder, no Gokapi tab / no MCP, prompt identical to
 *       what the backend would produce for that key
 *   T5  preserved behavior: relay permission display in the details rows and
 *       the relay checkbox gating in the key form stay intact
 *
 * Usage: node scripts/test-agent-onboarding.cjs
 */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const DIR = path.resolve(__dirname, '../internal/admin/web/assets');

function mkEl(tag) {
  const el = {
    nodeType: 1, tagName: tag, className: '', children: [], _listeners: {}, attrs: {},
    style: {}, dataset: {}, value: '', disabled: false, checked: false,
    selected: false, hidden: false, open: false, textContent: '', innerHTML: '',
    // select.value must reflect the selected option like a real browser
    get value() {
      if (this.tagName === 'select') {
        const chosen = this.children.find((o) => o.tagName === 'option' && o.selected);
        if (chosen) return chosen.attrs.value ?? '';
        const first = this.children.find((o) => o.tagName === 'option');
        if (first) return first.attrs.value ?? '';
        return this._value ?? '';
      }
      // a textarea's initial text child is its value until value is assigned
      if (this.tagName === 'textarea' && this._value === undefined) {
        let out = '';
        for (const c of this.children) if (c.nodeType === 3) out += (c.nodeValue !== undefined ? c.nodeValue : c.text);
        return out;
      }
      return this._value ?? '';
    },
    set value(v) {
      this._value = String(v);
      if (this.tagName === 'select') {
        let hit = false;
        for (const o of this.children) {
          if (o.tagName !== 'option') continue;
          if (!hit && o.attrs.value === String(v)) { o.selected = true; hit = true; }
          else o.selected = false;
        }
        if (!hit) { const first = this.children.find((o) => o.tagName === 'option'); if (first) first.selected = true; }
      }
    },
    get childNodes() { return this.children; },
    get firstChild() { return this.children[0] || null; },
    append(...cs) { for (const c of cs) this.children.push(c); },
    prepend(c) { this.children.unshift(c); },
    appendChild(c) { this.children.push(c); return c; },
    insertBefore(c, ref) {
      const i = this.children.indexOf(ref);
      if (i >= 0) this.children.splice(i, 0, c); else this.children.push(c);
      return c;
    },
    removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) this.children.splice(i, 1); },
    addEventListener(t, fn) { (this._listeners[t] = this._listeners[t] || []).push(fn); },
    removeEventListener() {},
    setAttribute(k, v) {
      this.attrs[k] = v === undefined ? '' : String(v);
      if (k === 'class') this.className = String(v);
      else if (k === 'checked') this.checked = true;
      else if (k === 'value') this.value = String(v);
      else if (k === 'disabled') this.disabled = true;
      else if (k === 'selected') this.selected = true;
    },
    getAttribute(k) { return this.attrs[k] ?? null; },
    removeAttribute() {},
    querySelector() { return mkEl('span'); },
    querySelectorAll(sel) {
      const cls = String(sel).replace(/^\./, '').trim();
      const out = [];
      (function w(el) {
        for (const c of el.children || []) {
          if (c.nodeType === 1) {
            if (String(c.className).split(' ').includes(cls)) out.push(c);
            w(c);
          }
        }
      }(this));
      return out;
    },
    closest() { return null; },
    focus() {},
    fire(t) { for (const fn of (this._listeners[t] || []).slice()) fn({ target: this }); },
  };
  el.classList = { add() {}, remove() {}, toggle() {}, contains() { return false; } };
  return el;
}
function textOf(el) {
  let out = '';
  for (const c of el.children || []) {
    if (typeof c === 'string') out += c;
    else if (c.nodeType === 3) out += (c.nodeValue !== undefined ? c.nodeValue : c.text);
    else if (c.nodeType === 1 && !['input', 'select', 'textarea'].includes(c.tagName)) out += textOf(c);
  }
  return out;
}
/** Element text with copy buttons excluded — codebox text without the
 * 复制 label that the real ui.js appends as a child button. */
function codeText(el) {
  let out = '';
  for (const c of el.children || []) {
    if (c.nodeType === 3) out += (c.nodeValue !== undefined ? c.nodeValue : c.text);
    else if (c.nodeType === 1 && c.tagName !== 'button') out += codeText(c);
  }
  return out;
}
/** Visible text plus codebox/textarea contents (textOf skips form fields). */
function fullText(el) {
  let out = textOf(el);
  for (const t of findAll(el, (x) => x.tagName === 'textarea')) out += '\n' + (t.value || '');
  for (const pre of findAll(el, (x) => String(x.className).split(' ').includes('codebox'))) out += '\n' + codeText(pre);
  return out;
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

const store = {};
global.localStorage = { getItem: (k) => store[k] ?? null, setItem: (k, v) => { store[k] = String(v); } };
global.sessionStorage = { getItem: () => null, setItem: () => {} };
global.location = { search: '?mock=1', href: 'http://x/admin/app.html', origin: 'http://x', hash: '#/keys', pathname: '/admin/app.html' };
Object.defineProperty(global, 'navigator', { value: {}, configurable: true });
if (typeof global.crypto === 'undefined') {
  try { Object.defineProperty(global, 'crypto', { value: require('node:crypto').webcrypto }); } catch (e) { /* very old Node */ }
}
global.window = global;
global.document = {
  readyState: 'loading',
  addEventListener() {}, removeEventListener() {},
  createTextNode(t) { return { nodeType: 3, text: String(t), nodeValue: String(t) }; },
  getElementById: () => null,
  querySelector: () => null,
  querySelectorAll: () => [],
  body: mkEl('body'),
  documentElement: { dataset: {} },
  createElement: (tag) => mkEl(tag),
};
global.window.C2A = global.window.C2A || {};

for (const f of ['api.js', 'mock.js', 'ui.js', 'pages-main.js', 'pages-drives.js', 'pages-keys.js', 'app.js']) {
  eval(fs.readFileSync(DIR + '/' + f, 'utf8'));
}
const C2A = window.C2A;

/* ---- UI stubs: capture modals / confirm dialogs / row menus ---- */
const modals = [];
C2A.modal = (opts) => {
  const bodyEl = mkEl('div'), footEl = mkEl('div');
  const m = { opts, body: bodyEl, foot: footEl, closed: false };
  const closeFn = () => { m.closed = true; if (opts.onClose) opts.onClose(); };
  m.close = closeFn;
  if (opts.body) opts.body(bodyEl, closeFn);
  if (opts.foot) opts.foot(footEl, closeFn);
  modals.push(m);
  return m;
};
let confirmOpts = null;
C2A.confirmModal = (opts) => { confirmOpts = opts; };
C2A.toast = () => {};
const menus = [];
C2A.menu = (items) => { const btn = mkEl('button'); menus.push({ items, btn }); return btn; };

/* real in-memory mock answers every admin API call */
const backend = new window.C2AMockBackend();
C2A.API.mock = backend;
const BASE = backend.db.settings.public_base_url; // http://198.51.100.20:8080

const tick = (ms) => new Promise((r) => setTimeout(r, ms));
function keyRowEl(root, id) {
  const cands = findAll(root, (el) => el.tagName === 'tr' && !String(el.className).includes('row-details') && textOf(el).includes(id));
  return cands.find((t) => findAll(t, (el) => String(el.className).includes('cell-main'))
    .some((c) => { const s = textOf(c).trim(); return s === id || s === '↳ ' + id; })) || null;
}
function findKeyRowMenu(root, id, label) {
  const tr = keyRowEl(root, id);
  if (!tr) throw new Error('row for key ' + id + ' not found');
  const rec = menus.find((m) => (function contains(hay, node) {
    if (hay === node) return true;
    for (const c of hay.children || []) if (contains(c, node)) return true;
    return false;
  })(tr, m.btn));
  if (!rec) throw new Error('row menu for key ' + id + ' not found');
  const item = rec.items.find((it) => it.label === label);
  if (!item) throw new Error('menu item ' + label + ' not found for ' + id);
  return item;
}
function lastModal() { return modals[modals.length - 1]; }

const HIDDEN_ANYWHERE = [
  '/mcp', 'MCP', 'gokapi-agent-instructions', 'Gokapi', '/api/files/', '/api/info/',
  '/v1/list', '/v1/files', '/v1/tree', '/v1/search', '/v1/fetch', '/v1/batch',
  '/v1/selftest', '/v1/trash', '/v1/activity', '/v1/keys', '/admin/', '/@connect', '{{BASE}}',
];

(async () => {
  let failed = 0;
  const ok = (cond, msg) => { console.log((cond ? 'PASS ' : 'FAIL ') + msg); if (!cond) failed++; };
  const noHidden = (text, label) => {
    for (const bad of HIDDEN_ANYWHERE) ok(!text.includes(bad), label + ' hides ' + JSON.stringify(bad));
    ok(!/\/v1\/drives(?![/\w.-])/.test(text), label + ' has no bare /v1/drives discovery alias');
    ok(!/[?&]apikey=/.test(text), label + ' never puts the key in a URL');
    ok(!/https?:\/\/\S*sk-c2a-/.test(text), label + ' never embeds a key plaintext in a URL');
  };

  /* ---- T1: mock backend prompt matches the basic wording ---- */
  {
    const key = backend.db.keys.find((k) => k.id === 'histopilot');
    const prompt = backend.onboardingPrompt(key, 'sk-c2a-unit-test-plaintext');
    ok(prompt.includes('basic file API'), 'T1 prompt names the basic file API');
    ok(prompt.includes('First call GET /v1/me'), 'T1 prompt starts discovery at /v1/me');
    ok(prompt.includes('Fetch and follow the basic integration instructions:'), 'T1 prompt links the basic instructions');
    ok(prompt.includes('Basic OpenAPI:'), 'T1 prompt labels the OpenAPI as basic');
    ok(prompt.includes(BASE + '/custom/agent-instructions.txt'), 'T1 prompt links agent-instructions.txt');
    ok(prompt.includes(BASE + '/custom/c2a.py') && prompt.includes(BASE + '/custom/SKILL.md'), 'T1 prompt links helper and skill');
    ok(prompt.includes('Drives: baidu-main (read/write, root /agents/histopilot), pan123-main (read)'),
      'T1 drive modes match the backend wording (got: ' + (prompt.split('Drives: ')[1] || '').split('\n')[0] + ')');
    noHidden(prompt, 'T1 prompt');
  }

  /* ---- T2: create flow result dialog ---- */
  {
    const root = mkEl('div');
    await C2A.pages.keys(root);
    const createBtn = findAll(root, (el) => el.tagName === 'button' && textOf(el).includes('创建 Key'))[0];
    ok(!!createBtn, 'T2 create button rendered');
    createBtn.fire('click');
    await tick(600); // GET /drives + dialog open

    const form = lastModal();
    const singleSel = findAll(form.body, (el) => el.tagName === 'select' &&
      el.children.some((o) => o.tagName === 'option' && textOf(o).includes('选择网盘…')))[0];
    ok(!!singleSel, 'T2 create dialog shows the single-drive picker');
    singleSel.value = 'baidu-main';
    singleSel.fire('change');

    const submit = findAll(form.foot, (el) => el.tagName === 'button' && textOf(el).includes('创建并显示明文'))[0];
    ok(!!submit, 'T2 create submit button present');
    submit.fire('click');
    await tick(700); // POST /keys + result dialog + /settings base swap

    const res = lastModal();
    ok(res !== form, 'T2 result dialog opened after create');
    const createdKey = backend.db.keys[backend.db.keys.length - 1];
    const keyBox = findAll(res.body, (el) => String(el.className).split(' ').includes('key-plain'))[0];
    const plaintext = keyBox ? codeText(keyBox) : '';
    ok(/^sk-c2a-/.test(plaintext), 'T2 plaintext key shown once in the result dialog');

    const ta = findAll(res.body, (el) => el.tagName === 'textarea')[0];
    ok(!!ta && ta.value === backend.onboardingPrompt(createdKey, plaintext),
      'T2 onboarding textarea equals the backend basic prompt for the created key');
    ok(textOf(res.body).includes('基础接入提示'), 'T2 onboarding section is explicitly labeled basic');

    const text = fullText(res.body);
    ok(text.includes('/v1/drives/' + encodeURIComponent('baidu-main') + '/list?path=' + encodeURIComponent('/')),
      'T2 quickstart lists the default drive via the per-drive basic route');
    ok(text.includes('/v1/drives/' + encodeURIComponent('baidu-main') + '/files?path=' + encodeURIComponent('/report.pdf')),
      'T2 quickstart upload/download uses the encoded per-drive files route');
    ok(!text.includes('?path=/report.pdf') && !text.includes("?path='"),
      'T2 example paths are URL-encoded, never raw');
    ok(text.includes(BASE + '/v1/me'), 'T2 quickstart keeps the /v1/me self-check');
    ok(text.includes(BASE + '/custom/c2a.py') && text.includes(BASE + '/custom/SKILL.md'),
      'T2 helper and skill offers preserved');
    noHidden(text, 'T2 dialog');
    ok(text.includes('apikey: ' + plaintext), 'T2 key rides in the apikey header');
  }

  /* ---- T3: rotate flow, default_drive null → first granted drive ---- */
  {
    menus.length = 0;
    const root = mkEl('div');
    await C2A.pages.keys(root);
    const item = findKeyRowMenu(root, 'old-expired', '轮换密钥');
    confirmOpts = null;
    await item.onSelect();
    ok(!!confirmOpts, 'T3 rotate opens a confirm dialog');
    await confirmOpts.onConfirm();
    await tick(700);

    const res = lastModal();
    const rotated = backend.findKey('old-expired');
    const keyBox = findAll(res.body, (el) => String(el.className).split(' ').includes('key-plain'))[0];
    const plaintext = keyBox ? codeText(keyBox) : '';
    ok(/^sk-c2a-/.test(plaintext), 'T3 rotated plaintext shown');
    const ta = findAll(res.body, (el) => el.tagName === 'textarea')[0];
    ok(!!ta && ta.value === backend.onboardingPrompt(rotated, plaintext),
      'T3 onboarding textarea equals the backend basic prompt for the rotated key');
    const text = fullText(res.body);
    ok(text.includes('/v1/drives/' + encodeURIComponent('pan123-main') + '/list?path=' + encodeURIComponent('/')),
      'T3 quickstart falls back to the first granted drive when default_drive is null');
    ok(!text.includes('/v1/drives/baidu-main'), 'T3 ungranted drives never appear in the quickstart');
    noHidden(text, 'T3 dialog');
  }

  /* ---- T4: use-key dialog on a relay-permitted key ---- */
  {
    menus.length = 0;
    const root = mkEl('div');
    await C2A.pages.keys(root);
    const tr = keyRowEl(root, 'relay-handoff');
    ok(!!tr, 'T4 relay key row rendered');
    const useBtn = findAll(tr, (el) => el.tagName === 'button' && textOf(el) === '使用')[0];
    ok(!!useBtn, 'T4 使用 button present');
    useBtn.fire('click');
    await tick(600); // GET /settings

    const dlg = lastModal();
    const masked = 'sk-c2a-****（明文仅在创建/轮换时显示一次；如已丢失请轮换）';
    const ta = findAll(dlg.body, (el) => el.tagName === 'textarea')[0];
    ok(!!ta && ta.value === backend.onboardingPrompt(backend.findKey('relay-handoff'), masked),
      'T4 use dialog shows exactly the backend basic prompt with the masked placeholder');
    ok(ta.value.includes('sk-c2a-****'), 'T4 prompt uses the masked key placeholder');
    const text = fullText(dlg.body);
    ok(!String(dlg.body.className).includes('usekey-tabs') &&
      !findAll(dlg.body, (el) => String(el.className).includes('usekey-tab')).length,
      'T4 no Gokapi compatibility tab remains');
    ok(text.includes(BASE + '/v1/me'), 'T4 quickstart keeps the /v1/me self-check');
    ok(text.includes('/v1/drives/' + encodeURIComponent('baidu-main') + '/list?path=' + encodeURIComponent('/')),
      'T4 quickstart lists the default drive via the per-drive basic route');
    ok(text.includes('$C2A_KEY'), 'T4 quickstart carries the key via env var + header');
    noHidden(text, 'T4 dialog');
  }

  /* ---- T5: preserved permission behavior (relay display + form gating) ---- */
  {
    const root = mkEl('div');
    await C2A.pages.keys(root);
    const relayDt = findAll(root, (el) => el.tagName === 'dt' && textOf(el) === 'Gokapi 中转');
    ok(relayDt.length >= 2, 'T5 details rows still show the Gokapi 中转 config block');
    const page = textOf(root);
    ok(page.includes('未启用（缺少 relay 权限）'), 'T5 non-relay key still reports the missing relay permission');
    ok(page.includes('配额 20.0 GiB') && page.includes('保留 3 天'), 'T5 relay key still shows drive/quota/retention config');

    // form: relay checkbox is gated by the relay permission like before
    const createBtn = findAll(root, (el) => el.tagName === 'button' && textOf(el).includes('创建 Key'))[0];
    createBtn.fire('click');
    await tick(600);
    const form = lastModal();
    const relayLabel = findAll(form.body, (el) => String(el.className).includes('relay-check'))[0];
    const relayCb = relayLabel ? relayLabel.children.find((c) => c.tagName === 'input') : null;
    ok(!!relayCb && relayCb.checked && !relayCb.disabled,
      'T5 create with 通用 preset: relay enabled by default (has relay permission)');
    const roChip = findAll(form.body, (el) => String(el.className).includes('chip-btn') && textOf(el).includes('只读'))[0];
    roChip.fire('click');
    await tick(50);
    ok(relayCb.checked === false && relayCb.disabled === true,
      'T5 readonly preset still disables relay (no implicit relay upload)');
  }

  console.log(failed ? ('\n' + failed + ' FAILED') : '\nALL TESTS PASSED');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error('HARNESS ERROR', e); process.exit(1); });
