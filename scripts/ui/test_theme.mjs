import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import vm from 'node:vm';

const root = fileURLToPath(new URL('../../', import.meta.url));
const source = readFileSync(`${root}internal/localui/assets/theme.js`, 'utf8');
const html = readFileSync(`${root}internal/localui/assets/index.html`, 'utf8');
const css = readFileSync(`${root}internal/localui/assets/app.css`, 'utf8');
const cases = [];
const key = 'my-whatsapp-theme';

function harness({dark = false, stored = null, blockedStorage = false, blockedStorageGetter = false, noMedia = false, legacyMedia = false, ready = false} = {}) {
  const listeners = new Map();
  const mediaListeners = [];
  const controls = [0, 1].map(() => ({value: 'system', listeners: new Map(), addEventListener(name, handler) {this.listeners.set(name, handler);}}));
  const values = new Map(stored === null ? [] : [[key, stored]]);
  const writes = [];
  const storage = {
    getItem(name) {if (blockedStorage) throw new Error('storage blocked'); return values.get(name) ?? null;},
    setItem(name, value) {if (blockedStorage) throw new Error('storage blocked'); values.set(name, value); writes.push([name, value]);},
    removeItem(name) {if (blockedStorage) throw new Error('storage blocked'); values.delete(name); writes.push([name, null]);},
  };
  const document = {documentElement: {dataset: {}}, readyState: ready ? 'complete' : 'loading', querySelectorAll(selector) {assert.equal(selector, '[data-theme-select]'); return controls;}, addEventListener(name, handler) {listeners.set(`document:${name}`, handler);}};
  const media = {matches: dark};
  if (legacyMedia) media.addListener = handler => mediaListeners.push(handler);
  else media.addEventListener = (name, handler) => {assert.equal(name, 'change'); mediaListeners.push(handler);};
  const window = {localStorage: storage, addEventListener(name, handler) {listeners.set(`window:${name}`, handler);}};
  if (blockedStorageGetter) Object.defineProperty(window, 'localStorage', {get() {throw new Error('storage unavailable');}});
  if (!noMedia) window.matchMedia = query => {assert.equal(query, '(prefers-color-scheme: dark)'); return media;};
  vm.runInNewContext(source, {window, document});
  return {
    document, controls, values, writes,
    theme: () => document.documentElement.dataset.theme,
    preference: () => document.documentElement.dataset.themePreference,
    ready() {listeners.get('document:DOMContentLoaded')?.();},
    select(value, index = 0) {const control = controls[index]; control.value = value; control.listeners.get('change')?.({target: control});},
    os(value) {media.matches = value; for (const handler of mediaListeners) handler({matches: value});},
    storage(value, name = key) {listeners.get('window:storage')?.({key: name, newValue: value, storageArea: storage});},
  };
}

function check(name, run) {run(); cases.push(name);}

