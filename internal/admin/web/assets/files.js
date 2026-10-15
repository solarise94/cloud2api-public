/* cloud2api /files/ — 人类可用的网盘文件空间页（standalone, no admin app deps).
 *
 * 这是 API Key 的领域：粘贴本实例签发的 API Key（sk-c2a-…）浏览 / 上传 / 下载 /
 * 分享该 Key 已授权的网盘。不是管理员登录，也不是 OpenList 登录；不调用任何
 * /admin/* 数据接口。
 *
 * 持久工作区（正式模式，无 ?mock=1）：
 *   - 登录 POST /files/api/login {api_key, workspace_id?} → {workspace_id,url,
 *     expires_at}；之后浏览器凭 HttpOnly Cookie（c2a_files）访问该工作区。
 *   - 数据面走同源适配 /files/w/<id>/api/v1/<suffix>，请求固定带
 *     X-C2A-Workspace: 1，fetch credentials:'same-origin'，上传 XHR 同源携带
 *     Cookie；页面不再持有原始 Key（KEY 恒为空），登录输入框提交后立即清空。
 *   - /files/ 启动先 GET /files/api/workspaces 恢复本浏览器的入口列表：
 *     恰好一个有效入口时 history.replaceState 直入其 /files/w/<id>/；多个入口
 *     时列表可选 + 添加 Key；零入口时直接显示登录门。显式「切换工作区」与
 *     退出后一律只列列表/添加 Key，不自动恢复单项——否则单 Key 用户永远
 *     无从添加第二个 Key。
 *   - 服务器返回的入口 URL 只接受与入口格式一致的相对路径
 *     （/files/w/<id>/，与 id 一致），其余一律拒绝，不信任任何重定向目标。
 *
 * 安全不变量：
 *   - 正式模式 Key 只在登录请求体里出现一次，用完立即清输入与局部变量，
 *     绝不写入 URL/查询串、localStorage、sessionStorage、Cookie 或下载链接。
 *     Mock 模式仍用内存 KEY（mockKEY）驱动内存后端，零真实网络。
 *   - 原生下载只使用短时单文件凭证；不使用 Blob/对象 URL 缓冲整文件。
 *   - 所有服务器数据一律 createElement/textContent 渲染，绝不 innerHTML。
 *   - authGen（会话代）与 navGen（浏览代）快照使过期响应、过期列表、过期
 *     对话框、过期上传回调无法写入 UI；登出/切换/重登后无法复制/下载。
 *   - pagehide 与登出都会清除内存状态并中止本页在途 API 请求；pagehide 不向
 *     服务端注销持久会话；退出只 POST 当前工作区的 /files/w/<id>/api/logout。
 *   - 会话失效（401）回到该入口的重登录门，登录自动携带 workspace_id，
 *     不能用另一个 Key 覆盖既有入口。
 *   - 上传成功只认服务器 2xx + 完整 JSON {path,size,crc64,sha256,conflict} 且
 *     path/size 与目标一致；2xx 无字段 / 响应畸形 = 未确认；网络中断/取消 =
 *     未知。两者都不算成功、不回滚、不盲目重试。100% 传输只是发送完毕，
 *     仍在等待上游确认。零字节文件走 multipart（FormData file 字段），避免
 *     后端拒绝 Content-Length<=0 的裸 PUT。
 *
 * Mock：?mock=1 时所有请求路由到内存 mock（零真实网络请求，不伪造持久
 * Cookie，不调用 /files/api/*），并仅在此时暴露
 * window.__FILES_MOCK = { map, calls, reset, keys }。
 */
