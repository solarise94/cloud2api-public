#!/usr/bin/env node
/* Regression harness for the file workspace page /files/
 * (internal/admin/web/files.html + internal/admin/web/assets/files.js).
 *
 * Runs the REAL files.js inside a minimal tree-building DOM stub that mirrors
 * files.html, under a deterministic virtual clock (all setTimeout calls are
 * virtual, so every async chain is instant and orderable). Two boot modes:
 *
 *   mock boot       ?mock=1  → files.js answers everything from its internal
 *                              in-memory backend (__FILES_MOCK); real
 *                              fetch/XMLHttpRequest CONSTRUCTING throws, so
 *                              "mock mode is zero network" is enforced, not
 *                              assumed.
 *   production boot no ?mock → fetch/XMLHttpRequest are recording stubs that
 *                              answer from fixtures, so the real production
 *                              transport code paths (raw PUT vs multipart,
 *                              apikey header, credentials omit) are exercised.
 *
 * The clipboard is MODELLED, not rubber-stamped: navigator.clipboard can be
 * present/absent, writeText can resolve/reject/throw synchronously or be held;
 * document.execCommand('copy') copies the value of the control that is actually
 * focused/selected at that instant and only succeeds if that control is reachable
 * (a selection outside an OPEN modal dialog is inert, exactly like Chrome). Tests
 * assert the ACTUAL copied text, never just a success notice.
 *
 * Contract exercised (docs/FILE-WORKSPACE.md + internal/agentapi source):
 *   GET  /v1/me                              → {key, drives[{id,provider,root,
 *                                               perms,health,default}],
 *                                               global_perms, limits, trash}
 *   GET  /v1/drives/{drive}/list             → {items[{name,path,is_dir,size,
 *                                               modified,hashes}],total,page,
 *                                               per_page}   (?path=&page=&per_page=)
 *   POST /v1/drives/{drive}/mkdir            {path}            → {path,created}
 *   POST /v1/drives/{drive}/move             {from,to}         → {from,to}
 *   POST /v1/drives/{drive}/delete           {paths:[…]}       → {results:[
 *                                               {path,status,error?}]} — HTTP 200
 *                                               even for per-item errors
 *   PUT  /v1/drives/{drive}/files            ?path=&on_conflict= → raw body when
 *                                               size>0 (positive Content-Length),
 *                                               multipart `file` part when
 *                                               size===0 → {path,size,crc64,
 *                                               sha256,conflict}
 *   POST /v1/drives/{drive}/download-ticket  {path}            → {url:'/download/
 *                                               <unguessable-token>',
 *                                               expires_at:ISO-string} — same-origin
 *                                               URL handed to a NATIVE browser
 *                                               download (no apikey, no body, no
 *                                               Blob/stream/FileSystem buffering)
 *   POST /v1/links                           {drive,path,expires_in≤604800}
 *                                                              → {url,expires_at}
 * All /v1 responses are RAW JSON (no envelope); errors are
 * {error:{code,message,…}} with the HTTP status. The key rides only in the
 * `apikey` request header, credentials:'omit', never URL/storage/cookie.
 *
 * Mock hook contract (frontend): window.__FILES_MOCK = {map, calls, reset}
 *   map['GET /v1/me'](…) → plain JSON success payload; throw
 *   {mockStatus, mockBody:{error:{code,message}}} for errors. Keys are exact
 *   ('GET /v1/me', 'GET /v1/drives/<id>/list', …) with
 *   '…/drives/{drive}/…' templates as fallback. calls entries record
 *   {method, path, query, has_key, at, body} and must never contain key
 *   material. Override functions may return promises (the dispatcher awaits).
 *
 * Coverage map (F-tags appear on every assertion):
 *   F1  key gate: empty key, invalid key, mock banner, login
 *   F2  drive cards: granted drives only, grant roots, perms, health,
 *       default chip; login landing depends on grant count (F2b/F2c)
 *   F2b exactly ONE granted drive: login enters it directly at the logical
 *       root '/', no landing, but ONLY through the normal list-permission
 *       guard (a no-list single drive still refuses; never a bypass); a list
 *       error is surfaced instead of crashing
 *   F2c multiple drives keep the 我的网盘 selector; zero drives keep the
 *       empty landing state
 *   F3  readonly drive: write controls disabled, upload/mkdir refused with an
 *       explanation, destructive row actions disabled, download stays
 *   F4  permission union: global perms union into per-drive perms (mkdir and
 *       upload work on a list/read drive); delete/share NOT unioned in; global
 *       perms never make an ungranted drive visible; no-grants empty state
 *   F5  broken (needs_reauth) drive: list error surfaced, no crash, no logout
 *   F6  folder navigation + breadcrumb + back-to-landing
 *   F7  pagination (server per_page authoritative) + current-page-only filter
 *       (no refetch while filtering, empty-match explanation)
 *   F8  hostile names rendered verbatim as text; encoded path queries
 *   F9  mkdir dialog (empty name rejected; hostile-but-legal name posted)
 *   F10 rename dialog → POST move {from,to,on_conflict:'fail'}
 *   F11 delete: explicit confirmation required, cancel = no request, per-item
 *       server errors (HTTP 200 {status:'error'}) shown as failures, deleted
 *       row leaves the refreshed listing
 *   F12 share dialog: ttl ≤ 7d, a single short public notice (no verbose
 *       unsupported-capability disclaimer), NO password/download-limit inputs,
 *       javascript:/credential/data URLs rejected, explicit action only; the
 *       copy button hands the ACTUAL share URL to the clipboard (secure API) and
 *       shows 已复制; F12b a stale slow share response never fills a reopened
 *       dialog
 *   F13 upload receipts: only a valid upstream-confirmed
 *       {path,size,crc64,sha256,conflict} for the REQUESTED path renders done;
 *       missing checksums / malformed checksum shapes (40-hex sha256) / wrong
 *       path / wrong size / 5xx / 409 stay not-done;
 *       sequential queue; drop and paste paths; has_key on every PUT
 *   F14 conflict handling: overwrite requires an explicit confirmation,
 *       on_conflict=rename/overwrite reach the request query; the role=group
 *       dropzone opens the picker only via 选择文件 or its plain surface
 *   F15 captured destination: navigating away never re-targets an in-flight
 *       upload; the queue keeps showing the original directory
 *   F16 stale callbacks: late list/share/upload/login results across
 *       logout/relogin never leak a previous session (deferred promises +
 *       real UI events); 401 mid-session returns to the gate
 *   F16e stale session-A upload completion never pumps session B's queue
 *   F17 key hygiene: key only in memory + apikey header; __FILES_MOCK.calls
 *       never contains the key; no URL/storage/cookie/clipboard key writes
 *   F18 URL drive/path-shaped query parameters never bypass the landing or
 *       authorization; hostile-encoded special names stay exercised through
 *       REAL directory navigation (mock override list items)
 *   F19 production transport: credentials omit, apikey header, raw PUT for
 *       size>0 (octet-stream + Idempotency-Key), multipart for size===0,
 *       100% progress ≠ completion, garbage 200 ≠ completion, cancel aborts,
 *       download-ticket contract (F19g) + late/stale 401 (F19f)
 *   F19g native download-ticket: POST body {path} to the exact drive, apikey
 *       header + credentials omit ONLY on the ticket request; the ticket URL is
 *       same-origin (no key) and is handed to a native <a> click (download attr,
 *       noreferrer/noopener, no-referrer); malformed / cross-origin / credential
 *       / non-/download/ URLs and past/absent expiry never trigger a download;
 *       a late ticket response after logout/relogin never starts one; ticket 401
 *       signs out; ticket error is explained and clears the "preparing" notice;
 *       a read-without-link drive can download while link-without-read cannot;
 *       login/upload/list never auto-request a ticket
 *   F20 pagehide: doLogout-grade invalidation — in-flight upload aborted, the
 *       queue scrubbed, and a late response never completing anything
 *   F21 manager defaults OPEN: EVERY drive (writable included) enters with
 *       #file-manager OPEN (files.html ships it visible, aria-expanded=true,
 *       toggle 收起文件管理); the toggle still collapses/expands both ways with
 *       aria-expanded/aria-controls/label in sync; uploads complete while the
 *       user has MANUALLY collapsed it and the finished task exposes a prominent
 *       primary 获取链接 that opens the SAME explicit share dialog (never an auto
 *       POST /v1/links); switching drives re-opens it (no collapse state leaks);
 *       returning to 我的网盘 collapses it and logout/login re-opens it on entry;
 *       a no-link key sees a permission-aware upload hint and no 获取链接
 *   F22 clipboard correctness: the copy button puts the ACTUAL share URL on the
 *       clipboard — secure Clipboard API success/rejection/sync-throw fallbacks;
 *       absent clipboard on insecure LAN HTTP; execCommand true/false/throwing;
 *       selection failure; lazy-copy success only when the write resolves or
 *       execCommand returns true; a rejected/failed copy shows an inline manual
 *       hint and selects #share-url for Ctrl+C; a body-level temp textarea behind
 *       the modal (the old bug) copies nothing (mutation-proof); stale late
 *       clipboard callbacks after dialog close / logout never mutate a new session
 *   WS  persistent workspaces (production boot, cookie sessions):
 *       WS1 single valid entry auto-restores via history.replaceState into
 *       /files/w/<id>/ and /me rides the workspace adapter with
 *       X-C2A-Workspace:1 + credentials same-origin (never apikey/omit);
 *       WS2 multiple entries render a selectable list with 添加 Key and enter
 *       only on an explicit click; WS3 zero entries show the key gate;
 *       WS4 login POSTs {api_key} once to /files/api/login, clears the input
 *       immediately, never persists the key, then /me via the adapter;
 *       WS5 hostile server entry URLs (cross-origin, non-/files/w/, query,
 *       id mismatch) are rejected before any navigation or /me;
 *       WS6 a deep-linked dead entry answers 401 with a re-login bound to THAT
 *       entry (login body carries workspace_id, another key cannot take over);
 *       WS7 stale /me races between entries never cross-render;
 *       WS8 logout POSTs ONLY the current workspace /api/logout then returns to
 *       /files/ (list refetched); WS9 pagehide aborts requests but NEVER calls
 *       logout; WS10 切换工作区 keeps the cookie (no logout) and re-lists;
 *       WS11 malformed workspace entries are dropped; WS12 failed logout keeps
 *       the session and says so; WS13 a single saved entry is only LISTED
 *       (with 添加 Key) after an EXPLICIT switch / logout — auto-restoring one
 *       entry is a boot(/files/)-only behavior — and a second key provisions
 *       its own new entry; WS14 share copy under the PRODUCTION cookie session
 *       (mode-independent authenticated guard): real-transport link POST via
 *       the adapter + secure Clipboard API success, LAN execCommand fallback,
 *       failed-copy manual hint, and stale copy isolation across dialog close
 *       and logout/relogin; WS-mock mock mode never touches /files/api/*
 *       and keeps zero real network with the in-memory key only
 *
 * Usage:
 *   node scripts/test-files-page.cjs             (full suite; exit 2 when
 *                                                 files.js has not landed yet)
 *   node scripts/test-files-page.cjs --selftest  (infrastructure only)
 */
'use strict';
const fs = require('node:fs');
const path = require('node:path');

const WEB_DIR = path.resolve(__dirname, '..', 'internal', 'admin', 'web');
const FILES_JS = path.join(WEB_DIR, 'assets', 'files.js');
const FILES_HTML = path.join(WEB_DIR, 'files.html');
const SELFTEST = process.argv.includes('--selftest');

/* ============================ virtual clock ============================ */
const realSetTimeout = setTimeout;
let vnow = 0;
let timerSeq = 0;
let timers = []; // {id, at, fn}
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
/** Advance the virtual clock until `ms` elapsed or every pending timer fired. */
function sleepV2(ms) {
  const target = vnow + ms;
  return pumpUntil(() => vnow >= target || timers.length === 0, ms + 5000, 'sleepV(' + ms + ')');
}
function dropTimers() { timers = []; }

/* ========================= failure bookkeeping ========================= */
let failed = 0;
let passCount = 0;
function ok(cond, msg) {
  console.log((cond ? 'PASS ' : 'FAIL ') + msg);
  if (cond) passCount++; else failed++;
}
process.on('unhandledRejection', (e) => {
  console.log('FAIL unhandled rejection: ' + ((e && e.stack) || e));
  failed++;
});

/* ======================= fixtures (backend shapes) ===================== */
/* Shapes mirror internal/agentapi handlers exactly (raw /v1 JSON). */
const KEY_MAIN = 'sk-c2a-fxmain0001';
const KEY_RO = 'sk-c2a-fxro000002';
const KEY_UNION = 'sk-c2a-fxunion003';
const KEY_EMPTY = 'sk-c2a-fxempty004';
const KEY_B = 'sk-c2a-fxsessionB';
const KEY_PROD = 'sk-c2a-fxprod0005';
const ALL_KEYS = [KEY_MAIN, KEY_RO, KEY_UNION, KEY_EMPTY, KEY_B, KEY_PROD];

function meDoc() {
  return {
    key: { id: 'k-main', prefix: 'sk-c2a-fxmain…', default_drive: 'fx-alpha' },
    drives: [
      { id: 'fx-alpha', provider: 'openlist', root: '/AlphaRoot', perms: ['list', 'read', 'write', 'delete', 'link'], health: 'healthy', default: true },
      { id: 'fx-readonly', provider: 'openlist', root: '/Vault', perms: ['list', 'read'], health: 'healthy' },
      { id: 'fx-broken', provider: 'openlist', root: '/BrokenRoot', perms: ['list'], health: 'needs_reauth' },
    ],
    global_perms: [],
    limits: { max_upload_bytes: 1048576, requests_per_minute: null, concurrent_jobs: null },
    trash: { enabled: true, days: 7 },
    relay: { enabled: false, drive: '', quota_bytes: 0, max_age_days: 7, used_bytes: 0, files: 0 },
    policy_text: '',
  };
}
function meReadOnly() {
  const doc = meDoc();
  doc.key = { id: 'k-ro', prefix: 'sk-c2a-fxro…', default_drive: 'fx-readonly' };
  doc.drives = [doc.drives[1]];
  return doc;
}
function meUnion() {
  const doc = meDoc();
  doc.key = { id: 'k-union', prefix: 'sk-c2a-fxunion…', default_drive: null };
  doc.drives = [{ id: 'fx-ro-union', provider: 'openlist', root: '/Union', perms: ['list', 'read'], health: 'healthy' }];
  doc.global_perms = ['write'];
  return doc;
}
function meEmpty() {
  const doc = meDoc();
  doc.key = { id: 'k-empty', prefix: 'sk-c2a-fxempty…', default_drive: null };
  doc.drives = [];
  doc.global_perms = ['list', 'read', 'write', 'delete', 'link'];
  return doc;
}
function meB() {
  const doc = meDoc();
  doc.key = { id: 'k-b', prefix: 'sk-c2a-fxsession…', default_drive: null };
  doc.drives = [{ id: 'fx-b-drive', provider: 'openlist', root: '/B-Root', perms: ['list'], health: 'healthy' }];
  return doc;
}
/* One exactly-single-drive grant (drives the direct-entry landing rule). */
function meSingle(id, perms, health) {
  const doc = meDoc();
  doc.key = { id: 'k-one', prefix: 'sk-c2a-fxone…', default_drive: id };
  doc.drives = [{ id: id, provider: 'openlist', root: '/OneRoot', perms: perms || ['list', 'read', 'write'], health: health || 'healthy', default: true }];
  doc.global_perms = [];
  return doc;
}
function entry(name, isDir, size) {
  const p = '/' + name;
  return { name, path: p, is_dir: !!isDir, size: size || 0, modified: '2026-10-14T10:00:00Z', hashes: {} };
}
function listDoc(path, items, total, page, perPage) {
  return { items, total: total == null ? items.length : total, page: page || 1, per_page: perPage || 200 };
}
function uploadReceipt(path, size) {
  return { path, size: size == null ? 3 : size, crc64: '11051210869376104954', sha256: 'ab'.repeat(32) /* 64 hex */, conflict: 'created' };
}
function mkErr(status, code, message) {
  // files.js mock dispatcher only maps thrown values carrying `mock:true`
  return { mock: true, mockStatus: status, mockBody: { error: { code: code || 'error', message: message || 'mock failure', status } } };
}

/* Hostile names: pure text, must never become markup or break requests. */
const XSS_NAME = '<img src=x onerror="globalThis.__FXPWNED=1">';
const QUERY_HOSTILE_NAME = 'a&b=c#d?e f%20g+h.txt';
const TRAV_NAME = '..%2F..%2Fetc%20passwd.txt';
const UNI_NAME = '文件 🌍（下載）.txt';

/* ================= workspace adapter (production stub) =================
 * Persistent-workspace contract (docs/DESIGN-persistent-workspaces.md §2):
 * /files/w/<id>/api/v1/<suffix> answers exactly like /v1/<suffix>; the /files
 * page API (login / workspaces / per-entry logout) is answered from w.prodDocs
 * so frontend flows can be driven without any real server. */
const FUTURE_LOGIN_ISO = new Date(Date.now() + 30 * 86400e3).toISOString();
const WS_ENTRY = (id, days) => ({
  id,
  url: '/files/w/' + id + '/',
  expires_at: new Date(Date.now() + (days == null ? 30 : days) * 86400e3).toISOString(),
});
function stripWsPath(p) {
  const m = /^\/files\/w\/[^/]+\/api(\/v1\/.*)$/.exec(String(p));
  return m ? m[1] : String(p);
}
function routeError(status, code, message) {
  const e = new Error(message);
  e.__status = status;
  e.__body = { error: { code, message, status } };
  return e;
}
/* Handle the /files page API (login / workspaces / per-entry logout).
 * Returns undefined when the path is not one of these. */
async function filesApiRoute(w, p, init) {
  if (p === '/files/api/workspaces') {
    return JSON.parse(JSON.stringify(w.prodDocs.workspaces || { workspaces: [] }));
  }
  if (p === '/files/api/login') {
    let body = {};
    try { body = JSON.parse(String((init && init.body) || '{}')); } catch (e) { body = {}; }
    w.loginCalls.push(JSON.parse(JSON.stringify(body || {})));
    if (w.prodDocs.loginFail) {
      throw routeError(w.prodDocs.loginFail.status || 401, w.prodDocs.loginFail.code || 'unauthorized',
        w.prodDocs.loginFail.message || 'login refused');
    }
    if (!body || typeof body.api_key !== 'string' || !body.api_key) {
      throw routeError(400, 'invalid_request', 'field `api_key` is required');
    }
    if (w.prodDocs.loginKeys && body.api_key !== w.prodDocs.loginKeys) {
      throw routeError(401, 'unauthorized', 'invalid api key');
    }
    if (w.prodDocs.loginDoc) return JSON.parse(JSON.stringify(w.prodDocs.loginDoc));
    const id = w.prodDocs.wsId || 'w-prodmain01';
    return { workspace_id: id, url: '/files/w/' + id + '/', expires_at: FUTURE_LOGIN_ISO };
  }
  const lo = /^\/files\/w\/([^/]+)\/api\/logout$/.exec(p);
  if (lo && (init.method || '') === 'POST') {
    w.prodDocs.logoutCount = (w.prodDocs.logoutCount || 0) + 1;
    if (w.prodDocs.logoutFail === 'network') throw new Error('harness: logout network down');
    if (w.prodDocs.logoutFail) {
      throw routeError(w.prodDocs.logoutFail.status || 500, 'logout_failed', w.prodDocs.logoutFail.message || 'logout failed');
    }
    return { ok: true };
  }
  return undefined;
}
/* Workspace adapter session check: dead entries answer 401 (fail closed). */
function assertWsAlive(w, urlPathname) {
  const m = /^\/files\/w\/([^/]+)\/api\//.exec(String(urlPathname));
  if (!m) return;
  const dead = w.prodDocs.ws401 === true ||
    (Array.isArray(w.prodDocs.ws401For) && w.prodDocs.ws401For.indexOf(m[1]) >= 0);
  if (dead) throw routeError(401, 'unauthorized', 'workspace session is invalid or expired');
}

/* ======================= session (world) machinery ===================== */
/* Elements pre-created to mirror files.html exactly (id, tag, initial
 * state, parent). files.js must work against THIS structure. */
const HTML_TREE = [
  ['mock-banner', 'div', { hidden: true }, null],
  ['ws-list', 'section', { hidden: true }, null],
  ['ws-sub', 'p', { text: '正在恢复已保存的工作区…' }, 'ws-list'],
  ['ws-grid', 'div', {}, 'ws-list'],
  ['ws-add-key', 'button', { type: 'button', text: '添加 API Key' }, 'ws-list'],
  ['gate', 'section', {}, null],
  ['gate-hint', 'p', { class: 'fx-hint', text: '输入访问密钥，查看已授权的网盘，上传和分享文件。' }, 'gate'],
  ['login-form', 'form', {}, 'gate'],
  ['key', 'input', { type: 'password' }, 'login-form'],
  ['key-toggle', 'button', { type: 'button' }, 'login-form'],
  ['gate-err', 'p', {}, 'login-form'],
  ['gate-go', 'button', { type: 'submit' }, 'login-form'],
  ['workspace', 'div', { hidden: true }, null],
  ['me-prefix', 'span', {}, 'workspace'],
  ['theme-toggle', 'button', { type: 'button' }, 'workspace'],
  ['switch-ws', 'button', { type: 'button', text: '切换工作区' }, 'workspace'],
  ['logout', 'button', { type: 'button' }, 'workspace'],
  ['notice', 'p', { hidden: true }, 'workspace'],
  ['drives-home', 'section', {}, 'workspace'],
  ['drives-sub', 'p', {}, 'drives-home'],
  ['drive-grid', 'div', {}, 'drives-home'],
  ['browser-view', 'section', { hidden: true }, 'workspace'],
  /* breadcrumb + back control + manager toggle sit OUTSIDE the #file-manager
   * container, which holds only search/refresh/mkdir/table/pager. The manager
   * now DEFAULTS OPEN for every drive (writable included): files.html ships it
   * visible with aria-expanded=true and the toggle starts on 收起文件管理.
   * files.js must honor that initial state; the user can still collapse it. */
  ['back-drives', 'button', { type: 'button' }, 'browser-view'],
  ['manager-toggle', 'button', { type: 'button', 'aria-controls': 'file-manager', 'aria-expanded': 'true', text: '收起文件管理' }, 'browser-view'],
  ['breadcrumb', 'nav', {}, 'browser-view'],
  ['dropzone', 'div', { role: 'group' }, 'browser-view'],
  ['dz-title', 'h3', {}, 'dropzone'],
  ['dz-text', 'div', {}, 'dropzone'],
  ['choose-files', 'button', { type: 'button', text: '选择文件' }, 'dropzone'],
  ['conflict-mode', 'select', { options: [['fail', '跳过（报错，不覆盖）'], ['rename', '自动重命名（name (1).ext）'], ['overwrite', '覆盖']] }, 'dropzone'],
  ['overwrite-row', 'label', { hidden: true }, 'dropzone'],
  ['overwrite-ok', 'input', { type: 'checkbox' }, 'overwrite-row'],
  ['upload-hint', 'p', { hidden: true }, 'dropzone'],
  ['file-input', 'input', { type: 'file' }, 'browser-view'],
  ['upload-panel', 'div', { hidden: true }, 'browser-view'],
  ['upload-status', 'span', {}, 'upload-panel'],
  ['upload-queue', 'div', {}, 'upload-panel'],
  ['file-manager', 'section', {}, 'browser-view'],
  ['page-search', 'input', { type: 'search' }, 'file-manager'],
  ['refresh-btn', 'button', { type: 'button' }, 'file-manager'],
  ['mkdir-btn', 'button', { type: 'button' }, 'file-manager'],
  ['file-list', 'table', {}, 'file-manager'],
  ['file-tbody', 'tbody', {}, 'file-list'],
  ['page-prev', 'button', { type: 'button' }, 'file-manager'],
  ['page-info', 'span', {}, 'file-manager'],
  ['page-next', 'button', { type: 'button' }, 'file-manager'],
  ['dlg-mkdir', 'dialog', {}, null],
  ['mkdir-cancel', 'button', { type: 'button', 'data-close': '', text: '取消' }, 'dlg-mkdir'],
  ['mkdir-form', 'form', {}, 'dlg-mkdir'],
  ['mkdir-where', 'p', {}, 'mkdir-form'],
  ['mkdir-name', 'input', { type: 'text' }, 'mkdir-form'],
  ['mkdir-err', 'p', {}, 'mkdir-form'],
  ['mkdir-ok', 'button', { type: 'submit' }, 'mkdir-form'],
  ['dlg-rename', 'dialog', {}, null],
  ['rename-cancel', 'button', { type: 'button', 'data-close': '', text: '取消' }, 'dlg-rename'],
  ['rename-form', 'form', {}, 'dlg-rename'],
  ['rename-old', 'p', {}, 'rename-form'],
  ['rename-name', 'input', { type: 'text' }, 'rename-form'],
  ['rename-err', 'p', {}, 'rename-form'],
  ['rename-ok', 'button', { type: 'submit' }, 'rename-form'],
  ['dlg-delete', 'dialog', {}, null],
  ['delete-cancel', 'button', { type: 'button', 'data-close': '', text: '取消' }, 'dlg-delete'],
  ['delete-form', 'form', {}, 'dlg-delete'],
  ['delete-path', 'p', {}, 'delete-form'],
  ['delete-note', 'p', {}, 'delete-form'],
  ['delete-err', 'p', {}, 'delete-form'],
  ['delete-ok', 'button', { type: 'submit' }, 'delete-form'],
  ['dlg-share', 'dialog', {}, null],
  ['share-cancel', 'button', { type: 'button', 'data-close': '', text: '关闭' }, 'dlg-share'],
  ['share-form', 'form', {}, 'dlg-share'],
  ['share-what', 'p', {}, 'share-form'],
  /* single short public-access hint (no duplicate post-result warning block) */
  ['share-note', 'p', { text: '持有链接的人可下载，有效期内无需登录。' }, 'share-form'],
  ['share-ttl-field', 'div', {}, 'share-form'],
  ['share-ttl', 'select', { options: [['3600', '1 小时'], ['86400', '1 天'], ['259200', '3 天'], ['604800', '7 天（上限）']] }, 'share-form'],
  ['share-err', 'p', {}, 'share-form'],
  ['share-result', 'div', { hidden: true }, 'share-form'],
  ['share-result-label', 'label', { text: '链接' }, 'share-result'],
  ['share-url', 'input', { type: 'text', readonly: true }, 'share-form'],
  ['share-copy', 'button', { type: 'button', text: '复制' }, 'share-form'],
  ['share-exp', 'p', {}, 'share-form'],
  ['share-ok', 'button', { type: 'submit' }, 'share-form'],
];
const HTML_IDS = HTML_TREE.map((r) => r[0]);

