#!/usr/bin/env node
/* Regression harness for internal/admin/web/assets/relay.js (the /relay/ page).
 *
 * Runs the REAL relay.js in its ?mock=1 mode inside a minimal tree-building DOM
 * stub, under a deterministic virtual clock (all setTimeout calls are virtual,
 * so polling/retry timing is instant and orderable). Production relay.js code
 * paths are exercised unmodified.
 *
 * Covers the production must-fixes:
 *   T1  happy path: uploading → server receipt completed; new attempts get
 *       NEW immutable upload ids
 *   T2  regression: 200 + FileInfo but NO Status is never "completed" —
 *       result stays 结果未确认 (unconfirmed) with a failed probe
 *   T3  non-2xx WITH a server receipt {UploadId, Status:"failed"} renders the
 *       copyable failed result with that id (not a swallowed error)
 *   T4  non-2xx WITHOUT a receipt renders an honest "rejected" structured
 *       result — never a fabricated completed/failed receipt
 *   T5  401 mid-session returns to the gate AND preserves the friendly error
 *   T6  logout during a non-blocking poll: no stale callback renders into or
 *       mutates the next session's UI; relogin with another key shows only the
 *       new key's file list (per-key metadata never leaks)
 *   T6b logout while the upload request itself is still in flight: same
 *       guarantees
 *   T7  two overlapping doLogin attempts: the newest wins deterministically;
 *       the loser's late probe results never commit or clobber
 *
 * Usage: node scripts/test-relay-page.cjs
 */
'use strict';
const fs = require('fs');
const path = require('path');

const RELAY_JS = path.resolve(__dirname, '..', 'internal', 'admin', 'web', 'assets', 'relay.js');

