/* cloud2api /relay/ — 中转上传工具页（standalone, no portal dependencies).
 * This tool is NOT a storage connection and NOT a second login: it pastes an
 * API key issued by THIS instance (relay permission) into the Gokapi-compatible
 * upload/download relay. Drives — including chaining another GoAPI instance as
 * upstream storage — are configured in OpenList via the admin portal's
 * 添加网盘; their credentials never pass through here.
 *
 * Security invariants (docs/API-KEYS-AND-RELAY.md):
 *   - The API key lives in the module-local KEY variable only. It is never
 *     written to the URL/query, localStorage, sessionStorage or cookies, and
 *     is scrubbed on logout / gate return.
 *   - No admin session is created or required; the page only calls the
 *     key-authenticated /v1/me and Gokapi-compatible /api/* endpoints.
 *   - All server data is rendered through createElement/textContent — never
 *     innerHTML.
 *
 * Upload receipt contract (backend): POST /api/files/add keeps the Gokapi
 * fields ({Result, FileInfo, …}) and adds {UploadId, Status, Message} where
 * Status ∈ uploading|completed|failed|interrupted; the request carries the
 * client-generated X-Upload-ID header; GET /api/files/status/{UploadId}
 * answers {UploadId, Status, Message, FileInfo?} for the same key only.
 * "completed" is the ONLY server terminal success — browser progress 100%
 * and a sent request body never imply success. Unknown states must be shown
 * as 未确认, never as completed/failed.
 *
 * Mock: ?mock=1 routes every call to the in-memory mock below and performs
 * zero real network requests. Production never enters that branch. */
