/* cloud2api admin — router, sidebar, boot */
(function () {
  'use strict';
  const C2A = window.C2A;
  const { h, icon } = C2A;

  const ROUTES = [
    { name: 'overview', title: '总览', sub: 'cloud2api 管理门户', icon: 'overview', group: null },
    { name: 'drives', title: '网盘', sub: 'OpenList 存储的发现、纳管与状态跟踪', icon: 'drives', group: '资源' },
    { name: 'keys', title: 'API Keys', sub: '白名单 + 子目录 chroot + 权限 + 配额；明文只在创建/轮换时显示一次', icon: 'keys', group: '资源' },
    { name: 'audit', title: '审计', sub: '数据面调用日志', icon: 'audit', group: '运维' },
    { name: 'openlist', title: 'OpenList', sub: '上游连接、安全检查与联动事件', icon: 'openlist', group: '集成' },
    { name: 'settings', title: '设置', sub: '实例级参数', icon: 'settings', group: '集成' },
  ];
  const GROUPS = [
    { label: null, items: ['overview'] },
    { label: '资源', items: ['drives', 'keys'] },
    { label: '运维', items: ['audit'] },
    { label: '集成', items: ['openlist', 'settings'] },
  ];

  let compact = C2A.preference.get('sidebar', 'expanded') === 'compact';
  document.body.classList.toggle('sidebar-compact', compact);
  document.documentElement.dataset.theme = C2A.preference.get('theme', 'light') === 'dark' ? 'dark' : 'light';

  function currentRoute() {
    const hash = location.hash.replace(/^#\/?/, '');
    const name = hash.split('?')[0];
    return ROUTES.some((r) => r.name === name) ? name : 'overview';
  }
  C2A.currentRoute = currentRoute;

  function renderSidebar() {
    const sb = document.getElementById('sidebar');
    C2A.clear(sb);
    const brand = h('div', { class: 'brand' },
      h('div', { class: 'brand-mark' }, h('img', { src: '/admin/assets/logo.png', alt: 'cloud2api logo', width: '44', height: '44', decoding: 'async' })),
      h('div', null,
        h('div', { class: 'brand-name' }, 'cloud2api'),
        h('div', { class: 'brand-sub' }, '网盘 · Agent 网关')));
    const nav = h('nav', { class: 'nav', 'aria-label': '主导航' });
    for (const g of GROUPS) {
      const grp = h('div', { class: 'nav-group' });
      if (g.label) grp.append(h('div', { class: 'nav-group-label' }, g.label));
      for (const name of g.items) {
        const r = ROUTES.find((x) => x.name === name);
        grp.append(h('a', { class: 'nav-item', dataset: { route: name }, href: '#/' + name, title: r.title, 'aria-label': r.title },
          icon(r.icon, 16),
          h('span', null, r.title)));
      }
      if (g.label === '资源') {
        grp.append(h('a', { class: 'nav-item', href: C2A.API.isMock ? '/files/?mock=1' : '/files/', target: '_blank', rel: 'noopener noreferrer', title: '用受限 API Key 浏览、上传与分享文件', 'aria-label': '我的文件（新窗口）' },
          icon('drives', 16), h('span', null, '我的文件')));
      }
      nav.append(grp);
    }
    const foot = h('div', { class: 'sidebar-foot' },
      h('div', { class: 'session-card' },
        h('div', { class: 'avatar' }, '管'),
        h('div', { class: 'grow' },
          h('div', { class: 'session-name' }, '管理员'),
          h('div', { class: 'session-hint' }, '管理会话已登录')),
        h('button', { class: 'icon-btn', type: 'button', 'aria-label': '退出登录', title: '退出登录', onclick: logout }, icon('logout', 15))));
    const collapse = h('button', { id: 'sidebar-collapse', class: 'sidebar-collapse', type: 'button', 'aria-label': compact ? '展开侧栏' : '收起侧栏', 'aria-expanded': String(!compact), 'aria-controls': 'sidebar', title: compact ? '展开侧栏' : '收起侧栏' }, icon('collapse', 16));
    collapse.addEventListener('click', () => {
      compact = !compact;
      C2A.preference.set('sidebar', compact ? 'compact' : 'expanded');
      document.body.classList.toggle('sidebar-compact', compact);
      collapse.setAttribute('aria-expanded', String(!compact));
      collapse.setAttribute('aria-label', compact ? '展开侧栏' : '收起侧栏');
      collapse.title = compact ? '展开侧栏' : '收起侧栏';
    });
    sb.append(brand, nav, foot, collapse);
  }

  function markActive(name) {
    document.querySelectorAll('.nav-item').forEach((el) => {
      el.classList.toggle('active', el.dataset.route === name);
      if (el.dataset.route === name) el.setAttribute('aria-current', 'page');
      else el.removeAttribute('aria-current');
    });
  }

  /* Renders are serialized: overlapping triggers (rapid navigation, a refresh
   * during an in-flight render) would otherwise interleave two page renders
   * into the same root and duplicate cards. Each render fully clears the root
   * when it starts, so the last one always wins with a clean page. */
  let routeChain = Promise.resolve();

  /* Page lifecycle generation: bumped at the START of every route render, so
   * it changes whenever the route moves — away from a page AND back to it.
   * Async page work captures C2A.pageGen() when it starts and compares later:
   * a mismatch means the router owns a different route now and the stale
   * continuation must not touch its (possibly detached) DOM. Pages keep their
   * own instance counter for direct re-renders that bypass route() (e.g. a
   * page's fullReload). */
  let pageGen = 0;
  C2A.pageGen = function () { return pageGen; };

  async function route() {
    routeChain = routeChain.then(doRoute).catch(() => {});
    return routeChain;
  }

  async function doRoute() {
    pageGen++;
    const name = currentRoute();
    const r = ROUTES.find((x) => x.name === name) || ROUTES[0];
    document.getElementById('page-title').textContent = r.title;
    document.getElementById('page-sub').textContent = r.sub;
    markActive(name);
    closeSidebarMobile();
    const root = document.getElementById('page');
    const refreshBtn = document.getElementById('refresh-btn');
    refreshBtn.disabled = true;
    refreshBtn.classList.add('refreshing');
    root.setAttribute('aria-busy', 'true');
    root.classList.remove('page-enter');
    C2A.clear(root);
    try {
      await C2A.pages[name](root);
    } catch (e) {
      C2A.clear(root);
      root.append(C2A.errorState(e.message || String(e), () => route()));
    } finally {
      root.removeAttribute('aria-busy');
      root.classList.add('page-enter');
      refreshBtn.disabled = false;
      refreshBtn.classList.remove('refreshing');
    }
  }

  async function logout() {
    try { await C2A.API.post('/logout'); } catch (e) { /* ignore */ }
    if (C2A.API.isMock) {
      C2A.toast('演示模式：已模拟退出登录', 'success');
    } else {
      location.href = '/admin/login';
    }
  }

  /* mobile sidebar */
  function openSidebarMobile() {
    document.getElementById('sidebar').classList.add('open');
    document.getElementById('sb-overlay').hidden = false;
    document.getElementById('nav-toggle').setAttribute('aria-expanded', 'true');
  }
  function closeSidebarMobile() {
    document.getElementById('sidebar').classList.remove('open');
    document.getElementById('sb-overlay').hidden = true;
    document.getElementById('nav-toggle').setAttribute('aria-expanded', 'false');
  }

  async function boot() {
    renderSidebar();
    const navToggle = document.getElementById('nav-toggle');
    navToggle.append(icon('menu', 19));
    navToggle.addEventListener('click', () => document.getElementById('sidebar').classList.contains('open') ? closeSidebarMobile() : openSidebarMobile());
    document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeSidebarMobile(); });
    const themeBtn = document.getElementById('theme-btn');
    function renderThemeButton() {
      const dark = document.documentElement.dataset.theme === 'dark';
      C2A.clear(themeBtn);
      themeBtn.append(icon(dark ? 'sun' : 'moon', 18));
      themeBtn.title = dark ? '切换浅色主题' : '切换深色主题';
      themeBtn.setAttribute('aria-label', themeBtn.title);
    }
    renderThemeButton();
    themeBtn.addEventListener('click', () => {
      const theme = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
      document.documentElement.dataset.theme = theme;
      C2A.preference.set('theme', theme);
      renderThemeButton();
    });
    const logoutBtn = document.getElementById('logout-btn');
    logoutBtn.append(icon('logout', 18));
    logoutBtn.addEventListener('click', logout);
    document.getElementById('sb-overlay').addEventListener('click', closeSidebarMobile);
    const refreshBtn = document.getElementById('refresh-btn');
    refreshBtn.appendChild(icon('refresh', 14));
    refreshBtn.appendChild(document.createTextNode('刷新'));
    refreshBtn.addEventListener('click', () => route());
    if (C2A.API.isMock) document.getElementById('mock-pill').hidden = false;

    window.addEventListener('hashchange', route);

    // auth check (real mode redirects to /admin/login on 401)
    if (!C2A.API.isMock) {
      try {
        await C2A.API.get('/overview', { noRedirect: true });
      } catch (e) {
        if (e.status === 401) { location.href = '/admin/login'; return; }
        C2A.toast(e.message + '（部分功能可能不可用）', 'warn');
      }
    }
    route();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
