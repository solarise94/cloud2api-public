/* Minimal DOM harness for key-form state transitions. */
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const DIR = path.resolve(__dirname, '../internal/admin/web/assets');

function mkEl(tag) {
  const el = {
    nodeType: 1, tagName: tag, className: '', children: [], _listeners: {}, attrs: {},
    style: {}, dataset: {}, value: '', disabled: false, checked: false,
    selected: false, hidden: false, open: false, textContent: '', innerHTML: '',
    // select.value must reflect the selected option like a real browser
    get value() {
      if (this.tagName === 'select') {
        const chosen = this.children.find((o) => o.tagName === 'option' && o.selected);
        if (chosen) return chosen.attrs.value ?? '';
        const first = this.children.find((o) => o.tagName === 'option');
        if (first) return first.attrs.value ?? '';
        return this._value ?? '';
      }
      return this._value ?? '';
    },
    set value(v) {
      this._value = String(v);
      if (this.tagName === 'select') {
        let hit = false;
        for (const o of this.children) {
          if (o.tagName !== 'option') continue;
          if (!hit && o.attrs.value === String(v)) { o.selected = true; hit = true; }
          else o.selected = false;
        }
        if (!hit) { const first = this.children.find((o) => o.tagName === 'option'); if (first) first.selected = true; }
      }
    },
    get firstChild() { return this.children[0] || null; },
    append(...cs) { for (const c of cs) this.children.push(c); },
    prepend(c) { this.children.unshift(c); },
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) this.children.splice(i, 1); },
    addEventListener(t, fn) { (this._listeners[t] = this._listeners[t] || []).push(fn); },
    removeEventListener() {},
    setAttribute(k, v) {
      this.attrs[k] = v === undefined ? '' : String(v);
      if (k === 'class') this.className = String(v);
      // mirror the attribute→property reflection real browsers perform
      else if (k === 'checked') this.checked = true;
      else if (k === 'value') this.value = String(v);
      else if (k === 'disabled') this.disabled = true;
      else if (k === 'selected') this.selected = true;
    },
    getAttribute(k) { return this.attrs[k] ?? null; },
    removeAttribute() {},
    querySelector() { return mkEl('span'); },
    querySelectorAll(sel) {
      // minimal '.class' subtree selector — enough for grantsHost.querySelectorAll('.grant-row')
      const cls = String(sel).replace(/^\./, '').trim();
      const out = [];
      (function w(el) {
        for (const c of el.children || []) {
          if (c.nodeType === 1) {
            if (String(c.className).split(' ').includes(cls)) out.push(c);
            w(c);
          }
        }
      }(this));
      return out;
    },
    closest() { return null; },
    focus() {},
    fire(t) { for (const fn of (this._listeners[t] || []).slice()) fn({ target: this }); },
  };
  el.classList = { add() {}, remove() {}, toggle() {}, contains() { return false; } };
  return el;
}
function textOf(el) {
  let out = '';
  for (const c of el.children || []) {
    if (typeof c === 'string') out += c;
    else if (c.nodeType === 3) out += c.text;
    else if (c.nodeType === 1 && !['input', 'select', 'textarea'].includes(c.tagName)) out += textOf(c);
  }
  return out;
}
function hasClass(el, cls) {
  return String(el.className).split(' ').includes(cls);
}
function findAll(root, pred, out) {
  out = out || [];
  (function walk(el) {
    if (!el || typeof el !== 'object' || el.nodeType !== 1) return;
    try { if (pred(el)) out.push(el); } catch (e) { /* malformed stub node */ }
    for (const c of el.children || []) walk(c);
  }(root));
  return out;
}

const store = {};
global.localStorage = { getItem: (k) => store[k] ?? null, setItem: (k, v) => { store[k] = String(v); } };
global.sessionStorage = { getItem: () => null, setItem: () => {} };
global.location = { search: '?mock=1', href: 'http://x/admin/app.html', origin: 'http://x', hash: '#/keys', pathname: '/admin/app.html' };
Object.defineProperty(global, 'navigator', { value: {}, configurable: true });
global.window = global;
global.document = {
  readyState: 'loading',
  addEventListener() {}, removeEventListener() {},
  createTextNode(t) { return { nodeType: 3, text: String(t) }; },
  getElementById: () => null,
  querySelector: () => null,
  querySelectorAll: () => [],
  body: mkEl('body'),
  documentElement: { dataset: {} },
  createElement: (tag) => mkEl(tag),
};
global.window.C2A = global.window.C2A || {};