(function () {
  'use strict';

  /* ---------------- constants & module state ---------------- */

  var PER_PAGE = 50;
  var MOCK = false;
  try { MOCK = new URLSearchParams(location.search).get('mock') === '1'; } catch (e) { MOCK = false; }

  var KEY = '';      // API Key：仅 mock 模式在内存持有（mockKEY）；正式模式恒为空
  var ME = null;     // GET /v1/me 载荷
  var WS = null;     // 当前工作区入口 {id,url,expires_at}（正式模式；mock 恒为 null）
  var reloginWsId = ''; // 等待重新登录的入口 id（正式模式；登录时随 body 提交）
  var authGen = 0;   // 会话代：登录提交 / 登出时自增
  var navGen = 0;    // 浏览代：每次目录加载 / 离开浏览时自增
  var loginSeq = 0;  // 使最后一次登录/入口选择尝试胜出
  var taskSeq = 0;
  var dialogSeq = 0;

  var VIEW = null; // { driveId, drive, path, page, total, perPage, items, filter, loading, loadError }
  function freshView() {
    return { driveId: null, drive: null, path: '/', page: 1, total: 0, perPage: PER_PAGE, items: [], filter: '', loading: false, loadError: null };
  }
  VIEW = freshView();

  var aborts = new Set();   // fetch AbortController
  var queue = [];           // 上传任务
  var activeTask = null;
  var pendingDeletePath = null;
  var pendingRenamePath = null;
  var shareCtx = null;      // { drive, path }
  var noticeTimer = null;

  /* ---------------- element cache ---------------- */

  var el = {};
  var IDS = ['mock-banner', 'gate', 'gate-hint', 'login-form', 'key', 'key-toggle', 'gate-err', 'gate-go',
    'ws-list', 'ws-sub', 'ws-grid', 'ws-add-key',
    'workspace', 'me-prefix', 'theme-toggle', 'switch-ws', 'logout', 'notice',
    'drives-home', 'drives-sub', 'drive-grid',
    'browser-view', 'breadcrumb', 'back-drives', 'manager-toggle', 'file-manager', 'page-search', 'refresh-btn', 'mkdir-btn',
    'dropzone', 'dz-title', 'dz-text', 'upload-hint', 'choose-files', 'conflict-mode', 'overwrite-row', 'overwrite-ok', 'file-input',
    'file-list', 'file-tbody', 'page-prev', 'page-next', 'page-info',
    'upload-queue', 'upload-status', 'upload-panel',
    'dlg-mkdir', 'mkdir-form', 'mkdir-where', 'mkdir-name', 'mkdir-err',
    'dlg-rename', 'rename-form', 'rename-old', 'rename-name', 'rename-err',
    'dlg-delete', 'delete-form', 'delete-path', 'delete-note', 'delete-err',
    'dlg-share', 'share-form', 'share-what', 'share-ttl', 'share-err', 'share-result',
    'share-url', 'share-copy', 'share-exp', 'share-ok'];
  function cacheEls() {
    IDS.forEach(function (id) { el[id] = document.getElementById(id); });
  }

  /* ---------------- tiny DOM helpers (textContent only) ---------------- */

  function h(tag, attrs, text) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        if (k === 'class') node.className = attrs[k];
        else if (k === 'text') node.textContent = attrs[k];
        else node.setAttribute(k, attrs[k]);
      });
    }
    if (text != null) node.textContent = text;
    return node;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  /* ---------------- formatting ---------------- */

  function fmtBytes(n) {
    n = Number(n);
    if (!isFinite(n) || n < 0) return '—';
    if (n === 0) return '0 B';
    var units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
    var i = 0;
    var v = n;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return (i === 0 ? String(v) : v.toFixed(1)) + ' ' + units[i];
  }

  function fmtTime(s) {
    if (!s) return '—';
    var d = new Date(s);
    if (isNaN(d.getTime())) return String(s);
    function p2(x) { return x < 10 ? '0' + x : '' + x; }
    return d.getFullYear() + '-' + p2(d.getMonth() + 1) + '-' + p2(d.getDate()) + ' ' + p2(d.getHours()) + ':' + p2(d.getMinutes());
  }

  function uuid() {
    try { if (typeof crypto.randomUUID === 'function') return crypto.randomUUID(); } catch (e) { /* fall through */ }
    var buf = new Uint8Array(16);
    crypto.getRandomValues(buf);
    var hex = '';
    for (var i = 0; i < buf.length; i++) hex += (buf[i] < 16 ? '0' : '') + buf[i].toString(16);
    return hex.slice(0, 8) + '-' + hex.slice(8, 12) + '-' + hex.slice(12, 16) + '-' + hex.slice(16, 20) + '-' + hex.slice(20);
  }

  /* ---------------- paths ---------------- */

  function pathSegs(p) { return String(p || '/').split('/').filter(Boolean); }
  function joinPath(dir, name) { return (dir === '/' ? '' : String(dir).replace(/\/+$/, '')) + '/' + name; }
  function dirname(p) {
    var segs = pathSegs(p);
    return segs.length <= 1 ? '/' : '/' + segs.slice(0, -1).join('/');
  }

  /* ---------------- providers / health / perms ---------------- */

  var PROVIDER_LABELS = {
    baidu: '百度网盘', aliyun: '阿里云盘', 'aliyundrive': '阿里云盘', quark: '夸克网盘',
    '123pan': '123云盘', '115': '115网盘', '115open': '115网盘', onedrive: 'OneDrive',
    googledrive: 'Google Drive', dropbox: 'Dropbox', box: 'Box', pikpak: 'PikPak',
    webdav: 'WebDAV', s3: 'S3 对象存储', local: '本地存储', goapi: 'GoAPI 上游'
  };
  var PROVIDER_TONES = {
    baidu: 'tone-blue', aliyun: 'tone-violet', quark: 'tone-amber', '123pan': 'tone-teal',
    '115open': 'tone-blue', onedrive: 'tone-blue', googledrive: 'tone-teal',
    webdav: 'tone-teal', s3: 'tone-slate', local: 'tone-slate', goapi: 'tone-violet'
  };
  function providerLabel(p) {
    var k = String(p || '').toLowerCase();
    return PROVIDER_LABELS[k] || (p ? String(p) : '未知类型');
  }
  function providerTone(p) {
    var k = String(p || '').toLowerCase();
    return PROVIDER_TONES[k] || 'tone-slate';
  }

  var HEALTH_LABELS = {
    healthy: ['badge-ok', '正常'],
    needs_reauth: ['badge-warn', '需要重新授权'],
    error: ['badge-danger', '上游错误'],
    disabled: ['badge-slate', '已停用'],
    orphaned: ['badge-danger', '存储已删除'],
    replacement_found: ['badge-warn', '存储被替换，待管理员确认'],
    unverified: ['badge-slate', '状态未知']
  };
  function healthBadge(health) {
    var map = HEALTH_LABELS[health] || ['badge-slate', health || '状态未知'];
    var b = h('span', { class: 'badge ' + map[0] });
    b.appendChild(h('span', { class: 'dot' }));
    b.appendChild(document.createTextNode(map[1]));
    b.title = '网盘健康状态：' + map[1] + '（/v1/me drives[].health = ' + (health || '?') + '）';
    return b;
  }
  function healthNote(health) {
    switch (health) {
      case 'needs_reauth': return '该网盘的上游登录凭据已失效，需要管理员在网盘侧更新授权；期间操作会失败。';
      case 'error': return '该网盘上游报错，操作可能失败；其他网盘不受影响。';
      case 'disabled': return '该网盘已被停用；操作会被拒绝。';
      case 'orphaned': return '该网盘在网盘侧的存储已被删除；需要管理员重新纳管。';
      case 'replacement_found': return '该网盘的存储被替换且尚未经管理员确认；访问被暂时阻止。';
      default: return '该网盘状态异常（' + health + '）；操作可能失败。';
    }
  }

  function driveById(id) {
    if (!ME || !Array.isArray(ME.drives)) return null;
    for (var i = 0; i < ME.drives.length; i++) if (ME.drives[i].id === id) return ME.drives[i];
    return null;
  }

  /* 有效权限 = global_perms ∪ drive.perms（仅对 me.drives 中存在的网盘） */
  function permsFor(d) {
    var set = new Set();
    if (ME && Array.isArray(ME.global_perms)) ME.global_perms.forEach(function (p) { set.add(p); });
    if (d && Array.isArray(d.perms)) d.perms.forEach(function (p) { set.add(p); });
    return set;
  }
  function limitSize() {
    return (ME && ME.limits && typeof ME.limits.max_upload_bytes === 'number') ? ME.limits.max_upload_bytes : null;
  }

  /* ---------------- notice ---------------- */

  function showNotice(text, cls, sticky) {
    if (!el.notice) return;
    clear(el.notice);
    el.notice.appendChild(document.createTextNode(text));
    el.notice.className = 'fx-notice' + (cls ? ' fx-' + cls : '');
    el.notice.hidden = false;
    if (noticeTimer) { clearTimeout(noticeTimer); noticeTimer = null; }
    if (!sticky) noticeTimer = setTimeout(hideNotice, 6000);
  }
  function hideNotice() {
    if (noticeTimer) { clearTimeout(noticeTimer); noticeTimer = null; }
    if (el.notice) { el.notice.hidden = true; clear(el.notice); }
  }
  function noticeOk(t) { showNotice(t, 'ok', false); }
  function noticeInfo(t) { showNotice(t, 'info', false); }
  function noticeErr(t) { showNotice(t, 'err', true); }

  /* ---------------- theme ---------------- */

  function initTheme() {
    var dark = false;
    try { dark = !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches); } catch (e) { dark = false; }
    document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
  }
  function toggleTheme() {
    var cur = document.documentElement.getAttribute('data-theme') === 'dark';
    document.documentElement.setAttribute('data-theme', cur ? 'light' : 'dark');
  }

  /* ---------------- dialogs ---------------- */

  function openDialog(dlg) {
    if (!dlg) return;
    dialogSeq++;
    try { dlg.showModal(); } catch (e) { /* already open */ }
  }
  function closeDialogs() {
    dialogSeq++;
    var open = document.querySelectorAll('dialog[open]');
    for (var i = 0; i < open.length; i++) { try { open[i].close(); } catch (e) { /* noop */ } }
  }
  function bindDialog(dlg) {
    if (!dlg) return;
    function invalidate() {
      dialogSeq++;
      if (dlg === el['dlg-share']) {
        shareCtx = null;
        el['share-url'].value = '';
        el['share-result'].hidden = true;
      }
    }
    dlg.addEventListener('cancel', invalidate);
    var closers = dlg.querySelectorAll('[data-close]');
    for (var i = 0; i < closers.length; i++) {
      closers[i].addEventListener('click', function () { invalidate(); dlg.close(); });
    }
  }

  /* ---------------- error helpers ---------------- */

  function errText(res, fallback) {
    var e = res && res.json && res.json.error;
    if (e && typeof e.message === 'string' && e.message) return e.message;
    if (res && res.json && typeof res.json.message === 'string' && res.json.message) return res.json.message;
    return fallback || '未知错误';
  }
  function retryAfterText(headers) {
    if (!headers) return '';
    var ra = Number(headers.get('Retry-After'));
    return (isFinite(ra) && ra > 0) ? '（约 ' + Math.ceil(ra) + ' 秒后可重试）' : '';
  }

  /* ---------------- transport ----------------
   * api() 返回 {status, ok, json, headers, network}，从不抛出。
   * mock 模式走 mockRequest（零真实网络）；正式模式 fetch + X-C2A-Workspace 头
   * + credentials:'same-origin'（Cookie 会话），无 apikey 长期 Key 头；
   * /v1/* 数据路径映射到当前入口的 /files/w/<id>/api/v1/* 同源适配。 */

  function apiPath(p) {
    if (MOCK) return p;
    if (WS && WS.id && p.lastIndexOf('/v1/', 0) === 0) {
      return '/files/w/' + encodeURIComponent(WS.id) + '/api' + p;
    }
    return p;
  }

  function api(method, path, opts) {
    opts = opts || {};
    if (MOCK) {
      // 与真实分支等价：json 载荷序列化前即对象，直接交给 mock 处理器
      return mockApiResult(mockRequest(method, path, {
        query: opts.query,
        apiKey: KEY,
        task: opts.task,
        onProgress: opts.onProgress,
        body: (opts.json !== undefined) ? opts.json : opts.body
      }));
    }
    var ctrl = new AbortController();
    aborts.add(ctrl);
    var gen = authGen;
    var qs = opts.query ? '?' + opts.query.toString() : '';
    var init = {
      method: method,
      credentials: 'same-origin',
      headers: { 'X-C2A-Workspace': '1' },
      signal: ctrl.signal
    };
    if (opts.keepalive) init.keepalive = true;
    if (opts.json !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.json);
    }
    return fetch(apiPath(path) + qs, init).then(function (res) {
      return res.json().catch(function () { return null; }).then(function (doc) {
        aborts.delete(ctrl);
        return { status: res.status, ok: res.ok, json: doc, headers: res.headers, gen: gen };
      });
    }, function () {
      aborts.delete(ctrl);
      return { status: 0, ok: false, json: null, headers: null, network: true, gen: gen };
    });
  }

  /* =====================================================
   * MOCK backend（仅 ?mock=1；零真实网络请求）
   * ===================================================== */

  var MOCKMAP = {};
  var MOCKCALLS = [];
  var MOCKDB = null;
  var MOCK_LAST_ME = null;

  function merr(status, code, message) {
    return { mock: true, mockStatus: status, mockBody: { error: { code: code, message: message, status: status } } };
  }
  function mclone(v) { return JSON.parse(JSON.stringify(v)); }
  function mrng(len) {
    var chars = 'abcdef0123456789';
    var buf = new Uint8Array(len);
    crypto.getRandomValues(buf);
    var s = '';
    for (var i = 0; i < len; i++) s += chars[buf[i] % 16];
    return s;
  }
  function mStamp(daysAgo) {
    return new Date(Date.now() - (daysAgo == null ? 3 : daysAgo) * 86400e3)
      .toISOString().slice(0, 19).replace('T', ' ');
  }
  function mNow() { return new Date().toISOString().slice(0, 19).replace('T', ' '); }

  function mdir(name, children, daysAgo) {
    return { name: name, is_dir: true, size: 0, modified: mStamp(daysAgo), children: children || [] };
  }
  function mfile(name, size, daysAgo) {
    return { name: name, is_dir: false, size: size, modified: mStamp(daysAgo) };
  }

  function mockSeed() {
    var big = [];
    for (var i = 1; i <= 120; i++) {
      var nn = i < 10 ? '00' + i : (i < 100 ? '0' + i : '' + i);
      big.push(mfile('file-' + nn + '.txt', 1024 * i, 30));
    }
    MOCKDB = {
        me: {
        key: { id: 'demo-key', prefix: 'sk-c2a-demodemo', default_drive: 'baidu-demo' },
        drives: [
          { id: 'baidu-demo', provider: 'baidu', root: '/', perms: ['list', 'read', 'write', 'delete', 'link'], health: 'healthy', default: true },
          { id: 'readonly-archive', provider: 'webdav', root: '/archive', perms: ['list', 'read'], health: 'healthy' },
          { id: 'aliyun-broken', provider: 'aliyun', root: '/', perms: ['list', 'read', 'write'], health: 'needs_reauth' }
        ],
        global_perms: [],
        limits: { max_upload_bytes: 1073741824, requests_per_minute: 60, concurrent_jobs: 1 },
        trash: { enabled: true, days: 7 }
      },
      trees: {
        'baidu-demo': [
          mdir('reports', [
            mfile('q3-summary.pdf', 184211, 3),
            mfile('q4-plan.docx', 92134, 1),
            mdir('data', [mfile('metrics.csv', 5511, 0)], 2)
          ], 1),
          mdir('photos', [mfile('beach.jpg', 2411000, 40), mfile('family.png', 1802000, 41)], 40),
          mdir('big', big, 30),
          mfile('readme.txt', 1234, 6),
          mfile('notes.md', 2345, 2)
        ],
        'readonly-archive': [
          mdir('2024', [mfile('logs.tar.gz', 8812345, 200)], 200),
          mdir('2025', [mfile('index.json', 987, 80)], 80),
          mfile('README.md', 432, 120)
        ],
        'aliyun-broken': [mfile('would-be-listed-but-drive-unhealthy.txt', 10, 1)]
      },
      links: []
    };
    MOCK_LAST_ME = null;
  }

  function mTree(driveId) { return (MOCKDB.trees && MOCKDB.trees[driveId]) || null; }
  function mRootNode(driveId) { return { name: '', is_dir: true, size: 0, children: mTree(driveId) || [] }; }
  function mResolve(driveId, p) {
    var segs = pathSegs(p);
    if (!segs.length) return mRootNode(driveId);
    var list = mTree(driveId) || [];
    var node = null;
    for (var i = 0; i < segs.length; i++) {
      var found = null;
      if (!list) return null;
      for (var j = 0; j < list.length; j++) if (list[j].name === segs[i]) { found = list[j]; break; }
      if (!found) return null;
      node = found;
      if (i < segs.length - 1) {
        if (!node.is_dir || !Array.isArray(node.children)) return null;
        list = node.children;
      }
    }
    return node;
  }
  function mChildList(driveId, p) {
    var node = mResolve(driveId, p);
    if (!node || !node.is_dir || !Array.isArray(node.children)) return null;
    return node.children;
  }
  function mParentList(driveId, p) {
    var segs = pathSegs(p);
    if (!segs.length) return null;
    return mChildList(driveId, dirname(p));
  }
  function mChildPath(p, name) { return (p === '/' ? '' : String(p).replace(/\/+$/, '')) + '/' + name; }

  function mGrant(driveId) {
    var me = MOCK_LAST_ME || MOCKDB.me;
    return ((me.drives || []).filter(function (d) { return d.id === driveId; })[0]) || null;
  }
  function mPermsFor(driveId) {
    var me = MOCK_LAST_ME || MOCKDB.me;
    var set = {};
    (me.global_perms || []).forEach(function (p) { set[p] = true; });
    var g = mGrant(driveId);
    if (g) (g.perms || []).forEach(function (p) { set[p] = true; });
    return set;
  }
  function mNeedPerm(ctx, driveId, perm) {
    var g = mGrant(driveId);
    if (!g) throw merr(404, 'drive_not_found', 'drive "' + driveId + '" is not authorized for this key (mock)');
    if (!mPermsFor(driveId)[perm]) throw merr(403, 'forbidden', 'missing "' + perm + '" permission on drive "' + driveId + '" (mock)');
    if (g.health && g.health !== 'healthy') {
      throw merr(503, 'drive_unavailable', 'drive "' + driveId + '" health is "' + g.health + '"; the upstream refuses this operation (mock)');
    }
  }
  function mFreeName(list, name) {
    var dot = name.lastIndexOf('.');
    var stem = dot > 0 ? name.slice(0, dot) : name;
    var ext = dot > 0 ? name.slice(dot) : '';
    for (var n = 1; n <= 999; n++) {
      var cand = stem + ' (' + n + ')' + ext;
      var taken = list.some(function (x) { return x.name === cand; });
      if (!taken) return cand;
    }
    return name + '.' + Date.now();
  }

  var DEMO = {};

  DEMO.me = function () { return mclone(MOCKDB.me); };

  DEMO.list = function (ctx) {
    var drive = ctx.path.split('/')[3];
    mNeedPerm(ctx, drive, 'list');
    var p = ctx.query.get('path') || '/';
    var page = Math.max(1, parseInt(ctx.query.get('page') || '1', 10) || 1);
    var per = Math.max(1, parseInt(ctx.query.get('per_page') || '200', 10) || 200);
    var node = mResolve(drive, p);
    if (!node) throw merr(404, 'path_not_found', 'path not found: ' + p + ' (mock)');
    if (!node.is_dir || !Array.isArray(node.children)) throw merr(400, 'not_a_directory', 'not a directory: ' + p + ' (mock)');
    var children = node.children;
    var start = (page - 1) * per;
    var items = children.slice(start, start + per).map(function (n) {
      return { name: n.name, path: mChildPath(p, n.name), is_dir: n.is_dir, size: n.size, modified: n.modified };
    });
    return { items: items, total: children.length, page: page, per_page: per };
  };

  DEMO.mkdir = function (ctx) {
    var drive = ctx.path.split('/')[3];
    mNeedPerm(ctx, drive, 'write');
    var p = String((ctx.body && ctx.body.path) || '');
    var segs = pathSegs(p);
    if (!segs.length) throw merr(400, 'invalid_request', 'field `path` is required (mock)');
    var parent = mParentList(drive, p);
    if (!parent) throw merr(404, 'path_not_found', 'parent directory not found: ' + dirname(p) + ' (mock)');
    var name = segs[segs.length - 1];
    for (var i = 0; i < parent.length; i++) {
      if (parent[i].name === name) return { path: p, created: false };
    }
    parent.push({ name: name, is_dir: true, size: 0, modified: mNow(), children: [] });
    return { path: p, created: true };
  };

  DEMO.move = function (ctx) {
    var drive = ctx.path.split('/')[3];
    mNeedPerm(ctx, drive, 'write');
    var from = String((ctx.body && ctx.body.from) || '');
    var to = String((ctx.body && ctx.body.to) || '');
    var oc = String((ctx.body && ctx.body.on_conflict) || 'fail');
    var node = mResolve(drive, from);
    if (!node) throw merr(404, 'path_not_found', 'path not found: ' + from + ' (mock)');
    var fromList = mParentList(drive, from);
    if (!fromList) throw merr(400, 'invalid_request', 'cannot move root (mock)');
    var toSegs = pathSegs(to);
    if (!toSegs.length) throw merr(400, 'invalid_request', 'field `to` is required (mock)');
    var toList = mParentList(drive, to);
    if (!toList) throw merr(404, 'path_not_found', 'destination directory not found: ' + dirname(to) + ' (mock)');
    var finalName = toSegs[toSegs.length - 1];
    var clash = null;
    for (var i = 0; i < toList.length; i++) if (toList[i].name === finalName) { clash = toList[i]; break; }
    if (clash) {
      if (oc === 'fail') throw merr(409, 'already_exists', 'target already exists: ' + to + ' (mock)');
      if (oc === 'rename') {
        finalName = mFreeName(toList, finalName);
      } else { // overwrite
        var k = toList.indexOf(clash);
        if (k >= 0) toList.splice(k, 1);
      }
    }
    var j = fromList.indexOf(node);
    if (j >= 0) fromList.splice(j, 1);
    node.name = finalName;
    toList.push(node);
    return { from: from, to: mChildPath(dirname(to), finalName) };
  };

  DEMO['delete'] = function (ctx) {
    var drive = ctx.path.split('/')[3];
    mNeedPerm(ctx, drive, 'delete');
    var b = ctx.body || {};
    var paths = Array.isArray(b.paths) ? b.paths : (b.path ? [b.path] : []);
    if (!paths.length) throw merr(400, 'invalid_request', 'field `paths` (array) is required (mock)');
    var me = MOCK_LAST_ME || MOCKDB.me;
    var useTrash = !!(me.trash && me.trash.enabled);
    var results = paths.map(function (p) {
      var list = mParentList(drive, p);
      var node = mResolve(drive, p);
      if (!node) return { path: p, status: 'already_absent' };
      if (!list) return { path: p, status: 'error', error: 'cannot remove this path (mock)' };
      var i = list.indexOf(node);
      if (i >= 0) list.splice(i, 1);
      return { path: p, status: useTrash ? 'trashed' : 'deleted' };
    });
    return { results: results };
  };

  DEMO.links = function (ctx) {
    var me = MOCK_LAST_ME || MOCKDB.me;
    var b = ctx.body || {};
    var drive = String(b.drive || (me.key && me.key.default_drive) || '');
    if (!drive) throw merr(400, 'invalid_request', 'field `drive` is required (this key has no default drive) (mock)');
    if (!mGrant(drive)) throw merr(404, 'drive_not_found', 'drive "' + drive + '" is not authorized (mock)');
    if (!mPermsFor(drive).link) throw merr(403, 'forbidden', 'missing "link" permission on drive "' + drive + '" (mock)');
    var p = String(b.path || '');
    var ttl = Number(b.expires_in) || 3600;
    if (ttl <= 0 || ttl > 604800) throw merr(400, 'invalid_request', 'expires_in must be between 1 and 604800 seconds (mock)');
    var node = mResolve(drive, p);
    if (!node) throw merr(404, 'path_not_found', 'path not found: ' + p + ' (mock)');
    var token = 'demo-' + mrng(18);
    var expiresAt = new Date(Date.now() + ttl * 1000).toISOString();
    MOCKDB.links.push({ token: token, drive: drive, path: p, expires_at: expiresAt });
    return { url: location.origin + '/l/' + token, expires_at: expiresAt };
  };

  DEMO.filesGet = function (ctx) {
    var drive = ctx.path.split('/')[3];
    mNeedPerm(ctx, drive, 'read');
    var p = ctx.query.get('path') || '/';
    var node = mResolve(drive, p);
    if (!node) throw merr(404, 'path_not_found', 'path not found: ' + p + ' (mock)');
    return { note: 'mock download: no real bytes are transferred', path: p, size: node.size };
  };

  DEMO.downloadTicket = function (ctx) {
    var drive = ctx.path.split('/')[3];
    mNeedPerm(ctx, drive, 'read');
    var p = String((ctx.body && ctx.body.path) || '');
    var node = mResolve(drive, p);
    if (!node) throw merr(404, 'path_not_found', 'path not found: ' + p + ' (mock)');
    if (node.is_dir) throw merr(400, 'not_a_file', 'cannot download a directory (mock)');
    return { url: '/download/' + mrng(43), expires_at: new Date(Date.now() + 120000).toISOString() };
  };

  DEMO.upload = function (ctx) {
    var drive = ctx.path.split('/')[3];
    mNeedPerm(ctx, drive, 'write');
    var p = String(ctx.query.get('path') || '');
    var oc = String(ctx.query.get('on_conflict') || 'fail');
    var segs = pathSegs(p);
    if (!segs.length) throw merr(400, 'invalid_request', 'query parameter `path` is required (mock)');
    var parent = mParentList(drive, p);
    if (!parent) throw merr(404, 'path_not_found', 'parent directory not found: ' + dirname(p) + ' (mock)');
    var f = ctx.body || {};
    var size = Number(f.size) || 0;
    var name = segs[segs.length - 1];
    return new Promise(function (resolve, reject) {
      var steps = 6;
      var i = 0;
      var total = Math.max(size, 1);
      (function tick() {
        if (ctx.task && ctx.task.cancelled) return reject(merr(499, 'aborted', 'upload cancelled (mock)'));
        i++;
        if (ctx.onProgress) {
          try { ctx.onProgress(Math.min(Math.round(total * i / steps), size), size); } catch (e) { /* UI only */ }
        }
        if (i < steps) { setTimeout(tick, 90); return; }
        var existing = null;
        for (var k = 0; k < parent.length; k++) if (parent[k].name === name) { existing = parent[k]; break; }
        var finalPath = p;
        var conflict = 'created';
        if (existing) {
          if (oc === 'fail') return reject(merr(409, 'already_exists', 'target already exists: ' + p + ' (on_conflict=fail) (mock)'));
          if (oc === 'overwrite') {
            existing.is_dir = false;
            existing.size = size;
            existing.modified = mNow();
            delete existing.children;
            conflict = 'overwritten';
          } else {
            var cand = mFreeName(parent, name);
            conflict = 'renamed';
            finalPath = mChildPath(dirname(p), cand);
            name = cand;
            existing = null;
          }
        }
        if (!existing) parent.push(mfile(name, size, 0));
        resolve({
          path: finalPath,
          size: size,
          crc64: String(Math.floor(1e17 + Math.random() * 8e17)),
          sha256: mrng(64),
          conflict: conflict
        });
      })();
    });
  };

  function mockRegister() {
    MOCKMAP['GET /v1/me'] = DEMO.me;
    MOCKMAP['GET /v1/drives/{drive}/list'] = DEMO.list;
    MOCKMAP['GET /v1/drives/{drive}/files'] = DEMO.filesGet;
    MOCKMAP['POST /v1/drives/{drive}/download-ticket'] = DEMO.downloadTicket;
    MOCKMAP['POST /v1/drives/{drive}/mkdir'] = DEMO.mkdir;
    MOCKMAP['POST /v1/drives/{drive}/move'] = DEMO.move;
    MOCKMAP['POST /v1/drives/{drive}/delete'] = DEMO['delete'];
    MOCKMAP['PUT /v1/drives/{drive}/files'] = DEMO.upload;
    MOCKMAP['POST /v1/links'] = DEMO.links;
  }

  function mockFindHandler(method, path) {
    var exact = MOCKMAP[method + ' ' + path];
    if (typeof exact === 'function') return exact;
    var m = /^\/v1\/drives\/[^/]+\/(list|files|mkdir|move|delete|download-ticket)$/.exec(path);
    if (m) {
      var tpl = MOCKMAP[method + ' /v1/drives/{drive}/' + m[1]];
      if (typeof tpl === 'function') return tpl;
    }
    return null;
  }

  function mockSummarize(b) {
    if (b == null) return null;
    if (typeof File === 'function' && b instanceof File) return { name: b.name, size: b.size };
    if (typeof b === 'object') { try { return mclone(b); } catch (e) { return '[unserializable]'; } }
    return String(b);
  }

  function mockRequest(method, path, opts) {
    opts = opts || {};
    return new Promise(function (resolve, reject) {
      setTimeout(function () {
        MOCKCALLS.push({
          method: method, path: path,
          query: opts.query ? String(opts.query) : '',
          has_key: !!opts.apiKey,
          at: new Date().toISOString(),
          body: mockSummarize(opts.body)
        });
        var fn = mockFindHandler(method, path);
        if (!fn) return reject(merr(404, 'not_found', 'mock: no handler for ' + method + ' ' + path));
        var ctx = {
          method: method, path: path,
          query: opts.query || new URLSearchParams(),
          body: opts.body || null,
          key: opts.apiKey || '',
          onProgress: opts.onProgress || null,
          task: opts.task || null
        };
        Promise.resolve().then(function () { return fn(ctx); }).then(function (doc) {
          if (method === 'GET' && path === '/v1/me' && doc) MOCK_LAST_ME = doc;
          resolve(doc);
        }, reject);
      }, 60 + Math.round(Math.random() * 120));
    });
  }

  function mockApiResult(promise) {
    return promise.then(
      function (doc) { return { status: 200, ok: true, json: doc, headers: null, gen: authGen }; },
      function (e) {
        if (e && e.mock) return { status: e.mockStatus, ok: false, json: e.mockBody, headers: null, gen: authGen };
        return { status: 0, ok: false, json: null, headers: null, network: true, gen: authGen };
      }
    );
  }

  function exposeMock() {
    // 仅 mock 模式可见；绝不暴露 Key 本体（calls 只记录 has_key，不记录密钥值）。
    window.__FILES_MOCK = {
      map: MOCKMAP,
      calls: MOCKCALLS,
      reset: function () { mockSeed(); MOCKCALLS.length = 0; },
      keys: [
        'GET /v1/me',
        'GET /v1/drives/{drive}/list',
        'GET /v1/drives/{drive}/files',
        'POST /v1/drives/{drive}/download-ticket',
        'POST /v1/drives/{drive}/mkdir',
        'POST /v1/drives/{drive}/move',
        'POST /v1/drives/{drive}/delete',
        'PUT /v1/drives/{drive}/files',
        'POST /v1/links'
      ]
    };
  }

  /* ---------------- gate / login / logout ---------------- */

  function gateErr(t) { clear(el['gate-err']); if (t) el['gate-err'].appendChild(document.createTextNode(t)); }

  var GATE_HINT_DEFAULT = '输入访问密钥，查看已授权的网盘，上传和分享文件。';
  function resetGateHint() {
    reloginWsId = '';
    if (el['gate-hint']) el['gate-hint'].textContent = GATE_HINT_DEFAULT;
  }
  function applyReloginContext(id) {
    reloginWsId = id;
    if (el['gate-hint']) {
      el['gate-hint'].textContent = '工作区 ' + shortWsId(id) + ' 的登录已失效：请输入创建该工作区时使用的 API Key 重新登录（其他 Key 无法接管此入口）。';
    }
  }
  function shortWsId(id) {
    id = String(id || '');
    return id.length > 10 ? id.slice(0, 8) + '…' : id;
  }

  /* 服务器返回的入口只允许与入口格式一致的相对路径（与 id 一致），
   * 绝不信任绝对 URL / 跨源 / 查询串 / 其他路径形态。
   * 登录响应字段是 workspace_id，/workspaces 列表字段是 id，两者都接受。 */
  var WS_ID_RE = /^[A-Za-z0-9_-]{8,128}$/;
  function sanitizeEntry(raw) {
    if (!raw || typeof raw !== 'object') return null;
    var id = (raw.id !== undefined && raw.id !== null) ? raw.id : raw.workspace_id;
    var url = raw.url;
    if (typeof id !== 'string' || !WS_ID_RE.test(id)) return null;
    if (typeof url !== 'string' || !url) return null;
    var u;
    try { u = new URL(url, location.origin); } catch (e) { return null; }
    if (u.origin !== location.origin) return null;
    if (!/^\/files\/w\/[A-Za-z0-9_-]{8,128}\/$/.test(u.pathname)) return null;
    if (u.search || u.hash || u.username || u.password) return null;
    if (u.pathname !== '/files/w/' + id + '/') return null; // url 与 id 必须一致
    if (raw.expires_at !== undefined && raw.expires_at !== null && typeof raw.expires_at !== 'string') return null;
    return {
      id: id,
      url: '/files/w/' + id + '/',
      expires_at: typeof raw.expires_at === 'string' ? raw.expires_at : ''
    };
  }
  function sanitizeEntries(list) {
    var out = [];
    var seen = {};
    (Array.isArray(list) ? list : []).forEach(function (raw) {
      var en = sanitizeEntry(raw);
      if (en && !seen[en.id]) { seen[en.id] = true; out.push(en); }
    });
    return out;
  }

  function doLogin(ev) {
    ev.preventDefault();
    var v = el.key.value.trim();
    if (!v) { gateErr('请输入访问密钥（API Key，sk-c2a-…）'); return; }
    el['gate-go'].disabled = true;
    gateErr('');
    if (MOCK) {
      KEY = v;
      var myLogin = ++loginSeq;
      var p = api('GET', '/v1/me');
      p.then(function (res) {
        if (myLogin !== loginSeq) return; // 已被新的登录/登出取代
        el['gate-go'].disabled = false;
        if (res.status === 200 && res.json && Array.isArray(res.json.drives) && res.json.key) {
          authGen++;
          el.key.value = '';
          el.key.type = 'password';
          el['key-toggle'].textContent = '显示';
          ME = res.json;
          enterWorkspace();
          return;
        }
        KEY = '';
        if (res.network) gateErr('网络错误：无法连接服务器，请稍后重试');
        else if (res.status === 401 || res.status === 403) gateErr('密钥无效或已被吊销（HTTP ' + res.status + '），请检查后重试');
        else if (res.status === 429) gateErr('尝试过于频繁（HTTP 429）' + retryAfterText(res.headers) + '，请稍后再试');
        else gateErr('读取授权失败（HTTP ' + (res.status || '?') + '）：' + errText(res, '请稍后重试'));
      });
      return;
    }
    /* 正式模式：Key 用完立即清输入；本页从此不持有原始 Key，后续凭 Cookie。 */
    el.key.value = '';
    el.key.type = 'password';
    el['key-toggle'].textContent = '显示';
    var mySeq = ++loginSeq;
    var targetWs = reloginWsId; // 指定入口的重登录必须携带 workspace_id，不能被其他 Key 覆盖
    var body = { api_key: v };
    v = null; // 局部变量即时清除
    if (targetWs) body.workspace_id = targetWs;
    api('POST', '/files/api/login', { json: body }).then(function (res) {
      if (mySeq !== loginSeq) return; // 已被新的登录/入口选择取代
      el['gate-go'].disabled = false;
      if (res.status === 200 && res.json) {
        var entry = sanitizeEntry(res.json);
        if (!entry) { gateErr('服务器返回的工作区入口不合法，已拒绝进入'); return; }
        if (targetWs && entry.id !== targetWs) { gateErr('登录返回的入口与目标工作区不一致，已拒绝进入'); return; }
        authGen++;
        resetGateHint();
        WS = entry;
        try { history.replaceState(null, '', entry.url); } catch (e) { /* noop */ }
        showWsBooting('正在打开工作区 ' + shortWsId(entry.id) + '…');
        loadMeIntoWorkspace();
        return;
      }
      if (res.network) gateErr('网络错误：无法连接服务器，请稍后重试');
      else if (res.status === 401 || res.status === 403) gateErr('密钥无效或已被吊销（HTTP ' + res.status + '），请检查后重试');
      else if (res.status === 429) gateErr('尝试过于频繁（HTTP 429）' + retryAfterText(res.headers) + '，请稍后再试');
      else gateErr('登录失败（HTTP ' + (res.status || '?') + '）：' + errText(res, '请稍后重试'));
    });
  }

  /* ---------------- 工作区恢复 / 入口选择（正式模式） ---------------- */

  function showGateOnly() {
    el.workspace.hidden = true;
    el['ws-list'].hidden = true;
    el.gate.hidden = false;
  }
  function showWsBooting(text) {
    el.gate.hidden = true;
    el.workspace.hidden = true;
    el['ws-list'].hidden = false;
    clear(el['ws-grid']);
    el['ws-sub'].textContent = text;
  }
  /* 打开某个入口期间保留列表卡片：响应悬空时仍可改选其他入口。 */
  function showWsOpening(text) {
    el.gate.hidden = true;
    el.workspace.hidden = true;
    el['ws-list'].hidden = false;
    el['ws-sub'].textContent = text;
  }

  function renderWsList(entries) {
    el.gate.hidden = true;
    el.workspace.hidden = true;
    el['ws-list'].hidden = false;
    el['ws-sub'].textContent = '此浏览器保存了 ' + entries.length + ' 个有效的文件工作区：选择一个入口继续，或添加其他 API Key。';
    clear(el['ws-grid']);
    entries.forEach(function (en) {
      var card = h('div', { 'class': 'fx-ws-card', 'data-ws-id': en.id });
      card.appendChild(h('div', { 'class': 'fx-ws-name', text: '工作区 ' + shortWsId(en.id) }));
      card.appendChild(h('div', { 'class': 'fx-ws-meta mono', text: '入口 ' + en.id }));
      if (en.expires_at) card.appendChild(h('div', { 'class': 'fx-ws-meta', text: '有效期至 ' + fmtTime(en.expires_at) }));
      var go = h('button', { 'class': 'btn btn-primary', type: 'button', text: '进入' });
      go.setAttribute('aria-label', '进入工作区 ' + en.id);
      go.addEventListener('click', function () { chooseEntry(en); });
      var foot = h('div', { 'class': 'fx-ws-foot' });
      foot.appendChild(go);
      card.appendChild(foot);
      el['ws-grid'].appendChild(card);
    });
  }

  function chooseEntry(en) {
    authGen++;
    navGen++;
    loginSeq++;
    closeDialogs();
    ME = null;
    VIEW = freshView();
    WS = en;
    try { history.pushState(null, '', en.url); } catch (e) { /* noop */ }
    showWsOpening('正在打开工作区 ' + shortWsId(en.id) + '…');
    loadMeIntoWorkspace();
  }

  /* 从 /files/w/<id>/ 深链接恢复：入口 id 来自本页 URL（格式已校验）。 */
  function deepRestore(id) {
    loginSeq++;
    WS = { id: id, url: '/files/w/' + id + '/', expires_at: '' };
    showWsBooting('正在打开工作区 ' + shortWsId(id) + '…');
    loadMeIntoWorkspace();
  }

  /* GET /files/api/workspaces 后按场景分流：
   * - 启动 /files/（bootWorkspaces）：恰好一个有效入口时 history.replaceState 直入；
   * - 显式切换 / 退出后（listWorkspaces）：一律只列列表/添加 Key，绝不自动恢复，
   *   否则单 Key 用户永远看不到“添加 API Key”的入口。 */
  function fetchWorkspaces(onDone) {
    showWsBooting('正在获取已保存的工作区…');
    var gen = authGen;
    var seq = loginSeq;
    api('GET', '/files/api/workspaces').then(function (res) {
      if (gen !== authGen || seq !== loginSeq) return; // 已有新的登录/入口动作
      if (res.status === 200 && res.json && Array.isArray(res.json.workspaces)) {
        onDone(sanitizeEntries(res.json.workspaces));
        return;
      }
      showGateOnly();
      if (res.network) gateErr('网络错误：无法获取已保存的工作区，可直接输入密钥登录');
      else gateErr('获取工作区列表失败（HTTP ' + (res.status || '?') + '），可直接输入密钥登录');
    });
  }
  function bootWorkspaces() {
    fetchWorkspaces(function (entries) {
      if (entries.length === 1) {
        WS = entries[0];
        try { history.replaceState(null, '', WS.url); } catch (e) { /* noop */ }
        showWsBooting('正在恢复工作区 ' + shortWsId(WS.id) + '…');
        loadMeIntoWorkspace();
        return;
      }
      if (entries.length > 1) { renderWsList(entries); return; }
      showGateOnly();
    });
  }
  function listWorkspaces() {
    fetchWorkspaces(function (entries) {
      if (entries.length) { renderWsList(entries); return; }
      showGateOnly();
    });
  }

  /* list=true：显式切换/退出后只列列表；false（默认）：启动语义，可单项直入。 */
  function toFilesHome(list) {
    try { history.pushState(null, '', '/files/'); } catch (e) { /* noop */ }
    if (list) listWorkspaces();
    else bootWorkspaces();
  }

  /* 进入入口后先 /me（沿用原 /v1 返回），成功才真正进工作区。 */
  function loadMeIntoWorkspace() {
    var gen = authGen;
    var seq = loginSeq;
    var entry = WS;
    api('GET', '/v1/me').then(function (res) {
      if (gen !== authGen || seq !== loginSeq || entry !== WS) return; // 过期响应
      if (res.status === 200 && res.json && Array.isArray(res.json.drives) && res.json.key) {
        authGen++;
        ME = res.json;
        enterWorkspace();
        return;
      }
      if (res.network) { entryOpenFail('网络错误：无法连接服务器，请重试'); return; }
      if (res.status === 401 || res.status === 403) {
        startRelogin(entry ? entry.id : '', '登录已失效（HTTP ' + res.status + '）：此工作区的会话无效或已过期，请重新登录');
        return;
      }
      entryOpenFail('打开工作区失败（HTTP ' + (res.status || '?') + '）：' + errText(res, '请稍后重试'));
    });
  }
  function entryOpenFail(t) {
    if (!el['ws-list'].hidden) {
      el['ws-sub'].textContent = t;
      return;
    }
    showGateOnly();
    gateErr(t);
  }

  /* 会话失效（401）：回到该入口的重登录门；不能被另一个 Key 覆盖。 */
  function startRelogin(id, msg) {
    doLogout(msg || '');
    if (id) applyReloginContext(id);
  }
  function sessionExpired(msg) {
    if (MOCK) { doLogout(msg); return; }
    var id = WS ? WS.id : reloginWsId;
    doLogout(msg);
    if (id) applyReloginContext(id);
  }

  /* 退出登录（正式）：立即清页面状态并中止请求，再 POST 仅当前入口的
   * /logout；随后回 /files/。注销失败时会话仍有效，列表会如实恢复。
   * pagehide 不走这里（不注销 Cookie）。 */
  function productionLogout() {
    var entry = WS;
    doLogout('已退出登录');
    if (!entry) { toFilesHome(true); return; }
    var gen = authGen;
    var seq = loginSeq;
    api('POST', '/files/w/' + encodeURIComponent(entry.id) + '/api/logout', { keepalive: true }).then(function (res) {
      if (gen !== authGen || seq !== loginSeq) return;
      if (res.network) gateErr('退出请求未完成：该工作区会话仍然有效');
      toFilesHome(true); // 只列列表/添加 Key，不自动恢复
    });
  }

  function enterWorkspace() {
    el.gate.hidden = true;
    el['ws-list'].hidden = true;
    el.workspace.hidden = false;
    hideNotice();
    var prefix = ME && ME.key && ME.key.prefix ? ME.key.prefix : '—';
    clear(el['me-prefix']);
    el['me-prefix'].appendChild(document.createTextNode(prefix));
    VIEW = freshView();
    el['browser-view'].hidden = true;
    el['drives-home'].hidden = false;
    renderDrives();
    if (ME.drives.length === 1) enterDrive(ME.drives[0]);
  }

  function doLogout(msg) {
    authGen++;
    navGen++;
    loginSeq++;
    aborts.forEach(function (c) { try { c.abort(); } catch (e) { /* noop */ } });
    aborts.clear();
    queue.forEach(function (t) {
      t.cancelled = true;
      if (t.xhr) { try { t.xhr.abort(); } catch (e) { /* noop */ } }
      t.file = null;
      t.xhr = null;
    });
    queue = [];
    activeTask = null;
    KEY = '';
    ME = null;
    WS = null;
    VIEW = freshView();
    clear(el['drive-grid']);
    clear(el['file-tbody']);
    clear(el['upload-queue']);
    el['upload-panel'].hidden = true;
    el['upload-status'].textContent = '暂无任务';
    el['page-search'].value = '';
    el['conflict-mode'].value = 'fail';
    el['overwrite-ok'].checked = false;
    el['overwrite-row'].hidden = true;
    el['breadcrumb'].textContent = '';
    el['page-info'].textContent = '';
    el['page-prev'].disabled = true;
    el['page-next'].disabled = true;
    closeDialogs();
    shareCtx = null;
    pendingDeletePath = null;
    pendingRenamePath = null;
    el['share-url'].value = '';
    el['share-what'].textContent = '';
    el['share-exp'].textContent = '';
    el['share-result'].hidden = true;
    el['me-prefix'].textContent = '—';
    el['file-input'].value = '';
    el['dz-text'].textContent = '';
    el['dz-title'].textContent = '把文件拖到这里';
    el['upload-hint'].textContent = '';
    setManagerOpen(false);
    el['drives-sub'].textContent = '';
    ['mkdir-where', 'mkdir-name', 'mkdir-err', 'rename-old', 'rename-err', 'delete-path', 'delete-note', 'delete-err', 'share-err'].forEach(function (id) { el[id].textContent = ''; });
    el['mkdir-name'].value = '';
    el['rename-name'].value = '';
    el['gate-go'].disabled = false;
    el.key.type = 'password';
    el['key-toggle'].textContent = '显示';
    resetGateHint();
    hideNotice();
    el.workspace.hidden = true;
    el['ws-list'].hidden = true;
    clear(el['ws-grid']);
    el.gate.hidden = false;
    el.key.value = '';
    gateErr(msg || '');
    try { el.key.focus(); } catch (e) { /* noop */ }
  }

  /* ---------------- drives home ---------------- */

  function permChip(p, fromGlobal) {
    var chip = h('span', { class: 'chip ' + (fromGlobal ? 'perm-g' : 'perm-p') });
    chip.textContent = p;
    chip.title = fromGlobal ? p + '：来自该 Key 的全局权限（global_perms）' : p + '：该网盘的授权（drive.perms）';
    return chip;
  }

  function renderDrives() {
    clear(el['drive-grid']);
    var drives = (ME && Array.isArray(ME.drives)) ? ME.drives : [];
    var lim = limitSize();
    if (!drives.length) {
      el['drives-sub'].textContent = '该密钥没有授权任何网盘：无法浏览文件。请联系管理员在管理门户为该 Key 配置网盘授权（drives）。';
      var empty = h('div', { class: 'empty' });
      empty.appendChild(h('div', { class: 'empty-title', text: '没有已授权的网盘' }));
      empty.appendChild(h('div', { class: 'empty-sub', text: '此 Key 的 /v1/me 未包含任何 drives 授权（no grants）。' }));
      el['drive-grid'].appendChild(empty);
      return;
    }
    el['drives-sub'].textContent = '共 ' + drives.length + ' 个已授权网盘。选择上传位置，上传文件并获取分享链接。';
    drives.forEach(function (d) {
      el['drive-grid'].appendChild(buildDriveCard(d, lim));
    });
  }

  function buildDriveCard(d, lim) {
    var perms = permsFor(d);
    var card = h('div', { class: 'drive-card', 'data-drive-id': d.id });

    var head = h('div', { class: 'drive-card-head' });
    head.appendChild(h('span', { class: 'provider-pill ' + providerTone(d.provider), text: providerLabel(d.provider) }));
    if (d.default) head.appendChild(h('span', { class: 'badge badge-accent fx-default-chip', text: '默认网盘' }));
    card.appendChild(head);

    card.appendChild(h('div', { class: 'fx-drive-name', text: providerLabel(d.provider) }));
    card.appendChild(h('div', { class: 'fx-drive-root', text: 'ID ' + d.id + ' · 根目录 ' + (d.root || '/') }));
    card.appendChild(h('div', { class: 'fx-drive-root', text: d.root && d.root !== '/' ? '（仅能访问 ' + d.root + ' 之内的路径）' : '（可访问整个网盘根目录）' }));

    var permRow = h('div', { class: 'chip-row fx-drive-perms' });
    var drivePerms = Array.isArray(d.perms) ? d.perms : [];
    var globalPerms = (ME && Array.isArray(ME.global_perms)) ? ME.global_perms : [];
    var seen = {};
    drivePerms.forEach(function (p) { seen[p] = true; permRow.appendChild(permChip(p, false)); });
    globalPerms.forEach(function (p) { if (!seen[p]) { seen[p] = true; permRow.appendChild(permChip(p, true)); } });
    if (!permRow.firstChild) permRow.appendChild(h('span', { class: 'fx-sub', text: '无任何权限' }));
    card.appendChild(permRow);

    var status = h('div', { class: 'drive-status' });
    var st = h('div', { class: 'drive-status-top' });
    st.appendChild(healthBadge(d.health));
    st.appendChild(h('span', { class: 'card-desc', text: '健康状态' }));
    status.appendChild(st);
    if (d.health !== 'healthy') {
      var note = h('div', { class: 'fx-health-note fx-warn', text: healthNote(d.health) });
      status.appendChild(note);
    } else {
      status.appendChild(h('div', { class: 'fx-health-note', text: '网盘工作正常。健康异常时这里会显示原因，操作仍可尝试，由服务端最终裁决。' }));
    }
    card.appendChild(status);

    var foot = h('div', { class: 'fx-drive-foot' });
    var enter = h('button', { class: 'btn btn-primary', type: 'button', text: '进入文件空间' });
    if (!perms.has('list')) {
      enter.disabled = true;
      enter.title = '该密钥对此网盘没有 list 权限，无法浏览目录';
    } else {
      enter.title = '浏览 ' + d.id;
    }
    enter.setAttribute('aria-label', '进入网盘 ' + d.id);
    enter.addEventListener('click', function () { enterDrive(d); });
    foot.appendChild(enter);
    foot.appendChild(h('span', { class: 'fx-drive-limit', text: '单文件上传上限 ' + (lim ? fmtBytes(lim) : '未限制') }));
    card.appendChild(foot);
    return card;
  }

  function enterDrive(d) {
    if (!driveById(d.id)) { noticeErr('该网盘已不在当前密钥的授权列表中'); return; }
    if (!permsFor(d).has('list')) { noticeErr('网盘 ' + d.id + ' 未授权 list 权限，无法浏览目录'); return; }
    navGen++;
    closeDialogs();
    VIEW = freshView();
    VIEW.driveId = d.id;
    VIEW.drive = d;
    el['page-search'].value = '';
    el['drives-home'].hidden = true;
    el['browser-view'].hidden = false;
    setManagerOpen(true);
    syncPathHints();
    loadList(1);
  }

  function setManagerOpen(open) {
    el['file-manager'].hidden = !open;
    el['manager-toggle'].setAttribute('aria-expanded', String(open));
    el['manager-toggle'].textContent = open ? '收起文件管理' : '管理文件';
  }

  function backToDrives() {
    navGen++;
    closeDialogs();
    setManagerOpen(false);
    VIEW = freshView();
    el['browser-view'].hidden = true;
    el['drives-home'].hidden = false;
    renderDrives();
  }

  /* ---------------- browser: breadcrumb / list / pager / search ---------------- */

  function crumbBtn(label, cls) {
    var b = h('button', { class: cls, type: 'button', text: label });
    return b;
  }

  function updateBreadcrumb() {
    var bc = el.breadcrumb;
    clear(bc);
    var d = driveById(VIEW.driveId);
    bc.appendChild(crumbBtn(d ? d.id : String(VIEW.driveId), 'fx-crumb fx-crumb-home'));
    var home = bc.lastChild;
    if (VIEW.path === '/') home.setAttribute('aria-current', 'page');
    else {
      home.addEventListener('click', function () { openDir('/'); });
      bc.appendChild(h('span', { class: 'fx-crumb-sep' }));
    }
    var segs = pathSegs(VIEW.path);
    var acc = '';
    segs.forEach(function (s, i) {
      acc += '/' + s;
      var p = acc;
      var b = crumbBtn(s, 'fx-crumb');
      if (i === segs.length - 1) b.setAttribute('aria-current', 'page');
      else b.addEventListener('click', function () { openDir(p); });
      bc.appendChild(b);
      if (i < segs.length - 1) bc.appendChild(h('span', { class: 'fx-crumb-sep' }));
    });
    if (d && d.health && d.health !== 'healthy') {
      bc.appendChild(healthBadge(d.health));
      bc.lastChild.classList.add('fx-drive-health');
    }
  }

  function updateDropzoneText() {
    var d = driveById(VIEW.driveId);
    var canWrite = !!d && permsFor(d).has('write');
    ['mkdir-btn', 'choose-files', 'file-input', 'conflict-mode', 'overwrite-ok'].forEach(function (id) {
      el[id].disabled = !canWrite;
      el[id].title = canWrite ? '' : '该密钥缺少 write 权限，不能写入此网盘';
    });
    el.dropzone.setAttribute('aria-disabled', String(!canWrite));
    var target = (d ? d.id : VIEW.driveId) + ' 的 ' + VIEW.path;
    clear(el['dz-text']);
    el['dz-title'].textContent = canWrite ? '把文件拖到这里' : '此网盘为只读';
    el['upload-hint'].hidden = !!d && permsFor(d).has('link');
    el['upload-hint'].textContent = el['upload-hint'].hidden ? '' : '当前 Key 没有 link 权限，不能生成公开分享链接。';
    if (!canWrite) {
      el['dz-text'].textContent = '此网盘未授予写入权限。';
      return;
    }
    el['dz-text'].appendChild(document.createTextNode('上传到 '));
    var b = document.createElement('b');
    b.className = 'mono';
    b.textContent = target;
    el['dz-text'].appendChild(b);
    el['dz-text'].appendChild(document.createTextNode(' · 支持多文件和 Ctrl+V 粘贴'));
  }

  function openDir(p) {
    closeDialogs();
    VIEW.path = p;
    VIEW.page = 1;
    VIEW.filter = '';
    el['page-search'].value = '';
    syncPathHints();
    loadList(1);
  }

  /* 路径提示不能依赖目录请求成功。 */
  function syncPathHints() {
    updateBreadcrumb();
    updateDropzoneText();
  }

  function loadList(page) {
    if (!VIEW.driveId) return;
    var d = driveById(VIEW.driveId);
    if (!d) { noticeErr('该网盘已不在当前密钥的授权列表中'); backToDrives(); return; }
    if (!permsFor(d).has('list')) {
      VIEW.loading = false;
      VIEW.loadError = '该密钥对此网盘缺少 list 权限，无法列出目录（HTTP 403 之外的本地拦截）。';
      dropListing();
      renderRows();
      updatePager();
      return;
    }
    var myNav = ++navGen;
    VIEW.loading = true;
    VIEW.loadError = null;
    VIEW.page = page;
    dropListing();
    renderRows();
    updatePager();
    var q = new URLSearchParams({ path: VIEW.path, page: String(page), per_page: String(PER_PAGE) });
    api('GET', '/v1/drives/' + encodeURIComponent(VIEW.driveId) + '/list', { query: q }).then(function (res) {
      if (myNav !== navGen) return; // 过期列表：丢弃
      VIEW.loading = false;
      if (res.status === 200 && res.json && Array.isArray(res.json.items)) {
        VIEW.items = res.json.items;
        VIEW.total = Number(res.json.total) || 0;
        VIEW.perPage = Number(res.json.per_page) || PER_PAGE;
        renderRows();
        updatePager();
        return;
      }
      dropListing();
      if (res.status === 401) { sessionExpired('登录已失效（HTTP 401）：密钥无效或已被吊销，请重新输入'); return; }
      if (res.status === 403) {
        VIEW.loadError = 'HTTP 403：权限不足。该密钥对此网盘缺少 list 权限，或路径超出了授权根目录。';
      } else if (res.status === 429) {
        VIEW.loadError = 'HTTP 429：请求过于频繁' + retryAfterText(res.headers) + '，请稍后重试。';
      } else if (res.status === 503) {
        VIEW.loadError = 'HTTP 503：该网盘当前不可用（' + errText(res, 'drive unavailable') + '）。这是网盘健康问题，不是登录问题；可稍后重试。';
      } else if (res.network) {
        VIEW.loadError = '网络错误：无法连接服务器。';
      } else {
        VIEW.loadError = 'HTTP ' + (res.status || '?') + '：' + errText(res, '目录读取失败');
      }
      renderRows();
      updatePager();
    });
  }

  function dropListing() {
    VIEW.items = [];
    VIEW.total = 0;
  }

  function filteredItems() {
    if (!VIEW.filter) return VIEW.items;
    var f = VIEW.filter.toLowerCase();
    return VIEW.items.filter(function (it) { return String(it.name || '').toLowerCase().indexOf(f) >= 0; });
  }

  function actionBtn(label, title, enabled, onClick, disabledReason) {
    var b = h('button', { class: 'btn btn-ghost btn-sm', type: 'button', text: label });
    if (enabled) {
      b.title = title;
      b.addEventListener('click', onClick);
    } else {
      b.disabled = true;
      b.title = disabledReason || '当前密钥没有所需权限';
      b.setAttribute('aria-disabled', 'true');
    }
    return b;
  }

  function renderRows() {
    var tb = el['file-tbody'];
    clear(tb);
    if (VIEW.loading) {
      var lr = document.createElement('tr');
      var ld = h('td', { colspan: '4' });
      var lb = h('div', { class: 'loading-block', text: '正在读取目录…' });
      ld.appendChild(lb);
      lr.appendChild(ld);
      tb.appendChild(lr);
      return;
    }
    if (VIEW.loadError) {
      tb.appendChild(messageRow('目录读取失败', VIEW.loadError, '重试', function () { loadList(VIEW.page); }));
      return;
    }
    var items = filteredItems();
    if (!items.length) {
      if (VIEW.filter) {
        tb.appendChild(messageRow('当前页没有匹配“' + VIEW.filter + '”的条目',
          '这里的搜索只过滤当前页已加载的 ' + VIEW.items.length + ' 项（服务器每页 ' + VIEW.perPage + ' 项），不是全局搜索。翻页或清空搜索可看到其他结果。', null, null));
      } else if (!VIEW.items.length) {
        var msg = VIEW.page === 1 ? '此文件夹为空' : '本页没有条目';
        var sub = VIEW.path === '/' ? '该网盘的根目录目前没有文件或文件夹。' : ('目录 ' + VIEW.path + ' 下没有条目，或它们都在后面几页。');
        tb.appendChild(messageRow(msg, sub, null, null));
      }
      return;
    }
    var d = driveById(VIEW.driveId);
    var perms = permsFor(d);
    items.forEach(function (it) { tb.appendChild(buildRow(it, perms)); });
  }

  function messageRow(title, sub, retryLabel, retryFn) {
    var tr = document.createElement('tr');
    var td = h('td', { colspan: '4' });
    var box = h('div', { class: 'empty fx-empty-inline' });
    box.appendChild(h('div', { class: 'empty-title', text: title }));
    if (sub) box.appendChild(h('div', { class: 'empty-sub', text: sub }));
    if (retryLabel && retryFn) {
      var b = h('button', { class: 'btn btn-secondary btn-sm', type: 'button', text: retryLabel });
      b.addEventListener('click', retryFn);
      box.appendChild(b);
    }
    td.appendChild(box);
    tr.appendChild(td);
    return tr;
  }

  function buildRow(it, perms) {
    var tr = document.createElement('tr');
    var tdN = h('td', { class: 'fx-cell-name' });
    if (it.is_dir) {
      var b = h('button', { class: 'fx-namebtn', type: 'button' });
      b.appendChild(h('span', { class: 'fx-ico-dir' }));
      b.appendChild(document.createTextNode(it.name));
      b.title = '打开目录 ' + it.path;
      b.addEventListener('click', function () { openDir(it.path); });
      tdN.appendChild(b);
    } else {
      var s = h('span', { class: 'fx-nametext' });
      s.appendChild(h('span', { class: 'fx-ico-file' }));
      s.appendChild(document.createTextNode(it.name));
      s.title = it.path;
      tdN.appendChild(s);
    }
    tr.appendChild(tdN);

    tr.appendChild(h('td', { class: 'fx-cell-num', text: it.is_dir ? '—' : fmtBytes(it.size) }));
    tr.appendChild(h('td', { class: 'fx-cell-time', text: fmtTime(it.modified) }));

    var tdA = h('td', { class: 'fx-cell-actions' });
    if (it.is_dir) {
      tdA.appendChild(actionBtn('打开', '打开目录 ' + it.path, true, function () { openDir(it.path); }));
    } else {
      tdA.appendChild(actionBtn('下载', '下载 ' + it.name, perms.has('read'), function () { downloadItem(it); }, '该密钥缺少 read 权限，无法下载'));
      tdA.appendChild(actionBtn('分享', '创建公开链接：' + it.name, perms.has('link'), function () { openShare(VIEW.driveId, it.path, it.name); }, '该密钥缺少 link 权限，无法创建分享链接'));
    }
    tdA.appendChild(actionBtn('重命名', '重命名 ' + it.name, perms.has('write'), function () { openRename(it); }, '该密钥缺少 write 权限，无法重命名'));
    tdA.appendChild(actionBtn('删除', '删除 ' + it.name, perms.has('delete'), function () { openDelete(it); }, '该密钥缺少 delete 权限，无法删除'));
    tr.appendChild(tdA);
    return tr;
  }

  function updatePager() {
    var per = VIEW.perPage || PER_PAGE;
    var totalPages = Math.max(1, Math.ceil(VIEW.total / per));
    el['page-prev'].disabled = VIEW.loading || VIEW.page <= 1 || !!VIEW.loadError;
    el['page-next'].disabled = VIEW.loading || VIEW.page >= totalPages || !!VIEW.loadError;
    /* loading / error 期间不显示“共 N 项”统计：旧目录的统计不应继续冒充当前结果。 */
    if (VIEW.loading) { el['page-info'].textContent = '正在读取目录…'; return; }
    if (VIEW.loadError) { el['page-info'].textContent = '统计不可用（目录读取失败）'; return; }
    var t = '共 ' + VIEW.total + ' 项 · 第 ' + VIEW.page + ' / ' + totalPages + ' 页（服务器分页，每页 ' + per + ' 项）';
    if (VIEW.filter) {
      t += ' · 当前页搜索：' + filteredItems().length + ' / ' + VIEW.items.length + ' 项匹配（仅当前页，非全局搜索）';
    }
    el['page-info'].textContent = t;
  }

  /* ---------------- download ---------------- */

  function downloadItem(it) {
    var d = driveById(VIEW.driveId);
    if (!d || it.is_dir) return;
    if (!permsFor(d).has('read')) { noticeErr('该密钥缺少 read 权限，无法下载 ' + it.name); return; }
    var gen = authGen;
    noticeInfo('正在准备下载 ' + it.name + '…');
    api('POST', '/v1/drives/' + encodeURIComponent(d.id) + '/download-ticket', {
      json: { path: it.path }
    }).then(function (res) {
      if (gen !== authGen) return;
      if (res.status !== 200 || !res.json) {
        if (res.status === 401) { sessionExpired('登录已失效（HTTP 401）：密钥无效或已被吊销，请重新输入'); return; }
        noticeErr(res.network ? '网络错误：无法准备下载，请重试' : 'HTTP ' + (res.status || '?') + '：' + errText(res, '无法准备下载'));
        return;
      }
      var doc = res.json;
      var target;
      try { target = new URL(doc.url, location.origin); } catch (e) { /* invalid ticket */ }
      var expiry = typeof doc.expires_at === 'string' ? Date.parse(doc.expires_at) : NaN;
      if (typeof doc.url !== 'string' || !target || target.origin !== location.origin ||
          (target.protocol !== 'http:' && target.protocol !== 'https:') || target.username || target.password ||
          !/^\/download\/[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$/.test(target.pathname) ||
          target.search || target.hash || (KEY && target.href.indexOf(KEY) !== -1) ||
          !isFinite(expiry) || expiry <= Date.now()) {
        noticeErr('下载凭证无效或已过期，请重试');
        return;
      }
      if (MOCK) {
        noticeInfo('演示模式：已模拟短时下载凭证（无真实字节，不启动浏览器下载）。');
        return;
      }
      var a = document.createElement('a');
      a.href = target.href;
      a.download = it.name;
      a.rel = 'noreferrer noopener';
      a.referrerPolicy = 'no-referrer';
      document.body.appendChild(a);
      a.click();
      a.remove();
      noticeInfo('已交由浏览器下载 ' + it.name + '，进度与结果请查看浏览器下载列表。');
    });
  }

  /* ---------------- share ---------------- */

  function openShare(driveId, path, name) {
    var d = driveById(driveId);
    if (!d) { noticeErr('该网盘已不在当前密钥的授权列表中'); return; }
    if (!permsFor(d).has('link')) { noticeErr('该密钥在网盘 ' + driveId + ' 上缺少 link 权限，无法创建分享链接'); return; }
    shareCtx = { drive: driveId, path: path };
    clear(el['share-what']);
    el['share-what'].appendChild(document.createTextNode(name || path.split('/').pop() || path));
    clear(el['share-err']);
    el['share-result'].hidden = true;
    el['share-url'].value = '';
    el['share-copy'].textContent = '复制';
    clear(el['share-exp']);
    el['share-ok'].disabled = false;
    el['share-ok'].textContent = '生成链接';
    el['share-ttl'].value = '3600';
    openDialog(el['dlg-share']);
  }

  function validateShareUrl(u) {
    var parsed;
    try { parsed = new URL(u); } catch (e) { return '链接不是合法 URL'; }
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return '链接协议必须是 http/https';
    if (parsed.username || parsed.password) return '链接包含凭据，已拒绝';
    return null;
  }

  function doShare(ev) {
    ev.preventDefault();
    if (!shareCtx) { el['dlg-share'].close(); return; }
    clear(el['share-err']);
    el['share-result'].hidden = true;
    el['share-copy'].textContent = '复制';
    var ttl = Number(el['share-ttl'].value) || 3600;
    el['share-ok'].disabled = true;
    el['share-ok'].textContent = '生成中…';
    var gen = authGen;
    var dlgGen = dialogSeq;
    var ctx = shareCtx;
    api('POST', '/v1/links', { json: { drive: ctx.drive, path: ctx.path, expires_in: ttl } }).then(function (res) {
      if (gen !== authGen || dlgGen !== dialogSeq || ctx !== shareCtx || !el['dlg-share'].open) return;
      el['share-ok'].disabled = false;
      el['share-ok'].textContent = '重新生成';
      if (res.status === 200 && res.json && res.json.url) {
        var bad = validateShareUrl(res.json.url);
        if (bad) { shareErr('服务器返回的链接不合法（' + bad + '），已拒绝展示'); return; }
        clear(el['share-url']);
        el['share-url'].value = res.json.url;
        clear(el['share-exp']);
        el['share-exp'].appendChild(document.createTextNode('到期：' + fmtTime(res.json.expires_at) + '（' + ttlHuman(ttl) + '）'));
        el['share-result'].hidden = false;
      } else if (res.status === 401) {
        sessionExpired('登录已失效（HTTP 401）：密钥无效或已被吊销，请重新输入');
      } else if (res.status === 403) {
        shareErr('HTTP 403：该密钥缺少 link 权限');
      } else if (res.status === 404) {
        shareErr('HTTP 404：' + errText(res, '文件不存在（可能已被移动或删除），请刷新后重试'));
      } else if (res.status === 429) {
        shareErr('HTTP 429：请求过于频繁，请稍后再试');
      } else {
        shareErr('HTTP ' + (res.status || '?') + '：' + errText(res, '创建分享链接失败'));
      }
    });
  }

  function shareErr(t) { clear(el['share-err']); el['share-err'].appendChild(document.createTextNode(t)); }

  function ttlHuman(sec) {
    if (sec >= 604800) return '7 天';
    if (sec >= 259200) return '3 天';
    if (sec >= 86400) return '1 天';
    return '1 小时';
  }

  function copyText(text) {
    var gen = authGen;
    var dlgGen = dialogSeq;
    var ctx = shareCtx;
    var valid = function () {
      /* 认证态与模式无关：mock 看内存 KEY（mockKEY），正式看工作区会话 WS
       * （正式 KEY 恒空，绝不能作为判据，否则复制会静默无动作）。其余
       * gen/dialogSeq/shareCtx/对话框打开/结果可见/URL 一致校验保持不变。 */
      var authenticated = MOCK ? !!KEY : !!WS;
      return gen === authGen && dlgGen === dialogSeq && authenticated && ctx === shareCtx &&
        el['dlg-share'].open && !el['share-result'].hidden && el['share-url'].value === text;
    };
    var done = function () {
      if (!valid()) return;
      shareErr('');
      el['share-copy'].textContent = '已复制';
    };
    var fail = function () {
      if (!valid()) return;
      el['share-copy'].textContent = '复制';
      shareErr('未能复制，请按 Ctrl+C 或长按链接复制。');
    };
    var fallback = function () { if (valid()) legacyCopy(text) ? done() : fail(); };
    if (!valid()) return;
    shareErr('');
    if (navigator.clipboard && window.isSecureContext) {
      try {
        navigator.clipboard.writeText(text).then(done, fallback);
        return;
      } catch (e) { /* fall back to the selected link */ }
    }
    fallback();
  }
  function legacyCopy(text) {
    try {
      // A modal dialog makes controls outside it inert, including body-level textareas.
      var field = el['share-url'];
      field.focus();
      field.select();
      field.setSelectionRange(0, text.length);
      return document.execCommand('copy') === true;
    } catch (e) { return false; }
  }

  /* ---------------- mkdir / rename / delete dialogs ---------------- */

  function openMkdir() {
    var d = driveById(VIEW.driveId);
    if (!d) return;
    if (!permsFor(d).has('write')) { noticeErr('该密钥在网盘 ' + d.id + ' 上缺少 write 权限，无法新建文件夹'); return; }
    clear(el['mkdir-err']);
    el['mkdir-name'].value = '';
    clear(el['mkdir-where']);
    el['mkdir-where'].appendChild(document.createTextNode(d.id + ':' + VIEW.path));
    openDialog(el['dlg-mkdir']);
  }

  function doMkdir(ev) {
    ev.preventDefault();
    var name = el['mkdir-name'].value.trim();
    if (!name) { dlgErr(el['mkdir-err'], '请输入文件夹名称'); return; }
    if (name.indexOf('/') >= 0) { dlgErr(el['mkdir-err'], '名称不能包含 /'); return; }
    if (name === '.' || name === '..') { dlgErr(el['mkdir-err'], '名称不能是 . 或 ..'); return; }
    var target = joinPath(VIEW.path, name);
    var gen = authGen;
    var dlgGen = dialogSeq;
    api('POST', '/v1/drives/' + encodeURIComponent(VIEW.driveId) + '/mkdir', { json: { path: target } }).then(function (res) {
      if (gen !== authGen || dlgGen !== dialogSeq || !el['dlg-mkdir'].open) return;
      if (res.status === 200) {
        el['dlg-mkdir'].close();
        noticeOk('已创建文件夹 ' + name);
        loadList(VIEW.page);
      } else if (res.status === 401) {
        sessionExpired('登录已失效（HTTP 401）：密钥无效或已被吊销，请重新输入');
      } else {
        dlgErr(el['mkdir-err'], 'HTTP ' + (res.status || '?') + '：' + errText(res, '创建失败'));
      }
    });
  }

  function openRename(it) {
    var d = driveById(VIEW.driveId);
    if (!d || !permsFor(d).has('write')) return;
    pendingRenamePath = it.path;
    clear(el['rename-old']);
    el['rename-old'].appendChild(document.createTextNode(it.path));
    el['rename-name'].value = it.name;
    clear(el['rename-err']);
    openDialog(el['dlg-rename']);
  }

  function doRename(ev) {
    ev.preventDefault();
    if (!pendingRenamePath) { el['dlg-rename'].close(); return; }
    var newName = el['rename-name'].value.trim();
    if (!newName) { dlgErr(el['rename-err'], '请输入新名称'); return; }
    if (newName.indexOf('/') >= 0 || newName === '.' || newName === '..') { dlgErr(el['rename-err'], '名称不能包含 /，也不能是 . 或 ..'); return; }
    var oldName = pendingRenamePath.split('/').pop();
    if (newName === oldName) { el['dlg-rename'].close(); return; }
    var to = joinPath(dirname(pendingRenamePath), newName);
    var gen = authGen;
    var dlgGen = dialogSeq;
    api('POST', '/v1/drives/' + encodeURIComponent(VIEW.driveId) + '/move', {
      json: { from: pendingRenamePath, to: to, on_conflict: 'fail' }
    }).then(function (res) {
      if (gen !== authGen || dlgGen !== dialogSeq || !el['dlg-rename'].open) return;
      if (res.status === 200) {
        el['dlg-rename'].close();
        noticeOk('已重命名为 ' + newName);
        loadList(VIEW.page);
      } else if (res.status === 401) {
        sessionExpired('登录已失效（HTTP 401）：密钥无效或已被吊销，请重新输入');
      } else if (res.status === 409) {
        dlgErr(el['rename-err'], '目标名称已存在：' + newName + '（on_conflict=fail，请换一个名称）');
      } else {
        dlgErr(el['rename-err'], 'HTTP ' + (res.status || '?') + '：' + errText(res, '重命名失败'));
      }
    });
  }

  function openDelete(it) {
    var d = driveById(VIEW.driveId);
    if (!d || !permsFor(d).has('delete')) return;
    pendingDeletePath = it.path;
    clear(el['delete-path']);
    el['delete-path'].appendChild(document.createTextNode(d.id + ':' + it.path + (it.is_dir ? '（整个文件夹）' : '')));
    var trash = ME && ME.trash;
    clear(el['delete-note']);
    el['delete-note'].appendChild(document.createTextNode(
      trash && trash.enabled
        ? '该密钥已开启回收站：删除后内容先移入 /.c2a-trash，' + ((trash.days || 7)) + ' 天后自动清除。'
        : '该密钥未开启回收站：删除为直接删除，不可恢复。'
    ));
    clear(el['delete-err']);
    openDialog(el['dlg-delete']);
  }

  function doDelete(ev) {
    ev.preventDefault();
    if (!pendingDeletePath) { el['dlg-delete'].close(); return; }
    var target = pendingDeletePath;
    var gen = authGen;
    var dlgGen = dialogSeq;
    api('POST', '/v1/drives/' + encodeURIComponent(VIEW.driveId) + '/delete', { json: { paths: [target] } }).then(function (res) {
      if (gen !== authGen || dlgGen !== dialogSeq || !el['dlg-delete'].open) return;
      if (res.status === 200 && res.json && Array.isArray(res.json.results) && res.json.results.length) {
        var r0 = res.json.results[0];
        el['dlg-delete'].close();
        if (r0.status === 'deleted') noticeOk('已删除 ' + target);
        else if (r0.status === 'trashed') noticeOk('已移入回收站 ' + target);
        else if (r0.status === 'already_absent') noticeInfo('目标已不存在（视为删除成功）');
        else noticeErr('删除失败：' + (r0.error || '未知错误') + '（' + (r0.path || target) + '）');
        loadList(VIEW.page);
      } else if (res.status === 401) {
        sessionExpired('登录已失效（HTTP 401）：密钥无效或已被吊销，请重新输入');
      } else if (res.status === 200) {
        dlgErr(el['delete-err'], '服务器响应缺少逐项结果（results），删除结果未知，请刷新目录核实');
      } else {
        dlgErr(el['delete-err'], 'HTTP ' + (res.status || '?') + '：' + errText(res, '删除失败'));
      }
    });
  }

  function dlgErr(node, t) { clear(node); node.appendChild(document.createTextNode(t)); }

  /* ---------------- uploads ---------------- */

  function makeTask(file) {
    return {
      id: ++taskSeq,
      file: file,
      name: file.name,
      size: file.size,
      drive: VIEW.driveId,
      dir: VIEW.path,
      targetPath: joinPath(VIEW.path, file.name),
      onConflict: el['conflict-mode'].value || 'fail',
      state: 'queued', // queued | uploading | done | failed | interrupted | unconfirmed
      loaded: 0,
      cancelled: false,
      finalPath: null,
      conflict: null,
      error: null,
      xhr: null,
      overLimit: false,
      row: null
    };
  }

  function enqueueFiles(fileList) {
    if (!VIEW.driveId || el['browser-view'].hidden) return;
    var d = driveById(VIEW.driveId);
    if (!d) { noticeErr('该网盘已不在当前密钥的授权列表中'); return; }
    if (!permsFor(d).has('write')) { noticeErr('该密钥在网盘 ' + d.id + ' 上缺少 write 权限，无法上传'); return; }
    var mode = el['conflict-mode'].value || 'fail';
    if (mode === 'overwrite' && !el['overwrite-ok'].checked) {
      noticeErr('已选择“覆盖”模式：请先在下方勾选“我确认要覆盖云端同名文件”后再上传');
      return;
    }
    var lim = limitSize();
    var added = 0;
    for (var i = 0; i < fileList.length; i++) {
      var f = fileList[i];
      if (!f || typeof f.size !== 'number' || f.size < 0) continue;
      var t = makeTask(f);
      t.overLimit = !!(lim && f.size > lim);
      queue.push(t);
      added++;
    }
    if (!added) { noticeErr('没有可上传的文件（暂不支持文件夹，请选择具体文件）'); return; }
    renderQueue();
    pump();
  }

  function pump() {
    if (activeTask) { updateQueueStatus(); return; }
    for (var i = 0; i < queue.length; i++) {
      var t = queue[i];
      if (t.state === 'queued' && t.overLimit) {
        t.state = 'failed';
        t.error = '文件 ' + fmtBytes(t.size) + ' 超过该密钥的单文件上传上限 ' + fmtBytes(limitSize()) + '（limits.max_upload_bytes）';
        t.file = null;
        updateTaskRow(t);
      }
    }
    var next = null;
    for (var j = 0; j < queue.length; j++) {
      if (queue[j].state === 'queued' && !queue[j].overLimit) { next = queue[j]; break; }
    }
    if (!next) { activeTask = null; updateQueueStatus(); return; }
    activeTask = next;
    next.state = 'uploading';
    updateTaskRow(next);
    var gen = authGen;
    var runner = MOCK ? mockUpload(next, gen) : xhrUpload(next, gen);
    runner.then(function (outcome) {
      if (gen !== authGen) return;
      activeTask = null;
      handleUploadOutcome(next, outcome);
      pump();
    });
    updateQueueStatus();
  }

  function handleUploadOutcome(task, outcome) {
    if (task.cancelled || outcome.cancelled) {
      task.state = 'interrupted';
      task.error = '已取消：传输中止，服务器上是否存在此文件未知。请刷新目录核实；本页不会自动重试。';
      task.file = null;
    } else if (outcome.network) {
      task.state = 'interrupted';
      task.error = '网络中断：上传结果未知（可能已部分上传）。请刷新目录核实；本页不会自动重试。';
      task.file = null;
    } else if (outcome.status >= 200 && outcome.status < 300) {
      var v = validateUpload(outcome.json, task);
      if (v === true) {
        task.state = 'done';
        task.finalPath = outcome.json.path;
        task.conflict = outcome.json.conflict;
        task.error = null;
      } else {
        task.state = 'unconfirmed';
        task.error = 'HTTP ' + outcome.status + ' 但响应不完整（' + v + '）——上传结果未知。请刷新目录核实；为避免产生重复文件，本页不会自动重试。';
      }
      task.file = null;
    } else if (outcome.status === 401) {
      task.state = 'failed';
      task.error = 'HTTP 401：密钥已失效，上传被拒绝。';
      task.file = null;
      sessionExpired('登录已失效（HTTP 401）：密钥无效或已被吊销，请重新输入');
    } else {
      task.state = 'failed';
      task.error = 'HTTP ' + (outcome.status || '?') + '：' + errText(outcome, '上传失败') + '。失败不代表远端未写入，请先刷新目录核实，再重新选择文件。';
      task.file = null;
    }
    task.xhr = null;
    updateTaskRow(task);
    refreshIfActiveDir(task.drive, task.dir);
    updateQueueStatus();
  }

  /* 成功只认：完整 JSON {path,size,crc64,sha256,conflict} 且 path/size 正确 */
  function validateUpload(doc, task) {
    if (!doc || typeof doc !== 'object') return '响应不是 JSON 对象';
    if (typeof doc.path !== 'string' || doc.path.charAt(0) !== '/') return '缺少有效 path 字段';
    if (typeof doc.size !== 'number' || !isFinite(doc.size) || doc.size !== task.size) {
      return 'size 与文件不符：服务器报告 ' + doc.size + '，本地 ' + task.size;
    }
    if (typeof doc.crc64 !== 'string' || !/^\d{1,20}$/.test(doc.crc64)) return 'crc64 字段无效';
    if (typeof doc.sha256 !== 'string' || !/^[a-fA-F0-9]{64}$/.test(doc.sha256)) return 'sha256 字段无效';
    if (doc.conflict !== 'created' && doc.conflict !== 'renamed' && doc.conflict !== 'overwritten') {
      return 'conflict 字段异常：' + String(doc.conflict);
    }
    var dirn = dirname(task.targetPath);
    if (doc.conflict === 'renamed') {
      if (doc.path === task.targetPath || dirname(doc.path) !== dirn) return '重命名后的 path 不在目标目录内';
    } else if (doc.path !== task.targetPath) {
      return 'path 与上传目标不符：' + doc.path + ' ≠ ' + task.targetPath;
    }
    return true;
  }

  function refreshIfActiveDir(drive, dir) {
    if (VIEW.driveId !== drive || el['browser-view'].hidden) return;
    if (VIEW.path === dir) loadList(VIEW.page);
  }

  function setTaskProgress(task, loaded, total) {
    task.loaded = loaded;
    if (task.row) {
      var max = total > 0 ? total : (task.size || 1);
      task.row.prog.setAttribute('max', String(max));
      task.row.prog.setAttribute('value', String(Math.min(loaded, max)));
      var pct = Math.floor((Math.min(loaded, max) / max) * 100);
      task.row.state.textContent = pct >= 100 ? '已发送 100%，等待云端确认…' : '上传中 ' + pct + '%';
      task.row.state.className = 'fx-task-state fx-state-busy';
    }
    updateQueueStatus();
  }

  function xhrUpload(task, gen) {
    return new Promise(function (resolve) {
      var url = apiPath('/v1/drives/' + encodeURIComponent(task.drive) + '/files') +
        '?path=' + encodeURIComponent(task.targetPath) +
        '&on_conflict=' + encodeURIComponent(task.onConflict);
      var xhr = new XMLHttpRequest();
      task.xhr = xhr;
      try {
        xhr.open('PUT', url, true);
      } catch (e) {
        task.xhr = null;
        resolve({ network: true });
        return;
      }
      if (MOCK) {
        xhr.withCredentials = false;
        xhr.setRequestHeader('apikey', KEY); // mock：Key 只进请求头，不进 URL
      } else {
        xhr.withCredentials = true; // 正式：同源上传携带会话 Cookie，无 apikey 头
        xhr.setRequestHeader('X-C2A-Workspace', '1');
      }
      xhr.setRequestHeader('Idempotency-Key', uuid()); // 每次尝试唯一
      var zero = task.size === 0;
      if (!zero) xhr.setRequestHeader('Content-Type', 'application/octet-stream');
      xhr.upload.onprogress = function (e) {
        if (gen !== authGen || task.cancelled) return;
        setTaskProgress(task, e.loaded, e.total || task.size);
      };
      xhr.onload = function () {
        if (gen !== authGen) { resolve({ stale: true }); return; }
        var doc = null;
        try { doc = JSON.parse(xhr.responseText); } catch (e) { doc = null; }
        resolve({ status: xhr.status, json: doc, headers: null });
      };
      xhr.onerror = function () {
        if (gen !== authGen) { resolve({ stale: true }); return; }
        resolve({ network: true });
      };
      xhr.onabort = function () {
        if (gen !== authGen) { resolve({ stale: true }); return; }
        resolve({ cancelled: true });
      };
      if (zero) {
        // 零字节：裸 PUT 会被 Content-Length<=0 拒绝，改走 multipart file 字段
        var fd = new FormData();
        fd.append('file', task.file, task.name);
        xhr.send(fd);
      } else {
        xhr.send(task.file); // 原始 File 体
      }
    });
  }

  function mockUpload(task, gen) {
    var q = new URLSearchParams({ path: task.targetPath, on_conflict: task.onConflict });
    return mockRequest('PUT', '/v1/drives/' + encodeURIComponent(task.drive) + '/files', {
      query: q,
      body: { name: task.name, size: task.size },
      apiKey: KEY,
      task: task,
      onProgress: function (loaded, total) {
        if (gen === authGen && !task.cancelled) setTaskProgress(task, loaded, total);
      }
    }).then(
      function (doc) { return { status: 200, json: doc, headers: null }; },
      function (e) {
        if (e && e.mock) return { status: e.mockStatus, json: e.mockBody, headers: null };
        return { network: true };
      }
    );
  }

  function cancelTask(t) {
    if (t.state !== 'queued' && t.state !== 'uploading') return;
    t.cancelled = true;
    if (t.xhr) { try { t.xhr.abort(); } catch (e) { /* noop */ } }
    if (t.state === 'queued') {
      // 还没开始：直接从队列移除
      var i = queue.indexOf(t);
      if (i >= 0) queue.splice(i, 1);
      t.file = null;
      renderQueue();
      updateQueueStatus();
    }
  }

  function dropTask(t) {
    var i = queue.indexOf(t);
    if (i >= 0) queue.splice(i, 1);
    t.cancelled = true;
    t.file = null;
    renderQueue();
    updateQueueStatus();
  }

  function buildTaskRow(t) {
    var root = h('div', { class: 'fx-task' });
    var nameCell = h('div', { class: 'fx-task-name', text: t.name });
    nameCell.appendChild(h('span', { class: 'fx-task-sub', text: t.drive + ':' + t.targetPath + ' · ' + fmtBytes(t.size) }));
    root.appendChild(nameCell);

    var mid = h('div', { class: 'fx-task-mid' });
    var prog = document.createElement('progress');
    prog.setAttribute('max', String(t.size || 1));
    prog.setAttribute('value', String(t.state === 'done' ? (t.size || 1) : Math.min(t.loaded || 0, t.size || 1)));
    prog.setAttribute('aria-label', '上传进度 ' + t.name);
    mid.appendChild(prog);
    root.appendChild(mid);

    var state = h('div', { class: 'fx-task-state fx-state-busy', text: '等待' });
    root.appendChild(state);

    var act = h('div', { class: 'fx-task-act' });
    root.appendChild(act);

    var row = { root: root, prog: prog, state: state, act: act, err: null };
    t.row = row;
    updateTaskRow(t);
    return root;
  }

  var STATE_LABELS = {
    queued: ['等待', 'fx-state-busy'],
    uploading: ['上传中…', 'fx-state-busy'],
    done: ['已完成', 'fx-state-done'],
    failed: ['失败', 'fx-state-failed'],
    interrupted: ['中断/未知', 'fx-state-interrupted'],
    unconfirmed: ['未确认', 'fx-state-interrupted']
  };

  function updateTaskRow(t) {
    if (!t.row) return;
    var map = STATE_LABELS[t.state] || [t.state, 'fx-state-busy'];
    t.row.state.textContent = map[0];
    t.row.state.className = 'fx-task-state ' + map[1];
    clear(t.row.act);
    if (t.state === 'queued' || t.state === 'uploading') {
      var c = h('button', { class: 'btn btn-ghost btn-sm', type: 'button', text: '取消' });
      c.title = '取消上传 ' + t.name;
      c.addEventListener('click', function () { cancelTask(t); });
      t.row.act.appendChild(c);
    } else if (t.state === 'done') {
      if (t.conflict === 'renamed') t.row.state.textContent = '已完成（服务器重命名）';
      var d = driveById(t.drive);
      if (d && permsFor(d).has('link') && t.finalPath) {
        var s = h('button', { class: 'btn btn-primary btn-sm', type: 'button', text: '获取链接' });
        s.title = '为 ' + t.finalPath + ' 创建公开链接';
        s.addEventListener('click', function () { openShare(t.drive, t.finalPath, t.name); });
        t.row.act.appendChild(s);
      }
      var x = h('button', { class: 'btn btn-ghost btn-sm', type: 'button', text: '移除' });
      x.title = '从队列中移除该记录';
      x.addEventListener('click', function () { dropTask(t); });
      t.row.act.appendChild(x);
    } else if (t.state === 'failed') {
      var d2 = h('button', { class: 'btn btn-ghost btn-sm', type: 'button', text: '移除' });
      d2.title = '从队列中移除该记录';
      d2.addEventListener('click', function () { dropTask(t); });
      t.row.act.appendChild(d2);
    } else {
      var x2 = h('button', { class: 'btn btn-ghost btn-sm', type: 'button', text: '移除' });
      x2.title = '从队列中移除该记录';
      x2.addEventListener('click', function () { dropTask(t); });
      t.row.act.appendChild(x2);
    }
    // 错误说明行
    if (t.row.err) { t.row.err.parentNode.removeChild(t.row.err); t.row.err = null; }
    if (t.error) {
      var warn = t.state === 'interrupted' || t.state === 'unconfirmed';
      var err = h('div', { class: 'fx-task-err' + (warn ? ' fx-warn' : ''), text: t.error });
      t.row.root.appendChild(err);
      t.row.err = err;
    }
  }

  function renderQueue() {
    var q = el['upload-queue'];
    clear(q);
    el['upload-panel'].hidden = queue.length === 0;
    if (!queue.length) {
      q.appendChild(h('div', { class: 'empty', text: '暂无上传任务' }));
      return;
    }
    queue.forEach(function (t) {
      q.appendChild(buildTaskRow(t));
    });
  }

  function updateQueueStatus() {
    var counts = { queued: 0, uploading: 0, done: 0, failed: 0, interrupted: 0, unconfirmed: 0 };
    queue.forEach(function (t) {
      counts[t.state] = (counts[t.state] || 0) + 1;
    });
    if (!queue.length) {
      el['upload-status'].textContent = '暂无任务';
      return;
    }
    var parts = [];
    if (counts.uploading) parts.push('上传中 ' + counts.uploading);
    if (counts.queued) parts.push('等待 ' + counts.queued);
    if (counts.done) parts.push('已完成 ' + counts.done);
    if (counts.failed) parts.push('失败 ' + counts.failed);
    if (counts.interrupted + counts.unconfirmed) parts.push('中断/未知 ' + (counts.interrupted + counts.unconfirmed));
    el['upload-status'].textContent = '共 ' + queue.length + ' 个 · ' + parts.join(' · ');
  }

  function collectDropFiles(dt) {
    var files = [];
    var dirs = 0;
    if (!dt) return { files: files, dirs: dirs };
    if (dt.items && dt.items.length && typeof dt.items[0].webkitGetAsEntry === 'function') {
      for (var i = 0; i < dt.items.length; i++) {
        var item = dt.items[i];
        if (item.kind !== 'file') continue;
        var entry = null;
        try { entry = item.webkitGetAsEntry(); } catch (e) { entry = null; }
        if (entry && entry.isDirectory) { dirs++; continue; }
        var f = item.getAsFile();
        if (f) files.push(f);
      }
    } else if (dt.files && dt.files.length) {
      for (var j = 0; j < dt.files.length; j++) files.push(dt.files[j]);
    }
    return { files: files, dirs: dirs };
  }

  /* ---------------- boot / events ---------------- */

  function bindEvents() {
    el['login-form'].addEventListener('submit', doLogin);
    el['key-toggle'].addEventListener('click', function () {
      var hidden = el.key.type === 'password';
      el.key.type = hidden ? 'text' : 'password';
      el['key-toggle'].textContent = hidden ? '隐藏' : '显示';
    });
    el.logout.addEventListener('click', function () {
      if (MOCK) { doLogout('已退出登录：密钥已从内存清除'); return; }
      productionLogout(); // POST 仅当前入口 /logout，成功后回 /files/
    });
    el['switch-ws'].addEventListener('click', function () {
      if (MOCK) return; // mock 无持久工作区
      doLogout(''); // 清页面状态并中止请求，但不动服务端会话
      toFilesHome(true); // 显式切换只列列表/添加 Key，绝不自动恢复单项
    });
    el['ws-add-key'].addEventListener('click', function () {
      el.gate.hidden = false;
      try { el.key.focus(); } catch (e) { /* noop */ }
    });
    el['theme-toggle'].addEventListener('click', toggleTheme);

    el['back-drives'].addEventListener('click', backToDrives);
    el['manager-toggle'].addEventListener('click', function () { setManagerOpen(el['file-manager'].hidden); });
    el['refresh-btn'].addEventListener('click', function () { if (!el['browser-view'].hidden) loadList(VIEW.page); });
    el['mkdir-btn'].addEventListener('click', openMkdir);
    el['page-search'].addEventListener('input', function () {
      VIEW.filter = el['page-search'].value.trim();
      renderRows();
      updatePager();
    });
    el['page-prev'].addEventListener('click', function () { if (VIEW.page > 1) loadList(VIEW.page - 1); });
    el['page-next'].addEventListener('click', function () { loadList(VIEW.page + 1); });

    // 上传：点击 / 键盘 / 拖放 / 粘贴 / 冲突选择
    el['choose-files'].addEventListener('click', function () { el['file-input'].click(); });
    el.dropzone.addEventListener('click', function (e) {
      if (e.target.closest('button, input, select, label, option')) return;
      if (!el['file-input'].disabled) el['file-input'].click();
    });
    ['dragenter', 'dragover'].forEach(function (ev) {
      el.dropzone.addEventListener(ev, function (e) { e.preventDefault(); el.dropzone.classList.add('fx-drag'); });
    });
    ['dragleave', 'dragend'].forEach(function (ev) {
      el.dropzone.addEventListener(ev, function () { el.dropzone.classList.remove('fx-drag'); });
    });
    el.dropzone.addEventListener('drop', function (e) {
      e.preventDefault();
      el.dropzone.classList.remove('fx-drag');
      var got = collectDropFiles(e.dataTransfer);
      if (!got.files.length) {
        noticeErr(got.dirs ? '暂不支持整个文件夹拖放：请打开文件夹后选择其中的文件' : '没有可上传的文件');
        return;
      }
      enqueueFiles(got.files);
    });
    el['file-input'].addEventListener('change', function () {
      if (el['file-input'].files && el['file-input'].files.length) enqueueFiles(el['file-input'].files);
      el['file-input'].value = ''; // 允许重复选择同一文件
    });
    document.addEventListener('paste', function (e) {
      if (el.workspace.hidden || el['browser-view'].hidden) return;
      var files = e.clipboardData && e.clipboardData.files;
      if (files && files.length) { e.preventDefault(); enqueueFiles(files); }
    });
    el['conflict-mode'].addEventListener('change', function () {
      el['overwrite-row'].hidden = el['conflict-mode'].value !== 'overwrite';
      if (el['conflict-mode'].value !== 'overwrite') el['overwrite-ok'].checked = false;
    });

    // 对话框
    ['dlg-mkdir', 'dlg-rename', 'dlg-delete', 'dlg-share'].forEach(function (id) { bindDialog(el[id]); });
    el['mkdir-form'].addEventListener('submit', doMkdir);
    el['rename-form'].addEventListener('submit', doRename);
    el['delete-form'].addEventListener('submit', doDelete);
    el['share-form'].addEventListener('submit', doShare);
    el['share-copy'].addEventListener('click', function () {
      if (!shareCtx || !el['dlg-share'].open || el['share-result'].hidden) return;
      var v = el['share-url'].value;
      if (!v) return;
      copyText(v);
    });
    // 离开页面：清除内存中的 Key 与 File 引用
    window.addEventListener('pagehide', function () { doLogout(); });
  }

  function boot() {
    cacheEls();
    initTheme();
    if (MOCK) {
      mockSeed();
      mockRegister();
      exposeMock();
      el['mock-banner'].hidden = false;
      el['switch-ws'].hidden = true; // mock 无持久工作区
      bindEvents();
      el.workspace.hidden = true;
      el['ws-list'].hidden = true;
      el.gate.hidden = false;
      try { el.key.focus(); } catch (e) { /* noop */ }
      return;
    }
    bindEvents();
    el.workspace.hidden = true;
    el.gate.hidden = true;
    el['ws-list'].hidden = true;
    var m = /^\/files\/w\/([A-Za-z0-9_-]{8,128})\/$/.exec(location.pathname);
    if (m) deepRestore(m[1]); // 指定入口：先 /me，401 回该入口的重登录门
    else bootWorkspaces(); // 仅启动 /files/ 时允许单项自动直入
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