check('the shipped bootstrap resolves the OS theme before the document is ready', () => {
  for (const dark of [false, true]) {const h = harness({dark}); assert.equal(h.theme(), dark ? 'dark' : 'light'); assert.equal(h.preference(), 'system'); assert.equal(h.writes.length, 0);}
});
check('a valid saved override takes precedence over the OS on first paint', () => {
  assert.equal(harness({dark: false, stored: 'dark'}).theme(), 'dark');
  assert.equal(harness({dark: true, stored: 'light'}).theme(), 'light');
});
check('untrusted saved values fall back to System without becoming DOM attributes', () => {
  for (const stored of ['', 'Dark', 'undefined', '<script>', 'light dark']) {const h = harness({dark: true, stored}); assert.equal(h.theme(), 'dark'); assert.equal(h.preference(), 'system'); assert.equal(h.writes.length, 0);}
});
check('OS changes update System and leave a manual selection in place', () => {
  const h = harness(); h.ready(); h.os(true); assert.equal(h.theme(), 'dark'); h.select('light'); h.os(false); h.os(true); assert.equal(h.theme(), 'light'); assert.equal(h.preference(), 'light');
});
check('both native controls synchronize and persist a changed preference', () => {
  const h = harness(); h.ready(); h.select('dark', 1); assert.equal(h.theme(), 'dark'); assert.deepEqual(h.controls.map(control => control.value), ['dark', 'dark']); assert.equal(h.values.get(key), 'dark');
});
check('selecting System restores the current OS theme and follows future changes', () => {
  const h = harness({dark: true, stored: 'light'}); h.ready(); h.select('system'); assert.equal(h.theme(), 'dark'); assert.equal(h.preference(), 'system'); h.os(false); assert.equal(h.theme(), 'light');
  assert.equal(harness({stored: h.values.get(key) ?? null, dark: true}).theme(), 'dark');
});
check('a selection survives a reload without modifying other browser storage', () => {
  const h = harness(); h.ready(); h.values.set('unrelated', 'keep'); h.select('dark'); assert.equal(harness({stored: h.values.get(key)}).theme(), 'dark'); assert.equal(h.values.get('unrelated'), 'keep'); assert.ok(h.writes.every(([name]) => name === key));
});
check('blocked storage still allows an in-memory theme choice and System updates', () => {
  const h = harness({dark: true, blockedStorage: true}); h.ready(); assert.equal(h.theme(), 'dark'); h.select('light'); assert.equal(h.theme(), 'light'); h.select('system'); h.os(false); assert.equal(h.theme(), 'light');
});
check('a missing media-query API degrades to a usable light default', () => {
  const h = harness({noMedia: true}); h.ready(); assert.equal(h.theme(), 'light'); h.select('dark'); assert.equal(h.theme(), 'dark');
});
check('a blocked storage property does not stop first paint or manual controls', () => {
  const h = harness({dark: true, blockedStorageGetter: true}); h.ready(); assert.equal(h.theme(), 'dark'); h.select('light'); assert.equal(h.theme(), 'light'); h.select('system'); assert.equal(h.theme(), 'dark');
});
check('older Safari media listeners still follow OS changes', () => {
  const h = harness({legacyMedia: true}); h.ready(); h.os(true); assert.equal(h.theme(), 'dark');
});
check('cross-tab preference updates synchronize without writing storage again', () => {
  const h = harness({dark: true}); h.ready(); h.storage('light'); assert.equal(h.theme(), 'light'); assert.deepEqual(h.controls.map(control => control.value), ['light', 'light']); h.storage('system'); assert.equal(h.theme(), 'dark'); assert.equal(h.writes.length, 0);
});
check('clearing storage in another tab restores System while unrelated keys are ignored', () => {
  const h = harness({stored: 'dark'}); h.ready(); h.storage('light', 'unrelated'); assert.equal(h.theme(), 'dark'); h.storage(null); assert.equal(h.preference(), 'system'); assert.equal(h.theme(), 'light'); h.select('dark'); h.storage(null, null); assert.equal(h.preference(), 'system'); assert.equal(h.theme(), 'light');
});
check('an invalid control value cannot set or persist an arbitrary attribute', () => {
  const h = harness({stored: 'dark'}); h.ready(); h.select('invalid'); assert.equal(h.theme(), 'dark'); assert.equal(h.preference(), 'dark'); assert.deepEqual(h.controls.map(control => control.value), ['dark', 'dark']); assert.equal(h.writes.length, 0);
});
check('an already-ready document still wires both controls', () => {
  const h = harness({ready: true}); h.select('dark'); assert.equal(h.theme(), 'dark'); assert.equal(h.controls[1].value, 'dark');
});
check('the external bootstrap precedes CSS and native controls retain accessible labels', () => {
  const bootstrap = html.indexOf('<script src="/assets/theme.js"></script>');
  assert.ok(bootstrap > 0 && bootstrap < html.indexOf('<link rel="stylesheet"'));
  assert.equal((html.match(/data-theme-select/g) ?? []).length, 2);
  for (const id of ['auth-theme', 'workspace-theme']) {assert.match(html, new RegExp(`<label[^>]*for="${id}"`)); assert.match(html, new RegExp(`<select[^>]*id="${id}"[^>]*data-theme-select`));}
  assert.doesNotMatch(html, /<script\s+[^>]*(?:theme[^>]*\b(?:async|defer)|(?:async|defer)[^>]*theme)/);
});
check('both palettes keep text, highlights, buttons and form boundaries readable', () => {
  function luminance(hex) {
    const value = hex.length === 4 ? `#${[...hex.slice(1)].map(part => part + part).join('')}` : hex;
    const channels = [1, 3, 5].map(index => Number.parseInt(value.slice(index, index + 2), 16) / 255).map(channel => channel <= .04045 ? channel / 12.92 : ((channel + .055) / 1.055) ** 2.4);
    return channels[0] * .2126 + channels[1] * .7152 + channels[2] * .0722;
  }
  function contrast(first, second) {const a = luminance(first), b = luminance(second); return (Math.max(a, b) + .05) / (Math.min(a, b) + .05);}
  for (const selector of [':root', ':root\\[data-theme="dark"\\]']) {
    const block = css.match(new RegExp(`${selector}\\s*\\{([^}]+)\\}`))[1];
    const palette = Object.fromEntries([...block.matchAll(/--([a-z0-9-]+):\s*(#[a-f0-9]+)\s*;/g)].map(match => [match[1], match[2]]));
    const textPairs = ['paper', 'wash', 'rail', 'context', 'emerald-wash', 'selection'].flatMap(surface => [['ink', surface], ['muted', surface]]);
    textPairs.push(['emerald', 'emerald-wash'], ['notice-ink', 'notice'], ['warning', 'warning-wash'], ['danger', 'paper'], ['highlight-ink', 'highlight'], ['button-ink', 'emerald-button'], ['button-ink', 'emerald-button-hover'], ['button-ink', 'warning-button'], ['button-ink', 'danger-button'], ['button-ink', 'danger-button-hover']);
    for (const [ink, surface] of textPairs) assert.ok(contrast(palette[ink], palette[surface]) >= 4.5, `${selector} ${ink}/${surface} below text contrast`);
    for (const surface of ['paper', 'wash']) assert.ok(contrast(palette['control-line'], palette[surface]) >= 3, `${selector} control-line/${surface} below control contrast`);
    assert.ok(contrast(palette.chart, palette.paper) >= 3, `${selector} chart below graphical contrast`);
  }
});

console.log(JSON.stringify({ok: true, tests: cases.length, cases, scope: 'actual theme bootstrap with synthetic browser events; no layout or live account'}, null, 2));