for (const f of ['api.js', 'mock.js', 'ui.js', 'pages-main.js', 'pages-drives.js', 'pages-keys.js', 'app.js']) {
  eval(fs.readFileSync(DIR + '/' + f, 'utf8'));
}
const C2A = window.C2A;

let modalBody = null, menuItems = null;
C2A.modal = (opts) => {
  const bodyEl = mkEl('div'), footEl = mkEl('div');
  if (opts.body) opts.body(bodyEl, () => {});
  if (opts.foot) opts.foot(footEl, () => {});
  modalBody = bodyEl;
  return { close() {}, body: bodyEl, foot: footEl };
};
C2A.confirmModal = () => { throw new Error('confirmModal must not fire at dialog open'); };
C2A.toast = () => {};
C2A.menu = (items) => { menuItems = items; return mkEl('button'); };

let currentKey = null;
window.C2AMockBackend = function () {
  this.handle = async (method, path) => {
    if (path === '/keys' && method === 'GET') return { keys: [currentKey] };
    if (path === '/drives' && method === 'GET') return { drives: [{ id: 'a' }, { id: 'b' }] };
    throw new Error('unexpected ' + method + ' ' + path);
  };
};

async function openEditDialog(key) {
  currentKey = key;
  menuItems = null; modalBody = null;
  const root = mkEl('div');
  await C2A.pages.keys(root);
  const editItem = (menuItems || []).find((it) => it.label === '编辑');
  if (!editItem) throw new Error('编辑 menu item not found');
  await editItem.onSelect();
  if (!modalBody) throw new Error('edit dialog did not open');
  return modalBody; // rendered dialog body element
}

function findPlaceholderSelect(rootEl) {
  return findAll(rootEl, (el) => el.tagName === 'select' &&
    el.children.some((o) => o.tagName === 'option' && textOf(o).includes('请选择默认网盘（必填）')))[0] || null;
}
function findDefaultSelect(rootEl) {
  // the default-drive select never carries the single-picker's 选择网盘… option
  return findPlaceholderSelect(rootEl) ||
    findAll(rootEl, (el) => el.tagName === 'select' &&
      !el.children.some((o) => textOf(o).includes('选择网盘')) &&
      el.children.some((o) => o.tagName === 'option' && ['a', 'b'].includes(o.attrs.value)))[0] || null;
}
function findGrantRows(rootEl) {
  const gt = findAll(rootEl, (el) => String(el.className).includes('grant-table'))[0];
  return gt ? gt.children.filter((r) => String(r.className).includes('grant-row')) : [];
}
function rowPermHost(row) {
  return findAll(row, (el) => String(el.className).includes('grant-perms'))[0];
}
function rowPerms(row) {
  const out = {};
  for (const label of rowPermHost(row).children) out[label.attrs.title] = !!label.children[0].checked;
  return out;
}
function rowDrive(row) {
  const sel = row.children[0].children[0];
  const chosen = sel.children.find((o) => o.selected);
  return chosen ? chosen.attrs.value : sel.value;
}