let worldCounter = 0;
let CUR = null; // most recently created world: module-level drivers act on it
function createWorld(opts) {
  opts = opts || {};
  worldCounter++;
  const production = !!opts.production;
  const search = opts.search == null ? (production ? '' : '?mock=1') : opts.search;
  /* Clipboard/insecure-context model. Defaults: a SECURE context whose async
   * Clipboard API resolves successfully — the ideal path. A world can instead
   * model no clipboard at all (opts.noClipboard), an insecure context
   * (opts.insecure, so window.isSecureContext === false), a writeText that
   * resolves/rejects/throws synchronously, an execCommand that returns true or
   * false, or a selection that fails. The harness records what was ACTUALLY
   * copied (the selected/focused control's value at the moment execCommand ran
   * or writeText resolved), not merely that a success notice appeared. */
  const clip = Object.assign({
    present: true,        // navigator.clipboard exists
    secure: true,         // window.isSecureContext
    write: 'resolve',     // 'resolve' | 'reject' | 'throw'
    exec: true,           // document.execCommand('copy') return value
    selectFails: false,   // field.select()/setSelectionRange() throw
    inertOutside: true,   // a modal dialog makes body-level controls inert
  }, opts.clipboard || {});
  if (opts.noClipboard) { clip.present = false; clip.write = 'absent'; }
  if (opts.insecure) clip.secure = false;
  const w = {
    name: 'world' + worldCounter,
    byId: {},
    ghostIds: new Set(),
    innerHTMLLog: [],
    storageLog: [],
    cookieLog: [],
    urlLog: [],
    storageMap: {},
    confirmLog: [],
    promptLog: [],
    promptQueue: [],
    clipboardLog: [],
    copiedLog: [],    // {text, via:'execCommand'|'writeText', focused, inDialog}
    focusLog: [],     // focus()/select() calls on fields
    execLog: [],      // every document.execCommand invocation
    clipCfg: clip,
    clipHold: null,   // {promise, resolve, reject} for a held async writeText
    fetchLog: [],
    xhrLog: [],
    anchorLog: [],    // native <a download> clicks (same-origin ticket downloads)
    objectUrlLog: [], // URL.createObjectURL calls (must be ZERO for ticket downloads)
    listenersLost: [],
    prodDocs: {
      listByPath: {}, uploads: [],
      /* persistent-workspace contract (docs/DESIGN-persistent-workspaces.md §2) */
      workspaces: { workspaces: [] }, // GET /files/api/workspaces payload
      loginDoc: null,    // full override of the login response (hostile shapes)
      loginFail: null,   // {status, code, message} to refuse a login
      loginKeys: null,   // when set, only this api_key is accepted
      wsId: 'w-prodmain01',
      ws401: false,      // every workspace api/v1 answer 401
      ws401For: null,    // array of entry ids that answer 401
      logoutFail: null,  // 'network' | {status, message}
      logoutCount: 0,
    },
    loginCalls: [],   // bodies handed to POST /files/api/login
    upReceipts: [],   // FIFO of receipts (doc, deferred or {throw}) for PUT
    expectedPath: null, // fallback request slot when calls are not peekable
  };

  /* ---------- tree-building DOM stub ---------- */
  function mkEl(tag) {
    const el = {
      nodeType: 1, tagName: String(tag).toLowerCase(), children: [], _listeners: {}, attrs: {},
      style: {}, dataset: {}, disabled: false, checked: false, selected: false,
      hidden: false, open: false, files: null, parentNode: null,
      _textContent: '', _innerHTML: undefined, _value: undefined,
      get firstChild() { return this.children[0] || null; },
      get lastChild() { return this.children[this.children.length - 1] || null; },
      get childNodes() { return this.children; },
      get isConnected() {
        let n = this;
        while (n && n.parentNode) n = n.parentNode;
        return !!(n && n._root);
      },
      get id() { return this.attrs.id || ''; },
      set id(v) { this.attrs.id = v === undefined ? '' : String(v); if (this.attrs.id) w.byId[this.attrs.id] = el; },
      append(...cs) { for (const c of cs) appendChild(el, c); },
      prepend(c) { c = asNode(c); if (!c) return; c.parentNode = this; this.children.unshift(c); },
      appendChild(c) { c = asNode(c); if (!c) return c; c.parentNode = this; this.children.push(c); return c; },
      insertBefore(c, ref) {
        c = asNode(c); if (!c) return c;
        c.parentNode = this;
        const i = ref ? this.children.indexOf(ref) : -1;
        if (i >= 0) this.children.splice(i, 0, c); else this.children.push(c);
        return c;
      },
      replaceChild(nu, old) {
        const i = this.children.indexOf(old);
        if (i >= 0) { this.children.splice(i, 1, asNode(nu)); nu.parentNode = this; old.parentNode = null; }
        return old;
      },
      replaceChildren(...cs) { this.children.length = 0; for (const c of cs) appendChild(el, c); },
      removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) this.children.splice(i, 1); c.parentNode = null; return c; },
      remove() { if (this.parentNode) this.parentNode.removeChild(this); },
      hasChildNodes() { return this.children.length > 0; },
      contains(n) { for (let x = n; x; x = x.parentNode) if (x === this) return true; return false; },
      addEventListener(t, fn) { (this._listeners[t] = this._listeners[t] || []).push(fn); },
      removeEventListener(t, fn) { const l = this._listeners[t] || []; const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1); },
      setAttribute(k, v) {
        k = String(k);
        this.attrs[k] = v === undefined ? '' : String(v);
        if (k === 'class') this.className = this.attrs[k];
        else if (k === 'id') this.id = this.attrs[k];
        // mirror the attribute→property reflection real browsers perform
        else if (k === 'hidden') this.hidden = true;
        else if (k === 'disabled') this.disabled = true;
        else if (k === 'checked') this.checked = true;
        else if (k === 'selected') this.selected = true;
        else if (k === 'value') this.value = String(v);
        else if (k === 'open') this.open = true;
        else if (k.indexOf('data-') === 0) {
          const key = k.slice(5).replace(/-([a-z])/g, (m, c) => c.toUpperCase());
          this.dataset[key] = this.attrs[k];
        }
      },
      getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
      hasAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k); },
      removeAttribute(k) {
        delete this.attrs[k];
        if (k === 'hidden') this.hidden = false;
        else if (k === 'disabled') this.disabled = false;
        else if (k === 'checked') this.checked = false;
        else if (k === 'open') this.open = false;
      },
      matches(sel) { return matchSelector(el, sel); },
      closest(sel) { for (let n = this; n; n = n.parentNode) { if (n.nodeType === 1 && matchSelector(n, sel)) return n; } return null; },
      querySelector(sel) { return queryAll(el, sel)[0] || null; },
      querySelectorAll(sel) { return queryAll(el, sel); },
      getBoundingClientRect() { return { height: 120, width: 320, top: 0, bottom: 120, left: 0, right: 320 }; },
      focus() { w.doc.activeElement = el; w.focusLog.push({ id: el.id, op: 'focus' }); },
      blur() { if (w.doc.activeElement === el) w.doc.activeElement = null; },
      select() {
        if (el._selectFail) throw new Error('harness: select() unavailable');
        w.focusLog.push({ id: el.id, op: 'select' });
        if (el.isTextlike()) w.doc.selectedField = el;
      },
      setSelectionRange(a, b) {
        if (el._selectFail) throw new Error('harness: setSelectionRange() unavailable');
        w.focusLog.push({ id: el.id, op: 'setSelectionRange', from: a, to: b });
        if (el.isTextlike()) w.doc.selectedField = el;
      },
      scrollIntoView() {}, click() { fire(el, 'click'); },
      isTextlike() { return el.tagName === 'input' || el.tagName === 'textarea'; },
      /* native <dialog> semantics */
      showModal() { el.open = true; el.attrs.open = ''; },
      show() { el.open = true; el.attrs.open = ''; },
      close() { if (!el.open) return; el.open = false; delete el.attrs.open; fire(el, 'close'); },
    };
    Object.defineProperty(el, 'textContent', {
      get() { return this._textContent || ''; },
      set(value) { this._textContent = value == null ? '' : String(value); this.children.length = 0; },
    });
    Object.defineProperty(el, 'type', {
      get() { return this.attrs.type == null ? '' : String(this.attrs.type); },
      set(v) { this.attrs.type = String(v); },
      configurable: true,
    });
    /* a field can be told to fail selection (models browsers that refuse
     * select()/setSelectionRange() on the readonly link input) */
    Object.defineProperty(el, '_selectFail', { value: opts.selectFail === true, writable: true, configurable: true });
    /* boolean reflected properties stay in sync with their attributes both
     * ways, exactly like real browsers (el.hidden = false removes the attr) */
    for (const prop of ['hidden', 'disabled', 'checked', 'open']) {
      Object.defineProperty(el, prop, {
        get() { return this['_' + prop] === true; },
        set(v) {
          this['_' + prop] = v === true;
          if (v === true) this.attrs[prop] = '';
          else delete this.attrs[prop];
        },
        configurable: true,
      });
    }
    Object.defineProperty(el, 'innerHTML', {
      get() { return this._innerHTML || ''; },
      set(value) { w.innerHTMLLog.push({ tag: this.tagName, value: String(value) }); this._innerHTML = String(value); this.children.length = 0; },
    });
    /* real classList backed by the className string */
    const classOf = () => String(el.className || '').split(/\s+/).filter(Boolean);
    el.classList = {
      add(...cs) { const s = new Set(classOf()); for (const c of cs) s.add(c); el.className = Array.from(s).join(' '); },
      remove(...cs) { const rm = new Set(cs); el.className = classOf().filter((c) => !rm.has(c)).join(' '); },
      toggle(c, force) { const has = classOf().includes(c); const want = force === undefined ? !has : !!force; if (want && !has) el.classList.add(c); if (!want && has) el.classList.remove(c); return want; },
      contains(c) { return classOf().includes(c); },
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
  function asNode(c) {
    if (c === null || c === undefined || c === false || c === true) return null;
    if (typeof c === 'string') return w.doc.createTextNode(c);
    if (c.nodeType) return c;
    return null;
  }
  function appendChild(el, c) {
    c = asNode(c);
    if (!c) return;
    c.parentNode = el;
    el.children.push(c);
  }
  function textOf(el) {
    if (el.children && el.children.length) {
      let out = '';
      for (const c of el.children) {
        if (typeof c === 'string') out += c;
        else if (c.nodeType === 3) out += (c.nodeValue !== undefined ? c.nodeValue : c.text);
        else if (c.nodeType === 1 && !['input', 'select', 'textarea', 'progress'].includes(c.tagName)) out += textOf(c);
        else if (c.nodeType === 1 && c.tagName === 'textarea') out += c.value || '';
      }
      return out;
    }
    return el.textContent || ''; // leaf: direct textContent (browsers report the parsed text)
  }
  function findAll(root, pred, out) {
    out = out || [];
    (function walk(el) {
      if (!el || typeof el !== 'object' || (el.nodeType !== 1 && el.nodeType !== 9)) return;
      try { if (el.nodeType === 1 && pred(el)) out.push(el); } catch (e) { /* malformed stub node */ }
      for (const c of el.children || []) walk(c);
    }(root));
    return out;
  }
  function matchSelector(el, sel) {
    sel = String(sel).trim();
    if (!sel) return false;
    for (const part of sel.split(',')) {
      const compound = part.trim().split(/(?=[.#])/);
      let hit = true;
      for (const tok of compound) {
        if (!tok) continue;
        if (tok[0] === '#') hit = hit && el.id === tok.slice(1);
        else if (tok[0] === '.') hit = hit && String(el.className || '').split(/\s+/).includes(tok.slice(1));
        else if (tok[0] === '[') {
          const m = /^\[([^=\]]+)(?:="([^"]*)")?\]$/.exec(tok);
          if (!m) { hit = false; break; }
          const v = el.getAttribute(m[1]);
          hit = hit && (m[2] === undefined ? v !== null : v === m[2]);
        } else hit = hit && el.tagName === tok.toLowerCase();
      }
      if (hit) return true;
    }
    return false;
  }
  function queryAll(root, sel) {
    const parts = String(sel).trim().split(/\s+/);
    let current = [root];
    for (const part of parts) {
      const next = [];
      for (const base of current) {
        for (const el of findAll(base, (e) => matchSelector(e, part))) next.push(el);
      }
      current = next;
    }
    const seen = new Set();
    return current.filter((e) => !seen.has(e) && seen.add(e));
  }
  function fire(el, type, props) {
    if (type === 'click' && el.disabled) return; // disabled controls never dispatch
    const ev = Object.assign({ target: el, type, preventDefault() {}, stopPropagation() {}, stopImmediatePropagation() {} }, props || {});
    // bubbles: ancestor listeners observe the same event with the original target
    for (let node = el; node && !ev.__stopped; node = node.parentNode) {
      if (node.nodeType !== 1 && node.nodeType !== 9) continue;
      for (const fn of ((node._listeners && node._listeners[type]) || []).slice()) fn(ev);
      const own = node['on' + type];
      if (typeof own === 'function') own(ev);
      if (node === el && type === 'click' && (el.tagName === 'button' || el.tagName === 'input') && String(el.attrs.type) === 'submit') {
        // browser semantics: clicking a submit button submits its ancestor form
        for (let n = el.parentNode; n; n = n.parentNode) {
          if (n.nodeType === 1 && n.tagName === 'form') { fire(n, 'submit'); break; }
        }
      }
      if (type !== 'click') break; // only clicks bubble in this harness
    }
  }
  function visible(el) {
    for (let n = el; n; n = n.parentNode) if (n.hidden === true) return false;
    return true;
  }

  /* ---------- pre-create files.html ---------- */
  const body = mkEl('body');
  body._root = true;
  for (const [id, tag, spec, parent] of HTML_TREE) {
    const el = mkEl(tag);
    el.id = id;
    for (const [k, v] of Object.entries(spec)) {
      if (k === 'hidden') el.hidden = true;
      else if (k === 'options') {
        for (const [ov, label] of v) {
          const o = mkEl('option');
          o.attrs.value = ov;
          o.selected = false;
          appendChild(o, { nodeType: 3, text: label, nodeValue: label });
          el.appendChild(o);
        }
      } else if (k === 'text') {
        appendChild(el, { nodeType: 3, text: String(v), nodeValue: String(v) });
      } else {
        el.setAttribute(k, v); // generic attribute + property reflection
        if (k === 'type' || k === 'title' || k === 'value') el[k] = v;
      }    }
    w.byId[id] = el;
    (parent ? w.byId[parent] : body).appendChild(el);
  }

  /* ---------- browser globals ---------- */
  const winListeners = {};
  const docListeners = [];
  const bootPathname = opts.pathname || '/files/';
  const locationObj = {
    origin: 'http://127.0.0.1:18893', pathname: bootPathname,
    search, href: 'http://127.0.0.1:18893' + bootPathname + search,
    assign(u) { w.urlLog.push({ kind: 'assign', url: String(u) }); },
    replace(u) { w.urlLog.push({ kind: 'replace', url: String(u) }); },
  };
  Object.defineProperty(locationObj, 'hash', { get() { return this._hash || ''; }, set(v) { this._hash = String(v); } });
  const historyObj = {
    pushState(...a) { w.urlLog.push({ kind: 'pushState', url: String(a[2]) }); },
    replaceState(...a) { w.urlLog.push({ kind: 'replaceState', url: String(a[2]) }); },
    back() {}, forward() {}, go() {},
    state: null,
    length: 1,
  };
  const storageStub = {
    getItem(k) { w.storageLog.push({ op: 'get', k: String(k) }); return Object.prototype.hasOwnProperty.call(w.storageMap, k) ? w.storageMap[k] : null; },
    setItem(k, v) { w.storageLog.push({ op: 'set', k: String(k), v: String(v) }); w.storageMap[k] = String(v); },
    removeItem(k) { w.storageLog.push({ op: 'remove', k: String(k) }); delete w.storageMap[k]; },
    clear() { w.storageLog.push({ op: 'clear' }); w.storageMap = {}; },
    key(i) { return Object.keys(w.storageMap)[i] ?? null; },
    get length() { return Object.keys(w.storageMap).length; },
  };
  /* The clipboard is modelled, never a rubber stamp. `writeText` records what
   * it was handed; `execCommand('copy')` copies the value of the control that is
   * ACTUALLY focused/selected RIGHT NOW and only succeeds when that control is
   * reachable — a selection living OUTSIDE an open modal dialog is inert, so a
   * body-level temp-textarea copy fails here exactly as it does in Chrome. */
  function openModalDialog() {
    return ['dlg-mkdir', 'dlg-rename', 'dlg-delete', 'dlg-share']
      .map((id) => w.byId[id]).find((d) => d && d.open === true) || null;
  }
  function currentSelection() {
    const f = w.doc.selectedField;
    if (!f) return { text: '', inDialog: false, activeId: '' };
    const modal = openModalDialog();
    const blocked = !!(modal && clip.inertOutside && !modal.contains(f));
    if (blocked) return { text: '', inDialog: false, activeId: f.id };
    return { text: f.value == null ? '' : String(f.value), inDialog: !!(modal && modal.contains(f)), activeId: f.id };
  }
  const clipboardStub = clip.present ? {
    writeText: (t) => {
      w.clipboardLog.push(String(t));
      if (clip.write === 'throw') throw new Error('harness: clipboard.writeText threw synchronously');
      if (clip.write === 'reject') return Promise.reject(new Error('harness: clipboard write rejected'));
      if (w.clipHold) {
        return w.clipHold.promise.then(() => { w.copiedLog.push({ text: String(t), via: 'writeText', resolvedAt: 'hold' }); });
      }
      w.copiedLog.push({ text: String(t), via: 'writeText', resolvedAt: 'immediate' });
      return Promise.resolve();
    },
    readText: async () => '',
  } : undefined;
  const navigatorObj = {
    clipboard: clipboardStub,
    userAgent: 'fx-harness',
    onLine: true,
  };
  w.doc = {
    _root: true,
    readyState: 'complete', // defer script: DOM parsed; boot() runs at load
    addEventListener(t, fn) { docListeners.push([t, fn]); },
    removeEventListener() {},
    createTextNode(t) { return { nodeType: 3, text: String(t), nodeValue: String(t) }; },
    createDocumentFragment() { const f = mkEl('fragment'); f.nodeType = 11; return f; },
    getElementById(id) {
      if (w.byId[id]) return w.byId[id];
      w.ghostIds.add(String(id)); // files.js referenced an id files.html does not have
      const el = mkEl('div');
      w.byId[id] = el;
      return el;
    },
    querySelector(sel) { return queryAll(body, sel)[0] || null; },
    querySelectorAll(sel) { return queryAll(body, sel); },
    createElement: (tag) => {
      const el = mkEl(tag);
      /* Native ticket downloads are handed to an <a download> click. Record the
       * element exactly as the browser would see it (href/download/rel/…) plus
       * whether it was attached to the document at click time. */
      if (String(tag).toLowerCase() === 'a') {
        const origClick = el.click.bind(el);
        el.click = () => {
          w.anchorLog.push({
            href: String(el.href != null ? el.href : (el.getAttribute('href') || '')),
            download: String(el.download != null ? el.download : (el.getAttribute('download') || '')),
            rel: String(el.rel != null ? el.rel : (el.getAttribute('rel') || '')),
            referrerPolicy: String(el.referrerPolicy != null ? el.referrerPolicy : (el.getAttribute('referrerpolicy') || '')),
            inDocument: el.isConnected,
          });
          el.clicked = true;
          origClick();
        };
      }
      return el;
    },
    execCommand(cmd) {
      w.execLog.push(String(cmd));
      if (String(cmd) !== 'copy') return false;
      if (clip.exec === 'throw') throw new Error('harness: execCommand(copy) threw');
      if (clip.exec !== true) return false;
      const sel = currentSelection();
      if (!sel.text) return false; // nothing selected (or the selection is inert)
      w.copiedLog.push({ text: sel.text, via: 'execCommand', inDialog: sel.inDialog, activeId: sel.activeId });
      w.clipboardLog.push(sel.text);
      return true;
    },
    body,
    documentElement: mkEl('html'),
    head: mkEl('head'),
    title: '',
    activeElement: null,
    selectedField: null,
  };
  Object.defineProperty(w.doc, 'cookie', {
    get() { w.cookieLog.push({ op: 'get' }); return ''; },
    set(v) { w.cookieLog.push({ op: 'set', v: String(v) }); },
  });

  global.window = global;
  global.document = w.doc;
  global.location = locationObj;
  global.history = historyObj;
  global.localStorage = storageStub;
  global.sessionStorage = storageStub;
  try { global.navigator = navigatorObj; } catch (e) {
    Object.defineProperty(global, 'navigator', { value: navigatorObj, configurable: true });
  }
  /* window.isSecureContext drives the secure-Clipboard-API branch; default true
   * (the ideal path), switched off per world to exercise the HTTP/LAN case. */
  try { Object.defineProperty(global, 'isSecureContext', { value: clip.secure, writable: true, configurable: true }); }
  catch (e) { global.isSecureContext = clip.secure; }
  global.addEventListener = (t, fn) => { (winListeners[t] = winListeners[t] || []).push(fn); };
  global.removeEventListener = (t, fn) => { const l = winListeners[t] || []; const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1); };
  global.matchMedia = () => ({ matches: true, addEventListener() {}, removeEventListener() {} });
  if (typeof global.crypto === 'undefined' || !global.crypto.getRandomValues) {
    try { Object.defineProperty(global, 'crypto', { value: require('node:crypto').webcrypto, configurable: true }); } catch (e) { /* very old Node */ }
  }
  /* native dialogs must never be used for the flows that have real dialogs */
  global.confirm = (msg) => { w.confirmLog.push(String(msg)); return false; };
  global.prompt = (msg) => { w.promptLog.push(String(msg)); return w.promptQueue.shift() ?? null; };
  global.alert = (msg) => { w.confirmLog.push('alert:' + String(msg)); };
  /* object-URL + execCommand stand-ins for the copy fallback. Ticket downloads
   * must NOT create object URLs (no Blob/stream buffering); the log stays empty
   * and that emptiness is asserted by F19g. */
  let blobSeq = 0;
  URL.createObjectURL = (b) => { w.objectUrlLog.push({ kind: 'create', size: b && b.size }); return 'blob:fx-harness-' + (++blobSeq); };
  URL.revokeObjectURL = () => { w.objectUrlLog.push({ kind: 'revoke' }); };

  class FormDataStub {
    constructor() { this.parts = []; }
    append(name, value, filename) { this.parts.push({ name: String(name), value, filename: filename === undefined ? null : String(filename) }); }
    get(name) { const p = this.parts.find((x) => x.name === name); return p ? p.value : null; }
    has(name) { return this.parts.some((x) => x.name === name); }
  }
  global.FormData = FormDataStub;

  if (!production) {
    /* mock mode: constructing a transport is a contract violation */
    global.fetch = () => { throw new Error('REAL NETWORK FETCH in mock mode'); };
    global.XMLHttpRequest = function () { throw new Error('REAL XHR in mock mode'); };
  } else {
    /* production boot: recording fetch + XHR answering from fixtures. The route
     * is invoked one microtask late (like a real response) so per-test fixtures
     * assigned right after createWorld land before the boot traffic. */
    global.fetch = async function (url, init) {
      init = init || {};
      const entry = { url: String(url), init, at: vnow };
      w.fetchLog.push(entry);
      let doc, status = 200, netErr = null;
      try { doc = await Promise.resolve().then(() => w.prodRoute(String(url), init, entry)); }
      catch (e) {
        if (e && e.__status) { status = e.__status; doc = e.__body; }
        else { netErr = e; }
      }
      if (netErr) throw netErr;
      const text = doc === undefined || doc === null ? '' : (doc && doc.__raw ? doc.text : JSON.stringify(doc));
      const rawHeaders = doc && doc.__raw ? (doc.headers || {}) : null;
      return {
        ok: status >= 200 && status < 300,
        status,
        statusText: String(status),
        headers: { get: (k) => (rawHeaders ? (rawHeaders[String(k).toLowerCase()] ?? null) : null) },
        json: async () => (text === '' ? null : JSON.parse(text)),
        text: async () => text,
        blob: async () => ({ size: text.length, text: async () => text }), // files.js reads blob.size
      };
    };
    class XHRStub {
      constructor() {
        this.method = ''; this.url = ''; this.headers = {}; this.body = null;
        this.status = 0; this.responseText = ''; this.response = null; this.readyState = 0;
        this.upload = {}; this.sent = false; this.aborted = false;
        this.onload = null; this.onerror = null; this.onabort = null; this.ontimeout = null;
        this._ls = {};
        w.xhrLog.push(this);
      }
      open(m, u) { this.method = String(m); this.url = String(u); this.readyState = 1; }
      setRequestHeader(k, v) { this.headers[String(k).toLowerCase()] = String(v); }
      send(body) { this.body = body; this.sent = true; this.readyState = 4; }
      abort() {
        if (this.aborted) return;
        this.aborted = true; this.status = 0;
        for (const fn of this._ls.abort || []) fn({});
        if (this.onabort) this.onabort();
      }
      addEventListener(t, fn) { (this._ls[t] = this._ls[t] || []).push(fn); }
      removeEventListener() {}
      getAllResponseHeaders() { return ''; }
      /* test-side controls */
      __respond(status, text) {
        this.status = status; this.responseText = text; this.response = text; this.readyState = 4;
        for (const fn of this._ls.load || []) fn({});
        if (this.onload) this.onload();
      }
      __progress(loaded, total) {
        if (this.upload && this.upload.onprogress) this.upload.onprogress({ lengthComputable: true, loaded, total });
      }
    }
    global.XMLHttpRequest = XHRStub;
    /* default production router: fixtures keyed like the /v1 surface, plus the
     * persistent-workspace page API and the /files/w/<id>/api/v1 adapter */
    w.prodRoute = async function (url, init) {
      const u = new URL(url, 'http://127.0.0.1:18893');
      const handled = await filesApiRoute(w, u.pathname, init);
      if (handled !== undefined) return handled;
      assertWsAlive(w, u.pathname);
      const p = stripWsPath(u.pathname);
      if (p === '/v1/me') return w.prodDocs.me || meDoc();
      let m = /^\/v1\/drives\/([^/]+)\/list$/.exec(p);
      if (m) {
        const path = u.searchParams.get('path') || '/';
        const doc = (w.prodDocs.listByPath[m[1]] || {})[path] || (w.prodDocs.listByPath[m[1]] || {})['*'];
        if (!doc) throw routeError(404, 'not_found', 'prod stub: no fixture for ' + p + '?path=' + path);
        return doc;
      }
      if (p === '/v1/links') return { url: 'http://127.0.0.1:18893/l/prodtok', expires_at: '2026-10-21T00:00:00Z' };
      m = /^\/v1\/drives\/([^/]+)\/(mkdir|move|delete)$/.exec(p);
      if (m) {
        if (m[2] === 'mkdir') return { path: String((init_json(u)) || '/x'), created: true };
        if (m[2] === 'move') return { from: '/x', to: '/y' };
        return { results: [] };
      }
      throw routeError(404, 'not_found', 'prod stub: unmatched ' + p);
      function init_json() { return null; }
    };
  }

  /* ---------- load the REAL files.js ---------- */
  if (opts.noLoad) {
    w.mock = null;
    w.locationObj = locationObj;
    w.E = (id) => w.doc.getElementById(id);
    w.textOf = textOf;
    w.findAll = (root, pred) => findAll(root, pred);
    w.fire = (el, t, props) => fire(el, t, props);
    w.visible = visible;
    w.mkEl = mkEl;
    w.winFire = (t, ev) => { for (const fn of (winListeners[t] || []).slice()) fn(ev || {}); };
    w.docFire = (t, ev) => {
    const full = Object.assign({ target: w.doc.body, type: t, preventDefault() {}, stopPropagation() {} }, ev || {});
    for (const [lt, fn] of docListeners.slice()) if (lt === t) fn(full);
  };
    w.pumpUntil = (cond, ms, label) => pumpUntil(cond, ms, label);
    w.sleepV = sleepV2;
    w.settle = async () => {
      await yieldMicro();
      await pumpUntil(() => timers.length === 0, 120000, 'settle ' + w.name);
      await yieldMicro();
    };
    return w;
  }
  const src = fs.readFileSync(FILES_JS, 'utf8');
  (0, eval)(src); // indirect eval → global scope, sees the fresh globals

  w.mock = production ? null : (global.__FILES_MOCK || null);
  CUR = w;
  w.locationObj = locationObj;
  w.E = (id) => w.doc.getElementById(id);
  w.textOf = textOf;
  w.findAll = (root, pred) => findAll(root, pred);
  w.fire = (el, t, props) => fire(el, t, props);
  w.visible = visible;
  w.mkEl = mkEl;
  w.winFire = (t, ev) => { for (const fn of (winListeners[t] || []).slice()) fn(ev || {}); };
  w.docFire = (t, ev) => {
    const full = Object.assign({ target: w.doc.body, type: t, preventDefault() {}, stopPropagation() {} }, ev || {});
    for (const [lt, fn] of docListeners.slice()) if (lt === t) fn(full);
  };
  w.pumpUntil = (cond, ms, label) => pumpUntil(cond, ms, label);
  w.sleepV = sleepV2;
  /* Drain the current world: every pending virtual timer plus ALL pending
   * promise chains (microtasks flush across the real-loop yields). */
  w.settle = async () => {
    await yieldMicro();
    await pumpUntil(() => timers.length === 0, 120000, 'settle ' + w.name);
    await yieldMicro();
  };
  return w;
}

/* ======================= shared assertion helpers ====================== */
function section(name) { console.log('\n--- ' + name + ' ---'); }
/* module-level event fire on the current world (the harness scopes listeners
 * per world; module-level drivers act on the most recent one) */
function fire(el, type, props) { return CUR.fire(el, type, props); }

/* Find clickable controls (button / a / [role=button]) under root whose text
 * matches one of the labels. includeDisabled keeps hidden/disabled controls
 * findable for "control must be gone" assertions. */
function findControls(w, root, labels, opts) {
  opts = opts || {};
  const cands = findAllW(w, root, (el) =>
    (el.tagName === 'button' || el.tagName === 'a' || el.getAttribute('role') === 'button' ||
      (el.tagName === 'input' && ['button', 'submit'].includes(String(el.attrs.type)))) &&
    (!opts.armedOnly || !el.disabled));
  const want = Array.isArray(labels) ? labels : [labels];
  return cands.filter((el) => {
    const t = w.textOf(el).trim();
    if (!t && el.tagName === 'input') {
      const label = String(el.attrs.title || el.attrs['aria-label'] || '');
      return want.some((x) => label.includes(x));
    }
    return want.some((x) => t === x || t.includes(x));
  });
}
function findAllW(w, root, pred) { return w.findAll(root, pred); }
function deepText(w, root) { return w.textOf(root); }
function wsText(w) { return w.textOf(w.E('workspace')) + '\n' + w.textOf(w.E('notice')) + '\n' + w.textOf(w.E('gate')); }
/** Rendered text only: subtrees under a hidden element are not displayed. */
function visibleText(w, root) {
  let out = '';
  (function walk(el) {
    if (!el || el.nodeType !== 1) return;
    if (el.hidden === true) return;
    if (['input', 'select', 'textarea'].includes(el.tagName)) return;
    for (const c of el.children || []) {
      if (typeof c === 'string') out += c;
      else if (c.nodeType === 3) out += (c.nodeValue !== undefined ? c.nodeValue : c.text);
      else walk(c);
    }
  }(root));
  return out;
}

/* Drive card lookup: dataset first, then text. */
function driveCard(w, id) {
  const byData = findAllW(w, w.E('drive-grid'), (el) =>
    el.dataset && (el.dataset.drive === id || el.dataset.driveId === id || el.dataset.id === id));
  if (byData.length) return byData[0];
  const cands = findAllW(w, w.E('drive-grid'), (el) => w.textOf(el).includes(id));
  return cands[cands.length - 1] || null;
}
/* File row: deepest tbody row mentioning the name. */
function fileRow(w, name) {
  const rows = findAllW(w, w.E('file-tbody'), (el) =>
    (el.tagName === 'tr' || /\b(fx-row|row|item|entry)\b/.test(String(el.className))) &&
    w.textOf(el).includes(name));
  if (rows.length) return rows[rows.length - 1];
  const cands = findAllW(w, w.E('file-tbody'), (el) => w.textOf(el).includes(name));
  return cands[cands.length - 1] || null;
}
/** Wait until the current listing renders a row containing `name`. */
async function listed(w, name, label) {
  await pumpUntil(() => !!fileRow(w, name), 20000, 'listed ' + (label || name));
}
/* Upload queue task ROOT for one filename (the .fx-task row, never one of
 * its inner text cells). */
function taskEl(w, name) {
  const tasks = findAllW(w, w.E('upload-queue'), (el) =>
    String(el.className).split(/\s+/).includes('fx-task') && w.textOf(el).includes(name));
  if (tasks.length) return tasks[0];
  const cands = findAllW(w, w.E('upload-queue'), (el) => w.textOf(el).includes(name));
  return cands[0] || null; // outermost match as a fallback
}
function taskState(w, name) {
  const t = taskEl(w, name);
  if (!t) return { found: false };
  const st = findAllW(w, t, (el) => String(el.className).split(/\s+/).some((c) => c.indexOf('fx-state-') === 0));
  const cls = st.length ? String(st[0].className).split(/\s+/).filter((c) => c.indexOf('fx-state-') === 0) : [];
  const txt = (st.length ? w.textOf(st[0]) : w.textOf(t)).trim();
  return {
    found: true,
    done: cls.includes('fx-state-done') || /完成|成功/.test(txt) && !/未确认|失败|中断/.test(txt),
    failed: cls.includes('fx-state-failed') || /失败|错误|拒绝/.test(txt),
    interrupted: cls.includes('fx-state-interrupted') || /未确认|中断|等待/.test(txt),
    busy: cls.includes('fx-state-busy') || /上传中|传输中|等待/.test(txt),
    text: txt,
    el: t,
  };
}
/* The one open native dialog, if any. */
function openDialog(w) {
  const dlgs = ['dlg-mkdir', 'dlg-rename', 'dlg-delete', 'dlg-share'].map((id) => w.E(id)).filter((d) => d.open === true);
  return dlgs[dlgs.length - 1] || null;
}
function dialogIsClosed(w, id) { return w.E(id).open !== true; }

/* Breadcrumb crumbs as {text, current} in DOM order. */
function crumbs(w) {
  const els = findAllW(w, w.E('breadcrumb'), (el) =>
    (el.tagName === 'a' || el.tagName === 'button' || el.tagName === 'span') &&
    (String(el.className).includes('fx-crumb') || String(el.className).includes('crumb')) &&
    w.textOf(el).trim() !== '');
  return els.map((el) => ({ el, text: w.textOf(el).trim(), current: el.getAttribute('aria-current') === 'page' }));
}

/* ---------- mock override plumbing ---------- */
function clearMap(w) {
  const m = w.mock && w.mock.map;
  if (!m) return;
  for (const k of Object.keys(m)) delete m[k];
}
/* Install overrides: value may be a function (receives dispatcher args),
 * a plain doc (success payload), or {throw: mkErr(...)}. */
function installOverrides(w, map) {
  if (!w.mock || !w.mock.map) throw new Error('mock map missing — __FILES_MOCK contract not honored');
  for (const [key, val] of Object.entries(map)) {
    w.mock.map[key] = typeof val === 'function' ? val
      : val && val.throw ? async () => { throw val.throw; }
        : async () => (typeof val === 'function' ? val() : JSON.parse(JSON.stringify(val)));
  }
}
function resetMock(w) {
  if (w.mock && typeof w.mock.reset === 'function') w.mock.reset();
  clearMap(w);
  w.upReceipts.length = 0;
  w.expectedPath = null;
}
/* Normalize a recorded/args request into {drive, op, path, page, perPage, query, body}. */
function normReq(w, c) {
  if (!c) return null;
  let path = c.path || c.url || '';
  let query = c.query;
  if (typeof query === 'string') query = Object.fromEntries(new URLSearchParams(query));
  if (!query && typeof path === 'string' && path.indexOf('?') >= 0) {
    const qi = path.indexOf('?');
    query = Object.fromEntries(new URLSearchParams(path.slice(qi + 1)));
    path = path.slice(0, qi);
  }
  query = query || {};
  const m = /^\/v1\/drives\/([^/]+)\/(\w+)$/.exec(String(path));
  const dec = (v) => { try { return decodeURIComponent(v == null ? '' : String(v)); } catch (e) { return String(v); } };
  return {
    method: c.method,
    path: String(path),
    drive: m ? dec(m[1]) : (c.drive ? dec(c.drive) : null),
    op: m ? m[2] : null,
    pathParam: dec(query.path),
    page: query.page != null ? Number(query.page) : null,
    perPage: query.per_page != null ? Number(query.per_page) : null,
    onConflict: query.on_conflict != null ? dec(query.on_conflict) : null,
    rawQuery: query,
    body: c.body,
    hasKey: c.has_key,
  };
}
/* Best-effort request peek for override handlers. The documented dispatcher
 * passes a ctx object {method, path, query:URLSearchParams, body, key,
 * onProgress, task}; fall back to the last recorded calls entries, then the
 * harness's expectedPath slot. */
function peekReq(w, args, method, pathPrefix) {
  for (const a of args || []) {
    if (a && typeof a === 'object' && (a.method || a.path)) {
      let query = a.query;
      if (query && typeof query.entries === 'function') query = Object.fromEntries(query.entries());
      else if (query && typeof query.forEach === 'function') { const o = {}; query.forEach((v, k) => { o[k] = v; }); query = o; }
      const n = normReq(w, { method: a.method || method, path: a.path || '', query: query || {}, body: a.body });
      if (n.path) return n;
    }
    if (typeof a === 'string') {
      const n = normReq(w, { method, path: a });
      if (n.path) return n;
    }
  }
  const calls = (w.mock && w.mock.calls) || [];
  for (let i = calls.length - 1; i >= 0 && i >= calls.length - 4; i--) {
    const n = normReq(w, calls[i]);
    if (n && n.method === method && n.path.indexOf(pathPrefix) === 0) return n;
  }
  if (w.expectedPath != null) return { method, path: pathPrefix, pathParam: w.expectedPath, drive: w.expectedDrive || null, rawQuery: {}, body: undefined };
  return null;
}
function callsWhere(w, pred) { return ((w.mock && w.mock.calls) || []).map((c) => normReq(w, c)).filter(pred); }

/* ---------- clipboard assertions (actual copied text, not just a notice) ---- */
/** The exact text that ended up on the clipboard, however it got there. */
function copiedTexts(w) { return (w.copiedLog || []).map((c) => c.text); }
function lastCopied(w) { const l = w.copiedLog || []; return l.length ? l[l.length - 1] : null; }
/** Only writeText (async Clipboard API) payloads that RESOLVED. */
function writeTextCopies(w) { return (w.copiedLog || []).filter((c) => c.via === 'writeText'); }
/** Only execCommand (legacy selection) copies. */
function execCopies(w) { return (w.copiedLog || []).filter((c) => c.via === 'execCommand'); }
function copiedEquals(w, text) { return copiedTexts(w).includes(text); }
/** #share-copy label: success shows 已复制, otherwise it stays/returns to 复制. */
function shareCopyLabel(w) { return w.textOf(w.E('share-copy')).trim(); }
function shareCopySaysCopied(w) { return /已复制/.test(shareCopyLabel(w)); }
/** Hold the async writeText so a stale callback can be resolved later. */
function holdClipboard(w) {
  const h = { promise: null, resolve: null, reject: null };
  h.promise = new Promise((res, rej) => { h.resolve = res; h.reject = rej; });
  w.clipHold = h;
  return h;
}
function releaseClipboard(w) { w.clipHold = null; }
/* Download-ticket requests (mock mode). normReq's op regex cannot carry the
 * hyphenated segment, so this parses the exact template path directly. */
function ticketCalls(w) {
  return ((w.mock && w.mock.calls) || [])
    .filter((c) => c.method === 'POST' && /\/download-ticket$/.test(String(c.path)))
    .map((c) => {
      const m = /^\/v1\/drives\/([^/]+)\/download-ticket$/.exec(String(c.path));
      return {
        drive: m ? decodeURIComponent(m[1]) : null,
        rawPath: String(c.path),
        body: c.body,
        hasKey: c.has_key,
        query: c.query || '',
      };
    });
}
/* Download-ticket requests (production boot; recorded by the fetch stub). */
function prodTicketFetches(w) {
  return (w.fetchLog || []).filter((f) =>
    f.init && f.init.method === 'POST' && /\/download-ticket$/.test(String(f.url).split('?')[0]));
}
function callsJSON(w) { return JSON.stringify((w.mock && w.mock.calls) || []); }
function assertNoKeyLeak(w, tag, keys) {
  const raw = callsJSON(w) + '\n' + JSON.stringify({
    storage: w.storageLog, cookie: w.cookieLog, url: w.urlLog, clipboard: w.clipboardLog, innerhtml: w.innerHTMLLog,
  });
  for (const k of keys) ok(!raw.includes(k), tag + ' never exposes key material (' + k.slice(0, 12) + '…) in calls/storage/cookie/URL/clipboard');
}

/* ---------- common flows ---------- */
async function login(w, key) {
  w.E('key').value = key;
  fire(w.E('login-form'), 'submit');
  await pumpUntil(() => w.E('workspace').hidden === false, 20000, 'login as ' + key);
}
async function logout(w) {
  fire(w.E('logout'), 'click');
  /* production logout scrubs in-page first, then returns to /files/ (gate or
   * list); mock logout goes straight to the gate. */
  await pumpUntil(() => w.E('gate').hidden === false || w.E('ws-list').hidden === false, 20000, 'logout');
  await w.settle();
}
/* ---- persistent-workspace (production) helpers ---- */
/* Workspace entry card lookup: dataset first, then text. */
function wsCard(w, id) {
  const byData = findAllW(w, w.E('ws-grid'), (el) =>
    el.dataset && (el.dataset.wsId === id || el.dataset.ws === id));
  if (byData.length) return byData[0];
  const cands = findAllW(w, w.E('ws-grid'), (el) => w.textOf(el).includes(id));
  return cands[cands.length - 1] || null;
}
/* Click a workspace entry's 进入 control and wait for the workspace UI. */
async function enterWsEntry(w, id) {
  const card = wsCard(w, id);
  if (!card) throw new Error('workspace card not found for ' + id);
  const go = findControls(w, card, ['进入'], { armedOnly: true })[0];
  fire(go || card, 'click');
  await pumpUntil(() => w.E('workspace').hidden === false, 20000, 'enter workspace ' + id);
}
function prodLogoutPosts(w) {
  return (w.fetchLog || []).filter((f) =>
    f.init && f.init.method === 'POST' &&
    /^\/files\/w\/[^/]+\/api\/logout$/.test(String(f.url).split('?')[0]));
}
function prodLoginPosts(w) {
  return (w.fetchLog || []).filter((f) =>
    f.init && f.init.method === 'POST' && String(f.url) === '/files/api/login');
}
function prodMeFetches(w, id) {
  return (w.fetchLog || []).filter((f) =>
    String(f.url).indexOf('/files/w/' + id + '/api/v1/me') === 0);
}
function prodWorkspacesFetches(w) {
  return (w.fetchLog || []).filter((f) => String(f.url) === '/files/api/workspaces');
}
/* The fetch/XHR record of every workspace-adapter data request. */
function prodDataUrls(w) {
  return (w.fetchLog || []).map((f) => f.url).concat((w.xhrLog || []).map((x) => x.url));
}
/* Log in and, if the landing rule did NOT auto-enter a browser, assert/return
 * the selector landing. Auto-entry only happens for exactly ONE granted drive;
 * callers that expect the classic landing use multi/zero-drive fixtures. */
async function loginExpectLanding(w, key) {
  await login(w, key);
  if (w.E('drives-home').hidden !== false) throw new Error('expected the 我的网盘 landing after login as ' + key);
  return w;
}
/* Enter a drive WITHOUT touching the upload-first manager collapse state. */
async function enterDriveRaw(w, id) {
  const card = driveCard(w, id);
  if (!card) throw new Error('drive card not found for ' + id);
  const ctl = findControls(w, card, ['进入', '打开', '浏览'], { armedOnly: true })[0];
  fire((ctl || card), 'click');
  await pumpUntil(() => w.E('browser-view').hidden === false, 20000, 'enter drive ' + id);
}
/* The manager DEFAULTS OPEN for every drive (files.html ships #file-manager
 * visible with aria-expanded=true). These classic file-management tests all need
 * the table/search/pager controls reachable, so normalize: if a drive somehow
 * left it collapsed, expand it once. Idempotent on the normal (already-open)
 * case and does NOT participate in the manager's own default-state assertions. */
function ensureManagerOpen(w) {
  const mgr = w.E('file-manager');
  const toggle = w.E('manager-toggle');
  if (mgr && toggle && mgr.hidden === true) toggle.click();
}
async function enterDrive(w, id) {
  await enterDriveRaw(w, id);
  ensureManagerOpen(w);
}
async function backToDrives(w) {
  fire(w.E('back-drives'), 'click');
  await pumpUntil(() => w.E('drives-home').hidden === false && w.E('browser-view').hidden === true, 20000, 'back to drives');
}
function fakeFile(name, size, type) {
  return { name, size: size == null ? 3 : size, type: type || 'application/octet-stream' };
}
async function queueUpload(w, files, via) {
  const before = findAllW(w, w.E('upload-queue'), (el) => w.textOf(el).includes(files[0].name)).length;
  if (via === 'drop') fire(w.E('dropzone'), 'drop', { dataTransfer: { files } });
  else if (via === 'paste') w.docFire('paste', { clipboardData: { files } }); // files.js binds paste on document
  else {
    w.E('file-input').files = files.slice();
    fire(w.E('file-input'), 'change');
  }
  await pumpUntil(() => findAllW(w, w.E('upload-queue'), (el) => w.textOf(el).includes(files[files.length - 1].name)).length > before || w.xhrLog.length, 20000, 'queue upload ' + files.map((f) => f.name).join(','));
}
/* FIFO receipts for PUT overrides (mock mode). */
function pushReceipts(w, ...items) { for (const it of items) w.upReceipts.push(it); }
function putOverride(w) {
  return async (...args) => {
    const req = peekReq(w, args, 'PUT', '/v1/drives/');
    const next = w.upReceipts.shift();
    if (!next) throw mkErr(500, 'harness', 'no scripted receipt for PUT ' + (req && req.pathParam));
    if (next.deferred) return next.deferred.promise;
    if (next.throw) throw next.throw;
    return next;
  };
}
/* List override that routes by the REQUESTED directory (mock mode). */
function listOverride(w, byPath, opts) {
  opts = opts || {};
  return async (...args) => {
    const req = peekReq(w, args, 'GET', '/v1/drives/');
    const p = req && req.pathParam ? req.pathParam : (w.expectedPath || '/');
    if (opts.record) opts.record.push(p);
    const doc = byPath[p] != null ? byPath[p] : byPath['*'];
    if (doc == null) throw mkErr(404, 'not_found', 'harness: no list fixture for "' + p + '"');
    if (doc.deferred) return doc.deferred.promise;
    if (doc.throw) throw doc.throw;
    return JSON.parse(JSON.stringify(typeof doc === 'function' ? doc(req) : doc));
  };
}
/* Filter the visible file rows down to names (assertion helper). */
function visibleRowNames(w) {
  const rows = findAllW(w, w.E('file-tbody'), (el) => el.tagName === 'tr');
  return rows.map((r) => w.textOf(r).trim()).filter(Boolean);
}

/* ============================ selftest mode ============================ */
async function selftest() {
  console.log('files-page harness infrastructure self-test');
  dropTimers();
  const w = createWorld({ search: '?mock=1', noLoad: true });

  /* classList + className sync */
  const el = w.mkEl('div');
  el.className = 'a b';
  ok(el.classList.contains('a') && el.classList.contains('b'), 'selftest: className parses into classList');
  el.classList.add('c');
  ok(el.className === 'a b c', 'selftest: classList.add syncs className');
  el.classList.remove('a');
  ok(!el.classList.contains('a') && el.className === 'b c', 'selftest: classList.remove syncs className');
  ok(el.classList.toggle('b', false) === false && !el.classList.contains('b'), 'selftest: classList.toggle(force)');

  /* attribute reflection */
  const el2 = w.mkEl('button');
  el2.setAttribute('disabled', '');
  ok(el2.disabled === true, 'selftest: setAttribute(disabled) reflects the property');
  el2.removeAttribute('disabled');
  ok(el2.disabled === false, 'selftest: removeAttribute(disabled) clears the property');
  el2.setAttribute('data-drive-id', 'x1');
  ok(el2.dataset.driveId === 'x1', 'selftest: data-* attributes sync into dataset');
  el2.setAttribute('hidden', 'until-found');
  ok(el2.hidden === true, 'selftest: setAttribute(hidden) reflects the property');
  el2.hidden = false;
  ok(el2.getAttribute('hidden') === null, 'selftest: property write clears the hidden attribute');

  /* disabled buttons never dispatch click */
  let clicks = 0;
  el2.disabled = true;
  el2.addEventListener('click', () => { clicks++; });
  el2.click();
  ok(clicks === 0, 'selftest: disabled button swallows click');
  el2.disabled = false;
  el2.click();
  ok(clicks === 1, 'selftest: enabled button dispatches click');

  /* native dialog semantics */
  const dlg = w.E('dlg-share');
  let closed = 0;
  dlg.addEventListener('close', () => { closed++; });
  dlg.showModal();
  ok(dlg.open === true, 'selftest: showModal opens the dialog');
  dlg.close();
  ok(dlg.open === false && closed === 1, 'selftest: close() resets open and fires close');

  /* select value semantics */
  const sel = w.E('share-ttl');
  sel.value = '259200';
  ok(sel.value === '259200', 'selftest: select.value picks the matching option');
  const opts = findAllW(w, sel, (o) => o.tagName === 'option');
  ok(opts.length === 4 && opts[2].selected === true && opts[0].selected === false, 'selftest: option selection follows select.value');

  /* textContent / textOf */
  const host = w.mkEl('div');
  host.append('plain ', w.mkEl('b'), ' tail');
  host.children[1].textContent = '<script>x</script>';
  ok(w.textOf(host) === 'plain <script>x</script> tail', 'selftest: textOf concatenates child text verbatim (no markup interpretation)');
  host.textContent = 'reset';
  ok(host.children.length === 0 && w.textOf(host) === 'reset', 'selftest: textContent= clears children');

  /* querySelector engine */
  const q = w.doc.querySelector('#workspace #me-prefix');
  ok(q === w.E('me-prefix'), 'selftest: document.querySelector #a #b hits the tree');
  ok(w.doc.querySelectorAll('.fx-state-done').length === 0, 'selftest: querySelectorAll misses absent classes');

  /* hidden visibility walk */
  w.E('browser-view').hidden = true;
  ok(!w.visible(w.E('upload-queue')), 'selftest: visible() honors ancestor hidden');
  w.E('workspace').hidden = false;
  w.E('browser-view').hidden = false;
  w.E('upload-panel').hidden = false;
  ok(w.visible(w.E('upload-queue')), 'selftest: visible() true when chain visible');

  /* virtual clock: ordering + deferred pump */
  const order = [];
  global.setTimeout(() => order.push('t20'), 20);
  global.setTimeout(() => order.push('t5'), 5);
  const d = { done: false };
  const deferredPromise = new Promise((r) => { d.resolve = () => { d.done = true; r(); }; });
  const pumped = pumpUntil(() => d.done, 5000, 'selftest deferred').then(() => order.push('after-defer'));
  realSetTimeout(() => d.resolve(), 5);
  await pumped;
  ok(order[0] === 't5' && order[1] === 't20', 'selftest: virtual clock fires in time order, got ' + order.join(','));

  /* innerHTML is recorded (server data must never flow through it) */
  w.E('dz-text').innerHTML = '<b>x</b>';
  ok(w.innerHTMLLog.length === 1 && w.innerHTMLLog[0].tag === 'div', 'selftest: innerHTML writes are recorded');

  /* ghost id detection */
  w.doc.getElementById('no-such-id-in-html');
  ok(w.ghostIds.has('no-such-id-in-html'), 'selftest: unknown getElementById ids are tracked');

  /* clipboard model: execCommand copies the SELECTED control's value, and a
   * selection placed outside an open modal dialog is inert (Chrome semantics). */
  const outside = w.mkEl('textarea');
  w.doc.body.appendChild(outside);
  outside.value = 'OUT-VALUE';
  outside.focus(); outside.select();
  w.E('dlg-share').showModal();
  const execOutside = w.doc.execCommand('copy');
  ok(execOutside === false && w.copiedLog.length === 0,
    'selftest: execCommand copies nothing from a control behind the open modal (inert)');
  const inside = w.E('share-url');
  inside.value = 'IN-VALUE';
  inside.focus(); inside.select();
  const execInside = w.doc.execCommand('copy');
  ok(execInside === true && w.copiedLog.length === 1 && w.copiedLog[0].text === 'IN-VALUE' && w.copiedLog[0].inDialog === true,
    'selftest: execCommand copies the selected control INSIDE the modal, got ' + JSON.stringify(w.copiedLog));
  w.E('dlg-share').close();
  outside.remove();
  w.copiedLog.length = 0; w.clipboardLog.length = 0; w.execLog.length = 0; w.focusLog.length = 0;

  /* HTML_TREE initial state: the manager defaults OPEN with the collapse label. */
  ok(w.E('file-manager').hidden === false, 'selftest: HTML_TREE ships #file-manager visible (default open)');
  ok(w.E('manager-toggle').getAttribute('aria-expanded') === 'true', 'selftest: HTML_TREE manager toggle starts aria-expanded=true');
  ok(/收起文件管理/.test(w.textOf(w.E('manager-toggle'))), 'selftest: HTML_TREE manager toggle label starts 收起文件管理');

  console.log(failed ? '\nSELFTEST FAILED: ' + failed : '\nSELFTEST ALL PASS (' + passCount + ' checks)');
  process.exit(failed ? 1 : 0);
}

/* ============================== main suite ============================= */
async function main() {
  if (!fs.existsSync(FILES_JS)) {
    console.error('FATAL: internal/admin/web/assets/files.js has not landed yet.');
    console.error('The harness is complete; re-run once the frontend implementation lands.');
    process.exit(2);
  }

  /* ============ Session 1: mock boot, full regression surface ============ */
  dropTimers();
  const w = createWorld({ search: '?mock=1' });
  if (!w.mock || !w.mock.map || !Array.isArray(w.mock.calls)) {
    console.error('FATAL: __FILES_MOCK missing or malformed (need {map, calls[, reset]})');
    process.exit(2);
  }
  const meOverride = (doc) => async () => JSON.parse(JSON.stringify(doc));

  /* ---- F1: key gate ---- */
  section('F1 key gate');
  ok(w.E('mock-banner').hidden === false, 'F1 mock banner visible in ?mock=1');
  ok(w.E('gate').hidden === false && w.E('workspace').hidden === true, 'F1 boot shows the gate only');
  w.E('key').value = '   ';
  fire(w.E('login-form'), 'submit');
  await w.settle();
  ok(w.E('gate').hidden === false && w.textOf(w.E('gate-err')).trim() !== '', 'F1 empty key is rejected with a message');
  ok(callsWhere(w, (r) => r.path === '/v1/me').length === 0, 'F1 empty key issues no /v1/me probe');
  installOverrides(w, { 'GET /v1/me': async () => { throw mkErr(401, 'unauthorized', 'BAD-KEY-SENTINEL'); } });
  w.E('key').value = 'sk-c2a-wrongkey';
  fire(w.E('login-form'), 'submit');
  await w.settle();
  ok(w.E('workspace').hidden === true && w.E('gate').hidden === false, 'F1 invalid key never enters the workspace');
  ok(w.textOf(w.E('gate-err')).includes('BAD-KEY-SENTINEL') || w.textOf(w.E('gate-err')).trim() !== '', 'F1 invalid key error is explained, got: ' + JSON.stringify(w.textOf(w.E('gate-err'))));

  resetMock(w);
  installOverrides(w, { 'GET /v1/me': meOverride(meDoc()) });
  await login(w, KEY_MAIN);
  ok(w.E('gate').hidden === true && w.textOf(w.E('me-prefix')).includes('fxmain'), 'F1 valid key opens the workspace and shows the key prefix');

  /* ---- F2: drive cards + landing (default drive must not skip it) ---- */
  section('F2 drive cards and landing');
  ok(w.E('drives-home').hidden === false, 'F2 landing (我的网盘) shows after login');
  ok(w.E('browser-view').hidden === true, 'F2 browser hidden on landing');
  ok(callsWhere(w, (r) => r.op === 'list').length === 0, 'F2 landing never auto-lists a drive');
  for (const d of [{ id: 'fx-alpha', root: '/AlphaRoot' }, { id: 'fx-readonly', root: '/Vault' }, { id: 'fx-broken', root: '/BrokenRoot' }]) {
    const card = driveCard(w, d.id);
    ok(!!card, 'F2 card rendered for granted drive ' + d.id);
    if (card) {
      const txt = w.textOf(card);
      ok(txt.includes(d.root), 'F2 card ' + d.id + ' shows the grant root ' + d.root + ' (not the mount path)');
    }
  }
  ok(driveCard(w, 'fx-notgranted') === null, 'F2 no card for a drive outside the grant');
  const alphaCard = driveCard(w, 'fx-alpha');
  ok(w.textOf(alphaCard).includes('默认') || findAllW(w, alphaCard, (el) => String(el.className).includes('fx-default-chip')).length > 0,
    'F2 default drive carries a default marker');
  const alphaEnter = findControls(w, alphaCard, ['进入'], { armedOnly: true })[0];
  ok(!!alphaEnter && String(alphaEnter.attrs['aria-label']) === '进入网盘 fx-alpha',
    'F2 enter button exposes aria-label "进入网盘 <id>", got: ' + (alphaEnter && JSON.stringify(alphaEnter.attrs['aria-label'])));
  const brokenCard = driveCard(w, 'fx-broken');
  ok(/needs_reauth|重新授权|需重新/.test(w.textOf(brokenCard)), 'F2 broken drive health surfaced on its card');

  /* ---- F2b/F2c: landing depends on the grant COUNT ---- */
  section('F2b single-drive direct entry');
  /* Exactly ONE granted drive: login enters it directly at the logical root '/'
   * WITHOUT stopping on the selector — but only through the normal list guard. */
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meSingle('solo', ['list', 'read', 'write'])),
    'GET /v1/drives/solo/list': listOverride(w, { '/': listDoc('/', [entry('solo.txt', false, 5)]) }),
  });
  await login(w, KEY_MAIN);
  ok(w.E('browser-view').hidden === false, 'F2b a single-drive login enters the browser directly');
  ok(w.E('drives-home').hidden === true, 'F2b the 我的网盘 selector is skipped for a single drive');
  await listed(w, 'solo.txt', 'F2b single-drive root listing');
  const soloLists = () => callsWhere(w, (r) => r.op === 'list');
  ok(soloLists().length === 1 && soloLists()[0].pathParam === '/',
    'F2b direct entry lists the LOGICAL root "/" exactly once, got ' + JSON.stringify(soloLists().map((c) => c.pathParam)));
  ok(ticketCalls(w).length === 0, 'F2b neither login nor the auto-entry list ever requests a download ticket');
  ok(w.textOf(w.E('breadcrumb')).includes('solo'), 'F2b breadcrumb names the single drive');
  ok(crumbs(w).length >= 1 && crumbs(w)[0].current, 'F2b the root crumb is current at "/" after direct entry');
  ok(w.E('gate').hidden === true, 'F2b single-drive entry keeps the gate hidden');
  ok(w.E('back-drives').hidden === false, 'F2b the explicit back control is retained (no auto re-entry)');

  /* Single drive WITHOUT list: must NOT be entered by force; surface the reason. */
  await logout(w);
  resetMock(w);
  installOverrides(w, { 'GET /v1/me': meOverride(meSingle('solo-nolist', ['read', 'write'])) });
  await login(w, KEY_MAIN);
  ok(w.E('drives-home').hidden === false && w.E('browser-view').hidden === true,
    'F2b a single drive without list permission does NOT bypass the guard into a browser');
  ok(callsWhere(w, (r) => r.op === 'list').length === 0, 'F2b no-list single drive issues no list request');
  ok(/list|权限/.test(w.textOf(w.E('notice'))), 'F2b the missing list permission is explained, got ' + JSON.stringify(w.textOf(w.E('notice'))));

  /* Single drive whose list call ERRORS: entry happens, the error is surfaced,
   * the session is not kicked to the gate and nothing crashes. */
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meSingle('solo-err', ['list', 'read'])),
    'GET /v1/drives/solo-err/list': async () => { throw mkErr(503, 'drive_unavailable', 'SOLO-LIST-SENTINEL upstream down'); },
  });
  await login(w, KEY_MAIN);
  await w.settle();
  ok(w.E('browser-view').hidden === false, 'F2b an erroring single drive is still entered (no crash back to landing)');
  ok(w.E('gate').hidden === true, 'F2b a single-drive list error never logs the session out');
  ok(w.textOf(w.E('file-list')).includes('SOLO-LIST-SENTINEL') || /失败|不可用|错误/.test(w.textOf(w.E('file-list'))),
    'F2b the single-drive list error is rendered, got ' + JSON.stringify(w.textOf(w.E('file-list')).slice(0, 160)));

  section('F2c multi/zero drives keep the selector');
  /* MULTIPLE drives: selector landing (already covered by F2), asserted again
   * here on a fresh session for the count rule. */
  await logout(w);
  resetMock(w);
  installOverrides(w, { 'GET /v1/me': meOverride(meDoc()) });
  await loginExpectLanding(w, KEY_MAIN);
  ok(w.E('drives-home').hidden === false && w.E('browser-view').hidden === true, 'F2c multiple drives land on the selector');
  ok(callsWhere(w, (r) => r.op === 'list').length === 0, 'F2c a multi-drive landing never auto-lists');
  /* ZERO drives: empty landing, never a browser. */
  await logout(w);
  resetMock(w);
  installOverrides(w, { 'GET /v1/me': meOverride(meEmpty()) });
  await loginExpectLanding(w, KEY_EMPTY);
  ok(w.E('drives-home').hidden === false && w.E('browser-view').hidden === true, 'F2c zero drives stay on the empty landing');
  ok(w.textOf(w.E('drives-home')).includes('没有已授权的网盘') || w.textOf(w.E('drives-sub')).includes('没有授权任何网盘'),
    'F2c zero-drive landing explains the empty grant state');
  ok(callsWhere(w, (r) => r.op === 'list').length === 0, 'F2c zero drives never list anything');

  /* restore the multi-drive KEY_MAIN session for the rest of session 1 */
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/': listDoc('/', [entry('dl.txt', false, 4), entry('docs', true)]) }),
    /* resetMock clears the boot-registered mock templates, so re-provide the
     * ticket handler exactly as the mock backend answers it. */
    'POST /v1/drives/fx-alpha/download-ticket': async () => ({
      url: '/download/' + 'm'.repeat(43),
      expires_at: new Date(Date.now() + 120000).toISOString(),
    }),
  });
  await loginExpectLanding(w, KEY_MAIN);

  /* ---- F2d: mock-mode download-ticket contract (UI simulation only) ---- */
  section('F2d mock download ticket');
  await enterDrive(w, 'fx-alpha');
  await listed(w, 'dl.txt', 'F2d root listing');
  ok(ticketCalls(w).length === 0, 'F2d entering a drive and listing never auto-requests a ticket');
  const mockDlBtn = () => findControls(w, fileRow(w, 'dl.txt'), ['下载'], { armedOnly: true })[0];
  const mockTicketsBefore = ticketCalls(w).length;
  fire(mockDlBtn(), 'click');
  /* the info notice is non-sticky: read it while it is still shown */
  await pumpUntil(() => /演示模式|模拟/.test(w.textOf(w.E('notice'))), 20000, 'F2d mock ticket notice');
  const mockTk = ticketCalls(w).slice(mockTicketsBefore);
  ok(mockTk.length === 1, 'F2d download issues exactly one mock ticket POST, got ' + mockTk.length);
  ok(mockTk[0] && mockTk[0].drive === 'fx-alpha', 'F2d ticket targets the exact drive, got ' + JSON.stringify(mockTk[0] && mockTk[0].drive));
  ok(mockTk[0] && mockTk[0].body && mockTk[0].body.path === '/dl.txt',
    'F2d ticket POST body is exactly {path:"/dl.txt"}, got ' + JSON.stringify(mockTk[0] && mockTk[0].body));
  ok(mockTk[0] && mockTk[0].hasKey === true, 'F2d mock ticket request carries the key (apikey header only)');
  ok(mockTk[0] && !String(mockTk[0].rawPath).includes('?') && !mockTk[0].query,
    'F2d ticket path carries no query (path rides in the JSON body), got ' + JSON.stringify(mockTk[0] && mockTk[0].rawPath));
  ok(w.anchorLog.length === 0, 'F2d mock mode performs NO native navigation (receipt only)');
  ok(w.objectUrlLog.length === 0, 'F2d mock mode creates no object URL / Blob');
  ok(/演示模式|模拟/.test(w.textOf(w.E('notice'))), 'F2d mock receipt is described as a simulation, got ' + JSON.stringify(w.textOf(w.E('notice'))));
  assertNoKeyLeak(w, 'F2d', ALL_KEYS);

  /* F2d-key-smuggle: in MOCK mode the page still knows the session key, so a
   * ticket that smuggles it (path or query) must be refused outright — the page
   * must never hand key-bearing URLs to a download. (The production page holds
   * no key at all, so this guard only exists where a key exists.) */
  {
    const prevTicket = w.mock.map['POST /v1/drives/fx-alpha/download-ticket'];
    const anchorBefore = w.anchorLog.length;
    installOverrides(w, {
      'POST /v1/drives/fx-alpha/download-ticket': async () => ({
        url: '/download/tok' + KEY_MAIN, expires_at: new Date(Date.now() + 120000).toISOString(),
      }),
    });
    fire(mockDlBtn(), 'click');
    await w.settle();
    ok(w.anchorLog.length === anchorBefore, 'F2d a mock ticket smuggling the session key in its token path starts no download');
    ok(/凭证无效|无效|过期/.test(w.textOf(w.E('notice'))),
      'F2d the key-smuggling ticket is refused with an explanation, got ' + JSON.stringify(w.textOf(w.E('notice'))));
    installOverrides(w, {
      'POST /v1/drives/fx-alpha/download-ticket': async () => ({
        url: '/download/tok?k=' + KEY_MAIN, expires_at: new Date(Date.now() + 120000).toISOString(),
      }),
    });
    fire(mockDlBtn(), 'click');
    await w.settle();
    ok(w.anchorLog.length === anchorBefore, 'F2d a mock ticket smuggling the session key in a query starts no download');
    /* restore the scripted ticket handler for later sections */
    installOverrides(w, { 'POST /v1/drives/fx-alpha/download-ticket': prevTicket });
    assertNoKeyLeak(w, 'F2d-key-smuggle', ALL_KEYS);
  }

  /* A directory row has NO download control (directories are not downloadable). */
  const docsDlRow = fileRow(w, 'docs');
  const dirDl = findControls(w, docsDlRow, ['下载'], { armedOnly: true });
  ok(dirDl.length === 0, 'F2d a directory row offers no download control');
  const ticketsBeforeDir = ticketCalls(w).length;
  fire(w.E('theme-toggle'), 'click'); // unrelated UI action in between
  ok(ticketCalls(w).length === ticketsBeforeDir, 'F2d no extra ticket is requested for a directory');
  await backToDrives(w);

  /* ---- F3: readonly drive — destructive/upload affordances inert ---- */
  section('F3 readonly drive');
  installOverrides(w, {
    'GET /v1/drives/fx-readonly/list': listOverride(w, { '/': listDoc('/', [entry('vault.txt', false, 5), entry('box', true)]) }),
  });
  await enterDrive(w, 'fx-readonly');
  await listed(w, 'vault.txt', 'F3 readonly listing');
  ok(w.textOf(w.E('breadcrumb')).includes('fx-readonly'), 'F3 breadcrumb names the entered drive');
  ok(w.textOf(w.E('dz-text')).includes('未授予写入权限'), 'F3 read-only drive dropzone explains the missing write grant');
  ok(w.E('file-input').disabled === true && w.E('mkdir-btn').disabled === true && w.E('conflict-mode').disabled === true,
    'F3 write controls are disabled on a read-only drive');
  // upload is attempted and REJECTED server-side of the UI: no PUT, explicit notice
  const roPuts = () => callsWhere(w, (r) => r.method === 'PUT');
  w.E('file-input').files = [fakeFile('ro-up.bin', 3)];
  fire(w.E('file-input'), 'change');
  await w.settle();
  ok(roPuts().length === 0, 'F3 upload on a list/read drive sends no PUT');
  ok(/write|上传|权限/.test(w.textOf(w.E('notice'))) && w.E('notice').hidden === false, 'F3 upload refusal is explained, got: ' + JSON.stringify(w.textOf(w.E('notice'))));
  // mkdir is rejected without a dialog
  fire(w.E('mkdir-btn'), 'click');
  await w.settle();
  ok(w.E('dlg-mkdir').open !== true, 'F3 mkdir dialog never opens without write');
  ok(/write|权限/.test(w.textOf(w.E('notice'))), 'F3 mkdir refusal is explained');
  ok(callsWhere(w, (r) => r.op === 'mkdir').length === 0, 'F3 no mkdir POST without write');
  // row actions: download stays, destructive actions render disabled
  const roRow = fileRow(w, 'vault.txt');
  ok(!!roRow, 'F3 readonly file row rendered');
  const roBtnOf = (label) => findControls(w, roRow, [label], {}).filter((b) => w.textOf(b).trim() === label);
  ok(roBtnOf('下载').length === 1 && !roBtnOf('下载')[0].disabled, 'F3 download stays available with read perm');
  for (const label of ['分享', '重命名', '删除']) {
    const btns = roBtnOf(label);
    ok(btns.length === 1 && btns[0].disabled === true, 'F3 ' + label + ' rendered disabled without the permission');
  }
  fire(roBtnOf('删除')[0], 'click');
  await w.settle();
  ok(w.E('dlg-delete').open !== true && callsWhere(w, (r) => r.op === 'delete').length === 0, 'F3 disabled delete fires nothing');
  let roPicker = 0;
  w.E('file-input').addEventListener('click', () => { roPicker++; });
  fire(w.E('dz-text'), 'click');
  await w.settle();
  ok(roPicker === 0, 'F3 the plain dropzone surface opens no picker while write is missing');
  await backToDrives(w);

  /* ---- F4: permission union (global write + per-drive list/read) ---- */
  section('F4 permission union');
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meUnion()),
    'GET /v1/drives/fx-ro-union/list': listOverride(w, { '/': listDoc('/', [entry('u.txt', false, 1)]) }),
    'POST /v1/drives/fx-ro-union/mkdir': async (...args) => {
      const req = peekReq(w, args, 'POST', '/v1/drives/');
      w.unionMkdir = (w.unionMkdir || 0) + 1;
      w.unionMkdirBody = req && req.body;
      return { path: (req && req.body && req.body.path) || '/x', created: true };
    },
    'PUT /v1/drives/fx-ro-union/files': putOverride(w),
    'PUT /v1/drives/{drive}/files': putOverride(w),
  });
  await login(w, KEY_UNION);
  const unionCard = driveCard(w, 'fx-ro-union');
  ok(!!unionCard, 'F4 union drive card rendered');
  ok(findAllW(w, unionCard, (el) => String(el.className).includes('perm-g')).length > 0,
    'F4 card marks the unioned global permission chip (perm-g)');
  await enterDrive(w, 'fx-ro-union');
  // mkdir works purely through the unioned global write
  fire(w.E('mkdir-btn'), 'click');
  await w.settle();
  ok(w.E('dlg-mkdir').open === true, 'F4 global write unions in: mkdir dialog opens');
  w.E('mkdir-name').value = 'from-global';
  fire(w.E('mkdir-form'), 'submit');
  await pumpUntil(() => w.unionMkdir === 1, 20000, 'F4 union mkdir POST');
  await w.settle();
  ok(w.unionMkdirBody && w.unionMkdirBody.path === '/from-global', 'F4 union mkdir POST body {path:"/from-global"}, got ' + JSON.stringify(w.unionMkdirBody));
  ok(dialogIsClosed(w, 'dlg-mkdir'), 'F4 union mkdir dialog closes');
  // upload works purely through the unioned global write
  pushReceipts(w, uploadReceipt('/u-union.bin', 3));
  await queueUpload(w, [fakeFile('u-union.bin', 3)]);
  await w.settle();
  const unionPut = callsWhere(w, (r) => r.method === 'PUT');
  ok(unionPut.length === 1 && unionPut[0].drive === 'fx-ro-union', 'F4 global write unions in: PUT sent to the list/read drive');
  let stU = taskState(w, 'u-union.bin');
  ok(stU.found && stU.done, 'F4 union upload completes against a valid receipt');
  // destructive permissions are NOT unioned in
  const uRow = fileRow(w, 'u.txt');
  const uBtnOf = (label) => findControls(w, uRow, [label], {}).filter((b) => w.textOf(b).trim() === label);
  ok(uBtnOf('重命名').length === 1 && !uBtnOf('重命名')[0].disabled, 'F4 rename enabled (write unioned in)');
  ok(uBtnOf('删除').length === 1 && uBtnOf('删除')[0].disabled === true, 'F4 delete NOT unioned in (global write ≠ delete)');
  ok(uBtnOf('分享').length === 1 && uBtnOf('分享')[0].disabled === true, 'F4 share NOT unioned in (global write ≠ link)');
  fire(uBtnOf('删除')[0], 'click');
  await w.settle();
  ok(callsWhere(w, (r) => r.op === 'delete').length === 0, 'F4 disabled delete sends no request');
  await backToDrives(w);
  /* global perms never make ungranted drives visible + no-grants state */
  await logout(w);
  resetMock(w);
  installOverrides(w, { 'GET /v1/me': meOverride(meEmpty()) });
  await login(w, KEY_EMPTY);
  ok(w.E('drives-home').hidden === false, 'F4 no-grants key still lands on 我的网盘');
  ok(findAllW(w, w.E('drive-grid'), (el) => String(el.className).includes('drive-card')).length === 0,
    'F4 global perms alone never render a drive card');
  ok(w.textOf(w.E('drives-home')).includes('没有已授权的网盘') || w.textOf(w.E('drives-sub')).includes('没有授权任何网盘'),
    'F4 empty grant state is explained, got: ' + JSON.stringify(w.textOf(w.E('drives-home')).slice(0, 140)));

  /* ---- back to main key for the rest of the session ---- */
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': listOverride(w, {
      '/': listDoc('/', [entry('docs', true), entry('photos', true), entry('readme.txt', false, 12)]),
      '/docs': listDoc('/docs', [entry('docs/alpha.txt', false, 1), entry('docs/' + QUERY_HOSTILE_NAME, false, 2), entry('docs/' + XSS_NAME, false, 3), entry('docs/' + TRAV_NAME, false, 4), entry('docs/' + UNI_NAME, false, 5)]),
      '/photos': listDoc('/photos', [entry('photos/pic.png', false, 9)]),
    }),
  });
  await login(w, KEY_MAIN);

  /* ---- F5: broken drive surfaces list errors ---- */
  section('F5 broken drive');
  installOverrides(w, { 'GET /v1/drives/fx-broken/list': async () => { throw mkErr(503, 'drive_unavailable', 'BROKEN-DRIVE-SENTINEL storage replaced'); } });
  await enterDrive(w, 'fx-broken');
  await w.settle();
  ok(wsText(w).includes('BROKEN-DRIVE-SENTINEL') || /重新授权|不可用|失败|错误/.test(wsText(w)), 'F5 broken drive list error surfaced to the user');
  ok(w.E('gate').hidden === true, 'F5 a 503 list never kicks the session to the gate');
  await backToDrives(w);

  /* ---- F6: folder navigation + breadcrumb ---- */
  section('F6 navigation and breadcrumb');
  await enterDrive(w, 'fx-alpha');
  await listed(w, 'readme.txt', 'F6 root listing');
  ok(w.visible !== null && w.textOf(w.E('breadcrumb')).length > 0, 'F6 breadcrumb rendered');
  ok(crumbs(w).length >= 1 && crumbs(w)[0].current, 'F6 root crumb is marked current at /');
  const docRow = fileRow(w, 'docs');
  ok(!!docRow, 'F6 docs directory row rendered');
  const nameBtn = findAllW(w, docRow, (el) => String(el.className).includes('fx-namebtn') || (el.tagName === 'button' && w.textOf(el).includes('docs')))[0];
  fire(nameBtn || docRow, 'click');
  await pumpUntil(() => callsWhere(w, (r) => r.op === 'list' && r.pathParam === '/docs').length > 0, 20000, 'F6 list /docs');
  await w.settle();
  const cs = crumbs(w);
  ok(cs.length >= 2 && cs[cs.length - 1].text.includes('docs') && cs[cs.length - 1].current, 'F6 breadcrumb shows docs as current');
  ok(crumbs(w)[0].text.includes('AlphaRoot') || crumbs(w)[0].text.includes('fx-alpha') || crumbs(w)[0].text.includes('我的网盘') || /根|首页|home/i.test(crumbs(w)[0].text),
    'F6 breadcrumb root names the grant root, got: ' + JSON.stringify(cs.map((c) => c.text)));
  ok(w.textOf(w.E('file-list')).includes(UNI_NAME), 'F6 docs listing shows the unicode file');
  /* breadcrumb back to root */
  fire(cs[0].el, 'click');
  await pumpUntil(() => callsWhere(w, (r) => r.op === 'list' && (r.pathParam === '/' || r.pathParam === '')).length >= 1, 20000, 'F6 back to root');
  await w.settle();
  ok(w.textOf(w.E('file-list')).includes('readme.txt'), 'F6 root listing restored via breadcrumb');
  /* landing return */
  await backToDrives(w);
  ok(w.E('drives-home').hidden === false && w.E('browser-view').hidden === true, 'F6 ← 我的网盘 returns to the landing');
  await enterDrive(w, 'fx-alpha');

  /* ---- F6b: navigation-path hints stay in sync (loading / failure / return /
   * drive-switch / stale response) ---- */
  section('F6b navigation path-hint sync');
  /* Deterministic root first: re-enter with a fresh override set. */
  await backToDrives(w);
  const docsHold = { promise: null, resolve: null, reject: null };
  docsHold.promise = new Promise((res, rej) => { docsHold.resolve = res; docsHold.reject = rej; });
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': listOverride(w, {
      '/': listDoc('/', [entry('docs', true), entry('photos', true), entry('readme.txt', false, 12)]),
      '/docs': { deferred: docsHold },                                  // held in flight
      '/photos': { throw: mkErr(503, 'drive_unavailable', 'NAV-SYNC-503 upstream down') },
    }),
    'GET /v1/drives/fx-readonly/list': listOverride(w, { '/': listDoc('/', [entry('vault2.txt', false, 5)]) }),
  });
  await enterDrive(w, 'fx-alpha');
  await listed(w, 'readme.txt', 'F6b root listing');

  /* (1) Entering docs with the list HELD: breadcrumb + dropzone hint follow the
   * NEW path immediately, the old rows are gone, the pager reports loading and
   * the previous total is never shown as the current directory's. */
  const docsRow6b = fileRow(w, 'docs');
  const docsBtn6b = findAllW(w, docsRow6b, (el) => String(el.className).includes('fx-namebtn') || (el.tagName === 'button' && w.textOf(el).includes('docs')))[0];
  fire(docsBtn6b || docsRow6b, 'click');
  const loadCrumbs = crumbs(w);
  ok(loadCrumbs.length >= 2 && loadCrumbs[loadCrumbs.length - 1].text === 'docs' && loadCrumbs[loadCrumbs.length - 1].current === true,
    'F6b breadcrumb switches to the target dir synchronously while its list is loading, got ' + JSON.stringify(loadCrumbs.map((c) => c.text)));
  ok(w.textOf(w.E('dz-text')).includes('/docs'), 'F6b upload hint follows the target dir while loading, got ' + JSON.stringify(w.textOf(w.E('dz-text'))));
  ok(/正在读取/.test(w.textOf(w.E('file-list'))), 'F6b table shows a loading state while the list is in flight');
  ok(!w.textOf(w.E('file-list')).includes('readme.txt'), 'F6b old directory rows are not shown as the new dir while loading');
  ok(/正在读取/.test(w.textOf(w.E('page-info'))) && !/共 \d+ 项/.test(w.textOf(w.E('page-info'))),
    'F6b pager reports loading and hides the stale total, got ' + JSON.stringify(w.textOf(w.E('page-info'))));
  ok(w.E('page-next').disabled === true && w.E('page-prev').disabled === true, 'F6b pager is inert while loading');
  await w.settle();
  ok(/正在读取/.test(w.textOf(w.E('page-info'))), 'F6b held response keeps the loading state (no stale total)');

  /* (2) Navigate back to root BEFORE the held docs response lands: the late
   * response must not repaint (stale-list isolation via navGen). */
  fire(crumbs(w)[0].el, 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('readme.txt'), 20000, 'F6b return root while docs held');
  ok(crumbs(w).length >= 1 && crumbs(w)[0].current === true, 'F6b root crumb is current again after returning');
  docsHold.resolve(listDoc('/docs', [entry('docs/LATE-DOCS.txt', false, 1)]));
  await w.settle();
  ok(!w.textOf(w.E('file-list')).includes('LATE-DOCS.txt'), 'F6b a late list response for an abandoned dir never renders');
  ok(w.textOf(w.E('file-list')).includes('readme.txt'), 'F6b current root listing intact after the stale resolution');
  ok(w.textOf(w.E('page-info')).indexOf('共 3 项') >= 0, 'F6b pager keeps the current root total (3) after the stale resolution, got ' + JSON.stringify(w.textOf(w.E('page-info'))));

  /* (3) A FAILING directory: the hint stays on the failing dir, no stale total
   * or stale rows masquerade as the current result, and the pager is inert. */
  const photosRow6b = fileRow(w, 'photos');
  const photosBtn6b = findAllW(w, photosRow6b, (el) => String(el.className).includes('fx-namebtn') || (el.tagName === 'button' && w.textOf(el).includes('photos')))[0];
  fire(photosBtn6b || photosRow6b, 'click');
  ok(crumbs(w)[crumbs(w).length - 1].text === 'photos' && crumbs(w)[crumbs(w).length - 1].current === true,
    'F6b breadcrumb follows the failing dir while it loads');
  await w.settle();
  ok(crumbs(w)[crumbs(w).length - 1].text === 'photos' && crumbs(w)[crumbs(w).length - 1].current === true,
    'F6b breadcrumb still names the failing dir after the error lands');
  ok(w.textOf(w.E('dz-text')).includes('/photos'), 'F6b upload hint stays on the failing dir');
  ok(!w.textOf(w.E('file-list')).includes('readme.txt'), 'F6b a failed load never keeps the previous directory rows');
  ok(!/共 \d+ 项/.test(w.textOf(w.E('page-info'))), 'F6b a failed load never keeps the previous (stale) total, got ' + JSON.stringify(w.textOf(w.E('page-info'))));
  ok(/统计不可用|失败/.test(w.textOf(w.E('page-info'))), 'F6b pager states the statistics are unavailable on failure, got ' + JSON.stringify(w.textOf(w.E('page-info'))));
  ok(w.E('page-next').disabled === true && w.E('page-prev').disabled === true, 'F6b pager is inert after a failed load');
  ok(/失败|不可用|错误/.test(w.textOf(w.E('file-list'))), 'F6b the failure is explained in place');

  /* (4) Return to root from the failed dir: hints + listing recover. */
  fire(crumbs(w)[0].el, 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('readme.txt') && /共 3 项/.test(w.textOf(w.E('page-info'))), 20000, 'F6b recover to root');
  ok(crumbs(w).length === 1 && crumbs(w)[0].current === true, 'F6b returning to root collapses the breadcrumb to the root crumb, got ' + JSON.stringify(crumbs(w).map((c) => c.text)));
  ok(w.textOf(w.E('dz-text')).includes('/') && !w.textOf(w.E('dz-text')).includes('/photos'), 'F6b upload hint follows back to root');

  /* (5) Drive SWITCH: all path hints move to the new drive with no fx-alpha
   * leftovers. */
  await enterDrive(w, 'fx-readonly');
  await listed(w, 'vault2.txt', 'F6b readonly listing');
  const roCrumbs6b = crumbs(w);
  ok(roCrumbs6b.length >= 1 && w.textOf(w.E('breadcrumb')).includes('fx-readonly'), 'F6b breadcrumb names the switched-to drive');
  ok(!w.textOf(w.E('breadcrumb')).includes('fx-alpha'), 'F6b no previous-drive name lingers in the breadcrumb');
  ok(!w.textOf(w.E('dz-text')).includes('fx-alpha'), 'F6b the previous drive name is gone from the upload hint after the switch, got ' + JSON.stringify(w.textOf(w.E('dz-text'))));
  ok(/只读/.test(w.textOf(w.E('dz-title'))), 'F6b the read-only drive dropzone reflects its own write state');
  ok(!w.textOf(w.E('file-list')).includes('readme.txt'), 'F6b the previous drive listing is replaced on switch');
  /* Switching BACK to the writable drive re-targets the hint at the new drive
   * and path (no cached fx-readonly text). */
  await enterDrive(w, 'fx-alpha');
  await listed(w, 'readme.txt', 'F6b back to fx-alpha');
  ok(w.textOf(w.E('dz-text')).includes('fx-alpha'), 'F6b the upload hint re-targets the switched-back drive, got ' + JSON.stringify(w.textOf(w.E('dz-text'))));
  ok(!w.textOf(w.E('dz-text')).includes('fx-readonly'), 'F6b no fx-readonly hint text lingers on the writable drive');

  /* ---- F7: pagination + current-page filter ---- */
  section('F7 pagination and current-page filter');
  const page1 = listDoc('/', ['p1-a', 'p1-b', 'p1-c', 'p1-d', 'p1-e'].map((n) => entry(n, false, 1)), 12, 1, 5);
  const page2 = listDoc('/', ['p2-f', 'p2-g', 'p2-h', 'p2-i', 'p2-j'].map((n) => entry(n, false, 2)), 12, 2, 5);
  const page3 = listDoc('/', ['p3-k', 'p3-l'].map((n) => entry(n, false, 3)), 12, 3, 5);
  const pageDocs = { 1: page1, 2: page2, 3: page3 };
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': async (...args) => {
      const req = peekReq(w, args, 'GET', '/v1/drives/');
      const page = req && req.page ? req.page : 1;
      return JSON.parse(JSON.stringify(pageDocs[page] || pageDocs[1]));
    },
  });
  fire(w.E('refresh-btn'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('p1-a'), 20000, 'F7 page1');
  ok(w.E('page-next').disabled === false, 'F7 next page enabled when total (12) > page size (5)');
  ok(/第 1 \/ 3 页/.test(w.textOf(w.E('page-info'))), 'F7 page info computes 3 pages from server per_page, got: ' + w.textOf(w.E('page-info')));
  fire(w.E('page-next'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('p2-f') && callsWhere(w, (r) => r.op === 'list' && r.page === 2).length > 0, 20000, 'F7 page2');
  ok(!w.textOf(w.E('file-list')).includes('p1-a'), 'F7 page 1 rows replaced by page 2 rows');
  ok(/第 2 \/ 3 页/.test(w.textOf(w.E('page-info'))), 'F7 page info shows page 2');
  fire(w.E('page-prev'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('p1-a') && callsWhere(w, (r) => r.op === 'list' && r.page === 1).length >= 1, 20000, 'F7 back to page1');
  ok(w.textOf(w.E('file-list')).includes('p1-e') && !w.textOf(w.E('file-list')).includes('p2-f'), 'F7 page 1 restored');
  /* current-page-only filter: no refetch */
  const callsBeforeFilter = (w.mock.calls || []).length;
  w.E('page-search').value = 'p1-b';
  fire(w.E('page-search'), 'input');
  await w.settle();
  const namesAfter = visibleRowNames(w);
  ok(namesAfter.join('\n').includes('p1-b') && !namesAfter.some((t) => t.includes('p1-a')), 'F7 filter narrows rows to matches only');
  ok((w.mock.calls || []).length === callsBeforeFilter, 'F7 filtering never refetches (current-page only, not a global search)');
  ok(/当前页搜索/.test(w.textOf(w.E('page-info'))), 'F7 page info labels the narrowed count as current-page search');
  w.E('page-search').value = 'zzz-nothing';
  fire(w.E('page-search'), 'input');
  await w.settle();
  ok(/当前页没有匹配/.test(w.textOf(w.E('file-list'))), 'F7 no-match filter shows an explained empty state');
  w.E('page-search').value = '';
  fire(w.E('page-search'), 'input');
  await w.settle();
  ok(w.textOf(w.E('file-list')).includes('p1-a'), 'F7 clearing the filter restores all page rows');
  fire(w.E('page-next'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('p2-f'), 20000, 'F7 next again');
  fire(w.E('page-next'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('p3-k'), 20000, 'F7 last page');
  ok(w.E('page-next').disabled === true, 'F7 next disabled on the last page');
  fire(w.E('page-prev'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('p2-f'), 20000, 'F7 settle on page2');

  /* ---- F8/F10-hostile: text-only hostile names ---- */
  section('F8 hostile names rendered as text');
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': listOverride(w, {
      '/': listDoc('/', [entry('docs', true)]),
      '/docs': listDoc('/docs', [entry('docs/' + XSS_NAME, false, 1), entry('docs/' + QUERY_HOSTILE_NAME, false, 2), entry('docs/' + TRAV_NAME, false, 3)]),
    }),
  });
  fire(w.E('back-drives'), 'click');
  await w.settle();
  await enterDrive(w, 'fx-alpha');
  await listed(w, 'docs', 'F8 root listing');
  const docsRow2 = fileRow(w, 'docs');
  const docsBtn2 = findAllW(w, docsRow2, (el) => String(el.className).includes('fx-namebtn') || (el.tagName === 'button' && w.textOf(el).includes('docs')))[0];
  fire(docsBtn2 || docsRow2, 'click');
  await pumpUntil(() => callsWhere(w, (r) => r.op === 'list' && r.pathParam === '/docs').length > 0, 20000, 'F8 docs');
  await w.settle();
  const listText = w.textOf(w.E('file-list'));
  ok(listText.includes(XSS_NAME), 'F8 XSS name rendered verbatim as text');
  ok(findAllW(w, w.E('file-list'), (el) => el.tagName === 'img' || el.tagName === 'script').length === 0, 'F8 hostile name created no img/script nodes');
  ok(listText.includes(QUERY_HOSTILE_NAME) && listText.includes(TRAV_NAME), 'F8 query-hostile and traversal-looking names rendered verbatim');
  const rawNav = (w.mock.calls || []).filter((c) => c.method === 'GET' && String(c.path).indexOf('/list') >= 0).pop();
  ok(!!rawNav && String(rawNav.query).indexOf(encodeURIComponent('/docs')) >= 0,
    'F8 navigation request percent-encodes the path query, got: ' + (rawNav && rawNav.query));
  ok(!w.innerHTMLLog.some((h) => h.value.includes('<img') || h.value.includes(XSS_NAME) || h.value.includes(QUERY_HOSTILE_NAME)),
    'F8 hostile names never flow through innerHTML');
  ok(global.__FXPWNED === undefined, 'F8 hostile name never executed');

  /* ---- F9: mkdir dialog ---- */
  section('F9 mkdir');
  let mkdirBodies = [];
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': listOverride(w, {
      '/docs': listDoc('/docs', [entry('docs/alpha.txt', false, 1)]),
      '*': listDoc('/', [entry('docs', true)]),
    }),
    'POST /v1/drives/fx-alpha/mkdir': async (...args) => {
      const req = peekReq(w, args, 'POST', '/v1/drives/');
      mkdirBodies.push(req && req.body);
      return { path: (req && req.body && req.body.path) || '/x', created: true };
    },
  });
  fire(w.E('mkdir-btn'), 'click');
  await w.settle();
  ok(w.E('dlg-mkdir').open === true, 'F9 mkdir dialog opens');
  ok(w.textOf(w.E('mkdir-where')).includes('docs') || w.textOf(w.E('mkdir-where')).includes('/'), 'F9 mkdir dialog names the target directory');
  fire(w.E('mkdir-form'), 'submit'); // empty name
  await w.settle();
  ok(mkdirBodies.length === 0, 'F9 empty folder name issues no request');
  ok(w.E('mkdir-err').textContent.trim() !== '' || w.textOf(w.E('dlg-mkdir')).trim().length > 0, 'F9 empty name explained inline');
  w.E('mkdir-name').value = 'newdir';
  fire(w.E('mkdir-form'), 'submit');
  await pumpUntil(() => mkdirBodies.length === 1, 20000, 'F9 mkdir POST');
  await w.settle();
  ok(mkdirBodies[0] && mkdirBodies[0].path === '/docs/newdir', 'F9 mkdir POST body {path:"/docs/newdir"}, got ' + JSON.stringify(mkdirBodies[0]));
  ok(dialogIsClosed(w, 'dlg-mkdir'), 'F9 dialog closed after success');
  fire(w.E('mkdir-btn'), 'click');
  await w.settle();
  ok(w.E('dlg-mkdir').open === true && w.E('mkdir-name').value === '', 'F9 reopened mkdir dialog starts with a clean name');
  w.E('mkdir-name').value = XSS_NAME;
  fire(w.E('mkdir-form'), 'submit');
  await pumpUntil(() => mkdirBodies.length === 2, 20000, 'F9 hostile mkdir POST');
  await w.settle();
  ok(dialogIsClosed(w, 'dlg-mkdir'), 'F9 hostile (but legal) name accepted server-side');

  /* ---- F10: rename → move ---- */
  section('F10 rename');
  let moveBodies = [];
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/docs': listDoc('/docs', [entry('docs/alpha.txt', false, 1)]) }),
    'POST /v1/drives/fx-alpha/move': async (...args) => {
      const req = peekReq(w, args, 'POST', '/v1/drives/');
      moveBodies.push(req && req.body);
      return { from: (req && req.body && req.body.from) || '/x', to: (req && req.body && req.body.to) || '/y' };
    },
  });
  fire(w.E('refresh-btn'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('alpha.txt'), 20000, 'F10 refresh docs');
  const aRow = fileRow(w, 'alpha.txt');
  fire(findControls(w, aRow, ['重命名'], { armedOnly: true })[0] || aRow, 'click');
  await w.settle();
  ok(w.E('dlg-rename').open === true, 'F10 rename dialog opens');
  ok(w.textOf(w.E('rename-old')).includes('alpha.txt'), 'F10 rename dialog shows the old name');
  w.E('rename-name').value = 'alpha2.txt';
  fire(w.E('rename-form'), 'submit');
  await pumpUntil(() => moveBodies.length === 1, 20000, 'F10 move POST');
  await w.settle();
  ok(moveBodies[0] && moveBodies[0].from === '/docs/alpha.txt' && moveBodies[0].to === '/docs/alpha2.txt',
    'F10 rename POSTs move {from:"/docs/alpha.txt",to:"/docs/alpha2.txt"}, got ' + JSON.stringify(moveBodies[0]));
  ok(dialogIsClosed(w, 'dlg-rename'), 'F10 rename dialog closes');

  /* ---- F11: delete confirm / cancel / per-item errors ---- */
  section('F11 delete');
  let deleteBodies = [];
  let deleteDoc = { results: [] };
  let docsAfterDelete = listDoc('/docs', [entry('docs/keep.txt', false, 1), entry('docs/gone.txt', false, 2), entry('docs/stuck.txt', false, 3)]);
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': listOverride(w, {
      get '/docs'() { return docsAfterDelete; },
    }),
    'POST /v1/drives/fx-alpha/delete': async (...args) => {
      const req = peekReq(w, args, 'POST', '/v1/drives/');
      deleteBodies.push(req && req.body);
      return deleteDoc;
    },
  });
  fire(w.E('refresh-btn'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('gone.txt'), 20000, 'F11 refresh');
  const delRow = fileRow(w, 'gone.txt');
  fire(findControls(w, delRow, ['删除'], { armedOnly: true })[0] || delRow, 'click');
  await w.settle();
  ok(w.E('dlg-delete').open === true, 'F11 delete opens a confirmation dialog');
  ok(w.textOf(w.E('delete-path')).includes('gone.txt'), 'F11 confirm dialog names the path');
  ok(deleteBodies.length === 0, 'F11 no delete request before confirmation');
  /* cancel → nothing happens */
  const cancelBtn = findControls(w, w.E('dlg-delete'), ['取消', '关闭'], { armedOnly: true })[0];
  fire(cancelBtn || w.E('dlg-delete'), 'click');
  await w.settle();
  ok(dialogIsClosed(w, 'dlg-delete') && deleteBodies.length === 0, 'F11 cancelling confirms nothing and sends nothing');
  /* confirm → one delete request with the exact path */
  fire(findControls(w, fileRow(w, 'gone.txt'), ['删除'], { armedOnly: true })[0], 'click');
  await w.settle();
  deleteDoc = { results: [{ path: '/docs/gone.txt', status: 'trashed' }] };
  docsAfterDelete = listDoc('/docs', [entry('docs/keep.txt', false, 1), entry('docs/stuck.txt', false, 3)]);
  fire(w.E('delete-ok'), 'click');
  await pumpUntil(() => deleteBodies.length === 1, 20000, 'F11 delete POST');
  await w.settle();
  ok(deleteBodies[0] && JSON.stringify(deleteBodies[0].paths || deleteBodies[0].path) === JSON.stringify(['/docs/gone.txt']),
    'F11 delete POST carries paths:["/docs/gone.txt"], got ' + JSON.stringify(deleteBodies[0]));
  ok(dialogIsClosed(w, 'dlg-delete'), 'F11 delete dialog closes after success');
  await pumpUntil(() => !fileRow(w, 'gone.txt'), 20000, 'F11 row refresh');
  ok(!fileRow(w, 'gone.txt'), 'F11 deleted item leaves the refreshed listing');
  /* per-item error must not read as success */
  deleteBodies.length = 0;
  deleteDoc = { results: [{ path: '/docs/stuck.txt', status: 'error', error: 'STUCK-DELETE-SENTINEL upstream refused' }] };
  fire(findControls(w, fileRow(w, 'stuck.txt'), ['删除'], { armedOnly: true })[0], 'click');
  await w.settle();
  fire(w.E('delete-ok'), 'click');
  await pumpUntil(() => deleteBodies.length === 1, 20000, 'F11 stuck delete POST');
  await w.settle();
  ok(wsText(w).includes('STUCK-DELETE-SENTINEL') || /失败|错误|未删除/.test(wsText(w)), 'F11 per-item delete error is surfaced, got: ' + JSON.stringify(wsText(w).slice(0, 200)));
  ok(!/删除成功|已删除|已移入回收站/.test(w.textOf(w.E('notice'))) || wsText(w).includes('STUCK-DELETE-SENTINEL'), 'F11 an errored delete is never announced as success');
  ok(dialogIsClosed(w, 'dlg-delete') || w.textOf(w.E('delete-err')).includes('STUCK'), 'F11 errored delete keeps or reopens an explanatory state');
  await w.settle();
  ok(!!fileRow(w, 'stuck.txt'), 'F11 the errored item is still listed after the refresh');

  /* ---- F12: share dialog ---- */
  section('F12 share');
  let linkBodies = [];
  let linkDoc = { url: 'http://127.0.0.1:18893/l/tok123', expires_at: '2026-10-21T00:00:00Z' };
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/docs': listDoc('/docs', [entry('docs/shareme.txt', false, 7)]) }),
    'POST /v1/links': async (...args) => {
      const req = peekReq(w, args, 'POST', '/v1/links');
      linkBodies.push(req && req.body);
      if (linkDoc.throw) throw linkDoc.throw;
      return linkDoc;
    },
  });
  fire(w.E('refresh-btn'), 'click');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('shareme.txt'), 20000, 'F12 refresh');
  fire(findControls(w, fileRow(w, 'shareme.txt'), ['分享', '创建链接', '链接'], { armedOnly: true })[0], 'click');
  await w.settle();
  ok(w.E('dlg-share').open === true, 'F12 share dialog opens from the file row');
  ok(w.textOf(w.E('share-what')).includes('shareme.txt'), 'F12 share dialog names the file');
  const dlgInputs = findAllW(w, w.E('dlg-share'), (el) => el.tagName === 'input');
  ok(dlgInputs.every((i) => i.attrs.type !== 'password'), 'F12 share has NO password input');
  ok(findControls(w, w.E('dlg-share'), ['密码', 'password']).length === 0, 'F12 share has no password field label');
  const SHORT_NOTICE = '持有链接的人可下载，有效期内无需登录。';
  ok(w.textOf(w.E('dlg-share')).includes(SHORT_NOTICE),
    'F12 share dialog carries the single short public notice, got: ' + JSON.stringify(w.textOf(w.E('dlg-share')).slice(0, 160)));
  ok(!/不支持密码或下载次数限制/.test(w.textOf(w.E('dlg-share'))),
    'F12 no verbose unsupported-capability (password/download-limit) disclaimer remains');
  const ttlOptions = findAllW(w, w.E('share-ttl'), (o) => o.tagName === 'option').map((o) => Number(o.attrs.value));
  ok(ttlOptions.length > 0 && ttlOptions.every((v) => v > 0 && v <= 604800), 'F12 every offered TTL is within 7 days: ' + ttlOptions.join(','));
  const shareFields = findAllW(w, w.E('dlg-share'), (el) => el.tagName === 'input' || el.tagName === 'select');
  ok(shareFields.every((f) => f.id === 'share-ttl' || f.id === 'share-url'),
    'F12 share has no password/download-limit inputs (only TTL + read-only URL), got ids: ' + shareFields.map((f) => f.id).join(','));
  w.E('share-ttl').value = '3600';
  fire(w.E('share-form'), 'submit');
  await pumpUntil(() => linkBodies.length === 1, 20000, 'F12 link POST');
  await w.settle();
  const lb = linkBodies[0] || {};
  ok(lb.path === '/docs/shareme.txt', 'F12 POST /v1/links body carries the exact path, got ' + JSON.stringify(lb));
  ok(lb.expires_in === 3600 || lb.ttl === 3600 || lb.expires_in == null, 'F12 expires_in matches the selected TTL (3600s)');
  ok(!(Number(lb.expires_in || lb.ttl || 0) > 604800), 'F12 never requests more than 604800s');
  ok(w.E('share-result').hidden === false && String(w.E('share-url').value).includes('/l/tok123'), 'F12 server URL + expiry rendered');
  ok(/到期/.test(w.textOf(w.E('share-exp'))), 'F12 expiry shown with the link');
  ok(/无需登录|持有链接/.test(w.textOf(w.E('dlg-share'))), 'F12 public-access notice present with the result, got: ' + JSON.stringify(w.textOf(w.E('dlg-share')).slice(0, 160)));
  /* Copy: the ACTUAL copied text must equal the share URL, and the button label
   * must show the success state — no dependence on a global notice. */
  const shareUrlVal = String(w.E('share-url').value);
  w.copiedLog.length = 0; w.clipboardLog.length = 0; w.execLog.length = 0;
  fire(w.E('share-copy'), 'click');
  await pumpUntil(() => copiedEquals(w, shareUrlVal), 20000, 'F12 copy resolves');
  await w.settle();
  ok(copiedEquals(w, shareUrlVal) && lastCopied(w).via === 'writeText',
    'F12 secure Clipboard API copies EXACTLY the share URL (' + JSON.stringify(shareUrlVal) + '), got ' + JSON.stringify(copiedTexts(w)));
  ok(shareCopySaysCopied(w), 'F12 #share-copy shows 已复制 after a successful copy, got ' + JSON.stringify(shareCopyLabel(w)));
  ok(w.textOf(w.E('share-err')).trim() === '', 'F12 a successful copy leaves no inline error');
  /* hostile server URLs must not become actionable links */
  for (const hostile of ['javascript:alert(1)//x', 'http://user:pass@127.0.0.1:1/l/cred', 'data:text/html,<script>1</script>']) {
    linkDoc = { url: hostile, expires_at: '2026-10-21T00:00:00Z' };
    w.E('share-url').value = '';
    w.E('share-result').hidden = true;
    fire(w.E('share-form'), 'submit');
    await pumpUntil(() => linkBodies.length >= 2, 20000, 'F12 hostile link');
    await w.settle();
    const anchors = findAllW(w, w.E('dlg-share'), (el) => el.tagName === 'a');
    const hrefs = anchors.map((a) => String(a.attrs.href || ''));
    ok(!hrefs.some((h) => h.startsWith('javascript:') || h.includes('user:pass@') || h.startsWith('data:')),
      'F12 hostile URL never becomes a clickable anchor (' + hostile.slice(0, 22) + '…), hrefs=' + JSON.stringify(hrefs));
    ok(!String(w.E('share-url').value).startsWith('javascript:') && !String(w.E('share-url').value).includes('user:pass@'),
      'F12 hostile URL not handed to the copyable result field');
  }
  const closeShare = findControls(w, w.E('dlg-share'), ['关闭', '取消'], { armedOnly: true })[0];
  fire(closeShare || w.E('dlg-share'), 'click');
  await w.settle();
  ok(dialogIsClosed(w, 'dlg-share'), 'F12 share dialog closes');

  /* F12b: a slow share response must never fill a LATER dialog for the file */
  const shareDef = { promise: null, resolve: null };
  shareDef.promise = new Promise((r) => { shareDef.resolve = r; });
  linkDoc = shareDef.promise; // dispatcher awaits: response held in flight
  fire(findControls(w, fileRow(w, 'shareme.txt'), ['分享'], { armedOnly: true })[0], 'click');
  await w.settle();
  fire(w.E('share-form'), 'submit');
  await w.settle();
  const closeInFlight = findControls(w, w.E('dlg-share'), ['关闭', '取消'], { armedOnly: true })[0];
  fire(closeInFlight || w.E('dlg-share'), 'click');
  await w.settle();
  ok(dialogIsClosed(w, 'dlg-share'), 'F12b dialog closes while its request is in flight');
  fire(findControls(w, fileRow(w, 'shareme.txt'), ['分享'], { armedOnly: true })[0], 'click');
  await w.settle();
  ok(w.E('share-result').hidden === true && String(w.E('share-url').value) === '', 'F12b reopened dialog starts clean');
  shareDef.resolve({ url: 'http://127.0.0.1:18893/l/STALE-OLD-FILE', expires_at: '2026-10-21T00:00:00Z' });
  await w.settle();
  ok(!String(w.E('share-url').value).includes('STALE-OLD-FILE'), 'F12b stale response never fills the reopened dialog, got: ' + JSON.stringify(String(w.E('share-url').value)));
  ok(w.E('share-result').hidden === true, 'F12b stale response does not reveal the old result block');
  const closeShare2 = findControls(w, w.E('dlg-share'), ['关闭', '取消'], { armedOnly: true })[0];
  fire(closeShare2 || w.E('dlg-share'), 'click');
  await w.settle();

  /* ---- F13: upload queue semantics (mock mode) ---- */
  section('F13 upload receipts');
  let putCalls = [];
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': listOverride(w, {
      '/': listDoc('/', [entry('docs', true)]),
      '/docs': listDoc('/docs', [entry('docs/anchor.txt', false, 1)]),
    }),
    'PUT /v1/drives/fx-alpha/files': putOverride(w),
    'PUT /v1/drives/{drive}/files': putOverride(w),
    'POST /v1/links': async () => { throw mkErr(500, 'harness', 'uploads must never auto-share'); },
  });
  fire(w.E('back-drives'), 'click');
  await w.settle();
  await enterDrive(w, 'fx-alpha');
  await listed(w, 'docs', 'F13 root listing');
  const docsRow3 = fileRow(w, 'docs');
  const docsBtn3 = findAllW(w, docsRow3, (el) => String(el.className).includes('fx-namebtn') || (el.tagName === 'button' && w.textOf(el).includes('docs')))[0];
  fire(docsBtn3 || docsRow3, 'click');
  await pumpUntil(() => callsWhere(w, (r) => r.op === 'list' && r.pathParam === '/docs').length > 0, 20000, 'F13 into docs');
  await listed(w, 'anchor.txt', 'F13 docs listing');
  putCalls = () => callsWhere(w, (r) => (r.op === 'files' || r.method === 'PUT'));

  /* F13a: valid confirmed receipt → done */
  pushReceipts(w, uploadReceipt('/docs/up-a.bin', 3));
  await queueUpload(w, [fakeFile('up-a.bin', 3)]);
  await w.settle();
  ok(putCalls().length === 1 && putCalls()[0].pathParam === '/docs/up-a.bin', 'F13a PUT requested the exact captured path, got ' + JSON.stringify(putCalls().map((p) => p.pathParam)));
  ok(putCalls()[0].onConflict === null || ['fail', 'rename', 'overwrite'].includes(putCalls()[0].onConflict), 'F13a on_conflict query is a legal value');
  let st = taskState(w, 'up-a.bin');
  const linkCallsBefore = callsWhere(w, (r) => r.path === '/v1/links').length;
  ok(st.found && st.done && !st.failed && !st.interrupted, 'F13a valid upstream-confirmed receipt renders 完成, got: ' + JSON.stringify(st.text));
  ok(callsWhere(w, (r) => r.path === '/v1/links').length === linkCallsBefore, 'F13a uploading never auto-creates a public link');

  /* F13b: 200-with-incomplete-receipt (missing crc64/sha256) stays not-done */
  pushReceipts(w, { path: '/docs/up-b.bin', size: 4 });
  await queueUpload(w, [fakeFile('up-b.bin', 4)]);
  await w.settle();
  st = taskState(w, 'up-b.bin');
  ok(st.found && !st.done, 'F13b receipt missing checksums never renders 完成, got: ' + JSON.stringify(st.text));

  /* F13b2: structurally invalid checksum shape (40-hex sha256) stays unconfirmed */
  pushReceipts(w, { path: '/docs/up-b2.bin', size: 4, crc64: '11051210869376104954', sha256: 'ab'.repeat(20), conflict: 'created' });
  await queueUpload(w, [fakeFile('up-b2.bin', 4)]);
  await w.settle();
  st = taskState(w, 'up-b2.bin');
  ok(st.found && !st.done, 'F13b2 40-hex sha256 receipt never renders 完成 (server contract is 64 hex), got: ' + JSON.stringify(st.text));

  /* F13c: valid JSON but wrong path → not the requested upload's completion */
  pushReceipts(w, uploadReceipt('/elsewhere/up-c.bin', 5));
  await queueUpload(w, [fakeFile('up-c.bin', 5)]);
  await w.settle();
  st = taskState(w, 'up-c.bin');
  ok(st.found && !st.done, 'F13c receipt for a DIFFERENT path never completes this task, got: ' + JSON.stringify(st.text));

  /* F13d: wrong size → not completed */
  pushReceipts(w, uploadReceipt('/docs/up-d.bin', 999));
  await queueUpload(w, [fakeFile('up-d.bin', 6)]);
  await w.settle();
  st = taskState(w, 'up-d.bin');
  ok(st.found && !st.done, 'F13d size-mismatched receipt never completes the task, got: ' + JSON.stringify(st.text));

  /* F13e: 500 error → failed with the server message */
  pushReceipts(w, { throw: mkErr(500, 'upstream', 'UPSTREAM-BOOM-SENTINEL') });
  await queueUpload(w, [fakeFile('up-e.bin', 7)]);
  await w.settle();
  st = taskState(w, 'up-e.bin');
  ok(st.found && st.failed && !st.done, 'F13e 5xx upload renders failed, got: ' + JSON.stringify(st.text));
  ok(w.textOf(w.E('upload-queue')).includes('UPSTREAM-BOOM-SENTINEL') || /失败|错误/.test(w.textOf(w.E('upload-queue'))), 'F13e server error text surfaced in the queue');

  /* F13f: 409 conflict (default on_conflict=fail) → explicit failure, no overwrite */
  pushReceipts(w, { throw: mkErr(409, 'already_exists', 'ALREADY-THERE-SENTINEL') });
  await queueUpload(w, [fakeFile('up-f.bin', 8)]);
  await w.settle();
  st = taskState(w, 'up-f.bin');
  ok(st.found && st.failed && !st.done, 'F13f duplicate-name upload (on_conflict=fail) fails loudly, got: ' + JSON.stringify(st.text));

  /* F13g: sequential queue — second upload starts only after the first settles */
  const deferredA = { promise: null, resolve: null };
  deferredA.promise = new Promise((r) => { deferredA.resolve = r; });
  pushReceipts(w, { deferred: deferredA });
  pushReceipts(w, uploadReceipt('/docs/up-2.bin', 2));
  const putsBeforeSeq = putCalls().length;
  await queueUpload(w, [fakeFile('up-1.bin', 1), fakeFile('up-2.bin', 2)]);
  await w.settle();
  ok(putCalls().length === putsBeforeSeq + 1, 'F13g queue is sequential: one PUT in flight, got ' + (putCalls().length - putsBeforeSeq));
  deferredA.resolve(uploadReceipt('/docs/up-1.bin', 1));
  await w.settle();
    ok(putCalls().length === putsBeforeSeq + 2, 'F13g second upload starts after the first settles, got ' + (putCalls().length - putsBeforeSeq));
  st = taskState(w, 'up-1.bin');
  ok(st.found && st.done, 'F13g first upload completes with its own receipt');
  st = taskState(w, 'up-2.bin');
  ok(st.found && st.done, 'F13g second upload completes with its own receipt');

  /* F13h: drop and paste paths */
  pushReceipts(w, uploadReceipt('/docs/up-drop.bin', 3));
  await queueUpload(w, [fakeFile('up-drop.bin', 3)], 'drop');
  await w.settle();
  st = taskState(w, 'up-drop.bin');
  ok(st.found && st.done && putCalls().some((p) => p.pathParam === '/docs/up-drop.bin'), 'F13h drop uploads land in the current directory');
  pushReceipts(w, uploadReceipt('/docs/up-paste.bin', 3));
  await queueUpload(w, [fakeFile('up-paste.bin', 3)], 'paste');
  await w.settle();
  st = taskState(w, 'up-paste.bin');
  ok(st.found && st.done && putCalls().some((p) => p.pathParam === '/docs/up-paste.bin'), 'F13h paste uploads land in the current directory');
  const allPuts = callsWhere(w, (r) => r.method === 'PUT');
  ok(allPuts.length >= 8 && allPuts.every((c) => c.hasKey === true),
    'F13 every recorded upload carries has_key (key rides only in the apikey header), checked ' + allPuts.length + ' PUTs');
  ok(ticketCalls(w).length === 0, 'F13 no upload (queue, drop or paste) ever auto-requests a download ticket');

  /* ---- F14: conflict-mode select + overwrite confirmation ---- */
  section('F14 conflict handling');
  w.E('conflict-mode').value = 'overwrite';
  fire(w.E('conflict-mode'), 'change');
  await w.settle();
  ok(w.E('overwrite-row').hidden === false, 'F14 choosing 覆盖 reveals the explicit overwrite confirmation');
  w.E('conflict-mode').value = 'rename';
  fire(w.E('conflict-mode'), 'change');
  await w.settle();
  ok(w.E('overwrite-row').hidden === true, 'F14 non-overwrite modes hide the overwrite confirmation again');
  w.E('conflict-mode').value = 'overwrite';
  fire(w.E('conflict-mode'), 'change');
  await w.settle();
  const putsBeforeOw = putCalls().length;
  w.E('file-input').files = [fakeFile('up-ow.bin', 3)];
  fire(w.E('file-input'), 'change');
  await w.settle();
  ok(taskEl(w, 'up-ow.bin') === null && putCalls().length === putsBeforeOw,
    'F14 overwrite without explicit confirmation enqueues nothing and sends nothing');
  ok(/确认要覆盖|覆盖/.test(w.textOf(w.E('notice'))) && w.E('notice').hidden === false, 'F14 blocked overwrite is explained');
  w.E('overwrite-ok').checked = true;
  fire(w.E('overwrite-ok'), 'change');
  pushReceipts(w, uploadReceipt('/docs/up-ow2.bin', 3));
  await queueUpload(w, [fakeFile('up-ow2.bin', 3)]);
  await w.settle();
  const owPut = putCalls().find((p) => p.pathParam === '/docs/up-ow2.bin');
  ok(!!owPut && owPut.onConflict === 'overwrite', 'F14 confirmed overwrite uploads with on_conflict=overwrite');
  w.E('conflict-mode').value = 'fail';
  fire(w.E('conflict-mode'), 'change');
  await w.settle();
  /* F14b: dropzone is a role=group — only the explicit picker entry opens it */
  let pickerOpens = 0;
  w.E('file-input').addEventListener('click', () => { pickerOpens++; });
  fire(w.E('conflict-mode'), 'click'); // control inside the group: guarded by closest(...)
  fire(w.E('choose-files'), 'click');  // the explicit picker entry
  await w.settle();
  ok(pickerOpens === 1, 'F14b 选择文件 opens the picker exactly once and the conflict-mode click opens none, got ' + pickerOpens);
  fire(w.E('dz-text'), 'click'); // plain dropzone surface still opens the picker
  await w.settle();
  ok(pickerOpens === 2, 'F14b clicking the plain dropzone surface opens the picker, got ' + pickerOpens);

  /* ---- F15: captured destination across navigation ---- */
  section('F15 captured destination');
  const deferredUp = { promise: null, resolve: null };
  deferredUp.promise = new Promise((r) => { deferredUp.resolve = r; });
  pushReceipts(w, { deferred: deferredUp });
  await queueUpload(w, [fakeFile('up-move.bin', 3)]);
  await w.settle();
  const putIndex = putCalls().length;
  ok(putIndex >= 1 && putCalls()[putIndex - 1].pathParam === '/docs/up-move.bin', 'F15 PUT targeted /docs while still there');
  /* navigate away while the upload is in flight */
  installOverrides(w, {
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/': listDoc('/', [entry('docs', true), entry('photos', true)]), '/photos': listDoc('/photos', [entry('photos/pic.png', false, 9)]) }),
    'PUT /v1/drives/fx-alpha/files': putOverride(w),
    'PUT /v1/drives/{drive}/files': putOverride(w),
  });
  fire(w.E('back-drives'), 'click');
  await w.settle();
  await enterDrive(w, 'fx-alpha');
  await listed(w, 'photos', 'F15 root listing');
  const photosRow = fileRow(w, 'photos');
  const photosBtn = findAllW(w, photosRow, (el) => String(el.className).includes('fx-namebtn'))[0];
  fire(photosBtn || photosRow, 'click');
  await pumpUntil(() => callsWhere(w, (r) => r.op === 'list' && r.pathParam === '/photos').length > 0, 20000, 'F15 into photos');
  await w.settle();
  deferredUp.resolve(uploadReceipt('/docs/up-move.bin', 3));
  await w.settle();
  ok(putCalls().length === putIndex && putCalls().every((p) => p.pathParam !== '/photos/up-move.bin'),
    'F15 navigation never re-targets the in-flight upload');
  st = taskState(w, 'up-move.bin');
  ok(st.found && st.done, 'F15 the upload still completes against its captured destination');
  ok(w.textOf(taskEl(w, 'up-move.bin') || w.mkEl('div')).includes('/docs') || /\/docs/.test(w.textOf(w.E('upload-queue'))),
    'F15 queue shows the original destination directory');

  /* ---- F16: stale callbacks across logout/relogin ---- */
  section('F16 stale callbacks');
  /* F16a: session A's held list response lands after session B committed */
  const listDef = { promise: null, resolve: null };
  listDef.promise = new Promise((r) => { listDef.resolve = r; });
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': async () => listDef.promise, // A's list: held in flight
  });
  await login(w, KEY_MAIN);
  await enterDrive(w, 'fx-alpha'); // fires A's list, stays pending
  await logout(w);
  ok(w.E('gate').hidden === false && w.E('workspace').hidden === true, 'F16 logout returns to the gate');
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meB()),
    'GET /v1/drives/fx-b-drive/list': listOverride(w, { '/': listDoc('/', [entry('b-file.txt', false, 1)]) }),
  });
  await login(w, KEY_B);
  ok(w.textOf(w.E('me-prefix')).includes('fxsession'), 'F16 session B logged in');
  await enterDrive(w, 'fx-b-drive');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('b-file.txt'), 20000, 'F16 B list');
  listDef.resolve(listDoc('/', [entry('A-LEAK.txt', false, 1)])); // A's list lands late
  await w.settle();
  ok(!w.textOf(w.E('file-list')).includes('A-LEAK.txt'), 'F16 late session-A list never renders into session B');
  ok(w.textOf(w.E('file-list')).includes('b-file.txt'), 'F16 session B list intact after stale resolution');
  ok(w.E('gate').hidden === true, 'F16 stale resolution does not log session B out');

  /* F16b: stale upload completion after logout */
  const deferredUp2 = { promise: null, resolve: null };
  deferredUp2.promise = new Promise((r) => { deferredUp2.resolve = r; });
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/': listDoc('/', [entry('docs', true)]) }),
    'PUT /v1/drives/fx-alpha/files': putOverride(w),
    'PUT /v1/drives/{drive}/files': putOverride(w),
  });
  await login(w, KEY_MAIN);
  await enterDrive(w, 'fx-alpha');
  pushReceipts(w, { deferred: deferredUp2 });
  await queueUpload(w, [fakeFile('up-ghost.bin', 3)]);
  await w.settle();
  await logout(w);
  resetMock(w);
  installOverrides(w, { 'GET /v1/me': meOverride(meB()) });
  await login(w, KEY_B);
  ok(w.textOf(w.E('upload-queue')).trim() === '' || !w.textOf(w.E('upload-queue')).includes('up-ghost.bin'), 'F16b queue cleared for the new session');
  deferredUp2.resolve(uploadReceipt('/up-ghost.bin', 3));
  await w.settle();
  ok(!w.textOf(w.E('upload-queue')).includes('up-ghost.bin'), 'F16b stale upload completion never re-renders after logout');
  ok(wsText(w).indexOf('up-ghost.bin') === -1, 'F16b stale upload info absent from the whole workspace');

  /* F16e: a stale session-A upload completion must never pump session B's queue */
  const staleAUp = { promise: null, resolve: null };
  staleAUp.promise = new Promise((r) => { staleAUp.resolve = r; });
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/': listDoc('/', [entry('docs', true)]) }),
    'PUT /v1/drives/fx-alpha/files': putOverride(w),
    'PUT /v1/drives/{drive}/files': putOverride(w),
  });
  await login(w, KEY_MAIN);
  await enterDrive(w, 'fx-alpha');
  await listed(w, 'docs', 'F16e root listing');
  pushReceipts(w, { deferred: staleAUp });
  await queueUpload(w, [fakeFile('up-staleA.bin', 3)]);
  await w.settle();
  ok(callsWhere(w, (r) => r.method === 'PUT').some((p) => p.pathParam === '/up-staleA.bin'), 'F16e session A upload held in flight');
  await logout(w);
  resetMock(w);
  const meBWriter = meB();
  meBWriter.drives[0].perms = ['list', 'read', 'write']; // writable B session for the queue test
  installOverrides(w, {
    'GET /v1/me': meOverride(meBWriter),
    'GET /v1/drives/fx-b-drive/list': listOverride(w, { '/': listDoc('/', [entry('b-file.txt', false, 1)]) }),
    'PUT /v1/drives/fx-b-drive/files': putOverride(w),
    'PUT /v1/drives/{drive}/files': putOverride(w),
  });
  await login(w, KEY_B);
  await enterDrive(w, 'fx-b-drive');
  await listed(w, 'b-file.txt', 'F16e B listing');
  const bDef1 = { promise: null, resolve: null };
  bDef1.promise = new Promise((r) => { bDef1.resolve = r; });
  const bDef2 = { promise: null, resolve: null };
  bDef2.promise = new Promise((r) => { bDef2.resolve = r; });
  pushReceipts(w, { deferred: bDef1 });           // b-1: held in flight
  pushReceipts(w, { deferred: bDef2 });           // b-2: queued, held when it starts
  pushReceipts(w, uploadReceipt('/b-3.bin', 3));  // b-3: added during b-1's flight
  const bPutsBase = callsWhere(w, (r) => r.method === 'PUT').length;
  const bPuts = () => callsWhere(w, (r) => r.method === 'PUT');
  const bPutPaths = () => bPuts().map((p) => p.pathParam);
  await queueUpload(w, [fakeFile('b-1.bin', 1), fakeFile('b-2.bin', 2)]);
  await w.settle();
  ok(bPuts().length === bPutsBase + 1 && bPuts()[bPuts().length - 1].pathParam === '/b-1.bin',
    'F16e session B first upload in flight, second still queued: ' + bPutPaths());
  /* stale session-A completion lands while B's FIRST upload is still pending */
  staleAUp.resolve(uploadReceipt('/up-staleA.bin', 3));
  await w.settle();
  ok(bPuts().length === bPutsBase + 1, 'F16e stale A completion alone starts no B PUT, got ' + bPutPaths());
  const staleState2 = taskState(w, 'up-staleA.bin');
  ok(!staleState2.found || !staleState2.done, 'F16e stale A task never re-renders into session B');
  /* THE REGRESSION TRIGGER: enqueue another session-B file while B's FIRST
   * upload is STILL pending — enqueueFiles calls pump(). Correct code keeps its
   * lock (activeTask = b-1) so the queued b-2 must NOT start. A stale callback
   * that nulled activeTask BEFORE its generation check would leave the lock
   * clear and let pump() start b-2 in parallel with the in-flight b-1. */
  w.E('file-input').files = [fakeFile('b-3.bin', 3)];
  fire(w.E('file-input'), 'change');
  await w.settle();
  ok(bPuts().length === bPutsBase + 1,
    'F16e enqueuing during B\'s active upload starts NO parallel PUT (a stale callback must not clear the activeTask lock), got ' + bPutPaths());
  bDef1.resolve(uploadReceipt('/b-1.bin', 1));
  await w.settle();
  ok(bPuts().length === bPutsBase + 2 && bPuts()[bPuts().length - 1].pathParam === '/b-2.bin',
    'F16e B queue advances only after its own upload settles (b-2 next), got ' + bPutPaths());
  bDef2.resolve(uploadReceipt('/b-2.bin', 2));
  await w.settle();
  ok(bPuts().length === bPutsBase + 3 && bPuts()[bPuts().length - 1].pathParam === '/b-3.bin',
    'F16e B third (later-enqueued) upload runs after b-2 settles, got ' + bPutPaths());
  const b3State = taskState(w, 'b-3.bin');
  ok(b3State.found && b3State.done, 'F16e B third upload completes against its own receipt');

  /* F16c: overlapping logins — newest wins */
  const slowMe = { promise: null, resolve: null };
  slowMe.promise = new Promise((r) => { slowMe.resolve = r; });
  await logout(w);
  resetMock(w);
  clearMap(w);
  w.mock.map['GET /v1/me'] = (() => {
    let n = 0;
    return async () => {
      n++;
      if (n === 1) return slowMe.promise;
      return meB();
    };
  })();
  w.E('key').value = KEY_MAIN;
  fire(w.E('login-form'), 'submit'); // slow (resolves last)
  await w.settle();
  w.E('key').value = KEY_B;
  fire(w.E('login-form'), 'submit'); // fast (newest)
  await pumpUntil(() => w.E('workspace').hidden === false, 20000, 'F16c login B');
  ok(w.textOf(w.E('me-prefix')).includes('fxsession'), 'F16c newest login attempt won, got: ' + w.textOf(w.E('me-prefix')));
  slowMe.resolve(meDoc());
  await w.settle();
  const wsVisible = visibleText(w, w.E('workspace'));
  ok(!wsVisible.includes('AlphaRoot') && !wsVisible.includes('fx-alpha'),
    'F16c late losing login never repaints the VISIBLE workspace with session-A drives, got: ' + JSON.stringify(wsVisible.slice(0, 140)));
  const wsHiddenLeftover = w.textOf(w.E('workspace')).includes('fx-alpha');
  ok(!wsHiddenLeftover, 'F16c-minor no stale drive id is left anywhere in the workspace DOM after relogin (hidden dropzone text carries the previous session drive; scrub dz-text on logout/enterWorkspace)');
  ok(w.E('workspace').hidden === false, 'F16c workspace still up after the stale login settled');

  /* F16d: 401 mid-session returns to the gate with an explanation */
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/': listDoc('/', [entry('docs', true)]) }),
  });
  await login(w, KEY_MAIN);
  await enterDrive(w, 'fx-alpha');
  await pumpUntil(() => w.textOf(w.E('file-list')).includes('docs'), 20000, 'F16d working list');
  installOverrides(w, { 'GET /v1/drives/fx-alpha/list': async () => { throw mkErr(401, 'unauthorized', 'EXPIRED-KEY-SENTINEL'); } });
  fire(w.E('refresh-btn'), 'click');
  await pumpUntil(() => w.E('gate').hidden === false, 20000, 'F16d 401 gate');
  ok(w.E('workspace').hidden === true, 'F16d workspace hidden after 401');
  ok(/401|失效|吊销/.test(w.textOf(w.E('gate-err'))), 'F16d 401 explained at the gate, got: ' + JSON.stringify(w.textOf(w.E('gate-err'))));

  /* ---- F17: key hygiene (mock session) ---- */
  section('F17 key hygiene');
  assertNoKeyLeak(w, 'F17', ALL_KEYS);
  ok(callsWhere(w, (r) => String(r.path).indexOf('/files/api') === 0 || String(r.path).indexOf('/files/w/') === 0).length === 0,
    'WS-mock mock mode never calls the cookie workspace API (zero real network, no fabricated sessions)');
  ok(w.E('switch-ws').hidden === true, 'WS-mock the workspace switch control is hidden in mock mode');
  ok(w.urlLog.length === 0 || !w.urlLog.some((u) => ALL_KEYS.some((k) => u.url.includes(k))), 'F17 key never written to any URL/navigation');
  ok(w.confirmLog.length === 0 || !w.confirmLog.some((c) => ALL_KEYS.some((k) => c.includes(k))), 'F17 key never surfaces via native dialogs');
  const mockPuts = callsWhere(w, (r) => r.method === 'PUT');
  const noKeyPuts = mockPuts.filter((c) => c.hasKey !== true);
  ok(mockPuts.length === 0 || noKeyPuts.length === 0, 'F17 every recorded upload carries has_key (key present in the apikey header only)' + (noKeyPuts.length ? ' — offenders: ' + JSON.stringify(noKeyPuts.map((c) => c.path)) : ''));
  const keyFieldClear = String(w.E('key').value) === '';
  ok(keyFieldClear || w.E('gate').hidden === false, 'F17 key input not retained after a session (memory-only display)');

  /* ---- F21: upload-first manager (collapsed by default; explicit get link) ---- */
  section('F21 manager defaults OPEN (collapsible)');
  const mgrHidden = (ww) => ww.E('file-manager').hidden === true;
  const mgrToggle = (ww) => ww.E('manager-toggle');

  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/': listDoc('/', [entry('docs', true), entry('hero.txt', false, 5)]) }),
    'PUT /v1/drives/fx-alpha/files': putOverride(w),
    'PUT /v1/drives/{drive}/files': putOverride(w),
    'POST /v1/links': async () => ({ url: 'http://127.0.0.1:18893/l/hero-tok', expires_at: '2026-10-21T00:00:00Z' }),
  });
  await loginExpectLanding(w, KEY_MAIN);

  /* EVERY drive — a writable one included — enters with the manager OPEN. */
  await enterDriveRaw(w, 'fx-alpha');
  await listed(w, 'hero.txt', 'F21 root listing');
  ok(w.E('file-manager').hidden === false, 'F21 a writable drive enters with #file-manager OPEN (default)');
  ok(mgrToggle(w).getAttribute('aria-expanded') === 'true', 'F21 the default-open toggle reports aria-expanded=true');
  ok(mgrToggle(w).getAttribute('aria-controls') === 'file-manager', 'F21 toggle aria-controls points at #file-manager');
  ok(/收起文件管理/.test(w.textOf(mgrToggle(w))) && !/管理文件/.test(w.textOf(mgrToggle(w))),
    'F21 the default-open toggle label reads 收起文件管理, got: ' + JSON.stringify(w.textOf(mgrToggle(w))));
  ok(w.visible(w.E('breadcrumb')) === true, 'F21 the destination breadcrumb stays visible outside the manager');
  ok(w.textOf(w.E('breadcrumb')).includes('fx-alpha'), 'F21 breadcrumb names the drive');
  ok(w.visible(w.E('back-drives')) === true, 'F21 the ← 我的网盘 control stays visible outside the manager');
  ok(findAllW(w, w.E('dropzone'), (el) => el.id === 'dz-title').length === 1, 'F21 the dropzone (#dz-title) is present alongside the open manager');
  ok(w.textOf(w.E('dz-title')).includes('拖到这里') || w.textOf(w.E('dz-title')).length > 0, 'F21 the dropzone carries a prominent title, got: ' + JSON.stringify(w.textOf(w.E('dz-title'))));

  /* The user can still COLLAPSE it manually; the toggle flips both directions and
   * keeps label/aria in sync. */
  fire(mgrToggle(w), 'click');
  await w.settle();
  ok(mgrHidden(w), 'F21 clicking 收起文件管理 collapses #file-manager');
  ok(mgrToggle(w).getAttribute('aria-expanded') === 'false', 'F21 collapsing flips aria-expanded to false');
  ok(/管理文件/.test(w.textOf(mgrToggle(w))), 'F21 collapsed toggle label reads 管理文件, got: ' + JSON.stringify(w.textOf(mgrToggle(w))));
  fire(mgrToggle(w), 'click');
  await w.settle();
  ok(w.E('file-manager').hidden === false && mgrToggle(w).getAttribute('aria-expanded') === 'true' && /收起文件管理/.test(w.textOf(mgrToggle(w))),
    'F21 re-expanding restores 收起文件管理 / aria-expanded=true');
  /* collapse again so we can prove upload + 获取链接 still work while MANUALLY
   * collapsed (upload-first layout preserves the whole flow). */
  fire(mgrToggle(w), 'click');
  await w.settle();
  ok(mgrHidden(w), 'F21 the writable manager is manually collapsed for the upload flow');

  /* Upload works while the manager stays COLLAPSED; the completed task exposes a
   * prominent 获取链接 that opens the SAME explicit share dialog — never an
   * automatic POST /v1/links. */
  pushReceipts(w, uploadReceipt('/hero-up.bin', 3));
  await queueUpload(w, [fakeFile('hero-up.bin', 3)]);
  await w.settle();
  ok(mgrHidden(w), 'F21 uploading never expands the collapsed manager');
  const heroTask = taskState(w, 'hero-up.bin');
  ok(heroTask.found && heroTask.done, 'F21 the upload completes while the manager is collapsed');
  const heroGet = findControls(w, taskEl(w, 'hero-up.bin'), ['获取链接'], { armedOnly: true })[0];
  ok(!!heroGet, 'F21 a completed task offers a 获取链接 control');
  ok(heroGet && String(heroGet.className).includes('btn-primary'), 'F21 获取链接 is the prominent (primary) action, got class ' + JSON.stringify(heroGet && heroGet.className));
  ok(findControls(w, taskEl(w, 'hero-up.bin'), ['分享']).length === 0, 'F21 the old secondary 分享 label is gone from the completed task');
  ok(callsWhere(w, (r) => r.path === '/v1/links').length === 0, 'F21 completing an upload never auto-requests a public link');
  fire(heroGet, 'click');
  await w.settle();
  ok(w.E('dlg-share').open === true, 'F21 获取链接 opens the explicit share dialog');
  ok(w.textOf(w.E('share-what')).includes('hero-up.bin'), 'F21 the share dialog names the uploaded file');
  ok(callsWhere(w, (r) => r.path === '/v1/links').length === 0, 'F21 opening the dialog still issues no link request');
  fire(w.E('share-form'), 'submit');
  await pumpUntil(() => callsWhere(w, (r) => r.path === '/v1/links').length === 1, 20000, 'F21 explicit get-link POST');
  await w.settle();
  const heroLink = callsWhere(w, (r) => r.path === '/v1/links')[0];
  ok(heroLink && heroLink.body && heroLink.body.path === '/hero-up.bin', 'F21 explicit 获取链接 posts the final uploaded path, got ' + JSON.stringify(heroLink && heroLink.body));
  ok(String(w.E('share-url').value).includes('/l/hero-tok') && w.E('share-result').hidden === false, 'F21 the share dialog shows the returned public URL');
  fire(findControls(w, w.E('dlg-share'), ['关闭', '取消'], { armedOnly: true })[0] || w.E('dlg-share'), 'click');
  await w.settle();

  /* Switching drives RE-OPENS the manager (the default applies to every drive;
   * no collapse state leaks across drives). A read-only drive's dropzone also
   * states the drive is read-only. */
  installOverrides(w, { 'GET /v1/drives/fx-readonly/list': listOverride(w, { '/': listDoc('/', [entry('vault.txt', false, 5)]) }) });
  await enterDriveRaw(w, 'fx-readonly');
  await listed(w, 'vault.txt', 'F21 readonly listing');
  ok(w.E('file-manager').hidden === false, 'F21 a read-only drive entry opens the file manager');
  ok(mgrToggle(w).getAttribute('aria-expanded') === 'true', 'F21 read-only entry sets aria-expanded=true');
  ok(/收起文件管理/.test(w.textOf(mgrToggle(w))), 'F21 read-only entry label reads 收起文件管理');
  ok(w.textOf(w.E('dz-title')).includes('只读'), 'F21 read-only dropzone title states the drive is read-only, got: ' + JSON.stringify(w.textOf(w.E('dz-title'))));
  fire(mgrToggle(w), 'click');
  await w.settle();
  ok(mgrHidden(w) && mgrToggle(w).getAttribute('aria-expanded') === 'false', 'F21 the read-only manager can still be collapsed by the user');
  fire(mgrToggle(w), 'click');
  await w.settle();
  ok(w.E('file-manager').hidden === false, 'F21 the read-only manager re-opens through the toggle');
  /* back to a writable drive from a collapsed read-only one: opens again */
  fire(mgrToggle(w), 'click');
  await w.settle();
  ok(mgrHidden(w), 'F21 read-only manager collapsed before switching');
  await enterDriveRaw(w, 'fx-alpha');
  await listed(w, 'hero.txt', 'F21 back to writable');
  ok(w.E('file-manager').hidden === false && mgrToggle(w).getAttribute('aria-expanded') === 'true',
    'F21 a writable drive entered after a collapsed read-only one opens expanded again');
  ok(w.E('dlg-share').open !== true && String(w.E('share-url').value) === '', 'F21 no share dialog/URL is left over across a drive switch');

  /* Returning to 我的网盘 still collapses it; logout/login re-opens on entry. */
  await backToDrives(w);
  ok(mgrHidden(w) && mgrToggle(w).getAttribute('aria-expanded') === 'false', 'F21 returning to 我的网盘 collapses the manager');
  await logout(w);
  resetMock(w);
  installOverrides(w, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/': listDoc('/', [entry('docs', true), entry('hero.txt', false, 5)]) }),
  });
  await loginExpectLanding(w, KEY_MAIN);
  await enterDriveRaw(w, 'fx-alpha');
  await listed(w, 'hero.txt', 'F21 relogin listing');
  ok(w.E('file-manager').hidden === false && mgrToggle(w).getAttribute('aria-expanded') === 'true' && /收起文件管理/.test(w.textOf(mgrToggle(w))),
    'F21 after logout/login the manager opens again with the 收起文件管理 label');

  /* Without link permission: the permission-aware hint is the only dropzone
   * warning, and no 获取链接 is offered (upload still works, collapsed). */
  await logout(w);
  resetMock(w);
  const meNoLinkMock = meDoc();
  meNoLinkMock.drives[0].perms = ['list', 'read', 'write']; // fx-alpha: no link
  installOverrides(w, {
    'GET /v1/me': meOverride(meNoLinkMock),
    'GET /v1/drives/fx-alpha/list': listOverride(w, { '/': listDoc('/', [entry('docs', true), entry('hero.txt', false, 5)]) }),
    'PUT /v1/drives/fx-alpha/files': putOverride(w),
    'PUT /v1/drives/{drive}/files': putOverride(w),
  });
  await loginExpectLanding(w, KEY_MAIN);
  await enterDriveRaw(w, 'fx-alpha');
  await listed(w, 'hero.txt', 'F21 no-link listing');
  ok(w.E('file-manager').hidden === false, 'F21 a writable no-link drive also enters with the manager open');
  ok(w.E('upload-hint').hidden === false && /没有 link 权限|不能生成公开分享链接|无法生成/.test(w.textOf(w.E('upload-hint'))),
    'F21 without link permission the upload hint explains public links cannot be generated, got: ' + JSON.stringify(w.textOf(w.E('upload-hint'))));
  fire(mgrToggle(w), 'click');
  await w.settle();
  pushReceipts(w, uploadReceipt('/nolink-up.bin', 3));
  await queueUpload(w, [fakeFile('nolink-up.bin', 3)]);
  await w.settle();
  const nlState = taskState(w, 'nolink-up.bin');
  ok(nlState.found && nlState.done, 'F21 a no-link drive still completes an upload');
  ok(findControls(w, taskEl(w, 'nolink-up.bin'), ['获取链接'], { armedOnly: true }).length === 0,
    'F21 a no-link drive never offers 获取链接 on a completed upload');
  ok(callsWhere(w, (r) => r.path === '/v1/links').length === 0, 'F21 a no-link completion issues no link request');
  assertNoKeyLeak(w, 'F21', ALL_KEYS);
  await logout(w);

  /* ================= F22: clipboard + insecure-context fallbacks =================
   * The copy button must put the ACTUAL share URL on the clipboard. Chrome/LAN
   * HTTP has isSecureContext=false and no navigator.clipboard, and a native
   * modal makes body-level controls inert — so the legacy path may ONLY succeed
   * by focusing/selecting #share-url INSIDE the dialog. Success is claimed only
   * when writeText resolves or execCommand returns true. */
  section('F22 clipboard and insecure-context copy');

  /* Shared setup: land on the 我的网盘 selector, then enter a drive and open the
   * share dialog for a known file. helper returns the world with dl rows ready. */
  async function setUpShareDialogFor(ww, keySuffix) {
    installOverrides(ww, {
      'GET /v1/me': meOverride(meDoc()),
      'GET /v1/drives/fx-alpha/list': listOverride(ww, { '/': listDoc('/', [entry('share-me.txt', false, 7)]) }),
      'POST /v1/links': async () => ({ url: 'http://127.0.0.1:18893/l/tok-' + keySuffix, expires_at: '2026-10-21T00:00:00Z' }),
    });
    await loginExpectLanding(ww, KEY_MAIN);
    await enterDrive(ww, 'fx-alpha');
    await listed(ww, 'share-me.txt', 'F22 listing ' + keySuffix);
    fire(findControls(ww, fileRow(ww, 'share-me.txt'), ['分享', '创建链接', '链接'], { armedOnly: true })[0], 'click');
    await ww.settle();
    fire(ww.E('share-form'), 'submit');
    await pumpUntil(() => ww.E('share-result').hidden === false, 20000, 'F22 link generated (' + keySuffix + ')');
    await ww.settle();
    return String(ww.E('share-url').value);
  }
  const resetCopyLogs = (ww) => { ww.copiedLog.length = 0; ww.clipboardLog.length = 0; ww.execLog.length = 0; ww.focusLog.length = 0; };
  const closeShareDlg = (ww) => {
    const c = findControls(ww, ww.E('dlg-share'), ['关闭', '取消'], { armedOnly: true })[0];
    fire(c || ww.E('dlg-share'), 'click');
  };
  /* Re-point a COPY CASE at a freshly-logged-in session: logout → reinstall me →
   * login → re-enter the drive → regenerate the link. */
  async function rearmShare(ww, suffix) {
    await logout(ww);
    resetMock(ww);
    return setUpShareDialogFor(ww, suffix);
  }

  /* M1 — secure Clipboard API succeeds: the async path is used, nothing else. */
  {
    await logout(w);
    dropTimers();
    const wSec = createWorld({ search: '?mock=1' });
    const url = await setUpShareDialogFor(wSec, 'secure');
    resetCopyLogs(wSec);
    fire(wSec.E('share-copy'), 'click');
    await wSec.settle();
    ok(writeTextCopies(wSec).some((c) => c.text === url), 'F22 secure: writeText receives exactly the share URL');
    ok(copiedEquals(wSec, url), 'F22 secure: the actual copied text equals the share URL, got ' + JSON.stringify(copiedTexts(wSec)));
    ok(execCopies(wSec).length === 0 && wSec.execLog.length === 0, 'F22 secure: no legacy execCommand used when the API works, execLog=' + JSON.stringify(wSec.execLog));
    ok(shareCopySaysCopied(wSec), 'F22 secure: button shows 已复制, got ' + JSON.stringify(shareCopyLabel(wSec)));
    ok(wSec.textOf(wSec.E('share-err')).trim() === '', 'F22 secure: no inline error');
  }

  /* M2 — secure Clipboard API rejects: fall back to selecting #share-url INSIDE
   * the dialog; the copied text is the URL and the button shows success. */
  {
    const wRej = createWorld({ search: '?mock=1', clipboard: { write: 'reject' } });
    const url = await setUpShareDialogFor(wRej, 'reject');
    resetCopyLogs(wRej);
    fire(wRej.E('share-copy'), 'click');
    await wRej.settle();
    ok(wRej.clipboardLog.includes(url) || writeTextCopies(wRej).length === 0, 'F22 reject: rejected writeText never records a copy');
    const ex = execCopies(wRej);
    ok(ex.length === 1 && ex[0].text === url, 'F22 reject: execCommand copied the share URL, got ' + JSON.stringify(copiedTexts(wRej)));
    ok(ex.length === 1 && ex[0].inDialog === true && ex[0].activeId === 'share-url',
      'F22 reject: legacy copy selected #share-url INSIDE the dialog (not a body-level temp textarea), got ' + JSON.stringify(ex));
    ok(wRej.execLog.length === 1, 'F22 reject: exactly one execCommand(copy) call, got ' + JSON.stringify(wRej.execLog));
    ok(shareCopySaysCopied(wRej), 'F22 reject: fallback success shows 已复制, got ' + JSON.stringify(shareCopyLabel(wRej)));
    ok(wRej.textOf(wRej.E('share-err')).trim() === '', 'F22 reject: no inline error on successful fallback');
  }

  /* M3 — writeText throws SYNCHRONOUSLY: still falls back (no crash). */
  {
    const wThrow = createWorld({ search: '?mock=1', clipboard: { write: 'throw' } });
    const url = await setUpShareDialogFor(wThrow, 'syncthrow');
    resetCopyLogs(wThrow);
    fire(wThrow.E('share-copy'), 'click');
    await wThrow.settle();
    ok(wThrow.execLog.includes('copy'), 'F22 sync throw: sync writeText throw still reaches the execCommand fallback, execLog=' + JSON.stringify(wThrow.execLog));
    const ex = execCopies(wThrow);
    ok(ex.length === 1 && ex[0].text === url && ex[0].activeId === 'share-url', 'F22 sync throw: copied the URL from #share-url inside the dialog');
    ok(shareCopySaysCopied(wThrow), 'F22 sync throw: button shows 已复制, got ' + JSON.stringify(shareCopyLabel(wThrow)));
  }

  /* M4 — INSECURE context + NO clipboard API (Chrome over LAN HTTP): the ONLY
   * path is legacyCopy. It must select #share-url inside the dialog, and the old
   * bug (appending a temp <textarea> to <body>, inert behind the modal) must be
   * caught: with a body-level selection, execCommand copies nothing. */
  {
    const wHttp = createWorld({ search: '?mock=1', noClipboard: true, insecure: true });
    const url = await setUpShareDialogFor(wHttp, 'http');
    ok(wHttp.clipCfg.secure === false && !wHttp.clipCfg.present, 'F22 http: world models isSecureContext=false and no navigator.clipboard');
    resetCopyLogs(wHttp);
    fire(wHttp.E('share-copy'), 'click');
    await wHttp.settle();
    const ex = execCopies(wHttp);
    ok(writeTextCopies(wHttp).length === 0, 'F22 http: no async Clipboard API is used when it is absent');
    ok(ex.length === 1 && ex[0].text === url, 'F22 http: legacy execCommand puts the exact share URL on the clipboard, got ' + JSON.stringify(copiedTexts(wHttp)));
    ok(ex.length === 1 && ex[0].inDialog === true && ex[0].activeId === 'share-url',
      'F22 http: the copied control is #share-url inside the dialog (a body-level temp textarea would be inert and copy nothing), got ' + JSON.stringify(ex));
    ok(!findAllW(wHttp, wHttp.doc.body, (el) => el.tagName === 'textarea').length,
      'F22 http: no temporary <textarea> is appended to <body> for the copy');
    ok(shareCopySaysCopied(wHttp), 'F22 http: button shows 已复制, got ' + JSON.stringify(shareCopyLabel(wHttp)));
    /* MUTATION PROOF: model the OLD bug — a body-level textarea selection behind
     * the modal — and confirm the harness makes it FAIL (inert), so the assertion
     * above is load-bearing rather than vacuous. */
    const bug = wHttp.doc.createElement('textarea');
    wHttp.doc.body.appendChild(bug);
    bug.value = url; bug.focus(); bug.select();
    resetCopyLogs(wHttp);
    const execOkBehindModal = wHttp.doc.execCommand('copy');
    ok(execOkBehindModal === false && copiedTexts(wHttp).length === 0,
      'F22 http mutation-proof: a body-level temp textarea behind the modal copies NOTHING (the old bug is detectable)');
    bug.remove();
  }

  /* M5 — execCommand returns false: no success is claimed, an inline error tells
   * the user to copy manually, and the URL is still SELECTED in #share-url so the
   * user can press Ctrl+C. */
  {
    const wFalse = createWorld({ search: '?mock=1', noClipboard: true, insecure: true, clipboard: { exec: false } });
    const url = await setUpShareDialogFor(wFalse, 'execfalse');
    resetCopyLogs(wFalse);
    fire(wFalse.E('share-copy'), 'click');
    await wFalse.settle();
    ok(copiedTexts(wFalse).length === 0, 'F22 exec false: nothing is recorded as copied, got ' + JSON.stringify(copiedTexts(wFalse)));
    ok(!shareCopySaysCopied(wFalse) && shareCopyLabel(wFalse) === '复制', 'F22 exec false: button stays 复制 (no false success), got ' + JSON.stringify(shareCopyLabel(wFalse)));
    ok(/未能复制|手动|Ctrl\+C|选择/.test(wFalse.textOf(wFalse.E('share-err'))),
      'F22 exec false: an inline error tells the user to copy manually, got ' + JSON.stringify(wFalse.textOf(wFalse.E('share-err'))));
    /* the URL must be SELECTED for manual copy: the field was focused/selected */
    ok(wFalse.focusLog.some((f) => f.id === 'share-url' && f.op === 'select'),
      'F22 exec false: #share-url was selected so the user can copy manually, focusLog=' + JSON.stringify(wFalse.focusLog.map((f) => f.id + ':' + f.op)));
    ok(String(wFalse.E('share-url').value) === url, 'F22 exec false: the share URL is still present for manual selection');
  }

  /* M6 — selection itself fails / execCommand throws: the click is clean, no
   * crash, no success claimed, and #share-url is left intact for manual copy. */
  {
    const wSelFail = createWorld({ search: '?mock=1', noClipboard: true, insecure: true, selectFail: true });
    await setUpShareDialogFor(wSelFail, 'selfail');
    wSelFail.E('share-url')._selectFail = true;
    const url = String(wSelFail.E('share-url').value);
    resetCopyLogs(wSelFail);
    fire(wSelFail.E('share-copy'), 'click');
    await wSelFail.settle();
    ok(copiedTexts(wSelFail).length === 0, 'F22 selection failure: nothing copied');
    ok(!shareCopySaysCopied(wSelFail), 'F22 selection failure: no 已复制 success claimed, got ' + JSON.stringify(shareCopyLabel(wSelFail)));
    ok(/未能复制|手动|Ctrl\+C|选择/.test(wSelFail.textOf(wSelFail.E('share-err'))),
      'F22 selection failure: inline error explains manual copy, got ' + JSON.stringify(wSelFail.textOf(wSelFail.E('share-err'))));
    ok(String(wSelFail.E('share-url').value) === url && wSelFail.E('dlg-share').open === true,
      'F22 selection failure: dialog stays open and the URL field is intact (no crash)');
    /* execCommand throwing must be caught too */
    const wExecThrow = createWorld({ search: '?mock=1', noClipboard: true, insecure: true, clipboard: { exec: 'throw' } });
    await setUpShareDialogFor(wExecThrow, 'execthrow');
    resetCopyLogs(wExecThrow);
    fire(wExecThrow.E('share-copy'), 'click');
    await wExecThrow.settle();
    ok(copiedTexts(wExecThrow).length === 0 && !shareCopySaysCopied(wExecThrow),
      'F22 exec throw: a throwing execCommand is caught — no copy, no success, got ' + JSON.stringify(shareCopyLabel(wExecThrow)));
    ok(wExecThrow.textOf(wExecThrow.E('share-err')).trim() !== '' || wExecThrow.E('dlg-share').open === true,
      'F22 exec throw: the click is handled cleanly (dialog intact, error or manual hint shown)');
  }

  /* M7 — a stale async clipboard callback after CLOSE must not mutate the closed
   * dialog, and after LOGOUT/relogin must not mutate the NEW session. */
  {
    const wStale = createWorld({ search: '?mock=1' });
    const urlA = await setUpShareDialogFor(wStale, 'staleA');
    const hold = holdClipboard(wStale);
    resetCopyLogs(wStale);
    fire(wStale.E('share-copy'), 'click');
    await wStale.settle();
    ok(wStale.clipboardLog.includes(urlA), 'F22 stale/close: the in-flight writeText was handed the share URL');
    ok(execCopies(wStale).length === 0, 'F22 stale/close: no fallback while the async write is still pending');
    /* close the dialog, then resolve the held write successfully */
    closeShareDlg(wStale);
    await wStale.settle();
    ok(dialogIsClosed(wStale, 'dlg-share'), 'F22 stale/close: dialog closed while the copy was pending');
    hold.resolve();
    await wStale.settle();
    releaseClipboard(wStale);
    ok(shareCopyLabel(wStale) !== '已复制', 'F22 stale/close: a late writeText success never marks the closed dialog 已复制, got ' + JSON.stringify(shareCopyLabel(wStale)));
    ok(wStale.textOf(wStale.E('share-err')).trim() === '', 'F22 stale/close: no error injected into the closed dialog either');
    /* now the LOGOUT variant: hold a write, log out, change sessions, then land it */
    await logout(wStale);
    resetMock(wStale);
    const urlB = await setUpShareDialogFor(wStale, 'staleB');
    const hold2 = holdClipboard(wStale);
    resetCopyLogs(wStale);
    fire(wStale.E('share-copy'), 'click');
    await wStale.settle();
    await logout(wStale);
    resetMock(wStale);
    installOverrides(wStale, { 'GET /v1/me': meOverride(meB()) });
    await login(wStale, KEY_B);
    const labelAfterB = shareCopyLabel(wStale);
    hold2.resolve();
    await wStale.settle();
    releaseClipboard(wStale);
    ok(!shareCopySaysCopied(wStale) && shareCopyLabel(wStale) === labelAfterB,
      'F22 stale/logout: a late copy success after relogin never mutates session B, got ' + JSON.stringify(shareCopyLabel(wStale)));
    ok(wStale.textOf(wStale.E('share-err')).trim() === '', 'F22 stale/logout: session B shows no stale copy error');
    ok(urlB.includes('/l/tok-staleB'), 'F22 stale/logout: sanity — the second session URL was generated');
  }

  dropTimers();

  /* ==== Session 2: URL drive/path query must NOT bypass home/authorization ====
   * The agreed flow is login → 我的网盘 landing → explicit drive entry. A URL
   * that carries drive/path-looking parameters must never auto-navigate; the
   * percent-encoding exercise happens through REAL navigation into a
   * hostile-named directory seeded via the mock override list. */
  dropTimers();
  section('F18 URL drive/path query never bypasses the landing or authorization');
  const HOSTILE_DIR = 'Deep Dir a&b#c?d';
  const w2 = createWorld({ search: '?mock=1&drive=' + encodeURIComponent('fx-alpha') + '&path=' + encodeURIComponent('/' + HOSTILE_DIR) });
  installOverrides(w2, {
    'GET /v1/me': meOverride(meDoc()),
    'GET /v1/drives/fx-alpha/list': listOverride(w2, {
      '/': listDoc('/', [entry(HOSTILE_DIR, true), entry('plain.txt', false, 2)]),
      [('/' + HOSTILE_DIR)]: listDoc('/' + HOSTILE_DIR, [entry('/' + HOSTILE_DIR + '/inside.txt', false, 1)]),
    }),
  });
  await login(w2, KEY_MAIN);
  ok(w2.E('drives-home').hidden === false, 'F18 login always lands on 我的网盘 even with drive/path-shaped URL parameters');
  ok(w2.E('browser-view').hidden === true, 'F18 URL drive/path never auto-enters a browser view');
  ok(callsWhere(w2, (r) => r.op === 'list').length === 0, 'F18 no directory is listed before an explicit user action (no authorization bypass)');
  ok(driveCard(w2, 'fx-alpha') !== null && w2.textOf(w2.E('drive-grid')).includes('plain') === false, 'F18 landing renders the grant cards, not URL-supplied content');
  /* encoding survives REAL navigation into the hostile-named seeded directory */
  await enterDrive(w2, 'fx-alpha');
  await pumpUntil(() => !!fileRow(w2, HOSTILE_DIR), 20000, 'F18 root listing with hostile dir');
  const hdRow = fileRow(w2, HOSTILE_DIR);
  const hdBtn = findAllW(w2, hdRow, (el) => String(el.className).includes('fx-namebtn'))[0];
  fire(hdBtn || hdRow, 'click');
  await pumpUntil(() => callsWhere(w2, (r) => r.op === 'list' && r.pathParam === '/' + HOSTILE_DIR).length > 0, 20000, 'F18 hostile dir list');
  await w2.settle();
  const rawNav2 = (w2.mock.calls || []).filter((c) => c.method === 'GET' && String(c.path).indexOf('/list') >= 0).pop();
  const navQ = rawNav2 ? String(rawNav2.query) : '';
  const navDecoded = new URLSearchParams(navQ).get('path');
  ok(navDecoded === '/' + HOSTILE_DIR,
    'F18 real navigation carries the hostile directory exactly in the path query (round-trips), got: ' + navQ);
  ok(navQ.includes('%26') && navQ.includes('%23'),
    'F18 & and # inside the name are percent-encoded (no query/fragment injection), got: ' + navQ);
  ok(w2.textOf(w2.E('file-tbody')).includes('inside.txt'), 'F18 hostile-named directory listing renders');
  ok(w2.textOf(w2.E('breadcrumb')).includes(HOSTILE_DIR), 'F18 breadcrumb renders the hostile segment as text');
  assertNoKeyLeak(w2, 'F18', [KEY_MAIN]);

  /* ============ Session 3: production transport (no ?mock) ============ */
  dropTimers();
  section('F19 production transport');
  const wp = createWorld({ production: true });
  wp.prodDocs.me = meDoc();
  wp.prodDocs.listByPath['fx-alpha'] = {
    '/': listDoc('/', [entry('docs', true)]),
    /* name is the basename; path is the full path — the ticket body/path use the
     * full path while the download attr uses the basename. */
    '/docs': listDoc('/docs', [{ name: 'pd.txt', path: '/docs/pd.txt', is_dir: false, size: 8, modified: '2026-10-14T10:00:00Z', hashes: {} }]),
  };
  const prodList = async function (url) {
    const u = new URL(url, 'http://x');
    const drive = decodeURIComponent(stripWsPath(u.pathname).split('/')[3]);
    const p = u.searchParams.get('path') || '/';
    return (wp.prodDocs.listByPath[drive] || {})[p] || listDoc('/', []);
  };
  ok(wp.E('mock-banner').hidden === true, 'F19 mock banner hidden outside ?mock=1');
  /* boot first restores the cookie workspace list (default stub: none) → gate */
  await pumpUntil(() => wp.E('gate').hidden === false, 20000, 'F19 boot gate after empty restore');
  ok(prodWorkspacesFetches(wp).length === 1, 'F19 boot asked GET /files/api/workspaces exactly once');
  ok(wp.E('ws-list').hidden === true, 'F19 zero saved entries go straight to the key gate');
  wp.E('key').value = KEY_PROD;
  fire(wp.E('login-form'), 'submit');
  /* the raw key must leave the input immediately, before any response */
  ok(String(wp.E('key').value) === '', 'F19 login clears the key input synchronously (page never retains it)');
  await pumpUntil(() => wp.E('workspace').hidden === false, 20000, 'F19 prod login');
  const WS_ID = 'w-prodmain01';
  const loginPost = prodLoginPosts(wp)[0];
  ok(!!loginPost, 'F19 login POSTed /files/api/login');
  let loginBody = null;
  try { loginBody = JSON.parse(String(loginPost.init.body)); } catch (e) { /* not json */ }
  ok(loginBody && loginBody.api_key === KEY_PROD && loginBody.workspace_id === undefined,
    'F19 login body is exactly {api_key} (no key anywhere else), got ' + JSON.stringify(loginBody));
  ok(loginPost.init.credentials === 'same-origin' && String(loginPost.init.headers && loginPost.init.headers['X-C2A-Workspace']) === '1',
    'F19 login request is same-origin with the X-C2A-Workspace header, got ' + JSON.stringify(loginPost.init.credentials) + '/' + JSON.stringify(loginPost.init.headers));
  ok(!loginPost.init.headers || !loginPost.init.headers.apikey, 'F19 no apikey header on the login request');
  const meFetch = prodMeFetches(wp, WS_ID)[0];
  ok(!!meFetch, 'F19 after login /me rides the workspace adapter /files/w/<id>/api/v1/me');
  ok(meFetch && meFetch.init.credentials === 'same-origin', 'F19 fetch uses credentials:"same-origin", got ' + JSON.stringify(meFetch && meFetch.init.credentials));
  ok(meFetch && String(meFetch.init.headers && meFetch.init.headers['X-C2A-Workspace']) === '1', 'F19 X-C2A-Workspace:1 header on the workspace /me');
  ok(meFetch && !(meFetch.init.headers && meFetch.init.headers.apikey), 'F19 no apikey/Authorization long-term key header on /me');
  ok(meFetch && !meFetch.url.includes(KEY_PROD), 'F19 key never rides in the URL');
  ok(wp.urlLog.some((u) => u.kind === 'replaceState' && u.url === '/files/w/' + WS_ID + '/'),
    'F19 login replaceState navigates to the returned entry /files/w/<id>/, got ' + JSON.stringify(wp.urlLog));
  /* enter drive → encoded list request */
  await enterDrive(wp, 'fx-alpha');
  await pumpUntil(() => !!fileRow(wp, 'docs'), 20000, 'F19 root listing');
  const docsRowP = fileRow(wp, 'docs');
  const docsBtnP = findAllW(wp, docsRowP, (el) => String(el.className).includes('fx-namebtn') || (el.tagName === 'button' && wp.textOf(el).includes('docs')))[0];
  fire(docsBtnP || docsRowP, 'click');
  await pumpUntil(() => wp.textOf(wp.E('file-list')).includes('pd.txt'), 20000, 'F19 list /docs');
  const listFetch = wp.fetchLog.filter((f) => stripWsPath(String(f.url).split('?')[0]) === '/v1/drives/fx-alpha/list').pop();
  ok(listFetch.init.credentials === 'same-origin' && String(listFetch.init.headers['X-C2A-Workspace']) === '1' && !(listFetch.init.headers.apikey),
    'F19 list keeps credentials same-origin + X-C2A-Workspace and never an apikey header');
  ok(listFetch.url.includes(encodeURIComponent('/docs')), 'F19 list path query is percent-encoded, got ' + listFetch.url);
  ok(prodDataUrls(wp).every((u) => String(u).indexOf('/files/w/' + WS_ID + '/api/v1/') === 0 || String(u) === '/files/api/login' || String(u) === '/files/api/workspaces'),
    'F19 every data request stays inside the current entry adapter, got ' + JSON.stringify(prodDataUrls(wp)));

  /* F19b: non-empty upload → raw PUT body (positive length), cookie session */
  const heldResolve = { fn: null };
  const held = new Promise((r) => { heldResolve.fn = r; });
  wp.prodRoute = async function (url, init) {
    const u = new URL(url, 'http://x');
    const handled = await filesApiRoute(wp, u.pathname, init);
    if (handled !== undefined) return handled;
    assertWsAlive(wp, u.pathname);
    const p = stripWsPath(u.pathname);
    if (p === '/v1/drives/fx-alpha/files' && (init.method || '') === 'PUT') return held;
    if (p === '/v1/me') return wp.prodDocs.me;
    if (p.indexOf('/list') >= 0) return prodList(url);
    throw routeError(404, 'not_found', 'prod stub ' + url);
  };
  await queueUpload(wp, [fakeFile('up-raw.bin', 1024)]);
  await wp.settle();
  const rawXhr = wp.xhrLog.filter((x) => x.sent && x.method === 'PUT').pop();
  const rawFetchUp = wp.fetchLog.filter((f) => f.init.method === 'PUT').pop();
  if (rawXhr) {
    ok(stripWsPath(String(rawXhr.url).split('?')[0]) === '/v1/drives/fx-alpha/files' && rawXhr.url.includes(encodeURIComponent('/docs/up-raw.bin')), 'F19b raw PUT url rides the entry adapter and encodes drive+path, got ' + rawXhr.url);
    ok(!rawXhr.headers.apikey, 'F19b upload XHR carries NO apikey long-term key header');
    ok(String(rawXhr.headers['x-c2a-workspace']) === '1', 'F19b upload XHR carries X-C2A-Workspace:1');
    ok(rawXhr.withCredentials === true, 'F19b upload XHR is credentialed (same-origin session Cookie), got ' + rawXhr.withCredentials);
    ok(String(rawXhr.headers['idempotency-key'] || '').length > 0, 'F19b XHR carries an Idempotency-Key');
    ok(String(rawXhr.headers['content-type']) === 'application/octet-stream', 'F19b non-empty raw PUT sets octet-stream, got ' + String(rawXhr.headers['content-type']));
    ok(!(rawXhr.body instanceof global.FormData), 'F19b non-empty file uses a RAW body, not multipart');
    const rawLen = rawXhr.body && (rawXhr.body.size != null ? rawXhr.body.size : (rawXhr.body.byteLength != null ? rawXhr.body.byteLength : (rawXhr.body.length != null ? rawXhr.body.length : -1)));
    ok(rawLen === 1024, 'F19b raw body length is positive and exact (' + rawLen + ')');
    ok(!String(rawXhr.url).includes(KEY_PROD), 'F19b the key never appears in the upload URL');
    /* 100% progress must NOT complete the task */
    rawXhr.__progress(1024, 1024);
    await wp.settle();
    let stp = taskState(wp, 'up-raw.bin');
    ok(stp.found && !stp.done, 'F19b 100% browser progress alone never shows 完成, got: ' + JSON.stringify(stp.text));
    rawXhr.__respond(200, JSON.stringify(uploadReceipt('/docs/up-raw.bin', 1024)));
    await wp.settle();
    stp = taskState(wp, 'up-raw.bin');
    ok(stp.found && stp.done, 'F19b valid upstream receipt completes the task, got: ' + JSON.stringify(stp.text));
  } else if (rawFetchUp) {
    ok(!(rawFetchUp.init.headers && rawFetchUp.init.headers.apikey) && rawFetchUp.init.credentials === 'same-origin', 'F19b fetch upload keeps same-origin credentials and no apikey header');
    const b = rawFetchUp.init.body;
    ok(!(b instanceof global.FormData) && b && (b.size === 1024 || b.byteLength === 1024), 'F19b fetch upload carries the raw 1024-byte body');
    heldResolve.fn(uploadReceipt('/docs/up-raw.bin', 1024));
    await wp.settle();
  } else {
    ok(false, 'F19b no PUT transport observed for the non-empty upload');
  }

  /* F19c: zero-byte upload MUST use multipart (raw PUT needs Content-Length>0) */
  const zeroHeld = { fn: null };
  const zeroPromise = new Promise((r) => { zeroHeld.fn = r; });
  wp.prodRoute = async function (url, init) {
    const u = new URL(url, 'http://x');
    const handled = await filesApiRoute(wp, u.pathname, init);
    if (handled !== undefined) return handled;
    assertWsAlive(wp, u.pathname);
    const p = stripWsPath(u.pathname);
    if (p === '/v1/drives/fx-alpha/files' && (init.method || '') === 'PUT') return zeroPromise;
    if (p === '/v1/me') return wp.prodDocs.me;
    if (p.indexOf('/list') >= 0) return prodList(url);
    throw routeError(404, 'not_found', 'prod stub ' + url);
  };
  await queueUpload(wp, [fakeFile('up-zero.bin', 0)]);
  await wp.settle();
  const zeroXhr = wp.xhrLog.filter((x) => x.sent && x.method === 'PUT').pop();
  const zeroFetchAny = wp.fetchLog.filter((f) => f.init.method === 'PUT').pop();
  if (zeroXhr) {
    ok(zeroXhr.body instanceof global.FormData, 'F19c zero-byte upload uses multipart FormData, got ' + (zeroXhr.body && zeroXhr.body.constructor && zeroXhr.body.constructor.name));
    ok(zeroXhr.body.has('file'), 'F19c multipart body carries the `file` part');
    ok(!zeroXhr.headers.apikey && String(zeroXhr.headers['x-c2a-workspace']) === '1', 'F19c zero upload authenticates via the workspace cookie header, no apikey');
    zeroXhr.__respond(200, JSON.stringify(uploadReceipt('/docs/up-zero.bin', 0)));
    await wp.settle();
  } else if (zeroFetchAny) {
    ok(zeroFetchAny.init.body instanceof global.FormData && zeroFetchAny.init.body.has('file'), 'F19c zero-byte fetch upload uses multipart with a file part');
    zeroHeld.fn(uploadReceipt('/docs/up-zero.bin', 0));
    await wp.settle();
  } else {
    ok(false, 'F19c no PUT transport observed for the zero-byte upload');
  }
  const zeroState = taskState(wp, 'up-zero.bin');
  ok(zeroState.found && zeroState.done, 'F19c valid receipt completes the zero-byte upload, got: ' + JSON.stringify(zeroState.text));

  /* F19d: 200 with garbage body is never completion */
  const badXhrHold = { fn: null };
  const badPromise = new Promise((r) => { badXhrHold.fn = r; });
  wp.prodRoute = async function (url, init) {
    const u = new URL(url, 'http://x');
    const handled = await filesApiRoute(wp, u.pathname, init);
    if (handled !== undefined) return handled;
    assertWsAlive(wp, u.pathname);
    const p = stripWsPath(u.pathname);
    if (p === '/v1/drives/fx-alpha/files' && (init.method || '') === 'PUT') return badPromise;
    if (p === '/v1/me') return wp.prodDocs.me;
    if (p.indexOf('/list') >= 0) return prodList(url);
    throw routeError(404, 'not_found', 'prod stub ' + url);
  };
  await queueUpload(wp, [fakeFile('up-bad.bin', 5)]);
  await wp.settle();
  const badXhr = wp.xhrLog.filter((x) => x.sent && x.method === 'PUT').pop();
  if (badXhr) {
    badXhr.__respond(200, '<html>not json</html>');
    await wp.settle();
  } else {
    const badFetch = wp.fetchLog.filter((f) => f.init.method === 'PUT').pop();
    badXhrHold.fn({ __raw: true, status: 200, text: '<html>not json</html>', headers: {} });
    await wp.settle();
  }
  const badState = taskState(wp, 'up-bad.bin');
  ok(badState.found && !badState.done, 'F19d 2xx garbage response never renders 完成, got: ' + JSON.stringify(badState.text));

  /* F19e: user cancel aborts the in-flight request and shows non-completion */
  const cancelHold = { fn: null };
  const cancelPromise = new Promise((r) => { cancelHold.fn = r; });
  wp.prodRoute = async function (url, init) {
    const u = new URL(url, 'http://x');
    const handled = await filesApiRoute(wp, u.pathname, init);
    if (handled !== undefined) return handled;
    assertWsAlive(wp, u.pathname);
    const p = stripWsPath(u.pathname);
    if (p === '/v1/drives/fx-alpha/files' && (init.method || '') === 'PUT') return cancelPromise;
    if (p === '/v1/me') return wp.prodDocs.me;
    if (p.indexOf('/list') >= 0) return prodList(url);
    throw routeError(404, 'not_found', 'prod stub ' + url);
  };
  await queueUpload(wp, [fakeFile('up-cancel.bin', 5)]);
  await wp.settle();
  const cancelXhr = wp.xhrLog.filter((x) => x.sent && x.method === 'PUT' && !x.aborted).pop();
  const cancelTaskEl = taskEl(wp, 'up-cancel.bin');
  const cancelCtl = cancelTaskEl ? findControls(wp, cancelTaskEl, ['取消', '停止'], { armedOnly: true })[0] : null;
  if (cancelCtl && cancelXhr) {
    fire(cancelCtl, 'click');
    await wp.settle();
    ok(cancelXhr.aborted === true, 'F19e cancel aborts the in-flight XHR');
    const cState = taskState(wp, 'up-cancel.bin');
    ok(cState.found && !cState.done, 'F19e cancelled upload is never marked 完成, got: ' + JSON.stringify(cState.text));
  } else {
    ok(false, 'F19e cancel control / in-flight XHR not found for the queued task');
  }

  /* F19g: native download-ticket (production transport) */
  section('F19 download-ticket transport');
  /* Route: list + /v1/me as before; POST /v1/drives/{drive}/download-ticket
   * answers with the short-lived same-origin ticket. Any GET to the old
   * streaming path is a contract violation and is counted. */
  let dlBytesHits = 0;
  let ticketDoc = null; // set per-case; function form allowed
  let prodTicketCount = 0;
  wp.prodRoute = async function (url, init) {
    const u = new URL(url, 'http://x');
    const handled = await filesApiRoute(wp, u.pathname, init);
    if (handled !== undefined) return handled;
    assertWsAlive(wp, u.pathname);
    const p = stripWsPath(u.pathname);
    if (p === '/v1/drives/fx-alpha/download-ticket' && (init.method || '') === 'POST') {
      prodTicketCount++;
      if (typeof ticketDoc === 'function') return ticketDoc(init);
      if (ticketDoc) return ticketDoc;
      throw routeError(500, 'harness', 'no ticket fixture');
    }
    if (p === '/v1/drives/fx-alpha/files' && (init.method || 'GET') !== 'PUT') {
      dlBytesHits++; // the removed streaming download path
      return { __raw: true, status: 200, text: 'PD-BYTES', headers: {} };
    }
    if (p === '/v1/me') return wp.prodDocs.me;
    if (p.indexOf('/list') >= 0) return prodList(url);
    throw routeError(404, 'not_found', 'prod stub ' + url);
  };
  const FUTURE = new Date(Date.now() + 120000).toISOString();
  const okTicket = () => ({ url: '/download/AbC123_-token', expires_at: FUTURE });
  const dlRow = fileRow(wp, 'pd.txt');
  ok(!!dlRow, 'F19g download row present');
  const dlBtn = () => findControls(wp, fileRow(wp, 'pd.txt'), ['下载'], { armedOnly: true })[0];
  const resetDl = () => {
    wp.anchorLog.length = 0;
    wp.objectUrlLog.length = 0;
    wp.E('notice').hidden = true;
    wp.E('notice').textContent = '';
  };

  /* F19g-ok: a valid ticket → exactly one POST body {path}, apikey header +
   * credentials omit ONLY on the ticket request, then a native <a> click. */
  ticketDoc = () => okTicket();
  resetDl();
  const ticketBase = prodTicketCount;
  fire(dlBtn(), 'click');
  await pumpUntil(() => wp.anchorLog.length > 0, 20000, 'F19g ticket download anchor');
  /* the success notice is non-sticky and cleared when the virtual clock drains,
   * so snapshot it before settle() */
  const dlOkNotice = wp.textOf(wp.E('notice'));
  await wp.settle();
  const prodTickets = prodTicketFetches(wp);
  ok(prodTickets.length === 1, 'F19g exactly one download-ticket POST, got ' + prodTickets.length);
  const tk = prodTickets[0];
  ok(tk && tk.init.credentials === 'same-origin' && !(tk.init.headers && tk.init.headers.apikey),
    'F19g ticket POST keeps same-origin credentials and never an apikey header, got ' + JSON.stringify(tk && tk.init.credentials));
  ok(tk && String(tk.init.headers && tk.init.headers['X-C2A-Workspace']) === '1', 'F19g ticket POST carries X-C2A-Workspace:1');
  ok(tk && !tk.url.includes(KEY_PROD), 'F19g ticket request URL never carries the key');
  let tBody = null;
  try { tBody = JSON.parse(String(tk && tk.init.body)); } catch (e) { /* not json */ }
  ok(tBody && tBody.path === '/docs/pd.txt', 'F19g ticket POST body is exactly {path:"/docs/pd.txt"}, got ' + JSON.stringify(tBody));
  ok(prodTicketCount === ticketBase + 1, 'F19g small download issues exactly one ticket request');
  ok(dlBytesHits === 0, 'F19g no legacy byte-stream GET is issued for a download (ticket only), got ' + dlBytesHits);
  ok(wp.objectUrlLog.length === 0, 'F19g no Blob/object-URL buffering is used, objectUrlLog=' + JSON.stringify(wp.objectUrlLog));
  const a0 = wp.anchorLog[wp.anchorLog.length - 1];
  ok(!!a0, 'F19g a native anchor click performs the download');
  ok(a0 && a0.href === 'http://127.0.0.1:18893/download/AbC123_-token',
    'F19g anchor href is the resolved same-origin ticket URL, got ' + JSON.stringify(a0 && a0.href));
  ok(a0 && a0.download === 'pd.txt', 'F19g anchor carries download="pd.txt", got ' + JSON.stringify(a0 && a0.download));
  ok(a0 && /noreferrer/.test(a0.rel) && /noopener/.test(a0.rel), 'F19g anchor rel is noreferrer+noopener, got ' + JSON.stringify(a0 && a0.rel));
  ok(a0 && /no-referrer/i.test(a0.referrerPolicy), 'F19g anchor suppresses the referrer, got ' + JSON.stringify(a0 && a0.referrerPolicy));
  ok(a0 && a0.inDocument === true, 'F19g the anchor was attached to the document when clicked');
  ok(!wp.fetchLog.some((f) => String(f.url).includes('AbC123_-token')),
    'F19g the ticket URL is never fetched by the page itself (no body/Blob buffering)');
  ok(/已交由浏览器下载|进度与结果/.test(dlOkNotice) && !/已完成|保存成功/.test(dlOkNotice),
    'F19g notice says the browser owns progress (never claims completion), got ' + JSON.stringify(dlOkNotice));

  /* F19g-bad: malformed/unsafe ticket URLs and bad expiry never start a download */
  const hostileTickets = [
    ['cross-origin absolute', { url: 'http://evil.example/download/tok', expires_at: FUTURE }],
    ['credentialed URL', { url: 'http://u:p@127.0.0.1:18893/download/tok', expires_at: FUTURE }],
    ['javascript: URL', { url: 'javascript:alert(1)', expires_at: FUTURE }],
    ['non-/download/ same-origin path', { url: '/files/tok', expires_at: FUTURE }],
    ['same-origin with query', { url: '/download/tok?x=1', expires_at: FUTURE }],
    ['same-origin with fragment', { url: '/download/tok#frag', expires_at: FUTURE }],
    ['extra path segment', { url: '/download/tok/extra', expires_at: FUTURE }],
    ['traversal escaping /download/', { url: '/download/../files/tok', expires_at: FUTURE }],
    ['percent-encoding in the token', { url: '/download/tok%2Fkey', expires_at: FUTURE }],
    ['dot segment in the token', { url: '/download/..%2Ftok', expires_at: FUTURE }],
    ['expired expiry', { url: '/download/tok', expires_at: new Date(Date.now() - 1000).toISOString() }],
    ['non-string expiry', { url: '/download/tok', expires_at: 12345 }],
    ['missing url', { expires_at: FUTURE }],
    /* a ticket URL that smuggles key material in a query must be refused */
    ['key in the URL query', { url: '/download/tok?k=' + KEY_PROD, expires_at: FUTURE }],
    /* NOTE: "key embedded in the token path" is exercised in the MOCK session
     * (F2d-key-smuggle) where the page still knows the session key; the
     * production page never holds a key, so it cannot leak one into an href. */
  ];
  for (const [label, doc] of hostileTickets) {
    ticketDoc = () => doc;
    resetDl();
    fire(dlBtn(), 'click');
    await wp.settle();
    ok(wp.anchorLog.length === 0, 'F19g hostile ticket (' + label + ') starts no native download');
    ok(wp.E('notice').hidden === false && /凭证无效|无效|过期/.test(wp.textOf(wp.E('notice'))),
      'F19g hostile ticket (' + label + ') is explained, got ' + JSON.stringify(wp.textOf(wp.E('notice'))));
    ok(!wp.anchorLog.some((a) => a.href.includes(KEY_PROD)), 'F19g key never reaches any ticket anchor href (' + label + ')');
  }

  /* F19g-perm: a read-without-link drive can download, while a link-without-read
   * drive cannot (the download button is only armed for read). Each perm change
   * needs a fresh login because ME is the authoritative grant list. */
  const navDocs = async () => {
    if (wp.E('browser-view').hidden === true) await enterDrive(wp, 'fx-alpha');
    await pumpUntil(() => !!fileRow(wp, 'docs') || !!fileRow(wp, 'pd.txt'), 20000, 'F19g root list');
    if (!fileRow(wp, 'pd.txt')) {
      const r = fileRow(wp, 'docs');
      const b = findAllW(wp, r, (el) => String(el.className).includes('fx-namebtn') || (el.tagName === 'button' && wp.textOf(el).includes('docs')))[0];
      fire(b || r, 'click');
    }
    await pumpUntil(() => !!fileRow(wp, 'pd.txt'), 20000, 'F19g docs list');
  };
  const prodRelogin = async (doc) => {
    if (wp.E('workspace').hidden === false) await logout(wp);
    wp.prodDocs.me = doc;
    wp.E('key').value = KEY_PROD;
    fire(wp.E('login-form'), 'submit');
    await pumpUntil(() => wp.E('workspace').hidden === false, 20000, 'F19g prod relogin');
    await navDocs();
  };
  const meNoLink = meDoc();
  meNoLink.drives = [{
    id: 'fx-alpha', provider: 'openlist', root: '/AlphaRoot',
    perms: ['list', 'read', 'write', 'delete'], health: 'healthy', default: true,
  }];
  ticketDoc = () => okTicket();
  await prodRelogin(meNoLink);
  resetDl();
  const noLinkBtn = findControls(wp, fileRow(wp, 'pd.txt'), ['下载'], { armedOnly: true })[0];
  ok(!!noLinkBtn && !noLinkBtn.disabled, 'F19g read-without-link drive still offers download');
  fire(noLinkBtn, 'click');
  await pumpUntil(() => wp.anchorLog.length > 0, 20000, 'F19g no-link download anchor');
  ok(wp.anchorLog.length === 1, 'F19g read-without-link drive downloads via a ticket');
  const meLinkNoRead = meDoc();
  meLinkNoRead.drives = [{
    id: 'fx-alpha', provider: 'openlist', root: '/AlphaRoot',
    perms: ['list', 'link'], health: 'healthy', default: true,
  }];
  await prodRelogin(meLinkNoRead);
  resetDl();
  const linkOnlyBtn = findControls(wp, fileRow(wp, 'pd.txt'), ['下载'], { armedOnly: true })[0];
  ok(!linkOnlyBtn, 'F19g link-without-read drive offers no armed download control');
  const ticketBeforePerm = prodTicketCount;
  fire(findControls(wp, fileRow(wp, 'pd.txt'), ['下载'], {})[0] || wp.mkEl('div'), 'click');
  await wp.settle();
  ok(prodTicketCount === ticketBeforePerm && wp.anchorLog.length === 0, 'F19g link-without-read drive starts no ticket/download');

  /* restore the standard multi-drive prod session for the error/401 checks */
  await prodRelogin(meDoc());
  resetDl();

  /* F19g-err: a ticket error is explained and leaves no pending download */
  ticketDoc = () => { throw Object.assign(new Error('boom'), { __status: 503, __body: { error: { code: 'unavailable', message: 'TICKET-BOOM-SENTINEL' } } }); };
  resetDl();
  fire(dlBtn(), 'click');
  await pumpUntil(() => wp.E('notice').hidden === false && /TICKET-BOOM-SENTINEL|503|失败|错误/.test(wp.textOf(wp.E('notice'))), 20000, 'F19g ticket error notice');
  await wp.settle();
  ok(wp.anchorLog.length === 0, 'F19g ticket error never starts a download');
  ok(!/正在准备下载/.test(wp.textOf(wp.E('notice'))), 'F19g ticket error clears the preparing notice, got ' + JSON.stringify(wp.textOf(wp.E('notice'))));

  /* F19g-late: a ticket response that lands AFTER logout/relogin never starts a
   * download (authGen guard). */
  const lateTicket = { resolve: null };
  lateTicket.promise = new Promise((r) => { lateTicket.resolve = r; });
  ticketDoc = () => lateTicket.promise;
  resetDl();
  const lateBase = prodTicketCount;
  fire(dlBtn(), 'click');
  /* downloadItem shows the preparing notice synchronously before the request */
  const dlPendingNotice = wp.textOf(wp.E('notice'));
  await pumpUntil(() => prodTicketCount > lateBase, 20000, 'F19g late ticket in flight');
  await wp.settle();
  ok(/正在准备下载/.test(dlPendingNotice), 'F19g late-ticket case: request is in flight, got ' + JSON.stringify(dlPendingNotice));
  await logout(wp);
  ok(wp.E('gate').hidden === false, 'F19g logout returns to the gate while the ticket is held');
  wp.E('key').value = KEY_PROD;
  fire(wp.E('login-form'), 'submit');
  await pumpUntil(() => wp.E('workspace').hidden === false, 20000, 'F19g relogin');
  resetDl();
  lateTicket.resolve(okTicket());
  await wp.settle();
  ok(wp.anchorLog.length === 0, 'F19g a late ticket after logout/relogin never starts a download');
  ok(!wp.fetchLog.some((f) => String(f.url).includes('AbC123_-token')), 'F19g late ticket URL is never fetched either');
  await navDocs();

  /* F19g-401: ticket returns 401 → sign out to the gate */
  ticketDoc = () => { throw Object.assign(new Error('401'), { __status: 401, __body: { error: { code: 'unauthorized', message: 'TICKET-401-SENTINEL' } } }); };
  resetDl();
  fire(dlBtn(), 'click');
  await pumpUntil(() => wp.E('gate').hidden === false, 20000, 'F19g ticket 401 gate');
  ok(wp.E('workspace').hidden === true, 'F19g ticket 401 hides the workspace');
  ok(/401|失效|吊销/.test(wp.textOf(wp.E('gate-err'))), 'F19g ticket 401 explained at the gate, got ' + JSON.stringify(wp.textOf(wp.E('gate-err'))));
  /* restore a working prod session for the remaining transport checks */
  ticketDoc = () => okTicket();
  await prodRelogin(meDoc());
  resetDl();

  /* F19f: production 401 returns to the gate */
  section('F19 production 401');
  wp.prodRoute = async function (url) {
    const u = new URL(url, 'http://x');
    const handled = await filesApiRoute(wp, u.pathname, {});
    if (handled !== undefined) return handled;
    const p = stripWsPath(u.pathname);
    if (p.indexOf('/list') >= 0) {
      throw routeError(401, 'unauthorized', 'PROD-401-SENTINEL');
    }
    if (p === '/v1/me') return wp.prodDocs.me;
    throw routeError(404, 'not_found', 'prod stub ' + url);
  };
  fire(wp.E('refresh-btn'), 'click');
  await pumpUntil(() => wp.E('gate').hidden === false, 20000, 'F19f prod 401 gate');
  ok(wp.E('workspace').hidden === true, 'F19f workspace hidden after production 401');
  ok(/401|失效|吊销/.test(wp.textOf(wp.E('gate-err'))), 'F19f production 401 explained, got: ' + JSON.stringify(wp.textOf(wp.E('gate-err'))));
  ok(String(wp.E('gate-hint').textContent).includes('重新登录') && String(wp.E('gate-hint').textContent).includes('工作区'),
    'F19f the 401 gate is bound to THIS entry for re-login, got: ' + JSON.stringify(wp.E('gate-hint').textContent));

  /* F20: pagehide invalidates the session for in-flight uploads */
  section('F20 pagehide invalidation');
  wp.prodDocs.me = meDoc();
  wp.prodDocs.listByPath['fx-alpha'] = { '/': listDoc('/', [entry('docs', true)]) };
  const phHold = { fn: null };
  const phPromise = new Promise((r) => { phHold.fn = r; });
  wp.prodRoute = async function (url, init) {
    const u = new URL(url, 'http://x');
    const handled = await filesApiRoute(wp, u.pathname, init);
    if (handled !== undefined) return handled;
    assertWsAlive(wp, u.pathname);
    const p = stripWsPath(u.pathname);
    if (p === '/v1/drives/fx-alpha/files' && (init.method || '') === 'PUT') return phPromise;
    if (p === '/v1/me') return wp.prodDocs.me;
    if (p.indexOf('/list') >= 0) return prodList(url);
    throw routeError(404, 'not_found', 'prod stub ' + url);
  };
  wp.E('key').value = KEY_PROD;
  fire(wp.E('login-form'), 'submit');
  await pumpUntil(() => wp.E('workspace').hidden === false, 20000, 'F20 re-login');
  await enterDrive(wp, 'fx-alpha');
  await pumpUntil(() => !!fileRow(wp, 'docs'), 20000, 'F20 list');
  await queueUpload(wp, [fakeFile('up-ph.bin', 5)]);
  await wp.settle();
  const phXhr = wp.xhrLog.filter((x) => x.sent && x.method === 'PUT').pop();
  ok(!!phXhr && !phXhr.aborted, 'F20 upload in flight before pagehide');
  const logoutPostsBeforePh = prodLogoutPosts(wp).length;
  wp.winFire('pagehide', {});
  await wp.settle();
  ok(phXhr.aborted === true, 'F20 pagehide aborts the in-flight upload request');
  ok(prodLogoutPosts(wp).length === logoutPostsBeforePh,
    'F20 pagehide NEVER POSTs a workspace logout (the cookie session must survive), got ' +
    (prodLogoutPosts(wp).length - logoutPostsBeforePh) + ' new');
  /* even if a response still arrives, the dead session must not complete it */
  phXhr.__respond(200, JSON.stringify(uploadReceipt('/up-ph.bin', 5)));
  await wp.settle();
  const phState = taskState(wp, 'up-ph.bin');
  // the task must be gone from the queue OR present in a non-done state
  ok(!phState.found || !phState.done, 'F20 post-pagehide response never completes the task, got: ' + JSON.stringify(phState.found ? phState.text : '(task removed)'));

  /* ================= Session 4: persistent workspace restore/logout ========
   * Production-boot flows around the cookie workspace contract: restore,
   * multi-entry list, hostile server URLs, per-entry re-login, switch and
   * logout isolation. All via the recording fetch stub (zero real network). */
  dropTimers();

  /* ---- WS1: a single valid entry auto-restores ---- */
  section('WS1 single valid entry auto-restores');
  {
    const wsr = createWorld({ production: true });
    wsr.prodDocs.me = meDoc();
    wsr.prodDocs.listByPath['fx-alpha'] = { '/': listDoc('/', [entry('restored.txt', false, 3)]) };
    const wsOne = 'ws-single-01';
    wsr.prodDocs.workspaces = { workspaces: [WS_ENTRY(wsOne)] };
    await pumpUntil(() => wsr.E('workspace').hidden === false, 20000, 'WS1 auto-restore');
    ok(wsr.E('gate').hidden === true && wsr.E('ws-list').hidden === true, 'WS1 a single entry never stops at the gate or the list');
    ok(wsr.urlLog.some((u) => u.kind === 'replaceState' && u.url === '/files/w/' + wsOne + '/'),
      'WS1 restore replaceState-navigates into /files/w/<id>/, got ' + JSON.stringify(wsr.urlLog));
    ok(wsr.urlLog.every((u) => u.kind !== 'pushState'), 'WS1 restore does not push a new history entry');
    ok(prodLoginPosts(wsr).length === 0, 'WS1 restore never POSTs a login (cookie only)');
    const meF = prodMeFetches(wsr, wsOne)[0];
    ok(!!meF, 'WS1 restore reads /me through the entry adapter');
    ok(meF && meF.init.credentials === 'same-origin' && String(meF.init.headers['X-C2A-Workspace']) === '1' && !(meF.init.headers.apikey),
      'WS1 restore /me is same-origin + X-C2A-Workspace:1 without any apikey header');
    ok(wsr.textOf(wsr.E('me-prefix')).includes('fxmain'), 'WS1 the restored session shows the /me key prefix');
    assertNoKeyLeak(wsr, 'WS1', ALL_KEYS);
  }

  /* ---- WS2: multiple entries render a selectable list + 添加 Key ---- */
  section('WS2 multiple entries render a selectable list');
  {
    const wsm = createWorld({ production: true });
    wsm.prodDocs.me = meDoc();
    const idA = 'ws-entry-aaaa';
    const idB = 'ws-entry-bbbb';
    wsm.prodDocs.workspaces = { workspaces: [WS_ENTRY(idA), WS_ENTRY(idB)] };
    await pumpUntil(() => wsm.E('ws-list').hidden === false && !!wsCard(wsm, idA) && !!wsCard(wsm, idB), 20000, 'WS2 list shown');
    ok(wsm.E('gate').hidden === true && wsm.E('workspace').hidden === true, 'WS2 list boot shows neither gate nor workspace');
    ok(!!wsCard(wsm, idA) && !!wsCard(wsm, idB), 'WS2 both entries render as cards');
    ok(findControls(wsm, wsm.E('ws-grid'), ['进入'], { armedOnly: true }).length === 2, 'WS2 each entry offers an explicit 进入 control');
    ok(prodMeFetches(wsm, idA).length === 0 && prodMeFetches(wsm, idB).length === 0, 'WS2 the list never auto-opens an entry');
    ok(prodLoginPosts(wsm).length === 0, 'WS2 the list never auto-logins');
    /* 添加 Key reveals the login form beside the list, touching nothing */
    fire(wsm.E('ws-add-key'), 'click');
    await wsm.settle();
    ok(wsm.E('gate').hidden === false && wsm.E('ws-list').hidden === false, 'WS2 添加 API Key reveals the login form beside the list');
    ok(prodLoginPosts(wsm).length === 0, 'WS2 revealing the form sends nothing');
    /* explicit entry click → its own adapter */
    await enterWsEntry(wsm, idB);
    ok(wsm.urlLog.some((u) => u.kind === 'pushState' && u.url === '/files/w/' + idB + '/'),
      'WS2 an explicit entry click navigates to its /files/w/<id>/');
    ok(prodMeFetches(wsm, idB).length === 1 && prodMeFetches(wsm, idA).length === 0,
      'WS2 /me targets exactly the chosen entry');
    ok(wsm.textOf(wsm.E('me-prefix')).includes('fxmain'), 'WS2 the chosen entry opened the workspace');
    assertNoKeyLeak(wsm, 'WS2', ALL_KEYS);
  }

  /* ---- WS3: restore failures fall back to the usable gate ---- */
  section('WS3 restore failures fall back to the gate');
  {
    const wne = createWorld({ production: true });
    wne.prodDocs.me = meDoc();
    const baseRoute = wne.prodRoute;
    wne.prodRoute = async (url, init) => {
      if (String(url) === '/files/api/workspaces') throw new Error('restore network down');
      return baseRoute(url, init);
    };
    await pumpUntil(() => wne.E('gate').hidden === false, 20000, 'WS3 network-error gate');
    ok(wne.E('ws-list').hidden === true && wne.E('workspace').hidden === true, 'WS3 a failed restore shows only the gate');
    ok(/网络错误|工作区/.test(wne.textOf(wne.E('gate-err'))),
      'WS3 the restore failure is explained, got ' + JSON.stringify(wne.textOf(wne.E('gate-err'))));
    /* the gate still allows a fresh login despite the restore failure */
    wne.prodRoute = baseRoute;
    wne.E('key').value = KEY_PROD;
    fire(wne.E('login-form'), 'submit');
    await pumpUntil(() => wne.E('workspace').hidden === false, 20000, 'WS3 login after failed restore');
    ok(wne.E('workspace').hidden === false, 'WS3 login still works after a failed restore');
  }

  /* ---- WS4: failed logins keep the gate and explain ---- */
  section('WS4 login failures stay at the gate');
  {
    const wlf = createWorld({ production: true });
    wlf.prodDocs.me = meDoc();
    wlf.prodDocs.loginFail = { status: 401, code: 'unauthorized', message: 'BAD-LOGIN-SENTINEL' };
    await pumpUntil(() => wlf.E('gate').hidden === false, 20000, 'WS4 gate');
    wlf.E('key').value = 'sk-c2a-wronglogin';
    fire(wlf.E('login-form'), 'submit');
    await wlf.settle();
    ok(wlf.E('workspace').hidden === true && wlf.E('gate').hidden === false, 'WS4 a failed login stays at the gate');
    ok(/BAD-LOGIN-SENTINEL|401|无效|吊销/.test(wlf.textOf(wlf.E('gate-err'))),
      'WS4 the login failure is explained, got ' + JSON.stringify(wlf.textOf(wlf.E('gate-err'))));
    ok(prodMeFetches(wlf, 'w-prodmain01').length === 0, 'WS4 a failed login issues no workspace /me');
    ok(wlf.urlLog.every((u) => u.kind !== 'replaceState' && u.kind !== 'pushState'), 'WS4 a failed login never navigates');
    /* the retry still works and the input is cleared on every attempt */
    wlf.prodDocs.loginFail = null;
    wlf.E('key').value = KEY_PROD;
    fire(wlf.E('login-form'), 'submit');
    ok(String(wlf.E('key').value) === '', 'WS4 the key input clears again on the retry');
    await pumpUntil(() => wlf.E('workspace').hidden === false, 20000, 'WS4 retry login');
    ok(wlf.E('workspace').hidden === false, 'WS4 the retry succeeds');
  }

  /* ---- WS5: hostile server entry URLs are rejected outright ---- */
  section('WS5 hostile entry URLs from the server are rejected');
  {
    const idOk = 'ws-target-01';
    const hostiles = [
      ['cross-origin absolute', { workspace_id: idOk, url: 'https://evil.example/files/w/' + idOk + '/', expires_at: FUTURE_LOGIN_ISO }],
      ['protocol-relative', { workspace_id: idOk, url: '//evil.example/files/w/' + idOk + '/', expires_at: FUTURE_LOGIN_ISO }],
      ['admin path', { workspace_id: idOk, url: '/admin/', expires_at: FUTURE_LOGIN_ISO }],
      ['non-entry path', { workspace_id: idOk, url: '/files/other/', expires_at: FUTURE_LOGIN_ISO }],
      ['query smuggling', { workspace_id: idOk, url: '/files/w/' + idOk + '/?x=1', expires_at: FUTURE_LOGIN_ISO }],
      ['fragment', { workspace_id: idOk, url: '/files/w/' + idOk + '/#f', expires_at: FUTURE_LOGIN_ISO }],
      ['id/url mismatch', { workspace_id: idOk, url: '/files/w/ws-other-99/', expires_at: FUTURE_LOGIN_ISO }],
      ['bad id chars', { workspace_id: 'ws/bad/id', url: '/files/w/ws/bad/id/', expires_at: FUTURE_LOGIN_ISO }],
      ['traversal path', { workspace_id: idOk, url: '/files/w/../admin/', expires_at: FUTURE_LOGIN_ISO }],
      ['missing url', { workspace_id: idOk, expires_at: FUTURE_LOGIN_ISO }],
      ['javascript: url', { workspace_id: idOk, url: 'javascript:alert(1)', expires_at: FUTURE_LOGIN_ISO }],
    ];
    for (const [label, doc] of hostiles) {
      dropTimers();
      const wh = createWorld({ production: true });
      wh.prodDocs.me = meDoc();
      wh.prodDocs.loginDoc = doc;
      await pumpUntil(() => wh.E('gate').hidden === false, 20000, 'WS5 gate ' + label);
      wh.E('key').value = KEY_PROD;
      fire(wh.E('login-form'), 'submit');
      await wh.settle();
      ok(wh.E('workspace').hidden === true, 'WS5 hostile entry (' + label + ') never opens a workspace');
      ok(!wh.urlLog.some((u) => (u.kind === 'replaceState' || u.kind === 'pushState') && String(u.url).indexOf('/files/w/') === 0),
        'WS5 hostile entry (' + label + ') never navigates, urlLog=' + JSON.stringify(wh.urlLog));
      ok(wh.fetchLog.every((f) => !/^\/files\/w\/[^/]+\/api\/v1\//.test(String(f.url))),
        'WS5 hostile entry (' + label + ') issues no workspace data requests');
      ok(/不合法|拒绝/.test(wh.textOf(wh.E('gate-err'))),
        'WS5 hostile entry (' + label + ') is explained, got ' + JSON.stringify(wh.textOf(wh.E('gate-err'))));
    }
  }

  /* ---- WS6: a deep-linked dead entry re-logins into THAT entry ---- */
  section('WS6 deep-linked dead entry re-logins into that entry');
  {
    const deadId = 'ws-dead-0001';
    const wsd = createWorld({ production: true, pathname: '/files/w/' + deadId + '/' });
    wsd.prodDocs.me = meDoc();
    wsd.prodDocs.loginKeys = KEY_PROD;
    wsd.prodDocs.wsId = deadId; // the re-login must return THIS entry
    wsd.prodDocs.ws401For = [deadId];
    await pumpUntil(() => wsd.E('gate').hidden === false, 20000, 'WS6 relogin gate');
    ok(wsd.E('workspace').hidden === true && wsd.E('ws-list').hidden === true, 'WS6 a dead deep link shows only the gate');
    const meDead = prodMeFetches(wsd, deadId);
    ok(meDead.length === 1 && meDead[0].init.credentials === 'same-origin', 'WS6 the deep link probed /me through the entry adapter first');
    const gateCtx = wsd.E('gate-hint').textContent + '\n' + wsd.textOf(wsd.E('gate-err'));
    ok(/工作区/.test(gateCtx) && /失效|重新登录/.test(gateCtx),
      'WS6 the gate names the dead workspace and asks for re-login, got ' + JSON.stringify(gateCtx));
    /* re-login: the body MUST carry workspace_id so another key cannot take over */
    wsd.E('key').value = KEY_PROD;
    fire(wsd.E('login-form'), 'submit');
    wsd.prodDocs.ws401For = null; // server accepted the matching key for this entry
    await wsd.settle();
    const relog = wsd.loginCalls[0];
    ok(relog && relog.workspace_id === deadId && relog.api_key === KEY_PROD,
      'WS6 the re-login POST carries the dead entry workspace_id, got ' + JSON.stringify(relog));
    await pumpUntil(() => wsd.E('workspace').hidden === false, 20000, 'WS6 relogin enters');
    ok(wsd.urlLog.some((u) => u.kind === 'replaceState' && u.url === '/files/w/' + deadId + '/'),
      'WS6 the successful re-login lands on the same entry url');
  }

  /* ---- WS7: stale entry loads never cross-render ---- */
  section('WS7 stale entry loads never cross-render');
  {
    const wst = createWorld({ production: true });
    const idA = 'ws-stale-aaaa';
    const idB = 'ws-stale-bbbb';
    wst.prodDocs.workspaces = { workspaces: [WS_ENTRY(idA), WS_ENTRY(idB)] };
    const meA = meDoc();
    meA.key = { id: 'k-a', prefix: 'sk-c2a-entryA…', default_drive: null };
    meA.drives = [{ id: 'fx-a-drive', provider: 'openlist', root: '/A', perms: ['list'], health: 'healthy' }];
    const meBws = meB();
    const holdA = { promise: null, resolve: null };
    holdA.promise = new Promise((r) => { holdA.resolve = r; });
    wst.prodRoute = async function (url, init) {
      const u = new URL(url, 'http://x');
      const handled = await filesApiRoute(wst, u.pathname, init);
      if (handled !== undefined) return handled;
      const mm = /^\/files\/w\/([^/]+)\/api\/v1\/me$/.exec(u.pathname);
      if (mm) {
        if (mm[1] === idA) return holdA.promise; // entry A stalls mid-air
        if (mm[1] === idB) return meBws;
        return meDoc();
      }
      throw routeError(404, 'not_found', 'WS7 stub ' + url);
    };
    await pumpUntil(() => wst.E('ws-list').hidden === false && !!wsCard(wst, idA) && !!wsCard(wst, idB), 20000, 'WS7 list');
    /* click A (stalls), then switch to B while A is still pending */
    const cardA = wsCard(wst, idA);
    fire(findControls(wst, cardA, ['进入'], { armedOnly: true })[0], 'click');
    await wst.settle();
    ok(wst.E('workspace').hidden === true && /打开工作区/.test(wst.E('ws-sub').textContent),
      'WS7 entry A stays pending with an opening notice');
    const cardB = wsCard(wst, idB);
    ok(!!cardB, 'WS7 the other entry stays clickable while A is pending');
    fire(findControls(wst, cardB, ['进入'], { armedOnly: true })[0], 'click');
    await pumpUntil(() => wst.E('workspace').hidden === false, 20000, 'WS7 entry B opens');
    ok(wst.textOf(wst.E('me-prefix')).includes('fxsession'), 'WS7 entry B opened with its own /me');
    holdA.resolve(meA); // A's stale response lands late
    await wst.settle();
    ok(wst.E('workspace').hidden === false && wst.textOf(wst.E('me-prefix')).includes('fxsession'),
      'WS7 the late entry-A response never takes over the open entry-B session');
    ok(prodMeFetches(wst, idA).length === 1 && prodMeFetches(wst, idB).length === 1,
      'WS7 exactly one /me per entry was issued');
    ok(wst.urlLog.some((u) => u.kind === 'pushState' && u.url === '/files/w/' + idB + '/'),
      'WS7 the URL follows the winning entry');
  }

  /* ---- WS8: logout POSTs only the current entry, then /files/ ---- */
  section('WS8 logout posts only the current workspace');
  {
    const wsl = createWorld({ production: true });
    wsl.prodDocs.me = meDoc();
    const idW = 'ws-logout-01';
    wsl.prodDocs.workspaces = { workspaces: [WS_ENTRY(idW)] };
    await pumpUntil(() => wsl.E('workspace').hidden === false, 20000, 'WS8 restore');
    fire(wsl.E('logout'), 'click');
    wsl.prodDocs.workspaces = { workspaces: [] }; // server removed the session
    await pumpUntil(() => wsl.E('gate').hidden === false, 20000, 'WS8 logout done');
    await wsl.settle();
    const posts = prodLogoutPosts(wsl);
    ok(posts.length === 1, 'WS8 exactly one logout POST, got ' + posts.length);
    ok(posts[0] && new RegExp('^/files/w/' + idW + '/api/logout$').test(String(posts[0].url)),
      'WS8 logout POST targets ONLY the current entry, got ' + (posts[0] && posts[0].url));
    ok(posts[0] && posts[0].init.credentials === 'same-origin' &&
      !(posts[0].init.headers && posts[0].init.headers.apikey) &&
      String(posts[0].init.headers && posts[0].init.headers['X-C2A-Workspace']) === '1',
      'WS8 the logout POST is same-origin with the workspace header and no apikey');
    ok(wsl.urlLog.some((u) => u.kind === 'pushState' && u.url === '/files/'), 'WS8 after logout the page returns to /files/');
    ok(prodWorkspacesFetches(wsl).length === 2, 'WS8 the list is refetched after logout, got ' + prodWorkspacesFetches(wsl).length);
    ok(wsl.E('gate').hidden === false && wsl.E('workspace').hidden === true && wsl.E('ws-list').hidden === true,
      'WS8 with the entry gone, /files/ shows only the key gate');
  }

  /* ---- WS10: 切换工作区 keeps the cookie session ---- */
  section('WS10 switch workspace keeps the session');
  {
    const wsw = createWorld({ production: true });
    wsw.prodDocs.me = meDoc();
    const idS = 'ws-switch-01';
    const idT = 'ws-switch-02';
    wsw.prodDocs.workspaces = { workspaces: [WS_ENTRY(idS), WS_ENTRY(idT)] };
    await pumpUntil(() => wsw.E('ws-list').hidden === false && !!wsCard(wsw, idS) && !!wsCard(wsw, idT), 20000, 'WS10 list');
    await enterWsEntry(wsw, idS);
    ok(wsw.E('workspace').hidden === false, 'WS10 entered the first workspace');
    fire(wsw.E('switch-ws'), 'click');
    await pumpUntil(() => wsw.E('ws-list').hidden === false && !!wsCard(wsw, idS) && !!wsCard(wsw, idT), 20000, 'WS10 back to list');
    ok(prodLogoutPosts(wsw).length === 0, 'WS10 switching NEVER POSTs logout (the cookie session is kept)');
    ok(wsw.urlLog.some((u) => u.kind === 'pushState' && u.url === '/files/'), 'WS10 switching returns to /files/');
    ok(!!wsCard(wsw, idS) && !!wsCard(wsw, idT), 'WS10 both entries are still offered after switching');
    await enterWsEntry(wsw, idT);
    ok(prodMeFetches(wsw, idT).length === 1, 'WS10 the other entry opens through its own adapter');
    ok(wsw.E('workspace').hidden === false, 'WS10 the other entry opened');
  }

  /* ---- WS11: the workspaces list is sanitized ---- */
  section('WS11 workspace list sanitization');
  {
    const wsg = createWorld({ production: true });
    wsg.prodDocs.me = meDoc();
    const good1 = 'ws-good-0001';
    const good2 = 'ws-good-0002';
    wsg.prodDocs.workspaces = { workspaces: [
      WS_ENTRY(good1),
      WS_ENTRY(good2),
      { id: 'bad/id', url: '/files/w/bad/id/', expires_at: FUTURE_LOGIN_ISO },
      { id: 'ws-mismatch0', url: '/files/w/ws-other00/', expires_at: FUTURE_LOGIN_ISO },
      { id: 'ws-absolute', url: 'https://evil.example/files/w/ws-absolute/', expires_at: FUTURE_LOGIN_ISO },
      { id: 'short' },
      'garbage',
      null,
    ] };
    await pumpUntil(() => wsg.E('ws-list').hidden === false && findControls(wsg, wsg.E('ws-grid'), ['进入'], { armedOnly: true }).length === 2, 20000, 'WS11 list shown');
    const cards = findControls(wsg, wsg.E('ws-grid'), ['进入'], { armedOnly: true });
    ok(cards.length === 2 && !!wsCard(wsg, good1) && !!wsCard(wsg, good2),
      'WS11 exactly the valid entries survive sanitization, got ' + cards.length);
    /* all-invalid list falls back to the gate */
    dropTimers();
    const wsb = createWorld({ production: true });
    wsb.prodDocs.workspaces = { workspaces: [{ id: 'nope' }, 'x', null] };
    await pumpUntil(() => wsb.E('gate').hidden === false, 20000, 'WS11 all-invalid gate');
    ok(wsb.E('ws-list').hidden === true && wsb.E('workspace').hidden === true,
      'WS11 an all-invalid list falls back to the key gate');
  }

  /* ---- WS12: a failed logout returns to /files/ with the session intact ---- */
  section('WS12 failed logout keeps the session');
  {
    const wsf = createWorld({ production: true });
    wsf.prodDocs.me = meDoc();
    const idF = 'ws-faillogo-1';
    wsf.prodDocs.workspaces = { workspaces: [WS_ENTRY(idF)] };
    await pumpUntil(() => wsf.E('workspace').hidden === false, 20000, 'WS12 restore');
    const listsBefore = prodWorkspacesFetches(wsf).length;
    wsf.prodDocs.logoutFail = 'network';
    fire(wsf.E('logout'), 'click');
    await wsf.settle();
    ok(prodLogoutPosts(wsf).length === 1, 'WS12 the logout POST was attempted');
    ok(prodWorkspacesFetches(wsf).length === listsBefore + 1, 'WS12 a failed logout still returns to /files/ to re-list');
    await pumpUntil(() => wsf.E('ws-list').hidden === false && !!wsCard(wsf, idF), 20000, 'WS12 list after failed logout');
    ok(wsf.E('workspace').hidden === true && wsf.E('gate').hidden === true,
      'WS12 after a failed logout the still-valid entry is LISTED, never silently re-entered');
    /* the still-valid entry stays selectable; from inside it the logout retry completes */
    wsf.prodDocs.logoutFail = null;
    await enterWsEntry(wsf, idF);
    wsf.prodDocs.workspaces = { workspaces: [] }; // server removes the session this time
    fire(wsf.E('logout'), 'click');
    await pumpUntil(() => wsf.E('gate').hidden === false, 20000, 'WS12 retry logout');
    await wsf.settle();
    ok(prodLogoutPosts(wsf).length === 2, 'WS12 the re-entry retry POSTs logout again');
    ok(wsf.E('gate').hidden === false, 'WS12 the completed logout lands on the gate');
  }

  /* ---- WS13: explicit switch never auto-restores a single entry ----
   * THE regression behind "cannot add a second key": with exactly one saved
   * workspace, 切换工作区 must land on the LIST + 添加 Key flow — the single-
   * entry auto-restore is a BOOT(/files/)-only behavior. */
  section('WS13 single-entry switch lists instead of auto-restoring');
  {
    const wss = createWorld({ production: true });
    wss.prodDocs.me = meDoc();
    const id1 = 'ws-onelisting';
    const id2 = 'ws-secondkey';
    wss.prodDocs.workspaces = { workspaces: [WS_ENTRY(id1)] };
    await pumpUntil(() => wss.E('workspace').hidden === false, 20000, 'WS13 boot auto-restore');
    ok(wss.E('workspace').hidden === false, 'WS13 boot with one entry still auto-restores');
    /* explicit switch with the SAME single entry: must show the list + 添加 Key */
    wss.prodDocs.workspaces = { workspaces: [WS_ENTRY(id1)] };
    fire(wss.E('switch-ws'), 'click');
    await pumpUntil(() => wss.E('ws-list').hidden === false && !!wsCard(wss, id1), 20000, 'WS13 list after switch');
    ok(wss.E('workspace').hidden === true && wss.E('gate').hidden === true,
      'WS13 explicit switch with a single entry shows the LIST, never auto-re-enters it');
    ok(wss.urlLog.some((u) => u.kind === 'pushState' && u.url === '/files/'), 'WS13 the switch lands on /files/');
    ok(prodLogoutPosts(wss).length === 0, 'WS13 switching never POSTs logout');
    /* 添加 Key beside the list: a second key opens a NEW workspace */
    fire(wss.E('ws-add-key'), 'click');
    await wss.settle();
    ok(wss.E('gate').hidden === false && wss.E('ws-list').hidden === false, 'WS13 添加 API Key reveals the login form beside the list');
    wss.prodDocs.wsId = id2; // the next login provisions a new workspace entry
    wss.E('key').value = 'sk-c2a-secondkey1';
    fire(wss.E('login-form'), 'submit');
    ok(String(wss.E('key').value) === '', 'WS13 the add-key input clears immediately');
    await pumpUntil(() => wss.E('workspace').hidden === false, 20000, 'WS13 second key enters');
    const addLogin = wss.loginCalls[0];
    ok(addLogin && addLogin.api_key === 'sk-c2a-secondkey1' && addLogin.workspace_id === undefined,
      'WS13 the add-key login posts a plain {api_key} (no workspace_id), got ' + JSON.stringify(addLogin));
    ok(wss.urlLog.some((u) => u.kind === 'replaceState' && u.url === '/files/w/' + id2 + '/'),
      'WS13 the second key lands on its own NEW entry, got ' + JSON.stringify(wss.urlLog));
    /* switching again lists BOTH entries without auto-entry */
    wss.prodDocs.workspaces = { workspaces: [WS_ENTRY(id1), WS_ENTRY(id2)] };
    fire(wss.E('switch-ws'), 'click');
    await pumpUntil(() => wss.E('ws-list').hidden === false && !!wsCard(wss, id1) && !!wsCard(wss, id2), 20000, 'WS13 both entries listed');
    ok(wss.E('workspace').hidden === true && wss.E('gate').hidden === true,
      'WS13 the two-entry list shows neither gate nor an auto-entered workspace');
    ok(prodLogoutPosts(wss).length === 0, 'WS13 no logout happened anywhere in the flow');
    /* the first entry is still selectable from the list */
    const meId1Before = prodMeFetches(wss, id1).length;
    await enterWsEntry(wss, id1);
    ok(wss.E('workspace').hidden === false && prodMeFetches(wss, id1).length === meId1Before + 1,
      'WS13 the original entry remains selectable from the list');
    assertNoKeyLeak(wss, 'WS13', ALL_KEYS);
  }

  /* ---- WS14: share copy under the PRODUCTION cookie session ----
   * P2 regression guard: copyText's authenticated check must be mode-
   * independent (MOCK ? KEY : WS). With the old !!KEY guard the production
   * page (which never holds a key) silently no-op'd every copy, and the
   * mock-only F22 suite could not see it. */
  section('WS14 production share copy, fallback and stale isolation');
  async function prodShareReady(ww) {
    ww.prodDocs.me = meDoc();
    ww.prodDocs.listByPath['fx-alpha'] = { '/': listDoc('/', [entry('shareme.txt', false, 7)]) };
    ww.prodDocs.workspaces = { workspaces: [WS_ENTRY('ws-copydemo1')] };
    await pumpUntil(() => ww.E('workspace').hidden === false, 20000, 'WS14 restore');
    await enterDrive(ww, 'fx-alpha');
    await listed(ww, 'shareme.txt', 'WS14 listing');
    fire(findControls(ww, fileRow(ww, 'shareme.txt'), ['分享'], { armedOnly: true })[0], 'click');
    await ww.settle();
    fire(ww.E('share-form'), 'submit');
    await pumpUntil(() => ww.E('share-result').hidden === false, 20000, 'WS14 link generated');
    await ww.settle();
    return String(ww.E('share-url').value);
  }
  /* WS14a: secure Clipboard API success over the real transport */
  {
    const wcp = createWorld({ production: true });
    const url = await prodShareReady(wcp);
    const linkPost = (wcp.fetchLog || []).filter((f) =>
      f.init.method === 'POST' && /\/api\/v1\/links$/.test(String(f.url).split('?')[0]));
    ok(linkPost.length === 1 && String(linkPost[0].url).indexOf('/files/w/ws-copydemo1/api/v1/links') === 0,
      'WS14 the share link POST rode the workspace adapter, got ' + JSON.stringify(linkPost.map((f) => f.url)));
    ok(linkPost[0].init.credentials === 'same-origin' && !(linkPost[0].init.headers.apikey),
      'WS14 the link POST is same-origin with no apikey header');
    ok(url.includes('/l/prodtok') && wcp.E('share-result').hidden === false, 'WS14 production share URL rendered');
    wcp.copiedLog.length = 0; wcp.clipboardLog.length = 0; wcp.execLog.length = 0;
    fire(wcp.E('share-copy'), 'click');
    await wcp.settle();
    ok(writeTextCopies(wcp).some((c) => c.text === url),
      'WS14 production copy puts the EXACT share URL on the clipboard, got ' + JSON.stringify(copiedTexts(wcp)));
    ok(shareCopySaysCopied(wcp), 'WS14 production copy shows 已复制, got ' + JSON.stringify(shareCopyLabel(wcp)));
    ok(!wcp.execLog.includes('copy'), 'WS14 no execCommand fallback when the async API works');
    ok(wcp.textOf(wcp.E('share-err')).trim() === '', 'WS14 no inline error on a successful production copy');
  }
  /* WS14b: insecure-context legacy fallback still works in production */
  {
    const wfb = createWorld({ production: true, noClipboard: true, insecure: true });
    const url = await prodShareReady(wfb);
    wfb.copiedLog.length = 0; wfb.execLog.length = 0;
    fire(wfb.E('share-copy'), 'click');
    await wfb.settle();
    const ex = execCopies(wfb);
    ok(writeTextCopies(wfb).length === 0 && ex.length === 1 && ex[0].text === url,
      'WS14 production LAN fallback copies the exact URL via execCommand, got ' + JSON.stringify(copiedTexts(wfb)));
    ok(ex.length === 1 && ex[0].inDialog === true && ex[0].activeId === 'share-url',
      'WS14 the legacy fallback selected #share-url INSIDE the modal, got ' + JSON.stringify(ex));
    ok(shareCopySaysCopied(wfb), 'WS14 fallback success shows 已复制, got ' + JSON.stringify(shareCopyLabel(wfb)));
  }
  /* WS14c: a failing legacy copy claims no success and enables manual copy */
  {
    const wex = createWorld({ production: true, noClipboard: true, insecure: true, clipboard: { exec: false } });
    const url = await prodShareReady(wex);
    wex.copiedLog.length = 0;
    fire(wex.E('share-copy'), 'click');
    await wex.settle();
    ok(copiedTexts(wex).length === 0, 'WS14 a failed production copy records nothing, got ' + JSON.stringify(copiedTexts(wex)));
    ok(!shareCopySaysCopied(wex) && shareCopyLabel(wex) === '复制', 'WS14 no false 已复制, got ' + JSON.stringify(shareCopyLabel(wex)));
    ok(/未能复制|手动|Ctrl\+C|选择/.test(wex.textOf(wex.E('share-err'))),
      'WS14 a manual-copy hint is shown, got ' + JSON.stringify(wex.textOf(wex.E('share-err'))));
    ok(wex.focusLog.some((f) => f.id === 'share-url' && f.op === 'select'),
      'WS14 #share-url was selected for manual copy, focusLog=' + JSON.stringify(wex.focusLog.map((f) => f.id + ':' + f.op)));
    ok(String(wex.E('share-url').value) === url, 'WS14 the URL stays intact for manual copying');
  }
  /* WS14d: stale production copies after dialog close / logout+relogin */
  {
    const wsc = createWorld({ production: true });
    const url = await prodShareReady(wsc);
    const holdA = holdClipboard(wsc);
    fire(wsc.E('share-copy'), 'click');
    await wsc.settle();
    ok(wsc.clipboardLog.includes(url) && execCopies(wsc).length === 0,
      'WS14 the held write was handed the URL; no fallback while it is pending');
    closeShareDlg(wsc);
    await wsc.settle();
    ok(dialogIsClosed(wsc, 'dlg-share'), 'WS14 the dialog closed while the copy was pending');
    holdA.resolve();
    await wsc.settle();
    releaseClipboard(wsc);
    ok(shareCopyLabel(wsc) !== '已复制',
      'WS14 a late copy success never marks the closed dialog 已复制, got ' + JSON.stringify(shareCopyLabel(wsc)));
    /* logout/relogin isolation: the persistent entry is re-entered through the
     * list (the real UI path — an explicit logout never auto-restores). */
    const reenter = async () => {
      await logout(wsc); // → /files/ list (single entry listed, not auto-entered)
      await pumpUntil(() => wsc.E('ws-list').hidden === false && !!wsCard(wsc, 'ws-copydemo1'), 20000, 'WS14 relist');
      await enterWsEntry(wsc, 'ws-copydemo1');
      await enterDrive(wsc, 'fx-alpha');
      await listed(wsc, 'shareme.txt', 'WS14 relisted');
    };
    await reenter();
    await prodShareReady(wsc); // session 2: fresh share dialog with a result
    const holdB = holdClipboard(wsc);
    fire(wsc.E('share-copy'), 'click');
    await wsc.settle();
    await reenter(); // session 3 while session 2's write is still pending
    fire(findControls(wsc, wsc.E('file-tbody'), ['分享'], { armedOnly: true })[0], 'click');
    await wsc.settle();
    fire(wsc.E('share-form'), 'submit');
    await pumpUntil(() => wsc.E('share-result').hidden === false, 20000, 'WS14 session3 link');
    await wsc.settle();
    const labelBefore = shareCopyLabel(wsc);
    ok(!shareCopySaysCopied(wsc), 'WS14 session 3 starts with a clean 复制 label');
    holdB.resolve();
    await wsc.settle();
    releaseClipboard(wsc);
    ok(!shareCopySaysCopied(wsc) && shareCopyLabel(wsc) === labelBefore,
      'WS14 a stale production copy resolved after relogin never mutates the new session, got ' + JSON.stringify(shareCopyLabel(wsc)));
    ok(wsc.textOf(wsc.E('share-err')).trim() === '', 'WS14 no stale copy error in the new session');
    assertNoKeyLeak(wsc, 'WS14', ALL_KEYS);
  }

  dropTimers();

  /* production hygiene */
  assertNoKeyLeak(wp, 'F19', ALL_KEYS);
  const prodUrls = wp.fetchLog.map((f) => f.url).concat(wp.xhrLog.map((x) => x.url));
  ok(prodUrls.every((u) => u.startsWith('/v1/') || u.startsWith('/files/') || u.startsWith('http://127.0.0.1:18893/')), 'F19 production requests never leave the instance: ' + JSON.stringify(prodUrls));
  ok(!prodUrls.some((u) => u.includes(KEY_PROD)), 'F19 key absent from every production URL');

  /* ---- summary ---- */
  console.log('\n' + (failed ? failed + ' CHECK(S) FAILED' : 'ALL FILES-PAGE TESTS PASSED') + ' (' + passCount + ' passed, ' + failed + ' failed)');
  process.exit(failed ? 1 : 0);
}

(async () => {
  if (SELFTEST) await selftest();
  else await main();
})().catch((e) => {
  console.error('HARNESS ERROR', (e && e.stack) || e);
  process.exit(1);
});
