import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {execFileSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {chromium} from '../docs/.vitepress/node_modules/playwright/index.mjs';

// Real browser DOM with the shipped owner assets. Every owner API response is
// synthetic and intercepted on a loopback origin: no archive, provider, model,
// audio, installer, private configuration, token, SDK or live service is opened.
// Pass --baseline [rev] to load committed assets instead of the working tree.
const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const baselineIndex = process.argv.indexOf('--baseline');
const baselineArgument = baselineIndex === -1 ? null : process.argv[baselineIndex + 1];
const baseline = baselineIndex === -1 ? null : (baselineArgument && !baselineArgument.startsWith('--') ? baselineArgument : 'HEAD');

function asset(name) {
  if (!baseline) return fs.readFileSync(path.join(repoRoot, 'internal/localui/assets', name));
  return execFileSync('git', ['show', `${baseline}:internal/localui/assets/${name}`], {cwd: repoRoot, maxBuffer: 32 * 1024 * 1024});
}

const ending = '  start();\n})();';
const appSource = asset('app.js').toString('utf8');
assert.equal(appSource.split(ending).length, 2, 'exact startup seam');
// Test-only exposure at the exact startup seam; the shipped assets stay unchanged.
const exposed = `  window.__transcriptionBrowser = {
    openTranscriptionSetup: typeof openTranscriptionSetup === "function" ? openTranscriptionSetup : null,
    refreshTranscriptionStatus: typeof refreshTranscriptionStatus === "function" ? refreshTranscriptionStatus : null,
    copyTranscriptionCommand: typeof copyTranscriptionCommand === "function" ? copyTranscriptionCommand : null,
    configureTranscriptionSetup: typeof configureTranscriptionSetup === "function" ? configureTranscriptionSetup : null,
    refreshStatus: typeof refreshStatus === "function" ? refreshStatus : null,
  };
  start();
})();`;
const script = appSource.replace(ending, exposed);
const indexHtml = asset('index.html').toString('utf8');
const assets = new Map([
  ['/assets/app.js', ['text/javascript; charset=utf-8', script]],
  ['/assets/app.css', ['text/css; charset=utf-8', asset('app.css')]],
  ['/assets/theme.js', ['text/javascript; charset=utf-8', asset('theme.js')]],
  ['/assets/selects.js', ['text/javascript; charset=utf-8', asset('selects.js')]],
  ['/assets/logo.png', ['image/png', asset('logo.png')]],
]);

const origin = 'http://127.0.0.1:39876';
const CSRF = 's'.repeat(43);
const STATUS_PATH = '/api/transcription/status';
const FIRST_RUN_POLICY = {Generation: 0, DateMode: 'since', ImportStart: '0001-01-01T00:00:00Z', ChatMode: 'selective', IncludedChats: [], RollingRetention: 0};
const DESKTOP = {width: 1280, height: 900};
const MOBILE = {width: 390, height: 844};
const stamp = new Date().toISOString().replace(/[:.]/g, '-');
const screenshotDir = process.env.HEBREW_SETUP_SCREENSHOTS || path.join(fs.realpathSync(os.tmpdir()), 'opencode', 'hebrew-setup-browser-' + stamp);
fs.mkdirSync(screenshotDir, {recursive: true});
const screenshots = [];

function makeWorld(overrides = {}) {
  return {
    realm: 'local', demo: false, historical: null, configured: false,
    policy: structuredClone(FIRST_RUN_POLICY),
    transcription: {configured: false, state: 'not_configured', hebrew_ready: false},
    holdTranscription: false, heldTranscription: [],
    unauthorized: false, requests: [], policyWrites: 0, pairingStarts: 0, transcriptionHttpStatus: 200,
    ...overrides,
  };
}
const statusRequests = world => world.requests.filter(request => request.path === STATUS_PATH);
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(predicate, timeout, label) {
  const started = Date.now();
  while (!predicate()) {
    if (Date.now() - started > timeout) throw new Error('Timed out waiting for ' + label);
    await sleep(25);
  }
}
function statsFor(messages) {
  const sent = Math.min(40, messages), received = messages - sent;
  return {
    sampled_at: 1791709200, archive_revision: 1, messages, chats: messages ? 1 : 0, sent, received,
    first_message_at: messages ? 1791705600 : 0, last_message_at: messages ? 1791709200 : 0,
    activity_interval: 'day', activity_step: 1,
    daily: messages ? [{date: '2026-10-11', messages}] : [],
    top_chats: messages ? [{id: 'synthetic-chat', name: 'Synthetic conversation', messages}] : [],
    kinds: messages ? [{kind: 'text', messages}] : [],
    media: {voice_messages: 0, retained_voice: 0, transcribed_voice: 0},
    insights: {active_days: messages ? 1 : 0, longest_streak_days: messages ? 1 : 0, busiest_date: messages ? '2026-10-11' : '', busiest_date_messages: messages},
    rhythm: messages ? [{weekday: 0, hour: 8, messages}] : [],
    breakdown: {dimension: 'chats', page: 1, page_size: 10, total: messages ? 1 : 0, sort: 'messages', rows: messages ? [{id: 'synthetic-chat', name: 'Synthetic conversation', messages, sent, received, chats: 1, last_message_at: 1791709200}] : []},
  };
}
function statusFor(world) {
  if (!world.configured) return {Connection: 'disconnected', Paused: false, Messages: 0, Codes: [], Keyword: {State: 'not_configured', Complete: 0, Pending: 0, Failed: 0}, Media: {State: 'not_configured'}, Semantic: {State: 'not_configured'}, Snapshot: {PolicyGeneration: world.policy.Generation, ArchiveRevision: 0, RecoveryEpoch: 0}};
  return {Connection: 'connected', Paused: false, Messages: 219, Keyword: {State: 'ready', Complete: 219, Pending: 0, Failed: 0}, Media: {State: 'not_configured'}, Semantic: {State: 'not_configured'}, Codes: [], Snapshot: {PolicyGeneration: world.policy.Generation, ArchiveRevision: 1, RecoveryEpoch: 0}};
}

async function serve(route, world, html) {
  const request = route.request();
  const url = new URL(request.url());
  if (url.origin !== origin) return route.abort();
  if (url.pathname === '/') return route.fulfill({status: 200, contentType: 'text/html; charset=utf-8', body: html});
  const known = assets.get(url.pathname);
  if (known) return route.fulfill({status: 200, contentType: known[0], body: known[1]});
  const method = request.method();
  const body = request.postData();
  world.requests.push({method, path: url.pathname, query: url.search, body, csrf: request.headers()['x-csrf-token'] || ''});
  const respond = (value, status = 200) => route.fulfill({status, contentType: 'application/json', body: JSON.stringify(value)});
  switch (url.pathname) {
    case '/api/interface': return respond({realm: world.realm});
    case '/api/session': return respond({csrf_token: CSRF});
    case '/api/setup': return respond({configured: world.configured, demo: world.demo, historical_snapshot: world.historical ?? undefined, policy: world.policy});
    case '/api/status':
      if (world.unauthorized) return respond({code: 'unauthorized'}, 401);
      return respond(statusFor(world));
    case '/api/pairing': return respond({state: world.configured ? 'disconnected' : 'setup_required', has_session: false});
    case '/api/pairing/start':
      if (method !== 'POST') return respond({code: 'method_not_allowed'}, 405);
      world.pairingStarts++;
      return respond({state: 'pairing', has_session: false});
    case STATUS_PATH:
      if (method !== 'GET' || url.search) return respond({code: 'invalid_request'}, 400);
      if (world.holdTranscription) { world.heldTranscription.push(route); return undefined; }
      return respond(world.transcription, world.transcriptionHttpStatus ?? 200);
    case '/api/policy': {
      if (method !== 'PUT') return respond({code: 'method_not_allowed'}, 405);
      const requested = JSON.parse(body || '{}');
      world.policyWrites++;
      world.policy = {...world.policy, Generation: world.policy.Generation + 1, DateMode: requested.date_mode === 'all_available' ? 'all_available' : 'since', ChatMode: requested.chat_mode, IncludedChats: requested.included_chats || [], RollingRetention: requested.rolling_retention_days || 0};
      world.configured = true;
      return respond(world.policy);
    }
    case '/api/stats': return respond(statsFor(world.configured ? 219 : 0));
    case '/api/chats': return respond({Chats: []});
    case '/api/archive/browse':
    case '/api/search':
      return respond({Hits: [], NextCursor: '', Snapshot: {PolicyGeneration: world.policy.Generation, RecoveryEpoch: 0}, saved_view: {source: 'saved', coverage: 'partial', current_phone_state: 'unknown'}});
    default: return respond({code: 'not_found'}, 404);
  }
}

async function open(browser, {world = makeWorld(), viewport = DESKTOP, theme = 'light', motion = 'no-preference', clipboard = 'allow', remoteAdmin = false} = {}) {
  const context = await browser.newContext({viewport, colorScheme: theme, reducedMotion: motion, serviceWorkers: 'block', locale: 'en-US'});
  await context.addInitScript(mode => {
    // Clipboard behaviour is simulated inside this browser context only.
    window.__clipboardWrites = [];
    const writeText = mode === 'deny'
      ? () => Promise.reject(new DOMException('Synthetic clipboard denial', 'NotAllowedError'))
      : text => { window.__clipboardWrites.push(String(text)); return Promise.resolve(); };
    Object.defineProperty(navigator, 'clipboard', {configurable: true, value: {writeText}});
  }, clipboard);
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const html = remoteAdmin ? indexHtml.replace('<html lang="en">', '<html lang="en" data-remote-admin="true">') : indexHtml;
  if (remoteAdmin) assert.notEqual(html, indexHtml, 'the remote administration marker is applied to the served page');
  await context.route('**/*', route => serve(route, world, html));
  await page.goto(origin);
  return {context, page, world, errors};
}

async function waitStartup(f, {remoteAdmin = false} = {}) {
  const {page, world} = f;
  if (remoteAdmin || world.realm !== 'local') return page.waitForFunction(() => document.getElementById('auth-screen')?.hidden === false, null, {timeout: 8000});
  if (!world.configured) return page.waitForFunction(() => document.getElementById('settings-dialog')?.open === true, null, {timeout: 8000});
  return page.waitForFunction(() => document.getElementById('workspace')?.hidden === false && document.getElementById('stats-message-count')?.textContent === '219', null, {timeout: 8000});
}

const choiceTrigger = page => page.getByRole('combobox', {name: /Set up voice transcription/});
async function chooseHebrewByKeyboard(page) {
  const trigger = choiceTrigger(page);
  await trigger.focus();
  await page.keyboard.press('ArrowDown');
  const option = page.getByRole('option', {name: /Hebrew transcription/});
  await option.waitFor();
  assert.equal(await trigger.getAttribute('aria-expanded'), 'true');
  assert.equal(await trigger.getAttribute('aria-activedescendant'), await option.getAttribute('id'), 'the highlighted option is announced to the combobox');
  await page.keyboard.press('Enter');
}
async function chooseHebrewByPointer(page) {
  await choiceTrigger(page).click();
  await page.getByRole('option', {name: /Hebrew transcription/}).click();
}
async function openGuideFromChoice(f) {
  const before = statusRequests(f.world).length;
  await chooseHebrewByKeyboard(f.page);
  await until(() => statusRequests(f.world).length > before, 5000, 'the guide status check');
  await f.page.waitForFunction(() => document.getElementById('transcription-setup-dialog')?.open === true);
}
async function openGuideFromModels(f) {
  const before = statusRequests(f.world).length;
  await f.page.locator('#models-open').click();
  await f.page.locator('#models-transcription-open').click();
  await until(() => statusRequests(f.world).length > before, 5000, 'the models guide status check');
  await f.page.waitForFunction(() => document.getElementById('transcription-setup-dialog')?.open === true);
}
async function waitCheckSettled(page) {
  await page.waitForFunction(() => document.getElementById('transcription-check').disabled === false, null, {timeout: 9000});
}
// The guide's close event resets the choice and clears its readout asynchronously.
const waitChoiceReset = page => page.waitForFunction(() => document.getElementById('setup-voice-choice').value === 'later' && document.getElementById('transcription-status').textContent === '', null, {timeout: 3000});
async function runCheck(f, payload) {
  const before = statusRequests(f.world).length;
  f.world.transcription = payload;
  await f.page.locator('#transcription-check').click();
  await until(() => statusRequests(f.world).length === before + 1, 5000, 'a repeated status check');
  await waitCheckSettled(f.page);
}
const statusText = page => page.locator('#transcription-status').textContent();
const guideOpen = page => page.evaluate(() => document.getElementById('transcription-setup-dialog').open);
const settingsOpen = page => page.evaluate(() => document.getElementById('settings-dialog').open);
const value = (page, id) => page.locator('#' + id).inputValue();
const escapeRegExp = text => text.replace(/[$.*+?^(){}|[\]\\]/g, '\\$&');

const cases = [];
const failures = [];
const browser = await chromium.launch();
async function check(name, body) {
  const fixtures = [];
  const use = async options => {
    const f = await open(browser, options);
    fixtures.push(f);
    return f;
  };
  try {
    await body(use);
    for (const f of fixtures) assert.deepEqual(f.errors, [], 'no uncaught page errors');
    cases.push(name);
  } catch (error) {
    failures.push({case: name, error: String(error.message).split('\n').slice(0, 80).join(' ').slice(0, 6000)});
  } finally {
    for (const f of fixtures) await f.context.close().catch(() => {});
  }
}

try {
  await check('first run opens Settings with the Hebrew choice, makes no transcription request at startup and exposes it as a named combobox', async use => {
    const f = await use({world: makeWorld()});
    await waitStartup(f);
    assert.equal(await settingsOpen(f.page), true, 'an unconfigured archive opens its first-run Settings');
    assert.equal(await f.page.locator('#setup-transcription').evaluate(node => node.hidden), false);
    assert.equal(await f.page.locator('#models-transcription').evaluate(node => node.hidden), false, 'the Models entry stays reachable');
    assert.match(await choiceTrigger(f.page).getAttribute('aria-label'), /Set up voice transcription: Not now/);
    assert.equal(statusRequests(f.world).length, 0, 'startup never checks or starts the native service');
    assert.equal(await value(f.page, 'setup-voice-choice'), 'later');
    const shot = path.join(screenshotDir, 'first-run-desktop-light.png');
    await f.page.screenshot({path: shot});
    screenshots.push(path.basename(shot));
  });

  await check('the styled Hebrew choice selects by keyboard, announces its highlighted option and makes one guide check with the owner CSRF header', async use => {
    const f = await use({world: makeWorld()});
    await waitStartup(f);
    await openGuideFromChoice(f);
    assert.equal(await value(f.page, 'setup-voice-choice'), 'hebrew');
    assert.equal(statusRequests(f.world).length, 1, 'opening the guide performs exactly one status check');
    const request = statusRequests(f.world)[0];
    assert.equal(request.method, 'GET');
    assert.equal(request.query, '', 'the status route takes no query');
    assert.equal(request.body, null, 'the status route sends no body');
    assert.equal(request.csrf, CSRF, 'the native owner CSRF header is sent');
    await waitCheckSettled(f.page);
    assert.match(await statusText(f.page), /not configured/i, 'a not-configured archive says so');
    assert.equal(await choiceTrigger(f.page).getAttribute('aria-expanded'), 'false', 'committing closes the listbox');
  });

  await check('the styled Hebrew choice also selects with the pointer on its listbox', async use => {
    const f = await use({world: makeWorld()});
    await waitStartup(f);
    await chooseHebrewByPointer(f.page);
    await until(() => statusRequests(f.world).length >= 1, 5000, 'the pointer guide status check');
    await f.page.waitForFunction(() => document.getElementById('transcription-setup-dialog')?.open === true);
    assert.equal(await value(f.page, 'setup-voice-choice'), 'hebrew');
  });

  await check('the guide has three step details with labelled read-only commands and opens only the install step first', async use => {
    const f = await use({world: makeWorld()});
    await waitStartup(f);
    await openGuideFromChoice(f);
    const steps = f.page.locator('#transcription-setup-dialog ol.transcription-steps > li > details');
    assert.equal(await steps.count(), 3);
    assert.deepEqual(await steps.evaluateAll(nodes => nodes.map(node => node.open)), [true, false, false]);
    for (const label of ['Run in a terminal', 'Run in the separate terminal', 'Add to your existing serve command']) {
      await f.page.getByLabel(label).waitFor({state: 'attached'});
    }
    assert.equal(await f.page.locator('#transcription-setup-dialog ol.transcription-steps textarea[readonly]').count(), 4, 'every command is read-only text');
  });

  await check('install, service, launch and default commands share the Hebrew profile, language he and one env file, portably from a source checkout', async use => {
    const f = await use({world: makeWorld()});
    await waitStartup(f);
    await openGuideFromChoice(f);
    const install = await value(f.page, 'transcription-install-command');
    const service = await value(f.page, 'transcription-service-command');
    const launch = await value(f.page, 'transcription-launch-argument');
    const fallback = await value(f.page, 'transcription-default-command');
    const dir = '$HOME/.local/share/my-whatsapp-mcp/transcription-hebrew';
    const env = dir + '/transcription.env';
    assert.match(install, /python3\.11 services\/transcription\/setup\.py/, 'the installer runs from the source checkout');
    assert.match(install, /--python python3\.11/);
    assert.match(install, /--profile\s+hebrew\b/);
    assert.match(install, /--language\s+he\b/);
    assert.match(install, /--data-dir "\$TRANSCRIPTION_DIR"/);
    assert.match(install, new RegExp('TRANSCRIPTION_DIR="' + escapeRegExp(dir) + '"'));
    assert.match(service, /"\$TRANSCRIPTION_DIR\/venv\/bin\/python" services\/transcription\/server\.py\s*\\?\s*--env-file "\$TRANSCRIPTION_DIR\/transcription\.env"/, 'the service uses the venv beside the shared env');
    assert.match(service, new RegExp('TRANSCRIPTION_DIR="' + escapeRegExp(dir) + '"'));
    assert.equal(launch, '--transcription-env-file "' + env + '"', 'the launch argument names the same shared env file');
    assert.match(fallback, /\.\/bin\/wa-indexer serve\s*\\?\s*--transcription-env-file "\$HOME\/\.local\/share\/my-whatsapp-mcp\/transcription-hebrew\/transcription\.env"/);
    for (const text of [install, service, launch, fallback]) {
      assert.doesNotMatch(text, /\/Users\/|\/home\/|\/private\/|\/var\/|[A-Za-z]:[\\/]/, 'no machine-specific absolute path');
      assert.doesNotMatch(text, /sudo|curl |wget |\beval\b|TOKEN|PASSWORD|rm -rf/i, 'no privileged, remote or secret-bearing command');
    }
    assert.doesNotMatch(launch + fallback, /--data-dir/, 'launch guidance never redirects an existing archive');
  });

  await check('Next buttons move focus to the next step summary by pointer and by keyboard', async use => {
    const f = await use({world: makeWorld()});
    await waitStartup(f);
    await openGuideFromChoice(f);
    await f.page.locator('#transcription-next-service').click();
    assert.equal(await f.page.locator('#transcription-service-step').evaluate(node => node.open), true);
    assert.equal(await f.page.evaluate(() => document.activeElement === document.querySelector('#transcription-service-step > summary')), true, 'pointer Next focuses the service summary');
    await f.page.locator('#transcription-next-archive').focus();
    await f.page.keyboard.press('Enter');
    assert.equal(await f.page.locator('#transcription-archive-step').evaluate(node => node.open), true);
    assert.equal(await f.page.evaluate(() => document.activeElement === document.querySelector('#transcription-archive-step > summary')), true, 'keyboard Next focuses the archive summary');
  });

  await check('copy uses the in-context clipboard when allowed and falls back to keyboard selection when denied', async use => {
    const allowed = await use({world: makeWorld(), clipboard: 'allow'});
    await waitStartup(allowed);
    await openGuideFromChoice(allowed);
    const before = allowed.world.requests.length;
    await allowed.page.locator('#transcription-copy-install').click();
    await allowed.page.waitForFunction(() => document.getElementById('transcription-feedback').textContent.startsWith('Copied.'));
    const writes = await allowed.page.evaluate(() => window.__clipboardWrites);
    assert.equal(writes.length, 1);
    assert.equal(writes[0], await value(allowed.page, 'transcription-install-command'));
    assert.equal(allowed.world.requests.length, before, 'copying never sends a request');
    const denied = await use({world: makeWorld(), clipboard: 'deny'});
    await waitStartup(denied);
    await openGuideFromChoice(denied);
    await denied.page.locator('#transcription-copy-install').click();
    await denied.page.waitForFunction(() => document.getElementById('transcription-feedback').textContent.includes('keyboard'));
    const selection = await denied.page.evaluate(() => {
      const node = document.getElementById('transcription-install-command');
      return {focused: document.activeElement === node, whole: node.selectionStart === 0 && node.selectionEnd === node.value.length};
    });
    assert.deepEqual(selection, {focused: true, whole: true});
  });

  await check('Continue without voice setup closes the guide, returns to the first-run choices and makes no install, pairing or policy request after steps were clicked', async use => {
    const f = await use({world: makeWorld(), clipboard: 'allow'});
    await waitStartup(f);
    await openGuideFromChoice(f);
    await f.page.locator('#transcription-next-service').click();
    await f.page.locator('#transcription-next-archive').click();
    await f.page.locator('#transcription-copy-argument').click();
    await f.page.locator('#transcription-continue').click();
    await f.page.waitForFunction(() => document.getElementById('transcription-setup-dialog').open === false);
    await waitChoiceReset(f.page);
    assert.equal(await settingsOpen(f.page), true, 'the first-run choices remain on screen');
    assert.equal(await value(f.page, 'setup-voice-choice'), 'later', 'closing the guide returns the choice to Not now');
    assert.equal(await statusText(f.page), '');
    assert.equal(await f.page.locator('#transcription-feedback').textContent(), '');
    assert.equal(f.world.policyWrites, 0, 'no archive policy was saved');
    assert.equal(f.world.pairingStarts, 0, 'no pairing was started');
    // Stats reads are POST filters by design; every other write-like request is a failure here.
    const readPosts = ['/api/stats', '/api/search', '/api/archive/browse', '/api/context'];
    assert.deepEqual(f.world.requests.filter(request => request.method !== 'GET' && !readPosts.includes(request.path)), [], 'no install, pairing or policy request was sent');
    assert.equal(statusRequests(f.world).length, 1, 'only the opening check was made');
    assert.doesNotMatch(await f.page.locator('#setup-transcription').textContent(), /installed|paired|Hebrew transcription connected/i, 'no install or readiness claim');
  });

  await check('closing the guide with its close button or Escape returns to the first-run choices without a readiness claim', async use => {
    const byButton = await use({world: makeWorld()});
    await waitStartup(byButton);
    await openGuideFromChoice(byButton);
    await byButton.page.locator('#transcription-setup-close').click();
    await byButton.page.waitForFunction(() => document.getElementById('transcription-setup-dialog').open === false);
    await waitChoiceReset(byButton.page);
    assert.equal(await value(byButton.page, 'setup-voice-choice'), 'later');
    const byEscape = await use({world: makeWorld()});
    await waitStartup(byEscape);
    await openGuideFromChoice(byEscape);
    await byEscape.page.keyboard.press('Escape');
    await byEscape.page.waitForFunction(() => document.getElementById('transcription-setup-dialog').open === false);
    await waitChoiceReset(byEscape.page);
    assert.equal(await value(byEscape.page, 'setup-voice-choice'), 'later');
    assert.equal(await settingsOpen(byEscape.page), true);
    assert.equal(byEscape.world.policyWrites, 0);
  });

  await check('choosing Not now cannot block saving the archive policy or the first-run pairing', async use => {
    const f = await use({world: makeWorld()});
    await waitStartup(f);
    await openGuideFromChoice(f);
    await f.page.locator('#transcription-continue').click();
    await f.page.waitForFunction(() => document.getElementById('transcription-setup-dialog').open === false);
    await f.page.locator('#save-policy').click();
    await until(() => f.world.policyWrites === 1, 5000, 'the archive policy save');
    await f.page.waitForFunction(() => document.getElementById('connection-dialog')?.open === true);
    await until(() => f.world.pairingStarts === 1, 5000, 'the first-run pairing start');
    assert.equal(f.world.policy.Generation, 1, 'the saved policy is the one the archive now uses');
    assert.equal(statusRequests(f.world).length, 1, 'saving never claims the voice service is installed');
  });

  for (const [name, state, hebrewReady, copy, isError] of [
    ['connecting', 'connecting', false, /configured and connecting/i, false],
    ['ready', 'ready', true, /Hebrew transcription connected\./, false],
    ['busy after verification', 'busy', true, /previously verified · service busy now/, false],
    ['busy before verification', 'busy', false, /reports busy\. Hebrew setup is not yet verified/, false],
    ['unavailable', 'unavailable', false, /configured voice service is unavailable/i, false],
    ['mismatch', 'mismatch', false, /does not match this Hebrew setup/i, true],
    ['stopped', 'stopped', false, /voice worker is stopped/i, false],
  ]) {
    await check('a configured archive reports ' + name + ' honestly and claims Hebrew only when verified', async use => {
      const f = await use({world: makeWorld({configured: true, transcription: {configured: true, state, hebrew_ready: hebrewReady}})});
      await waitStartup(f);
      await openGuideFromModels(f);
      await waitCheckSettled(f.page);
      const text = await statusText(f.page);
      assert.match(text, copy);
      if (hebrewReady) assert.match(text, /verified/i, 'a verified claim is explicit');
      else assert.doesNotMatch(text, /(?<!not yet )verified|connected/i, 'an unverified state never claims verification');
      assert.equal(await f.page.locator('#transcription-status').evaluate(node => node.classList.contains('error')), isError);
      assert.equal(await f.page.locator('#transcription-check').textContent(), 'Check running app');
    });
  }

  await check('HTTP refusals never claim Hebrew: a busy admission asks for a later check and an unavailable status says it could not be verified', async use => {
    const f = await use({world: makeWorld({configured: true})});
    await waitStartup(f);
    await openGuideFromModels(f);
    f.world.transcriptionHttpStatus = 429;
    await runCheck(f, {code: 'transcription_status_busy'});
    assert.match(await statusText(f.page), /Another status check is finishing or just ran/i);
    assert.equal(await f.page.locator('#transcription-status').evaluate(node => node.classList.contains('error')), false);
    f.world.transcriptionHttpStatus = 503;
    await runCheck(f, {code: 'transcription_status_unavailable'});
    assert.match(await statusText(f.page), /could not be verified/i);
    assert.equal(await f.page.locator('#transcription-status').evaluate(node => node.classList.contains('error')), true);
    assert.doesNotMatch(await statusText(f.page), /connected/i);
  });

  await check('malformed or contradictory readiness never claims Hebrew is connected and fails closed', async use => {
    const f = await use({world: makeWorld({configured: true})});
    await waitStartup(f);
    await openGuideFromModels(f);
    const invalid = [{}, {configured: true, state: 'future_state', hebrew_ready: true}, {configured: false, state: 'ready', hebrew_ready: true}, {configured: true, state: 'ready', hebrew_ready: false}, {configured: true, state: 'unavailable', hebrew_ready: true}, {configured: true, state: 'ready', hebrew_ready: 'true'}, {configured: true, state: 'ready'}];
    for (const payload of invalid) {
      await runCheck(f, payload);
      const text = await statusText(f.page);
      assert.match(text, /could not be verified/i, JSON.stringify(payload));
      assert.doesNotMatch(text, /connected/i, JSON.stringify(payload));
    }
  });

  await check('a status pair that only a configured app can have is not accepted when the app is unconfigured', async use => {
    const f = await use({world: makeWorld({configured: true})});
    await waitStartup(f);
    await openGuideFromModels(f);
    for (const payload of [{configured: false, state: 'connecting', hebrew_ready: false}, {configured: false, state: 'unavailable', hebrew_ready: false}, {configured: false, state: 'mismatch', hebrew_ready: false}]) {
      await runCheck(f, payload);
      assert.match(await statusText(f.page), /could not be verified/i, 'the backend never produces this pair, so the UI must not describe it: ' + JSON.stringify(payload));
    }
  });

  await check('a configured archive whose service is absent never claims ready, and a page revisit keeps the archive policy unchanged', async use => {
    const world = makeWorld({configured: true, transcription: {configured: true, state: 'unavailable', hebrew_ready: false}});
    const f = await use({world});
    await waitStartup(f);
    const policyAtStart = structuredClone(world.policy);
    await openGuideFromModels(f);
    await waitCheckSettled(f.page);
    assert.doesNotMatch(await statusText(f.page), /connected/i);
    world.transcription = {configured: true, state: 'ready', hebrew_ready: true};
    await f.page.reload();
    await waitStartup(f);
    await openGuideFromModels(f);
    await waitCheckSettled(f.page);
    assert.match(await statusText(f.page), /Hebrew transcription connected\./, 'a verified ready worker is shown after revisit');
    assert.equal(await f.page.locator('#setup-transcription').evaluate(node => node.hidden), true, 'a configured archive does not repeat first-run choices');
    assert.deepEqual(world.policy, policyAtStart, 'voice checks leave the archive policy unchanged');
    assert.equal(world.policyWrites, 0);
  });

  await check('status checks coalesce while one is pending and the check button stays disabled', async use => {
    const world = makeWorld({configured: true, holdTranscription: true});
    const f = await use({world});
    await waitStartup(f);
    await openGuideFromModels(f);
    assert.equal(await f.page.locator('#transcription-check').isDisabled(), true);
    await f.page.evaluate(() => document.getElementById('transcription-check').click());
    await sleep(300);
    assert.equal(statusRequests(world).length, 1, 'a click while checking starts no second request');
    world.holdTranscription = false;
    for (const route of world.heldTranscription.splice(0)) await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify({configured: true, state: 'ready', hebrew_ready: true})});
    await waitCheckSettled(f.page);
    assert.match(await statusText(f.page), /Hebrew transcription connected\./);
  });

  await check('a status check is bounded to five seconds and a late success after the timeout is ignored', async use => {
    const world = makeWorld({configured: true, holdTranscription: true});
    const f = await use({world});
    await waitStartup(f);
    await openGuideFromModels(f);
    await waitCheckSettled(f.page);
    assert.match(await statusText(f.page), /could not be verified/i, 'the timed-out check says it could not be verified');
    for (const route of world.heldTranscription.splice(0)) await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify({configured: true, state: 'ready', hebrew_ready: true})}).catch(() => {});
    await sleep(200);
    assert.doesNotMatch(await statusText(f.page), /connected/i, 'a late success cannot restore a claim');
  });

  await check('closing the guide fences a late ready response and reopening issues one fresh check', async use => {
    const world = makeWorld({configured: true, holdTranscription: true});
    const f = await use({world});
    await waitStartup(f);
    await openGuideFromModels(f);
    await f.page.locator('#transcription-setup-close').click();
    await f.page.waitForFunction(() => document.getElementById('transcription-setup-dialog').open === false);
    world.holdTranscription = false;
    for (const route of world.heldTranscription.splice(0)) await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify({configured: true, state: 'ready', hebrew_ready: true})}).catch(() => {});
    world.transcription = {configured: true, state: 'stopped', hebrew_ready: false};
    await f.page.locator('#models-transcription-open').click();
    await until(() => statusRequests(world).length === 2, 5000, 'the reopened check');
    await waitCheckSettled(f.page);
    assert.match(await statusText(f.page), /voice worker is stopped/i);
    assert.doesNotMatch(await statusText(f.page), /connected/i, 'the closed guide late response never restores readiness');
  });

  await check('owner expiry while a check is pending removes readiness, closes the guide and stops later checks', async use => {
    const world = makeWorld({configured: true, holdTranscription: true});
    const f = await use({world});
    await waitStartup(f);
    await openGuideFromModels(f);
    world.unauthorized = true;
    await f.page.evaluate(() => window.__transcriptionBrowser.refreshStatus());
    await f.page.waitForFunction(() => document.getElementById('workspace').hidden === true, null, {timeout: 6000});
    for (const route of world.heldTranscription.splice(0)) await route.fulfill({status: 200, contentType: 'application/json', body: JSON.stringify({configured: true, state: 'ready', hebrew_ready: true})}).catch(() => {});
    await sleep(200);
    assert.equal(await f.page.locator('#transcription-status').textContent(), '');
    assert.equal(await guideOpen(f.page), false);
    const before = statusRequests(world).length;
    await f.page.evaluate(() => window.__transcriptionBrowser.openTranscriptionSetup());
    await sleep(200);
    assert.equal(statusRequests(world).length, before, 'an expired owner session never starts a voice check');
  });

  for (const [label, world, remoteAdmin] of [
    ['demo', makeWorld({demo: true}), false],
    ['historical snapshot', makeWorld({historical: {captured_at: '2026-01-02T03:04:05Z'}}), false],
    ['remote realm', makeWorld({realm: 'remote'}), false],
    ['remote administration', makeWorld({configured: true}), true],
  ]) {
    await check(label + ' views never render or call the native transcription status', async use => {
      const f = await use({world, remoteAdmin});
      await waitStartup(f, {remoteAdmin});
      await f.page.evaluate(() => { window.__transcriptionBrowser.configureTranscriptionSetup?.(); return window.__transcriptionBrowser.openTranscriptionSetup?.(); });
      await sleep(200);
      assert.equal(statusRequests(world).length, 0, label + ' sent no native status request');
      assert.equal(await f.page.locator('#setup-transcription').evaluate(node => node.hidden), true);
      assert.equal(await f.page.locator('#models-transcription').evaluate(node => node.hidden), true);
    });
  }

  // Visible targets smaller than 44px in either dimension, with their measured size.
  const targets = (page, selector) => page.evaluate(sel => [...document.querySelectorAll(sel)]
    .map(node => ({label: node.id || node.getAttribute('aria-label') || node.textContent.trim().slice(0, 40), box: node.getBoundingClientRect()}))
    .filter(item => item.box.width > 0 && item.box.height > 0 && (item.box.height < 44 || item.box.width < 44))
    .map(item => ({label: item.label, width: Math.round(item.box.width), height: Math.round(item.box.height)})), selector);
  for (const [theme, motion, viewport, label] of [['light', 'no-preference', DESKTOP, 'desktop-light-1280x900'], ['dark', 'reduce', MOBILE, 'mobile-dark-reduced-390x844']]) {
    await check('the Hebrew setup controls keep 44px targets, no overflow and keyboard Details at ' + label, async use => {
      const f = await use({world: makeWorld(), viewport, theme, motion});
      await waitStartup(f);
      await choiceTrigger(f.page).waitFor({state: 'visible'});
      await f.page.locator('#setup-transcription-open').waitFor({state: 'visible'});
      const entry = await targets(f.page, '#setup-transcription-open, #setup-transcription .select-trigger');
      await openGuideFromChoice(f);
      assert.equal(await f.page.evaluate(() => window.matchMedia('(prefers-reduced-motion: reduce)').matches), motion === 'reduce');
      assert.equal(await f.page.evaluate(() => window.matchMedia('(prefers-color-scheme: dark)').matches), theme === 'dark');
      assert.equal(await f.page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, 'no page-level horizontal overflow');
      assert.equal(await f.page.evaluate(() => { const d = document.getElementById('transcription-setup-dialog'); return d.scrollWidth <= d.clientWidth + 1; }), true, 'the guide does not overflow sideways');
      await f.page.locator('#transcription-service-step > summary').focus();
      await f.page.keyboard.press('Enter');
      assert.equal(await f.page.locator('#transcription-service-step').evaluate(node => node.open), true, 'Enter opens a step summary');
      await f.page.keyboard.press(' ');
      assert.equal(await f.page.locator('#transcription-service-step').evaluate(node => node.open), false, 'Space closes it again');
      const shot = path.join(screenshotDir, 'guide-' + label + '.png');
      await f.page.screenshot({path: shot});
      screenshots.push(path.basename(shot));
      await f.page.locator('#transcription-archive-step > summary').focus();
      await f.page.keyboard.press('Enter');
      await f.page.locator('#transcription-setup-dialog').evaluate(node => { node.scrollTop = node.scrollHeight; });
      const scrolled = path.join(screenshotDir, 'guide-' + label + '-archive-step.png');
      await f.page.screenshot({path: scrolled});
      screenshots.push(path.basename(scrolled));
      const guide = await targets(f.page, '#transcription-setup-dialog button, #transcription-setup-dialog summary');
      assert.deepEqual([...entry, ...guide], [], 'every Hebrew setup control must be at least 44px in both dimensions');
    });
  }

  for (const [theme, motion, viewport, label] of [['light', 'no-preference', DESKTOP, 'desktop-light-1280x900'], ['dark', 'reduce', MOBILE, 'mobile-dark-reduced-390x844']]) {
    await check('all commands are fully readable without inner scrolling at ' + label, async use => {
      const f = await use({world: makeWorld(), viewport, theme, motion});
      await waitStartup(f);
      await openGuideFromChoice(f);
      await f.page.locator('#transcription-setup-dialog').evaluate(node => { for (const step of node.querySelectorAll('details')) step.open = true; });
      await f.page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      const clipped = await f.page.locator('#transcription-setup-dialog textarea').evaluateAll(nodes => nodes.filter(node => node.scrollHeight > node.clientHeight + 1).map(node => ({id:node.id,scrollHeight:node.scrollHeight,clientHeight:node.clientHeight})));
      assert.deepEqual(clipped, [], 'the complete command, including language and env path, must be visible before copying');
    });
    await check('the Hebrew Models entry point keeps a 44px target at ' + label, async use => {
      const f = await use({world: makeWorld({configured: true}), viewport, theme, motion});
      await waitStartup(f);
      await f.page.locator('#models-open').click();
      await f.page.locator('#models-transcription-open').waitFor({state: 'visible'});
      assert.deepEqual(await targets(f.page, '#models-transcription-open'), [], 'the Models entry point must be at least 44px in both dimensions');
    });
  }
} finally {
  await browser.close();
}

console.log(JSON.stringify({suite: 'Hebrew transcription setup browser', baseline, origin, screenshotDir, screenshots, passed: cases, failures}, null, 2));
if (failures.length) process.exitCode = 1;