(function () {
  'use strict';

  var MOCK = false;
  try { MOCK = new URLSearchParams(location.search).get('mock') === '1'; } catch (e) { /* keep false */ }

  /* ---------- module state (key: memory only) ---------- */
  var KEY = '';
  var ME = null;        // /v1/me payload
  var CAPS = null;      // /api/info/config payload ({MaxFilesize, MaxChunksize} MiB)
  var VERSION = '';     // /api/info/version payload Version
  var lastUploadId = '';
  var activeXhr = null;
  var pollToken = 0;    // invalidates in-flight poll loops
  /* Session/attempt generations (stale-callback guards):
   *  - authGen increments on logout AND when a new session commits, so any
   *    async continuation (upload response, progress, poll tick, list fetch)
   *    captured under an older generation can never touch the UI or reveal a
   *    previous key's metadata.
   *  - loginSeq makes the newest doLogin attempt win and lets logout cancel
   *    in-flight logins.
   *  - attemptSeq/currentAttempt mark the one upload attempt allowed to
   *    finish; every attempt gets its own immutable X-Upload-ID. */
  var authGen = 0;
  var loginSeq = 0;
  var attemptSeq = 0;
  var currentAttempt = 0;
  var uploadBusy = false;

  /* ---------- tiny DOM helpers (textContent only) ---------- */
  function h(tag, attrs) {
    var el = document.createElement(tag);
    if (attrs) {
      for (var k of Object.keys(attrs)) {
        var v = attrs[k];
        if (v === null || v === undefined || v === false) continue;
        if (k === 'class') el.className = v;
        else if (v === true) el.setAttribute(k, '');
        else el.setAttribute(k, String(v));
      }
    }
    for (var i = 2; i < arguments.length; i++) appendChild(el, arguments[i]);
    return el;
  }
  function appendChild(el, c) {
    if (c === null || c === undefined || c === false || c === true) return;
    if (Array.isArray(c)) { for (var x of c) appendChild(el, x); return; }
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); }
  function $(id) { return document.getElementById(id); }

  function fmtBytes(n) {
    if (n === null || n === undefined || n === '' || !isFinite(Number(n))) return '—';
    n = Number(n);
    if (n === 0) return '0 B';
    var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    var i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2)) + ' ' + units[i];
  }

  /** Random upload id (UUIDv4 shape) from crypto.getRandomValues — available
   * on plain HTTP too, so LAN deployments are covered. */
  function uuid() {
    var b = new Uint8Array(16);
    if (window.crypto && window.crypto.getRandomValues) window.crypto.getRandomValues(b);
    else for (var i = 0; i < 16; i++) b[i] = Math.floor(Math.random() * 256);
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    var hex = '';
    for (var j = 0; j < 16; j++) hex += (b[j] + 0x100).toString(16).slice(1);
    return hex.slice(0, 8) + '-' + hex.slice(8, 12) + '-' + hex.slice(12, 16) + '-' + hex.slice(16, 20) + '-' + hex.slice(20);
  }

  async function copyText(text, okMsg) {
    var ok = false;
    try {
      if (navigator.clipboard && window.isSecureContext) { await navigator.clipboard.writeText(text); ok = true; }
    } catch (e) { /* fall through */ }
    if (!ok) {
      try {
        var ta = document.createElement('textarea');
        ta.value = text;
        ta.style.cssText = 'position:fixed;top:-100px;opacity:0';
        document.body.appendChild(ta);
        ta.select();
        ok = document.execCommand('copy');
        ta.remove();
      } catch (e) { ok = false; }
    }
    setStatus(ok ? (okMsg || '已复制到剪贴板') : '复制失败，请手动选择复制', ok ? '' : 'bad');
    return ok;
  }

  /* ---------- sanitized error mapping (never echoes the key) ---------- */
  function serverMessage(json, fallback) {
    if (json && typeof json.Message === 'string' && json.Message) return json.Message;
    if (json && typeof json.message === 'string' && json.message) return json.message;
    return fallback || '请求失败';
  }
  function friendlyError(status, json) {
    if (status === 401) return '密钥无效或已过期：请退出后重新粘贴正确的 API Key';
    if (status === 403) return '该 Key 没有中转权限（relay）：请在管理门户为它启用中转或选择含中转的权限组';
    if (status === 0) return '网络错误：无法连接中转服务';
    return serverMessage(json, 'HTTP ' + status);
  }

  /* ---------- transport ---------- */
  async function jsonReq(method, path, keyOpt) {
    if (MOCK) return mock.json(method, path);
    var headers = {};
    var apiKey = keyOpt !== undefined ? keyOpt : KEY;
    if (apiKey) headers['apikey'] = apiKey;
    var res;
    try {
      res = await fetch(path, { method: method, headers: headers, credentials: 'omit' });
    } catch (e) {
      return { status: 0, json: null };
    }
    var json = null;
    try { json = await res.json(); } catch (e) { /* non-JSON */ }
    return { status: res.status, json: json };
  }

  /** Multipart upload via XHR (progress + abort). Field names are the ones
   * the Gokapi-compat backend parses: `file`, `expiryDays`,
   * `allowedDownloads` (0/absent = unlimited). Carries X-Upload-ID so the
   * final status stays queryable after aborts/disconnects. */
  function uploadReq(file, fields, uploadId, onProgress) {
    if (MOCK) return mock.upload(file, fields, uploadId, onProgress);
    return new Promise(function (resolve) {
      var fd = new FormData();
      fd.append('file', file, file.name);
      if (fields.expiryDays) fd.append('expiryDays', String(fields.expiryDays));
      if (fields.allowedDownloads !== null && fields.allowedDownloads !== undefined && fields.allowedDownloads !== '') {
        fd.append('allowedDownloads', String(fields.allowedDownloads));
      }
      var xhr = new XMLHttpRequest();
      activeXhr = xhr;
      xhr.open('POST', '/api/files/add');
      xhr.setRequestHeader('apikey', KEY);
      xhr.setRequestHeader('X-Upload-ID', uploadId);
      if (xhr.upload) {
        xhr.upload.onprogress = function (e) {
          if (e.lengthComputable && onProgress) onProgress(e.loaded, e.total);
        };
      }
      xhr.onload = function () {
        var json = null;
        try { json = JSON.parse(xhr.responseText); } catch (e) { /* non-JSON */ }
        resolve({ kind: 'response', status: xhr.status, json: json });
      };
      xhr.onerror = function () { resolve({ kind: 'network' }); };
      xhr.onabort = function () { resolve({ kind: 'aborted' }); };
      xhr.send(fd);
    });
  }

  /* ---------- receipt polling ----------
   * Bounded polling of GET /api/files/status/{id}. Resolves
   *   {Status, UploadId, Message, FileInfo?} on a server answer, or
   *   {unknown:true} when the receipt stays unavailable (404 / network /
   *   attempts exhausted while still uploading) — callers must render that
   *   as 未确认, never as success or failure. */
  function pollStatus(uploadId, attempts, delayMs, gen0) {
    attempts = attempts || 20;
    delayMs = delayMs || 1500;
    var token = ++pollToken;
    return new Promise(function (resolve) {
      var i = 0;
      function tick() {
        if (token !== pollToken) return resolve({ unknown: true, superseded: true });
        if (gen0 !== undefined && authGen !== gen0) return resolve({ unknown: true, superseded: true });
        jsonReq('GET', '/api/files/status/' + encodeURIComponent(uploadId)).then(function (res) {
          if (token !== pollToken) return resolve({ unknown: true, superseded: true });
          if (gen0 !== undefined && authGen !== gen0) return resolve({ unknown: true, superseded: true });
          if (res.status === 401) return resolve({ unknown: true, unauthorized: true });
          if (res.status === 200 && res.json && res.json.Status) {
            if (res.json.Status === 'uploading' && i < attempts - 1) { i++; setTimeout(tick, delayMs); return; }
            return resolve(res.json); // completed/failed/interrupted, or still-uploading after the last try
          }
          if (i < attempts - 1) { i++; setTimeout(tick, delayMs); return; }
          resolve({ unknown: true });
        });
      }
      tick();
    });
  }

  /* ---------- mock backend (?mock=1 only, zero real requests) ---------- */
  var mock = {
    seq: 1,
    files: [],
    pending: {},     // uploadId -> {Status:'uploading'}
    completed: {},   // uploadId -> file entry (receipt lookup by X-Upload-ID)
    /* Scripted overrides for deterministic regression tests (mock mode only;
     * never consulted in production). e.g.
     *   __RELAY_MOCK.on['GET /v1/me'] = async () => ({ status: 200, json: {...} })
     *   __RELAY_MOCK.on['GET /api/files/status/*'] = async (id) => ({...})
     *   __RELAY_MOCK.on['POST /api/files/add'] = async (file, fields, id, onProgress) => ({kind:'response',...}) */
    on: {},
    ensure: function () {
      if (this.seeded) return;
      this.seeded = true;
      var now = Date.now();
      this.files.push({
        Id: 'mockfile0001', Name: 'sample-report.pdf', Size: '1.2 MiB', SizeBytes: 1258291,
        CRC64: '9125668143435234567', ContentType: 'application/pdf',
        ExpireAtString: new Date(now + 6 * 86400e3).toISOString().replace('T', ' ').slice(0, 16) + ' UTC',
        UrlDownload: location.pathname + '?mock=1#d-mock-0001', UrlHotlink: '',
        UploadDate: Math.floor(now / 1000), ExpireAt: Math.floor((now + 6 * 86400e3) / 1000),
        DownloadsRemaining: 0, DownloadCount: 2, UnlimitedDownloads: true,
      });
    },
    me: function () {
      this.ensure();
      var used = 1258291;
      return {
        key: { id: 'mock-key', prefix: 'sk-c2a-mockpre1', default_drive: 'mock-drive', last_selftest_at: null },
        drives: [{ id: 'mock-drive', root: '/demo', state: 'healthy' }],
        global_perms: ['relay'],
        limits: { max_upload_bytes: 2 * 1024 ** 3, requests_per_minute: 60, concurrent_jobs: 1 },
        trash: { enabled: true, days: 7 },
        relay: {
          enabled: true, drive: 'mock-drive', quota_bytes: 20 * 1024 ** 3,
          max_age_days: 7, used_bytes: used, files: this.files.length,
          instructions: '#mock',
        },
        policy_text: '演示模式：单文件上限 2 GiB；/.c2a-trash 保留 7 天。',
      };
    },
    json: async function (method, path) {
      var self = this;
      await new Promise(function (r) { setTimeout(r, 120); });
      var override = this.on[method + ' ' + path];
      if (override) return override();
      if (method === 'GET' && path.indexOf('/api/files/status/') === 0 && this.on['GET /api/files/status/*']) {
        return this.on['GET /api/files/status/*'](decodeURIComponent(path.slice('/api/files/status/'.length)));
      }
      if (path === '/v1/me' && method === 'GET') return { status: 200, json: self.me() };
      if (path === '/api/info/version' && method === 'GET') return { status: 200, json: { Version: 'mock-0.0.0 (mock)', VersionInt: 0, EndToEndEncryptionEnabled: false } };
      if (path === '/api/info/config' && method === 'GET') return { status: 200, json: { MaxFilesize: 2048, MaxChunksize: 45 } };
      if (path === '/api/files/list' && method === 'GET') {
        this.ensure();
        return { status: 200, json: this.files.slice() };
      }
      var m = /^\/api\/files\/status\/([^/]+)$/.exec(path);
      if (m && method === 'GET') {
        var id = decodeURIComponent(m[1]);
        var p = this.pending[id];
        if (p) return { status: 200, json: { UploadId: id, Status: p.Status, Message: p.Message || '' } };
        var byUpload = this.completed[id];
        if (byUpload) return { status: 200, json: { UploadId: id, Status: 'completed', Message: '', FileInfo: byUpload } };
        var f = this.files.find(function (x) { return x.Id === id; });
        if (f) return { status: 200, json: { UploadId: id, Status: 'completed', Message: '', FileInfo: f } };
        return { status: 404, json: { Message: 'unknown upload id (mock)' } };
      }
      return { status: 404, json: { Message: 'not found (mock)' } };
    },
    upload: async function (file, fields, uploadId, onProgress) {
      var self = this;
      this.ensure();
      if (this.on['POST /api/files/add']) {
        return this.on['POST /api/files/add'](file, fields, uploadId, onProgress);
      }
      // simulate the documented non-blocking flow: respond uploading, then
      // let /api/files/status converge to completed
      await new Promise(function (r) { setTimeout(r, 300); });
      for (var p = 1; p <= 4; p++) {
        if (onProgress) onProgress(Math.round(file.size * p / 4), file.size);
        await new Promise(function (r) { setTimeout(r, 120); });
      }
      var id = 'mockfile' + String(self.seq++).padStart(4, '0');
      var expiryMs = (Number(fields.expiryDays) || 7) * 86400e3;
      var entry = {
        Id: id, Name: file.name, Size: fmtBytes(file.size), SizeBytes: file.size,
        CRC64: '10000000000000000' + String(self.seq % 10), ContentType: file.type || 'application/octet-stream',
        ExpireAtString: new Date(Date.now() + expiryMs).toISOString().replace('T', ' ').slice(0, 16) + ' UTC',
        UrlDownload: location.pathname + '?mock=1#d-mock-' + id, UrlHotlink: '',
        UploadDate: Math.floor(Date.now() / 1000), ExpireAt: Math.floor((Date.now() + expiryMs) / 1000),
        DownloadsRemaining: Number(fields.allowedDownloads) || 0,
        DownloadCount: 0, UnlimitedDownloads: !Number(fields.allowedDownloads),
      };
      this.pending[uploadId] = { Status: 'uploading', Message: 'received (mock)' };
      setTimeout(function () {
        delete self.pending[uploadId];
        self.completed[uploadId] = entry;
        self.files.unshift(entry);
      }, 2500);
      return { kind: 'response', status: 200, json: { Result: 'OK', UploadId: uploadId, Status: 'uploading', Message: 'received (mock)', FileInfo: null, IncludeFilename: false } };
    },
  };

  /* ---------- gate / auth flow ---------- */
  async function doLogin(e) {
    if (e) e.preventDefault();
    var btn = $('btn');
    var err = $('err');
    var myKey = $('key').value.trim();
    err.textContent = '';
    if (!myKey) { err.textContent = '请粘贴 API Key'; return false; }
    // The newest attempt wins; logout bumps loginSeq to cancel in-flight logins.
    // The candidate key stays local until every probe passed — a superseded or
    // failed attempt never mutates KEY/ME of the winning session.
    var myLogin = ++loginSeq;
    btn.disabled = true; btn.textContent = '进入中…';
    var me = await jsonReq('GET', '/v1/me', myKey);
    if (myLogin !== loginSeq) return false;
    if (me.status !== 200 || !me.json || !me.json.key) {
      btn.disabled = false; btn.textContent = '进入工具';
      err.textContent = friendlyError(me.status, me.json);
      return false;
    }
    var ver = await jsonReq('GET', '/api/info/version', myKey);
    if (myLogin !== loginSeq) return false;
    var version = (ver.status === 200 && ver.json && ver.json.Version) ? String(ver.json.Version) : '';
    if (ver.status === 401) { btn.disabled = false; btn.textContent = '进入工具'; err.textContent = friendlyError(401, ver.json); return false; }
    var caps = await jsonReq('GET', '/api/info/config', myKey);
    if (myLogin !== loginSeq) return false;
    if (caps.status === 401) { btn.disabled = false; btn.textContent = '进入工具'; err.textContent = friendlyError(401, caps.json); return false; }
    // commit: a new authenticated session begins (invalidates old callbacks)
    KEY = myKey;
    authGen++;
    ME = me.json;
    VERSION = version;
    CAPS = (caps.status === 200 && caps.json) ? caps.json : null;
    $('key').value = '';
    showApp();
    btn.disabled = false; btn.textContent = '进入工具';
    return false;
  }

  function logout(gateMsg) {
    // invalidate EVERYTHING in flight: pending logins, upload continuations,
    // poll loops and list renders. A superseded callback must never clear the
    // credentials of, start work for, or render into a newer session.
    loginSeq++;
    authGen++;
    pollToken++;
    if (activeXhr) { try { activeXhr.abort(); } catch (e) { /* resolved via onabort */ } activeXhr = null; }
    KEY = ''; ME = null; CAPS = null; VERSION = ''; lastUploadId = '';
    $('key').value = '';
    $('err').textContent = typeof gateMsg === 'string' ? gateMsg : ''; // preserved friendly error (e.g. 401 mid-session)
    clear($('me-kv'));
    clear($('files-host'));
    clear($('result-body'));
    $('result-card').hidden = true;
    $('upload-status').textContent = '';
    $('relay-warn').hidden = true;
    $('app').hidden = true;
    $('gate').hidden = false;
    $('file').value = '';
    $('progress').hidden = true;
    $('progress-bar').style.width = '0%';
    $('cancel-btn').hidden = true;
    $('upload-btn').disabled = false;
    $('file').disabled = false;
    $('recheck-btn').disabled = false;
    $('recheck-btn').hidden = true;
    uploadBusy = false;
  }

  /* ---------- app rendering ---------- */
  function kvRow(dt, dd) { return [h('dt', null, dt), h('dd', null, dd)]; }
  function mono(t) { return h('span', { class: 'mono' }, t); }

  function showApp() {
    $('gate').hidden = true;
    $('app').hidden = false;
    var prefix = (ME.key && ME.key.prefix) ? ME.key.prefix + '-…' : (ME.key && ME.key.id) || '—';
    $('me-prefix').textContent = prefix + (MOCK ? ' · 演示模式（不发真实请求）' : '');

    var relay = ME.relay || {};
    var warnEl = $('relay-warn');
    if (!relay.enabled) {
      clear(warnEl);
      warnEl.append('该 Key 当前没有生效的中转权限（relay）：上传与文件列表不可用。请在管理门户为该 Key 选择含中转的权限组（通用/全部）或勾选 relay 权限。');
      warnEl.hidden = false;
      $('upload-btn').disabled = true;
    } else {
      warnEl.hidden = true;
      $('upload-btn').disabled = false;
    }

    var kv = $('me-kv');
    clear(kv);
    var relayGrant = Array.isArray(ME.drives) ? ME.drives.find(function (drive) { return drive.id === relay.drive; }) : null;
    var quota = relay.quota_bytes > 0 ? fmtBytes(relay.quota_bytes) : '200 GiB（默认）';
    var uploadCap = (ME.limits && ME.limits.max_upload_bytes > 0)
      ? fmtBytes(ME.limits.max_upload_bytes)
      : (CAPS && CAPS.MaxFilesize > 0 ? CAPS.MaxFilesize + ' MiB（Gokapi 兼容默认）' : '—');
    appendChild(kv, [
      kvRow('默认网盘', mono((ME.key && ME.key.default_drive) || '—')),
      kvRow('中转目标网盘', relay.enabled ? mono(relay.drive || '—') : '未启用'),
      kvRow('中转根目录', relay.enabled && relayGrant ? mono(relayGrant.root || '/') : '—'),
      kvRow('中转容量', relay.enabled ? fmtBytes(relay.used_bytes || 0) + ' / ' + quota : '—'),
      kvRow('保留期', relay.enabled ? (relay.max_age_days > 0 ? relay.max_age_days + ' 天' : '7 天（默认）') : '—'),
      kvRow('中转文件数', relay.enabled ? String(relay.files || 0) : '—'),
      kvRow('单文件上限', uploadCap),
      VERSION ? kvRow('兼容层版本', h('span', { class: 'mono', title: VERSION }, VERSION.split(' ')[0])) : null,
    ]);
    renderList();
  }

  /* ---------- upload flow ---------- */
  function setStatus(text, cls) {
    var el = $('upload-status');
    el.textContent = text || '';
    el.className = 'status-line' + (cls ? ' status-' + cls : '');
  }
  function setProgress(pct) {
    $('progress').hidden = pct === null;
    $('progress-bar').style.width = Math.max(0, Math.min(100, pct)) + '%';
  }

  async function startUpload() {
    var fileInput = $('file');
    var file = fileInput.files && fileInput.files[0];
    if (!file) { setStatus('请先选择要上传的文件', 'bad'); return; }
    if (file.size === 0) { setStatus('不能上传空文件', 'bad'); return; }
    var relay = (ME && ME.relay) || {};
    if (!relay.enabled) { setStatus('该 Key 没有中转权限（relay），无法上传', 'bad'); return; }

    // Receipt IDs are immutable per attempt: every new upload gets a fresh
    // X-Upload-ID (never reuse a previous attempt's id for a new body), and
    // any poll still running for a superseded attempt is cancelled so it
    // cannot clobber this attempt's result card.
    pollToken++;
    var gen0 = authGen;             // session guard: logout/new login invalidates this attempt
    var myAttempt = ++attemptSeq;   // only the current attempt may finish
    currentAttempt = myAttempt;
    uploadBusy = true;
    $('recheck-btn').disabled = true;
    $('recheck-btn').hidden = true;
    $('result-card').hidden = true;
    clear($('result-body'));

    var uploadId = uuid();
    lastUploadId = uploadId;
    var fields = {
      expiryDays: Number($('expiry').value) || 7,
      allowedDownloads: $('downloads').value, // '' = unlimited (absent → 0)
    };
    $('upload-btn').disabled = true;
    $('cancel-btn').hidden = false;
    $('file').disabled = true;
    setProgress(0);
    setStatus('上传中 0%（进度 100% 不代表完成，需等待服务器确认）…');

    var res = await uploadReq(file, fields, uploadId, function (loaded, total) {
      if (authGen !== gen0 || currentAttempt !== myAttempt) return; // stale progress never touches the UI
      var pct = total > 0 ? Math.round(loaded / total * 100) : 0;
      setProgress(pct);
      setStatus('传输中 ' + pct + '% — 请求体已发送 ' + (pct >= 100 ? '，等待服务器确认…' : '（进度 100% 不代表完成）'));
    });
    if (authGen !== gen0 || currentAttempt !== myAttempt) return; // logout/new attempt owns the UI now

    if (res.kind === 'aborted') {
      setStatus('已取消：正在用上传 ID 查询最终状态…');
      var after = await pollStatus(uploadId, 10, 1500, gen0);
      finishResult(after, uploadId, '上传已取消', gen0);
      return;
    }
    if (res.kind === 'network') {
      setStatus('连接中断：正在用上传 ID 查询最终状态…');
      var afterNet = await pollStatus(uploadId, 20, 2000, gen0);
      finishResult(afterNet, uploadId, '网络中断', gen0);
      return;
    }
    if (res.status === 401) {
      // dead session: return to the gate, keeping the friendly error visible
      logout(friendlyError(401, res.json));
      return;
    }
    if (res.status < 200 || res.status >= 300) {
      // The backend may attach a real receipt to an error response — render it
      // verbatim so the model gets a copyable failed/interrupted result with
      // its ID. Without a receipt the upload is honestly "rejected": never a
      // fabricated completed/failed receipt.
      var jErr = res.json || {};
      var stErr = String(jErr.Status || '').toLowerCase();
      if ((stErr === 'failed' || stErr === 'interrupted') && jErr.UploadId) {
        lastUploadId = String(jErr.UploadId);
        finishResult({ UploadId: jErr.UploadId, Status: stErr, Message: jErr.Message || '', FileInfo: jErr.FileInfo || null }, lastUploadId, '', gen0);
        return;
      }
      var rejMsg = friendlyError(res.status, res.json);
      if (jErr.UploadId) {
        lastUploadId = String(jErr.UploadId);
        setStatus('服务器返回错误：正在查询上传回执…');
        var errorDoc = await pollStatus(lastUploadId, 15, 2000, gen0);
        finishResult(errorDoc, lastUploadId, rejMsg, gen0);
        return;
      }
      resetUploadUi();
      setStatus('上传被拒绝：' + rejMsg, 'bad');
      renderResult({ status: 'rejected', uploadId: '', message: rejMsg, info: null });
      return;
    }
    var j = res.json || {};
    if (j.UploadId) lastUploadId = String(j.UploadId);
    // ONLY the server's terminal Status may confirm an upload. A 200 that
    // carries FileInfo but no Status is NOT completion — treat it as
    // unconfirmed and query the upload ID for the truth.
    var status = String(j.Status || '').toLowerCase();
    if (status === 'completed') { finishResult(j, lastUploadId, '', gen0); return; }
    if (status === 'failed' || status === 'interrupted') { finishResult(j, lastUploadId, '', gen0); return; }
    if (status === 'uploading') {
      setProgress(100);
      setStatus('服务器已接收（非阻塞）：正在等待终态…');
      var finalDoc = await pollStatus(lastUploadId, 60, 2000, gen0);
      finishResult(finalDoc, lastUploadId, '', gen0);
      return;
    }
    setStatus('服务器响应缺少上传状态：正在用上传 ID 查询…');
    var probe = await pollStatus(lastUploadId, 20, 1500, gen0);
    finishResult(probe, lastUploadId, '服务器响应缺少上传状态', gen0);
  }

  function resetUploadUi() {
    uploadBusy = false;
    setProgress(null);
    $('upload-btn').disabled = !((ME && ME.relay || {}).enabled);
    $('cancel-btn').hidden = true;
    $('file').disabled = false;
  }

  /** Normalize any terminal/unknown document into the result card. `cause`
   * describes the transport event (取消/网络中断/''）. Superseded polls (the
   * attempt they were polling was abandoned for a newer one) never render, and
   * neither does anything from a superseded session (authGen mismatch): the
   * currently logged-in key's UI is never mutated by stale callbacks. */
  function finishResult(doc, uploadId, cause, gen0) {
    if (gen0 !== undefined && authGen !== gen0) return;
    if (doc && doc.superseded) return;
    if (doc && doc.unauthorized) { logout(friendlyError(401, null)); return; }
    resetUploadUi();
    var r;
    if (doc && !doc.unknown && doc.Status) {
      var st = String(doc.Status).toLowerCase();
      r = {
        status: st,
        uploadId: String(doc.UploadId || uploadId || ''),
        message: String(doc.Message || '') + (cause ? '（' + cause + '）' : ''),
        info: doc.FileInfo || null,
        stillUploading: st === 'uploading',
      };
    } else {
      r = {
        status: 'unknown',
        uploadId: uploadId,
        message: (cause ? cause + '：' : '') + '服务器没有返回该上传 ID 的结果，最终状态未确认',
        info: null,
        stillUploading: false,
      };
    }
    lastUploadId = r.uploadId || lastUploadId;
    renderResult(r);
    if (ME) renderList();
  }

  function resultPayload(r) {
    var hints = {
      completed: null,
      failed: '可根据 Message 修正后重新上传；新上传必须使用新的 X-Upload-ID（原上传 ID 的结果不可复用）',
      interrupted: '重试前先查询原上传结果（GET /api/files/status/' + (r.uploadId || '<upload_id>') + '），远端可能已有部分或完整文件；如需重传必须使用新的 X-Upload-ID，不要宣称已回滚',
      uploading: '尚未到达终态：稍后用同一 Key 查询 GET /api/files/status/' + (r.uploadId || '<upload_id>') + '，不要按“已上传完成”处理；重新上传属于新尝试，需新的 X-Upload-ID',
      unknown: '结果未确认：不要断言上传成功，也不要断言远端没有文件；稍后用同一 Key 查询 GET /api/files/status/' + (r.uploadId || '<upload_id>') + '，重传前先查原结果并使用新的 X-Upload-ID',
      rejected: '请求被拒绝，服务端未生成上传回执：不要声称任何文件已上传；可根据 message 修正后使用新的 X-Upload-ID 重新上传',
    };
    return {
      upload_id: r.uploadId || null,
      status: r.status,
      confirmed: r.status === 'completed',
      message: r.message || '',
      file: r.info ? {
        id: r.info.Id,
        name: r.info.Name,
        size: r.info.Size,
        size_bytes: r.info.SizeBytes,
        crc64: r.info.CRC64,
        sha256: r.info.SHA256 || null,
        download_page: r.info.UrlDownload,
        expires_at: r.info.ExpireAtString,
        downloads_remaining: r.info.DownloadsRemaining,
        unlimited_downloads: !!r.info.UnlimitedDownloads,
      } : null,
      retry_hint: hints[r.status] || null,
      query: 'GET /api/files/status/' + (r.uploadId || '<upload_id>') + '（请求头 apikey）',
    };
  }

  var STATUS_LABEL = {
    completed: '已完成（服务器确认）',
    failed: '失败',
    interrupted: '已中断',
    uploading: '处理中（未到终态）',
    unknown: '结果未确认',
    rejected: '已拒绝（未生成服务端回执）',
  };

  function renderResult(r) {
    var host = $('result-body');
    clear(host);
    $('result-card').hidden = false;
    $('recheck-btn').hidden = !(r.status === 'uploading' || r.status === 'unknown');
    $('recheck-btn').disabled = false;

    var lines = [];
    if (r.info) {
      lines.push('文件 ' + (r.info.Name || '') + '（' + (r.info.Size || fmtBytes(r.info.SizeBytes)) + '）');
      if (r.info.CRC64) lines.push('CRC64 ' + r.info.CRC64);
      if (r.info.SHA256) lines.push('SHA256 ' + r.info.SHA256);
      if (r.info.UrlDownload) lines.push('下载页 ' + r.info.UrlDownload);
    }
    var human = h('p', { class: 'result-msg' }, lines.length ? lines.join(' · ') : (r.message || STATUS_LABEL[r.status] || ''));

    host.append(
      h('span', { class: 'result-status ' + r.status }, STATUS_LABEL[r.status] || r.status),
      human,
      h('div', { class: 'snippet-title' }, '给模型 / agent 的结果 JSON'),
      h('pre', { class: 'jsonbox' }, JSON.stringify(resultPayload(r), null, 2)));
    if (r.status === 'unknown') {
      host.append(h('div', { class: 'banner unknown' },
        '结果未确认：该上传 ID 暂无服务器记录或查询超时。不能断言上传成功；远端也可能已有部分或完整文件。可稍后点「重新查询该上传 ID」。'));
    }
    if (r.status === 'interrupted') {
      host.append(h('div', { class: 'banner warn' },
        '上传被中断：远端可能已有部分或完整文件；不能宣称已回滚或不存在。重试前请先查询原上传结果。'));
    }
  }

  /* ---------- file list ---------- */
  async function renderList() {
    var gen0 = authGen; // a stale list must never reveal a previous key's files
    var host = $('files-host');
    clear(host);
    host.append(h('p', { class: 'muted' }, '加载中…'));
    var res = await jsonReq('GET', '/api/files/list');
    if (authGen !== gen0) return; // superseded by logout/new login
    clear(host);
    if (res.status === 401) { logout(friendlyError(401, res.json)); return; }
    if (res.status !== 200) {
      host.append(h('div', { class: 'banner bad' }, friendlyError(res.status, res.json)));
      return;
    }
    var files = Array.isArray(res.json) ? res.json : [];
    if (!files.length) { host.append(h('div', { class: 'empty' }, '暂无中转文件')); return; }
    var tbody = h('tbody');
    for (var f of files) {
      var copyBtn = h('button', { class: 'btn-ghosty', type: 'button', title: '复制下载页链接' }, '复制链接');
      var url = String(f.UrlDownload || '');
      copyBtn.addEventListener('click', function (u) { return function () { copyText(u, '下载页链接已复制'); }; }(url));
      tbody.append(h('tr', null,
        h('td', { class: 'wrap mono' }, String(f.Name || '')),
        h('td', null, String(f.Size || fmtBytes(f.SizeBytes))),
        h('td', null, String(f.ExpireAtString || '')),
        h('td', null, f.UnlimitedDownloads ? '不限' : String(f.DownloadsRemaining != null ? f.DownloadsRemaining : '—')),
        h('td', null,
          copyBtn,
          url ? h('a', { class: 'btn-ghosty', href: url, target: '_blank', rel: 'noopener noreferrer', style: 'text-decoration:none' }, '打开') : null)));
    }
    host.append(h('div', { class: 'table-wrap' },
      h('table', null,
        h('thead', null, h('tr', null,
          h('th', null, '文件'), h('th', null, '大小'), h('th', null, '过期时间'), h('th', null, '剩余下载'), h('th', null, '操作'))),
        tbody)));
  }

  /* ---------- boot ---------- */
  function boot() {
    // CSP (script-src 'self') forbids inline handlers — bind everything here
    $('login-form').addEventListener('submit', function (e) { e.preventDefault(); doLogin(e); });
    $('key-toggle').addEventListener('click', function () {
      var inp = $('key');
      var show = inp.type === 'password';
      inp.type = show ? 'text' : 'password';
      $('key-toggle').textContent = show ? '隐藏' : '显示';
    });
    $('logout').addEventListener('click', function () { logout(); });
    $('upload-btn').addEventListener('click', startUpload);
    $('cancel-btn').addEventListener('click', function () {
      if (activeXhr) { try { activeXhr.abort(); } catch (e) { /* resolved via onabort */ } }
    });
    $('refresh-btn').addEventListener('click', renderList);
    $('copy-result').addEventListener('click', function () {
      var pre = document.querySelector('#result-body .jsonbox');
      if (pre) copyText(pre.textContent, '结果 JSON 已复制');
    });
    $('recheck-btn').addEventListener('click', async function () {
      if (!lastUploadId || uploadBusy || $('recheck-btn').disabled) return;
      var gen0 = authGen;
      $('recheck-btn').disabled = true;
      setStatus('正在查询上传 ID ' + lastUploadId + ' …');
      var doc = await pollStatus(lastUploadId, 15, 2000, gen0);
      if (authGen !== gen0) return; // session changed while querying
      $('recheck-btn').disabled = false;
      finishResult(doc, lastUploadId, '', gen0);
    });
    // expose for programmatic/testing use (no inline handlers due to CSP)
    window.doLogin = doLogin;
    if (MOCK) window.__RELAY_MOCK = mock; // scripted overrides, mock mode only
    if (MOCK) {
      var hint = document.querySelector('.brand-sub');
      if (hint) hint.textContent += ' · 演示模式';
    }
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
