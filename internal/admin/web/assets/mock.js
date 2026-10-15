/* cloud2api admin — in-memory mock backend (?mock=1).
 * Mirrors the step-2 OpenList-entry contract: cloud2api never writes OpenList
 * storage config. Removed endpoints (drive/key creation, drive settings,
 * providers, login flows) answer 404; drive rows carry `enabled` (cloud2api's
 * own switch) and `storage_disabled` (OpenList side) instead of
 * customized_fields. State lives in RAM only and is reset on reload. */
(function () {
  'use strict';

  const now = () => new Date().toISOString();
  const hoursAgo = (h) => new Date(Date.now() - h * 3600e3).toISOString();
  const daysFromNow = (d) => new Date(Date.now() + d * 86400e3).toISOString();
  const sleep = (ms) => new Promise((res) => setTimeout(res, ms));
  const clone = (v) => JSON.parse(JSON.stringify(v));

  function rng(len) {
    const chars = 'abcdef0123456789';
    const buf = new Uint8Array(len);
    crypto.getRandomValues(buf);
    let s = '';
    for (let i = 0; i < len; i++) s += chars[buf[i] % 16];
    return s;
  }
  /** Key 明文前缀（8 字符），明文 = sk-c2a-<prefix>-<32 字符密钥>；与真实后端一致。 */
  function genPrefix() { return rng(8); }
  function genPlaintext(prefix) {
    return 'sk-c2a-' + (prefix || genPrefix()) + '-' + rng(24) + rng(8);
  }

  const PERM_VOCAB = ['list', 'read', 'write', 'delete', 'purge', 'transfer', 'link', 'fetch', 'delegate', 'relay'];

  /* ---------------- seed data ---------------- */

  function seed() {
    /* Drive rows mirror the live step-2 contract: `enabled` = cloud2api's own
     * switch (expose to agents), `storage_disabled` = the storage is disabled
     * in OpenList. `state` is 'disabled' for either cause. There is no
     * customized_fields anymore. One drive demos each disabled cause. */
    const drives = [
      {
        id: 'baidu-main', provider: 'baidu', driver: 'BaiduNetdisk', mount_path: '/d/baidu-main',
        storage_id: 3, state: 'healthy', status_text: 'work', enabled: true, storage_disabled: false,
        created_at: hoursAgo(240), static: false,
      },
      {
        id: 'aliyun-main', provider: 'aliyun', driver: 'AliyundriveOpen', mount_path: '/d/aliyun-main',
        storage_id: 4, state: 'needs_reauth', status_text: 'failed refresh token: invalid grant token',
        enabled: true, storage_disabled: false, created_at: hoursAgo(500), static: false,
      },
      {
        id: 'pan123-main', provider: '123pan', driver: '123Pan', mount_path: '/d/pan123-main',
        storage_id: 5, state: 'healthy', status_text: 'work', enabled: true, storage_disabled: false,
        created_at: hoursAgo(210), static: false,
      },
      {
        id: 'pan115-open', provider: '115open', driver: '115 Open', mount_path: '/d/pan115',
        storage_id: 6, state: 'error', status_text: 'failed get fs: upstream timeout after 30s',
        enabled: true, storage_disabled: false, created_at: hoursAgo(120), static: false,
      },
      {
        id: 'local-backup', provider: 'local', driver: 'Local', mount_path: '/d/backup',
        storage_id: 7, state: 'healthy', status_text: 'work', enabled: true, storage_disabled: false,
        created_at: hoursAgo(960), static: true,
      },
      {
        // disabled in cloud2api only: the OpenList storage itself is fine
        id: 'nas-open', provider: 'webdav', driver: 'WebDav', mount_path: '/nas',
        storage_id: 8, state: 'disabled', status_text: 'disabled', enabled: false, storage_disabled: false,
        created_at: hoursAgo(300), static: false,
      },
      {
        // disabled in OpenList only: cloud2api's switch is still on, the fix
        // lives in OpenList (edit link), so the portal offers no 启用 button
        id: 'team-share', provider: 'onedrive', driver: 'OneDrive', mount_path: '/d/team',
        storage_id: 10, state: 'disabled', status_text: 'disabled', enabled: true, storage_disabled: true,
        created_at: hoursAgo(420), static: false,
      },
      {
        id: 'quark-old', provider: 'quark', driver: 'QuarkOpen', mount_path: '/d/quark',
        storage_id: 9, state: 'orphaned', status_text: 'no OpenList storage for this drive',
        enabled: true, storage_disabled: false, created_at: hoursAgo(720), static: false,
      },
      {
        // storage 14 was deleted in OpenList; a Local storage now sits on the
        // same mount → needs the admin's confirmation (rebind) before agent
        // access resumes. driver_changed=true demos the prominent warning.
        id: 'media-old', provider: 'webdav', driver: 'WebDav', mount_path: '/d/media',
        storage_id: 14, state: 'replacement_found',
        status_text: 'storage 14 not found; unmanaged storage 15 found on mount /d/media',
        enabled: true, storage_disabled: false, created_at: hoursAgo(800), static: false,
        replacement: { storage_id: 15, driver: 'Local', mount_path: '/d/media', status: 'work', disabled: false, driver_changed: true },
      },
    ];

    const keys = [
      {
        id: 'histopilot', parent_id: null, prefix: 'sk-c2a-hstopl01', static: false, revoked: false,
        created_at: hoursAgo(200), expires_at: null, last_used_at: hoursAgo(0.4),
        default_drive: 'baidu-main',
        drives: [
          { drive: 'baidu-main', root: '/agents/histopilot', perms: ['list', 'read', 'write', 'delete'] },
          { drive: 'pan123-main', root: '/', perms: ['list', 'read'] },
        ],
        global_perms: ['transfer', 'link'],
        limits: { max_upload_bytes: 8 * 1024 ** 3, requests_per_minute: 120, concurrent_jobs: 2 },
        trash: { enabled: true, days: 7 },
        relay: { drive: null, quota_bytes: 0, max_age_days: 0 },
      },
      {
        // Gokapi relay demo: relay permission + explicit relay config (1d)
        id: 'relay-handoff', parent_id: null, prefix: 'sk-c2a-rlhand01', static: false, revoked: false,
        created_at: hoursAgo(50), expires_at: null, last_used_at: hoursAgo(0.9),
        default_drive: 'baidu-main',
        drives: [{ drive: 'baidu-main', root: '/', perms: ['relay'] }],
        global_perms: [],
        limits: { max_upload_bytes: 2 * 1024 ** 3, requests_per_minute: 60, concurrent_jobs: 1 },
        trash: { enabled: true, days: 3 },
        relay: { drive: 'baidu-main', quota_bytes: 20 * 1024 ** 3, max_age_days: 3 },
      },
      {
        id: 'histopilot-sub', parent_id: 'histopilot', prefix: 'sk-c2a-hsub2k09', static: false, revoked: false,
        created_at: hoursAgo(30), expires_at: daysFromNow(0.8), last_used_at: hoursAgo(2),
        default_drive: 'baidu-main',
        drives: [{ drive: 'baidu-main', root: '/agents/histopilot/tmp', perms: ['list', 'read', 'write'] }],
        global_perms: [],
        limits: { max_upload_bytes: 1024 ** 3, requests_per_minute: 60, concurrent_jobs: 1 },
        trash: { enabled: true, days: 1 },
      },
      {
        id: 'readonly-report', parent_id: null, prefix: 'sk-c2a-roreport', static: false, revoked: false,
        created_at: hoursAgo(420), expires_at: null, last_used_at: hoursAgo(26),
        default_drive: 'pan123-main',
        drives: [
          { drive: 'baidu-main', root: '/reports', perms: ['list', 'read'] },
          { drive: 'pan123-main', root: '/', perms: ['list', 'read', 'link'] },
        ],
        global_perms: ['link'],
        limits: { max_upload_bytes: null, requests_per_minute: 30, concurrent_jobs: 1 },
        trash: { enabled: false, days: 7 },
      },
      {
        id: 'etl-loader', parent_id: null, prefix: 'sk-c2a-etllod07', static: false, revoked: false,
        created_at: hoursAgo(96), expires_at: null, last_used_at: hoursAgo(5),
        default_drive: 'pan115-open',
        drives: [{ drive: 'pan115-open', root: '/etl', perms: ['list', 'read', 'write', 'delete', 'transfer', 'fetch'] }],
        global_perms: ['transfer', 'fetch'],
        limits: { max_upload_bytes: 32 * 1024 ** 3, requests_per_minute: 300, concurrent_jobs: 4 },
        trash: { enabled: true, days: 3 },
      },
      {
        // legacy static key from config.yaml: prefix null (live behavior), read-only
        id: 'legacy-static', parent_id: null, prefix: null, static: true, revoked: false,
        created_at: hoursAgo(2000), expires_at: null, last_used_at: null,
        default_drive: 'baidu-main',
        drives: [{ drive: 'baidu-main', root: '/', perms: ['list', 'read', 'write', 'delete', 'transfer'] }],
        global_perms: ['transfer'],
        limits: { max_upload_bytes: null, requests_per_minute: null, concurrent_jobs: null },
        trash: { enabled: false, days: 7 },
      },
      {
        id: 'old-expired', parent_id: null, prefix: 'sk-c2a-oldexp03', static: false, revoked: false,
        created_at: hoursAgo(1500), expires_at: hoursAgo(48), last_used_at: hoursAgo(60),
        default_drive: null,
        drives: [{ drive: 'pan123-main', root: '/tmp', perms: ['list', 'read', 'write'] }],
        global_perms: [],
        limits: { max_upload_bytes: null, requests_per_minute: null, concurrent_jobs: null },
        trash: { enabled: false, days: 7 },
      },
      {
        id: 'removed-demo', parent_id: null, prefix: 'sk-c2a-rmdemo11', static: false, revoked: true,
        created_at: hoursAgo(800), expires_at: null, last_used_at: hoursAgo(300),
        default_drive: 'baidu-main',
        drives: [{ drive: 'baidu-main', root: '/demo', perms: ['list', 'read'] }],
        global_perms: [],
        limits: { max_upload_bytes: null, requests_per_minute: null, concurrent_jobs: null },
        trash: { enabled: false, days: 7 },
      },
    ];

    const requests = []; // agent approval flow removed: /v1/requests is gone

    const unmanaged = [
      { storage_id: 11, mount_path: '/movies', driver: 'Local', status: 'work' },
      { storage_id: 12, mount_path: '/docs', driver: 'S3', status: 'work' },
      { storage_id: 13, mount_path: '/old-baidu', driver: 'BaiduNetdisk', status: 'failed refresh token: invalid grant token' },
      // no ASCII slug in the mount: the adopt dialog suggests the connect
      // platform id (baidu) instead — try #/drives?connected=16
      { storage_id: 16, mount_path: '/百度网盘', driver: 'BaiduNetdisk', status: 'work' },
      // the mount belongs to media-old → adoptable only via rebind confirmation
      { storage_id: 15, mount_path: '/d/media', driver: 'Local', status: 'work', replacement_for: 'media-old' },
      // another GoAPI instance chained as API upstream storage: URL and
      // upstream API key were entered in the OpenList wizard (/#/new/goapi)
      { storage_id: 17, mount_path: '/goapi-upstream', driver: 'GoAPI', status: 'work' },
    ];

    const settings = {
      public_base_url: 'http://198.51.100.20:8080',
      openlist_public_url: 'http://198.51.100.20:5244',
      policy_text: '单文件上传上限 8 GiB；/.c2a-trash 回收站默认保留 7 天；其余路径为持久网盘存储——请勿删除非自己创建的文件。所有流量经网关主机转发。',
    };

    const auditOps = ['list', 'read', 'write', 'mkdir', 'delete', 'move', 'link', 'transfer'];
    const keyIds = ['histopilot', 'histopilot-sub', 'readonly-report', 'etl-loader'];
    const driveIds = ['baidu-main', 'pan123-main', 'pan115-open', 'aliyun-main'];
    const paths = [
      '/reports/q3-summary.pdf', '/reports/', '/agents/histopilot/notes.md', '/tmp/upload-8f3.bin',
      '/etl/dim_user.parquet', '/reports/2026-10/', '/agents/histopilot/', '/movies/preview.jpg', '/docs/manual.pdf',
    ];
    const entries = [];
    let t = 0.2;
    for (let i = 0; i < 46; i++) {
      const op = auditOps[i % auditOps.length];
      const status = i % 11 === 3 ? 404 : (i % 17 === 5 ? 429 : 200);
      const isDir = op === 'list' || op === 'mkdir';
      entries.push({
        ts: hoursAgo(t),
        key_id: keyIds[i % keyIds.length],
        drive: driveIds[i % driveIds.length],
        op: op,
        path: paths[i % paths.length],
        bytes: isDir ? 0 : (op === 'read' || op === 'write') ? (2 + (i % 9)) * 1024 * 1024 + i * 977 : 0,
        status: status,
        duration_ms: 40 + ((i * 137) % 1900),
        error: status === 404 ? 'path_not_found: /reports/q3-summary.pdf' : status === 429 ? 'rate_limited: try again after 1s' : '',
      });
      t += 0.4 + (i % 5) * 0.7;
    }

    /* linkage events. The portal no longer writes OpenList storage config, so
     * no new customized / settings_changed events are produced — the seeded
     * rows are history that stays in the DB and must still render. */
    const events = [
      { ts: hoursAgo(0.4), drive: 'quark-old', kind: 'deleted_in_openlist', detail: 'storage 9 (mount /d/quark) no longer exists in OpenList; drive is orphaned' },
      { ts: hoursAgo(0.6), drive: 'team-share', kind: 'disabled_in_openlist', detail: 'storage 10 was disabled in OpenList; /v1 returns 503 drive_unavailable' },
      { ts: hoursAgo(1.1), drive: 'pan123-main', kind: 'customized', detail: 'customized in OpenList: cache_expiration 30 -> 120; order_by name -> size' },
      { ts: hoursAgo(3), drive: 'pan115-open', kind: 'mount_renamed', detail: 'mount renamed in OpenList: /d/115 -> /d/pan115 (followed; drive id and key grants unchanged)' },
      { ts: hoursAgo(5), drive: 'nas-open', kind: 'disabled', detail: '网盘已在 cloud2api 停用：agent 无法再通过 cloud2api 访问（OpenList 中的存储不变，Key 授权保留）' },
      { ts: hoursAgo(14), drive: 'nas-open', kind: 'storage_rebound', detail: 'storage 18 is gone; rebound to storage 8 by mount path /nas' },
      { ts: hoursAgo(26), drive: 'pan123-main', kind: 'adopted', detail: 'adopted OpenList storage 5 (driver 123Pan, mount /d/pan123-main); OpenList configuration untouched' },
      { ts: hoursAgo(47), drive: 'g-manual-demo', kind: 'released', detail: 'drive released; OpenList storage 13 (mount /g-manual) kept untouched' },
      { ts: hoursAgo(96), drive: 'local-backup', kind: 'enabled_in_openlist', detail: 'storage 7 was re-enabled in OpenList' },
    ];

    return { drives, keys, requests, unmanaged, settings, audit: entries, events };
  }

  /* ---------------- backend ---------------- */

  function MockBackend() {
    this.db = seed();
    this.auditLimit = 20;
    /* demo switches via query params (?mock=1&noolurl=1&syncfail=1&badolurl=1):
     * noolurl  — no openlist_public_url configured → empty manage_url
     * badolurl — an invalid stored URL (already contains /@manage)
     * syncfail — OpenList unreachable: sync 502, drives served from cache
     * noconnect — OpenList without the login-wizard patch: connect_* empty,
     *             no reauth urls (native OpenList forms only)
     * connecterr — the /@connect status probe failed (connect_error set)
     * nohandoff  — an older patched module without the handoff capability:
     *              connect_handoff_available false → the portal renders plain
     *              manual links instead of the choice modal
     * handofffail — POST /connect/handoff answers 502 (mint failure) */
    this.flags = {};
    try {
      for (const [k, v] of new URLSearchParams(location.search)) this.flags[k] = v;
    } catch (e) { /* non-browser */ }
    if (this.flags.noolurl) this.db.settings.openlist_public_url = '';
    if (this.flags.badolurl) this.db.settings.openlist_public_url = 'http://198.51.100.20:5244/@manage';
    this.olReachable = !this.flags.syncfail;
  }

  MockBackend.prototype.err = function (status, message, body) {
    throw new (window.C2A.API.ApiError)(status, message, body);
  };

  MockBackend.prototype.findDrive = function (id) {
    return this.db.drives.find((d) => d.id === id) || null;
  };
  MockBackend.prototype.findKey = function (id) {
    return this.db.keys.find((k) => k.id === id) || null;
  };
  MockBackend.prototype.keyActive = function (k) {
    if (k.revoked) return false;
    if (k.expires_at && new Date(k.expires_at).getTime() < Date.now()) return false;
    return true;
  };
  MockBackend.prototype.childrenOf = function (id) {
    return this.db.keys.filter((k) => k.parent_id === id);
  };
  MockBackend.prototype.syncKeyCounts = function () {
    for (const d of this.db.drives) {
      d.key_count = this.db.keys.filter((k) => !k.revoked && (k.drives || []).some((g) => g.drive === d.id)).length;
    }
  };
  /* ---- openlist_public_url helpers (mirror the server-side contract) ---- */

  /** Validation result message or '' when acceptable. Valid: http/https
   * absolute URL, optional path prefix. Rejected: credentials, query,
   * fragment, a value already containing /@manage. */
  MockBackend.prototype.validatePublicUrl = function (v) {
    let u;
    try { u = new URL(v); } catch (e) { return '不是合法的绝对地址：需要 http:// 或 https:// 开头'; }
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return '只支持 http:// 或 https:// 地址';
    if (u.username || u.password) return '地址不能包含用户名或密码';
    if (u.search) return '地址不能包含查询参数（?…）';
    if (u.hash) return '地址不能包含锚点（#…）';
    if (String(u.pathname).indexOf('/@manage') >= 0 || v.indexOf('/@manage') >= 0) return '地址不需要带 /@manage，填写 OpenList 后台根地址即可';
    return '';
  };
  /** normalized public URL parts used by overview + drive rows */
  MockBackend.prototype.publicUrlParts = function () {
    const raw = String(this.db.settings.openlist_public_url || '').trim();
    if (!raw) return { public_url: '', manage_url: '', public_url_error: '' };
    const err = this.validatePublicUrl(raw);
    if (err) return { public_url: raw.replace(/\/+$/, ''), manage_url: '', public_url_error: err };
    const base = raw.replace(/\/+$/, '');
    return { public_url: base, manage_url: base + '/@manage', public_url_error: '' };
  };
  MockBackend.prototype.editUrlFor = function (drive) {
    const parts = this.publicUrlParts();
    if (!parts.manage_url || !drive.storage_id) return '';
    return parts.manage_url + '/storages/edit/' + drive.storage_id;
  };

  /* ---- login wizard (patched OpenList /@connect) helpers ----
   * Mirrors the live contract: overview.openlist carries connect_available /
   * connect_url / connect_version / connect_platforms / connect_error /
   * connect_handoff_available, and drive rows carry openlist_reauth_url when
   * the storage's driver has a login adapter. The platform list covers the
   * netdisk logins (BaiduNetdisk, AliyundriveOpen, 123Pan, Onedrive,
   * GoogleDrive) plus the API upstream connection: another GoAPI instance is
   * chained as storage via the wizard route /#/new/goapi, where its URL and
   * API key are entered — never in this portal. */
  /* verified mirrors the real-account test status (docs/REAL-ACCOUNT-TEST.md):
   * Baidu / Aliyun / 123 / OneDrive pass end-to-end; Google Drive is wired but
   * not yet verified with a real account. */
  const CONNECT_PLATFORMS = [
    { id: 'baidu', name: '百度网盘', driver: 'BaiduNetdisk', verified: true },
    { id: 'aliyun', name: '阿里云盘', driver: 'AliyundriveOpen', verified: true },
    { id: '123pan', name: '123云盘', driver: '123Pan', verified: true },
    { id: 'onedrive', name: 'OneDrive', driver: 'Onedrive', verified: true },
    { id: 'googledrive', name: 'Google Drive', driver: 'GoogleDrive', verified: false },
    { id: 'goapi', name: 'GoAPI 上游', driver: 'GoAPI', verified: true },
  ];
  const CONNECT_DRIVERS = CONNECT_PLATFORMS.map((p) => p.driver.toLowerCase());
  function driverHasConnect(driver) {
    return CONNECT_DRIVERS.indexOf(String(driver || '').toLowerCase()) >= 0;
  }
  MockBackend.prototype.connectInfo = function () {
    const parts = this.publicUrlParts();
    if (this.flags.noconnect || !parts.public_url) {
      return { available: false, handoff: false, url: '', version: '', platforms: [], error: '' };
    }
    const url = parts.public_url + '/@connect/';
    if (this.flags.connecterr) {
      // the wizard route exists but the status probe failed: the URL is still
      // derivable, version/platforms are unknown
      return { available: true, handoff: false, url: url, version: '', platforms: [], error: '读取 /@connect 状态失败：连接超时（演示）' };
    }
    /* connect_handoff_available mirrors the live contract: only a module with
     * the explicit handoff capability AND a valid public URL lets the portal
     * mint short-lived wizard URLs (?nohandoff=1 demos an older module). */
    return { available: true, handoff: !this.flags.nohandoff, url: url, version: 'v0.4.0', platforms: CONNECT_PLATFORMS, error: '' };
  };
  /** The public return landing the backend bakes into every minted wizard
   * URL: same-origin JS hop that re-enters the portal with the session
   * cookie (SameSite=Strict) and lands on the drives return flow. The wizard
   * replaces the {storage_id} placeholder textually. */
  MockBackend.prototype.connectReturnParam = function () {
    return location.origin + '/admin/connect-return?storage_id={storage_id}';
  };
  /** Drives rows as the live API returns them: openlist_edit_url only when the
   * storage is currently matched (never for orphaned / replacement_found),
   * openlist_reauth_url only when connect is available and the driver has a
   * login adapter; replacement as null or the candidate object. */
  MockBackend.prototype.driveRow = function (d) {
    const row = clone(d);
    const matched = !!d.storage_id && d.state !== 'orphaned' && d.state !== 'replacement_found';
    row.openlist_edit_url = matched ? this.editUrlFor(d) : '';
    const conn = this.connectInfo();
    row.openlist_reauth_url = matched && conn.available && conn.url && driverHasConnect(d.driver)
      ? conn.url + '#/reauth/' + d.storage_id
      : '';
    if (!row.replacement) row.replacement = null;
    return row;
  };
  MockBackend.prototype.drivesPayload = function () {
    this.syncKeyCounts();
    const out = { drives: this.db.drives.map((d) => this.driveRow(d)) };
    if (!this.olReachable) {
      out.snapshot_error = 'OpenList 不可达（演示模式）：以下为最近一次成功读取的缓存数据';
    }
    return out;
  };
  /* basic onboarding prompt — mirrors internal/admin/keys.go onboardingPrompt */
  MockBackend.prototype.onboardingPrompt = function (key, plaintext) {
    const base = String(this.db.settings.public_base_url || location.origin).replace(/\/+$/, '');
    const drv = (key.drives || []).map((g) => {
      let mode = 'read-only';
      if (g.perms.includes('write')) mode = 'read/write';
      else if (g.perms.includes('list') && g.perms.length > 1) mode = 'read';
      else if (g.perms.includes('list')) mode = 'list-only';
      return g.drive + ' (' + mode + (g.root && g.root !== '/' ? ', root ' + g.root : '') + ')';
    }).join(', ');
    const policy = String(this.db.settings.policy_text || '').replace(/\s*\n\s*/g, ' ').trim()
      || 'max upload 8 GiB per file; /.c2a-trash trash retention 7 days by default; other paths are durable netdisk storage — do not delete files you did not create. Traffic passes through the gateway host.';
    return [
      'The cloud2api netdisk gateway is already deployed. Connect this agent/project using its basic file API and only the operations this key permits.',
      '',
      'Base URL: ' + base,
      'API key: ' + plaintext,
      'Authentication: apikey HTTP header',
      '',
      'First call GET /v1/me to inspect the effective drives, roots and permissions; do not write a test file merely to connect.',
      'Use explicit drive IDs for file operations. A listed operation is not a grant; do not attempt deletion without delete permission.',
      '',
      'Fetch and follow the basic integration instructions:',
      base + '/custom/agent-instructions.txt',
      '',
      'Basic OpenAPI:',
      base + '/apidocumentation/openapi.json',
      '',
      'Drives: ' + drv,
      'Policy: ' + policy,
      '',
      'Download the helper ' + base + '/custom/c2a.py and the skill ' + base + '/custom/SKILL.md if your environment supports them.',
    ].join('\n');
  };

  MockBackend.prototype.handle = async function (method, pathWithQuery, body) {
    await sleep(120 + Math.round(Math.random() * 160));
    const qIdx = pathWithQuery.indexOf('?');
    const path = qIdx >= 0 ? pathWithQuery.slice(0, qIdx) : pathWithQuery;
    const query = new URLSearchParams(qIdx >= 0 ? pathWithQuery.slice(qIdx + 1) : '');
    const seg = path.split('/').filter(Boolean); // e.g. ['drives','baidu-main','settings']

    // ----- auth -----
    if (path === '/login' && method === 'POST') return null;
    if (path === '/logout' && method === 'POST') return null;

    // ----- overview -----
    if (path === '/overview' && method === 'GET') {
      const warnings = [];
      const conn = this.connectInfo();
      const aliyun = this.findDrive('aliyun-main');
      if (aliyun) {
        warnings.push({
          level: 'error',
          text: conn.available && conn.url
            ? '网盘 aliyun-main 需要更新登录凭据：在网盘页点「重新授权」，完成后再「刷新并纳管」'
            : '网盘 aliyun-main 需要更新登录凭据：请在 OpenList 中更新后「刷新并纳管」',
          action_url: '#/drives',
        });
      }
      const p115 = this.findDrive('pan115-open');
      if (p115) warnings.push({ level: 'error', text: '网盘 pan115-open 上游错误：failed get fs: upstream timeout', action_url: '#/drives' });
      const orphans = this.db.drives.filter((d) => d.state === 'orphaned');
      for (const d of orphans) warnings.push({ level: 'warning', text: '网盘 ' + d.id + ' 的 OpenList 存储已被删除（orphaned）', action_url: '#/drives' });
      const repls = this.db.drives.filter((d) => d.state === 'replacement_found');
      for (const d of repls) warnings.push({ level: 'error', text: '网盘 ' + d.id + ' 的存储被替换，等待管理员确认替代存储', action_url: '#/drives' });
      const parts = this.publicUrlParts();
      if (parts.public_url_error) {
        warnings.push({ level: 'warning', text: 'OpenList 公开访问地址无效：' + parts.public_url_error, action_url: '#/settings' });
      } else if (parts.public_url) {
        warnings.push({ level: 'info', text: 'OpenList 已发布到局域网（' + parts.public_url.replace(/^https?:\/\//, '') + '），已确认游客用户处于关闭状态', action_url: '#/openlist' });
      }
      return {
        openlist: {
          version: this.olReachable ? 'v4.2.6 (Commit: 2bdf16d) - Frontend: v4.2.6' : '',
          reachable: this.olReachable,
          token_ok: this.olReachable ? true : null,
          guest_enabled: this.olReachable ? false : undefined,
          tested_version: 'v4.2.6',
          public_url: parts.public_url,
          manage_url: parts.manage_url,
          public_url_error: parts.public_url_error,
          connect_available: conn.available,
          connect_url: conn.url,
          connect_version: conn.version,
          connect_platforms: clone(conn.platforms),
          connect_handoff_available: conn.handoff === true,
          connect_error: conn.error,
        },
        counts: {
          drives: this.db.drives.length,
          keys: this.db.keys.filter((k) => this.keyActive(k)).length,
        },
        warnings: warnings,
      };
    }

    // ----- drives -----
    if (path === '/drives' && method === 'GET') {
      return this.drivesPayload();
    }
    if (path === '/drives/sync' && method === 'POST') {
      // one snapshot for both lists; failure changes nothing
      if (!this.olReachable) this.err(502, 'OpenList 不可达');
      return {
        drives: this.db.drives.map((d) => this.driveRow(d)),
        unmanaged: clone(this.db.unmanaged),
        synced_at: now(),
      };
    }
    /* POST /drives (create a drive with credentials) was removed in step 2 —
     * drives are added in OpenList now. Falls through to the 404 below. */

    // ----- connect handoff: portal → OpenList login wizard -----
    // Mirrors the live contract: POST /connect/handoff {target} answers
    // {url, fallback_url, expires_in}. The minted url is
    //   /@connect/?return=<landing>#handoff=<43-char base64url>&target=<enc>
    // — the ticket and the wizard route ride in the FRAGMENT (a query
    // credential would leak into logs/history/Referer); only the public
    // landing return is in the query. An old module still answers 200 but
    // with an EMPTY url and the manual link; a failed mint is a 502 with the
    // fallback in the envelope. The portal never sends or stores OpenList
    // credentials.
    if (path === '/connect/handoff' && method === 'POST') {
      const conn = this.connectInfo();
      const target = String((body && body.target) || '/');
      if (!/^(\/|\/new\/goapi|\/reauth\/\d+)$/.test(target)) this.err(400, '不支持的向导目标: ' + target);
      if (this.flags.handofffail) {
        this.err(502, '自动登录签发失败（演示）', { data: { fallback_url: conn.url + '#' + target } });
      }
      if (!conn.available || !conn.handoff) {
        return { url: '', fallback_url: conn.url + '#' + target, expires_in: 0 };
      }
      const ticket = 'A'.repeat(43); // demo: deterministic low-entropy base64url ticket
      return {
        url: conn.url
          + '?return=' + encodeURIComponent(this.connectReturnParam())
          + '#handoff=' + ticket + '&target=' + encodeURIComponent(target),
        fallback_url: conn.url + '#' + target,
        expires_in: 60,
      };
    }
    if (path === '/drives/unmanaged' && method === 'GET') {
      return { storages: clone(this.db.unmanaged) };
    }
    if (path === '/drives/adopt' && method === 'POST') {
      const sid = Number(body.storage_id);
      const id = String(body.id || '').trim();
      const st = this.db.unmanaged.find((s) => s.storage_id === sid);
      if (!st) this.err(404, '未找到该 OpenList 存储（可能已被删除或纳管）；请刷新后重试');
      if (st.replacement_for) this.err(409, '存储 #' + sid + ' 的挂载点属于网盘 ' + st.replacement_for + '；请在该网盘上通过「确认使用替代存储」完成重绑');
      if (!id) this.err(400, 'id 不能为空');
      if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(id)) this.err(400, 'id 只能包含小写字母、数字和中划线');
      if (this.findDrive(id)) this.err(409, '网盘 ' + id + ' 已存在');
      const drive = {
        id: id, provider: st.driver.toLowerCase().replace(/\s+/g, ''), driver: st.driver,
        mount_path: st.mount_path, storage_id: st.storage_id,
        state: st.disabled ? 'disabled' : (st.status === 'work' ? 'healthy' : 'error'), status_text: st.status,
        enabled: true, storage_disabled: !!st.disabled, created_at: now(), static: false,
      };
      this.db.drives.push(drive);
      this.db.unmanaged = this.db.unmanaged.filter((s) => s.storage_id !== sid);
      this.db.events.unshift({ ts: now(), drive: id, kind: 'adopted', detail: 'adopted OpenList storage ' + st.storage_id + ' (driver ' + st.driver + ', mount ' + st.mount_path + '); OpenList configuration untouched' });
      return this.driveRow(drive); // the row carries the storage's REAL state
    }
    if (seg[0] === 'drives' && seg.length === 3 && seg[2] === 'rebind' && method === 'POST') {
      const drive = this.findDrive(seg[1]);
      if (!drive) this.err(404, 'drive not found');
      if (drive.state !== 'replacement_found') this.err(409, '网盘 ' + drive.id + ' 当前没有待确认的替代存储；请先「刷新并纳管」');
      const sid = Number(body && body.storage_id);
      const idx = this.db.unmanaged.findIndex((s) => s.storage_id === sid);
      if (idx < 0) this.err(404, '候选存储 #' + sid + ' 不存在或已被纳管；请刷新后重试');
      const cand = this.db.unmanaged[idx];
      if (cand.mount_path !== drive.mount_path) this.err(409, '存储 #' + sid + ' 已不在挂载点 ' + drive.mount_path + ' 上；请刷新后重试');
      if (cand.replacement_for && cand.replacement_for !== drive.id) this.err(409, '存储 #' + sid + ' 已被其他网盘占用；请刷新后重试');
      drive.storage_id = cand.storage_id;
      drive.driver = cand.driver;
      drive.state = cand.disabled ? 'disabled' : (cand.status === 'work' ? 'healthy' : 'error');
      drive.status_text = cand.status;
      drive.storage_disabled = !!cand.disabled;
      drive.replacement = null;
      this.db.unmanaged.splice(idx, 1);
      this.db.events.unshift({ ts: now(), drive: drive.id, kind: 'storage_rebound', detail: '管理员确认替代存储：重绑到 storage ' + cand.storage_id + ' (driver ' + cand.driver + ', mount ' + cand.mount_path + ')；已有 Key 授权对新存储生效' });
      return this.driveRow(drive);
    }
    /* GET|PUT /drives/{id}/settings (portal 常用设置) were removed in step 2 —
     * storage config is edited in OpenList. Falls through to the 404 below. */
    if (seg[0] === 'drives' && seg.length === 3 && (seg[2] === 'enable' || seg[2] === 'disable') && method === 'POST') {
      // cloud2api's own switch only: never calls OpenList, key grants kept
      const drive = this.findDrive(seg[1]);
      if (!drive) this.err(404, 'drive not found');
      if (drive.static) this.err(409, 'static drives (config.yaml) cannot be enabled or disabled via the portal');
      if (drive.state === 'orphaned') this.err(409, 'storage has been deleted in OpenList; rebuild it there and 刷新并纳管 first, or release the mapping');
      drive.enabled = seg[2] === 'enable';
      drive.state = !drive.enabled || drive.storage_disabled ? 'disabled' : 'healthy';
      drive.status_text = drive.state === 'disabled' ? 'disabled' : 'work';
      this.db.events.unshift({
        ts: now(), drive: drive.id, kind: drive.enabled ? 'enabled' : 'disabled',
        detail: drive.enabled
          ? '网盘已在 cloud2api 启用：agent 恢复通过 cloud2api 访问（OpenList 中的存储不变，Key 授权保留）'
          : '网盘已在 cloud2api 停用：agent 无法再通过 cloud2api 访问（OpenList 中的存储不变，Key 授权保留）',
      });
      return this.driveRow(drive);
    }
    if (seg[0] === 'drives' && seg.length === 2 && method === 'DELETE') {
      // always mapping-only: the OpenList storage is kept, reappears as
      // unmanaged and can be adopted again (keep_storage is ignored)
      const drive = this.findDrive(seg[1]);
      if (!drive) this.err(404, 'drive not found');
      if (drive.static) this.err(409, 'static drives (config.yaml) cannot be deleted via the portal');
      if (drive.storage_id && drive.state !== 'orphaned' && !this.db.unmanaged.some((s) => s.storage_id === drive.storage_id)) {
        this.db.unmanaged.push({ storage_id: drive.storage_id, mount_path: drive.mount_path, driver: drive.driver, status: 'work' });
      }
      this.db.events.unshift({ ts: now(), drive: drive.id, kind: 'released', detail: 'drive released; OpenList storage ' + drive.storage_id + ' (mount ' + drive.mount_path + ') kept untouched, adoptable again' });
      this.db.drives = this.db.drives.filter((x) => x.id !== seg[1]);
      return null;
    }

    /* GET /providers and /login-flows/** were removed in step 2 — login and
     * storage configuration happen in OpenList's own admin UI. They fall
     * through to the 404 below. */

    // ----- keys -----
    if (path === '/keys' && method === 'GET') {
      return { keys: clone(this.db.keys) };
    }
    if (path === '/keys' && method === 'POST') {
      let id = String(body.id || '').trim();
      if (!id) id = 'key-' + rng(4); // live: omitted id auto-generates key-xxxx
      if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(id)) this.err(400, 'Key ID 只能包含字母、数字、中划线和下划线');
      if (this.findKey(id)) this.err(409, 'Key ' + id + ' 已存在');
      if (!body.drives || !body.drives.length) this.err(400, '至少需要授权一个网盘');
      const key = this.normalizeKey(body, id, null);
      this.db.keys.push(key);
      return this.keyCreatedResult(key);
    }
    if (seg[0] === 'keys' && seg.length === 2 && method === 'PATCH') {
      const key = this.findKey(seg[1]);
      if (!key) this.err(404, 'Key 不存在');
      if (key.static) this.err(409, '静态 Key（config.yaml）为只读，不能在门户中修改');
      if (key.revoked) this.err(409, 'Key 已吊销，不能修改');
      const editable = clone(key);
      this.applyWritable(key, body);
      return clone(key);
    }
    if (seg[0] === 'keys' && seg.length === 3 && seg[2] === 'rotate' && method === 'POST') {
      const key = this.findKey(seg[1]);
      if (!key) this.err(404, 'Key 不存在');
      if (key.static) this.err(409, '静态 Key（config.yaml）不能在门户中轮换');
      const plaintext = genPlaintext(key.prefix);
      key.revoked = false;
      return { key: clone(key), plaintext: plaintext, onboarding_prompt: this.onboardingPrompt(key, plaintext) };
    }
    if (seg[0] === 'keys' && seg.length === 2 && method === 'DELETE') {
      const key = this.findKey(seg[1]);
      if (!key) this.err(404, 'Key 不存在');
      if (key.static) this.err(409, '静态 Key（config.yaml）不能在门户吊销');
      if (key.revoked) {
        // live 1b behavior: DELETE on an already-revoked key hard-deletes the row
        this.db.keys = this.db.keys.filter((x) => x.id !== seg[1]);
        return null;
      }
      key.revoked = true;
      for (const c of this.childrenOf(seg[1])) c.revoked = true;
      return null;
    }

    // ----- requests: removed. /v1/requests and the admin approval endpoints
    // no longer exist; the portal has no requests page and the mock answers
    // 404 like the live backend. -----

    // ----- audit / events -----
    if (path === '/audit' && method === 'GET') {
      const fKey = (query.get('key') || '').trim();
      const fDrive = (query.get('drive') || '').trim();
      const fOp = (query.get('op') || '').trim();
      const fStatus = (query.get('status') || '').trim();
      const before = query.get('before');
      const limit = Math.min(Number(query.get('limit')) || this.auditLimit, 100);
      let list = this.db.audit;
      if (before) {
        const idx = list.findIndex((e) => e.ts === before);
        if (idx >= 0) list = list.slice(idx + 1);
      }
      if (fKey) list = list.filter((e) => e.key_id.includes(fKey));
      if (fDrive) list = list.filter((e) => e.drive.includes(fDrive));
      if (fOp) list = list.filter((e) => e.op === fOp);
      if (fStatus) list = list.filter((e) => String(e.status) === fStatus);
      const page = list.slice(0, limit);
      const next = list.length > limit ? page[page.length - 1].ts : null;
      return { entries: clone(page), next_before: next };
    }
    if (path === '/events' && method === 'GET') {
      const limit = Math.min(Number(query.get('limit')) || 50, 200);
      return { events: clone(this.db.events.slice(0, limit)) };
    }

    // ----- settings -----
    if (path === '/settings' && method === 'GET') {
      return clone(this.db.settings);
    }
    if (path === '/settings' && method === 'PUT') {
      // oplist_api_base is gone: the online token service is configured in
      // OpenList, and a PUT that still sends it is rejected
      if (typeof body.oplist_api_base === 'string') {
        this.err(400, 'oplist_api_base 已废弃：网盘登录与令牌服务在 OpenList 后台配置，cloud2api 不再保存该地址');
      }
      // openlist_public_url is validated before anything is saved (400 → nothing persisted)
      if (typeof body.openlist_public_url === 'string') {
        const v = body.openlist_public_url.trim();
        if (v) {
          const perr = this.validatePublicUrl(v);
          if (perr) this.err(400, perr);
        }
      }
      for (const k of ['public_base_url', 'openlist_public_url', 'policy_text']) {
        if (typeof body[k] === 'string') this.db.settings[k] = body[k].trim();
      }
      // openlist_public_url is stored without a trailing slash
      if (typeof body.openlist_public_url === 'string') {
        this.db.settings.openlist_public_url = body.openlist_public_url.trim().replace(/\/+$/, '');
      }
      return clone(this.db.settings);
    }

    this.err(404, '接口不存在（mock）: ' + method + ' ' + path);
  };

  MockBackend.prototype.normalizeKey = function (body, id, parentId) {
    const key = {
      id: id, parent_id: parentId, prefix: genPrefix(), static: false, revoked: false,
      created_at: now(), expires_at: body.expires_at || null, last_used_at: null,
      default_drive: body.default_drive || null,
      drives: (body.drives || []).map((g) => ({
        drive: g.drive, root: g.root || '/',
        perms: Array.from(new Set((g.perms || []).filter((p) => PERM_VOCAB.includes(p)))),
      })),
      global_perms: Array.from(new Set((body.global_perms || []).filter((p) => PERM_VOCAB.includes(p)))),
      limits: {
        max_upload_bytes: (body.limits && body.limits.max_upload_bytes) || null,
        requests_per_minute: (body.limits && body.limits.requests_per_minute) || null,
        concurrent_jobs: (body.limits && body.limits.concurrent_jobs) || null,
      },
      trash: body.trash || { enabled: false, days: 7 },
      relay: this.normalizeRelay(body.relay, key0Drives(body)),
    };
    return key;
  };
  function key0Drives(body) { return (body && body.drives) || []; }
  /** relay zero values = documented defaults; null/absent resets to defaults. */
  MockBackend.prototype.normalizeRelay = function (relay, drives) {
    const r = relay || {};
    if (!relay || (!r.drive && !r.quota_bytes && !r.max_age_days)) return { drive: null, quota_bytes: 0, max_age_days: 0 };
    const granted = (drives || []).map((g) => g.drive);
    if (r.drive && !granted.includes(r.drive)) this.err(400, 'relay drive 未在该 Key 的授权列表中');
    if (r.quota_bytes < 0) this.err(400, 'relay quota 不能为负数');
    if (r.max_age_days < 0 || r.max_age_days > 7) this.err(400, 'relay max_age 只能在 0–7 之间');
    return { drive: r.drive || null, quota_bytes: Math.max(0, Number(r.quota_bytes) || 0), max_age_days: Math.max(0, Math.min(7, Number(r.max_age_days) || 0)) };
  };
  MockBackend.prototype.applyWritable = function (key, body) {
    if (body.default_drive !== undefined) key.default_drive = body.default_drive || null;
    if (body.expires_at !== undefined) key.expires_at = body.expires_at || null;
    if (body.drives !== undefined) {
      key.drives = (body.drives || []).map((g) => ({
        drive: g.drive, root: g.root || '/',
        perms: Array.from(new Set((g.perms || []).filter((p) => PERM_VOCAB.includes(p)))),
      }));
    }
    if (body.global_perms !== undefined) {
      key.global_perms = Array.from(new Set((body.global_perms || []).filter((p) => PERM_VOCAB.includes(p))));
    }
    if (body.limits !== undefined) {
      key.limits = {
        max_upload_bytes: (body.limits && body.limits.max_upload_bytes) || null,
        requests_per_minute: (body.limits && body.limits.requests_per_minute) || null,
        concurrent_jobs: (body.limits && body.limits.concurrent_jobs) || null,
      };
    }
    if (body.trash !== undefined) key.trash = body.trash;
    if (body.relay !== undefined) key.relay = this.normalizeRelay(body.relay, key.drives);
  };
  MockBackend.prototype.keyCreatedResult = function (key) {
    const plaintext = genPlaintext(key.prefix);
    return { key: clone(key), plaintext: plaintext, onboarding_prompt: this.onboardingPrompt(key, plaintext) };
  };

  window.C2AMockBackend = MockBackend;
})();
