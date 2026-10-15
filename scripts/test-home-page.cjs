#!/usr/bin/env node
/* Static contract checks for the public navigation page / (web/home.html).
 *
 * The home page is served under a strict CSP (script-src 'self';
 * style-src 'self'; no inline script/style) and must stay credential-free:
 * it only links to the two entry points and loads the same public assets the
 * /files/ shell serves (internal/admin/files.go allowlist: app.css, files.css,
 * files.js, logo.png). These checks are pure file inspections — zero network.
 */
'use strict';
const fs = require('node:fs');
const path = require('node:path');

const HOME = path.resolve(__dirname, '..', 'internal', 'admin', 'web', 'home.html');
const APP_CSS = path.resolve(__dirname, '..', 'internal', 'admin', 'web', 'assets', 'app.css');
const FILES_CSS = path.resolve(__dirname, '..', 'internal', 'admin', 'web', 'assets', 'files.css');
const ALLOWED_ASSETS = ['app.css', 'files.css', 'files.js', 'logo.png'];

let pass = 0;
let failed = 0;
function ok(cond, msg) {
  console.log((cond ? 'PASS ' : 'FAIL ') + msg);
  if (cond) pass++; else failed++;
}

const html = fs.existsSync(HOME) ? fs.readFileSync(HOME, 'utf8') : null;
ok(html !== null, 'HOME web/home.html exists');
if (html === null) process.exit(1);

/* CSP-safe: no inline script and no style="" / <style> blocks */
ok(!/<script(?![^>]*\bsrc=)/i.test(html), 'HOME no inline <script> blocks (CSP script-src self)');
ok(!/<style/i.test(html), 'HOME no inline <style> blocks (CSP style-src self)');
ok(!/\sstyle\s*=\s*"/i.test(html), 'HOME no style="" attributes (CSP style-src self)');
ok(!/\son[a-z]+\s*=\s*"/i.test(html), 'HOME no inline event handlers (CSP script-src self)');

/* navigation links */
ok(/href="\/files\/"/.test(html), 'HOME links to the file workspace /files/');
ok(/href="\/admin\/"/.test(html), 'HOME links to the admin portal /admin/');
ok(!/href="https?:\/\//.test(html), 'HOME links stay same-origin (no absolute hrefs)');

/* assets reference only the /files/assets allowlist */
const assetRefs = [...html.matchAll(/(?:href|src)="(\/files\/assets\/[^"]+)"/g)].map((m) => m[1]);
ok(assetRefs.length >= 3, 'HOME references shared public assets (' + assetRefs.join(', ') + ')');
ok(assetRefs.every((a) => ALLOWED_ASSETS.includes(a.replace('/files/assets/', ''))),
  'HOME only references the served /files/assets allowlist');
ok(/href="\/files\/assets\/app\.css"/.test(html), 'HOME loads app.css design tokens');
ok(/href="\/files\/assets\/files\.css"/.test(html), 'HOME loads files.css page styles');
ok(/src="\/files\/assets\/logo\.png"/.test(html), 'HOME shows the project logo');

/* credentials must never appear: no password/key inputs, no forms, no fetch/XHR */
ok(!/<form/i.test(html), 'HOME contains no forms (navigation only, no credentials)');
ok(!/type="password"/i.test(html), 'HOME contains no password inputs');
ok(/noindex|robots/.test(html) || /<meta name="robots" content="noindex">/.test(html), 'HOME is marked noindex');
ok(/name="referrer" content="no-referrer"/.test(html), 'HOME sets no-referrer');

/* classes used by the markup exist in files.css (visual reuse, responsive + dark) */
const css = fs.readFileSync(FILES_CSS, 'utf8');
const homeClasses = [...html.matchAll(/class="([^"]+)"/g)]
  .flatMap((m) => m[1].split(/\s+/))
  .filter((c) => c.startsWith('home-'));
ok(homeClasses.length > 0, 'HOME uses home-* classes (' + [...new Set(homeClasses)].join(', ') + ')');
const missing = [...new Set(homeClasses)].filter((c) => !new RegExp('\\.' + c.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '[\\s,{:.]').test(css));
ok(missing.length === 0, 'HOME every home-* class is styled in files.css, missing: ' + JSON.stringify(missing));

/* responsive + dark-mode coverage for the home page */
ok(/@media \(max-width: 760px\)[\s\S]*\.home-grid\s*{[^}]*grid-template-columns:\s*1fr/.test(css),
  'HOME home-grid collapses to one column on narrow screens');
ok(/\.home-grid\s*>\s*\.home-card\s*\{[^}]*margin:\s*0\s*;/.test(css),
  'HOME grid cards override the shared stacked-card sibling margin');
ok(/@media \(prefers-color-scheme: dark\)/.test(css), 'HOME dark mode follows the system preference');
ok(/:root:not\(\[data-theme\]\)/.test(css), 'HOME dark tokens apply only without an explicit data-theme (files page unaffected)');

/* the dark token block mirrors app.css dark values for the shared variables */
const appCss = fs.readFileSync(APP_CSS, 'utf8');
const darkBlock = /@media \(prefers-color-scheme: dark\)\s*{\s*:root:not\(\[data-theme\]\)\s*{([\s\S]*?)\n  }\n}/.exec(css);
ok(!!darkBlock, 'HOME dark token block found in files.css');
if (darkBlock) {
  const want = ['--bg', '--surface', '--text', '--ok', '--border'];
  const have = darkBlock[1];
  ok(want.every((v) => have.includes(v)), 'HOME dark block redefines the core tokens (' + want.join(' ') + ')');
  const bgDark = /--bg:\s*(#[0-9a-fA-F]+)/.exec(have);
  const appDark = /:root\[data-theme="dark"\]\s*{[\s\S]*?--bg:\s*(#[0-9a-fA-F]+)/.exec(appCss);
  ok(bgDark && appDark && bgDark[1].toLowerCase() === appDark[1].toLowerCase(),
    'HOME dark --bg matches app.css dark --bg (' + (bgDark && bgDark[1]) + ')');
}

/* language / titles */
ok(/<html lang="zh-CN">/.test(html), 'HOME declares zh-CN like the other shells');
ok(/<title>cloud2api<\/title>/.test(html), 'HOME has the product title');

console.log('\n' + (failed ? failed + ' CHECK(S) FAILED' : 'ALL HOME-PAGE TESTS PASSED') + ' (' + pass + ' passed, ' + failed + ' failed)');
process.exit(failed ? 1 : 0);
