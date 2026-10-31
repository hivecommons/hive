'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function settings() {
  const context = { _configState: { dirty: {}, data: { auto_merge: {} } } };
  vm.createContext(context);
  vm.runInContext(fs.readFileSync(path.join(__dirname,
    '../../src/pkg/dashboard/static-src/js/config-helpers.js'), 'utf8'), context);
  return context;
}

test('native queue staging preserves partial fields and never silently enables', () => {
  const context = settings();
  context.markDirtyNativeQueue('repos', ' HiveCommons/Hive, acme/widget ');
  context.markDirtyNativeQueue('bases', 'hivecommons/hive#v5, acme/widget#Release/V5');
  context.markDirtyNativeQueue('probe_ttl', '5m');
  const staged = JSON.parse(JSON.stringify(context._configState.dirty['auto-merge'].native_merge_queue));
  assert.deepEqual(staged, {
    repos: ['HiveCommons/Hive', 'acme/widget'],
    bases: ['hivecommons/hive#v5', 'acme/widget#Release/V5'], probe_ttl: '5m',
  });
  assert.equal(context._configState.data.auto_merge.native_merge_queue, undefined);
});

test('native queue enable and disable stage owner policy without dropping scope', () => {
  const context = settings();
  const classes = new Set();
  const attributes = {};
  const element = {
    dataset: {},
    classList: {
      contains: key => classes.has(key),
      toggle(key) { if (classes.delete(key)) return false; classes.add(key); return true; },
    },
    setAttribute: (key, value) => { attributes[key] = value; },
  };
  context.markDirtyNativeQueue('bases', 'hivecommons/hive#v5');
  context.toggleConfigSwitch(element, 'native-queue', 'enabled');
  assert.equal(context._configState.dirty['auto-merge'].native_merge_queue.enabled, true);
  assert.equal(attributes['aria-checked'], 'true');
  context.toggleConfigSwitch(element, 'native-queue', 'enabled');
  const staged = context._configState.dirty['auto-merge'].native_merge_queue;
  assert.equal(staged.enabled, false);
  assert.equal(staged.bases[0], 'hivecommons/hive#v5');
  assert.equal(attributes['aria-checked'], 'false');
  element.dataset.disabled = 'true';
  context.toggleConfigSwitch(element, 'native-queue', 'enabled');
  assert.equal(staged.enabled, false);
});