(async () => {
  let failed = 0;
  const ok = (cond, msg) => { console.log((cond ? 'PASS ' : 'FAIL ') + msg); if (!cond) failed++; };

  /* ---- T1: legacy cross key, default_drive=null ---- */
  {
    const key = {
      id: 'legacy-x', default_drive: null,
      drives: [
        { drive: 'a', root: '/', perms: ['list', 'read'] },
        { drive: 'b', root: '/keep-b', perms: ['list', 'read'] },
      ],
      global_perms: [],
    };
    const body = await openEditDialog(key);
    const sel = findPlaceholderSelect(body);
    ok(!!sel, 'T1 placeholder select rendered for legacy null-default cross key');
    ok(sel && sel.disabled === false, 'T1 default select is enabled (choosable)');
    const values = sel ? sel.children.filter((o) => o.tagName === 'option').map((o) => o.attrs.value) : [];
    ok(values[0] === '' && values.includes('a') && values.includes('b'),
      'T1 options = placeholder + every granted drive (got: ' + JSON.stringify(values) + ')');
    sel.value = 'b';
    sel.fire('change');
    const rootIn = findAll(body, (el) => el.tagName === 'input' && el.attrs.placeholder === '/' && el.disabled === false);
    ok(rootIn.some((el) => el.value === '/keep-b'), 'T1 choosing a drive enables its root input prefilled with the saved root');
    ok(sel.value === 'b', 'T1 default drive selection sticks');
  }

  /* ---- T2: untick/re-tick preserves fresh advanced edits ---- */
  {
    const key = {
      id: 'custom-x', default_drive: null,
      drives: [
        { drive: 'a', root: '/', perms: ['list', 'read', 'delete'] },
        { drive: 'b', root: '/b', perms: ['write'] },
      ],
      global_perms: [],
    };
    const body = await openEditDialog(key);
    let rows = findGrantRows(body);
    ok(rows.length === 2, 'T2 advanced rows built for custom legacy key (auto-expanded)');

    const selOpen = findPlaceholderSelect(body);
    ok(selOpen && !selOpen.children.some((o) => o.selected && o.attrs.value),
      'T2 no default drive silently promoted when the legacy key had none (at open)');

    const labelB = findAll(body, (el) => hasClass(el, 'drive-check') && textOf(el) === 'b')[0];
    ok(!!labelB, 'T2 cross-drive checklist contains drive b');
    const cb = labelB.children[0];
    ok(cb && cb.checked, 'T2 cross-drive checkbox for b starts ticked');

    cb.checked = false; cb.fire('change'); // untick b
    rows = findGrantRows(body);
    ok(rows.length === 1 && rowDrive(rows[0]) === 'a', 'T2 unticking b drops only its row');

    // fresh advanced edit on a: remove 'delete'
    const rowA = rows[0];
    const delLabel = rowPermHost(rowA).children.find((l) => l.attrs.title === 'delete');
    const delInput = delLabel.children[0];
    delInput.checked = false; delInput.fire('change');

    // re-tick b
    cb.checked = true; cb.fire('change');
    rows = findGrantRows(body);
    ok(rows.length === 2, 'T2 re-ticking b restores its row');
    const byDrive = {};
    for (const r of rows) byDrive[rowDrive(r)] = rowPerms(r);
    ok(byDrive.a && byDrive.a.list && byDrive.a.read && !byDrive.a.delete,
      'T2 a keeps the fresh advanced edit (delete removed) after b untick/re-tick');
    ok(byDrive.b && byDrive.b.write && !byDrive.b.list && !byDrive.b.read,
      'T2 b restores its own prior draft perms (write), not a preset/common set');

    const selAfter = findDefaultSelect(body);
    ok(selAfter && selAfter.value === 'b',
      'T2 re-ticking b promotes it visibly in the default select (explicit user action)');
  }

  /* ---- T3: scope picker visibility ---- */
  {
    const key = {
      id: 'single-y', default_drive: 'a',
      drives: [{ drive: 'a', root: '/', perms: ['list', 'read', 'write'] }],
      global_perms: [],
    };
    const body = await openEditDialog(key);
    const picker = findAll(body, (el) => String(el.className).includes('scope-picker'))[0];
    const single = picker ? picker.children.find((el) => el.tagName === 'select') : null;
    const cross = picker ? picker.children.find((el) => String(el.className).includes('drive-checklist')) : null;
    ok(cross && cross.style.display === 'none', 'T3 cross checklist hidden in single-drive scope');
    ok(single && single.style.display !== 'none', 'T3 single-drive picker visible in single scope');
  }

  console.log(failed ? ('\n' + failed + ' FAILED') : '\nALL TESTS PASSED');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error('HARNESS ERROR', e); process.exit(1); });
