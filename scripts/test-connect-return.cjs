#!/usr/bin/env node
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const root = path.resolve(__dirname, '..');
const script = fs.readFileSync(path.join(root, 'internal/admin/web/assets/connect-return.js'), 'utf8');
let count = 0;
for (const [search, expected] of [
  ['?storage_id=7', '/admin/#/drives?connected=7'],
  ['?storage_id=123', '/admin/#/drives?connected=123'],
  ['', '/admin/#/drives'],
  ['?storage_id=0', undefined],
  ['?storage_id=-1', undefined],
  ['?storage_id=01', undefined],
  ['?storage_id=%3Cscript%3E', undefined],
  ['?storage_id=https%3A%2F%2Fevil.example.com', undefined],
]) {
  let destination;
  vm.runInNewContext(script, { URLSearchParams, encodeURIComponent, location: {
    search, replace(value) { destination = value; },
  } });
  assert.equal(destination, expected, 'unsafe or missing landing destination: ' + search);
  count++;
}
assert.ok(!script.includes('localStorage') && !script.includes('sessionStorage') && !script.includes('fetch('), 'landing must not establish authentication or browser storage');
count++;
console.log('PASS ' + count + ' strict-cookie return landing checks');
