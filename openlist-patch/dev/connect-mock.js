/* connect 页面开发用 Mock API（不参与生产构建）。
 * 由 app.js 在 ?mock=1 时动态加载，提供 window.CONNECT_MOCK.handle(method, path, body)，
 * 返回 Promise<{code, message, data}>，形状与 OpenList 响应封装一致。
 *
 * 特性（可用页面 URL 参数控制）：
 *   ?mock=1            启用 mock
 *   &qr=expire         扫码会话 5 秒后二维码过期（可刷新两次）
 *   &qr=neterr         扫码会话先出现几次网络错误，随后恢复等待扫码
 * 登录：任意用户名/密码均可登录；密码为 wrong 时登录失败；
 *       用户名为 otp 时需要两步验证码 123456（首次不带码返回 402）。
 * 应用：初始没有任何已保存配置——百度默认公共应用（扫码）、阿里云默认在线
 *       服务（扫码）、OneDrive/Google 默认在线服务（粘贴令牌 + 授权指引）、
 *       123 无需应用（账号密码）；保存/删除配置后走对应的已保存/默认分支。
 * 表单：粘贴令牌填 fail 模拟网盘拒绝；账号密码用户名 fail 模拟校验失败。
 * 保存：挂载路径 /taken → 可重试；/fail → 初始化失败且清理成功；
 *       /fail-cleanup → 初始化失败且清理失败；其余成功。
 * 重新授权：目标 12 成功；15 失败但已恢复；16 失败且未恢复。
 * 挂载建议：/百度网盘 已被占用 → 返回 /百度网盘-2；保存成功后占用新路径。 */
