#!/usr/bin/env node
/* Frontend contract test for the connect wizard handoff (pure Node, no DOM,
 * no network, no dependencies): runs the real web/app.js inside a vm sandbox
 * with private stubs and asserts the wire contract with the portal and the
 * patched OpenList backend.
 *
 * Usage: node scripts/test-connect-handoff.cjs
 *
 * Covered:
 *  1. Portal contract target without '#' (…#handoff=<43b64url>&target=%2Fnew%2Fgoapi)
 *     is accepted and lands on '#/new/goapi'.
 *  2. The fragment is removed via history.replaceState BEFORE any fetch.
 *  3. A malformed code (length/charset), duplicate params or malformed
 *     percent-encoding produce the explicit error panel and NO fetch at all,
 *     without crashing boot; the fragment is removed first in every case.
 *  4. returnHref(null) on the portal landing contract
 *     (/admin/connect-return?storage_id={storage_id}) yields the query-less
 *     landing page itself (…/admin/connect-return), never '?storage_id='.
 *  5. returnHref(valid id) keeps the landing page with the real id.
 *  6. Neither the ticket nor any session token is written to
 *     localStorage/sessionStorage or the URL query. */

'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

const APP_JS = path.join(__dirname, '..', 'src', 'server', 'connect', 'web', 'app.js');

// The portal contract uses a loopback return target (the page validator only
// accepts same-host or loopback return URLs).
const LANDING = 'http://127.0.0.1:8098/admin/connect-return?storage_id={storage_id}';
const LANDING_FIXED = 'http://127.0.0.1:8098/admin/connect-return'; // query-less landing mode
const OL_ORIGIN = 'https://ol.example';
const GOOD_CODE = 'A'.repeat(43); // 32 bytes, base64url unpadded

// ---- tiny DOM ----

