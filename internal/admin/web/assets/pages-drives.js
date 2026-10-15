/* cloud2api admin — page: 网盘 (drives)
 *
 * Step 1 of the OpenList-entry plan: adding, logging in and editing storages
 * happens in OpenList's own admin UI. cloud2api only discovers storages,
 * adopts (纳管) them and keeps the drive ↔ storage mapping. 「刷新并纳管」
 * (POST /drives/sync) replaces both lists from a single snapshot. No timers,
 * no window watching.
 *
 * With the patched OpenList (overview.openlist.connect_url set) the primary
 * 「添加网盘」 action opens the choice modal (网盘登录 for every platform, or
 * API 上游连接 = another GoAPI instance chained as storage, entered only in
 * OpenList's /#/new/goapi) and mints the handoff URL per click; drive rows
 * carry openlist_reauth_url and mint /reauth/<id> the same way. Without the
 * handoff capability (overview.openlist.connect_handoff_available false, e.g.
 * an older module) the links fall back to connectLink() hrefs, labelled
 * 手动登录 OpenList; the native OpenList admin (<base>/@manage) stays as the
 * advanced fallback for other netdisks and parameters. Wizard links carry a
 * `return` parameter (C2A.connectLink): after saving, the wizard links back
 * to #/drives?connected=<storage_id>, which runs one sync here and then opens
 * the adopt dialog for the new storage or highlights the updated drive.
 *
 * The return is entry-agnostic: this page only parses #/drives?connected=…,
 * so it does not matter whether the browser arrived by direct link or via a
 * same-origin landing hop. That hop matters under SameSite=Strict when
 * OpenList is published on a different site than the portal (LAN address vs
 * loopback): a top-level cross-site navigation omits the portal session
 * cookie and would land on the login form. The fix is a public landing route
 * on the portal origin that JS-navigates to /admin/#/drives?connected=<id>
 * (same-site then, cookie sent); it is backend-owned, this page needs
 * nothing beyond the parser below.
 *
 * Every async continuation checks `stale()` (page generation + route): the
 * router may render a NEW drives page while a sync/return flow is still in
 * flight, and a detached old page must never re-render.
 */