/* ---------- virtual clock (installed BEFORE relay.js loads) ---------- */
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
/** Advance the virtual clock until cond() or the virtual horizon is reached. */
async function pumpUntil(cond, horizonMs, label) {
  const horizon = vnow + (horizonMs || 30000);
  let idle = 0;
  for (;;) {
    if (cond()) return;
    timers.sort((a, b) => a.at - b.at || a.id - b.id);
    const t = timers.find((x) => x.at <= horizon);
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
/** Sleep virtually: returns once the virtual clock advanced by >= ms. */
function sleepV2(ms) {
  const target = vnow + ms;
  // done when the virtual clock reached the target OR every pending virtual
  // timer already fired (queue drained = nothing stale can run anymore)
  return pumpUntil(() => vnow >= target || timers.length === 0, ms + 5000, 'sleepV(' + ms + ')');
}

/* ---------- tree-building DOM stub ---------- */
function mkEl(tag) {
  const el = {
    nodeType: 1, tagName: tag, className: '', children: [], _listeners: {}, attrs: {},
    style: {}, dataset: {}, disabled: false, checked: false, selected: false,
    hidden: false, open: false, textContent: '', innerHTML: '', files: null,
    get firstChild() { return this.children[0] || null; },
    append(...cs) { for (const c of cs) this.children.push(c); },
    prepend(c) { this.children.unshift(c); },
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) this.children.splice(i, 1); },
    addEventListener(t, fn) { (this._listeners[t] = this._listeners[t] || []).push(fn); },
    removeEventListener() {},
    setAttribute(k, v) {
      this.attrs[k] = v === undefined ? '' : String(v);
      if (k === 'class') this.className = String(v);
      else if (k === 'checked') this.checked = true;
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
    fire(t) { for (const fn of (this._listeners[t] || []).slice()) fn({ target: this, preventDefault() {} }); },
  };
  Object.defineProperty(el, 'textContent', {
    get() { return this._textContent || ''; },
    set(value) { this._textContent = value == null ? '' : String(value); this.children.length = 0; },
  });
  el.classList = { add() {}, remove() {}, toggle() {}, contains() { return false; } };
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
  return el.textContent || ''; // leaf: textContent property (set directly by relay.js)
}
function clearEl(el) { while (el.firstChild) el.removeChild(el.firstChild); }
function findAll(root, pred, out) {
  out = out || [];
  (function walk(el) {
    if (!el || typeof el !== 'object' || el.nodeType !== 1) return;
    try { if (pred(el)) out.push(el); } catch (e) { /* malformed stub node */ }
    for (const c of el.children || []) walk(c);
  }(root));
  return out;
}

/* ---------- browser globals ---------- */
const byId = {};
const E = (id) => { if (!byId[id]) byId[id] = mkEl('div'); return byId[id]; };
const forbiddenStorage = { getItem() { throw new Error('Browser storage accessed'); }, setItem() { throw new Error('Browser storage written'); }, removeItem() { throw new Error('Browser storage changed'); } };
global.localStorage = forbiddenStorage;
global.sessionStorage = forbiddenStorage;
global.location = { search: '?mock=1', href: 'http://lan-host/relay/', origin: 'http://lan-host', pathname: '/relay/' };
Object.defineProperty(global, 'navigator', { value: {}, configurable: true });
global.window = global;
global.document = {
  readyState: 'complete', // boot() runs synchronously at script load
  addEventListener() {}, removeEventListener() {},
  createTextNode(t) { return { nodeType: 3, text: String(t) }; },
  getElementById(id) { if (!byId[id]) byId[id] = mkEl('div'); return byId[id]; },
  querySelector: () => null,
  querySelectorAll: () => [],
  body: mkEl('body'),
  documentElement: { dataset: {} },
  createElement: (tag) => mkEl(tag),
};
Object.defineProperty(global.document, 'cookie', {
  get() { throw new Error('Cookie accessed'); },
  set() { throw new Error('Cookie written'); },
});
global.window.C2A = global.window.C2A || {};

/* no real network may ever happen — fail loudly if relay.js leaves mock mode */
global.fetch = () => { throw new Error('REAL NETWORK FETCH in mock mode'); };
global.XMLHttpRequest = function () { throw new Error('REAL XHR in mock mode'); };

/* ---------- load the REAL relay.js ---------- */
eval(fs.readFileSync(RELAY_JS, 'utf8'));

const mock = global.__RELAY_MOCK;
if (!mock) { console.error('FATAL: __RELAY_MOCK not exposed'); process.exit(1); }

/* ---------- tiny assertions ---------- */
let failed = 0;
function ok(cond, msg) { console.log((cond ? 'PASS ' : 'FAIL ') + msg); if (!cond) failed++; }
function text(el) { return textOf(el); }
function resultCard() { return E('result-body'); }
function resultStatusText() {
  const s = findAll(resultCard(), (el) => String(el.className).includes('result-status'))[0];
  return s ? textOf(s) : '';
}
function resultJson() {
  const pre = findAll(resultCard(), (el) => String(el.className).includes('jsonbox'))[0];
  if (!pre || !pre.children[0]) return null;
  try { return JSON.parse(pre.children[0].text); } catch (e) { return null; }
}
function fakeFile(name, size) { return { name: name || 'report.pdf', size: size == null ? 1024 : size, type: 'application/pdf' }; }
async function doLogin(key) {
  E('key').value = key;
  E('login-form').fire('submit');
  await pumpUntil(() => !E('btn').disabled, 20000, 'login ' + key);
}
async function startUploadBtn() {
  E('file').files = [fakeFile()];
  E('upload-btn').disabled = false;
  E('upload-btn').fire('click');
}
/** Fresh session + clean cards between tests: logout invalidates all
 * generations, and stale result/list content can no longer satisfy pumps. */
async function resetSession() {
  mock.on = {};
  E('logout').fire('click');
  await yieldMicro();
  E('result-card').hidden = true;
  clearEl(E('result-body'));
  clearEl(E('files-host'));
  E('upload-status').textContent = '';
  E('err').textContent = '';
  E('file').files = null;
  E('progress').hidden = true;
  E('me-prefix').textContent = '';
  E('recheck-btn').hidden = true;
}

/* ---------- tests ---------- */
(async () => {
  /* ---- T1: happy path + immutable per-attempt ids ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t1key');
    ok(E('gate').hidden === true && E('app').hidden === false, 'T1 login reveals the relay app');
    ok(text(E('me-prefix')).indexOf('sk-c2a-mockpre1') === 0, 'T1 key prefix rendered (memory-only display)');
    ok(text(E('me-kv')).includes('默认网盘mock-drive') && text(E('me-kv')).includes('中转容量'), 'T1 capability rows render as DOM text, not stringified node arrays');
    ok(text(E('me-kv')).includes('中转根目录/demo'), 'T1 relay root is read from the matching granted drive');

    await startUploadBtn();
    await pumpUntil(() => resultStatusText() !== '', 60000, 'T1 completion');
    ok(resultStatusText() === '已完成（服务器确认）', 'T1 server receipt completed → 已完成（服务器确认）, got: ' + resultStatusText());
    const j1 = resultJson();
    ok(j1 && j1.status === 'completed' && j1.confirmed === true && j1.upload_id, 'T1 copyable result JSON confirmed with upload_id');
    ok(j1.file && j1.file.id && j1.file.crc64, 'T1 file info (id + CRC64) present for the model');

    // a second attempt MUST get a new immutable id
    await startUploadBtn();
    await pumpUntil(() => { const j = resultJson(); return j && j.upload_id && j.upload_id !== j1.upload_id; }, 60000, 'T1 second upload');
    const j2 = resultJson();
    ok(j2.upload_id !== j1.upload_id, 'T1 new attempt → new X-Upload-ID (immutable receipts, got ' + j1.upload_id + ' vs ' + j2.upload_id + ')');
    await pumpUntil(() => text(E('files-host')).indexOf('sample-report.pdf') !== -1, 20000, 'T1 list');
    ok(text(E('files-host')).indexOf('sample-report.pdf') !== -1, 'T1 relay file list shows the uploaded/mock file');
  }

  /* ---- T2: 200 + FileInfo without Status is NEVER completed (fix 1 regression) ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t2key');
    let queryCount = 0;
    mock.on = {
      'POST /api/files/add': async () => ({
        kind: 'response', status: 200,
        json: { Result: 'OK', FileInfo: { Id: 'fx-noStatus', Name: 'ghost.pdf', SizeBytes: 5, CRC64: '1', UrlDownload: '/d?id=fx' }, IncludeFilename: false },
      }),
      'GET /api/files/status/*': async () => { queryCount++; return { status: 404, json: { Message: 'unknown upload id (mock)' } }; },
    };
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() !== '', 60000, 'T2 probe');
    ok(resultStatusText() === '结果未确认', 'T2 missing Status + FileInfo → 结果未确认 (never completed), got: ' + resultStatusText());
    const j = resultJson();
    ok(j && j.status === 'unknown' && j.confirmed === false, 'T2 structured result status=unknown, confirmed=false');
    ok(j && j.retry_hint && j.retry_hint.indexOf('结果未确认') === 0 && j.query.indexOf('GET /api/files/status/') === 0, 'T2 retry hint directs to the status query');
    ok(queryCount > 0, 'T2 missing Status triggers a real receipt probe instead of trusting FileInfo');
  }

  /* ---- T2b: missing Status can recover only through a confirmed receipt ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t2bkey');
    mock.on = {
      'POST /api/files/add': async () => ({ kind: 'response', status: 200, json: { UploadId: 'rcpt-no-status', FileInfo: { Name: 'untrusted-response.pdf' } } }),
      'GET /api/files/status/*': async (id) => ({ status: 200, json: { UploadId: id, Status: 'completed', FileInfo: { Name: 'server-confirmed.pdf' } } }),
    };
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() !== '', 1000, 'T2b recovered receipt');
    const result = resultJson();
    ok(result && result.confirmed && result.upload_id === 'rcpt-no-status', 'T2b an explicit completed receipt can recover a missing response Status');
    ok(result && result.file && result.file.name === 'server-confirmed.pdf', 'T2b recovery uses receipt metadata, not the original unconfirmed FileInfo');
  }

  /* ---- T3: non-2xx WITH a server receipt renders the failed result verbatim ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t3key');
    mock.on = {
      'POST /api/files/add': async () => ({
        kind: 'response', status: 500,
        json: { UploadId: 'rcpt-e1', Status: 'failed', Message: 'upstream boom' },
      }),
    };
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() !== '', 60000, 'T3 receipt');
    ok(resultStatusText() === '失败', 'T3 error receipt Status=failed → 失败, got: ' + resultStatusText());
    const j = resultJson();
    ok(j && j.status === 'failed' && j.upload_id === 'rcpt-e1' && j.confirmed === false, 'T3 copyable failed receipt keeps its immutable UploadId');
    ok(j && j.message.indexOf('upstream boom') !== -1, 'T3 backend failure message surfaced to the model');
  }

  /* ---- T4: non-2xx WITHOUT a receipt → honest rejected result ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t4key');
    mock.on = {
      'POST /api/files/add': async () => ({
        kind: 'response', status: 400,
        json: { Message: 'file exceeds the configured maximum size of 8589934592 bytes' },
      }),
    };
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() !== '', 60000, 'T4 rejected');
    ok(resultStatusText() === '已拒绝（未生成服务端回执）', 'T4 no-receipt error → 已拒绝（未生成服务端回执）, got: ' + resultStatusText());
    const j = resultJson();
    ok(j && j.status === 'rejected' && j.confirmed === false && !j.file, 'T4 structured rejected result, confirmed=false, no fabricated file');
    ok(text(E('upload-status')).indexOf('上传被拒绝') !== -1, 'T4 human status line explains the rejection');
  }

  /* ---- T5: 401 mid-session → gate with preserved friendly error ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t5key');
    mock.on = {
      'POST /api/files/add': async () => ({ kind: 'response', status: 401, json: { Message: 'invalid or missing API key' } }),
    };
    await startUploadBtn();
    await pumpUntil(() => E('gate').hidden === false, 60000, 'T5 gate');
    ok(E('app').hidden === true, 'T5 app hidden after 401');
    ok(text(E('err')).indexOf('密钥无效或已过期') !== -1, 'T5 friendly 401 error preserved in the gate, got: ' + JSON.stringify(text(E('err'))));
  }

  /* ---- T6: logout during a non-blocking poll; relogin as another key ---- */
  {
    await resetSession();
    const meCalls = [];
    const listCalls = [];
    mock.on = {
      'GET /v1/me': async () => {
        meCalls.push(1);
        const n = meCalls.length;
        return {
          status: 200,
          json: {
            key: { id: n === 1 ? 'key-a' : 'key-b', prefix: n === 1 ? 'sk-c2a-aaaa' : 'sk-c2a-bbbb', default_drive: 'd1' },
            drives: [{ id: 'd1' }], global_perms: ['relay'],
            limits: {}, trash: { enabled: true, days: 7 },
            relay: { enabled: true, drive: 'd1', quota_bytes: 1024, max_age_days: 7, used_bytes: 0, files: 0 },
            policy_text: '',
          },
        };
      },
      'GET /api/files/list': async () => {
        listCalls.push(1);
        const n = listCalls.length;
        return new Promise((resolve) => {
          setTimeout(() => resolve({ status: 200, json: [{ Id: n === 1 ? 'a1' : 'b1', Name: n === 1 ? 'a-file.bin' : 'b-file.bin', SizeBytes: 3, Size: '3 B', UrlDownload: '/d?id=x', DownloadsRemaining: 0, UnlimitedDownloads: true, ExpireAtString: '' }] }), n === 1 ? 150 : 0);
        });
      },
      'POST /api/files/add': async (file, fields, uploadId) => ({
        kind: 'response', status: 200, json: { UploadId: uploadId, Status: 'uploading', Message: '' },
      }),
      'GET /api/files/status/*': async (id) => new Promise((resolve) => {
        setTimeout(() => resolve({ status: 200, json: { UploadId: id, Status: 'uploading', Message: '' } }), 60);
      }),
    };
    await doLogin('sk-c2a-aaaa'); // session A
    await pumpUntil(() => text(E('files-host')).indexOf('a-file.bin') !== -1, 20000, 'T6 listA');

    await startUploadBtn();
    await sleepV2(30); // upload responded `uploading`; the 60ms poll tick is pending
    E('logout').fire('click'); // logout while the poll is in flight
    ok(E('gate').hidden === false && E('app').hidden === true, 'T6 logout during poll returns to the gate');
    await doLogin('sk-c2a-bbbb'); // session B
    await pumpUntil(() => text(E('me-prefix')).indexOf('sk-c2a-bbbb') === 0 && text(E('files-host')).indexOf('b-file.bin') !== -1, 20000, 'T6 sessionB');
    await sleepV2(2000); // let every stale A-callback (poll ticks, renders) fire
    ok(E('app').hidden === false, 'T6 stale callbacks did not kick session B back to the gate');
    ok(E('result-card').hidden === true, 'T6 stale A upload/poll never rendered a result into session B');
    ok(text(E('files-host')).indexOf('a-file.bin') === -1, 'T6 session A file metadata never leaks into session B');
    ok(text(E('files-host')).indexOf('b-file.bin') !== -1, 'T6 session B file list intact after all stale callbacks');
  }

  /* ---- T6b: logout while the upload REQUEST is still in flight ---- */
  {
    await resetSession();
    const meCallsB = [];
    mock.on = {
      'GET /v1/me': async () => {
        meCallsB.push(1);
        const n = meCallsB.length;
        return {
          status: 200,
          json: {
            key: { id: n === 1 ? 'key-a' : 'key-b', prefix: n === 1 ? 'sk-c2a-aaaa' : 'sk-c2a-bbbb', default_drive: 'd1' },
            drives: [{ id: 'd1' }], global_perms: ['relay'], limits: {}, trash: { enabled: true, days: 7 },
            relay: { enabled: true, drive: 'd1', quota_bytes: 1024, max_age_days: 7, used_bytes: 0, files: 0 }, policy_text: '',
          },
        };
      },
      'POST /api/files/add': (file, fields, uploadId) => new Promise((resolve) => {
        setTimeout(() => resolve({ kind: 'response', status: 200, json: { UploadId: uploadId, Status: 'uploading', Message: '' } }), 80);
      }),
      'GET /api/files/status/*': async (id) => ({ status: 200, json: { UploadId: id, Status: 'completed', Message: '', FileInfo: { Id: 'ghost', Name: 'ghost.bin', SizeBytes: 1 } } }),
    };
    await doLogin('sk-c2a-aaaa'); // session A again
    await startUploadBtn();
    await sleepV2(30); // request in flight (resolves at +80)
    E('logout').fire('click'); // abort path + generation bump
    await doLogin('sk-c2a-bbbb'); // session B
    ok(text(E('me-prefix')).indexOf('sk-c2a-bbbb') === 0, 'T6b session B committed');
    await sleepV2(2000); // A's request resolves at +80 → guarded continuation
    ok(E('result-card').hidden === true, 'T6b aborted in-flight upload never rendered into session B');
    ok(E('app').hidden === false, 'T6b session B stays logged in');
  }

  /* ---- T7: overlapping doLogin attempts — newest wins ---- */
  {
    await resetSession();
    const meCalls = [];
    mock.on = {
      'GET /v1/me': async () => {
        meCalls.push(1);
        const n = meCalls.length;
        return new Promise((resolve) => {
          setTimeout(() => resolve({
            status: 200,
            json: {
              key: { id: n === 1 ? 'key-slow' : 'key-fast', prefix: n === 1 ? 'sk-c2a-slow' : 'sk-c2a-fast', default_drive: 'd1' },
              drives: [{ id: 'd1' }], global_perms: ['relay'], limits: {}, trash: { enabled: true, days: 7 },
              relay: { enabled: true, drive: 'd1', quota_bytes: 1, max_age_days: 7, used_bytes: 0, files: 0 }, policy_text: '',
            },
          }), n === 1 ? 300 : 10);
        });
      },
    };
    E('gate').hidden = false; E('app').hidden = true; // simulate logged-out start
    E('key').value = 'sk-c2a-slow';
    E('login-form').fire('submit'); // slow attempt (resolves last)
    E('key').value = 'sk-c2a-fast';
    E('login-form').fire('submit'); // fast attempt (resolves first, newest)
    await pumpUntil(() => E('app').hidden === false, 60000, 'T7 login');
    ok(text(E('me-prefix')).indexOf('sk-c2a-fast') === 0, 'T7 newest attempt won the session, got: ' + text(E('me-prefix')));
    await sleepV2(2000); // let the slow probe resolve late
    ok(text(E('me-prefix')).indexOf('sk-c2a-slow') === -1, 'T7 late losing attempt never clobbered the winning session');
    ok(E('app').hidden === false, 'T7 app still visible after the losing attempt settled');
  }

  /* ---- T8: click-driven logout clears the gate and unlocks file selection ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t8key');
    E('file').disabled = true;
    E('logout').fire('click');
    ok(E('err').textContent === '', 'T8 logout click never renders a click Event as gate text');
    ok(E('file').disabled === false, 'T8 logout clears a previous attempt\'s disabled file input');
  }

  /* ---- T9: logout during recheck must not disable the next session\'s button ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t9a');
    mock.on = {
      'POST /api/files/add': async () => ({ kind: 'response', status: 200, json: {} }),
      'GET /api/files/status/*': async () => ({ status: 404, json: {} }),
    };
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() === '结果未确认', 60000, 'T9 unknown');
    E('recheck-btn').fire('click');
    ok(E('recheck-btn').disabled, 'T9 recheck is disabled while its query is in flight');
    E('logout').fire('click');
    ok(E('recheck-btn').disabled === false, 'T9 logout releases the old recheck button immediately');
    await doLogin('sk-c2a-t9b');
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() === '结果未确认', 60000, 'T9 new session');
    ok(!E('recheck-btn').disabled && !E('recheck-btn').hidden, 'T9 the new session has a working recheck button');
  }

  /* ---- T10: rechecking an older result cannot cancel the active upload poll ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t10key');
    mock.on = {
      'POST /api/files/add': async () => ({ kind: 'response', status: 200, json: {} }),
      'GET /api/files/status/*': async () => ({ status: 404, json: {} }),
    };
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() === '结果未确认', 60000, 'T10 old result');
    const pending = [];
    mock.on = {
      'POST /api/files/add': async (file, fields, id) => ({ kind: 'response', status: 200, json: { UploadId: id, Status: 'uploading' } }),
      'GET /api/files/status/*': (id) => new Promise((resolve) => pending.push({ id, resolve })),
    };
    await startUploadBtn();
    await pumpUntil(() => pending.length === 1, 1000, 'T10 active poll');
    ok(E('recheck-btn').hidden && E('recheck-btn').disabled && E('result-card').hidden, 'T10 old results and recheck are hidden during a new attempt');
    E('recheck-btn').fire('click');
    await sleepV2(250);
    ok(pending.length === 1, 'T10 recheck cannot supersede the active attempt\'s poll');
    pending[0].resolve({ status: 200, json: { UploadId: pending[0].id, Status: 'completed' } });
    await pumpUntil(() => resultStatusText() === '已完成（服务器确认）', 1000, 'T10 completion');
    ok(!E('upload-btn').disabled && !E('file').disabled, 'T10 completion releases the upload controls');
  }

  /* ---- T11: receipt polling 401 returns to the gate without retrying ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t11key');
    let calls = 0;
    mock.on = {
      'POST /api/files/add': async (file, fields, id) => ({ kind: 'response', status: 200, json: { UploadId: id, Status: 'uploading' } }),
      'GET /api/files/status/*': async () => { calls++; return { status: 401, json: {} }; },
    };
    await startUploadBtn();
    await pumpUntil(() => E('gate').hidden === false, 1000, 'T11 expired key');
    ok(calls === 1, 'T11 revoked key does not trigger repeated receipt queries');
    ok(E('err').textContent.includes('密钥无效或已过期'), 'T11 polling 401 preserves the authentication explanation');
  }

  /* ---- T12: unexpected error with an upload ID is resolved by its receipt ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t12key');
    mock.on = {
      'POST /api/files/add': async () => ({ kind: 'response', status: 500, json: { UploadId: 'rcpt-query', Status: 'completed', FileInfo: { Name: 'error-envelope.pdf' } } }),
      'GET /api/files/status/*': async (id) => ({ status: 200, json: { UploadId: id, Status: 'completed', FileInfo: { Name: 'query-confirmed.pdf' } } }),
    };
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() !== '', 1000, 'T12 queried receipt');
    const result = resultJson();
    ok(result && result.confirmed && result.upload_id === 'rcpt-query' && result.file.name === 'query-confirmed.pdf', 'T12 an error response with an ID is not mislabeled as having no receipt');
  }

  /* ---- T13: expired key at the final login probe never commits a session ---- */
  {
    await resetSession();
    mock.on = { 'GET /api/info/config': async () => ({ status: 401, json: {} }) };
    await doLogin('sk-c2a-t13key');
    ok(!E('gate').hidden && E('app').hidden, 'T13 final login-probe 401 never enters the relay app');
    ok(E('err').textContent.includes('密钥无效或已过期'), 'T13 final login-probe failure is explained');
  }

  /* ---- T14: 403 with a real failed receipt keeps the backend result ---- */
  {
    await resetSession();
    await doLogin('sk-c2a-t14key');
    mock.on = {
      'POST /api/files/add': async () => ({ kind: 'response', status: 403, json: { UploadId: 'rcpt-forbidden', Status: 'failed', Message: 'upstream refused this write' } }),
    };
    await startUploadBtn();
    await pumpUntil(() => resultStatusText() !== '', 1000, 'T14 forbidden receipt');
    const result = resultJson();
    ok(result && result.status === 'failed' && result.upload_id === 'rcpt-forbidden' && result.message === 'upstream refused this write', 'T14 403 preserves an existing failure receipt instead of fabricating a rejection');
  }

  console.log(failed ? ('\n' + failed + ' FAILED') : '\nALL RELAY PAGE TESTS PASSED');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error('HARNESS ERROR', e); process.exit(1); });
