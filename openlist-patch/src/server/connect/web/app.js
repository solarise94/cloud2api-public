/* OpenList connect 管理页（无框架、无构建、无外部依赖）。
 * 只做：管理员登录、平台列表、应用配置、登录授权会话（OAuth / 扫码 / 表单）、
 * 确认并保存与保存结果。所有服务器数据一律经 textContent 写入，不使用 innerHTML。
 *
 * cloud2api 打开本页时可带 return 参数（见下方 returnUrl），保存成功后优先
 * 引导返回 cloud2api 刷新网盘列表。
 *
 * 免登录进入：cloud2api 打开本页时可带 #handoff=<一次性票据>&target=<路由>，
 * 页面立即把 fragment 从地址栏移除，再用票据兑换 HttpOnly 的连接会话
 * Cookie（服务端只下发用户名等元数据，绝不下发令牌）。兑换失败时给出明确
 * 的重试与手动登录回退；手动登录仍走 OpenList 账号密码。
 *
 * 开发调试：URL 加 ?mock=1 时动态加载 ./mock.js，把所有 API 调用改走
 * window.CONNECT_MOCK.handle(method, path, body)。生产构建不含 mock.js。 */
'use strict';

(function () {
  // ---------- 常量 ----------
  var LOGIN_SALT = '-https://github.com/alist-org/alist';
  var SHA256_ABC = 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad';
  var POLL_MS = 2000;

  // ---------- 返回 cloud2api ----------
  // return 是 cloud2api 页面的地址，可含 {storage_id} 占位符（保存后替换成
  // 新存储 ID）。只接受长度受控、无凭据的 http(s) 地址，主机名须与本页相同
  // 或是本机回环地址（门户用 127.0.0.1 打开、本页用局域网地址打开时）。
  // 加载时判定一次，之后只在这两者之间取值：完整地址或 ''。
  var returnUrl = (function () {
    var raw = new URLSearchParams(location.search).get('return') || '';
    if (!raw || raw.length > 2048) return '';
    var u;
    try { u = new URL(raw); } catch (e) { return ''; }
    if ((u.protocol !== 'http:' && u.protocol !== 'https:') || u.username || u.password) return '';
    var h = u.hostname;
    var loopback = h === 'localhost' || h === '[::1]' || /^127\.\d+\.\d+\.\d+$/.test(h);
    if (h !== location.hostname && !loopback) return '';
    return u.href;
  })();

  // storageId 为空（0/undefined）时占位符替换为空串。门户的公共回程页
  // /admin/connect-return 只接受正数 storage_id：没有 ID 的返回链接（失败、
  // 过期、取消）一律去掉查询与锚点、指向回程页本身（无参数模式由门户自行
  // 引导到网盘列表），避免生成 ?storage_id= 的 400 链接，也不借道需要门户
  // 登录的后台地址。
  function returnHref(storageId) {
    if (!returnUrl) return '';
    if (storageId) return returnUrl.split('{storage_id}').join(String(storageId));
    var cut = returnUrl.indexOf('connect-return');
    if (cut > 0 && returnUrl.charAt(cut - 1) === '/') {
      var base = returnUrl.slice(0, cut);
      if (/^https?:\/\//.test(base)) return base + 'connect-return';
    }
    return returnUrl.split('{storage_id}').join('');
  }

  // ---------- 状态 ----------
  var state = {
    mock: new URLSearchParams(location.search).get('mock') !== null,
    mockLoaded: false,
    token: '',
    me: null,
    lastScope: '',           // 'connect'（免登录 Cookie）或 'bearer'（手动登录令牌）
    notice: null,            // {kind: success|info|warn|danger, text}
    loginNeedsOtp: false,
    loginBusy: false,
    loginError: '',
    sessionId: null,
    sessionView: null,
    storage: null,           // {mount_path, root, remark}
    storageError: '',
    saveError: '',
    formError: '',
    appFormError: '',
    saving: false,
    lastViewJson: '',
    forceRender: false,
    pollTimer: 0,
    pollBusy: false,
    sha256ok: true
  };

  // ---------- DOM 构建助手（所有数据只经 textContent / value 写入） ----------
  function el(tag, attrs) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        var v = attrs[k];
        if (v === null || v === undefined || v === false) return;
        if (k === 'class') node.className = v;
        else if (k === 'text') node.textContent = v;
        else if (k === 'on' && typeof v === 'object') {
          Object.keys(v).forEach(function (ev) { node.addEventListener(ev, v[ev]); });
        } else if (v === true) node.setAttribute(k, '');
        else node.setAttribute(k, String(v));
      });
    }
    for (var i = 2; i < arguments.length; i++) appendChild(node, arguments[i]);
    return node;
  }

  function appendChild(node, child) {
    if (child === null || child === undefined || child === false || child === '') return;
    if (Array.isArray(child)) { child.forEach(function (c) { appendChild(node, c); }); return; }
    if (child instanceof Node) node.appendChild(child);
    else node.appendChild(document.createTextNode(String(child)));
  }

  function harvestInputs(root) {
    var vals = {};
    if (!root) return vals;
    var nodes = root.querySelectorAll('[data-persist]');
    for (var i = 0; i < nodes.length; i++) vals[nodes[i].getAttribute('data-persist')] = nodes[i].value;
    return vals;
  }

  function applyPersisted(root, vals) {
    if (!root || !vals) return;
    var nodes = root.querySelectorAll('[data-persist]');
    for (var i = 0; i < nodes.length; i++) {
      var k = nodes[i].getAttribute('data-persist');
      if (vals[k]) nodes[i].value = vals[k];
    }
  }

  // 用“先收割旧输入、再重建、再回填”的方式重绘，避免轮询重绘丢掉正在输入的内容。
  function withPersist(build) {
    var keep = harvestInputs(document.getElementById('app'));
    var node = build();
    applyPersisted(node, keep);
    return node;
  }

  function announce(text) {
    var n = document.getElementById('conn-live');
    if (!n || !text) return;
    n.textContent = '';
    setTimeout(function () { n.textContent = text; }, 50);
  }

  // ---------- SHA-256（FIPS 180-4） ----------
  // crypto.subtle 在普通 http 局域网源上不可用，这里自带一份纯 JS 实现。
  // >>> SHA-256
  var sha256Hex = (function () {
    var K = [
      0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
      0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
      0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
      0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
      0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
      0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
      0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
      0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2
    ];
    function rotr(x, n) { return (x >>> n) | (x << (32 - n)); }
    return function (text) {
      var bytes = new TextEncoder().encode(text);
      var len = bytes.length;
      var blocks = Math.ceil((len + 9) / 64);
      var words = new Uint32Array(blocks * 16);
      var i;
      for (i = 0; i < len; i++) words[i >> 2] |= bytes[i] << ((3 - (i & 3)) * 8);
      words[len >> 2] |= 0x80 << ((3 - (len & 3)) * 8);
      words[words.length - 1] = (len << 3) >>> 0;
      words[words.length - 2] = Math.floor(len / 0x20000000);
      var h0 = 0x6a09e667, h1 = 0xbb67ae85, h2 = 0x3c6ef372, h3 = 0xa54ff53a;
      var h4 = 0x510e527f, h5 = 0x9b05688c, h6 = 0x1f83d9ab, h7 = 0x5be0cd19;
      var w = new Uint32Array(64);
      for (var b = 0; b < blocks; b++) {
        for (i = 0; i < 16; i++) w[i] = words[b * 16 + i];
        for (i = 16; i < 64; i++) {
          var s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
          var s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
          w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
        }
        var a = h0, bb = h1, c = h2, d = h3, e = h4, f = h5, g = h6, h = h7;
        for (i = 0; i < 64; i++) {
          var S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
          var ch = (e & f) ^ (~e & g);
          var t1 = (h + S1 + ch + K[i] + w[i]) >>> 0;
          var S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
          var maj = (a & bb) ^ (a & c) ^ (bb & c);
          var t2 = (S0 + maj) >>> 0;
          h = g; g = f; f = e;
          e = (d + t1) >>> 0;
          d = c; c = bb; bb = a;
          a = (t1 + t2) >>> 0;
        }
        h0 = (h0 + a) >>> 0; h1 = (h1 + bb) >>> 0; h2 = (h2 + c) >>> 0; h3 = (h3 + d) >>> 0;
        h4 = (h4 + e) >>> 0; h5 = (h5 + f) >>> 0; h6 = (h6 + g) >>> 0; h7 = (h7 + h) >>> 0;
      }
      return [h0, h1, h2, h3, h4, h5, h6, h7]
        .map(function (x) { return ('00000000' + x.toString(16)).slice(-8); })
        .join('');
    };
  })();
  // <<< SHA-256

  // ---------- API ----------
  function apiUrl(path) {
    return new URL('../api/' + path, location.href).href;
  }

  function mkErr(message, code, errorCode) {
    var e = new Error(message);
    e.code = code;
    e.errorCode = errorCode || '';
    return e;
  }

  function api(method, path, body) {
    if (state.mock) {
      if (!window.CONNECT_MOCK) {
        return Promise.reject(mkErr('开发脚本 mock.js 未加载', 500, ''));
      }
      return window.CONNECT_MOCK.handle(method, path, body).then(function (r) {
        if (r && r.code === 200) return r.data;
        throw mkErr((r && r.message) || '请求失败', (r && r.code) || 500,
          (r && r.data && r.data.error_code) || '');
      });
    }
    var headers = { 'Accept': 'application/json' };
    if (state.token) headers['Authorization'] = state.token;
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    return fetch(apiUrl(path), {
      method: method,
      headers: headers,
      body: body !== undefined ? JSON.stringify(body) : undefined
    }).then(function (res) {
      return res.json().catch(function () { return null; }).then(function (env) {
        if (!env || typeof env.code !== 'number') {
          throw mkErr('服务器响应格式错误（HTTP ' + res.status + '）', res.status, '');
        }
        if (env.code === 200) return env.data;
        throw mkErr(env.message || ('请求失败（' + env.code + '）'), env.code,
          (env.data && env.data.error_code) || '');
      });
    }, function (err) {
      throw mkErr('无法连接服务器：' + err.message, 0, '');
    });
  }

  function clearToken() {
    state.token = '';
    state.me = null;
    try { localStorage.removeItem('token'); } catch (e) { /* 忽略 */ }
  }

  // ---------- 文案 ----------
  // form 登录的文案可被适配器覆盖（form_label，如「粘贴令牌」）。
  function methodLabel(m, formLabel) {
    if (m === 'oauth') return '浏览器授权';
    if (m === 'qrcode') return '扫码登录';
    if (m === 'form') return formLabel || '账号密码';
    return m || '';
  }

  function methodBadge(m, formLabel) {
    return el('span', { class: 'badge badge-method', text: methodLabel(m, formLabel) });
  }

  function verifiedBadge(v) {
    return v
      ? el('span', { class: 'badge badge-verified', text: '已验证：' + v })
      : el('span', { class: 'badge badge-unverified', text: '未经真实账号验证' });
  }

  function statusBadge(s) {
    var map = { work: '正常', disabled: '已停用' };
    var label = map[s] !== undefined ? map[s] + '（' + s + '）' : (s || '未知');
    var cls = s === 'work' ? 'badge-ok' : s === 'disabled' ? 'badge-warn' : 'badge-neutral';
    return el('span', { class: 'badge ' + cls, text: label });
  }

  function credLabel(t) {
    if (t === 'oauth_refresh_token') return 'OAuth 刷新令牌';
    if (t === 'account_password') return '账号密码';
    return t || '原始凭据';
  }

  function appModeLabel(m, longForm) {
    if (m === 'own_app') return longForm ? '自有开发者应用' : '自有应用';
    if (m === 'online_service') return 'OpenList 在线授权服务';
    if (m === 'public_app') return '公共应用';
    if (m === 'none' || !m) return '无需开发者应用';
    return m || '';
  }

  // 平台首个模式不是自有应用时有可用的默认（无需应用、公共应用、默认地址的
  // 在线授权服务），删除已保存配置即回到该默认。
  function modeHasDefault(m) {
    return !!m && m !== 'own_app';
  }

  // 平台在某应用模式下使用的登录方式（Meta.MethodFor 的页面侧版本）。
  function modeMethodOf(p, m) {
    return ((p && p.mode_methods) || {})[m] || (p && p.method) || '';
  }

  var KIND_LABEL = { success: '成功', info: '提示', warn: '注意', danger: '失败' };

  function banner(kind, text) {
    return el('div', { class: 'banner banner-' + kind, role: kind === 'danger' ? 'alert' : 'status' },
      el('strong', { text: (KIND_LABEL[kind] || '提示') + '：' }),
      el('span', { text: text }));
  }

  function errorCodeLine(e) {
    if (!e || (!e.errorCode && !e.code)) return null;
    return el('p', { class: 'error-code', text: '错误码：' + (e.errorCode || '') + '（HTTP ' + e.code + '）' });
  }

  function scopeChips(scopes) {
    return el('span', null, (scopes || []).map(function (s) {
      return el('span', { class: 'chip', text: s });
    }));
  }

  function kvTable(rows) {
    return el('table', { class: 'kv' },
      el('tbody', null, rows.map(function (r) {
        return el('tr', null, el('th', { scope: 'row', text: r[0] }), el('td', null, r[1]));
      })));
  }

  function buildSteps(defs, current) {
    return el('ol', { class: 'steps', 'aria-label': '步骤' }, defs.map(function (s, i) {
      var cls = i < current ? 'done' : i === current ? 'current' : 'todo';
      return el('li', { class: cls },
        el('span', { class: 'n', text: i < current ? '✓' : String(i + 1) }),
        el('span', { text: s }));
    }));
  }

  // ---------- 渲染骨架 ----------
  function paint(content) {
    var app = document.getElementById('app');
    var notice = state.notice;
    state.notice = null;
    app.textContent = '';
    if (state.me) {
      app.appendChild(el('header', { class: 'top' },
        el('div', { class: 'top-inner' },
          el('a', { class: 'brand', href: '#/' }, '网盘连接'),
          el('div', { class: 'top-user' },
            el('span', { class: 'username', text: state.me.username || '' }),
            el('button', { class: 'btn btn-ghost', on: { click: logout }, text: '退出' })))));
    }
    var main = el('main', { class: 'main' });
    if (notice) main.appendChild(banner(notice.kind, notice.text));
    main.appendChild(content);
    app.appendChild(main);
  }

  function go(hash) {
    if (location.hash === hash) route();
    else location.hash = hash;
  }

  // ---------- 登录 ----------
  // 身份校验走模块自己的 /api/admin/connect/auth/me：免登录 Cookie 与手动
  // 登录令牌都由它判定，页面从此不再调用上游 /api/me。Cookie 会话优先：
  // 即使本地还留着旧的（可能已过期的）手动登录令牌，scope=connect 时也不再
  // 发送 Authorization 头，后续调用完全依赖 HttpOnly Cookie。
  function ensureAuth() {
    if (state.me && state.me.role === 2) return Promise.resolve({ ok: true });
    state.token = '';
    try { state.token = localStorage.getItem('token') || ''; } catch (e) { state.token = ''; }
    return api('GET', 'admin/connect/auth/me').then(function (me) {
      if (me && me.role === 2) {
        state.me = me;
        state.lastScope = me.scope === 'connect' ? 'connect' : 'bearer';
        if (me.scope === 'connect') state.token = '';
        return { ok: true };
      }
      state.me = me || null;
      return { reason: 'notadmin' };
    }, function (e) {
      if (e.code === 401) { clearToken(); return { reason: 'notoken' }; }
      if (e.code === 403) { return { reason: 'notadmin' }; }
      return { reason: 'error', message: e.message };
    });
  }

  function logout() {
    var done = function () {
      clearToken();
      state.loginNeedsOtp = false;
      state.loginError = '';
      state.lastScope = '';
      state.notice = { kind: 'info', text: '已退出登录' };
      route();
    };
    // POST /api/admin/connect/auth/logout：免登录会话在服务端吊销并清除
    // Cookie；手动登录令牌沿用上游登出语义（令牌作废）。
    api('POST', 'admin/connect/auth/logout').then(done, done);
  }

  function fieldInput(opts) {
    var id = opts.id;
    // 密码类输入只写：不进持久化机制（无 data-persist），重绘后需重新输入。
    var input = el('input', {
      id: id,
      type: opts.type || 'text',
      'data-persist': opts.type === 'password' ? null : id,
      autocomplete: opts.autocomplete || 'off',
      placeholder: opts.placeholder || ''
    });
    if (opts.value) input.value = opts.value;
    var wrap = el('div', { class: 'field' },
      el('label', { for: id, text: opts.label }),
      input);
    if (opts.help) wrap.appendChild(el('p', { class: 'help', text: opts.help }));
    return wrap;
  }

  function renderGate(info) {
    stopPolling();
    state.me = null;
    paint(withPersist(function () {
      var card = el('div', { class: 'card gate' },
        el('h1', { text: '网盘连接' }),
        el('p', { class: 'sub', text: '登录 OpenList 管理员账号后配置网盘存储。' }));
      if (state.lastScope === 'connect') {
        card.appendChild(banner('warn',
          '免登录会话已过期或已退出。请回到 cloud2api 重新点「添加网盘」获取新的进入链接，或在下方重新登录。'));
      }
      if (info && info.reason === 'notadmin') {
        card.appendChild(banner('danger', '当前账号不是管理员。'));
        card.appendChild(el('div', { class: 'actions' },
          el('button', { class: 'btn', on: { click: logout }, text: '退出' })));
        return card;
      }
      if (info && info.reason === 'error' && info.message) {
        card.appendChild(banner('danger', info.message));
      }
      if (!state.sha256ok) {
        card.appendChild(banner('warn', '本浏览器的 SHA-256 自检未通过，登录可能无法完成，请更换现代浏览器。'));
      }
      var form = el('form', { on: { submit: doLogin } },
        fieldInput({ id: 'li-user', label: '用户名', autocomplete: 'username' }),
        fieldInput({ id: 'li-pass', label: '密码（OpenList 管理员密码）', type: 'password', autocomplete: 'current-password',
          help: '这里需要 OpenList 自己的管理员账号与密码，不是 cloud2api 管理门户的密码。' }));
      if (state.loginNeedsOtp) {
        form.appendChild(fieldInput({
          id: 'li-otp', label: '两步验证码', autocomplete: 'one-time-code',
          help: '该账号开启了两步验证，请输入动态验证码。'
        }));
      }
      if (state.loginError) form.appendChild(banner('danger', state.loginError));
      form.appendChild(el('div', { class: 'actions' },
        el('button', { class: 'btn btn-primary', type: 'submit', disabled: state.loginBusy },
          state.loginBusy ? '登录中…' : '登录')));
      card.appendChild(form);
      if (returnUrl) {
        card.appendChild(el('div', { class: 'actions' },
          el('a', { class: 'btn btn-ghost', href: returnHref(null) }, '返回 cloud2api')));
      }
      return card;
    }));
  }

  function val(id) {
    var n = document.getElementById(id);
    return n ? n.value.trim() : '';
  }

  function doLogin(ev) {
    ev.preventDefault();
    var u = val('li-user');
    var pn = document.getElementById('li-pass');
    var p = pn ? pn.value : '';
    var otp = val('li-otp');
    if (!u || !p) {
      state.loginError = '请输入用户名和密码。';
      renderGate();
      return;
    }
    state.loginBusy = true;
    renderGate();
    var body = {
      username: u,
      password: sha256Hex(p + LOGIN_SALT),
      otp_code: otp || ''
    };
    api('POST', 'auth/login/hash', body).then(function (data) {
      state.token = data && data.token ? data.token : '';
      try { localStorage.setItem('token', state.token); } catch (e) { /* 忽略 */ }
      state.loginNeedsOtp = false;
      state.loginError = '';
      state.loginBusy = false;
      return api('GET', 'admin/connect/auth/me').then(function (me) {
        if (me && me.role === 2) {
          state.me = me;
          state.notice = { kind: 'success', text: '登录成功' };
          route();
        } else {
          state.me = me || null;
          renderGate({ reason: 'notadmin' });
        }
      });
    }, function (e) {
      state.loginBusy = false;
      if (e.code === 401) { clearToken(); }
      if (e.code === 402 || /otp|两步/i.test(e.message || '')) {
        state.loginNeedsOtp = true;
        state.loginError = e.message || '请输入两步验证码。';
      } else if (e.code === 429) {
        state.loginError = '尝试次数过多，请稍后再试。';
      } else {
        state.loginError = e.message || '登录失败。';
      }
      renderGate();
    });
  }

  // ---------- 首页 ----------
  function appLineText(app) {
    var prefix = app.default ? '默认：' : '';
    if (!app.required) return prefix + '无需开发者应用';
    if (!app.configured) return prefix + '尚未配置应用';
    if (app.problem) return prefix + '应用配置无效：' + app.problem;
    if (app.mode === 'own_app') return prefix + '自有应用 ' + (app.identity || '');
    if (app.mode === 'online_service') return prefix + 'OpenList 在线授权服务 ' + (app.identity || '');
    if (app.mode === 'public_app') return prefix + '公共应用 ' + (app.identity || '');
    return prefix;
  }

  function platformCard(p) {
    var app = p.app || {};
    var ready = !app.required || (app.configured && !app.problem);
    var reason = '';
    if (!ready) {
      if (!app.configured) reason = '尚未配置应用，请先完成应用配置';
      else if (app.problem) reason = '应用配置无效：' + app.problem;
    }
    var card = el('div', { class: 'card platform' });
    card.appendChild(el('h2', null, p.name, ' ',
      methodBadge(p.active_method || p.method, p.form_label), ' ', verifiedBadge(p.verified)));
    card.appendChild(el('p', { class: 'driver-line' }, el('code', { text: p.driver })));
    card.appendChild(el('p', { class: 'app-line', text: appLineText(app) }));
    var notes = p.notes || [];
    if (notes.length) {
      card.appendChild(el('details', { class: 'notes' },
        el('summary', null, '说明'),
        el('ul', null, notes.map(function (n) { return el('li', { text: n }); }))));
    }
    var actions = el('div', { class: 'actions' });
    actions.appendChild(el('button', {
      class: 'btn btn-primary',
      disabled: !ready,
      title: reason,
      on: { click: function () { go('#/new/' + encodeURIComponent(p.id)); } },
      text: '添加'
    }));
    if (app.required) {
      actions.appendChild(el('button', {
        class: 'btn',
        on: { click: function () { go('#/app/' + encodeURIComponent(p.id)); } },
        text: '配置应用'
      }));
    }
    card.appendChild(actions);
    var grow = el('div', { class: 'grow' });
    card.insertBefore(grow, card.querySelector('.actions'));
    return card;
  }

  function renderHome() {
    return api('GET', 'admin/connect/platforms').then(function (data) {
      var wrap = el('div');
      wrap.appendChild(el('h1', { text: '添加网盘' }));
      wrap.appendChild(el('p', { class: 'muted', text: '选择网盘平台并完成授权，填写挂载路径后即可创建 OpenList 存储；完成后回到 cloud2api 点「刷新并纳管」。' }));
      var grid = el('div', { class: 'cards' });
      (data.platforms || []).forEach(function (p) { grid.appendChild(platformCard(p)); });
      wrap.appendChild(grid);
      wrap.appendChild(el('p', { class: 'more-link' },
        '其他网盘请在 ',
        el('a', { href: '../@manage/storages/add' }, 'OpenList 原生表单'),
        ' 中添加。'));
      if (returnUrl) {
        wrap.appendChild(el('p', { class: 'more-link' },
          el('a', { href: returnHref(null) }, '返回 cloud2api')));
      }
      var m = data.module || {};
      wrap.appendChild(el('p', {
        class: 'module-info',
        text: 'connect ' + (m.version || '?') + ' · OpenList ' + (m.upstream_version || '?') +
          ' (' + (m.upstream_commit || '?') + ')'
      }));
      return wrap;
    });
  }

  // ---------- 应用配置 ----------
  var appForm = null; // {platformId, mode, values…} 跨重绘保留

  function renderAppConfig(platformId) {
    return api('GET', 'admin/connect/platforms').then(function (data) {
      var p = (data.platforms || []).find(function (x) { return x.id === platformId; });
      if (!p) throw mkErr('平台不存在', 404, 'not_found');
      if (appForm === null || appForm.platformId !== platformId) {
        var app = p.app || {};
        var modes = p.app_modes || [];
        var mode = app.mode && modes.indexOf(app.mode) >= 0 ? app.mode : (modes[0] || '');
        appForm = {
          platformId: platformId,
          platform: p,
          module: data.module || {},
          callbackPath: data.callback_path || '',
          suggested: data.suggested_redirect_uri || '',
          modes: modes,
          mode: mode,
          saving: false,
          deleting: false
        };
      } else {
        appForm.platform = p;
        appForm.module = data.module || {};
        appForm.suggested = data.suggested_redirect_uri || '';
        appForm.callbackPath = data.callback_path || '';
      }
      return buildAppConfigPage();
    });
  }

  function defaultRedirect() {
    var f = appForm;
    var app = f.platform.app || {};
    return app.redirect_uri || f.suggested ||
      (f.callbackPath ? location.origin + f.callbackPath : '');
  }

  function buildAppConfigPage() {
    return withPersist(function () {
      var f = appForm;
      var p = f.platform;
      var app = p.app || {};
      var firstNoInput = modeHasDefault((p.app_modes || [])[0]);
      var card = el('div', { class: 'card' });
      card.appendChild(el('h1', { text: '配置应用 · ' + p.name }));
      card.appendChild(el('p', { class: 'driver-line' },
        el('code', { text: p.driver }), ' · ',
        methodBadge(p.active_method || p.method, p.form_label)));
      if (p.docs_url) {
        card.appendChild(el('p', null,
          el('a', { href: p.docs_url, target: '_blank', rel: 'noopener noreferrer' }, '接入说明')));
      }
      if (p.scopes && p.scopes.length) {
        card.appendChild(el('p', null, '请求的权限：', scopeChips(p.scopes)));
      } else {
        card.appendChild(el('p', { class: 'muted', text: '该平台未声明需要请求的权限。' }));
      }
      if (app.problem) card.appendChild(banner('warn', '应用配置无效：' + app.problem));
      if (app.default) {
        card.appendChild(el('p', { class: 'muted', text: '当前使用平台默认应用，尚未保存自定义配置。' }));
      }

      var form = el('form', { on: { submit: saveApp } });

      if (f.modes.length > 1) {
        var radios = el('div', { class: 'radio-row' });
        f.modes.forEach(function (m) {
          var input = el('input', {
            type: 'radio', name: 'app-mode', value: m, id: 'mode-' + m,
            checked: f.mode === m,
            on: { change: function () { f.mode = m; f.values = readAppFormValues(); state.appFormError = ''; paint(buildAppConfigPage()); } }
          });
          radios.appendChild(el('label', { for: 'mode-' + m }, input, ' ',
            appModeLabel(m, true), ' · ', methodLabel(modeMethodOf(p, m), p.form_label)));
        });
        card.appendChild(radios);
      }

      f.values = f.values || readAppFormDefaultValues();
      if (f.mode === 'own_app') {
        form.appendChild(fieldInput({
          id: 'app-client-id', label: 'client_id', value: app.client_id || '',
          help: '网盘开放平台颁发的应用 ID，不是保密信息。'
        }));
        form.appendChild(fieldInput({
          id: 'app-client-secret', label: 'client_secret', type: 'password',
          placeholder: app.client_secret_set ? '已保存，留空保持不变' : '',
          help: '应用密钥只写不读：保存后无法再从页面查看。'
        }));
        var rid = el('input', {
          id: 'app-redirect-uri', type: 'text', 'data-persist': 'app-redirect-uri',
          autocomplete: 'off'
        });
        rid.value = f.values['app-redirect-uri'] || defaultRedirect();
        form.appendChild(el('div', { class: 'field' },
          el('label', { for: 'app-redirect-uri', text: '回调地址（redirect_uri）' }),
          rid,
          el('div', { class: 'inline-btns' },
            el('button', {
              class: 'btn', type: 'button',
              on: { click: function () { rid.value = defaultRedirect(); } },
              text: '使用建议地址'
            }),
            el('button', {
              class: 'btn', type: 'button',
              on: { click: function () { copyText(rid.value, this); } },
              text: '复制'
            })),
          el('p', { class: 'help', text: '需要在网盘开放平台的应用设置中登记这个回调地址，必须完全一致。' }),
          p.allow_oob ? el('p', { class: 'help', text: '无法登记时可填写 oob，授权后手动粘贴授权码。' }) : null));
      } else if (f.mode === 'public_app') {
        form.appendChild(el('p', { class: 'app-line', text: p.public_app_label || '使用适配器内置的公共应用。' }));
        form.appendChild(el('p', { class: 'help', text: '无需填写；驱动直接向平台续期' }));
      } else if (f.mode === 'online_service') {
        form.appendChild(fieldInput({
          id: 'app-service-url', label: '在线授权服务地址', type: 'url',
          value: app.service_url || 'https://api.oplist.org',
          help: '授权以及之后的令牌续期都会经过该服务。'
        }));
      }

      if (state.appFormError) form.appendChild(banner('danger', state.appFormError));
      form.appendChild(el('div', { class: 'actions' },
        el('button', { class: 'btn btn-primary', type: 'submit', disabled: f.saving },
          f.saving ? '保存中…' : '保存'),
        (app.required && app.configured && !app.default) ? el('button', {
          class: 'btn btn-danger', type: 'button', disabled: f.deleting,
          on: { click: deleteApp },
          text: f.deleting
            ? (firstNoInput ? '恢复中…' : '删除中…')
            : (firstNoInput ? '恢复默认' : '删除配置')
        }) : null,
        el('a', { class: 'btn btn-ghost', href: '#/' }, '返回')));
      card.appendChild(form);
      return card;
    });
  }

  function readAppFormValues() {
    var vals = {};
    ['app-client-id', 'app-client-secret', 'app-redirect-uri', 'app-service-url'].forEach(function (id) {
      var n = document.getElementById(id);
      if (n) vals[id] = n.value;
    });
    return vals;
  }

  function readAppFormDefaultValues() { return {}; }

  function copyText(text, btn) {
    var done = function (ok) {
      if (!btn) return;
      var old = btn.textContent;
      btn.textContent = ok ? '已复制' : '复制失败，请手动选择文本';
      setTimeout(function () { btn.textContent = old; }, 1500);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(false); });
    } else {
      done(false);
    }
  }

  function saveApp(ev) {
    ev.preventDefault();
    var f = appForm;
    f.values = readAppFormValues();
    var body = {
      platform: f.platformId,
      mode: f.mode,
      client_id: (f.values['app-client-id'] || '').trim(),
      client_secret: (f.values['app-client-secret'] || '').trim(),
      redirect_uri: (f.values['app-redirect-uri'] || '').trim(),
      service_url: (f.values['app-service-url'] || '').trim()
    };
    f.saving = true;
    paint(buildAppConfigPage());
    api('POST', 'admin/connect/app/save', body).then(function () {
      appForm = null;
      state.notice = { kind: 'success', text: '应用配置已保存' };
      go('#/');
    }, function (e) {
      f.saving = false;
      state.appFormError = e.message || '保存失败。';
      paint(buildAppConfigPage());
    });
  }

  function deleteApp() {
    var f = appForm;
    var restoreDefault = modeHasDefault((f.platform.app_modes || [])[0]);
    var ask = restoreDefault
      ? '确定删除「' + f.platform.name + '」已保存的应用配置、恢复平台默认吗？已有存储不受影响。'
      : '确定删除「' + f.platform.name + '」的应用配置吗？已发起的会话和已有存储不受影响。';
    if (!window.confirm(ask)) return;
    f.deleting = true;
    paint(buildAppConfigPage());
    api('POST', 'admin/connect/app/delete', { platform: f.platformId }).then(function () {
      appForm = null;
      state.notice = { kind: 'info', text: restoreDefault ? '已恢复平台默认应用' : '应用配置已删除' };
      go('#/');
    }, function (e) {
      f.deleting = false;
      state.appFormError = e.message || '删除失败。';
      paint(buildAppConfigPage());
    });
  }

  // ---------- 会话：开始 / 恢复 / 轮询 ----------
  function beginSessionLocal(view) {
    state.sessionId = view.id;
    state.sessionView = view;
    state.storage = null;
    state.storageError = '';
    state.saveError = '';
    state.formError = '';
    state.saving = false;
    state.lastViewJson = '';
    try {
      // 只替换 hash：URL 里其余部分（含 ?mock=1 与可能的 base path）原样保留。
      history.replaceState(null, '', '#/s/' + encodeURIComponent(view.id));
    } catch (e) { /* 忽略 */ }
    showSession(view);
  }

  function startSessionCommon(body, platformId) {
    paint(el('div', { class: 'card' }, el('p', { class: 'muted', text: '正在创建登录会话…' })));
    api('POST', 'admin/connect/session/start', body).then(function (view) {
      beginSessionLocal(view);
    }, function (e) {
      if (e.code === 401) { clearToken(); renderGate({ reason: 'notoken' }); return; }
      var panel = el('div', { class: 'card' }, banner('danger', e.message || '创建会话失败。'));
      panel.appendChild(errorCodeLine(e));
      if (e.errorCode === 'app_missing' || e.errorCode === 'app_invalid') {
        panel.appendChild(el('div', { class: 'actions' },
          el('a', { class: 'btn btn-primary', href: '#/app/' + encodeURIComponent(platformId) }, '配置应用'),
          el('a', { class: 'btn btn-ghost', href: '#/' }, '返回首页')));
      } else {
        panel.appendChild(el('div', { class: 'actions' },
          el('a', { class: 'btn', href: '#/' }, '返回首页')));
      }
      paint(panel);
    });
  }

  function startCreate(platformId) {
    startSessionCommon({ platform: platformId }, platformId);
    return null;
  }

  function startReauth(platformId, storageId) {
    startSessionCommon({ platform: platformId, reauth_storage_id: storageId }, platformId);
  }

  function resumeSession(id) {
    state.sessionId = id;
    state.storage = null;
    state.storageError = '';
    state.saveError = '';
    state.formError = '';
    state.lastViewJson = '';
    return api('GET', 'admin/connect/session/get?id=' + encodeURIComponent(id)).then(function (view) {
      showSession(view);
    }, function (e) {
      if (e.code === 401) { clearToken(); renderGate({ reason: 'notoken' }); return; }
      var msg = e.code === 404 ? '会话不存在或已过期。' : (e.message || '无法获取会话。');
      var panel = el('div', { class: 'card' }, banner('danger', msg));
      panel.appendChild(errorCodeLine(e));
      panel.appendChild(el('div', { class: 'actions' },
        el('a', { class: 'btn btn-primary', href: '#/' }, '返回首页')));
      paint(panel);
    });
  }

  function startPolling() {
    stopPolling();
    state.pollTimer = setInterval(pollTick, POLL_MS);
  }

  function stopPolling() {
    if (state.pollTimer) {
      clearInterval(state.pollTimer);
      state.pollTimer = 0;
    }
  }

  function pollTick() {
    if (state.pollBusy || !state.sessionId) return;
    if (location.hash.indexOf('#/s/') !== 0) { stopPolling(); return; }
    state.pollBusy = true;
    api('GET', 'admin/connect/session/get?id=' + encodeURIComponent(state.sessionId)).then(function (view) {
      state.pollBusy = false;
      if (location.hash.indexOf('#/s/') === 0) showSession(view);
    }, function (e) {
      state.pollBusy = false;
      if (e.code === 401) { stopPolling(); clearToken(); renderGate({ reason: 'notoken' }); return; }
      if (e.code === 404) {
        stopPolling();
        paint(el('div', { class: 'card' }, banner('danger', '会话不存在或已过期。'),
          el('div', { class: 'actions' }, el('a', { class: 'btn btn-primary', href: '#/' }, '返回首页'))));
        return;
      }
      if (e.code === 410) {
        stopPolling();
        paint(el('div', { class: 'card' }, banner('warn', e.message || '会话已结束。'),
          el('div', { class: 'actions' }, el('a', { class: 'btn btn-primary', href: '#/' }, '返回首页'))));
        return;
      }
      // 其他错误（网络抖动等）：保留界面，继续轮询。
    });
  }

  function showSession(view) {
    state.sessionId = view.id;
    state.sessionView = view;
    var json = JSON.stringify(view);
    if (json === state.lastViewJson && !state.forceRender) return;
    var changed = json !== state.lastViewJson;
    state.lastViewJson = json;
    state.forceRender = false;
    if (changed && view.qr) announce(qrStatusText(view));
    if (changed && view.phase === 'saving') announce('正在保存并初始化驱动…');
    var page = withPersist(function () { return buildSessionPage(view); });
    paint(page);
    if (view.phase === 'authorized' && view.mode === 'create') maybeSuggestMount(view);
    if (view.phase === 'pending' || view.phase === 'saving') startPolling();
    else stopPolling();
  }

  // ---------- 会话页面 ----------
  function qrStatusText(v) {
    var qr = v.qr || {};
    switch (qr.status) {
      case 'waiting': return '请使用' + v.platform_name + ' App 扫描二维码';
      case 'scanned': return '已扫码，请在手机上确认登录';
      case 'confirmed': return '已确认，正在获取凭据…';
      case 'cancelled': return '已在手机上取消';
      case 'expired': return '二维码已过期';
      case 'network_error': return '网络错误，正在重试…' + (qr.message ? '（' + qr.message + '）' : '');
      default: return qr.message || qr.status || '';
    }
  }

  function qrStatusClass(s) {
    return 'status-line status-' + (s || 'waiting');
  }

  function sessionDefs(v) {
    return v.mode === 'reauth'
      ? ['登录授权', '确认保存', '完成']
      : ['登录授权', '存储设置', '完成'];
  }

  function sessionStepIndex(v) {
    switch (v.phase) {
      case 'pending': return 0;
      case 'authorized':
      case 'saving':
        // 授权完成后是合并的「确认并保存」一屏。
        return 1;
      default: return sessionDefs(v).length - 1;
    }
  }

  function buildSessionPage(v) {
    var defs = sessionDefs(v);
    var idx = sessionStepIndex(v);
    var body;
    switch (v.phase) {
      case 'pending': body = buildAuthStep(v); break;
      case 'authorized':
        body = v.mode === 'reauth' ? buildReauthConfirm(v) : buildCreateConfirm(v);
        break;
      case 'saving': body = buildSaving(v); break;
      case 'saved':
      case 'save_failed': body = buildResult(v); break;
      default: body = buildEnded(v); break;
    }
    return el('div', null,
      el('h1', { text: (v.mode === 'reauth' ? '重新授权 ' : '添加 ') + v.platform_name }),
      el('p', { class: 'driver-line' },
        el('code', { text: v.driver }), ' · ',
        methodBadge(v.method, v.form_label), ' ', verifiedBadge(v.verified)),
      buildSteps(defs, idx),
      body);
  }

  // ---- 第 1 步：登录授权 ----
  function buildAuthStep(v) {
    var card = el('div', { class: 'card' });
    card.appendChild(el('h2', { text: '登录授权' }));
    card.appendChild(el('p', { class: 'app-line' },
      appModeLabel(v.app.mode) + (v.app.identity ? ' ' + v.app.identity : '')));
    // 表单登录（含粘贴令牌）不由本页发起授权请求，权限由令牌来源决定。
    if (v.method !== 'form' && v.app.scopes && v.app.scopes.length) {
      card.appendChild(el('p', null, '请求的权限：', scopeChips(v.app.scopes)));
    }

    if (v.method === 'oauth') card.appendChild(buildOAuthPending(v));
    else if (v.method === 'qrcode') card.appendChild(buildQRPending(v));
    else if (v.method === 'form') card.appendChild(buildFormPending(v));
    else card.appendChild(banner('warn', '该平台使用了未知的登录方式：' + v.method));

    card.appendChild(el('div', { class: 'actions' },
      el('button', { class: 'btn', on: { click: cancelSession }, text: '取消' })));
    return card;
  }

  function buildOAuthPending(v) {
    var o = v.oauth || {};
    var box = el('div');
    var a = el('a', {
      class: 'btn btn-primary',
      href: o.authorize_url || '#',
      target: '_blank',
      rel: 'noopener noreferrer'
    }, '打开授权页面');
    box.appendChild(el('p', null, a));
    box.appendChild(el('p', { class: 'status-line status-waiting', role: 'status' },
      '等待授权完成…在新标签页完成授权后这里会自动继续。'));
    var fallback = el('details', { class: 'fallback' });
    if (o.oob) {
      fallback.appendChild(el('summary', null, '授权后页面会显示授权码，粘贴到这里'));
      var input = el('input', { id: 'oauth-code', type: 'text', 'data-persist': 'oauth-code', autocomplete: 'off', placeholder: '授权码' });
      fallback.appendChild(el('div', { class: 'field' }, el('label', { for: 'oauth-code', text: '授权码' }), input));
      fallback.appendChild(el('div', { class: 'actions' },
        el('button', { class: 'btn', on: { click: function () { submitSessionValues({ code: val('oauth-code') }); } }, text: '提交授权码' })));
    } else {
      fallback.appendChild(el('summary', null, '授权后浏览器停在无法打开的页面？把地址栏中的完整地址粘贴到这里'));
      var ta = el('textarea', { id: 'oauth-callback', 'data-persist': 'oauth-callback', placeholder: 'https://…/api/connect/oauth/callback?code=…&state=…' });
      fallback.appendChild(el('div', { class: 'field' }, el('label', { for: 'oauth-callback', text: '完整回调地址' }), ta));
      fallback.appendChild(el('div', { class: 'actions' },
        el('button', { class: 'btn', on: { click: function () { submitSessionValues({ callback_url: val('oauth-callback') }); } }, text: '提交回调地址' })));
    }
    if (state.formError) fallback.appendChild(banner('danger', state.formError));
    box.appendChild(fallback);
    return box;
  }

  function buildQRPending(v) {
    var qr = v.qr || {};
    var box = el('div', { class: qr.status === 'expired' ? 'qr-box qr-stale' : 'qr-box' });
    if (qr.image_url) {
      var img = el('img', {
        class: 'qr-img',
        src: qr.image_url,
        alt: '登录二维码',
        referrerpolicy: 'no-referrer'
      });
      img.addEventListener('error', function () {
        box.textContent = '';
        box.appendChild(el('p', { class: 'muted', text: '二维码图片加载失败。' }));
        if (qr.content) box.appendChild(el('p', { class: 'qr-content', text: qr.content }));
      });
      box.appendChild(img);
    } else if (qr.content) {
      box.appendChild(el('p', { class: 'qr-content', text: qr.content }));
      box.appendChild(el('p', { class: 'help', text: '请用网盘 App 扫描此内容生成的二维码' }));
    } else {
      box.appendChild(el('p', { class: 'muted', text: '网盘未返回二维码。' }));
    }
    var status = el('p', { class: qrStatusClass(qr.status), role: 'status', text: qrStatusText(v) });
    var wrap = el('div', null, box, status);
    // 等待/已扫码等状态下也展示网盘提示（如设备码备选入口）；network_error
    // 的信息已并入状态行，避免重复。
    if (qr.message && qr.status !== 'network_error') {
      wrap.appendChild(el('p', { class: 'qr-message muted', text: qr.message }));
    }
    if (qr.can_refresh && (qr.status === 'expired' || qr.status === 'network_error')) {
      wrap.appendChild(el('div', { class: 'actions' },
        el('button', { class: 'btn', on: { click: refreshQR }, text: '刷新二维码' })));
    }
    if (state.formError) wrap.appendChild(banner('danger', state.formError));
    return wrap;
  }

  function buildFormPending(v) {
    var box = el('form', { on: { submit: submitFormLogin } });
    // FormGuider：提交字段之前先到指引页面获取要粘贴的内容。
    if (v.guide && v.guide.url) {
      box.appendChild(el('div', { class: 'guide-box' },
        el('a', {
          class: 'btn btn-primary',
          href: v.guide.url,
          target: '_blank',
          rel: 'noopener noreferrer'
        }, v.guide.label || '打开授权页面'),
        (v.guide.steps && v.guide.steps.length)
          ? el('ol', { class: 'guide-steps' },
            v.guide.steps.map(function (s) { return el('li', { text: s }); }))
          : null));
    }
    (v.form || []).forEach(function (f) {
      var id = 'sf-' + f.key;
      var secret = f.type === 'password' || f.secret;
      var type = secret ? 'password' : (f.type === 'url' ? 'url' : 'text');
      var input;
      if (f.type === 'select' && f.options && f.options.length) {
        input = el('select', { id: id, 'data-persist': id },
          f.options.map(function (o) { return el('option', { value: o, text: o }); }));
      } else {
        // 密钥字段只写：type=password、autocomplete=off，且不进持久化机制
        //（无 data-persist，重绘后需重新输入）。
        input = el('input', {
          id: id, type: type,
          'data-persist': secret ? null : id,
          autocomplete: 'off'
        });
      }
      if (f.default) input.value = f.default;
      box.appendChild(el('div', { class: 'field' },
        el('label', { for: id, text: f.label + (f.required ? ' *' : '') }),
        input,
        f.help ? el('p', { class: 'help', text: f.help }) : null));
    });
    if (state.formError) box.appendChild(banner('danger', state.formError));
    box.appendChild(el('div', { class: 'actions' },
      el('button', { class: 'btn btn-primary', type: 'submit', text: v.form_label ? '提交' : '登录' })));
    return box;
  }

  function submitFormLogin(ev) {
    ev.preventDefault();
    var values = {};
    (state.sessionView.form || []).forEach(function (f) {
      var n = document.getElementById('sf-' + f.key);
      values[f.key] = n ? n.value : '';
    });
    submitSessionValues(values);
  }

  function submitSessionValues(values) {
    api('POST', 'admin/connect/session/submit', { id: state.sessionId, values: values }).then(function (view) {
      state.formError = '';
      state.lastViewJson = '';
      showSession(view);
    }, function (e) {
      if (e.code === 401) { clearToken(); renderGate({ reason: 'notoken' }); return; }
      state.formError = e.message || '提交失败。';
      state.forceRender = true;
      showSession(state.sessionView);
    });
  }

  function refreshQR() {
    api('POST', 'admin/connect/session/refresh', { id: state.sessionId }).then(function (view) {
      state.formError = '';
      state.lastViewJson = '';
      showSession(view);
    }, function (e) {
      state.formError = e.message || '刷新二维码失败。';
      state.forceRender = true;
      showSession(state.sessionView);
    });
  }

  function cancelSession() {
    api('POST', 'admin/connect/session/cancel', { id: state.sessionId }).then(function () {
      go('#/');
    }, function () {
      go('#/');
    });
  }

  // ---- 授权失败 / 取消 / 过期 ----
  function buildEnded(v) {
    var label = v.phase === 'cancelled' ? '会话已取消' : v.phase === 'expired' ? '会话已过期' : '登录失败';
    var card = el('div', { class: 'card' });
    card.appendChild(el('h2', { text: label }));
    card.appendChild(banner('danger', v.error || '未能完成登录。'));
    if (v.error_code) card.appendChild(el('p', { class: 'error-code', text: 'error_code：' + v.error_code }));
    card.appendChild(el('div', { class: 'actions' },
      el('button', {
        class: 'btn btn-primary',
        on: { click: function () { restartSession(v); } },
        text: '重新开始'
      }),
      el('a', { class: 'btn btn-ghost', href: '#/' }, '返回')));
    return card;
  }

  function restartSession(v) {
    var body = { platform: v.platform };
    if (v.mode === 'reauth' && v.target) body.reauth_storage_id = v.target.storage_id;
    startSessionCommon(body, v.platform);
  }

  // ---- 授权成功：确认并保存（创建 / 重新授权各一屏） ----
  function summaryRows(v, a) {
    var rows = [];
    if (v.mode === 'reauth' && v.target) {
      rows.push(['目标存储', '#' + v.target.storage_id + ' · ' + v.target.mount_path]);
    }
    rows.push(['凭据类型', credLabel(a.credential_type)]);
    rows.push(['应用', appModeLabel(a.app_mode) + (a.app_identity ? ' ' + a.app_identity : '')]);
    if (a.account) rows.push(['账号', a.account]);
    if (a.renewal) rows.push(['续期方式', a.renewal]);
    rows.push(['将写入的字段', (a.fields || []).join('、') || '—']);
    if (a.create_fields && a.create_fields.length) {
      rows.push(['仅创建时写入', a.create_fields.join('、')]);
    }
    return rows;
  }

  function savingNote() {
    return el('p', { class: 'status-line status-confirming', role: 'status' },
      '正在保存并初始化驱动…（可能需要几十秒）');
  }

  // 创建：凭据摘要 + 存储设置 + 保存，一屏完成。
  function buildCreateConfirm(v) {
    var a = v.auth || {};
    var st = state.storage || { mount_path: '', root: (v.root && v.root.default) || '', remark: '' };
    var card = el('div', { class: 'card' });
    card.appendChild(el('h2', { text: '确认并保存' }));
    card.appendChild(kvTable(summaryRows(v, a)));
    if (state.storageError) card.appendChild(banner('danger', state.storageError));
    if (state.saveError) card.appendChild(banner('danger', state.saveError));
    var form = el('form', { on: { submit: submitCreate } });
    form.appendChild(fieldInput({
      id: 'st-mount', label: '挂载路径（存储名称）', value: st.mount_path,
      placeholder: '/' + v.platform_name,
      help: 'OpenList 中显示的路径，默认按平台名称填写，可以修改；不能与已有存储重复。'
    }));
    form.appendChild(fieldInput({
      id: 'st-root', label: (v.root && v.root.label) || '根目录',
      value: st.root, help: (v.root && v.root.help) || ''
    }));
    form.appendChild(fieldInput({ id: 'st-remark', label: '备注', value: st.remark, placeholder: '可选' }));
    form.appendChild(el('div', { class: 'actions' },
      el('button', { class: 'btn btn-primary', type: 'submit', disabled: state.saving },
        state.saving ? '正在保存…' : '保存'),
      el('button', {
        class: 'btn btn-ghost', type: 'button', disabled: state.saving,
        on: { click: cancelSession }, text: '取消'
      })));
    card.appendChild(form);
    if (state.saving) {
      card.appendChild(savingNote());
      announce('正在保存并初始化驱动…');
    }
    return card;
  }

  // 重新授权：凭据摘要 + 目标存储 + 提示 + 保存，一屏完成。
  function buildReauthConfirm(v) {
    var a = v.auth || {};
    var card = el('div', { class: 'card' });
    card.appendChild(el('h2', { text: '确认并保存' }));
    card.appendChild(kvTable(summaryRows(v, a)));
    card.appendChild(el('p', {
      class: 'next-hint',
      text: '只替换登录凭据与续期参数，挂载点、根目录和其他设置保持不变。'
    }));
    if (state.saveError) card.appendChild(banner('danger', state.saveError));
    card.appendChild(el('div', { class: 'actions' },
      el('button', {
        class: 'btn btn-primary', disabled: state.saving,
        on: { click: doSave },
        text: state.saving ? '正在保存…' : '保存'
      })));
    if (state.saving) {
      card.appendChild(savingNote());
      announce('正在保存并初始化驱动…');
    }
    return card;
  }

  function submitCreate(ev) {
    ev.preventDefault();
    var mount = val('st-mount');
    if (mount.indexOf('/') !== 0 || mount === '/') {
      state.storageError = '挂载路径必须以 / 开头，且不能只是 /。';
      state.forceRender = true;
      showSession(state.sessionView);
      return;
    }
    state.storage = { mount_path: mount, root: val('st-root'), remark: val('st-remark') };
    state.storageError = '';
    state.saveError = '';
    doSave();
  }

  // 挂载路径预填：本会话首次渲染「确认并保存」且输入框为空（也无持久化值）
  // 时，取服务器的建议路径；用户已输入则绝不覆盖，请求失败静默忽略。
  var mountSuggestFor = '';
  function maybeSuggestMount(v) {
    if (mountSuggestFor === v.id) return;
    mountSuggestFor = v.id;
    var input = document.getElementById('st-mount');
    if (!input || input.value.trim() !== '') return;
    api('GET', 'admin/connect/mount/suggest?platform=' + encodeURIComponent(v.platform))
      .then(function (d) {
        var cur = document.getElementById('st-mount');
        if (cur && cur.value.trim() === '' && d && d.mount_path) cur.value = d.mount_path;
      }, function () { /* 建议不可用时让管理员自己填写 */ });
  }

  function doSave() {
    var v = state.sessionView;
    state.saving = true;
    state.saveError = '';
    state.forceRender = true;
    showSession(v);
    var body;
    if (v.mode === 'reauth') {
      body = { id: v.id };
    } else {
      var st = state.storage || {};
      body = {
        id: v.id,
        mount_path: st.mount_path || '',
        root: st.root || (v.root && v.root.default) || '',
        remark: st.remark || ''
      };
    }
    api('POST', 'admin/connect/session/save', body).then(function (nv) {
      state.saving = false;
      state.sessionView = nv;
      state.lastViewJson = '';
      if (nv.phase === 'authorized' && nv.result && nv.result.retryable) {
        // 没有写入任何内容：留在本屏修改后重试。
        state.storageError = nv.result.message || '保存被拒绝，请修改后重试。';
      }
      showSession(nv);
    }, function (e) {
      state.saving = false;
      if (e.code === 401) { clearToken(); renderGate({ reason: 'notoken' }); return; }
      state.saveError = e.message || '保存失败。';
      state.forceRender = true;
      showSession(state.sessionView);
    });
  }

  function buildSaving(v) {
    // 页面在保存进行中被打开后会走到这里：继续轮询直到出现结果。
    return el('div', { class: 'card' },
      el('h2', { text: '正在保存' }),
      el('p', { class: 'status-line status-confirming', role: 'status' },
        '正在保存并初始化驱动…（可能需要几十秒）'));
  }

  // ---- 第 3 步：结果 ----
  function buildResult(v) {
    var r = v.result || {};
    var card = el('div', { class: 'card' });
    if (v.phase === 'saved') {
      card.appendChild(el('h2', { text: '保存成功' }));
      card.appendChild(banner('success', r.message || '存储已保存。'));
      if (r.storage_id) {
        card.appendChild(kvTable([
          ['存储', '#' + r.storage_id],
          ['挂载路径', r.mount_path || '—'],
          ['状态', r.status || '—']
        ]));
      }
      var links = el('div', { class: 'actions' });
      if (returnUrl) {
        // 优先回 cloud2api：占位符换成新存储 ID（没有则留空）。
        card.appendChild(el('p', {
          class: 'next-hint',
          text: v.mode === 'create'
            ? '点「在 cloud2api 中查看」会刷新网盘列表并打开纳管对话框，网盘 ID 已自动填写。'
            : '点「在 cloud2api 中查看」会刷新该网盘的状态。'
        }));
        links.appendChild(el('a', {
          class: 'btn btn-primary',
          href: returnHref(r.storage_id)
        }, '在 cloud2api 中查看'));
        if (r.storage_id) {
          links.appendChild(el('a', {
            class: 'btn',
            href: '../@manage/storages/edit/' + r.storage_id
          }, '在 OpenList 中查看'));
        }
        links.appendChild(el('a', { class: 'btn btn-ghost', href: '#/' }, '继续添加'));
      } else {
        card.appendChild(el('p', { class: 'next-hint', text: nextHintText(v.mode) }));
        if (r.storage_id) {
          links.appendChild(el('a', {
            class: 'btn btn-primary',
            href: '../@manage/storages/edit/' + r.storage_id
          }, '在 OpenList 中查看'));
        }
        links.appendChild(el('a', { class: 'btn', href: '#/' }, '继续添加'));
      }
      card.appendChild(links);
    } else {
      card.appendChild(el('h2', { text: '保存失败' }));
      card.appendChild(banner('danger', r.message || '保存失败。'));
      if (r.error) card.appendChild(el('pre', { class: 'err-block', text: r.error }));
      var facts = resultFacts(r);
      if (facts.length) {
        card.appendChild(el('ul', { class: 'fact-list' }, facts.map(function (f) {
          return el('li', { class: f.cls, text: f.text });
        })));
      }
      var actions = el('div', { class: 'actions' },
        el('button', {
          class: 'btn btn-primary',
          on: { click: function () { restartSession(v); } },
          text: '重新开始'
        }));
      if (returnUrl) {
        actions.appendChild(el('a', { class: 'btn', href: returnHref(null) }, '返回 cloud2api'));
      }
      actions.appendChild(el('a', { class: 'btn btn-ghost', href: '#/' }, '返回首页'));
      card.appendChild(actions);
    }
    return card;
  }

  function nextHintText(mode) {
    return mode === 'reauth'
      ? '回到 cloud2api 刷新状态；网盘映射与 Key 授权保持不变。'
      : '回到 cloud2api 点「刷新并纳管」，再为需要访问它的 API Key 授权。';
  }

  function resultFacts(r) {
    var facts = [];
    if (r.cleanup) {
      if (r.cleanup.deleted) {
        facts.push({ cls: 'fact-ok', text: '已删除本次创建的存储 #' + r.cleanup.storage_id });
      } else if (r.cleanup.attempted) {
        facts.push({
          cls: 'fact-danger',
          text: '清理失败：请在 OpenList 中手动删除存储 #' + r.cleanup.storage_id +
            (r.cleanup.error ? '（' + r.cleanup.error + '）' : '')
        });
      }
    }
    if (r.restore) {
      if (r.restore.restored) {
        facts.push({ cls: 'fact-ok', text: '已恢复原配置' + (r.restore.status ? '，当前状态：' + r.restore.status : '') });
      } else {
        facts.push({
          cls: 'fact-danger',
          text: '未能恢复原配置' + (r.restore.error ? '：' + r.restore.error : '') +
            (r.restore.status ? '（当前状态：' + r.restore.status + '）' : '')
        });
      }
    }
    if (r.credentials_written) {
      facts.push({ cls: 'fact-warn', text: '新凭据曾写入该存储' });
    }
    return facts;
  }

  // ---------- 重新授权入口 ----------
  function renderReauth(storageId) {
    return api('GET', 'admin/connect/target?id=' + encodeURIComponent(storageId)).then(function (t) {
      var card = el('div', { class: 'card' });
      card.appendChild(el('h1', { text: '重新授权存储 #' + t.storage_id }));
      card.appendChild(kvTable([
        ['挂载路径', t.mount_path || '—'],
        ['驱动', el('code', { text: t.driver || '—' })],
        ['状态', statusBadge(t.status)]
      ]));
      var plats = t.platforms || [];
      if (!plats.length) {
        card.appendChild(banner('warn', '该存储的驱动 ' + (t.driver || '') + ' 没有登录适配器。'));
        // edit_path 是绝对路径，已包含 OpenList 的 base path。
        card.appendChild(el('p', null,
          el('a', { href: t.edit_path || '../@manage/storages' }, '在 OpenList 中编辑')));
        return card;
      }
      var usable = [];
      plats.forEach(function (p) {
        if (p.problem) {
          card.appendChild(banner('warn', p.name + '：' + p.problem));
          if (/应用/.test(p.problem)) {
            card.appendChild(el('p', null,
              el('a', { href: '#/app/' + encodeURIComponent(p.id) }, '配置应用')));
          }
        } else {
          usable.push(p);
        }
      });
      if (!usable.length) {
        card.appendChild(el('p', { class: 'muted', text: '暂时没有可用的登录方式，请先解决上方的问题。' }));
      } else if (usable.length === 1) {
        card.appendChild(el('p', { class: 'muted', text: '将使用「' + usable[0].name + '」重新登录该存储。' }));
        card.appendChild(el('div', { class: 'actions' },
          el('button', {
            class: 'btn btn-primary',
            on: { click: function () { startReauth(usable[0].id, t.storage_id); } },
            text: '开始重新授权'
          })));
      } else {
        card.appendChild(el('p', { class: 'muted', text: '选择要使用的登录方式：' }));
        var acts = el('div', { class: 'actions' });
        usable.forEach(function (p) {
          acts.appendChild(el('button', {
            class: 'btn btn-primary',
            on: { click: function () { startReauth(p.id, t.storage_id); } },
            text: p.name
          }));
        });
        card.appendChild(acts);
      }
      return card;
    }, function (e) {
      if (e.code === 404) {
        var panel = el('div', { class: 'card' }, banner('danger', '存储不存在。'));
        panel.appendChild(el('div', { class: 'actions' },
          el('a', { class: 'btn btn-primary', href: '#/' }, '返回首页')));
        return panel;
      }
      throw e;
    });
  }

  // ---------- 通用错误页 ----------
  function renderErrorPanel(e) {
    var panel = el('div', { class: 'card' },
      banner('danger', (e && e.message) || '请求失败。'));
    panel.appendChild(errorCodeLine(e));
    panel.appendChild(el('div', { class: 'actions' },
      el('a', { class: 'btn btn-primary', href: '#/' }, '返回首页')));
    return panel;
  }

  function renderMockMissing() {
    paint(el('div', { class: 'card' },
      banner('warn', '已指定 ?mock=1，但 mock.js 不存在或加载失败。生产构建不包含该文件；请去掉 ?mock=1，或在开发环境使用。'),
      el('div', { class: 'actions' },
        el('a', { class: 'btn btn-primary', href: location.pathname }, '不使用 mock 重新打开'))));
  }

  // ---------- 免登录进入（cloud2api 移交） ----------
  // 入口形如 /@connect/?return=…#handoff=<一次性票据>&target=%2Fnew%2F平台。
  // 票据一次性、60 秒内有效、恰好 32 字节 base64url（43 字符），只活在 URL
  // fragment 里（不会发给服务器、不进 Referer）；页面读取后立即用
  // history.replaceState 把 fragment 从地址栏移除——即使内容畸形也先移除再
  // 报错——再兑换成 HttpOnly 的连接会话 Cookie。本地不保存票据，也不保存任
  // 何会话令牌；服务端只回传用户名等元数据。
  // 门户契约：target 是不带 # 的路由路径（'/'、'/new/<平台>'、
  // '/app/<平台>'、'/reauth/<正数storage id>'，百分号编码）。
  function normalizeHandoffTarget(raw) {
    if (!raw) return '';
    if (raw.charAt(0) === '#') raw = raw.slice(1);
    if (raw.length > 256) return '';
    if (raw === '/') return '#/';
    if (/^\/(new|app)\/[A-Za-z0-9._-]{1,64}$/.test(raw)) return '#' + raw;
    var id = /^\/reauth\/([1-9][0-9]{0,18})$/.exec(raw);
    if (id && (id[1].length < 19 || id[1] <= '9223372036854775807')) return '#' + raw;
    return '';
  }

  // 返回 null 表示地址里没有移交链接；{invalid:true} 表示有但格式非法。
  function parseHandoff() {
    var h = location.hash || '';
    // 裸的 #handoff（没有 =）也算移交入口：同样先移除、再显式报错。
    if (h !== '#handoff' && h.indexOf('#handoff=') !== 0) return null;
    var params = {};
    var malformed = false;
    h.slice(1).split('&').forEach(function (kv) {
      var i = kv.indexOf('=');
      var k = i > 0 ? kv.slice(0, i) : kv;
      var v = i > 0 ? kv.slice(i + 1) : '';
      if (k in params) { malformed = true; return; }
      try { params[k] = decodeURIComponent(v); } catch (e) { malformed = true; }
    });
    var code = params.handoff || '';
    if (malformed || !/^[A-Za-z0-9_-]{43}$/.test(code)) return { invalid: true };
    var target = normalizeHandoffTarget(params.target || '/');
    if (!target) return { invalid: true };
    return { code: code, target: target };
  }

  function enterViaHandoff(ho) {
    paint(el('div', { class: 'card' },
      el('h1', { text: '正在进入网盘连接…' }),
      el('p', { class: 'muted', text: '正在使用 cloud2api 的管理员授权进入，无需再次输入 OpenList 密码。' })));
    api('POST', 'connect/handoff/exchange', { code: ho.code }).then(function () {
      state.lastScope = 'connect';
      state.token = '';
      // 用 admin/connect/auth/me 确认 Cookie 已生效并取回用户名，然后进入
      // 目标路由。
      api('GET', 'admin/connect/auth/me').then(function (me) {
        if (me && me.role === 2) {
          state.me = me;
          state.notice = {
            kind: 'success',
            text: '已通过 cloud2api 进入（OpenList 管理员：' + (me.username || '?') + '），无需再次登录。'
          };
        }
        go(ho.target || '#/');
      }, function () {
        go(ho.target || '#/'); // 兜底：交给 ensureAuth 给出明确的登录提示
      });
    }, function (e) {
      renderHandoffError(e);
    });
  }

  function renderHandoffError(e) {
    stopPolling();
    state.me = null;
    var reason;
    if (e === null || e === undefined) reason = '移交链接格式无效，请回到 cloud2api 重新点「添加网盘」。';
    else if (e.code === 410) reason = '移交链接已失效或已被使用（一次性，60 秒内有效）。';
    else if (e.code === 403) reason = '进入请求被拒绝（来源校验未通过）。';
    else reason = (e && e.message) || '进入失败。';
    var card = el('div', { class: 'card gate' },
      el('h1', { text: '免登录进入失败' }),
      banner('danger', reason),
      el('p', { class: 'muted', text: '请回到 cloud2api 重新点「添加网盘」获取新的进入链接，或在下方直接登录 OpenList。' }));
    var actions = el('div', { class: 'actions' });
    if (returnUrl) {
      actions.appendChild(el('a', { class: 'btn btn-primary', href: returnHref(null) }, '返回 cloud2api 重新进入'));
    }
    actions.appendChild(el('button', { class: 'btn', on: { click: function () { route(); } }, text: '手动登录 OpenList' }));
    card.appendChild(actions);
    paint(card);
  }

  // ---------- 路由 ----------
  function route() {
    stopPolling();
    if (state.mock && !state.mockLoaded) { renderMockMissing(); return; }
    ensureAuth().then(function (auth) {
      if (!auth.ok) { renderGate(auth); return; }
      var parts = (location.hash || '#/').replace(/^#\/?/, '').split('/')
        .map(function (s) { try { return decodeURIComponent(s); } catch (e) { return s; } });
      var seg = parts[0] || '';
      var p;
      if (seg === '') {
        p = renderHome();
      } else if (seg === 'app' && parts[1]) {
        p = renderAppConfig(parts[1]);
      } else if (seg === 'new' && parts[1]) {
        return void startCreate(parts[1]);
      } else if (seg === 'reauth' && parts[1]) {
        p = renderReauth(parts[1]);
      } else if (seg === 's' && parts[1]) {
        return void resumeSession(parts[1]);
      } else {
        p = Promise.resolve(renderErrorPanel(mkErr('页面不存在', 404, 'not_found')));
      }
      Promise.resolve(p).then(function (node) {
        if (node) paint(node);
      }, function (e) {
        if (e && e.code === 401) { clearToken(); renderGate({ reason: 'notoken' }); return; }
        paint(renderErrorPanel(e));
      });
    });
  }

  // ---------- 启动 ----------
  function boot() {
    state.sha256ok = sha256Hex('abc') === SHA256_ABC;
    if (!state.sha256ok) {
      try { console.error('[connect] SHA-256 自检失败'); } catch (e) { /* 忽略 */ }
    }
    window.addEventListener('hashchange', route);
    if (state.mock) {
      var s = document.createElement('script');
      s.src = 'mock.js';
      s.onload = function () { state.mockLoaded = true; route(); };
      s.onerror = function () { state.mockLoaded = false; route(); };
      document.head.appendChild(s);
      return;
    }
    var ho = parseHandoff();
    if (ho !== null) {
      // 先把票据（无论有效与否）从地址栏移除——不写入历史、刷新后不再出现
      // ——再兑换或报错。
      history.replaceState(null, '', location.pathname + location.search + '#/');
      if (ho.invalid) renderHandoffError(null);
      else enterViaHandoff(ho);
      return;
    }
    route();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
