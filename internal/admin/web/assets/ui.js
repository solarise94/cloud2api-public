/* cloud2api admin — DOM helpers & shared UI components.
 * Everything is built with createElement/textContent; server data never goes
 * through innerHTML. */
(function () {
  'use strict';
  const C2A = (window.C2A = window.C2A || {});

  /* ---------- element builder ---------- */
  function h(tag, attrs) {
    const el = document.createElement(tag);
    if (attrs) {
      for (const k of Object.keys(attrs)) {
        const v = attrs[k];
        if (v === null || v === undefined || v === false) continue;
        if (k === 'class') el.className = v;
        else if (k === 'dataset') Object.assign(el.dataset, v);
        else if (k.slice(0, 2) === 'on' && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
        else if (v === true) el.setAttribute(k, '');
        else el.setAttribute(k, String(v));
      }
    }
    for (let i = 2; i < arguments.length; i++) appendChild(el, arguments[i]);
    return el;
  }
  function appendChild(el, c) {
    if (c === null || c === undefined || c === false || c === true) return;
    if (Array.isArray(c)) { for (const x of c) appendChild(el, x); return; }
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); }

  C2A.preference = {
    get(key, fallback) { try { return localStorage.getItem('c2a-ui-' + key) || fallback; } catch (e) { return fallback; } },
    set(key, value) { try { localStorage.setItem('c2a-ui-' + key, value); } catch (e) { /* private browsing */ } },
  };

  /* ---------- icons (static, trusted markup only) ---------- */
  const ICONS = {
    overview: '<path d="M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z"/>',
    drives: '<path d="M17.5 19a4.5 4.5 0 1 0-.42-8.98 6 6 0 0 0-11.6 1.6A4 4 0 0 0 6 19.5h11.5z"/>',
    keys: '<path d="M21 2l-2 2m-7.6 7.6a5.5 5.5 0 1 1-7.78 7.78 5.5 5.5 0 0 1 7.78-7.78zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/>',
    requests: '<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>',
    audit: '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6M16 13H8M16 17H8M10 9H8"/>',
    openlist: '<rect x="2" y="2" width="20" height="8" rx="2"/><rect x="2" y="14" width="20" height="8" rx="2"/><path d="M6 6h.01M6 18h.01"/>',
    settings: '<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
    refresh: '<path d="M23 4v6h-6M1 20v-6h6"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/>',
    close: '<path d="M18 6 6 18M6 6l12 12"/>',
    alert: '<path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><path d="M12 9v4M12 17h.01"/>',
    info: '<circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/>',
    check: '<path d="M20 6 9 17l-5-5"/>',
    xcircle: '<circle cx="12" cy="12" r="10"/><path d="m15 9-6 6M9 9l6 6"/>',
    copy: '<rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/>',
    external: '<path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><path d="M15 3h6v6M10 14 21 3"/>',
    dots: '<circle cx="12" cy="5" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="12" cy="19" r="1"/>',
    trash: '<path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2m3 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M10 11v6M14 11v6"/>',
    power: '<path d="M18.36 6.64a9 9 0 1 1-12.73 0M12 2v10"/>',
    rotate: '<path d="M23 4v6h-6"/><path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"/>',
    logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5M21 12H9"/>',
    menu: '<path d="M3 12h18M3 6h18M3 18h18"/>',
    login2: '<path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4"/><path d="m10 17 5-5-5-5M15 12H3"/>',
    inbox: '<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>',
    sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5 19 19M5 19l1.5-1.5M17.5 6.5 19 5"/>',
    moon: '<path d="M21 12.8A9 9 0 0 1 11.2 3 9 9 0 1 0 21 12.8z"/>',
    collapse: '<path d="m14 7-5 5 5 5"/>',
    grid: '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',
    list: '<path d="M9 6h12M9 12h12M9 18h12M3 6h.01M3 12h.01M3 18h.01"/>',
    search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  };
  function icon(name, size) {
    size = size || 16;
    const span = document.createElement('span');
    span.className = 'ico';
    span.style.display = 'inline-flex';
    span.innerHTML = '<svg width="' + size + '" height="' + size + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + (ICONS[name] || '') + '</svg>';
    return span.firstChild ? span : span; // span contains svg
  }
  function svgEl(name, size) {
    size = size || 40;
    const wrap = document.createElement('span');
    wrap.innerHTML = '<svg width="' + size + '" height="' + size + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + (ICONS[name] || '') + '</svg>';
    return wrap.firstChild;
  }

  /* ---------- formatting ---------- */
  function fmtBytes(n) {
    if (n === null || n === undefined || n === '') return '—';
    n = Number(n);
    if (!isFinite(n)) return '—';
    if (n === 0) return '0 B';
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    let i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(n >= 100 ? 0 : n >= 10 ? 1 : 2)) + ' ' + units[i];
  }
  function fmtTime(iso) {
    if (!iso) return '—';
    const d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso);
    const p = (x) => String(x).padStart(2, '0');
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }
  function fmtAgo(iso) {
    if (!iso) return '从未使用';
    const ms = Date.now() - new Date(iso).getTime();
    if (isNaN(ms)) return '—';
    const s = Math.floor(ms / 1000);
    if (s < 60) return '刚刚';
    const m = Math.floor(s / 60);
    if (m < 60) return m + ' 分钟前';
    const hr = Math.floor(m / 60);
    if (hr < 24) return hr + ' 小时前';
    const d = Math.floor(hr / 24);
    if (d < 30) return d + ' 天前';
    return fmtTime(iso);
  }
  function fmtDuration(ms) {
    if (ms === null || ms === undefined) return '—';
    if (ms < 1000) return ms + ' ms';
    return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + ' s';
  }

  /* ---------- labels & badges ---------- */
  const DRIVE_STATE = {
    healthy: { label: '正常', cls: 'badge-ok' },
    needs_reauth: { label: '需重新登录', cls: 'badge-warn' },
    error: { label: '错误', cls: 'badge-danger' },
    disabled: { label: '已停用', cls: 'badge-slate' },
    orphaned: { label: '存储已删除', cls: 'badge-violet' },
    replacement_found: { label: '待确认替代存储', cls: 'badge-accent' },
  };
  function stateBadge(state) {
    const m = DRIVE_STATE[state] || { label: state, cls: 'badge-slate' };
    return h('span', { class: 'badge ' + m.cls }, h('span', { class: 'dot' }), m.label);
  }

  /** One-line explanation of what a drive state means for the agent, plus
   * where it came from. `d` is a drives row: `enabled` is cloud2api's own
   * switch (expose to agents or not), `storage_disabled` says the OpenList
   * storage itself is disabled in OpenList. `state` is "disabled" for either
   * cause. */
  function stateDesc(d) {
    const st = String(d.status_text || '');
    switch (d.state) {
      case 'healthy':
        return '存储工作正常';
      case 'needs_reauth':
        // with a login adapter (openlist_reauth_url) the wizard's 重新授权 is
        // the fix; without it the native OpenList form is the only way
        if (d.openlist_reauth_url) {
          return '登录凭据已失效。点「重新授权」在 OpenList 登录向导中更新凭据；存储 ID、挂载点与 Key 授权保持不变；完成后点「刷新并纳管」更新状态。';
        }
        return '登录凭据已失效，/v1 访问会收到 503 drive_unavailable；请在 OpenList 中更新该网盘的登录凭据，状态正常后回到这里点「刷新并纳管」，ID 与 Key 授权保持不变';
      case 'error':
        return '上游返回错误，/v1 访问会失败；原因：' + (st || '未知');
      case 'disabled':
        if (d.storage_disabled && d.enabled === false) {
          return '已在 OpenList 中停用，且已在 cloud2api 中停用：/v1 返回 503 drive_unavailable；请在 OpenList 中启用该存储，并在此处启用网盘';
        }
        if (d.storage_disabled) {
          return '该存储在 OpenList 后台被停用：/v1 返回 503 drive_unavailable；请在 OpenList 中启用该存储';
        }
        return '该网盘已在 cloud2api 中停用：Key 授权与路由保留，但 agent 无法再通过 cloud2api 访问（/v1 返回 503）；点击「启用」即可恢复。OpenList 中的存储不受影响';
      case 'orphaned':
        return 'OpenList 中已没有该网盘对应的存储（可能已被删除）。/v1 返回 503；可在 OpenList 中于原挂载点重建存储，然后点「刷新并纳管」，或移除这条映射';
      case 'replacement_found':
        return '原存储已不存在，挂载点上发现了未纳管的替代存储。确认前 /v1 访问会被拒绝（503）；请核对候选信息后点「确认使用替代存储」';
      default:
        return st || '';
    }
  }

  /** External link into the OpenList admin UI. Renders `null` when `url` is
   * empty/missing so a dead link never appears (the backend only fills
   * manage/edit URLs when the public URL is configured and valid). */
  function openlistLink(url, label, cls) {
    if (!url) return null;
    return h('a', { class: cls || 'btn btn-secondary btn-sm', href: String(url), target: '_blank', rel: 'noopener noreferrer' },
      icon('external', 13), label);
  }

  /** URL for the OpenList login wizard with the portal `return` target
   * appended, so the wizard can send the admin back after a save. The return
   * target is the PUBLIC landing page /admin/connect-return (the portal mount
   * is fixed at /admin): under SameSite=Strict a cross-site top-level return
   * from OpenList would omit the session cookie, so the landing re-enters
   * from the portal origin — same-site, cookie attached — and its script
   * navigates to #/drives?connected=<storage_id>, which this page parses as
   * before. The wizard replaces the {storage_id} placeholder textually.
   * Non-wizard URLs (the native /@manage links) pass through unchanged; the
   * hash of wizard URLs (e.g. …/@connect/#/reauth/252) is kept, the return
   * value rides in the query string. Today this builds the MANUAL fallback
   * link only: when the module supports the handoff, wizard URLs are minted
   * per click by the backend (openConnectHandoff below) and never rendered
   * into the page. */
  function connectLink(url) {
    const raw = String(url || '');
    if (raw.indexOf('/@connect/') < 0) return raw;
    try {
      const u = new URL(raw, location.href);
      u.searchParams.set('return', location.origin + '/admin/connect-return?storage_id={storage_id}');
      return u.href;
    } catch (e) {
      return raw; // never let a malformed URL break rendering
    }
  }

  /* ---------- OpenList connect handoff ----------
   * The portal never handles OpenList credentials: it asks the backend for a
   * short-lived handoff URL (POST /admin/api/connect/handoff) and hands the
   * browser to the OpenList login wizard. window.open MUST happen
   * synchronously inside the click handler — an async POST would lose the
   * user gesture and the popup blocker would silently swallow the wizard —
   * so the blank window is opened first, its opener severed, and the real
   * URL is only assigned after the backend answers. Every failure path (POST
   * error, blocked popup, closed blank window) ends in an explicit fallback
   * link; there is no automatic retry and no password entry in the portal. */
  function connectFallbackModal(message, href) {
    C2A.modal({
      title: '手动登录 OpenList',
      width: 'sm',
      body(body) {
        body.append(h('p', { class: 'text-sub', style: 'line-height:1.6' }, message));
        const link = openlistLink(href, '手动登录 OpenList', 'btn btn-primary');
        if (link) body.append(h('div', { class: 'mt-14' }, link));
      },
    });
  }

  /** Mint and open the login wizard for one connect route — `/` (all
   * netdisk platforms), `/new/goapi` (API upstream connection: the other
   * GoAPI's URL and API key are entered ONLY in OpenList) or
   * `/reauth/<storage_id>`. `fallbackHref` (optional) is the plain manual
   * connectLink URL shown when minting fails. */
  async function openConnectHandoff(target, fallbackHref) {
    let win = null;
    try { win = window.open('about:blank', '_blank'); } catch (e) { win = null; }
    if (win) { try { win.opener = null; } catch (e) { /* already severed */ } }
    let doc = null;
    try {
      doc = await C2A.API.post('/connect/handoff', { target: String(target || '/') });
    } catch (e) {
      if (win) { try { win.close(); } catch (e2) { /* ignore */ } }
      // a failed mint answers HTTP 502 with the manual fallback in the envelope
      const fromErr = e && e.body && e.body.data && e.body.data.fallback_url
        ? String(e.body.data.fallback_url) : '';
      connectFallbackModal('无法自动登录 OpenList：' + ((e && e.message) || '接口错误'), fromErr || fallbackHref);
      return;
    }
    const url = doc && doc.url ? String(doc.url) : '';
    const fallback = (doc && doc.fallback_url ? String(doc.fallback_url) : '') || fallbackHref || '';
    if (!url) {
      // old module: HTTP 200 with an empty url and the manual link available
      if (win) { try { win.close(); } catch (e) { /* ignore */ } }
      connectFallbackModal('当前 OpenList 登录模块不支持自动登录：请手动登录', fallback);
      return;
    }
    if (!win || win.closed) {
      connectFallbackModal('浏览器拦截了弹出窗口：请用下面的链接手动打开 OpenList', fallback || url);
      return;
    }
    try { win.location.href = url; } catch (e) {
      connectFallbackModal('无法跳转到 OpenList：请用下面的链接手动打开', fallback || url);
    }
  }

  /** The 添加网盘 choice modal: netdisk login (all platforms) or API upstream
   * connection to another GoAPI instance. Both are configured in the OpenList
   * wizard — the portal never asks for netdisk or upstream API credentials,
   * so the modal contains no inputs at all. Only rendered when the module
   * supports the handoff; without it callers link to connectLink() instead
   * (an older module has no /new/goapi route either). */
  function addDriveChoices(opts) {
    // opts: {connectUrl, manageUrl}
    const choices = [
      { target: '/', name: '网盘登录', desc: '在 OpenList 登录向导中选择百度、阿里云盘、123 云盘、OneDrive、Google Drive 等平台完成授权' },
      { target: '/new/goapi', name: 'API 上游连接', desc: '把另一台 GoAPI 实例的 API 连接为存储：上游地址与 API Key 只在 OpenList 向导中填写' },
    ];
    C2A.modal({
      title: '添加网盘',
      width: 'sm',
      body(body, close) {
        body.append(h('p', { class: 'text-sub', style: 'margin:0 0 4px' },
          '两种存储都在 OpenList 登录向导中配置；本门户不填写任何网盘或上游凭据。'));
        for (const c of choices) {
          const btn = h('button', { class: 'btn btn-secondary btn-sm', type: 'button', style: 'flex:0 0 auto' }, '打开向导');
          btn.addEventListener('click', () => {
            close();
            openConnectHandoff(c.target, opts && opts.connectUrl ? C2A.connectLink(opts.connectUrl) : '');
          });
          body.append(h('div', { style: 'display:flex;gap:12px;align-items:center;padding:12px 0;border-bottom:1px solid var(--border)' },
            h('div', { class: 'grow' },
              h('div', { style: 'font-weight:600' }, c.name),
              h('div', { class: 'text-muted', style: 'font-size:12.5px;line-height:1.5;margin-top:2px' }, c.desc)),
            btn));
        }
      },
      foot(foot, close) {
        const cancel = h('button', { class: 'btn btn-secondary', type: 'button', onclick: () => close() }, '取消');
        // openlistLink yields null without a URL — never append(null)
        const extra = opts && opts.manageUrl ? openlistLink(opts.manageUrl, '在 OpenList 中手动添加', 'btn btn-ghost') : null;
        foot.append(...[extra, cancel].filter(Boolean));
      },
    });
  }

  /** HH:MM:SS for the manual 刷新并纳管 timestamp. */
  function fmtTimeSec(iso) {
    if (!iso) return '—';
    const d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso);
    const p = (x) => String(x).padStart(2, '0');
    return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  const PERM_LABELS = {
    list: '列目录', read: '读取', write: '写入', delete: '删除', purge: '彻底删除',
    transfer: '转存', link: '直链', fetch: 'URL 上传', delegate: '签发子 Key',
    relay: '中转 (Gokapi)',
  };
  const PERMS = Object.keys(PERM_LABELS);

  /** Compact permission letters (lrwdPTLFDR), with a tooltip expansion. */
  const PERM_SHORT_MAP = { list: 'l', read: 'r', write: 'w', delete: 'd', purge: 'P', transfer: 'T', link: 'L', fetch: 'F', delegate: 'D', relay: 'R' };
  function permShort(perms) {
    if (!perms || !perms.length) return '∅';
    let s = '';
    for (const p of PERMS) if (perms.includes(p)) s += PERM_SHORT_MAP[p];
    return s;
  }
  function permFull(perms) {
    if (!perms || !perms.length) return '（无）';
    return perms.map((p) => PERM_LABELS[p] || p).join('、');
  }

  function permChips(perms, cls) {
    const row = h('span', { class: 'chip-row' });
    for (const p of perms || []) {
      row.append(h('span', { class: 'chip ' + (cls || 'perm-p'), title: p }, PERM_LABELS[p] || p));
    }
    if (!perms || !perms.length) row.append(h('span', { class: 'text-muted', style: 'font-size:12px' }, '—'));
    return row;
  }

  /* Linkage event kinds. The portal no longer writes OpenList storage
   * config, so no new customized / settings_changed events are produced —
   * the labels stay only so historical rows keep rendering sensibly. */
  const EVENT_KIND = {
    mount_renamed: { label: '挂载点变更', cls: 'badge-info' },
    disabled_in_openlist: { label: 'OpenList 中停用', cls: 'badge-slate' },
    enabled_in_openlist: { label: 'OpenList 中启用', cls: 'badge-ok' },
    deleted_in_openlist: { label: 'OpenList 中删除', cls: 'badge-danger' },
    storage_rebound: { label: '存储重绑定', cls: 'badge-violet' },
    customized: { label: '配置已自定义', cls: 'badge-warn' },
    adopted: { label: '已纳管', cls: 'badge-accent' },
    released: { label: '已移出', cls: 'badge-slate' },
    settings_changed: { label: '常用设置更新', cls: 'badge-info' },
    reauthenticated: { label: '重新登录完成', cls: 'badge-ok' },
    enabled: { label: '网盘启用', cls: 'badge-ok' },
    disabled: { label: '网盘停用', cls: 'badge-slate' },
    // legacy / defensive
    storage_disabled: { label: '存储停用', cls: 'badge-slate' },
    storage_enabled: { label: '存储启用', cls: 'badge-ok' },
    storage_deleted: { label: '存储删除', cls: 'badge-danger' },
    fields_customized: { label: '配置已自定义', cls: 'badge-warn' },
    token_invalid: { label: '令牌失效', cls: 'badge-danger' },
    created: { label: '已创建', cls: 'badge-ok' },
    request_approved: { label: '请求已批准', cls: 'badge-accent' },
    key_granted: { label: '子 Key', cls: 'badge-violet' },
  };
  function eventKindBadge(kind) {
    const m = EVENT_KIND[kind] || { label: kind, cls: 'badge-slate' };
    return h('span', { class: 'badge ' + m.cls }, m.label);
  }

  /* ---------- clipboard ---------- */
  async function copyText(text, okMsg) {
    let ok = false;
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        ok = true;
      }
    } catch (e) { /* fall through */ }
    if (!ok) {
      try {
        const ta = document.createElement('textarea');
        ta.value = text;
        ta.style.cssText = 'position:fixed;top:-100px;opacity:0';
        document.body.appendChild(ta);
        ta.select();
        ok = document.execCommand('copy');
        ta.remove();
      } catch (e) { ok = false; }
    }
    toast(ok ? (okMsg || '已复制到剪贴板') : '复制失败，请手动选择复制', ok ? 'success' : 'error');
    return ok;
  }

  /* ---------- toasts ---------- */
  function toast(message, type, timeoutMs) {
    const root = document.getElementById('toast-root');
    type = type || 'info';
    const iconName = type === 'success' ? 'check' : type === 'error' ? 'xcircle' : type === 'warn' ? 'alert' : 'info';
    const el = h('div', { class: 'toast toast-' + type, role: 'status' },
      icon(iconName, 15),
      h('span', { class: 'toast-msg' }, String(message)),
      h('button', { class: 'icon-btn', type: 'button', 'aria-label': '关闭提示', onclick: () => dismiss() }, icon('close', 13)));
    function dismiss() {
      if (!el.isConnected) return;
      el.classList.add('leaving');
      setTimeout(() => el.remove(), 200);
    }
    root.appendChild(el);
    setTimeout(dismiss, timeoutMs || 4200);
    // cap stack size
    while (root.children.length > 5) root.firstChild.remove();
    return dismiss;
  }

  function leaveOverlay(overlay) {
    overlay.classList.add('leaving');
    overlay.setAttribute('aria-hidden', 'true');
    overlay.inert = true;
    setTimeout(() => overlay.remove(), window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 0 : 160);
  }

  /* ---------- modal ---------- */
  let openModalCount = 0;
  /**
   * UI.modal({title, width:'sm'|'md'|'lg', body: fn(container), foot: fn(container)|null,
   *           onClose: fn, dismissable: bool}) -> {close, body, foot}
   */
  function modal(opts) {
    const root = document.getElementById('modal-root');
    const bodyEl = h('div', { class: 'modal-body' });
    const footEl = h('div', { class: 'modal-foot' });
    const returnFocus = document.activeElement;
    let closed = false;
    function close() {
      if (closed) return;
      closed = true;
      openModalCount = Math.max(0, openModalCount - 1);
      // Keep the exit geometry while scrubbing sensitive text immediately.
      box.style.height = box.getBoundingClientRect().height + 'px';
      clear(bodyEl); clear(footEl);
      leaveOverlay(overlay);
      if (returnFocus && returnFocus.isConnected) returnFocus.focus();
      document.removeEventListener('keydown', onKey);
      if (opts.onClose) try { opts.onClose(); } catch (e) { /* ignore */ }
    }
    function onKey(e) {
      if (e.key === 'Escape' && (opts.dismissable !== false)) close();
    }
    const box = h('div', { class: 'modal' + (opts.width === 'md' ? ' modal-w-md' : opts.width === 'lg' ? ' modal-w-lg' : ''), role: 'dialog', 'aria-modal': 'true' },
      h('div', { class: 'modal-head' },
        h('div', { class: 'modal-title' }, opts.title || ''),
        h('button', { class: 'icon-btn modal-close', type: 'button', 'aria-label': '关闭', onclick: () => opts.dismissable !== false && close() }, icon('close', 16))),
      bodyEl,
      opts.foot ? footEl : null);
    const overlay = h('div', { class: 'modal-overlay' }, box);
    if (opts.dismissable !== false) {
      overlay.addEventListener('mousedown', (e) => { if (e.target === overlay) close(); });
      document.addEventListener('keydown', onKey);
    }
    root.appendChild(overlay);
    openModalCount++;
    if (opts.body) opts.body(bodyEl, close);
    if (opts.foot) opts.foot(footEl, close);
    const firstInput = box.querySelector('input, select, textarea, button.btn-primary');
    if (firstInput) try { firstInput.focus(); } catch (e) { /* ignore */ }
    return { close: close, body: bodyEl, foot: footEl, box: box };
  }

  /* ---------- confirm ---------- */
  function confirmModal(opts) {
    // opts: {title, message(str|node), danger, confirmText, cancelText, extra(node), onConfirm(async)}
    let busy = false;
    const m = modal({
      title: opts.title || '确认操作',
      body(body) {
        const msg = typeof opts.message === 'string' ? h('p', { class: 'text-sub', style: 'line-height:1.6' }, opts.message) : opts.message;
        body.append(msg);
        if (opts.extra) body.append(opts.extra);
      },
      foot(foot, close) {
        const btn = h('button', { class: 'btn ' + (opts.danger ? 'btn-danger' : 'btn-primary'), type: 'button' }, opts.confirmText || '确认');
        btn.addEventListener('click', async () => {
          if (busy) return;
          busy = true;
          btn.disabled = true;
          btn.textContent = '处理中…';
          try {
            const keepOpen = opts.onConfirm ? await opts.onConfirm() : false;
            if (!keepOpen) close();
          } catch (e) {
            toast(e.message || '操作失败', 'error');
            busy = false;
            btn.disabled = false;
            btn.textContent = opts.confirmText || '确认';
          }
        });
        foot.append(
          h('button', { class: 'btn btn-secondary', type: 'button', onclick: () => close() }, opts.cancelText || '取消'),
          btn);
      },
      onClose: opts.onClose,
    });
    return m;
  }

  /* ---------- dropdown menu ---------- */
  let openMenuCleanup = null;
  function menu(items) {
    // items: [{label, icon, danger, disabled, title, onSelect}]
    const btn = h('button', { class: 'icon-btn', type: 'button', 'aria-label': '更多操作', title: '更多操作' }, icon('dots', 16));
    function closeMenu() {
      const ex = document.querySelector('.menu-pop');
      if (ex) ex.remove();
      document.removeEventListener('mousedown', onDoc, true);
      document.removeEventListener('keydown', onKey, true);
      openMenuCleanup = null;
    }
    function onDoc(e) { if (!e.target.closest || !e.target.closest('.menu-pop')) closeMenu(); }
    function onKey(e) { if (e.key === 'Escape') closeMenu(); }
    btn.addEventListener('click', (e) => {
      e.stopPropagation();
      if (document.querySelector('.menu-pop')) { closeMenu(); return; }
      const pop = h('div', { class: 'menu-pop' });
      for (const it of items) {
        const row = h('button', { class: 'menu-item' + (it.danger ? ' danger' : ''), type: 'button', disabled: it.disabled ? true : null, title: it.title || null },
          it.icon ? icon(it.icon, 14) : null,
          h('span', null, it.label));
        if (it.disabled) row.disabled = true;
        row.addEventListener('click', () => { closeMenu(); if (!it.disabled && it.onSelect) it.onSelect(); });
        pop.appendChild(row);
      }
      document.body.appendChild(pop);
      const r = btn.getBoundingClientRect();
      const pw = 176;
      pop.style.top = (r.bottom + 6 + window.scrollY) + 'px';
      pop.style.left = Math.max(8, r.right - pw + window.scrollX) + 'px';
      document.addEventListener('mousedown', onDoc, true);
      document.addEventListener('keydown', onKey, true);
      openMenuCleanup = closeMenu;
    });
    return btn;
  }

  /* ---------- states ---------- */
  function emptyState(opts) {
    return h('div', { class: 'empty' },
      svgEl(opts.icon || 'inbox', 38),
      h('div', { class: 'empty-title' }, opts.title || '暂无数据'),
      opts.sub ? h('div', { class: 'empty-sub' }, opts.sub) : null,
      opts.action ? opts.action : null);
  }
  function loadingBlock(text) {
    return h('div', { class: 'loading-block' },
      h('span', { class: 'spin', style: 'display:inline-flex' }, icon('refresh', 15)),
      text || '加载中…');
  }
  function errorState(message, retry) {
    return h('div', { class: 'empty' },
      svgEl('alert', 36),
      h('div', { class: 'empty-title' }, '加载失败'),
      h('div', { class: 'empty-sub' }, String(message || '未知错误')),
      retry ? h('button', { class: 'btn btn-secondary btn-sm', type: 'button', onclick: retry, style: 'margin-top:12px' }, '重试') : null);
  }

  /** Calm placeholder for endpoints the backend has not shipped yet (404). */
  function comingSoon(opts) {
    opts = opts || {};
    return h('div', { class: 'empty empty-soon' },
      svgEl(opts.icon || 'info', 34),
      h('div', { class: 'empty-title' }, opts.title || '该功能将在后续版本启用'),
      h('div', { class: 'empty-sub' }, opts.sub || '后端尚未开放此接口，上线后这里会自动显示内容'));
  }

  /* table skeleton */
  function tableSkeleton(cols, rows) {
    const wrap = h('div', { class: 'table-wrap' });
    const tbl = h('table', { class: 'tbl' });
    const thead = h('thead');
    const hr = h('tr');
    for (let i = 0; i < cols; i++) hr.append(h('th', null, ' '));
    thead.append(hr);
    const tbody = h('tbody');
    for (let r = 0; r < (rows || 5); r++) {
      const tr = h('tr');
      for (let i = 0; i < cols; i++) tr.append(h('td', null, h('div', { class: 'skeleton', style: 'height:12px;width:' + (40 + ((r * 13 + i * 29) % 50)) + '%' })));
      tbody.append(tr);
    }
    tbl.append(thead, tbody);
    wrap.append(tbl);
    return wrap;
  }

  /* section title inside modals */
  function snippetTitle(text, opts) {
    return h('div', { class: 'snippet-title' }, text, opts && opts.tag ? h('span', { class: 'tag-opt' }, opts.tag) : null);
  }
  function codebox(text, opts) {
    const pre = h('pre', { class: 'codebox' + (opts && opts.plain ? ' key-plain' : '') }, text);
    pre.appendChild(h('button', { class: 'copy-btn', type: 'button', onclick: () => copyText(text) }, '复制'));
    return pre;
  }

  /** True while a modal or dropdown menu is open — background auto-refresh
   * should pause so it never re-renders under the admin. */
  C2A.hasOpenOverlay = function () {
    return !!(
      document.querySelector('#modal-root .modal-overlay') ||
      document.querySelector('.menu-pop'));
  };

  C2A.h = h;
  C2A.clear = clear;
  /** Public base URL used in onboarding snippets: configured value wins. */
  C2A.baseUrlOf = function (settings) {
    const cfg = settings && settings.public_base_url ? String(settings.public_base_url).trim() : '';
    return (cfg || location.origin).replace(/\/+$/, '');
  };
  C2A.icon = icon;
  C2A.svgEl = svgEl;
  C2A.fmtBytes = fmtBytes;
  C2A.fmtTime = fmtTime;
  C2A.fmtTimeSec = fmtTimeSec;
  C2A.fmtAgo = fmtAgo;
  C2A.fmtDuration = fmtDuration;
  C2A.stateBadge = stateBadge;
  C2A.DRIVE_STATE = DRIVE_STATE;
  C2A.stateDesc = stateDesc;
  C2A.openlistLink = openlistLink;
  C2A.connectLink = connectLink;
  C2A.openConnectHandoff = openConnectHandoff;
  C2A.addDriveChoices = addDriveChoices;
  C2A.PERM_SHORT = permShort;
  C2A.PERM_FULL = permFull;
  C2A.permChips = permChips;
  C2A.eventKindBadge = eventKindBadge;
  C2A.PERMS = PERMS;
  C2A.PERM_LABELS = PERM_LABELS;
  C2A.copyText = copyText;
  C2A.toast = toast;
  C2A.modal = modal;
  C2A.confirmModal = confirmModal;
  C2A.menu = menu;
  C2A.emptyState = emptyState;
  C2A.loadingBlock = loadingBlock;
  C2A.errorState = errorState;
  C2A.comingSoon = comingSoon;
  C2A.tableSkeleton = tableSkeleton;
  C2A.snippetTitle = snippetTitle;
  C2A.codebox = codebox;
})();