function makeNode(tag) {
  const node = {
    tagName: tag,
    _isNode: true,
    className: '',
    attrs: {},
    listeners: {},
    children: [],
    textContent: '',
    value: '',
    appendChild(c) { this.children.push(c); return c; },
    insertBefore(c) { this.children.unshift(c); return c; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
    querySelectorAll() { return []; },
  };
  return node;
}

function walk(node, fn) {
  if (!node || typeof node !== 'object') return;
  fn(node);
  (node.children || []).forEach((c) => walk(c, fn));
}

function collectText(node) {
  let out = '';
  walk(node, (n) => { out += (n.textContent || ''); });
  return out;
}

function collectHrefs(node) {
  const hrefs = [];
  walk(node, (n) => { if (n.attrs && n.attrs.href) hrefs.push(n.attrs.href); });
  return hrefs;
}

// ---- sandbox ----

function makeEnv({ hash, returnUrl }) {
  const calls = []; // ordered: 'replaceState <url>', 'fetch <url>', 'set-hash <h>', 'storage <kind> <k>'
  const app = makeNode('div');
  const search = returnUrl ? '?return=' + encodeURIComponent(returnUrl) : '';

  const location = {
    protocol: 'https:',
    host: 'ol.example',
    hostname: 'ol.example',
    origin: OL_ORIGIN,
    pathname: '/@connect/',
    search,
    href: OL_ORIGIN + '/@connect/' + search,
    _hash: hash || '',
  };
  Object.defineProperty(location, 'hash', {
    get() { return this._hash; },
    set(v) { this._hash = String(v); calls.push('set-hash ' + v); },
  });

  const history = {
    replaceState(_state, _title, url) {
      const u = String(url);
      const i = u.indexOf('#');
      location._hash = i >= 0 ? u.slice(i) : '';
      calls.push('replaceState ' + u);
    },
  };

  const storage = (kind) => ({
    getItem() { return null; },
    setItem(k) { calls.push('storage ' + kind + ' ' + k); },
    removeItem(k) { calls.push('storage-remove ' + kind + ' ' + k); },
  });

  const fetchStub = (url) => {
    calls.push('fetch ' + url);
    let plan = { code: 200, data: null };
    if (url.endsWith('/api/connect/handoff/exchange')) {
      plan = { code: 200, data: { username: 'admin-tok', expires_in: 1800 } };
    } else if (url.endsWith('/api/admin/connect/auth/me')) {
      plan = { code: 200, data: { id: 1, username: 'admin-tok', role: 2, scope: 'connect' } };
    }
    return Promise.resolve({
      status: 200,
      json: () => Promise.resolve({ code: plan.code, message: 'success', data: plan.data }),
    });
  };

  // app.js guards appendChild with `child instanceof Node`; Symbol.hasInstance
  // makes the check work across the vm realm for the host-side stub nodes.
  const NodeStub = function Node() {};
  // Inherited Symbol.hasInstance is non-writable: use defineProperty.
  Object.defineProperty(NodeStub, Symbol.hasInstance, { value: (o) => !!(o && o._isNode) });
  const sandbox = {
    console: { log() {}, error() {}, warn() {} },
    setTimeout, clearTimeout, setInterval, clearInterval,
    TextEncoder, Promise, URL, URLSearchParams,
    encodeURIComponent, decodeURIComponent,
    Node: NodeStub,
    document: {
      readyState: 'complete',
      addEventListener() {},
      createElement: (t) => makeNode(t),
      createTextNode: (t) => { const n = makeNode('#text'); n.textContent = t; return n; },
      getElementById: (id) => (id === 'app' ? app : makeNode('div')),
    },
    navigator: {},
    location,
    history,
    localStorage: storage('local'),
    sessionStorage: storage('session'),
    fetch: fetchStub,
  };
  sandbox.window = sandbox;
  sandbox.window.addEventListener = function () {};
  vm.createContext(sandbox);
  return { sandbox, app, calls, location };
}

function boot(env) {
  const src = fs.readFileSync(APP_JS, 'utf8');
  vm.runInContext(src, env.sandbox, { filename: 'app.js' });
}

const flush = (ms) => new Promise((r) => setTimeout(r, ms));

async function until(fn, what) {
  for (let i = 0; i < 100; i++) {
    if (fn()) return;
    await flush(10);
  }
  throw new Error('condition never became true: ' + what);
}

// evalReturnHref loads only the returnUrl/returnHref slice of app.js.
function evalReturnHref(returnUrlValue) {
  const src = fs.readFileSync(APP_JS, 'utf8');
  const start = src.indexOf('var returnUrl');
  const end = src.indexOf('// ---------- 状态 ----------');
  assert(start > 0 && end > start, 'returnUrl slice markers found');
  const slice = src.slice(start, end) + '\nthis.__t = { returnHref, returnUrl };';
  const search = returnUrlValue ? '?return=' + encodeURIComponent(returnUrlValue) : '';
  const location = {
    protocol: 'https:', host: 'ol.example', hostname: 'ol.example', origin: OL_ORIGIN,
    pathname: '/@connect/', search,
    href: OL_ORIGIN + '/@connect/' + search,
    hash: '',
  };
  const sb = { location, URL, URLSearchParams, console };
  vm.createContext(sb);
  vm.runInContext(slice, sb, { filename: 'returnHref-slice.js' });
  return sb.__t;
}

// ---- scenarios ----

async function handoffAcceptedWithPlainTarget() {
  const target = encodeURIComponent('/new/goapi'); // portal contract: no '#'
  const env = makeEnv({ hash: '#handoff=' + GOOD_CODE + '&target=' + target, returnUrl: LANDING });
  boot(env);
  await until(() => env.calls.some((c) => c.startsWith('set-hash')), 'navigation to target');

  const firstReplace = env.calls.findIndex((c) => c.startsWith('replaceState'));
  const firstFetch = env.calls.findIndex((c) => c.startsWith('fetch'));
  assert.ok(firstReplace >= 0 && firstFetch > firstReplace,
    'fragment must be removed before the first fetch, got: ' + JSON.stringify(env.calls));
  assert.ok(env.calls.some((c) => c === 'fetch ' + OL_ORIGIN + '/api/connect/handoff/exchange'), 'exchange fetched');
  assert.ok(env.calls.some((c) => c === 'fetch ' + OL_ORIGIN + '/api/admin/connect/auth/me'), 'auth/me fetched');
  assert.ok(env.calls.some((c) => c === 'set-hash #/new/goapi'),
    'plain target /new/goapi must land on #/new/goapi, got: ' +
    JSON.stringify(env.calls.filter((c) => c.startsWith('set-hash'))));
  assert.strictEqual(env.location.search, '?return=' + encodeURIComponent(LANDING),
    'the ticket must never appear in the URL query');
  assert.ok(!env.calls.some((c) => c.startsWith('storage')),
    'ticket/session must not be persisted: ' + JSON.stringify(env.calls));
  assert.ok(!collectText(env.app).includes(GOOD_CODE), 'ticket must not be rendered');
}

async function handoffAcceptsLegacyHashedTarget() {
  const target = encodeURIComponent('#/reauth/252');
  const env = makeEnv({ hash: '#handoff=' + GOOD_CODE + '&target=' + target, returnUrl: LANDING });
  boot(env);
  await until(() => env.calls.some((c) => c.startsWith('set-hash')), 'navigation to target');
  assert.ok(env.calls.some((c) => c === 'set-hash #/reauth/252'), 'legacy #target still normalized');
}

async function handoffAcceptsFullTargetSet() {
  for (const target of ['/', '/app/goapi', '/reauth/9223372036854775807']) {
    const env = makeEnv({hash: '#handoff=' + GOOD_CODE + '&target=' + encodeURIComponent(target), returnUrl: LANDING});
    boot(env);
    await until(() => env.calls.some(c => c.includes('/api/admin/connect/auth/me')), 'scoped identity for ' + target);
    await flush(30);
    assert.strictEqual(env.location.hash, '#' + target, 'target must be preserved after one decode');
  }
}

async function handoffRejectsArbitraryTarget() {
  const target = encodeURIComponent('/@manage/storages');
  const env = makeEnv({ hash: '#handoff=' + GOOD_CODE + '&target=' + target, returnUrl: LANDING });
  boot(env);
  await flush(30);
  assert.ok(collectText(env.app).includes('免登录进入失败'), 'unrecognised target must fail explicitly');
  assert.ok(!env.calls.some((c) => c.startsWith('fetch')), 'invalid target must not exchange the ticket');
  assert.ok(!env.calls.some((c) => c.includes('@manage')), 'arbitrary target must not navigate');
}

async function malformedCases() {
  const badHashes = [
    '#handoff=' + 'a'.repeat(42) + '&target=%2Fnew%2Fgoapi', // too short
    '#handoff=' + '*'.repeat(43),                            // bad charset
    '#handoff=' + GOOD_CODE + '&handoff=' + GOOD_CODE,       // duplicate param
    '#handoff=%ZZ&target=%2Fnew%2Fgoapi',                    // malformed encoding
    '#handoff=' + GOOD_CODE + '&target=%252Fnew%252Fgoapi',  // double-encoded target
    '#handoff=' + GOOD_CODE + '&target=%2Freauth%2F9223372036854775808', // int64 overflow
    '#handoff',                                              // no '=' at all
  ];
  for (const hash of badHashes) {
    const env = makeEnv({ hash, returnUrl: LANDING });
    boot(env); // must not throw
    await flush(30);
    assert.ok(env.calls.some((c) => c.startsWith('replaceState')),
      hash + ': fragment must be removed even when malformed');
    assert.ok(!env.calls.some((c) => c.startsWith('fetch')),
      hash + ': malformed handoff must not fetch, got ' + JSON.stringify(env.calls));
    const text = collectText(env.app);
    assert.ok(text.includes('免登录进入失败'), hash + ': explicit error panel rendered');
    const hrefs = collectHrefs(env.app);
    assert.ok(hrefs.includes(LANDING_FIXED),
      hash + ': return link must point at the fixed portal route, got ' + JSON.stringify(hrefs));
    assert.ok(!hrefs.some((h) => h.includes('storage_id=')),
      hash + ': return link must not carry an empty storage_id');
  }
}

function returnHrefUnit() {
  const t = evalReturnHref(LANDING);
  assert.strictEqual(t.returnUrl, LANDING, 'return URL accepted (loopback)');
  assert.strictEqual(t.returnHref(252), 'http://127.0.0.1:8098/admin/connect-return?storage_id=252',
    'valid id keeps the landing page with the id');
  assert.strictEqual(t.returnHref(null), LANDING_FIXED, 'null id → query-less landing page');
  assert.strictEqual(t.returnHref(0), LANDING_FIXED, 'zero id → query-less landing page');

  // A non-landing return target keeps the legacy placeholder behaviour.
  const legacy = evalReturnHref('http://127.0.0.1:8098/admin/#/drives?connected={storage_id}');
  assert.strictEqual(legacy.returnHref(null), 'http://127.0.0.1:8098/admin/#/drives?connected=',
    'legacy return keeps placeholder substitution');
}

(async () => {
  returnHrefUnit();
  await handoffAcceptedWithPlainTarget();
  await handoffAcceptsLegacyHashedTarget();
  await handoffAcceptsFullTargetSet();
  await handoffRejectsArbitraryTarget();
  await malformedCases();
  console.log('test-connect-handoff: all assertions passed');
})().catch((err) => {
  console.error('test-connect-handoff FAILED:', err && (err.stack || err.message));
  process.exit(1);
});