(function () {
  'use strict';

  var HASH_WRONG = 'faca0d94bb5e970580b7870e21011762fdaee1e2110a47d03e4d268ccf280295';
  var REDIRECT = 'http://127.0.0.1:8098/api/connect/oauth/callback';
  var SERVICE = 'https://api.oplist.org';
  var QR_MODE = new URLSearchParams(location.search).get('qr') || '';

  var now = function () { return new Date().toISOString(); };
  var seq = 0;

  // ---------- 平台静态数据 ----------
  var moduleInfo = {
    name: 'connect', version: '4', handoff: true,
    upstream_version: 'v4.2.6', upstream_commit: '2bdf16d', web_version: 'v4.2.6'
  };

  // 与 pkg/loginsdk/adapters 中各适配器的 Meta 保持一致。
  var platforms = [
    {
      id: 'baidu', name: '百度网盘', driver: 'BaiduNetdisk', method: 'qrcode',
      app_modes: ['public_app', 'online_service', 'own_app'],
      mode_methods: { online_service: 'form', own_app: 'oauth' },
      public_app_label: 'api.oplist.org「百度网盘 - 手动登录」使用的公开应用',
      form_label: '粘贴令牌',
      scopes: ['basic', 'netdisk'], pkce: false, allow_oob: true,
      form_fields: [
        {
          key: 'refresh_token', label: '刷新令牌（Refresh Token）',
          type: 'password', required: true, secret: true,
          help: '粘贴 OpenList 授权页显示的刷新令牌'
        }
      ],
      root_field: 'root_folder_path', root_default: '/',
      root_label: '根目录路径', root_help: '网盘中的路径，例如 /我的资源；保持 / 使用整个网盘',
      reauth_fields: ['refresh_token', 'AccessToken', 'use_online_api', 'api_url_address', 'client_id', 'client_secret'],
      notes: [
        '默认使用公共应用扫码登录（api.oplist.org「百度网盘 - 手动登录」），驱动直接向百度续期，无需配置开发者应用',
        '也可改用在线授权服务：在授权页获取刷新令牌后粘贴；或配置自有应用（AppKey/SecretKey）浏览器授权',
        '自有应用的回调地址需要在应用中登记；无法登记时可把回调地址设为 oob，授权后粘贴页面显示的授权码'
      ],
      verified: '', docs_url: 'https://doc.oplist.org/guide/drivers/baidu'
    },
    {
      id: 'aliyun', name: '阿里云盘', driver: 'AliyundriveOpen', method: 'qrcode',
      app_modes: ['online_service', 'own_app'], scopes: ['user:base', 'file:all:read', 'file:all:write'],
      pkce: false, allow_oob: false,
      root_field: 'root_folder_id', root_default: 'root',
      root_label: '根目录 ID', root_help: '阿里云盘文件夹 ID；保持 root 使用整个网盘',
      reauth_fields: ['refresh_token', 'AccessToken', 'use_online_api', 'api_url_address', 'alipan_type', 'client_id', 'client_secret'],
      notes: [
        '使用阿里云盘开放平台驱动（AliyundriveOpen），只接受与所选应用匹配的凭据',
        '默认走在线授权服务（默认 https://api.oplist.org）扫码，无需配置开发者应用；也可以配置自有应用',
        '默认挂载资源库（drive_type=resource），需要备份盘时在 OpenList 原生表单中修改'
      ],
      verified: '2026-10-14 在线服务扫码', docs_url: 'https://doc.oplist.org/guide/drivers/aliyundrive_open'
    },
    {
      id: '123pan', name: '123云盘', driver: '123Pan', method: 'form',
      app_modes: ['none'], pkce: false, allow_oob: false,
      form_fields: [
        { key: 'username', label: '手机号或邮箱', type: 'text', required: true, secret: false },
        { key: 'password', label: '密码', type: 'password', required: true, secret: true }
      ],
      root_field: 'root_folder_id', root_default: '0',
      root_label: '根目录 ID', root_help: '123 云盘文件夹 ID；保持 0 使用整个网盘',
      reauth_fields: ['username', 'password', 'AccessToken'],
      notes: [
        '使用普通账号密码登录（123Pan 驱动），不需要开发者应用；与「123 Open」开放平台驱动是不同的适配，两者的凭据不能混用',
        '账号密码在保存时由驱动登录验证；上游说明该驱动不再主动维护，登录可能触发风控',
        '首版只验证账号密码路径'
      ],
      verified: '2026-10-14 账号密码', docs_url: 'https://doc.oplist.org/guide/drivers/123'
    },
    {
      id: 'onedrive', name: 'OneDrive', driver: 'Onedrive', method: 'form',
      app_modes: ['online_service', 'own_app'],
      mode_methods: { own_app: 'oauth' },
      form_label: '粘贴令牌',
      scopes: ['offline_access', 'Files.ReadWrite.All'],
      pkce: true, allow_oob: false,
      form_fields: [
        {
          key: 'refresh_token', label: '刷新令牌（Refresh Token）',
          type: 'password', required: true, secret: true,
          help: '粘贴 OpenList 授权页显示的刷新令牌'
        }
      ],
      root_field: 'root_folder_path', root_default: '/',
      root_label: '根目录路径', root_help: 'OneDrive 中的路径；保持 / 使用整个 OneDrive',
      reauth_fields: ['refresh_token', 'use_online_api', 'api_url_address', 'client_id', 'client_secret', 'redirect_uri'],
      notes: [
        '默认使用 OpenList 在线授权服务：打开授权页获取刷新令牌后粘贴，个人账号和工作/学校账号都选「OneDrive - 个人版本 (OAuth App)」',
        '也可以在 Microsoft Entra 注册自有应用（支持任意组织目录与个人 Microsoft 账户，平台选 Web），重定向 URI 填写本页显示的回调地址，非 localhost 地址需要 HTTPS',
        '首版覆盖 global 区域；其他区域和 SharePoint 使用 OpenList 原生高级配置；工作/学校账号可能需要组织管理员同意'
      ],
      verified: '', docs_url: 'https://doc.oplist.org/guide/drivers/onedrive'
    },
    {
      id: 'googledrive', name: 'Google Drive', driver: 'GoogleDrive', method: 'form',
      app_modes: ['online_service', 'own_app'],
      mode_methods: { own_app: 'oauth' },
      form_label: '粘贴令牌',
      scopes: ['https://www.googleapis.com/auth/drive'], pkce: true, allow_oob: false,
      form_fields: [
        {
          key: 'refresh_token', label: '刷新令牌（Refresh Token）',
          type: 'password', required: true, secret: true,
          help: '粘贴 OpenList 授权页显示的刷新令牌'
        }
      ],
      root_field: 'root_folder_id', root_default: 'root',
      root_label: '根目录 ID', root_help: 'Google Drive 文件夹 ID；保持 root 使用「我的云端硬盘」',
      reauth_fields: ['refresh_token', 'use_online_api', 'api_url_address', 'client_id', 'client_secret'],
      notes: [
        '默认使用 OpenList 在线授权服务：打开授权页选择「Google Drive OAuth2 (OAuth App)」获取刷新令牌后粘贴',
        '首版只接入「我的云端硬盘」，共享云端硬盘尚未验证',
        'OAuth 同意屏幕处于「测试」状态时 refresh token 7 天后失效，长期使用需发布为正式版本；自有应用的已获授权重定向 URI 需登记本页显示的回调地址'
      ],
      verified: '', docs_url: 'https://doc.oplist.org/guide/drivers/google_drive'
    }
  ];

  // 与服务端一致：只声明 none 的平台不需要应用配置。
  function needsApp(p) {
    return !!(p.app_modes && p.app_modes.length && !(p.app_modes.length === 1 && p.app_modes[0] === 'none'));
  }

  // ---------- 应用配置（内存） ----------
  // 初始为空：各平台先用默认应用（app.default=true），保存后才有已存配置。
  var apps = {};

  // 适配器内置的公共应用（百度）：凭据随适配器发布，页面只见掩码 ID。
  var PUBLIC_APP = {
    baidu: { client_id: 'NqOMXF6XGhGRIGemsQ9nG0Na' }
  };

  function maskID(id) {
    if (!id) return '';
    if (id.length <= 8) return id;
    return id.slice(0, 4) + '…' + id.slice(-4);
  }

  // DefaultApp：平台首模式无需输入时的开箱即用应用；own_app 无默认。
  function defaultAppOf(p) {
    var m0 = p.app_modes && p.app_modes[0];
    if (m0 === 'public_app') {
      var pub = PUBLIC_APP[p.id] || {};
      return { mode: 'public_app', identity: maskID(pub.client_id), service_url: '' };
    }
    if (m0 === 'online_service') {
      return { mode: 'online_service', identity: SERVICE, service_url: SERVICE };
    }
    return null;
  }

  function appIdentity(p, a) {
    if (a.mode === 'own_app') return maskID(a.client_id);
    if (a.mode === 'online_service') return a.service_url;
    if (a.mode === 'public_app') return maskID((PUBLIC_APP[p.id] || {}).client_id);
    return '';
  }

  function publicApp(p) {
    if (!needsApp(p)) {
      return {
        required: false, configured: true, mode: 'none', identity: '', default: false,
        client_id: '', client_secret_set: false, redirect_uri: '',
        service_url: '', revision: 0, updated_at: '', problem: ''
      };
    }
    var a = apps[p.id];
    if (a) {
      return {
        required: true, configured: !a.problem, mode: a.mode,
        identity: appIdentity(p, a), default: false,
        // 公共应用的凭据来自适配器，不回显 client_id。
        client_id: a.mode === 'public_app' ? '' : (a.client_id || ''),
        client_secret_set: !!a.client_secret,
        redirect_uri: a.redirect_uri || '',
        service_url: a.service_url || '',
        revision: a.revision || 0,
        updated_at: a.updated_at || '',
        problem: a.problem || ''
      };
    }
    var def = defaultAppOf(p);
    if (def) {
      // 未保存但平台有默认：configured=true 且 default=true。
      return {
        required: true, configured: true, mode: def.mode,
        identity: def.identity, default: true,
        client_id: '', client_secret_set: false, redirect_uri: '',
        service_url: def.service_url || '',
        revision: 0, updated_at: '', problem: ''
      };
    }
    return {
      required: true, configured: false, mode: p.app_modes[0], identity: '', default: false,
      client_id: '', client_secret_set: false, redirect_uri: '',
      service_url: '', revision: 0, updated_at: '', problem: ''
    };
  }

  function checkApp(p, body) {
    if (!needsApp(p)) return '该平台不需要开发者应用';
    if (p.app_modes.indexOf(body.mode) < 0) return '该平台不支持此应用模式';
    if (body.mode === 'public_app') {
      return ''; // 公共应用的凭据来自适配器，无需任何输入
    } else if (body.mode === 'own_app') {
      if (!body.client_id) return '请填写 client_id';
      if (!body.client_secret && !(apps[p.id] && apps[p.id].client_secret)) return '请填写 client_secret';
      if (!body.redirect_uri) return '请填写回调地址';
      if (body.redirect_uri !== 'oob' && !/^https?:\/\/[^\s]+$/.test(body.redirect_uri)) {
        return '回调地址必须是完整的 http(s) 地址或 oob';
      }
    } else if (body.mode === 'online_service') {
      if (!body.service_url) return '请填写在线授权服务地址';
      if (!/^https?:\/\/[^\s]+$/.test(body.service_url)) return '在线授权服务地址必须是完整的 http(s) 地址';
    }
    return '';
  }

  // 当前生效的登录方式：有生效应用（已保存或默认）时按模式解析，否则用 Meta。
  function activeMethodOf(p) {
    var app = publicApp(p);
    if (app.required && app.configured) {
      return ((p.mode_methods || {})[app.mode]) || p.method;
    }
    return p.method;
  }

  // ---------- 已占用的挂载路径（挂载建议用） ----------
  var takenMounts = { '/百度网盘': true };

  // ---------- 重新授权目标 ----------
  var targets = {
    12: {
      storage_id: 12, mount_path: '/baidu', driver: 'BaiduNetdisk', status: 'work',
      disabled: false, modified: '2026-10-14T08:00:00Z',
      platforms: [{ id: 'baidu', name: '百度网盘', problem: '' }],
      edit_path: '/@manage/storages/edit/12'
    },
    13: {
      storage_id: 13, mount_path: '/onedrive', driver: 'Onedrive', status: 'work',
      disabled: false, modified: '2026-10-12T09:00:00Z',
      platforms: [{ id: 'onedrive', name: 'OneDrive', problem: '首版只支持重新授权 global 区域的个人/企业 OneDrive；其他区域和 SharePoint 请在 OpenList 原生表单中更新' }],
      edit_path: '/@manage/storages/edit/13'
    },
    14: {
      storage_id: 14, mount_path: '/local', driver: 'Local', status: 'work',
      disabled: false, modified: '2026-10-01T09:00:00Z',
      platforms: [],
      edit_path: '/@manage/storages/edit/14'
    },
    15: {
      storage_id: 15, mount_path: '/broken', driver: 'BaiduNetdisk', status: 'work',
      disabled: false, modified: '2026-10-14T07:00:00Z',
      platforms: [{ id: 'baidu', name: '百度网盘', problem: '' }],
      edit_path: '/@manage/storages/edit/15'
    },
    16: {
      storage_id: 16, mount_path: '/broken2', driver: 'BaiduNetdisk', status: 'work',
      disabled: false, modified: '2026-10-14T07:30:00Z',
      platforms: [{ id: 'baidu', name: '百度网盘', problem: '' }],
      edit_path: '/@manage/storages/edit/16'
    }
  };

  // ---------- 会话 ----------
  var sessions = new Map();

  // 会话持久化到 sessionStorage：与真实部署一致——OpenList 进程重启前会话一直在，
  // 页面刷新不应丢失。定时器不持久化（刷新后 OAuth 不再自动完成，状态保持快照）。
  function persistSessions() {
    try {
      var arr = [];
      sessions.forEach(function (s) {
        arr.push({
          id: s.id, num: s.num, platform: s.platform, method: s.method, mode: s.mode,
          phase: s.phase, createdAt: s.createdAt, expiresAt: s.expiresAt,
          startCount: s.startCount, refreshLeft: s.refreshLeft,
          error: s.error, errorCode: s.errorCode,
          auth: s.auth, result: s.result, oauth: s.oauth, qr: s.qr, form: s.form,
          target: s.target, app: s.app, guide: s.guide, userCode: s.userCode,
          oauthClientID: s.oauthClientID, oauthRedirect: s.oauthRedirect
        });
      });
      sessionStorage.setItem('connect-mock-sessions', JSON.stringify(arr));
    } catch (e) { /* 忽略 */ }
  }

  function restoreSessions() {
    try {
      var arr = JSON.parse(sessionStorage.getItem('connect-mock-sessions') || '[]');
      arr.forEach(function (s) {
        var p = null;
        platforms.forEach(function (x) { if (x.id === s.platform) p = x; });
        if (!p) return;
        s.platformDef = p;
        s.timers = [];
        sessions.set(s.id, s);
      });
    } catch (e) { /* 忽略 */ }
  }
  restoreSessions();

  // 登录态放 sessionStorage：页面重新加载后仍保持登录（与真实 OpenList 的
  // localStorage.token 行为一致），便于测试刷新与恢复会话。
  var loggedIn = sessionStorage.getItem('connect-mock-logged-in') === '1';
  // mockScope 模拟服务端的会话来源：'' 未登录、'bearer' 手动登录、
  // 'connect' 免登录 Cookie（不落地任何存储，仅内存，与真实 Cookie 最接近）。
  var mockScope = '';
  function setLoggedIn(v) {
    loggedIn = v;
    if (!v) mockScope = '';
    if (v) sessionStorage.setItem('connect-mock-logged-in', '1');
    else sessionStorage.removeItem('connect-mock-logged-in');
  }
  var savedSeq = 30;

  function qrDataUrl(seedNum) {
    var n = 25, cell = 8, size = n * cell;
    var s = seedNum;
    var rnd = function () { s = (s * 1103515245 + 12345) & 0x7fffffff; return s / 0x7fffffff; };
    var rects = '';
    var x, y;
    for (y = 0; y < n; y++) {
      for (x = 0; x < n; x++) {
        var finder = (x < 8 && y < 8) || (x >= n - 8 && y < 8) || (x < 8 && y >= n - 8);
        if (finder) continue;
        if (rnd() < 0.45) {
          rects += '<rect x="' + (x * cell) + '" y="' + (y * cell) + '" width="' + cell + '" height="' + cell + '"/>';
        }
      }
    }
    var finderAt = function (ox, oy) {
      return '<rect x="' + ox + '" y="' + oy + '" width="' + (7 * cell) + '" height="' + (7 * cell) + '"/>' +
        '<rect x="' + (ox + cell) + '" y="' + (oy + cell) + '" width="' + (5 * cell) + '" height="' + (5 * cell) + '" fill="#fff"/>' +
        '<rect x="' + (ox + 2 * cell) + '" y="' + (oy + 2 * cell) + '" width="' + (3 * cell) + '" height="' + (3 * cell) + '"/>';
    };
    var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="' + size + '" height="' + size + '" viewBox="0 0 ' + size + ' ' + size + '">' +
      '<rect width="' + size + '" height="' + size + '" fill="#fff"/><g fill="#000">' +
      rects + finderAt(0, 0) + finderAt(size - 7 * cell, 0) + finderAt(0, size - 7 * cell) +
      '</g></svg>';
    return 'data:image/svg+xml;utf8,' + encodeURIComponent(svg);
  }

  function clearTimers(s) {
    (s.timers || []).forEach(clearTimeout);
    s.timers = [];
  }

  function later(s, ms, fn) {
    s.timers = s.timers || [];
    s.timers.push(setTimeout(function () {
      if (s.phase !== 'pending') return;
      fn();
      persistSessions();
    }, ms));
  }

  function authorize(s) {
    if (s.phase !== 'pending') return;
    clearTimers(s);
    s.phase = 'authorized';
    s.oauth = null;
    s.qr = null;
    s.form = null;
    s.guide = null;
    var p = s.platformDef;
    var renewal;
    var fields;
    var credType;
    if (s.app.mode === 'public_app') {
      renewal = '驱动用该公开应用直接向' + p.name + '续期（use_online_api=false）';
      fields = (p.reauth_fields || []).filter(function (k) { return k !== 'api_url_address'; });
      credType = 'oauth_refresh_token';
    } else if (s.app.mode === 'online_service') {
      renewal = '驱动通过 ' + (s.app.identity || SERVICE) + ' 续期（use_online_api=true）';
      fields = p.reauth_fields;
      credType = 'oauth_refresh_token';
    } else if (s.app.mode === 'own_app') {
      renewal = '驱动使用自有应用直接续期（use_online_api=false）';
      fields = p.reauth_fields;
      credType = 'oauth_refresh_token';
    } else {
      renewal = '驱动在令牌失效时用保存的账号密码重新登录';
      fields = p.reauth_fields;
      credType = 'account_password';
    }
    s.auth = {
      credential_type: credType,
      app_mode: s.app.mode,
      app_identity: s.app.identity,
      renewal: renewal,
      account: '测试账号（mock）',
      fields: fields,
      create_fields: p.id === 'onedrive' ? ['is_sharepoint', 'region'] : []
    };
  }

  function randomUserCode() {
    var chars = 'abcdefghjkmnpqrstuvwxyz23456789';
    var out = '';
    for (var i = 0; i < 8; i++) out += chars.charAt(Math.floor(Math.random() * chars.length));
    return out;
  }

  // 公共应用扫码（百度设备码流程）的初始提示：给出电脑端输入代码的备选入口。
  function initialQRMessage(s) {
    if (s.app.mode !== 'public_app' || !s.userCode) return '';
    return '也可以在电脑浏览器打开 https://openapi.baidu.com/device 输入代码 ' + s.userCode;
  }

  function initQR(s, fresh) {
    s.userCode = randomUserCode();
    s.refreshLeft = 2;
    s.qr = {
      status: 'waiting',
      image_url: qrDataUrl(1000 + (fresh ? s.startCount : 0)),
      content: '',
      expires_at: new Date(Date.now() + 120000).toISOString(),
      message: initialQRMessage(s),
      can_refresh: true
    };
    s.startCount = (s.startCount || 0) + 1;
    clearTimers(s);
    if (QR_MODE === 'expire') {
      later(s, 5000, function () {
        s.qr.status = 'expired';
        s.qr.message = '';
      });
    } else if (QR_MODE === 'neterr') {
      s.qr.status = 'network_error';
      s.qr.message = '连接网盘服务器超时';
      later(s, 6500, function () {
        s.qr.status = 'waiting';
        s.qr.message = initialQRMessage(s);
        scheduleScan(s);
      });
    } else {
      scheduleScan(s);
    }
  }

  function scheduleScan(s) {
    later(s, 4000, function () { if (s.qr) s.qr.status = 'scanned'; });
    later(s, 8000, function () {
      if (s.qr) { s.qr.status = 'confirmed'; s.qr.message = ''; }
      authorize(s);
    });
  }

  function oauthURL(s) {
    var q = 'response_type=code&client_id=' + encodeURIComponent(s.oauthClientID || 'mock') +
      '&redirect_uri=' + encodeURIComponent(s.oauthRedirect || REDIRECT) +
      '&scope=' + encodeURIComponent(s.platformDef.scopes.join(',')) +
      '&state=mockstate' + s.num;
    return 'https://openapi.example-provider.com/oauth/authorize?' + q;
  }

  // FormGuider：在线服务粘贴令牌模式先去 api.oplist.org 授权页。
  // 与真实适配器一致：百度/OneDrive 用 hash 预选驱动，Google 不带 hash
  //（避免压掉授权页的 Google 说明）。
  function formGuideOf(p, eff) {
    if (!eff || eff.mode !== 'online_service') return null;
    var preselect = function (driverTxt) {
      return SERVICE + '/#' + btoa(JSON.stringify({ driver_txt: driverTxt, server_use: 'true' }));
    };
    if (p.id === 'baidu') {
      return {
        url: preselect('baiduyun_go'),
        label: '打开 OpenList 授权页',
        steps: [
          '在打开的页面确认已选中「百度网盘 - 验证登录 (OAuth App)」并勾选「使用 OpenList 提供的参数」',
          '点「获取 Token」，登录百度账号并同意授权',
          '回到授权页，复制「刷新令牌（Refresh Token）」，粘贴到下面'
        ]
      };
    }
    if (p.id === 'onedrive') {
      return {
        url: preselect('onedrive_pr'),
        label: '打开 OpenList 授权页',
        steps: [
          '在打开的页面确认已选中「OneDrive - 个人版本 (OAuth App)」（个人账号和工作/学校账号都选这一项）并勾选「使用 OpenList 提供的参数」',
          '点「获取 Token」，登录 Microsoft 账号并同意授权',
          '回到授权页，复制「刷新令牌（Refresh Token）」，粘贴到下面'
        ]
      };
    }
    if (p.id === 'googledrive') {
      return {
        url: SERVICE + '/',
        label: '打开 OpenList 授权页',
        steps: [
          '选择「Google Drive OAuth2 (OAuth App)」，阅读弹出的说明后勾选「使用 OpenList 提供的参数」',
          '点「获取 Token」，登录 Google 账号并允许访问 Google Drive',
          '复制「刷新令牌（Refresh Token）」，粘贴到下面'
        ]
      };
    }
    return null;
  }

  function sessionView(s) {
    return {
      id: s.id,
      platform: s.platform,
      platform_name: s.platformDef.name,
      driver: s.platformDef.driver,
      method: s.method,
      form_label: s.platformDef.form_label || '',
      mode: s.mode,
      target: s.target || null,
      phase: s.phase,
      app: s.app,
      oauth: s.oauth,
      qr: s.qr,
      form: s.form,
      guide: s.guide || null,
      auth: s.auth,
      result: s.result,
      error: s.error || '',
      error_code: s.errorCode || '',
      root: {
        field: s.platformDef.root_field,
        default: s.platformDef.root_default,
        label: s.platformDef.root_label,
        help: s.platformDef.root_help
      },
      notes: s.platformDef.notes || [],
      verified: s.platformDef.verified || '',
      created_at: s.createdAt,
      expires_at: s.expiresAt
    };
  }

  function startSession(body) {
    var p = null;
    for (var i = 0; i < platforms.length; i++) {
      if (platforms[i].id === body.platform) p = platforms[i];
    }
    if (!p) return err(404, '平台不存在', 'not_found');
    // 生效应用：已保存配置优先，否则平台默认；两者皆无 → app_missing。
    var eff;
    if (needsApp(p)) {
      var a = apps[p.id];
      if (a) {
        if (a.problem) return err(400, '应用配置无效：' + a.problem, 'app_invalid');
        eff = { mode: a.mode, identity: appIdentity(p, a), service_url: a.service_url || '' };
      } else {
        var def = defaultAppOf(p);
        if (!def) return err(400, '请先配置「' + p.name + '」的开发者应用', 'app_missing');
        eff = { mode: def.mode, identity: def.identity, service_url: def.service_url || '' };
      }
    } else {
      eff = { mode: 'none', identity: '', service_url: '' };
    }
    var s = {
      id: 'mock-session-' + (++seq),
      num: seq,
      platform: p.id,
      platformDef: p,
      method: ((p.mode_methods || {})[eff.mode]) || p.method,
      mode: body.reauth_storage_id ? 'reauth' : 'create',
      phase: 'pending',
      createdAt: now(),
      expiresAt: new Date(Date.now() + 600000).toISOString(),
      timers: [], startCount: 0, refreshLeft: 2,
      error: '', errorCode: '', auth: null, result: null,
      oauth: null, qr: null, form: null, guide: null, target: null
    };
    s.app = { mode: eff.mode, identity: eff.identity, scopes: p.scopes };
    if (body.reauth_storage_id) {
      var t = targets[body.reauth_storage_id];
      if (!t) return err(404, '存储不存在', 'not_found');
      var entry = null;
      (t.platforms || []).forEach(function (e) { if (e.id === p.id) entry = e; });
      if (!entry) return err(400, '该存储的驱动与本平台不一致', 'unsupported');
      if (entry.problem) return err(400, entry.problem, 'unsupported');
      s.target = {
        storage_id: t.storage_id, mount_path: t.mount_path,
        driver: t.driver, status: t.status, modified: t.modified
      };
    }
    if (s.method === 'oauth') {
      var oa = apps[p.id] || {};
      s.oauthClientID = oa.client_id || '';
      s.oauthRedirect = oa.redirect_uri || REDIRECT;
      s.oauth = {
        authorize_url: oauthURL(s),
        redirect_uri: s.oauthRedirect,
        oob: s.oauthRedirect === 'oob'
      };
      later(s, 6000, function () { authorize(s); });
    } else if (s.method === 'qrcode') {
      initQR(s, true);
    } else if (s.method === 'form') {
      s.form = p.form_fields;
      s.guide = formGuideOf(p, eff);
    } else {
      return err(400, '该平台使用了未知的登录方式', 'unsupported');
    }
    sessions.set(s.id, s);
    return ok(sessionView(s));
  }

  function submitSession(body) {
    var s = sessions.get(body.id);
    if (!s) return err(404, '会话不存在或已过期', 'not_found');
    if (s.phase !== 'pending') return err(410, phaseMessage(s.phase), s.phase === 'expired' ? 'expired' : 'conflict');
    var values = body.values || {};
    if (s.method === 'oauth') {
      if (s.oauth && s.oauth.oob) {
        if (!values.code) return err(400, '请粘贴授权码', 'invalid_input');
        authorize(s);
      } else {
        var cb = String(values.callback_url || '');
        if (cb.indexOf('code=') >= 0 && cb.indexOf('state=mockstate' + s.num) >= 0) {
          authorize(s);
        } else {
          return err(400, '地址中没有授权信息，请粘贴完整的跳转地址', 'invalid_input');
        }
      }
    } else if (s.method === 'form') {
      var pasteToken = (s.form || []).some(function (f) { return f.key === 'refresh_token'; });
      if (pasteToken) {
        // ParsePastedToken 的近似校验：非空、无空白、长度合理；fail 模拟网盘拒绝。
        var tok = String(values.refresh_token || '').trim();
        if (!tok) return err(400, '请先在授权页获取并粘贴刷新令牌', 'invalid_input');
        if (/\s/.test(tok) || tok.length < 16 || tok.length > 8192) {
          return err(400, '刷新令牌格式不正确：不能包含空白，长度至少 16 个字符', 'invalid_input');
        }
        if (tok === 'fail') return err(502, '网盘服务器拒绝请求：令牌无效或已撤销（mock）', 'provider');
        authorize(s);
      } else {
        if (values.username === 'fail') {
          return err(400, '账号或密码格式不正确', 'invalid_input');
        }
        if (!values.username || !values.password) {
          return err(400, '请填写账号和密码', 'invalid_input');
        }
        authorize(s);
      }
    } else {
      return err(400, '该会话不需要提交内容', 'invalid_input');
    }
    return ok(sessionView(s));
  }

  function refreshSession(body) {
    var s = sessions.get(body.id);
    if (!s) return err(404, '会话不存在或已过期', 'not_found');
    if (s.phase !== 'pending') return err(410, phaseMessage(s.phase), 'conflict');
    if (!s.qr) return err(400, '该会话没有二维码', 'invalid_input');
    if (s.refreshLeft <= 0) return err(429, '刷新次数已用完，请重新开始', 'limit');
    s.refreshLeft -= 1;
    initQR(s, true);
    return ok(sessionView(s));
  }

  function getSession(q) {
    var s = sessions.get(q.get('id'));
    if (!s) return err(404, '会话不存在或已过期', 'not_found');
    return ok(sessionView(s));
  }

  function cancelSession(body) {
    var s = sessions.get(body.id);
    if (!s) return err(404, '会话不存在或已过期', 'not_found');
    if (s.phase === 'saving') return err(409, '正在保存，无法取消', 'conflict');
    if (s.phase === 'pending' || s.phase === 'authorized') {
      clearTimers(s);
      s.phase = 'cancelled';
      s.oauth = null; s.qr = null; s.form = null; s.auth = null;
    }
    return ok(sessionView(s));
  }

  function saveSession(body) {
    var s = sessions.get(body.id);
    if (!s) return err(404, '会话不存在或已过期', 'not_found');
    if (s.phase === 'saving') return err(409, '正在保存中', 'conflict');
    if (s.phase !== 'authorized') return err(410, phaseMessage(s.phase), 'conflict');

    var mount = String(body.mount_path || '').trim();
    if (s.mode === 'create') {
      if (mount.indexOf('/') !== 0 || mount === '/') {
        s.result = {
          ok: false, mode: 'create', storage_id: 0, mount_path: mount, status: '',
          message: '挂载路径格式不正确：必须以 / 开头，且不能只是 /',
          error: '', retryable: true, credentials_written: false,
          cleanup: null, restore: null
        };
        return ok(sessionView(s));
      }
      if (mount === '/taken') {
        s.result = {
          ok: false, mode: 'create', storage_id: 0, mount_path: mount, status: '',
          message: '挂载路径已被占用：' + mount + '，请换一个路径',
          error: 'mount path already taken', retryable: true, credentials_written: false,
          cleanup: null, restore: null
        };
        return ok(sessionView(s));
      }
      var sid = ++savedSeq;
      if (mount === '/fail') {
        clearTimers(s);
        s.phase = 'save_failed';
        s.result = {
          ok: false, mode: 'create', storage_id: sid, mount_path: mount, status: '',
          message: '驱动初始化失败，已删除本次创建的存储 #' + sid,
          error: 'failed init storage: 网盘服务器无响应（mock）',
          retryable: false, credentials_written: true,
          cleanup: { attempted: true, deleted: true, storage_id: sid, error: '' },
          restore: null
        };
        return ok(sessionView(s));
      }
      if (mount === '/fail-cleanup') {
        clearTimers(s);
        s.phase = 'save_failed';
        s.result = {
          ok: false, mode: 'create', storage_id: sid, mount_path: mount, status: '',
          message: '驱动初始化失败，且清理未能删除存储 #' + sid + '，请手动处理',
          error: 'failed init storage: 网盘服务器拒绝请求（mock）',
          retryable: false, credentials_written: true,
          cleanup: { attempted: true, deleted: false, storage_id: sid, error: 'remove storage: context deadline exceeded' },
          restore: null
        };
        return ok(sessionView(s));
      }
      clearTimers(s);
      s.phase = 'saved';
      takenMounts[mount] = true; // 之后的挂载建议让出该路径（-2）
      s.result = {
        ok: true, mode: 'create', storage_id: sid, mount_path: mount, status: 'work',
        message: '存储创建成功，驱动已初始化。', error: '', retryable: false,
        credentials_written: true, cleanup: null, restore: null
      };
      return ok(sessionView(s));
    }

    // 重新授权
    var t = s.target;
    if (t.storage_id === 12) {
      clearTimers(s);
      s.phase = 'saved';
      s.result = {
        ok: true, mode: 'reauth', storage_id: 12, mount_path: '/baidu', status: 'work',
        message: '已更新存储 #12 的登录凭据与续期参数。', error: '', retryable: false,
        credentials_written: true, cleanup: null, restore: null
      };
      return ok(sessionView(s));
    }
    clearTimers(s);
    s.phase = 'save_failed';
    if (t.storage_id === 15) {
      s.result = {
        ok: false, mode: 'reauth', storage_id: 15, mount_path: '/broken', status: 'work',
        message: '驱动初始化失败，已恢复存储 #15 的原配置。',
        error: 'failed init storage: 百度服务器返回未授权（mock）',
        retryable: false, credentials_written: true,
        cleanup: null,
        restore: { attempted: true, restored: true, status: 'work', error: '' }
      };
    } else {
      s.result = {
        ok: false, mode: 'reauth', storage_id: 16, mount_path: '/broken2', status: 'work',
        message: '驱动初始化失败，且未能恢复存储 #16 的原配置，请手动检查。',
        error: 'failed init storage: 百度服务器返回未授权（mock）',
        retryable: false, credentials_written: true,
        cleanup: null,
        restore: { attempted: true, restored: false, status: 'work', error: 'update storage: context deadline exceeded' }
      };
    }
    return ok(sessionView(s));
  }

  function phaseMessage(phase) {
    if (phase === 'expired') return '会话已过期';
    if (phase === 'cancelled') return '会话已取消';
    return '当前状态（' + phase + '）不允许该操作';
  }

  // ---------- 登录 ----------
  function loginHash(body) {
    if (!body || !body.username || !body.password) {
      return err(400, '请输入用户名和密码', 'invalid_input');
    }
    if (body.password === HASH_WRONG) {
      return err(400, '登录失败：用户名或密码错误', '');
    }
    if (body.username === 'otp') {
      if (!body.otp_code) return err(402, '请输入两步验证码', '');
      if (body.otp_code !== '123456') return err(402, '两步验证码不正确', '');
    }
    setLoggedIn(true);
    mockScope = 'bearer';
    return ok({ token: 'mock-token-' + Date.now() });
  }

  // ---------- 路由 ----------
  function ok(data) {
    if (typeof persistSessions === 'function') persistSessions();
    return Promise.resolve({ code: 200, message: 'success', data: data });
  }
  function err(code, message, errorCode) {
    return Promise.resolve({ code: code, message: message, data: { error_code: errorCode || '' } });
  }

  // path 可以带查询串（target 与 session/get 的 id 在查询里）。
  function handle(method, rawPath, body) {
    body = body || {};
    var path = rawPath;
    var query = new URLSearchParams();
    var qIndex = rawPath.indexOf('?');
    if (qIndex >= 0) {
      path = rawPath.slice(0, qIndex);
      query = new URLSearchParams(rawPath.slice(qIndex + 1));
    }
    if (path === 'admin/connect/auth/me') {
      if (!loggedIn) return err(401, '未登录或登录已过期', '');
      return ok({
        id: 1, username: 'admin', role: 2, base_url: '', disabled: false,
        // scope 与服务端一致：connect=免登录 Cookie，bearer=手动登录令牌。
        scope: mockScope
      });
    }
    if (path === 'auth/login/hash' && method === 'POST') return loginHash(body);
    if (path === 'connect/handoff/exchange' && method === 'POST') {
      // 模拟兑换：code 为 good（默认）或任意非空值成功；expired/used 返回 410。
      var code = String(body.code || '');
      if (!code) return err(400, '缺少移交票据', 'invalid_input');
      if (code === 'expired' || code === 'used') return err(410, '移交链接已失效或已被使用', 'expired');
      mockScope = 'connect';
      setLoggedIn(true);
      return ok({ username: 'admin', expires_in: 1800 });
    }
    if (path === 'admin/connect/auth/logout') {
      mockScope = '';
      setLoggedIn(false);
      return ok(null);
    }
    if (path === 'admin/connect/platforms') {
      return ok({
        module: moduleInfo,
        callback_path: '/api/connect/oauth/callback',
        suggested_redirect_uri: '',
        platforms: platforms.map(function (p) {
          return {
            id: p.id, name: p.name, driver: p.driver, method: p.method,
            app_modes: p.app_modes, mode_methods: p.mode_methods || {},
            public_app_label: p.public_app_label || '',
            form_label: p.form_label || '',
            active_method: activeMethodOf(p),
            scopes: p.scopes, pkce: p.pkce,
            allow_oob: p.allow_oob, form_fields: p.form_fields,
            root_field: p.root_field, root_default: p.root_default,
            root_label: p.root_label, root_help: p.root_help,
            reauth_fields: p.reauth_fields, notes: p.notes || [],
            verified: p.verified, docs_url: p.docs_url || '',
            app: publicApp(p)
          };
        })
      });
    }
    if (path === 'admin/connect/app/save' && method === 'POST') {
      var p = null;
      platforms.forEach(function (x) { if (x.id === body.platform) p = x; });
      if (!p) return err(404, '平台不存在', 'not_found');
      if (!needsApp(p)) return err(400, '该平台不需要应用配置', 'unsupported');
      var problem = checkApp(p, body);
      if (problem) return err(400, problem, 'app_invalid');
      var old = apps[p.id] || { revision: 0, updated_at: '' };
      var pub = body.mode === 'public_app';
      apps[p.id] = {
        platform: p.id,
        mode: body.mode,
        // 公共应用只存模式：凭据永远来自适配器。
        client_id: pub ? '' : (body.client_id || ''),
        client_secret: pub ? '' : (body.client_secret || (old.client_secret || '')),
        redirect_uri: pub ? '' : (body.redirect_uri || ''),
        service_url: pub ? '' : (body.service_url || ''),
        revision: (old.revision || 0) + 1,
        updated_at: now(),
        problem: ''
      };
      return ok(publicApp(p));
    }
    if (path === 'admin/connect/app/delete' && method === 'POST') {
      delete apps[body.platform];
      return ok(null);
    }
    if (path === 'admin/connect/mount/suggest') {
      var sp = null;
      platforms.forEach(function (x) { if (x.id === query.get('platform')) sp = x; });
      if (!sp) return err(404, '平台不存在', 'not_found');
      var base = '/' + sp.name;
      return ok({ mount_path: takenMounts[base] ? base + '-2' : base });
    }
    if (path === 'admin/connect/target') {
      var t = targets[Number(query.get('id'))];
      if (!t) return err(404, '存储不存在', 'not_found');
      return ok(JSON.parse(JSON.stringify(t)));
    }
    if (path === 'admin/connect/session/start' && method === 'POST') return startSession(body);
    if (path === 'admin/connect/session/get') return getSession(query);
    if (path === 'admin/connect/session/submit' && method === 'POST') return submitSession(body);
    if (path === 'admin/connect/session/refresh' && method === 'POST') return refreshSession(body);
    if (path === 'admin/connect/session/cancel' && method === 'POST') return cancelSession(body);
    if (path === 'admin/connect/session/save' && method === 'POST') return saveSession(body);
    return err(404, '未知接口：' + path, 'not_found');
  }

  window.CONNECT_MOCK = { handle: handle };
})();