(function () {
  'use strict';
  const C2A = window.C2A;
  const { h, icon } = C2A;
  const pages = (C2A.pages = C2A.pages || {});

  const driveView = { search: '', provider: 'all', status: 'all', mode: C2A.preference.get('drive-view', 'grid') === 'table' ? 'table' : 'grid' };
  /* Render generation: pages.drives may overlap itself (rapid navigation, a
   * fullReload during an in-flight sync/return flow). Only the NEWEST
   * instance may render — an old one holds detached DOM that would silently
   * swallow the update. */
  let pageGen = 0;
  const PROVIDER_LABELS = {
    baidu: ['百度网盘', 'amber'], aliyun: ['阿里云盘', 'violet'], '123pan': ['123 云盘', 'blue'],
    '123open': ['123 开放平台', 'blue'], '115': ['115 网盘', 'teal'], '115open': ['115 开放平台', 'teal'],
    quark: ['夸克网盘', 'violet'], quarkopen: ['夸克开放平台', 'violet'], onedrive: ['OneDrive', 'blue'],
    googledrive: ['Google Drive', 'teal'], local: ['本地存储', 'slate'], webdav: ['WebDAV', 'slate'],
  };
  function providerInfo(d) { return PROVIDER_LABELS[d.provider] || [d.driver || d.provider || '存储', 'slate']; }
  function isProblem(d) { return d.state !== 'healthy' && d.state !== 'disabled'; }
  /** Tooltip for the 重新授权 entry: the login wizard updates credentials of
   * the SAME storage — id, mount point, settings and Key grants stay. */
  const REAUTH_TITLE = '在 OpenList 登录向导中更新该存储的登录凭据；存储 ID、挂载点与 Key 授权保持不变';

  /** Why is this drive disabled? Two independent causes share the `disabled`
   * state: cloud2api's own switch (`enabled === false`) and the storage being
   * disabled in OpenList (`storage_disabled`). */
  function disabledDesc(d) {
    if (d.storage_disabled && d.enabled === false) {
      return '已在 OpenList 中停用，且已在 cloud2api 中停用；现有 Key 授权保留。请在 OpenList 中启用该存储，并在此处启用网盘。';
    }
    if (d.storage_disabled) {
      return '已在 OpenList 中停用；请前往 OpenList 启用该存储。Key 授权保留，OpenList 之外的配置不变。';
    }
    return '已在 cloud2api 中停用：agent 无法再通过 cloud2api 访问该网盘（Key 授权保留）。点击「启用」恢复；OpenList 中的存储不受影响。';
  }

  /** storage status badge for unmanaged / replacement candidates */
  function storageStatusBadge(status, disabled) {
    if (disabled) return h('span', { class: 'badge badge-slate' }, '停用');
    if (status === 'work') return h('span', { class: 'badge badge-ok' }, h('span', { class: 'dot' }), 'work');
    return h('span', { class: 'badge badge-warn', title: status || '' },
      h('span', { class: 'elide', style: 'display:inline-block;max-width:240px;vertical-align:bottom' }, status || '未知'));
  }

  pages.drives = async function (root) {
    C2A.clear(root);
    const gen = ++pageGen;
    const routeGen0 = C2A.pageGen ? C2A.pageGen() : 0;
    /** True once this page instance has been superseded — by a newer drives
     * render, by ANY route change (app.js bumps its lifecycle generation on
     * every route, so moving away and back invalidates in-flight flows too),
     * or by the router leaving the drives route (also checked directly so the
     * guard holds when the SPA shell is not the caller, e.g. tests). Every
     * await continuation checks it before touching the detached DOM. */
    function stale() {
      if (gen !== pageGen) return true;
      if (C2A.pageGen && routeGen0 !== C2A.pageGen()) return true;
      return C2A.currentRoute ? C2A.currentRoute() !== 'drives' : false;
    }

    /* ---- page state ---- */
    const st = {
      drives: [],            // managed rows (latest snapshot)
      unmanaged: null,       // null = endpoint/response not available yet
      snapshotError: '',     // GET /drives served from cache
      syncError: '',         // last POST /drives/sync failure
      syncedAt: '',          // RFC3339 of the last successful sync
      manageUrl: '',         // <base>/@manage from /overview ('' when unset/invalid)
      connectUrl: '',        // <base>/@connect/ — patched OpenList login wizard ('' when unavailable)
      handoff: false,        // overview connect_handoff_available: the backend can mint wizard URLs
      platforms: [],         // overview openlist.connect_platforms: [{id,name,driver}]
      syncBusy: false,
    };

    /** Mint-and-open the reauth wizard for one storage. Availability comes
     * from the overview payload (st.handoff) — never inferred from the row —
     * so the decision is the same on every page that renders the entry. The
     * short-lived URL is requested per click, never at render. */
    function openReauthHandoff(d, reauthUrl) {
      C2A.openConnectHandoff('/reauth/' + d.storage_id, C2A.connectLink(reauthUrl));
    }

    /** The 重新授权 entry for one drive: a button minting the handoff when
     * the module supports it, else the manual connectLink anchor (older
     * module). */
    function reauthEntry(d, reauthUrl, cls) {
      if (st.handoff) {
        const b = h('button', { class: cls, type: 'button' }, icon('external', 13), '重新授权');
        b.title = REAUTH_TITLE;
        b.addEventListener('click', () => openReauthHandoff(d, reauthUrl));
        return b;
      }
      const a = C2A.openlistLink(C2A.connectLink(reauthUrl), '重新授权', cls);
      if (a) a.title = REAUTH_TITLE + '（手动登录 OpenList）';
      return a;
    }

    /* ---- static shell ---- */
    const primaryHolder = h('span', { class: 'flex items-center gap-8' });
    const syncBtn = h('button', { class: 'btn btn-secondary', type: 'button' }, icon('refresh', 14), '刷新并纳管');
    syncBtn.addEventListener('click', () => syncNow());
    root.append(C2A.pageHead('网盘管理',
      '网盘的添加、授权与参数修改都在 OpenList 后台完成；这里负责发现、纳管与状态跟踪。',
      [primaryHolder, syncBtn]));
    const calloutText = h('div', { class: 'co-text' });
    root.append(h('div', { class: 'callout callout-neutral mb-14' }, icon('info', 14), calloutText));

    const bannerHost = h('div');
    const workspace = h('section', { class: 'drive-workspace', 'aria-label': '已纳管网盘' });
    const unmanagedCard = h('div', { class: 'card unmanaged-card', style: 'margin-top:18px', dataset: { section: 'unmanaged' } });
    root.append(bannerHost, workspace, unmanagedCard);

    /* ---- render helpers ---- */
    function renderPrimary() {
      C2A.clear(primaryHolder);
      if (st.connectUrl) {
        if (st.handoff) {
          // single entry: the choice modal lists both storage kinds; the URL
          // is minted per click, never at render (short-lived tickets)
          const add = h('button', { class: 'btn btn-primary', type: 'button', title: '网盘登录或 API 上游连接，都在 OpenList 登录向导中完成' },
            icon('plus', 14), '添加网盘');
          add.addEventListener('click', () => C2A.addDriveChoices({ connectUrl: st.connectUrl, manageUrl: st.manageUrl }));
          primaryHolder.append(add);
        } else {
          // older connect module: plain manual link, no choice modal (it has
          // no /new/goapi route and cannot honour a minted ticket)
          const manual = C2A.openlistLink(C2A.connectLink(st.connectUrl), '手动登录 OpenList', 'btn btn-primary');
          if (manual) manual.title = '打开 OpenList 登录向导；当前登录模块不支持自动登录，API 上游连接不可用';
          primaryHolder.append(manual);
        }
        if (st.manageUrl) {
          const native = C2A.openlistLink(st.manageUrl, '在 OpenList 中手动添加', 'btn btn-secondary');
          if (native) native.title = '其他网盘或高级参数使用 OpenList 原生表单';
          primaryHolder.append(native);
        }
      } else if (st.manageUrl) {
        primaryHolder.append(C2A.openlistLink(st.manageUrl, '在 OpenList 中添加网盘', 'btn btn-primary'));
      } else {
        const cfg = h('button', { class: 'btn btn-primary', type: 'button', title: '未配置或地址无效：先在设置页填写 OpenList 公开访问地址' },
          icon('settings', 14), '配置 OpenList 访问地址');
        cfg.addEventListener('click', () => { location.hash = '#/settings'; });
        primaryHolder.append(cfg);
      }
    }

    function renderCallout() {
      C2A.clear(calloutText);
      calloutText.append(st.connectUrl
        ? (st.handoff
          ? '点「添加网盘」选择网盘登录或 API 上游连接（另一台 GoAPI 实例的地址与 API Key），两者都在 OpenList 登录向导中填写；保存后回到这里点「刷新并纳管」，再为需要访问它的 API Key 授权。'
          : '在 OpenList 登录向导中完成授权并保存存储后，回到这里点「刷新并纳管」，再为需要访问它的 API Key 授权。')
        : '在 OpenList 后台添加并保存存储，确认状态正常后，回到这里点「刷新并纳管」。部分网盘需要先用 OpenList 的授权工具取得令牌。');
    }

    function renderBanners() {
      C2A.clear(bannerHost);
      if (st.snapshotError) {
        const retry = h('button', { class: 'btn btn-secondary btn-sm', type: 'button', style: 'flex:0 0 auto' }, '重试');
        retry.addEventListener('click', () => fullReload());
        bannerHost.append(h('div', { class: 'callout callout-warn mb-14' }, icon('alert', 14),
          h('div', { class: 'co-text' },
            h('div', { style: 'font-weight:600' }, 'OpenList 暂时无法读取，以下列表来自最近一次缓存'),
            h('div', { class: 'mt-8 mono', style: 'font-size:12px;word-break:break-all' }, st.snapshotError)),
          retry));
      }
      if (st.syncError) {
        const retry = h('button', { class: 'btn btn-secondary btn-sm', type: 'button', style: 'flex:0 0 auto' }, '重试');
        retry.addEventListener('click', () => syncNow());
        bannerHost.append(h('div', { class: 'callout callout-danger mb-14' }, icon('xcircle', 14),
          h('div', { class: 'co-text' },
            h('div', { style: 'font-weight:600' }, '刷新失败，页面保留的是上一次的数据'),
            h('div', { class: 'mt-8 mono', style: 'font-size:12px;word-break:break-all' }, st.syncError)),
          retry));
      }
    }

    function renderAll() {
      renderPrimary();
      renderCallout();
      renderBanners();
      renderWorkspace();
      renderUnmanaged();
    }

    function applyDrives(data) {
      st.drives = (data && data.drives) || [];
      st.snapshotError = (data && data.snapshot_error) || '';
    }

    async function loadInitial() {
      C2A.clear(unmanagedCard);
      workspace.append(h('div', { class: 'drive-grid' }, Array.from({ length: 6 }, () =>
        h('div', { class: 'drive-card drive-skeleton', 'aria-hidden': 'true' },
          h('div', { class: 'skeleton', style: 'width:45%;height:22px' }),
          h('div', { class: 'skeleton', style: 'height:16px;margin-top:24px' }),
          h('div', { class: 'skeleton', style: 'height:76px;margin-top:20px' }),
          h('div', { class: 'skeleton', style: 'width:65%;height:30px;margin-top:20px' })))));
      workspace.setAttribute('aria-busy', 'true');

      let drivesData, un, ov;
      try {
        [drivesData, un, ov] = await Promise.all([
          C2A.API.get('/drives'),
          C2A.API.getSoft('/drives/unmanaged'),
          C2A.API.get('/overview', { noRedirect: true }).catch(() => null),
        ]);
      } catch (e) {
        if (stale()) return; // a newer page owns the router now
        C2A.clear(workspace);
        workspace.removeAttribute('aria-busy');
        workspace.append(C2A.errorState(e.message, () => fullReload()));
        return;
      }
      if (stale()) return; // superseded: never render into detached DOM
      workspace.removeAttribute('aria-busy');
      applyDrives(drivesData);
      st.unmanaged = un === null ? null : (un.storages || []);
      st.manageUrl = (ov && ov.openlist && ov.openlist.manage_url) || '';
      st.connectUrl = (ov && ov.openlist && ov.openlist.connect_url) || '';
      st.handoff = !!st.connectUrl && (ov && ov.openlist && ov.openlist.connect_handoff_available) === true;
      st.platforms = (ov && ov.openlist && ov.openlist.connect_platforms) || [];
      renderAll();

      // returning from the login wizard (`return` → #/drives?connected=<id>):
      // sync and react to the saved storage; the overview quick action only
      // flags one plain sync. Fire-and-forget: syncNow swallows its own
      // errors into st.syncError, everything after it checks stale().
      const connectedId = connectedRouteId();
      if (connectedId !== null) runConnectedFlow(connectedId);
      else if (C2A.pendingDriveSync) {
        delete C2A.pendingDriveSync;
        syncNow();
      }
    }

    function fullReload() {
      if (stale()) return; // a newer page instance owns the root already
      pages.drives(root);
    }

    /** The wizard's `return` target: #/drives?connected=<storage_id>. Returns
     * the id (digits, possibly '') when the hash carries the param, null when
     * it does not (or the value is not digits/empty). */
    function connectedRouteId() {
      const hash = String(location.hash || '');
      const q = hash.indexOf('?');
      if (q < 0) return null;
      let params;
      try { params = new URLSearchParams(hash.slice(q + 1)); } catch (e) { return null; }
      if (!params.has('connected')) return null;
      const v = params.get('connected') || '';
      return /^\d*$/.test(v) ? v : null;
    }

    /** Entry from the login wizard's 「在 cloud2api 中查看」: run the same
     * sync as 刷新并纳管, then react to the storage it just saved —
     * unmanaged candidate → adopt dialog (ID pre-filled); managed drive →
     * flash + state toast; else an info toast. Finally strip the query from
     * the URL so reloading the page does not re-trigger the flow. */
    async function runConnectedFlow(id) {
      await syncNow(true); // never throws; failures land in st.syncError
      if (stale()) return; // navigated away, hash already changed / newer page
      history.replaceState(null, '', '#/drives');
      if (!id) return;
      const cand = (st.unmanaged || []).find((s) => s && String(s.storage_id) === id);
      if (cand && !cand.replacement_for) {
        adoptDialog(cand);
        return;
      }
      if (cand && cand.replacement_for) {
        // storage 15-style candidates cannot be adopted (the mount belongs to
        // an existing drive) — send the admin to the rebind confirmation
        scrollToDrive(cand.replacement_for);
        C2A.toast('存储 #' + id + ' 是网盘 ' + cand.replacement_for + ' 的替代存储；请在对应网盘上确认使用', 'info', 6000);
        return;
      }
      const drive = st.drives.find((d) => String(d.storage_id) === id);
      if (drive) {
        scrollToDrive(drive.id);
        const lbl = (C2A.DRIVE_STATE[drive.state] || {}).label || drive.state;
        C2A.toast('网盘 ' + drive.id + '（存储 #' + id + '）：' + lbl,
          drive.state === 'healthy' ? 'success' : drive.state === 'disabled' ? 'info' : 'warn', 6000);
        return;
      }
      // stale data would make "已刷新" a lie — the sync-failure banner covers it
      if (!st.syncError) C2A.toast('已刷新，但没有找到存储 #' + id + '；请确认已在 OpenList 中保存', 'info', 6000);
    }

    async function syncNow(skipScroll) {
      if (st.syncBusy) return;
      st.syncBusy = true;
      syncBtn.disabled = true;
      C2A.clear(syncBtn); syncBtn.append(h('span', { class: 'spin', style: 'display:inline-flex' }, icon('refresh', 14)), '刷新中…');
      st.syncError = '';
      try {
        const out = await C2A.API.post('/drives/sync');
        if (stale()) { st.syncBusy = false; return; } // a newer page renders, not this DOM
        applyDrives(out);
        st.unmanaged = out.unmanaged || [];
        st.syncedAt = out.synced_at || new Date().toISOString();
        renderAll();
        if (!skipScroll) unmanagedCard.scrollIntoView({ behavior: 'smooth', block: 'start' });
      } catch (e) {
        st.syncError = e.message || String(e);
        if (!stale()) {
          renderBanners();
          C2A.toast(st.syncError, 'error');
        }
      }
      st.syncBusy = false;
      syncBtn.disabled = false;
      C2A.clear(syncBtn); syncBtn.append(icon('refresh', 14), '刷新并纳管');
    }

    /* ---------------- managed drives workspace ---------------- */
    function renderWorkspace() {
      C2A.clear(workspace);
      if (!st.drives.length) {
        const syncEmpty = h('button', { class: 'btn btn-primary btn-sm', type: 'button' }, icon('refresh', 13), '刷新并纳管');
        syncEmpty.addEventListener('click', () => syncNow());
        workspace.append(C2A.emptyState({
          icon: 'drives', title: '还没有网盘',
          sub: '在 OpenList 后台添加并保存存储后，回到这里点「刷新并纳管」让它出现在这里。',
          action: syncEmpty,
        }));
        return;
      }
      renderDriveWorkspace(workspace, st.drives);
    }

    function renderDriveWorkspace(root, drives) {
      const healthy = drives.filter((d) => d.state === 'healthy').length;
      const problems = drives.filter(isProblem).length;
      const summary = h('div', { class: 'drive-summary' },
        h('div', { class: 'drive-counts' },
          h('span', null, h('strong', null, String(drives.length)), ' 个网盘'),
          h('span', { class: 'text-ok' }, h('strong', null, String(healthy)), ' 个正常'),
          problems ? h('span', { class: 'text-warn' }, h('strong', null, String(problems)), ' 个需关注') : null),
        h('span', { class: 'card-desc' },
          icon('refresh', 12),
          st.syncedAt ? '上次刷新 ' + C2A.fmtTimeSec(st.syncedAt) : '尚未手动刷新'));
      const bars = h('div', { class: 'health-strip', 'aria-label': '当前网盘状态分布' },
        drives.map((d) => h('span', { class: 'health-segment health-' + (d.state === 'healthy' ? 'ok' : d.state === 'disabled' ? 'off' : 'warn'), title: d.id + ' · ' + (C2A.stateBadge(d.state).textContent) })));
      const tabs = h('div', { class: 'provider-tabs', role: 'group', 'aria-label': '按服务商筛选' });
      const providers = [...new Set(drives.map((d) => d.provider))];
      if (!providers.includes(driveView.provider)) driveView.provider = 'all';
      for (const prov of ['all', ...providers]) {
        const info = prov === 'all' ? ['全部', 'slate'] : providerInfo(drives.find((d) => d.provider === prov));
        const count = prov === 'all' ? drives.length : drives.filter((d) => d.provider === prov).length;
        const b = h('button', { class: 'provider-tab', type: 'button', dataset: { provider: prov }, 'aria-pressed': String(driveView.provider === prov) },
          prov === 'all' ? icon('grid', 16) : h('span', { class: 'provider-dot tone-' + info[1] }),
          info[0], h('span', { class: 'tab-count' }, String(count)));
        b.addEventListener('click', () => { driveView.provider = prov; renderResults(); });
        tabs.append(b);
      }
      const search = h('input', { class: 'input', type: 'search', 'aria-label': '搜索网盘', placeholder: '搜索网盘名称、驱动或挂载点', value: driveView.search });
      const status = h('select', { class: 'select', 'aria-label': '按状态筛选' },
        [['all', '全部状态'], ['healthy', '正常'], ['problem', '需关注'], ['disabled', '已停用']].map(([value, label]) => h('option', { value, selected: driveView.status === value }, label)));
      const modes = h('div', { class: 'view-switch', role: 'group', 'aria-label': '显示方式' });
      for (const mode of ['grid', 'table']) {
        const b = h('button', { class: 'icon-btn', type: 'button', dataset: { mode }, 'aria-label': mode === 'grid' ? '卡片视图' : '表格视图', title: mode === 'grid' ? '卡片视图' : '表格视图' }, icon(mode === 'grid' ? 'grid' : 'list', 17));
        b.addEventListener('click', () => { driveView.mode = mode; C2A.preference.set('drive-view', mode); renderResults(); });
        modes.append(b);
      }
      const count = h('span', { class: 'result-count', role: 'status' });
      const toolbar = h('div', { class: 'drive-toolbar' }, h('div', { class: 'drive-search' }, icon('search', 17), search), status, count, modes);
      const results = h('div', { class: 'drive-results' });
      let searchTimer;
      search.addEventListener('input', () => {
        driveView.search = search.value;
        clearTimeout(searchTimer);
        searchTimer = setTimeout(renderResults, 100);
      });
      status.addEventListener('change', () => { driveView.status = status.value; renderResults(); });
      function renderResults() {
        const query = driveView.search.trim().toLowerCase();
        const visible = drives.filter((d) =>
          (driveView.provider === 'all' || d.provider === driveView.provider) &&
          (driveView.status === 'all' || (driveView.status === 'problem' ? isProblem(d) : d.state === driveView.status)) &&
          (!query || [d.id, d.provider, d.driver, d.mount_path, providerInfo(d)[0]].some((v) => String(v || '').toLowerCase().includes(query))));
        tabs.querySelectorAll('button').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.provider === driveView.provider)));
        modes.querySelectorAll('button').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.mode === driveView.mode)));
        count.textContent = '显示 ' + visible.length + ' / ' + drives.length;
        C2A.clear(results);
        if (!visible.length) {
          const reset = h('button', { class: 'btn btn-secondary btn-sm', type: 'button' }, '清除筛选');
          reset.addEventListener('click', () => { driveView.search = ''; driveView.provider = 'all'; driveView.status = 'all'; search.value = ''; status.value = 'all'; renderResults(); });
          results.append(C2A.emptyState({ icon: 'search', title: '没有匹配的网盘', sub: '试试其他关键词，或清除当前筛选。', action: reset }));
        } else if (driveView.mode === 'table') {
          results.append(h('div', { class: 'card table-wrap' }, drivesTable(visible)));
        } else {
          results.append(h('div', { class: 'drive-grid' }, visible.map((d, i) => driveCard(d, i))));
        }
      }
      root.append(summary, bars, tabs, toolbar, results);
      renderResults();
    }

    function storageLabelOf(d) {
      if (d.state === 'replacement_found') return '原存储 ' + (d.storage_id ? '#' + d.storage_id : '') + '（已不存在）';
      return d.storage_id ? '#' + d.storage_id : '未绑定';
    }

    function driveCard(d, index) {
      const [label, tone] = providerInfo(d);
      const card = h('article', { class: 'drive-card', style: '--enter-delay:' + Math.min(index * 35, 175) + 'ms', 'aria-label': d.id, dataset: { driveId: d.id } });
      const title = h('div', { class: 'drive-card-head' },
        h('span', { class: 'provider-pill tone-' + tone }, icon('drives', 14), label),
        d.static ? h('span', { class: 'static-label', title: '来自 config.yaml，请在配置文件中修改' }, '静态配置') : null,
        h('span', { class: 'storage-label mono' }, storageLabelOf(d)));
      const desc = {
        healthy: '存储已连接，可通过授权的 API Key 访问。',
        needs_reauth: d.openlist_reauth_url
          ? '登录凭据已失效。点「重新授权」在 OpenList 登录向导中更新凭据；存储 ID、挂载点与 Key 授权保持不变；完成后点「刷新并纳管」更新状态。'
          : '登录凭据已失效。在 OpenList 中更新登录凭据后，原有挂载点与 Key 授权保持不变；完成后点「刷新并纳管」更新状态。',
        error: '上游暂时无法访问，请检查网盘服务或在 OpenList 中检查存储配置。',
        disabled: disabledDesc(d),
        orphaned: 'OpenList 中已没有该存储。可在 OpenList 中于原挂载点 ' + d.mount_path + ' 重建存储，完成后点「刷新并纳管」即可恢复原有授权；或移除这条映射。',
        replacement_found: '原存储已不存在，挂载点上发现了替代存储。确认前 agent 访问会被拒绝。',
      }[d.state] || C2A.stateDesc(d);
      const state = h('div', { class: 'drive-status status-' + (d.state === 'healthy' ? 'ok' : d.state === 'disabled' ? 'off' : 'warn') },
        h('div', { class: 'drive-status-top' }, C2A.stateBadge(d.state), h('span', { class: 'card-desc' }, '当前状态')),
        h('p', { class: 'drive-status-desc', title: desc + (d.status_text ? ' · ' + d.status_text : '') }, desc));
      card.append(title,
        h('h3', { class: 'drive-name', title: d.id }, d.id),
        h('div', { class: 'drive-mount mono', title: d.mount_path }, d.mount_path), state);
      if (d.state === 'replacement_found') card.append(replacementPanel(d));
      const footActions = h('div', { class: 'cell-actions' });
      /* rebuilding = adding a new storage at the old mount: prefer the login
       * wizard when the patched OpenList is available, else the native admin */
      const rebuildUrl = st.connectUrl || st.manageUrl;
      if (d.state === 'orphaned' && rebuildUrl) {
        footActions.append(C2A.openlistLink(C2A.connectLink(rebuildUrl), '在 OpenList 中重建存储', 'btn btn-ghost btn-sm'));
      }
      footActions.append(driveActions(d));
      card.append(
        h('div', { class: 'drive-meta' },
          h('span', null, icon('keys', 13), '授权 Key ', h('strong', null, String(d.key_count ?? '—')))),
        h('div', { class: 'drive-card-foot' }, h('span', { class: 'drive-created', title: '创建于 ' + C2A.fmtTime(d.created_at) }, C2A.fmtTime(d.created_at)), footActions));
      return card;
    }

    /** replacement_found panel: candidate facts + driver-changed warning +
     * confirm action (POST /drives/{id}/rebind). */
    function replacementPanel(d) {
      const r = d.replacement || {};
      const confirmBtn = h('button', { class: 'btn btn-primary btn-sm', type: 'button', style: 'margin-top:10px' },
        icon('check', 13), '确认使用替代存储');
      confirmBtn.addEventListener('click', () => rebindDialog(d, r));
      return h('div', { class: 'drive-repl' },
        h('div', { class: 'drive-repl-head' },
          h('strong', null, '发现替代存储'),
          h('span', { class: 'mono' }, '#' + (r.storage_id ?? '—')),
          storageStatusBadge(r.status, r.disabled)),
        r.driver_changed
          ? h('div', { class: 'callout callout-danger repl-warn' }, icon('alert', 13),
              h('div', { class: 'co-text' }, '驱动已变化：旧 ', h('strong', null, d.driver || d.provider || '—'), ' → 新 ', h('strong', null, r.driver || '—'), '。Key 授权会沿用，但新驱动的语义可能不同，请确认后再使用。'))
          : null,
        h('div', { class: 'kv-list repl-kv' },
          h('dt', null, '驱动'), h('dd', null, r.driver || '—'),
          h('dt', null, '挂载点'), h('dd', { class: 'mono' }, r.mount_path || d.mount_path),
          h('dt', null, '状态'), h('dd', null, storageStatusBadge(r.status, r.disabled))),
        confirmBtn);
    }

    function rebindDialog(d, r) {
      C2A.confirmModal({
        title: '确认使用替代存储 · ' + d.id,
        message: h('p', { class: 'text-sub', style: 'line-height:1.65' },
          '确认后，已授权该网盘的 Key 将可以访问新存储 #' + (r.storage_id ?? '—') + '（驱动 ' + (r.driver || '—') + '，挂载点 ' + (r.mount_path || d.mount_path) + '）。确认前 agent 访问该网盘会被拒绝。'),
        confirmText: '确认使用',
        onConfirm: async () => {
          await C2A.API.post('/drives/' + encodeURIComponent(d.id) + '/rebind', { storage_id: r.storage_id });
          C2A.toast('网盘 ' + d.id + ' 已重绑到存储 #' + r.storage_id, 'success');
          fullReload();
        },
      });
    }

    /* ---------------- unmanaged storages ---------------- */
    function renderUnmanaged() {
      if (st.unmanaged === null) { C2A.clear(unmanagedCard); return; }
      const storages = st.unmanaged;
      C2A.clear(unmanagedCard);
      unmanagedCard.append(
        h('div', { class: 'card-head' },
          h('span', { class: 'card-title' }, '未纳管的 OpenList 存储'),
          storages.length ? h('span', { class: 'badge badge-warn' }, String(storages.length)) : null,
          h('span', { class: 'card-desc' }, '在 OpenList 中手动创建、cloud2api 尚未跟踪的存储；纳管后才会暴露给 Key')),
        storages.length
          ? h('div', { class: 'table-wrap' }, unmanagedTable(storages))
          : C2A.emptyState({ icon: 'openlist', title: '没有未纳管的存储', sub: '在 OpenList 中新建存储后，点「刷新并纳管」让它出现在这里' }));
    }

    function unmanagedTable(storages) {
      const tbl = h('table', { class: 'tbl' },
        h('thead', null, h('tr', null,
          h('th', null, 'Storage ID'), h('th', null, '挂载点'), h('th', null, '驱动'), h('th', null, '状态'), h('th', { style: 'text-align:right' }, '操作'))));
      const tbody = h('tbody');
      for (const s of storages) {
        const actions = h('div', { class: 'cell-actions' });
        if (s.replacement_for) {
          // the mount belongs to an existing drive — confirm via rebind there
          const go = h('button', { class: 'btn btn-secondary btn-sm', type: 'button', title: '该挂载点已被网盘 ' + s.replacement_for + ' 占用，请在对应网盘上确认使用替代存储' }, '前往网盘 ' + s.replacement_for);
          go.addEventListener('click', () => scrollToDrive(s.replacement_for));
          actions.append(go);
          tbody.append(h('tr', { dataset: { unmanagedId: String(s.storage_id) } },
            h('td', { class: 'mono' }, '#' + s.storage_id),
            h('td', null,
              h('div', { class: 'mono' }, s.mount_path),
              h('div', { class: 'cell-sub' }, '可作为网盘 ' + s.replacement_for + ' 的替代存储')),
            h('td', null, s.driver),
            h('td', null, storageStatusBadge(s.status, s.disabled)),
            h('td', null, actions)));
          continue;
        }
        const adoptBtn = h('button', { class: 'btn btn-secondary btn-sm', type: 'button' }, '纳管');
        adoptBtn.addEventListener('click', () => adoptDialog(s));
        tbody.append(h('tr', { dataset: { unmanagedId: String(s.storage_id) } },
          h('td', { class: 'mono' }, '#' + s.storage_id),
          h('td', { class: 'mono' }, s.mount_path),
          h('td', null, s.driver),
          h('td', null, storageStatusBadge(s.status, s.disabled)),
          h('td', null, h('div', { class: 'cell-actions' }, adoptBtn))));
      }
      tbl.append(tbody);
      return tbl;
    }

    /** Scroll to (and briefly highlight) a managed drive card/row — used by
     * the unmanaged replacement_for entries. */
    function scrollToDrive(id) {
      const el = workspace.querySelector('[data-drive-id="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]');
      if (!el) return;
      el.scrollIntoView({ behavior: 'smooth', block: 'center' });
      el.classList.remove('drive-flash');
      // restart the animation if it is already highlighted
      void el.offsetWidth;
      el.classList.add('drive-flash');
      setTimeout(() => el.classList.remove('drive-flash'), 2200);
    }

    /** Slug for ID suggestions: lowercase, runs of characters outside
     * [a-z0-9] become '-', trimmed. Only results with ≥2 chars and at least
     * one letter qualify (the portal convention for drive IDs). */
    function slugFor(raw) {
      const s = String(raw || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '');
      return (s.length >= 2 && /[a-z]/.test(s)) ? s.slice(0, 40) : '';
    }

    /** Suggested drive ID for the adopt dialog: (a) slug of the mount path's
     * last segment, else (b) the connect platform id whose driver matches the
     * storage's driver, else (c) slug of the driver name, else "drive";
     * uniquified against the managed drive IDs with -2/-3 suffixes. The input
     * stays editable — the value is pre-filled and selected. */
    function suggestedDriveId(s) {
      const segs = String(s.mount_path || '').split('/');
      let base = slugFor(segs[segs.length - 1]);
      if (!base) {
        const plat = st.platforms.find((p) => p && p.id &&
          String(p.driver || '').toLowerCase() === String(s.driver || '').toLowerCase());
        base = plat ? String(plat.id).toLowerCase().slice(0, 40) : '';
      }
      if (!base) base = slugFor(s.driver) || 'drive';
      const taken = {};
      for (const d of st.drives) taken[d.id] = true;
      let id = base;
      for (let n = 2; taken[id]; n++) id = base + '-' + n;
      return id;
    }

    function adoptDialog(s) {
      const idInput = h('input', { class: 'input', type: 'text', value: suggestedDriveId(s), placeholder: '例如 movies-main', spellcheck: 'false', autocomplete: 'off' });
      const errBox = h('div', { class: 'field-error' });
      let busy = false;
      C2A.modal({
        title: '纳管存储 #' + s.storage_id,
        body(body) {
          body.append(
            h('div', { class: 'callout callout-neutral mb-14' }, icon('info', 14),
              h('div', { class: 'co-text' }, '纳管只记录「网盘 ID ↔ 存储 #' + s.storage_id + '」的映射，不修改 OpenList 中的任何配置，也不会给任何 Key 追加授权。')),
            h('div', { class: 'kv-list mb-14' },
              h('dt', null, '挂载点'), h('dd', { class: 'mono' }, s.mount_path),
              h('dt', null, '驱动'), h('dd', null, s.driver),
              h('dt', null, '状态'), h('dd', { class: 'mono' }, s.disabled ? s.status + '（停用）' : s.status)),
            h('div', { class: 'field' },
              h('label', { class: 'field-label' }, '网盘 ID', h('span', { class: 'req' }, '*')),
              idInput,
              h('div', { class: 'field-help' }, '已按挂载点自动填写，可直接纳管；用于 API Key 授权和 /v1/drives/<ID>/ 路径，只能用小写字母、数字和中划线。')),
            errBox);
        },
        foot(foot, close) {
          const btn = h('button', { class: 'btn btn-primary', type: 'button' }, '纳管');
          btn.addEventListener('click', async () => {
            if (busy) return;
            errBox.textContent = '';
            const id = idInput.value.trim();
            if (!id) { errBox.textContent = '请填写网盘 ID'; return; }
            busy = true; btn.disabled = true; btn.textContent = '提交中…';
            try {
              // the row comes back with the storage's REAL state (may be error/disabled)
              const row = await C2A.API.post('/drives/adopt', { storage_id: s.storage_id, id: id });
              C2A.toast('已纳管；请在 API Keys 中授权需要访问它的 Key', 'success');
              if (row && row.state && row.state !== 'healthy') {
                const lbl = (C2A.DRIVE_STATE[row.state] || {}).label || row.state;
                C2A.toast('已纳管，但存储当前状态：' + lbl + (row.status_text && row.status_text !== 'work' ? '（' + row.status_text + '）' : ''), 'warn', 6000);
              }
              close();
              fullReload();
            } catch (e) {
              errBox.textContent = e.message + '（候选可能已变化，请刷新后重试）';
              busy = false; btn.disabled = false; btn.textContent = '纳管';
            }
          });
          foot.append(
            h('button', { class: 'btn btn-secondary', type: 'button', onclick: () => close() }, '取消'),
            btn);
        },
      });
      // the suggestion is pre-filled AND selected: typing replaces it, but the
      // ID stays fully editable
      idInput.select();
    }

    /* ---------------- drives table ---------------- */
    function drivesTable(drives) {
      const tbl = h('table', { class: 'tbl' },
        h('thead', null, h('tr', null,
          h('th', null, '网盘'), h('th', null, '驱动'), h('th', null, '挂载点'), h('th', null, '状态'),
          h('th', { style: 'text-align:right' }, 'Key 数'), h('th', null, '创建时间'), h('th', { style: 'text-align:right' }, '操作'))));
      const tbody = h('tbody');
      for (const d of drives) tbody.append(driveRow(d));
      tbl.append(tbody);
      return tbl;
    }

    function driveRow(d) {
      const actions = driveActions(d);
      const idCell = h('div', null,
        h('div', { class: 'cell-main mono' }, d.id),
        d.static ? h('div', { class: 'cell-sub' }, h('span', { class: 'badge badge-accent', style: 'font-size:10.5px;padding:2px 7px' }, '静态 config.yaml')) : null);

      /* state cell: badge + one-line explanation (+ quick actions) */
      const stateCell = h('td', null, C2A.stateBadge(d.state));
      const desc = C2A.stateDesc(d);
      if (desc && d.state !== 'healthy') {
        const sub = h('div', { class: 'cell-sub state-desc', title: desc + (d.status_text ? '（' + d.status_text + '）' : '') }, desc);
        stateCell.append(sub);
        const quick = h('div', { class: 'mt-8' });
        if (d.state === 'replacement_found' && d.replacement) {
          const b = h('button', { class: 'btn btn-primary btn-xs', type: 'button', title: '确认后已授权的 Key 将可以访问新存储' }, icon('check', 12), '确认使用替代存储');
          b.addEventListener('click', () => rebindDialog(d, d.replacement));
          quick.append(b);
        }
        if (d.state === 'needs_reauth' && d.openlist_reauth_url) {
          quick.append(reauthEntry(d, d.openlist_reauth_url, 'btn btn-primary btn-xs'));
        }
        if (d.state === 'orphaned' && (st.connectUrl || st.manageUrl)) {
          const b = C2A.openlistLink(C2A.connectLink(st.connectUrl || st.manageUrl), '在 OpenList 中重建存储', 'btn btn-ghost btn-xs');
          quick.append(b);
          const b2 = h('button', { class: 'btn btn-ghost btn-xs', type: 'button', title: '仅删除 cloud2api 侧的映射（OpenList 存储已不存在）' }, icon('trash', 12), '移除映射');
          b2.addEventListener('click', () => releaseDriveDialog(d));
          quick.append(b2);
        }
        if (quick.children.length) stateCell.append(quick);
      }

      const tr = h('tr', { class: (d.state === 'orphaned' || d.state === 'disabled') ? 'row-muted' : null, dataset: { driveId: d.id } },
        h('td', null, idCell),
        h('td', null, h('div', null, d.driver || '—'), h('div', { class: 'cell-sub' }, d.provider)),
        h('td', { class: 'mono' }, d.mount_path),
        stateCell,
        h('td', { class: 'num', style: 'text-align:right' }, String(d.key_count ?? '—')),
        h('td', { class: 'nowrap text-muted num' }, C2A.fmtTime(d.created_at)),
        h('td', null, actions));
      return tr;
    }

    /** Per-drive actions: OpenList links (reauth wizard / edit / update
     * credentials / rebuild) + cloud2api-side enable/disable and release.
     * When the patched OpenList reports a login adapter for the storage's
     * driver (`openlist_reauth_url`) 重新授权 is the primary fix for
     * needs_reauth and the native editor becomes the「高级」fallback; without
     * it the labels fall back to the native-form behaviour.
     * 启用/停用 only flips cloud2api's own `enabled` switch (whether agents
     * can reach the drive through cloud2api); it never touches OpenList.
     * When the storage is disabled in OpenList (`storage_disabled`) the fix
     * lives in OpenList, so there is no cloud2api enable button for it. */
    function driveActions(d) {
      const actions = h('div', { class: 'cell-actions' });
      const storageOff = d.storage_disabled === true;
      const reauthUrl = d.openlist_reauth_url || '';
      const editLabel = reauthUrl ? '在 OpenList 中编辑（高级）'
        : d.state === 'needs_reauth' ? '在 OpenList 中更新登录凭据'
          : storageOff ? '在 OpenList 中启用该存储' : '在 OpenList 中编辑';
      const editTitle = storageOff ? '该存储已在 OpenList 中停用：打开 OpenList 后台重新启用它'
        : reauthUrl ? 'OpenList 原生存储表单：驱动参数、挂载点等高级配置（重新授权请用「重新授权」）'
          : '在 OpenList 后台编辑该存储（驱动参数、凭据、挂载点、启停）';
      if (reauthUrl) {
        actions.append(reauthEntry(d, reauthUrl, d.state === 'needs_reauth' ? 'btn btn-primary btn-sm' : 'btn btn-ghost btn-sm'));
      }
      if (d.openlist_edit_url) {
        actions.append(C2A.openlistLink(d.openlist_edit_url, editLabel, 'btn btn-ghost btn-sm'));
      }
      const items = [];
      if (reauthUrl) {
        items.push(st.handoff
          ? { label: '重新授权', icon: 'external', title: REAUTH_TITLE, onSelect: () => openReauthHandoff(d, reauthUrl) }
          : { label: '重新授权（手动登录）', icon: 'external', title: REAUTH_TITLE, onSelect: () => window.open(C2A.connectLink(reauthUrl), '_blank', 'noopener') });
      }
      if (d.openlist_edit_url) {
        items.push({ label: editLabel, icon: 'external', title: editTitle, onSelect: () => window.open(d.openlist_edit_url, '_blank', 'noopener') });
      }
      if (d.state === 'orphaned' && (st.connectUrl || st.manageUrl)) {
        // rebuilding = adding a new storage at the old mount; the replacement
        // confirmation flow stays on this page either way
        const rebuildUrl = st.connectUrl || st.manageUrl;
        items.push({ label: '在 OpenList 中重建存储', icon: 'external', title: '在 OpenList 后台于原挂载点 ' + d.mount_path + ' 重建存储，完成后点「刷新并纳管」', onSelect: () => window.open(C2A.connectLink(rebuildUrl), '_blank', 'noopener') });
      }
      const staticNote = '静态网盘（config.yaml 导入）不能在门户修改，请编辑 config.yaml 后重启';
      if (d.state === 'orphaned') {
        items.push({ label: '启用', icon: 'power', disabled: true, title: '存储已删除，无法启用；请在 OpenList 中重建存储后「刷新并纳管」，或移除映射' });
      } else if (d.enabled === false) {
        items.push({
          label: '启用', icon: 'power', disabled: !!d.static,
          title: d.static ? staticNote : '恢复 agent 通过 cloud2api 访问该网盘；不改变 OpenList 中的存储',
          onSelect: () => toggleDrive(d, true),
        });
      } else if (!storageOff) {
        items.push({ label: '停用', icon: 'power', disabled: !!d.static, title: d.static ? staticNote : '停用后 Key 授权保留，但 agent 无法再通过 cloud2api 访问', onSelect: () => toggleDrive(d, false) });
      }
      items.push({
        label: d.state === 'orphaned' ? '移除映射' : '移出',
        icon: 'trash', danger: true, disabled: !!d.static,
        title: d.static ? staticNote : '只移除 cloud2api 的映射，OpenList 中的存储保持不变',
        onSelect: () => releaseDriveDialog(d),
      });
      actions.append(C2A.menu(items));
      return actions;
    }

    /** 启用/停用 only flips cloud2api's own switch: whether agents can access
     * the drive through cloud2api. OpenList storage is never touched and key
     * grants are kept. The endpoint returns the updated row — use it to
     * refresh the UI in place. */
    function toggleDrive(d, enable) {
      const what = enable ? '启用' : '停用';
      C2A.confirmModal({
        title: what + '网盘 · ' + d.id,
        message: h('p', { class: 'text-sub', style: 'line-height:1.65' },
          enable
            ? '启用只影响 cloud2api：已授权的 Key 将恢复通过 cloud2api 访问该网盘。不会改动 OpenList 中的存储，现有 Key 授权保持不变。'
            : '停用只影响 cloud2api：agent 将无法再通过 cloud2api 访问该网盘（/v1 返回 503）。不会改动 OpenList 中的存储，现有 Key 授权保留，随时可再次启用。'),
        confirmText: what,
        onConfirm: async () => {
          const row = await C2A.API.post('/drives/' + encodeURIComponent(d.id) + '/' + (enable ? 'enable' : 'disable'));
          if (row && row.id) {
            const i = st.drives.findIndex((x) => x.id === d.id);
            if (i >= 0) st.drives[i] = row;
            renderAll();
          } else {
            fullReload();
          }
          C2A.toast('网盘 ' + d.id + ' 已' + what, 'success');
        },
      });
    }

    /** 移出网盘 = 只删除 cloud2api 的映射（DELETE /drives/{id}，始终只移出映射）。
     * OpenList 中的存储原样保留（会回到「未纳管」列表，可再次纳管）；
     * 删除存储本身请在 OpenList 后台操作。 */
    function releaseDriveDialog(d) {
      const orphaned = d.state === 'orphaned' || !d.storage_id;
      let busy = false;
      C2A.modal({
        title: (orphaned ? '移除映射 ' : '移出网盘 ') + d.id,
        body(body) {
          body.append(
            h('p', { class: 'text-sub', style: 'line-height:1.6' },
              orphaned
                ? '该网盘在 OpenList 中已没有对应存储，此操作仅删除 cloud2api 侧的映射。'
                : h('span', null,
                    '只移除 cloud2api 的映射：使用该网盘的 Key（', h('span', { class: 'mono' }, String(d.key_count ?? 0)), ' 个）将无法继续访问，agent 侧会收到 404 drive_not_found。OpenList 中的存储 ', h('span', { class: 'mono' }, '#' + d.storage_id), ' 保持不变，之后会出现在「未纳管」列表，可再次纳管。')),
            !orphaned && d.openlist_edit_url
              ? h('div', { class: 'callout callout-neutral mt-14' }, icon('info', 14),
                  h('div', { class: 'co-text' }, '要删除 OpenList 中的存储配置，请在 OpenList 后台操作（网盘里的文件不受影响）：',
                    h('div', { class: 'mt-8' }, C2A.openlistLink(d.openlist_edit_url, '在 OpenList 中编辑', 'btn btn-secondary btn-sm'))))
              : null);
        },
        foot(foot, close) {
          const btn = h('button', { class: 'btn btn-danger', type: 'button' }, orphaned ? '移除映射' : '移出网盘');
          btn.addEventListener('click', async () => {
            if (busy) return;
            busy = true; btn.disabled = true; btn.textContent = '提交中…';
            try {
              await C2A.API.call('DELETE', '/drives/' + encodeURIComponent(d.id));
              C2A.toast(orphaned
                ? '网盘 ' + d.id + ' 的映射已移除'
                : '网盘 ' + d.id + ' 已移出（OpenList 存储保留，可再次纳管）', 'success');
              close();
              fullReload();
            } catch (e) {
              busy = false; btn.disabled = false; btn.textContent = orphaned ? '移除映射' : '移出网盘';
              C2A.toast(e.message, 'error');
            }
          });
          foot.append(
            h('button', { class: 'btn btn-secondary', type: 'button', onclick: () => close() }, '取消'),
            btn);
        },
      });
    }

    await loadInitial();
  };
})();
