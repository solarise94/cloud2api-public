/* cloud2api admin — page: API Keys（含子 Key、创建/编辑、轮换、吊销、彻底删除、使用此 Key、Gokapi 中转） */
(function () {
  'use strict';
  const C2A = window.C2A;
  const { h, icon } = C2A;
  const pages = (C2A.pages = C2A.pages || {});

  /* revoked keys are hidden by default; the toggle lives in the card head */
  const view = { showRevoked: false };

  const GB = 1024 ** 3;
  const RELAY_DEFAULT_QUOTA = 200 * GB;
  const RELAY_DEFAULT_DAYS = 7;

  /** Key carries the `relay` permission (globally or on any drive grant). */
  function hasRelayPerm(k) {
    return (k.global_perms || []).includes('relay') ||
      (k.drives || []).some((g) => (g.perms || []).includes('relay'));
  }

  /** relay row → effective config, applying the documented zero-value defaults:
   * drive = default_drive / first grant, quota 200 GiB, max age 7 days. */
  function relayEffective(k) {
    const r = k.relay || {};
    const granted = (k.drives || []).map((g) => g.drive);
    const drive = r.drive || k.default_drive || granted[0] || null;
    return {
      drive: drive,
      quota_bytes: r.quota_bytes > 0 ? r.quota_bytes : RELAY_DEFAULT_QUOTA,
      max_age_days: r.max_age_days > 0 ? r.max_age_days : RELAY_DEFAULT_DAYS,
      customized: !!(r.drive || r.quota_bytes > 0 || r.max_age_days > 0),
    };
  }

  pages.keys = async function (root) {
    C2A.clear(root);
    const createBtn = h('button', { class: 'btn btn-primary', type: 'button' }, icon('plus', 14), '创建 Key');
    createBtn.addEventListener('click', async () => {
      try {
        const [drivesData] = await Promise.all([C2A.API.get('/drives')]);
        keyFormDialog({ create: true, drives: drivesData.drives || [], onDone: () => pages.keys(root) });
      } catch (e) { C2A.toast(e.message, 'error'); }
    });

    root.append(C2A.pageHead('API Keys', '白名单 + 子目录 chroot + 权限 + 配额；明文只在创建/轮换时显示一次', createBtn));

    const card = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('span', { class: 'card-title' }, '全部 Key'),
        h('span', { class: 'card-desc' }, '子 Key 显示在父 Key 下方；吊销父 Key 会一并吊销其子 Key')),
      C2A.tableSkeleton(8, 6));
    root.append(card);

    let data;
    try {
      data = await C2A.API.get('/keys');
    } catch (e) {
      C2A.clear(card);
      card.append(h('div', { class: 'card-body' }, C2A.errorState(e.message, () => pages.keys(root))));
      return;
    }
    if (C2A.currentRoute && C2A.currentRoute() !== 'keys') return;

    const keys = data.keys || [];
    const active = keys.filter((k) => !k.revoked);
    const revoked = keys.filter((k) => k.revoked);
    const shown = view.showRevoked ? keys : active;

    const toggleBtn = h('button', { class: 'btn btn-ghost btn-sm', type: 'button' },
      view.showRevoked ? '隐藏已吊销' : '显示已吊销 (' + revoked.length + ')');
    toggleBtn.addEventListener('click', () => { view.showRevoked = !view.showRevoked; pages.keys(document.getElementById('page')); });

    C2A.clear(card);
    card.append(
      h('div', { class: 'card-head' },
        h('span', { class: 'card-title' }, '全部 Key'),
        h('span', { class: 'badge badge-slate' }, String(active.length) + ' 有效'),
        revoked.length ? h('span', { class: 'badge badge-slate' }, String(revoked.length) + ' 已吊销') : null,
        revoked.length ? h('span', { style: 'margin-left:auto' }, toggleBtn) : null),
      shown.length
        ? h('div', { class: 'table-wrap' }, keysTable(shown, keys))
        : view.showRevoked
          ? C2A.emptyState({ icon: 'keys', title: '没有已吊销的 Key', sub: '吊销的 Key 会保留在这里，可随时彻底删除' })
          : C2A.emptyState({ icon: 'keys', title: '还没有 API Key', sub: '创建第一个 Key，让 agent 通过 apikey 头访问网盘' }));
  };

  /* ---------------- permission presets（docs/API-KEYS-AND-RELAY.md） ----------------
   * 通用（默认）= list read write transfer link fetch relay — 不含 delete/purge/delegate。
   * 只读 = list read — 不含 relay（不能隐式开通中转上传）与 link。
   * 全部 = 全部十项权限（含 delegate），只在管理员显式选择时应用，永不作为默认。
   * 应用权限组时全局权限按组派生（通用/全部 = 同组权限，只读 = 空），
   * 使 relay 等按权限判断的旧链路（hasRelayPerm）保持可用。 */
  const PRESET_COMMON = ['list', 'read', 'write', 'transfer', 'link', 'fetch', 'relay'];
  const PRESET_READONLY = ['list', 'read'];
  const PRESET_ALL = C2A.PERMS.slice(); // all ten, explicit choice only (ui.js loads first)
  const PRESETS = {
    common: { label: '通用', sub: '推荐 · 日常读写', perms: PRESET_COMMON },
    all: { label: '全部', sub: '含删除/子 Key，仅确需时', perms: PRESET_ALL },
    readonly: { label: '只读', sub: '仅列表与下载', perms: PRESET_READONLY },
  };

  function permSet(perms) { return new Set(perms || []); }
  function samePermSet(a, b) {
    const sa = permSet(a), sb = permSet(b);
    if (sa.size !== sb.size) return false;
    for (const p of sa) if (!sb.has(p)) return false;
    return true;
  }
  /** Canonical order (C2A.PERMS) for a perm set — only used when a preset is
   * applied; untouched keys keep their saved perm array order verbatim. */
  function canonicalPerms(list) {
    const s = permSet(list);
    return C2A.PERMS.filter((p) => s.has(p));
  }
  function presetGlobal(name) {
    if (name === 'readonly') return [];
    return canonicalPerms(name === 'all' ? PRESET_ALL : PRESET_COMMON);
  }

  /* ---------------- helpers ---------------- */
  function keyStatus(k) {
    if (k.revoked) return { label: '已吊销', cls: 'badge-danger' };
    if (k.expires_at && new Date(k.expires_at).getTime() < Date.now()) return { label: '已过期', cls: 'badge-slate' };
    if (k.static) return { label: '静态', cls: 'badge-accent' };
    return { label: '有效', cls: 'badge-ok' };
  }
  function limitsText(k) {
    const parts = [];
    const L = k.limits || {};
    if (L.max_upload_bytes) parts.push('≤' + C2A.fmtBytes(L.max_upload_bytes).replace(/\.00 /, ' '));
    if (L.requests_per_minute) parts.push(L.requests_per_minute + ' rpm');
    if (L.concurrent_jobs) parts.push('×' + L.concurrent_jobs);
    return parts.length ? parts.join(' · ') : '—';
  }
  /** null means "first granted drive" per contract — display it that way. */
  function effectiveDefaultDrive(k) {
    if (k.default_drive) return k.default_drive;
    const g = (k.drives || [])[0];
    return g ? g.drive : null;
  }

  function keysTable(keys, allKeys) {
    const universe = allKeys || keys; // full list, so hidden parents still resolve
    const childrenOf = (id) => keys.filter((k) => k.parent_id === id);
    const tbl = h('table', { class: 'tbl keys-tbl' },
      h('thead', null, h('tr', null,
        h('th', null, 'Key'), h('th', null, '状态'), h('th', null, '网盘授权'),
        h('th', null, '全局'), h('th', null, '限额'), h('th', null, '最后使用'),
        h('th', { style: 'text-align:right' }, '操作'))));
    const tbody = h('tbody');
    const rows = [];
    const seen = new Set();
    // top-level rows: no parent, or parent missing from the full key list
    for (const k of keys) {
      if (k.parent_id && universe.some((p) => p.id === k.parent_id)) continue;
      rows.push([k, 0]); seen.add(k.id);
      for (const c of childrenOf(k.id)) { rows.push([c, 1]); seen.add(c.id); }
    }
    // defensive fallback: anything not reachable from a top-level row
    for (const k of keys) if (!seen.has(k.id)) rows.push([k, 0]);
    for (const [k, depth] of rows) tbody.append(keyRow(k, depth), detailsRow(k));
    tbl.append(tbody);
    return tbl;
  }

  /* full-width expander row with every field that no longer fits the table */
  function detailsRow(k) {
    const L = k.limits || {};
    const grants = (k.drives || []).map((g) =>
      h('div', { class: 'kv-mini-row' },
        h('span', { class: 'kv-mini-k mono' }, g.drive),
        h('span', { class: 'kv-mini-v' },
          h('span', { class: 'mono' }, 'root ' + (g.root || '/')),
          ' · ',
          h('span', { class: 'grant-perm-names' }, C2A.PERM_FULL(g.perms)),
          ' ',
          h('span', { class: 'chip perm-p', title: C2A.PERM_FULL(g.perms) }, C2A.PERM_SHORT(g.perms)))));
    let relayItem = null;
    if (k.relay) {
      const re = relayEffective(k);
      relayItem = h('div', { class: 'kv-item' }, h('dt', null, 'Gokapi 中转'),
        h('dd', null,
          hasRelayPerm(k)
            ? h('span', null,
                h('span', { class: 'mono' }, re.drive || '—'),
                ' · 配额 ' + C2A.fmtBytes(re.quota_bytes).replace(/\.00 /, ' '),
                ' · 保留 ' + re.max_age_days + ' 天',
                re.customized ? null : h('span', { class: 'text-muted' }, '（默认值）'),
                ' · ', h('a', { href: '#/keys', title: '明文丢失时无法查看中转文件列表；兼容接口仍按 relay 权限与配额工作' }, '/api/files/*'))
            : h('span', { class: 'text-muted', title: '该 Key 未持有 relay 权限：/api/files/* 与 /api/info/* 会返回 403' }, '未启用（缺少 relay 权限）')));
    }
    return h('tr', { class: 'row-details' },
      h('td', { colspan: 7 },
        h('div', { class: 'details-inner' },
          h('dl', { class: 'kv-mini' },
            h('div', { class: 'kv-item' }, h('dt', null, 'Key 明文前缀'),
              h('dd', null, k.prefix ? h('span', { class: 'mono' }, k.prefix + '-…') : h('span', { class: 'text-muted' }, '—（旧格式静态 Key）'))),
            k.parent_id ? h('div', { class: 'kv-item' }, h('dt', null, '父 Key'),
              h('dd', { class: 'mono' }, k.parent_id)) : null,
            h('div', { class: 'kv-item' }, h('dt', null, '默认网盘'),
              h('dd', { class: 'mono' }, effectiveDefaultDrive(k) || '—',
                !k.default_drive && effectiveDefaultDrive(k) ? h('span', { class: 'text-muted' }, '（未设置，默认取首个授权）') : null)),
            h('div', { class: 'kv-item' }, h('dt', null, '创建时间'), h('dd', null, C2A.fmtTime(k.created_at))),
            h('div', { class: 'kv-item' }, h('dt', null, '过期时间'), h('dd', null, k.expires_at ? C2A.fmtTime(k.expires_at) : '永久')),
            h('div', { class: 'kv-item' }, h('dt', null, '最后使用'), h('dd', null, C2A.fmtAgo(k.last_used_at))),
            h('div', { class: 'kv-item' }, h('dt', null, '回收站'),
              h('dd', null, k.trash && k.trash.enabled ? '启用 · 保留 ' + (k.trash.days || 7) + ' 天' : '关闭')),
            h('div', { class: 'kv-item' }, h('dt', null, '单文件上限'), h('dd', null, L.max_upload_bytes ? C2A.fmtBytes(L.max_upload_bytes) : '不限')),
            h('div', { class: 'kv-item' }, h('dt', null, '请求速率'), h('dd', null, L.requests_per_minute ? L.requests_per_minute + ' 次/分钟' : '不限')),
            h('div', { class: 'kv-item' }, h('dt', null, '并发任务'), h('dd', null, L.concurrent_jobs ? String(L.concurrent_jobs) : '不限')),
            h('div', { class: 'kv-item' }, h('dt', null, '全局权限'), h('dd', null, C2A.PERM_FULL(k.global_perms))),
            relayItem),
          h('div', { class: 'kv-item', style: 'margin-top:10px' },
            h('span', { class: 'kv-mini-k' }, '网盘授权'), h('span', { class: 'kv-mini-v' },
              grants.length ? h('div', { class: 'kv-mini-rows' }, grants) : h('span', { class: 'text-muted' }, '—'))))));
  }

  /** "由 agent 签发" marker + TTL/expiry note for child keys. */
  function childMeta(k) {
    const parts = [];
    if (k.parent_id) {
      parts.push(h('span', { class: 'badge badge-violet badge-xs', title: '父 Key：' + k.parent_id + '；由 agent 通过 POST /v1/keys 签发，权限不超过父 Key' }, '由 agent 签发'));
    }
    if (k.expires_at) {
      const exp = new Date(k.expires_at).getTime();
      const expired = !isNaN(exp) && exp < Date.now();
      const label = (expired ? '已过期 · ' : 'TTL 至 ') + C2A.fmtTime(k.expires_at);
      parts.push(h('span', { class: expired ? 'text-danger' : 'text-muted', title: '过期时间（子 Key TTL 默认 24 小时、最长 7 天）' }, label));
    }
    return parts;
  }

  function keyRow(k, depth) {
    const st = keyStatus(k);
    const muted = k.revoked || st.label === '已过期';
    const actions = h('div', { class: 'cell-actions' });
    if (k.revoked) {
      // revoked keys keep only one action: hard delete (admin DELETE purges the row)
      const delBtn = h('button', { class: 'btn btn-danger-soft btn-sm', type: 'button', title: '从数据库中彻底删除该吊销记录' }, icon('trash', 13), '彻底删除');
      delBtn.addEventListener('click', () => hardDeleteDialog(k, () => pages.keys(document.getElementById('page'))));
      actions.append(delBtn);
    } else {
      const useBtn = h('button', { class: 'btn btn-secondary btn-sm', type: 'button', title: '生成 agent 接入提示' }, '使用');
      useBtn.addEventListener('click', () => useKeyDialog(k));
      actions.append(useBtn);
      const ro = k.static || muted;
      actions.append(C2A.menu([
        { label: '编辑', icon: 'settings', disabled: !!ro, title: ro ? (k.static ? '静态 Key（config.yaml）为只读' : '已失效的 Key 为只读') : null, onSelect: async () => {
          try {
            const dd = await C2A.API.get('/drives');
            keyFormDialog({ create: false, key: k, drives: dd.drives || [], onDone: () => pages.keys(document.getElementById('page')) });
          } catch (e) { C2A.toast(e.message, 'error'); }
        } },
        { label: '轮换密钥', icon: 'rotate', disabled: k.static || k.revoked, title: k.static ? '静态 Key（config.yaml）不能在门户轮换' : k.revoked ? '已吊销的 Key 不能轮换' : null, onSelect: () => rotateDialog(k) },
        { label: '吊销', icon: 'trash', danger: true, disabled: k.static || k.revoked, title: k.static ? '静态 Key（config.yaml）不能在门户吊销' : k.revoked ? '已吊销' : null, onSelect: () => revokeDialog(k, () => pages.keys(document.getElementById('page'))) },
      ]));
    }

    const caret = h('button', { class: 'caret-btn', type: 'button', 'aria-label': '展开详情', title: '展开详情' }, h('span', { class: 'caret' }));
    const childMeta_ = childMeta(k);
    const subLine = h('div', { class: 'key-sub' },
      h('span', { class: 'mono', title: k.prefix ? 'Key 明文前缀' : '旧格式静态 Key 没有前缀' }, k.prefix ? k.prefix + '-…' : (k.static ? 'config.yaml' : '无前缀')),
      h('span', { class: 'sub-sep' }, '·'),
      h('span', { title: '默认网盘（未设置时取首个授权）' }, '默认 ' + (effectiveDefaultDrive(k) || '—')));
    const idCell = h('div', { class: 'key-id-cell' },
      caret,
      h('div', { class: 'grow' },
        h('div', { class: 'cell-main mono' }, (depth ? '↳ ' : '') + k.id),
        subLine,
        // child markers get their own line so they never clip
        childMeta_.length ? h('div', { class: 'key-sub key-sub-extra' }, childMeta_) : null));
    caret.addEventListener('click', (e) => { e.stopPropagation(); keyRowToggle(caret); });

    return h('tr', { class: (depth ? 'row-child' : '') + (muted ? ' row-muted' : '') + (k.static ? ' tr-static' : '') },
      h('td', null, idCell),
      h('td', null, h('span', { class: 'badge ' + st.cls }, st.label)),
      h('td', null,
        h('div', { class: 'chip-row grants-cell' },
          (k.drives || []).slice(0, 2).map((g) => h('span', {
            class: 'chip grant-chip', title: g.drive + ' · root ' + (g.root || '/') + ' · ' + C2A.PERM_FULL(g.perms),
          },
          h('span', { class: 'grant-drive' }, g.drive),
          h('span', { class: 'grant-perms-mono' }, C2A.PERM_SHORT(g.perms)),
          g.root && g.root !== '/' ? h('span', { class: 'grant-root mono' }, g.root) : null)),
          (k.drives || []).length > 2 ? h('span', { class: 'chip', title: '在行详情中查看全部授权' }, '+' + ((k.drives || []).length - 2)) : null,
          !(k.drives || []).length ? h('span', { class: 'text-muted', style: 'font-size:12px' }, '—') : null)),
      h('td', null,
        h('span', { class: 'chip perm-g', title: C2A.PERM_FULL(k.global_perms) }, (k.global_perms || []).length ? C2A.PERM_SHORT(k.global_perms) : '—'),
        (k.global_perms || []).includes('delegate') ? h('span', { class: 'badge badge-violet badge-xs', style: 'margin-left:6px', title: '持有 delegate 全局权限：可通过 POST /v1/keys 签发不超过自身权限的子 Key' }, '可签发子 Key') : null),
      h('td', { class: 'text-sub', style: 'font-size:12px;white-space:nowrap', title: '单文件上限 · 每分钟请求数（rpm）· 并发任务数（×n）；回收站与完整限额见展开行' }, limitsText(k)),
      h('td', { class: 'nowrap text-muted num', style: 'font-size:12px' }, C2A.fmtAgo(k.last_used_at)),
      h('td', null, actions));
  }
  function keyRowToggle(caret) {
    const tr = caret.closest('tr');
    if (!tr) return;
    const det = tr.nextElementSibling;
    if (!det || !det.classList.contains('row-details')) return;
    const open = !det.classList.contains('open');
    det.classList.toggle('open', open);
    tr.classList.toggle('row-open', open);
  }

  /* ---------------- hard delete (revoked keys only) ---------------- */
  function hardDeleteDialog(k, onDone) {
    C2A.confirmModal({
      title: '彻底删除 Key · ' + k.id,
      message: h('p', { class: 'text-sub', style: 'line-height:1.6' },
        '该 Key 已吊销。彻底删除会把它的记录（含配置、前缀与授权）从数据库中移除，',
        h('span', { style: 'font-weight:700' }, '此操作不可恢复'),
        '；审计日志中已有的历史记录会保留 Key ID 字样。若只是想让它继续失效，无需删除——吊销状态已经足够。'),
      danger: true,
      confirmText: '彻底删除',
      async onConfirm() {
        await C2A.API.call('DELETE', '/keys/' + encodeURIComponent(k.id));
        C2A.toast('Key ' + k.id + ' 已彻底删除', 'success');
        if (onDone) onDone();
      },
    });
  }

  /* ---------------- create / edit dialog ----------------
   * Primary controls (scope / drives / default drive+root / preset) cover the
   * common case; every legacy fine-grained control (per-drive grants, global
   * perms, limits, trash, relay, expiry) stays under 高级权限设置. The shared
   * state `st` is initialised to mirror the saved key exactly, so opening and
   * saving a legacy/custom key without explicit edits reproduces the stored
   * grants byte-for-byte (order, roots and perm arrays included). */
  function keyFormDialog(opts) {
    // opts: {create, key?, drives, onDone}
    const isCreate = !!opts.create;
    const k = opts.key || {};
    const drives = opts.drives || [];

    const idInput = isCreate
      ? h('input', { class: 'input', type: 'text', placeholder: '留空自动生成（key-xxxx）', spellcheck: 'false', autocomplete: 'off' })
      : h('input', { class: 'input', type: 'text', value: k.id, disabled: true });

    /* ---- shared state (single source of truth for both control layers) ---- */
    const st = {
      scope: (k.drives && k.drives.length > 1) ? 'cross' : 'single',
      order: (k.drives || []).map((g) => g.drive),
      roots: {},
      perms: {},
      defaultDrive: k.default_drive || null, // never silently invent a default for legacy keys
      globalPerms: (k.global_perms || []).slice(),
      preset: 'custom',
    };
    // memory of every drive ever seen in this dialog: re-adding a drive after
    // unticking restores its previous root/perms instead of wiping them
    const knownRoots = {};
    const knownPerms = {};
    for (const g of k.drives || []) {
      st.roots[g.drive] = g.root || '/';
      st.perms[g.drive] = (g.perms || []).slice();
      knownRoots[g.drive] = g.root || '/';
      knownPerms[g.drive] = (g.perms || []).slice();
    }
    if (isCreate) {
      st.globalPerms = presetGlobal('common');
      st.preset = 'common';
    } else {
      st.preset = derivePreset();
    }
    function currentPresetPerms() {
      if (st.preset === 'custom') return ['list', 'read', 'write']; // legacy default for ad-hoc grants
      return canonicalPerms(PRESETS[st.preset].perms);
    }
    function derivePreset() {
      if (!st.order.length) return st.preset === 'custom' ? 'custom' : st.preset;
      const grants = st.order.map((id) => st.perms[id] || []);
      for (const name of ['common', 'all', 'readonly']) {
        const set = name === 'common' ? PRESET_COMMON : name === 'all' ? PRESET_ALL : PRESET_READONLY;
        if (grants.every((p) => samePermSet(p, set)) && samePermSet(st.globalPerms, presetGlobal(name))) return name;
      }
      return 'custom';
    }
    function hasRelayPermState() {
      return st.globalPerms.includes('relay') || st.order.some((id) => (st.perms[id] || []).includes('relay'));
    }

    /* ---- relay state (Gokapi 兼容层，Phase 1d) ----
     * New keys: relay auto-enabled with the documented defaults on the default
     * drive (relay.drive = null follows default_drive). Readonly preset turns
     * it off; without any relay permission the checkbox stays disabled so a
     * readonly key can never enable relay upload implicitly. */
    let relayOn = isCreate ? true : !!(k.relay && (k.relay.drive || k.relay.quota_bytes > 0 || k.relay.max_age_days > 0));
    const relayCfg = k.relay
      ? { drive: k.relay.drive || null, quota_bytes: k.relay.quota_bytes || 0, max_age_days: k.relay.max_age_days || 0 }
      : { drive: null, quota_bytes: 0, max_age_days: 0 };

    /* expires */
    let initialExp = '';
    if (k.expires_at) {
      const d0 = new Date(k.expires_at);
      if (!isNaN(d0.getTime())) {
        const p = (x) => String(x).padStart(2, '0');
        initialExp = d0.getFullYear() + '-' + p(d0.getMonth() + 1) + '-' + p(d0.getDate()) + 'T' + p(d0.getHours()) + ':' + p(d0.getMinutes());
      }
    }
    const expInput = h('input', { class: 'input', type: 'datetime-local', value: initialExp });

    /* limits */
    const GB = 1024 ** 3;
    const upVal = h('input', { class: 'input', type: 'number', min: '0', step: 'any', value: (k.limits && k.limits.max_upload_bytes) ? String(Math.round(k.limits.max_upload_bytes / GB * 100) / 100) : '' });
    const upUnit = h('select', { class: 'select', style: 'width:86px' },
      h('option', { value: 'MiB' }, 'MiB'), h('option', { value: 'GiB', selected: true }, 'GiB'), h('option', { value: 'TiB' }, 'TiB'));
    if (k.limits && k.limits.max_upload_bytes && k.limits.max_upload_bytes < GB) {
      upUnit.value = 'MiB';
      upVal.value = String(Math.round(k.limits.max_upload_bytes / (1024 ** 2) * 100) / 100);
    }
    const rpmVal = h('input', { class: 'input', type: 'number', min: '0', value: k.limits && k.limits.requests_per_minute ? String(k.limits.requests_per_minute) : '' });
    const concVal = h('input', { class: 'input', type: 'number', min: '0', value: k.limits && k.limits.concurrent_jobs ? String(k.limits.concurrent_jobs) : '' });

    /* trash */
    const trashCb = h('input', { type: 'checkbox', checked: !!(k.trash && k.trash.enabled) || null });
    const trashDays = h('input', { class: 'input', type: 'number', min: '1', max: '30', value: String((k.trash && k.trash.days) || 7), style: 'width:90px' });

    /* ================= primary controls ================= */

    /* scope radios */
    const scopeSingle = h('input', { type: 'radio', name: 'kb-scope', value: 'single', checked: st.scope === 'single' || null });
    const scopeCross = h('input', { type: 'radio', name: 'kb-scope', value: 'cross', checked: st.scope === 'cross' || null });

    /* single-drive picker */
    const singleSel = h('select', { class: 'select', style: 'max-width:320px' });

    /* cross-drive checklist (explicit current drives only — never a wildcard;
     * drives adopted later are NOT auto-granted) */
    const crossList = h('div', { class: 'drive-checklist' });

    /* default drive + its root */
    const defSel = h('select', { class: 'select' });
    const defRootIn = h('input', { class: 'input', type: 'text', value: '/', placeholder: '/', spellcheck: 'false', style: 'max-width:320px' });
    defRootIn.addEventListener('input', () => {
      if (st.defaultDrive) st.roots[st.defaultDrive] = defRootIn.value.trim() || '/';
    });

    /* preset chips */
    const chipHost = h('div', { class: 'preset-chips' });
    const customChip = h('span', { class: 'chip-btn chip-custom', title: '当前为自定义细项权限（可在高级权限设置中修改）' }, '自定义');

    function driveLabel(id) {
      const d = drives.find((x) => x.id === id);
      return d ? id : (id ? id + '（已删除）' : id);
    }

    function grantDrive(id, opts2) {
      opts2 = opts2 || {};
      if (st.order.includes(id)) return;
      st.order.push(id);
      if (st.roots[id] === undefined) st.roots[id] = knownRoots[id] !== undefined ? knownRoots[id] : '/';
      if (st.perms[id] === undefined) st.perms[id] = (knownPerms[id] || currentPresetPerms()).slice();
      if (opts2.makeDefault || !st.defaultDrive || !st.order.includes(st.defaultDrive)) st.defaultDrive = id;
    }
    function revokeDrive(id) {
      // snapshot the freshest draft before dropping the grant, so untick →
      // re-tick restores exactly what the admin last set (advanced edits
      // included), not an older snapshot
      if (st.perms[id]) knownPerms[id] = st.perms[id].slice();
      if (st.roots[id] !== undefined) knownRoots[id] = st.roots[id];
      st.order = st.order.filter((x) => x !== id);
      delete st.perms[id];
      if (st.defaultDrive === id) st.defaultDrive = st.order[0] || null;
    }

    function renderSingleSel() {
      singleSel.style.display = st.scope === 'single' ? '' : 'none';
      const prev = st.order[0] || '';
      C2A.clear(singleSel);
      singleSel.append(h('option', { value: '' }, '选择网盘…'));
      for (const d of drives) {
        const o = h('option', { value: d.id }, d.id);
        if (d.id === prev) o.selected = true;
        singleSel.append(o);
      }
      if (prev && !drives.some((d) => d.id === prev)) {
        const o = h('option', { value: prev }, driveLabel(prev));
        o.selected = true;
        singleSel.prepend(o);
      }
      singleSel.disabled = false;
    }
    function renderCrossList() {
      crossList.style.display = st.scope === 'cross' ? '' : 'none';
      C2A.clear(crossList);
      const known = new Set(st.order);
      const extra = st.order.filter((id) => !drives.some((d) => d.id === id));
      for (const d of drives) {
        const cb = h('input', { type: 'checkbox', checked: known.has(d.id) || null });
        cb.addEventListener('change', () => {
          if (cb.checked) grantDrive(d.id);
          else revokeDrive(d.id);
          renderDefaultControls();
          renderChips();
          syncAdvancedFromState();
        });
        crossList.append(h('label', { class: 'check drive-check', title: '勾选后按当前权限组授权该网盘；根目录等细节见高级权限设置' },
          cb, h('span', { class: 'mono' }, d.id)));
      }
      for (const id of extra) { // grants pointing at deleted drives stay visible/untouchable
        crossList.append(h('label', { class: 'check drive-check' },
          h('input', { type: 'checkbox', checked: true, disabled: true }),
          h('span', { class: 'mono' }, driveLabel(id))));
      }
      if (!drives.length && !extra.length) crossList.append(h('span', { class: 'text-muted', style: 'font-size:12px' }, '没有可授权的网盘'));
    }
    function renderDefaultControls() {
      C2A.clear(defSel);
      if (!st.order.length) {
        defSel.append(h('option', { value: '' }, st.scope === 'cross' ? '先勾选网盘' : '先选择网盘'));
        defSel.disabled = true;
      } else if (st.scope === 'single') {
        // single mode: the one granted drive IS the default drive (auto; the
        // only unambiguous choice — visible in the form, saved explicitly)
        defSel.disabled = false;
        C2A.clear(defSel);
        defSel.append(h('option', { value: st.order[0] }, driveLabel(st.order[0])));
        defSel.value = st.order[0];
        st.defaultDrive = st.order[0];
      } else {
        defSel.disabled = false;
        if (st.defaultDrive && st.order.includes(st.defaultDrive)) {
          for (const id of st.order) {
            const o = h('option', { value: id }, driveLabel(id));
            if (id === st.defaultDrive) o.selected = true;
            defSel.append(o);
          }
          defSel.value = st.defaultDrive;
        } else {
          // legacy cross-drive key without a default (or the default drive was
          // just unticked): show the required placeholder AND every granted
          // drive so the admin can actually pick one — never options-less
          st.defaultDrive = null;
          defSel.append(h('option', { value: '' }, '请选择默认网盘（必填）'));
          for (const id of st.order) defSel.append(h('option', { value: id }, driveLabel(id)));
        }
      }
      defRootIn.value = st.defaultDrive ? (st.roots[st.defaultDrive] || '/') : '/';
      defRootIn.disabled = !st.defaultDrive;
    }
    function renderChips() {
      C2A.clear(chipHost);
      for (const name of ['readonly', 'common', 'all']) {
        const p = PRESETS[name];
        const btn = h('button', {
          class: 'chip-btn' + (st.preset === name ? ' on' : ''), type: 'button',
          title: (name === 'common' ? 'list read write transfer link fetch relay（不含删除/彻底删除/子 Key）'
            : name === 'all' ? '全部十项权限（含删除、彻底删除、子 Key 授权），仅确实需要时选择'
              : 'list read（不含中转上传与直链）'),
        }, h('span', { class: 'chip-btn-label' }, p.label), h('span', { class: 'chip-btn-sub' }, p.sub));
        btn.addEventListener('click', () => applyPreset(name));
        chipHost.append(btn);
      }
      chipHost.append(customChip);
      customChip.classList.toggle('on', st.preset === 'custom');
    }

    singleSel.addEventListener('change', () => {
      const id = singleSel.value;
      if (!id) return;
      for (const cur of st.order.slice()) if (cur !== id) revokeDrive(cur); // single = exactly one
      grantDrive(id, { makeDefault: true });
      renderDefaultControls();
      renderChips();
      syncAdvancedFromState();
    });
    scopeSingle.addEventListener('change', () => setScope('single'));
    scopeCross.addEventListener('change', () => setScope('cross'));
    defSel.addEventListener('change', () => {
      st.defaultDrive = defSel.value || null;
      defRootIn.disabled = !st.defaultDrive; // picking a default re-enables its root input
      defRootIn.value = st.defaultDrive ? (st.roots[st.defaultDrive] || '/') : '/';
    });
    function setScope(s) {
      if (st.scope === s) return;
      st.scope = s;
      if (s === 'cross') {
        // start from the current single grant; the admin adds drives explicitly
        if (st.order.length && !st.order.includes(st.defaultDrive || '')) st.defaultDrive = st.order[0];
      } else {
        // collapse to the default drive; other grants return when re-ticked
        const keep = st.defaultDrive || st.order[0] || null;
        for (const id of st.order.slice()) if (id !== keep) revokeDrive(id);
        if (keep) grantDrive(keep, { makeDefault: true });
      }
      renderSingleSel();
      renderCrossList();
      renderDefaultControls();
      renderChips();
      syncAdvancedFromState();
    }

    /* ---- preset application: show the diff, confirm any widening ---- */
    function applyPreset(name) {
      const set = canonicalPerms(PRESETS[name].perms);
      const newGlobal = presetGlobal(name);
      const additions = [];
      const removals = [];
      for (const id of st.order) {
        const cur = permSet(st.perms[id]);
        for (const p of set) if (!cur.has(p)) additions.push(h('div', null, h('span', { class: 'mono' }, id), '：新增 ' + (C2A.PERM_LABELS[p] || p)));
        const next = permSet(set);
        for (const p of st.perms[id] || []) if (!next.has(p)) removals.push(h('div', null, h('span', { class: 'mono' }, id), '：移除 ' + (C2A.PERM_LABELS[p] || p)));
      }
      const curG = permSet(st.globalPerms);
      for (const p of newGlobal) if (!curG.has(p)) additions.push(h('div', null, '全局：新增 ' + (C2A.PERM_LABELS[p] || p)));
      for (const p of st.globalPerms) if (!permSet(newGlobal).has(p)) removals.push(h('div', null, '全局：移除 ' + (C2A.PERM_LABELS[p] || p)));

      const doApply = () => {
        for (const id of st.order) {
          st.perms[id] = set.slice();
          knownPerms[id] = set.slice();
        }
        st.globalPerms = newGlobal.slice();
        st.preset = name;
        if (name === 'readonly') relayOn = false; // 只读不隐式开通中转上传
        else relayOn = true; // 通用/全部：在默认网盘上自动启用中转（文档默认值）
        syncRelayUI();
        renderChips();
        syncAdvancedFromState();
      };
      if (!st.order.length || (!additions.length && !removals.length)) { doApply(); return; }
      C2A.confirmModal({
        title: '应用权限组「' + PRESETS[name].label + '」',
        message: h('div', { class: 'text-sub', style: 'line-height:1.7' },
          h('div', null, '权限组会覆盖', h('span', { style: 'font-weight:600' }, '每个网盘的现有细项权限与全局权限'),
            '（根目录、网盘范围、限额、有效期不变）：'),
          additions.length ? h('div', { class: 'mt-8' }, h('span', { style: 'font-weight:600' }, '将新增权限：'), additions) : null,
          removals.length ? h('div', { class: 'mt-8' }, h('span', { style: 'font-weight:600' }, '将移除权限：'), removals) : null,
          h('div', { class: 'mt-8 text-muted', style: 'font-size:12.5px' },
            name === 'all'
              ? '「全部」含删除、彻底删除与子 Key 授权（delegate），请仅在确实需要时选择。'
              : '注意：write 允许上传、覆盖与移动文件，「通用」不含删除并不代表写操作没有覆盖风险。'),
          name === 'all' ? h('div', { class: 'text-muted', style: 'font-size:12.5px' }, '注意：write 允许上传、覆盖与移动文件。') : null),
        confirmText: '应用',
        onConfirm: doApply,
      });
    }

    /* ================= advanced editor（既有细项，原样保留） ================= */
    const grantsHost = h('div', { class: 'grant-table' });
    let syncingAdvanced = false;

    function addGrantRow(g) {
      g = g || { drive: '', root: '/', perms: ['list', 'read', 'write'] };
      const driveSel = h('select', { class: 'select' }, h('option', { value: '' }, '选择网盘…'));
      for (const d of drives) {
        const o = h('option', { value: d.id }, d.id);
        if (g.drive === d.id) o.selected = true;
        driveSel.append(o);
      }
      if (g.drive && !drives.some((d) => d.id === g.drive)) {
        const o = h('option', { value: g.drive }, g.drive + '（已删除）');
        o.selected = true;
        driveSel.prepend(o);
      }
      const rootIn = h('input', { class: 'input', type: 'text', value: g.root || '/', placeholder: '/', spellcheck: 'false' });
      const permBoxes = {};
      const permRow = h('div', { class: 'grant-perms' }, C2A.PERMS.map((p) =>
        h('label', { class: 'check', title: p },
          (permBoxes[p] = h('input', { type: 'checkbox', checked: (g.perms || []).includes(p) || null })),
          C2A.PERM_LABELS[p])));
      const row = h('div', { class: 'grant-row' },
        h('div', null, driveSel, rootIn),
        h('div', null, permRow),
        h('div', { class: 'grant-remove' },
          h('button', { class: 'icon-btn', type: 'button', 'aria-label': '移除此授权', title: '移除此授权' }, icon('trash', 14))));
      row.querySelector('.grant-remove button').addEventListener('click', () => { row.remove(); onAdvancedEdited(); if (!grantsHost.querySelector('.grant-row')) addGrantRow(); });
      row._collect = () => ({ drive: driveSel.value, root: rootIn.value.trim() || '/', perms: C2A.PERMS.filter((p) => permBoxes[p].checked) });
      driveSel.addEventListener('change', onAdvancedEdited);
      rootIn.addEventListener('change', onAdvancedEdited);
      for (const p of C2A.PERMS) permBoxes[p].addEventListener('change', onAdvancedEdited);
      grantsHost.append(row);
    }

    function syncAdvancedFromState() {
      if (syncingAdvanced) return;
      syncingAdvanced = true;
      try {
        C2A.clear(grantsHost);
        for (const id of st.order) addGrantRow({ drive: id, root: st.roots[id] || '/', perms: st.perms[id] || [] });
        if (!st.order.length) addGrantRow({ drive: '', root: '/', perms: currentPresetPerms() });
        for (const p of C2A.PERMS) gpermBoxes[p].checked = st.globalPerms.includes(p);
        refreshRelayDrives();
      } finally {
        syncingAdvanced = false;
      }
    }
    /** Advanced rows are the source of truth while being edited: fold them
     * back into st, then re-render the primary controls from st. */
    function onAdvancedEdited() {
      if (syncingAdvanced) return;
      const rows = [...grantsHost.querySelectorAll('.grant-row')].map((r) => r._collect()).filter((g) => g.drive);
      st.order = rows.map((g) => g.drive);
      for (const g of rows) {
        st.roots[g.drive] = g.root;
        st.perms[g.drive] = g.perms;
        knownRoots[g.drive] = g.root;
        knownPerms[g.drive] = g.perms;
      }
      for (const id of Object.keys(st.perms)) if (!st.order.includes(id)) delete st.perms[id];
      st.globalPerms = C2A.PERMS.filter((p) => gpermBoxes[p].checked);
      st.preset = derivePreset();
      syncRelayUI();
      renderSingleSel();
      renderCrossList();
      renderDefaultControls();
      renderChips();
    }

    /* global perms */
    const gpermBoxes = {};
    const gpermGrid = h('div', { class: 'checkgrid' }, C2A.PERMS.map((p) =>
      h('label', { class: 'check', title: p },
        (gpermBoxes[p] = h('input', { type: 'checkbox', checked: st.globalPerms.includes(p) || null })),
        C2A.PERM_LABELS[p] + ' (' + p + ')')));
    for (const p of C2A.PERMS) gpermBoxes[p].addEventListener('change', onAdvancedEdited);

    /* relay subform — drive options follow the granted drives */
    const relayCb = h('input', { type: 'checkbox' });
    const relayBody = h('div', { class: 'relay-body', style: 'display:none' });
    const relayDriveSel = h('select', { class: 'select' });
    const relayQuota = h('input', { class: 'input', type: 'number', min: '0', step: 'any', style: 'width:120px', value: relayCfg.quota_bytes > 0 ? String(Math.round(relayCfg.quota_bytes / GB * 100) / 100) : '' });
    const relayAge = h('input', { class: 'input', type: 'number', min: '0', max: '7', style: 'width:90px', value: relayCfg.max_age_days > 0 ? String(relayCfg.max_age_days) : '0' });

    let relayDriveTouched = false;
    relayDriveSel.addEventListener('change', () => { relayDriveTouched = true; });
    function refreshRelayDrives() {
      // first paint must keep a customized relay.drive selected; after the
      // admin touches the select, their choice wins
      const prev = relayDriveTouched ? relayDriveSel.value : (relayCfg.drive || '');
      C2A.clear(relayDriveSel);
      relayDriveSel.append(h('option', { value: '' }, '默认（取默认网盘）'));
      for (const id of st.order) {
        const o = h('option', { value: id }, id);
        if (id === prev) o.selected = true;
        relayDriveSel.append(o);
      }
      if (prev && !st.order.includes(prev)) {
        const o = h('option', { value: prev }, driveLabel(prev));
        o.selected = true;
        relayDriveSel.append(o);
      }
    }
    function syncRelayUI() {
      relayCb.checked = relayOn;
      relayBody.style.display = relayOn ? '' : 'none';
      const allowed = hasRelayPermState();
      relayCb.disabled = !allowed;
      relayCb.title = allowed
        ? ''
        : '当前权限不含 relay（中转）：选择「通用/全部」权限组，或在高级设置中勾选 relay 权限后才能启用';
      refreshRelayDrives();
    }
    relayCb.addEventListener('change', () => {
      relayOn = relayCb.checked;
      relayBody.style.display = relayOn ? '' : 'none';
      refreshRelayDrives();
    });
    relayBody.append(
      h('div', { class: 'form-row' },
        h('div', { class: 'field', style: 'margin-bottom:0' },
          h('label', { class: 'field-label' }, '中转网盘'),
          relayDriveSel,
          h('div', { class: 'field-help' }, '文件落在 <授权根目录>/.relay/ 下；选项仅限本 Key 已授权的网盘。')),
        h('div', { class: 'field', style: 'margin-bottom:0' },
          h('label', { class: 'field-label' }, '容量配额（GiB）'),
          relayQuota,
          h('div', { class: 'field-help' }, '留空 = 默认 200 GiB；超限后按上传时间淘汰最旧的文件。')),
        h('div', { class: 'field', style: 'margin-bottom:0' },
          h('label', { class: 'field-label' }, '保留天数'),
          relayAge,
          h('div', { class: 'field-help' }, '0 = 默认 7 天；最长 7 天，到期自动清理。'))),
      h('div', { class: 'field-help', style: 'margin-top:8px' },
        '需要为该 Key 授予 relay 权限（全局或网盘授权上），否则 Gokapi 接口返回 403。'));

    const errBox = h('div', { class: 'field-error' });
    let busy = false;

    function collectBody() {
      const drivesGrants = st.order.map((id) => ({
        drive: id,
        root: (st.roots[id] || '/').trim() || '/',
        perms: (st.perms[id] || []).slice(),
      }));
      let maxUp = null;
      if (upVal.value !== '') {
        const n = Number(upVal.value);
        if (isFinite(n) && n > 0) maxUp = Math.round(n * (upUnit.value === 'MiB' ? 1024 ** 2 : upUnit.value === 'TiB' ? 1024 ** 4 : GB));
      }
      let relay = null;
      if (relayOn && hasRelayPermState()) {
        relay = {
          drive: relayDriveSel.value || null,
          quota_bytes: relayQuota.value === '' ? 0 : Math.max(0, Math.round(Number(relayQuota.value) * GB)),
          max_age_days: relayAge.value === '' ? 0 : Math.max(0, Math.min(7, Math.round(Number(relayAge.value)))),
        };
      }
      const body = {
        default_drive: st.defaultDrive || null,
        drives: drivesGrants,
        global_perms: st.globalPerms.slice(),
        limits: {
          max_upload_bytes: maxUp,
          requests_per_minute: rpmVal.value === '' ? null : Number(rpmVal.value),
          concurrent_jobs: concVal.value === '' ? null : Number(concVal.value),
        },
        trash: { enabled: trashCb.checked, days: Math.max(1, Number(trashDays.value) || 7) },
        relay: relay,
        expires_at: expInput.value ? new Date(expInput.value).toISOString() : null,
      };
      if (isCreate && idInput.value.trim()) body.id = idInput.value.trim();
      return body;
    }
    function validate(body) {
      if (isCreate && body.id !== undefined) {
        if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(body.id)) return 'Key ID 只能包含字母、数字、中划线和下划线';
      }
      if (!body.drives.length) return st.scope === 'cross' ? '跨网盘：请至少勾选一个当前网盘' : '请选择要授权的网盘';
      if (body.drives.some((g) => !g.perms.length)) return '每个网盘授权至少需要勾选一项权限';
      if (body.drives.some((g) => !/^\//.test(g.root))) return '根目录必须以 / 开头';
      if (!body.default_drive) return '请选择默认网盘（简短调用 /v1/list?path=/ 时使用）';
      if (!body.drives.some((g) => g.drive === body.default_drive)) return '默认网盘必须是已授权的网盘';
      if (body.relay) {
        if (body.relay.quota_bytes < 0) return '中转容量配额不能为负数';
        if (body.relay.max_age_days < 0 || body.relay.max_age_days > 7) return '中转保留天数只能在 0–7 之间（0 = 默认 7 天）';
        if (body.relay.drive && !body.drives.some((g) => g.drive === body.relay.drive)) return '中转网盘必须是该 Key 已授权的网盘';
        if (!hasRelayPermState()) return '当前权限不含 relay（中转）：请选择「通用/全部」权限组，或在高级设置中勾选 relay';
      }
      return null;
    }

    /* ---- assemble ---- */
    const addGrantBtn = h('button', { class: 'btn btn-ghost btn-sm', type: 'button' }, icon('plus', 12), '添加网盘授权');
    addGrantBtn.addEventListener('click', () => addGrantRow());
    const advBody = h('div', { class: 'adv-body' },
      h('div', { class: 'field' },
        h('label', { class: 'field-label' }, '网盘授权（白名单 + chroot + 权限）'),
        h('div', { class: 'grant-row-head' }, h('div', null, h('div', null, '网盘'), h('div', { style: 'margin-top:4px' }, '根目录 chroot')), h('div', null, '权限')),
        grantsHost,
        h('div', { class: 'grant-add' }, addGrantBtn),
        h('div', { class: 'field-help', style: 'margin-top:6px' }, 'agent 看到的 "/" 即其根目录，越界一律拒绝；此处修改会覆盖上方权限组状态（显示为「自定义」）。')),
      h('div', { class: 'field' },
        h('label', { class: 'field-label' }, '全局权限（对已授权网盘额外授予）'),
        gpermGrid,
        h('div', { class: 'field-help' }, '有效权限是全局权限与逐盘权限的并集。若全局仍勾选某权限，取消某盘的同一权限不会撤销它；需同时取消全局项。不会授权未勾选的网盘或绕过根目录。')),
      h('div', { class: 'form-row-3' },
        h('div', { class: 'field' },
          h('label', { class: 'field-label' }, '单文件上传上限'),
          h('div', { class: 'flex gap-8' }, upVal, upUnit),
          h('div', { class: 'field-help' }, '留空表示不限制')),
        h('div', { class: 'field' },
          h('label', { class: 'field-label' }, '每分钟请求数'),
          rpmVal,
          h('div', { class: 'field-help' }, '令牌桶限流，留空不限制')),
        h('div', { class: 'field' },
          h('label', { class: 'field-label' }, '并发任务数'),
          concVal,
          h('div', { class: 'field-help' }, '同时进行的转存/拉取数'))),
      h('div', { class: 'form-row' },
        h('div', { class: 'field' },
          h('label', { class: 'field-label' }, '回收站'),
          h('label', { class: 'check-block' },
            trashCb,
            h('span', null,
              h('div', { class: 'cb-title' }, '启用（删除先进入 /.c2a-trash）'),
              h('div', { class: 'cb-sub' }, 'agent 误删可自行恢复；彻底删除需要 purge 权限')))),
        h('div', { class: 'field' },
          h('label', { class: 'field-label' }, '回收站保留天数'),
          trashDays,
          h('div', { class: 'field-help' }, '1–30 天，到期自动清理'))),
      h('div', { class: 'field' },
        h('label', { class: 'check-block relay-check' },
          relayCb,
          h('span', null,
            h('div', { class: 'cb-title' }, '启用 Gokapi 兼容中转（/api/files/*）'),
            h('div', { class: 'cb-sub' }, '为 agent 提供文件中转语义：上传返回 ID + CRC64、限时直链、按容量与保留期自动淘汰；已有 Gokapi skill 只需换 Base URL 和 Key 即可使用。'))),
        relayBody),
      h('div', { class: 'field' },
        h('label', { class: 'field-label' }, '过期时间（可选）'),
        expInput,
        h('div', { class: 'field-help' }, '留空表示永不过期。子 Key TTL 默认 24 小时、最长 7 天。')));
    const advDetails = h('details', { class: 'adv-details' },
      h('summary', null, '高级权限设置',
        h('span', { class: 'adv-summary-sub' }, '按网盘细项权限 · 全局权限 · 限额 · 回收站 · Gokapi 中转 · 有效期')),
      advBody);
    if (!isCreate && st.preset === 'custom') advDetails.open = true; // legacy/custom keys open expanded

    C2A.modal({
      title: isCreate ? '创建 API Key' : '编辑 Key · ' + k.id,
      width: 'lg',
      body(body) {
        body.append(
          h('div', { class: 'form-row' },
            h('div', { class: 'field' },
              h('label', { class: 'field-label' }, 'Key ID', isCreate ? null : h('span', { class: 'req' }, '*')),
              idInput,
              h('div', { class: 'field-help' }, isCreate ? '可留空，服务端自动生成 key-xxxx；用于审计与子 Key 归属标识，创建后不可修改。' : 'Key ID 创建后不可修改。'))),
          h('div', { class: 'field' },
            h('label', { class: 'field-label' }, '授权范围'),
            h('div', { class: 'scope-row' },
              h('label', { class: 'check scope-opt' },
                scopeSingle,
                h('span', null,
                  h('div', { class: 'cb-title' }, '单个网盘'),
                  h('div', { class: 'cb-sub' }, '只授权一个网盘，它同时是默认网盘'))),
              h('label', { class: 'check scope-opt' },
                scopeCross,
                h('span', null,
                  h('div', { class: 'cb-title' }, '跨网盘'),
                  h('div', { class: 'cb-sub' }, '明确勾选当前已纳管的网盘；之后新纳管的网盘不会自动获得授权')))),
            h('div', { class: 'scope-picker' }, singleSel, crossList)),
          h('div', { class: 'form-row' },
            h('div', { class: 'field' },
              h('label', { class: 'field-label' }, '默认网盘', h('span', { class: 'req' }, '*')),
              defSel,
              h('div', { class: 'field-help' }, '简短调用（/v1/list?path=/）定位到默认网盘；必须是已授权网盘。')),
            h('div', { class: 'field' },
              h('label', { class: 'field-label' }, '默认网盘根目录'),
              defRootIn,
              h('div', { class: 'field-help' }, 'agent 看到的 "/" 即此目录（chroot）；可填 /子目录 收窄。跨网盘时其他网盘的根目录在高级设置中单独保留。'))),
          h('div', { class: 'field' },
            h('label', { class: 'field-label' }, '权限组'),
            chipHost,
            h('div', { class: 'field-help' }, '应用于当前勾选的全部网盘；切换权限组会先显示将要新增/移除的权限并确认。细项差异在高级权限设置中修改。')),
          advDetails,
          errBox);
      },
      foot(foot, close) {
        const btn = h('button', { class: 'btn btn-primary', type: 'button' }, isCreate ? '创建并显示明文' : '保存修改');
        btn.addEventListener('click', async () => {
          if (busy) return;
          errBox.textContent = '';
          const payload = collectBody();
          const problem = validate(payload);
          if (problem) { errBox.textContent = problem; return; }
          busy = true; btn.disabled = true; btn.textContent = '提交中…';
          try {
            if (isCreate) {
              const res = await C2A.API.post('/keys', payload);
              close();
              keyResultDialog(res.key, res.plaintext, res.onboarding_prompt, true);
            } else {
              await C2A.API.patch('/keys/' + encodeURIComponent(k.id), payload);
              C2A.toast('Key ' + k.id + ' 已保存', 'success');
              close();
            }
            opts.onDone && opts.onDone();
          } catch (e) {
            errBox.textContent = e.message;
            busy = false; btn.disabled = false; btn.textContent = isCreate ? '创建并显示明文' : '保存修改';
          }
        });
        foot.append(
          h('button', { class: 'btn btn-secondary', type: 'button', onclick: () => close() }, '取消'),
          btn);
      },
    });

    /* ---- initial paint (after gpermBoxes/relay exist) ---- */
    renderSingleSel();
    renderCrossList();
    renderDefaultControls();
    renderChips();
    syncRelayUI();
    if (advDetails.open) syncAdvancedFromState();
    // build the advanced rows lazily on first open so a legacy key that is
    // only peeked at still saves from st (identical to the stored grants)
    advDetails.addEventListener('toggle', () => { if (advDetails.open && !grantsHost.querySelector('.grant-row')) syncAdvancedFromState(); });
  }

  /** Basic per-drive quickstart (default drive, else first grant): drive is a
   * single URL-encoded path segment, the key rides only in the apikey header. */
  function quickStartSnippet(keyText, drive) {
    const at = '{{BASE}}';
    const me = '# 自检：查看授权网盘与权限\ncurl -H "apikey: ' + keyText + '" "' + at + '/v1/me"';
    if (!drive) return me;
    const d = encodeURIComponent(drive);
    const listPath = encodeURIComponent('/');
    const filePath = encodeURIComponent('/report.pdf');
    return me + '\n\n' +
      '# 列出 ' + drive + ' 根目录\ncurl -H "apikey: ' + keyText + '" "' + at + '/v1/drives/' + d + '/list?path=' + listPath + '"\n\n' +
      '# 上传文件（自动计算 CRC64 / SHA-256）\ncurl -T report.pdf -H "apikey: ' + keyText + '" "' + at + '/v1/drives/' + d + '/files?path=' + filePath + '"\n\n' +
      '# 下载文件\ncurl -o report.pdf -H "apikey: ' + keyText + '" "' + at + '/v1/drives/' + d + '/files?path=' + filePath + '"';
  }

  /* ---------------- result dialog (create / rotate) ---------------- */
  function keyResultDialog(key, plaintext, onboardingPrompt, isCreate) {
    // plaintext is held in this closure only; scrubbed on close.
    let keyText = String(plaintext || '');
    C2A.modal({
      title: (isCreate ? 'Key 已创建' : '已轮换') + ' · ' + (key && key.id),
      width: 'md',
      dismissable: true,
      body(body) {
        const keyBox = C2A.codebox(keyText, { plain: true });
        body.append(
          h('div', { class: 'callout callout-danger mb-14' }, icon('alert', 16),
            h('div', { class: 'co-text' }, h('span', { style: 'font-weight:700' }, '明文只显示这一次。'),
              '请立即复制保存；关闭本窗口后无法再次查看，只能轮换生成新密钥。')),
          keyBox,
          h('div', { class: 'callout callout-info mb-14' }, icon('drives', 16),
            h('div', { class: 'co-text' },
              h('a', { href: C2A.API.isMock ? '/files/?mock=1' : '/files/', target: '_blank', rel: 'noopener noreferrer' }, '打开我的文件'),
              '，粘贴已保存的 Key 即可浏览网盘、上传和分享；密钥不会自动传入新页面。')),
          C2A.snippetTitle('基础接入提示（发给 agent 即可完成接入）'),
          (() => {
            const ta = h('textarea', { class: 'prompt-box', readonly: true, style: 'min-height:190px' }, onboardingPrompt || '');
            const wrapBtn = h('button', { class: 'btn btn-secondary btn-sm', type: 'button' }, icon('copy', 12), '复制全部');
            wrapBtn.addEventListener('click', () => C2A.copyText(ta.value, '接入提示已复制'));
            return h('div', null, h('div', { class: 'flex', style: 'justify-content:flex-end;margin-bottom:6px' }, wrapBtn), ta);
          })(),
          C2A.snippetTitle('快速开始（基础 /v1 文件接口）'),
          C2A.codebox(quickStartSnippet(keyText, effectiveDefaultDrive(key))),
          C2A.snippetTitle('命令行助手 c2a.py'),
          C2A.codebox(
            'curl -O ' + '{{BASE}}' + '/custom/c2a.py\n' +
            'C2A_URL=' + '{{BASE}}' + ' C2A_KEY=' + keyText + ' python3 c2a.py ls /'),
          C2A.snippetTitle('Agent Skill（Claude Code / Codex）'),
          C2A.codebox('把 ' + '{{BASE}}' + '/custom/SKILL.md 放入 skills 目录即可让 agent 直接使用'));
        // replace {{BASE}} once settings are known
        C2A.API.get('/settings').then((s) => {
          const base = C2A.baseUrlOf(s);
          body.querySelectorAll('.codebox').forEach((pre) => {
            const btn = pre.querySelector('.copy-btn');
            let text = '';
            // reconstruct text from child nodes excluding the button
            for (const n of pre.childNodes) {
              if (n.nodeType === 3) text += n.nodeValue;
            }
            if (text.includes('{{BASE}}')) {
              const updated = text.split('{{BASE}}').join(base);
              // keep only text nodes (codebox content was text-only)
              for (const n of [...pre.childNodes]) if (n.nodeType === 3) pre.removeChild(n);
              pre.insertBefore(document.createTextNode(updated), btn);
            }
          });
        }).catch(() => {});
      },
      onClose() {
        keyText = '';
      },
    });
  }

  /* ---------------- rotate / revoke ---------------- */
  function rotateDialog(k) {
    C2A.confirmModal({
      title: '轮换 Key · ' + k.id,
      message: '将生成新的密钥明文，旧明文立即失效。需要重新把新明文交给 agent。其子 Key 的密钥不受影响。',
      confirmText: '轮换并显示新明文',
      async onConfirm() {
        const res = await C2A.API.post('/keys/' + encodeURIComponent(k.id) + '/rotate');
        keyResultDialog(res.key, res.plaintext, res.onboarding_prompt, false);
      },
    });
  }

  function revokeDialog(k, onDone) {
    let children = [];
    C2A.API.get('/keys').then((data) => {
      children = (data.keys || []).filter((x) => x.parent_id === k.id && !x.revoked);
      if (warnEl) {
        C2A.clear(warnEl);
        if (children.length) {
          warnEl.append(h('div', { class: 'callout callout-warn' }, icon('alert', 15),
            h('div', { class: 'co-text' }, '该 Key 有 ' + children.length + ' 个未吊销的子 Key（' + children.map((c) => c.id).join('、') + '），将一并吊销。')));
        }
      }
    }).catch(() => {});
    const warnEl = h('div', { class: 'mt-14' });
    C2A.confirmModal({
      title: '吊销 Key · ' + k.id,
      message: h('p', { class: 'text-sub', style: 'line-height:1.6' },
        '吊销后该 Key 立即失效，使用它的 agent 会收到 401 unauthorized；Key 的配置保留，可以查看但不能恢复（恢复需重新创建并配置）。',
        warnEl),
      danger: true,
      confirmText: '吊销',
      async onConfirm() {
        await C2A.API.call('DELETE', '/keys/' + encodeURIComponent(k.id));
        C2A.toast('Key ' + k.id + ' 已吊销' + (children.length ? '（含 ' + children.length + ' 个子 Key）' : ''), 'success');
        if (onDone) onDone();
      },
    });
  }

  /* ---------------- 使用此 Key ---------------- */
  async function useKeyDialog(k) {
    let settings = null;
    try { settings = await C2A.API.get('/settings'); } catch (e) { /* fall back to origin */ }
    const base = C2A.baseUrlOf(settings || {});
    const masked = 'sk-c2a-****（明文仅在创建/轮换时显示一次；如已丢失请轮换）';
    const onboarding = buildOnboardingLocal(k, base, masked, (settings && settings.policy_text) || '');

    const ta = h('textarea', { class: 'prompt-box', readonly: true, style: 'min-height:220px' }, onboarding);
    const copyBtn = h('button', { class: 'btn btn-secondary btn-sm', type: 'button' }, icon('copy', 12), '复制接入提示');
    copyBtn.addEventListener('click', () => C2A.copyText(ta.value));

    const quickDrive = effectiveDefaultDrive(k);
    const quick = quickDrive
      ? 'export C2A_KEY=<你的明文>\n\n' +
        'curl -H "apikey: $C2A_KEY" "' + base + '/v1/me"   # 自检：查看授权与限额\n' +
        'curl -H "apikey: $C2A_KEY" "' + base + '/v1/drives/' + encodeURIComponent(quickDrive) + '/list?path=' + encodeURIComponent('/') + '"   # 列出 ' + quickDrive + ' 根目录'
      : 'export C2A_KEY=<你的明文>\n\n' +
        'curl -H "apikey: $C2A_KEY" "' + base + '/v1/me"   # 自检：查看授权与限额';
    const extraStd = h('div', null,
      C2A.snippetTitle('快速开始（用环境变量携带密钥）'),
      C2A.codebox(quick));

    C2A.modal({
      title: '使用此 Key · ' + k.id,
      width: 'md',
      body(body) {
        body.append(
          h('div', { class: 'callout callout-info mb-14' }, icon('info', 15),
            h('div', { class: 'co-text' }, '出于安全考虑，明文只在', h('span', { style: 'font-weight:600' }, '创建和轮换时显示一次'),
              '。以下基础接入提示中的 Key 以占位符代替：把占位符替换成你保存的明文，或直接轮换一次获取全新明文。')),
          h('div', { class: 'flex', style: 'justify-content:flex-end;margin-bottom:6px' }, copyBtn),
          ta,
          extraStd,
          h('div', { class: 'callout callout-info mt-14' }, icon('drives', 16),
            h('div', { class: 'co-text' },
              h('a', { href: C2A.API.isMock ? '/files/?mock=1' : '/files/', target: '_blank', rel: 'noopener noreferrer' }, '打开我的文件'),
              '：使用你保存的 Key 明文进入人用文件工作台；不会创建管理会话或自动传递密钥。')));
      },
    });
  }

  function buildOnboardingLocal(key, base, plaintext, policyText) {
    const drv = (key.drives || []).map((g) => {
      let mode = 'read-only';
      if ((g.perms || []).includes('write')) mode = 'read/write';
      else if ((g.perms || []).includes('list') && (g.perms || []).length > 1) mode = 'read';
      else if ((g.perms || []).includes('list')) mode = 'list-only';
      return g.drive + ' (' + mode + (g.root && g.root !== '/' ? ', root ' + g.root : '') + ')';
    }).join(', ');
    const policy = String(policyText || '').replace(/\s*\n\s*/g, ' ').trim()
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
  }
})();
