/* cloud2api admin — pages: 总览 / 请求 / 审计 / OpenList / 设置 */
(function () {
  'use strict';
  const C2A = window.C2A;
  const { h, icon } = C2A;
  const pages = (C2A.pages = C2A.pages || {});

  function head(title, sub, actions) {
    return h('div', { class: 'page-head' },
      h('div', { class: 'page-head-text' }, h('h2', null, title), sub ? h('div', { class: 'sub' }, sub) : null),
      actions ? h('div', { class: 'page-head-actions' }, actions) : null);
  }
  C2A.pageHead = head;

  /** 登录向导 (patched OpenList /@connect) status text from the overview
   * connect_* fields: enabled (version + supported platforms), not detected
   * (native OpenList forms only), or the status probe failed. Platforms
   * without a real-account verification yet are marked 未验证. */
  function wizardStatus(ol) {
    if (ol.connect_error) return { cls: 'text-warn', text: '状态读取失败：' + ol.connect_error };
    if (ol.connect_url) {
      const names = ((ol.connect_platforms || []).map((p) => {
        if (!p || !p.name) return null;
        return p.verified === false ? p.name + '（未验证）' : p.name;
      }).filter(Boolean)).join('、');
      return { cls: '', text: '已启用（connect ' + (ol.connect_version || '?') + (names ? '，支持 ' + names : '') + '）' };
    }
    return { cls: 'text-muted', text: '未检测到（使用 OpenList 原生表单）' };
  }

  /* ===================== 总览 ===================== */
  pages.overview = async function (root) {
    root.append(C2A.loadingBlock());
    let data;
    try {
      data = await C2A.API.get('/overview');
    } catch (e) {
      C2A.clear(root);
      root.append(C2A.errorState(e.message, () => { C2A.clear(root); pages.overview(root); }));
      return;
    }
    if (C2A.currentRoute && C2A.currentRoute() !== 'overview') return;
    C2A.clear(root);

    const ol = data.openlist || {};
    const c = data.counts || {};
    const olVersion = String(ol.version || '').split(' - ')[0] || '未知版本';

    /* quick actions: OpenList is where drives are added; sync discovers them.
     * With the handoff-capable module the single 添加网盘 opens the choice
     * modal (网盘登录 for every platform, or API 上游连接 = another GoAPI
     * instance entered only in OpenList) and mints the wizard URL per click.
     * An older module falls back to the plain manual link — no modal, since
     * it has no /new/goapi route and cannot honour minted tickets. */
    const quickActions = [];
    if (ol.connect_url) {
      if (ol.connect_handoff_available === true) {
        const add = h('button', { class: 'btn btn-primary', type: 'button', title: '网盘登录或 API 上游连接，都在 OpenList 登录向导中完成' },
          icon('plus', 14), '添加网盘');
        add.addEventListener('click', () => C2A.addDriveChoices({ connectUrl: ol.connect_url, manageUrl: ol.manage_url || '' }));
        quickActions.push(add);
      } else {
        const manual = C2A.openlistLink(C2A.connectLink(ol.connect_url), '手动登录 OpenList', 'btn btn-primary');
        if (manual) manual.title = '打开 OpenList 登录向导；当前登录模块不支持自动登录，API 上游连接不可用';
        quickActions.push(manual);
      }
      if (ol.manage_url) {
        const native = C2A.openlistLink(ol.manage_url, '在 OpenList 中手动添加', 'btn btn-secondary');
        if (native) native.title = '其他网盘或高级参数使用 OpenList 原生表单';
        quickActions.push(native);
      }
    } else if (ol.manage_url) {
      quickActions.push(C2A.openlistLink(ol.manage_url, '在 OpenList 中添加网盘', 'btn btn-primary'));
    } else {
      quickActions.push(h('button', {
        class: 'btn btn-primary', type: 'button',
        title: ol.public_url_error ? 'OpenList 地址无效：' + ol.public_url_error : '未配置 OpenList 公开访问地址，先到设置页填写',
        onclick: () => { location.hash = '#/settings'; },
      }, icon('settings', 14), '配置 OpenList 访问地址'));
    }
    quickActions.push(h('button', {
      class: 'btn btn-secondary', type: 'button',
      title: '打开网盘页并立即同步一次 OpenList 存储',
      onclick: () => { C2A.pendingDriveSync = true; location.hash = '#/drives'; },
    }, icon('refresh', 14), '刷新并纳管'));

    const statGrid = h('div', { class: 'stat-grid' },
      h('a', { class: 'stat-card', href: '#/drives' },
        h('div', { class: 'stat-top' }, '网盘', h('span', { class: 'icon-chip' }, icon('drives', 16))),
        h('div', { class: 'stat-num' }, String(c.drives ?? '—')),
        h('div', { class: 'stat-sub' }, '已纳管的 OpenList 存储')),
      h('a', { class: 'stat-card', href: '#/keys' },
        h('div', { class: 'stat-top' }, '有效的 API Key', h('span', { class: 'icon-chip' }, icon('keys', 16))),
        h('div', { class: 'stat-num' }, String(c.keys ?? '—')),
        h('div', { class: 'stat-sub' }, '未吊销且未过期的 Key')));

    /* OpenList status card */
    const olCard = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('span', null, icon('openlist', 16)),
        h('span', { class: 'card-title' }, 'OpenList'),
        h('span', { class: 'card-desc mono', title: ol.version || '' }, olVersion),
        h('div', { class: 'card-head-actions' },
          ol.reachable ? C2A.stateBadge('healthy') : h('span', { class: 'badge badge-danger' }, h('span', { class: 'dot' }), '不可达'),
          ol.token_ok === true ? h('span', { class: 'badge badge-ok' }, '令牌有效')
            : ol.token_ok === false ? h('span', { class: 'badge badge-danger' }, '令牌失效')
              : h('span', { class: 'badge badge-slate' }, '令牌状态未知'),
          ol.manage_url ? C2A.openlistLink(ol.manage_url, '打开 OpenList 管理后台') : null)),
      h('div', { class: 'card-body', style: 'display:flex;flex-wrap:wrap;gap:10px 26px;font-size:13px' },
        h('div', null, h('span', { class: 'text-muted' }, '连接状态：'), ol.reachable ? '可达' : '不可达'),
        h('div', null, h('span', { class: 'text-muted' }, '管理令牌：'), ol.token_ok === undefined || ol.token_ok === null ? '未知' : ol.token_ok ? '有效' : '已失效（可配置用户名密码自动恢复）'),
        h('div', null, h('span', { class: 'text-muted' }, '游客用户：'), ol.guest_enabled === undefined ? '未知' : ol.guest_enabled ? h('span', { style: 'color:var(--warn);font-weight:600' }, '已启用（存在风险）') : '已关闭'),
        (() => {
          const wiz = wizardStatus(ol);
          return h('div', null, h('span', { class: 'text-muted' }, '登录向导：'), wiz.cls ? h('span', { class: wiz.cls }, wiz.text) : wiz.text);
        })(),
        (() => {
          const kids = [h('span', { class: 'text-muted' }, '访问地址：')];
          if (ol.public_url) kids.push(h('span', { class: 'mono' }, ol.public_url));
          else kids.push('未配置（网盘入口按钮会引导到设置页）');
          if (ol.public_url_error) {
            kids.push(h('span', { class: 'text-warn', style: 'margin-left:6px' }, '地址无效：' + ol.public_url_error));
            kids.push(h('a', { href: '#/settings', style: 'margin-left:6px' }, '去设置修改'));
          }
          return h('div', null, kids);
        })()));

    /* warnings card */
    const warnings = data.warnings || [];
    const warnCard = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('span', null, icon('alert', 16)),
        h('span', { class: 'card-title' }, '需要关注'),
        warnings.length ? h('span', { class: 'badge badge-warn' }, String(warnings.length)) : null),
      warnings.length
        ? h('div', { class: 'card-body' },
            h('div', { class: 'warning-list' }, warnings.map((w) =>
              h('div', { class: 'warning-item warning-lv-' + (w.level === 'error' ? 'error' : w.level === 'info' ? 'info' : 'warning') },
                icon(w.level === 'error' ? 'xcircle' : w.level === 'info' ? 'info' : 'alert', 15),
                h('div', { class: 'wi-text' }, w.text),
                w.action_url ? h('a', { class: 'wi-act btn btn-ghost btn-sm', href: w.action_url }, '前往处理 ', icon('external', 12)) : null))))
        : C2A.emptyState({ icon: 'check', title: '一切正常', sub: '当前没有需要处理的警告' }));

    root.append(head('总览', '网盘与 API Key 的整体状态', quickActions), statGrid, olCard, warnCard);
  };

  /* ===================== 审计 ===================== */
  const auditState = { filters: null };

  pages.audit = async function (root) {
    C2A.clear(root);
    const st = auditState.filters || { key: '', drive: '', op: '', status: '' };
    auditState.applied = Object.assign({}, st);

    const keyIn = h('input', { class: 'input', value: st.key, placeholder: '全部 Key' });
    const driveIn = h('input', { class: 'input', value: st.drive, placeholder: '全部网盘' });
    const opSel = h('select', { class: 'select' }, h('option', { value: '' }, '全部操作'));
    const stSel = h('select', { class: 'select' }, h('option', { value: '' }, '全部状态'));
    // options get filled from first load

    const filterCard = h('div', { class: 'card mb-14' },
      h('div', { class: 'card-body', style: 'padding:14px 18px' },
        h('form', { class: 'filterbar', onsubmit: (e) => { e.preventDefault(); apply(); } },
          h('div', { class: 'field' }, h('label', { class: 'field-label' }, 'Key'), keyIn),
          h('div', { class: 'field' }, h('label', { class: 'field-label' }, '网盘'), driveIn),
          h('div', { class: 'field' }, h('label', { class: 'field-label' }, '操作'), opSel),
          h('div', { class: 'field' }, h('label', { class: 'field-label' }, '状态码'), stSel),
          h('button', { class: 'btn btn-primary', type: 'submit' }, icon('search', 13), '查询'),
          h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => { keyIn.value = driveIn.value = ''; opSel.value = ''; stSel.value = ''; apply(); } }, '重置'))));

    const tableCard = h('div', { class: 'card' });
    const tableHost = h('div', { class: 'card-body flush' });
    tableCard.append(tableHost);
    const moreRow = h('div', { style: 'display:flex;justify-content:center;padding:12px' });

    /* Fill the op/status dropdowns from the ops actually seen in the log.
     * One wider probe (independent of the active filters) so the list covers
     * all 1b ops (selftest/transfer/fetch/batch/request/keys/…), not just the
     * current page. Falls back to filling from the first result page. */
    let optionsFilled = false;
    async function fillFilterOptions() {
      if (optionsFilled) return;
      optionsFilled = true;
      let probe = null;
      try { probe = await C2A.API.get('/audit?limit=100'); } catch (e) { probe = null; }
      const entries = (probe && probe.entries) || [];
      const ops = Array.from(new Set(entries.map((e) => e.op).filter(Boolean))).sort();
      const sts = Array.from(new Set(entries.map((e) => String(e.status)))).sort();
      const keepOp = opSel.value, keepSt = stSel.value;
      C2A.clear(opSel); opSel.append(h('option', { value: '' }, '全部操作'));
      C2A.clear(stSel); stSel.append(h('option', { value: '' }, '全部状态'));
      for (const o of ops) opSel.append(h('option', { value: o }, o));
      for (const s of sts) stSel.append(h('option', { value: s }, s));
      opSel.value = keepOp; stSel.value = keepSt;
    }

    function apply() {
      auditState.filters = { key: keyIn.value.trim(), drive: driveIn.value.trim(), op: opSel.value, status: stSel.value };
      auditState.applied = Object.assign({}, auditState.filters);
      load(null);
    }

    function queryFor(before) {
      const f = auditState.applied;
      const q = new URLSearchParams();
      if (f.key) q.set('key', f.key);
      if (f.drive) q.set('drive', f.drive);
      if (f.op) q.set('op', f.op);
      if (f.status) q.set('status', f.status);
      if (before) q.set('before', before);
      q.set('limit', '20');
      return q.toString();
    }

    async function load(before, append) {
      if (!append) {
        C2A.clear(tableHost);
        tableHost.append(C2A.tableSkeleton(9, 6));
        C2A.clear(moreRow);
      }
      let data;
      try {
        data = await C2A.API.get('/audit?' + queryFor(before));
      } catch (e) {
        C2A.clear(tableHost);
        tableHost.append(h('div', { class: 'card-body' }, C2A.errorState(e.message, () => load(before, append))));
        return;
      }
      const entries = data.entries || [];
      if (!append) C2A.clear(tableHost);
      if (!entries.length && !append) {
        tableHost.append(C2A.emptyState({ icon: 'audit', title: '没有匹配的审计记录', sub: '调整筛选条件后重试' }));
      } else {
        if (!append) {
          tableHost.append(h('div', { class: 'table-wrap' }, buildTable(entries)));
        } else {
          const tbody = tableHost.querySelector('tbody');
          for (const e of entries) tbody.append(auditRow(e));
        }
        // fallback when the wide probe failed: fill from this page
        if (!before && !append && !optionsFilled && opSel.options.length <= 1) {
          const ops = Array.from(new Set(entries.map((e) => e.op).filter(Boolean))).sort();
          for (const o of ops) opSel.append(h('option', { value: o }, o));
          const sts = Array.from(new Set(entries.map((e) => String(e.status)))).sort();
          for (const s of sts) stSel.append(h('option', { value: s }, s));
        }
      }
      C2A.clear(moreRow);
      if (data.next_before) {
        moreRow.append(h('button', {
          class: 'btn btn-secondary btn-sm', type: 'button',
          onclick: (e) => { e.target.disabled = true; e.target.textContent = '加载中…'; load(data.next_before, true); },
        }, '加载更多'));
        tableHost.append(moreRow);
      }
    }

    root.append(head('审计', '数据面的每次调用都会记录在此；日志按天滚动保留'), filterCard, tableCard);
    fillFilterOptions();
    await load(null, false);
  };

  function buildTable(entries) {
    const tbl = h('table', { class: 'tbl' },
      h('thead', null, h('tr', null,
        h('th', null, '时间'), h('th', null, 'Key'), h('th', null, '网盘'), h('th', null, '操作'),
        h('th', null, '路径'), h('th', { style: 'text-align:right' }, '大小'), h('th', null, '状态'),
        h('th', { style: 'text-align:right' }, '耗时'), h('th', null, '错误'))));
    const tbody = h('tbody');
    for (const e of entries) tbody.append(auditRow(e));
    tbl.append(tbody);
    return tbl;
  }
  function auditRow(e) {
    const cls = e.status >= 500 ? 'status-5xx' : e.status >= 400 ? 'status-4xx' : 'status-2xx';
    return h('tr', null,
      h('td', { class: 'nowrap text-muted num' }, C2A.fmtTime(e.ts)),
      h('td', { class: 'mono' }, e.key_id),
      h('td', { class: 'mono' }, e.drive || '—'),
      h('td', null, h('span', { class: 'chip' }, e.op || '—')),
      h('td', null, h('div', { class: 'path-cell', title: e.path || '' }, e.path || '—')),
      h('td', { class: 'num nowrap', style: 'text-align:right' }, e.bytes ? C2A.fmtBytes(e.bytes) : '—'),
      h('td', null, h('span', { class: 'status-code ' + cls }, String(e.status))),
      h('td', { class: 'num nowrap text-muted', style: 'text-align:right' }, C2A.fmtDuration(e.duration_ms)),
      h('td', null, e.error ? h('div', { class: 'path-cell', style: 'color:var(--danger)', title: e.error }, e.error) : h('span', { class: 'text-muted' }, '—')));
  }

  /* ===================== OpenList ===================== */
  /** major.minor comparison for the tested-version check (§3.7.6). */
  function verShort(v) {
    const m = String(v || '').match(/v?(\d+)\.(\d+)/);
    return m ? m[1] + '.' + m[2] : null;
  }

  pages.openlist = async function (root) {
    C2A.clear(root);
    root.append(C2A.loadingBlock());
    let ov, ev, drivesData;
    try {
      [ov, ev, drivesData] = await Promise.all([
        C2A.API.get('/overview'),
        C2A.API.getSoft('/events?limit=50'), // null until Phase 1c ships
        C2A.API.getSoft('/drives').catch(() => null),
      ]);
    } catch (e) {
      C2A.clear(root);
      root.append(C2A.errorState(e.message, () => { C2A.clear(root); pages.openlist(root); }));
      return;
    }
    if (C2A.currentRoute && C2A.currentRoute() !== 'openlist') return;
    C2A.clear(root);
    const ol = ov.openlist || {};
    const ovWarnings = (ov.warnings || []).filter((w) => /openlist|令牌|游客|存储/i.test(w.text));

    /* version row content: version + tested_version match check */
    const tested = ol.tested_version || null;
    const ver = verShort(ol.version);
    const testedShort = verShort(tested);
    const verMismatch = testedShort && ver && ver !== testedShort;
    const verEl = h('dd', { class: 'mono' },
      ol.version ? String(ol.version).split(' - ')[0] : '未知',
      tested && !verMismatch ? h('span', { class: 'badge badge-ok', style: 'margin-left:8px', title: 'cloud2api 已在 ' + tested + ' 上验证' }, '已验证 ' + tested) : null,
      verMismatch ? h('span', { class: 'badge badge-warn', style: 'margin-left:8px', title: 'OpenList 版本（' + ver + '）与 cloud2api 已验证版本（' + testedShort + '）主次版本不一致；功能大多兼容，但建议关注更新说明' }, '未验证版本') : null);

    const statusCard = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('span', null, icon('openlist', 16)),
        h('span', { class: 'card-title' }, '运行状态'),
        h('div', { class: 'card-head-actions' },
          ol.connect_url ? C2A.openlistLink(C2A.connectLink(ol.connect_url), '添加网盘', 'btn btn-secondary') : null,
          ol.manage_url ? C2A.openlistLink(ol.manage_url, '打开后台') : null)),
      h('div', { class: 'card-body' },
        h('dl', { class: 'kv-list' },
          h('dt', null, '连接'), h('dd', null, ol.reachable
            ? h('span', { class: 'badge badge-ok' }, h('span', { class: 'dot' }), '可达')
            : h('span', { class: 'badge badge-danger' }, h('span', { class: 'dot' }), '不可达')),
          h('dt', null, '版本'), verEl,
          h('dt', null, '管理令牌'), h('dd', null, ol.token_ok === undefined || ol.token_ok === null
            ? h('span', { class: 'badge badge-slate' }, '未知')
            : ol.token_ok ? h('span', { class: 'badge badge-ok' }, '有效') : h('span', { class: 'badge badge-danger' }, '已失效')),
          h('dt', null, '游客用户'), h('dd', null, ol.guest_enabled === undefined
            ? h('span', { class: 'badge badge-slate' }, '未知')
            : ol.guest_enabled ? h('span', { class: 'badge badge-warn' }, '已启用') : h('span', { class: 'badge badge-ok' }, '已关闭')),
          (() => {
            const wiz = wizardStatus(ol);
            return [h('dt', null, '登录向导'), h('dd', null, wiz.cls ? h('span', { class: wiz.cls }, wiz.text) : wiz.text)];
          })(),
          h('dt', null, '已纳管网盘'), h('dd', null,
            h('a', { href: '#/drives' }, String((drivesData && drivesData.drives || []).length) + ' 个（前往网盘页）')),
          h('dt', null, '访问地址'), h('dd', { class: 'mono' },
            ol.public_url || '未配置（OpenList 未对外发布，属安全默认）'),
          ol.public_url_error ? h('dt', null, '地址检查') : null,
          ol.public_url_error ? h('dd', null,
            h('span', { class: 'text-warn' }, ol.public_url_error + ' '),
            h('a', { href: '#/settings' }, '去设置修改')) : null)));

    const secCard = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('span', null, icon('alert', 15)),
        h('span', { class: 'card-title' }, '安全检查'),
        h('span', { class: 'card-desc' }, '只读检查，cloud2api 不会修改 OpenList 的全局设置')),
      h('div', { class: 'card-body' },
        h('div', { class: 'warning-list' },
          ovWarnings.length ? ovWarnings.map((w) =>
            h('div', { class: 'warning-item warning-lv-' + (w.level === 'error' ? 'error' : w.level === 'info' ? 'info' : 'warning') },
              icon(w.level === 'error' ? 'xcircle' : w.level === 'info' ? 'info' : 'alert', 15),
              h('div', { class: 'wi-text' }, w.text),
              w.action_url ? h('a', { class: 'wi-act btn btn-ghost btn-sm', href: w.action_url }, '前往') : null))
            : h('div', { class: 'warning-item' }, icon('check', 15), h('div', { class: 'wi-text' }, '未发现配置风险')))));

    const evList = ev ? (ev.events || []) : null;
    const evCard = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('span', null, icon('rotate', 15)),
        h('span', { class: 'card-title' }, '联动事件'),
        h('span', { class: 'card-desc' }, '对账循环发现的双向联动记录（挂载点变更、停用/删除、纳管等）')),
      evList === null
        ? h('div', { class: 'card-body' }, C2A.comingSoon({
            title: '联动事件将在后续版本启用',
            sub: 'OpenList 对账事件（改名、停用、删除、纳管）的记录接口尚未开放',
          }))
        : evList.length
          ? h('div', { class: 'event-list' }, evList.map((e) =>
              h('div', { class: 'event-item' },
                h('div', { class: 'event-time' }, C2A.fmtTime(e.ts)),
                h('div', { class: 'event-body' },
                  h('div', { class: 'flex items-center gap-8 flex-wrap' },
                    C2A.eventKindBadge(e.kind),
                    e.drive && e.drive !== '-'
                      ? h('a', { class: 'event-drive', href: '#/drives', title: '查看网盘列表中的 ' + e.drive }, e.drive)
                      : h('span', { class: 'event-drive' }, e.drive || '—')),
                  h('div', { class: 'event-detail' }, e.detail)))))
          : C2A.emptyState({ icon: 'rotate', title: '暂无联动事件', sub: '在 OpenList 中修改存储配置后，事件会出现在这里' }));

    root.append(head('OpenList', '上游网盘聚合服务的连接、安全与联动状态'), statusCard, secCard, evCard);
  };

  /* ===================== 设置 ===================== */
  pages.settings = async function (root) {
    C2A.clear(root);
    root.append(C2A.loadingBlock());
    let s;
    try {
      s = await C2A.API.get('/settings');
    } catch (e) {
      C2A.clear(root);
      root.append(C2A.errorState(e.message, () => { C2A.clear(root); pages.settings(root); }));
      return;
    }
    if (C2A.currentRoute && C2A.currentRoute() !== 'settings') return;
    C2A.clear(root);

    const baseIn = h('input', { class: 'input', type: 'text', value: s.public_base_url || '', placeholder: '例如 https://c2a.example.com（留空按请求 Host 自动生成）', spellcheck: 'false' });
    const olPubErr = h('div', { class: 'field-error' });
    const olPubIn = h('input', { class: 'input', type: 'text', value: s.openlist_public_url || '', placeholder: '例如 http://192.0.2.10:5244（留空则不显示 OpenList 入口）', spellcheck: 'false' });
    const policyIn = h('textarea', { class: 'textarea', style: 'min-height:110px' }, s.policy_text || '');

    const form = h('div', null,
      h('div', { class: 'field' },
        h('label', { class: 'field-label' }, '公网 Base URL'),
        baseIn,
        h('div', { class: 'field-help' }, '用于生成接入提示、OpenAPI 与直链地址。内网部署留空即可，将以浏览器/请求的 Host 为准。')),
      h('div', { class: 'field' },
        h('label', { class: 'field-label' }, 'OpenList 公开访问地址'),
        olPubIn,
        olPubErr,
        h('div', { class: 'field-help' }, 'OpenList 后台在浏览器中的访问地址，例如 http://192.0.2.10:5244。必须能从你的浏览器访问；填写地址不会发布端口。')),
      h('div', { class: 'field' },
        h('label', { class: 'field-label' }, '策略说明（写入 agent 接入提示）'),
        policyIn,
        h('div', { class: 'field-help' }, 'agent 首次接入时会读到这段策略：上传限额、回收站保留期、以及「不要删除非自己创建的文件」等约定。')));

    let busy = false;
    const saveBtn = h('button', { class: 'btn btn-primary', type: 'button' }, '保存设置');
    saveBtn.addEventListener('click', async () => {
      if (busy) return;
      busy = true; saveBtn.disabled = true; saveBtn.textContent = '保存中…';
      olPubErr.textContent = '';
      try {
        const out = await C2A.API.put('/settings', {
          public_base_url: baseIn.value.trim(),
          openlist_public_url: olPubIn.value.trim(),
          policy_text: policyIn.value,
        });
        s = out || s;
        C2A.toast('设置已保存', 'success');
      } catch (e) {
        if (e.status === 400) {
          // e.g. invalid openlist_public_url — the server rejected the whole
          // save; surface its message next to the field it refers to
          olPubErr.textContent = e.message;
        }
        C2A.toast(e.message, 'error');
      }
      busy = false; saveBtn.disabled = false; saveBtn.textContent = '保存设置';
    });

    const card = h('div', { class: 'card', style: 'max-width:720px' },
      h('div', { class: 'card-head' }, h('span', { class: 'card-title' }, '实例设置')),
      h('div', { class: 'card-body' }, form,
        h('div', { class: 'flex gap-8 items-center mt-18' },
          h('span', { class: 'text-muted', style: 'font-size:12.5px' }, '修改立即生效；接入提示中的 Base URL 在下一次生成时使用新值。'),
          h('span', { style: 'margin-left:auto' }), saveBtn)));

    root.append(head('设置', '实例级参数：公开地址、OpenList 访问地址与 agent 策略文本'), card);
  };
})();
