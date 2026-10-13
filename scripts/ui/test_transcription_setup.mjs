import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import crypto from 'node:crypto';

// Shipped-script checks with deferred, synthetic owner API responses. No model,
// archive, installer, private configuration or live service is opened here.
const source = fs.readFileSync(new URL('../../internal/localui/assets/app.js', import.meta.url), 'utf8');
const html = fs.readFileSync(new URL('../../internal/localui/assets/index.html', import.meta.url), 'utf8');
const ending = '  start();\n})();';
assert.equal(source.split(ending).length, 2);
assert.match(html, /id="setup-transcription"/, 'first run must offer optional Hebrew voice setup');
assert.match(html, /id="models-transcription"/, 'unfinished setup must remain reachable after restart');

class Node {
  constructor() { this.value = ''; this.textContent = ''; this.hidden = false; this.disabled = false; this.open = false; this.children = []; this.listeners = new Map(); this.dataset = {}; this.attributes = {}; this.classList = {toggle(){},contains(){return false;},add(){},remove(){}}; }
  addEventListener(name, fn) { this.listeners.set(name, [...this.listeners.get(name) || [], fn]); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  removeAttribute(name) { delete this.attributes[name]; }
  replaceChildren(...nodes) { this.children = nodes; this.textContent = ''; }
  append(...nodes) { this.children.push(...nodes); }
  querySelectorAll() { return []; }
  get options() { return []; }
  reset() {}
  showModal() { this.open = true; }
  close() { this.open = false; for (const fn of this.listeners.get('close') || []) fn(); }
  focus() { this.focused = true; }
  select() { this.selected = true; }
}
function harness({remote = false, clipboard} = {}) {
  const nodes = new Map([...html.matchAll(/\bid="([^"]+)"/g)].map(match => [match[1], new Node()]));
  const requests = [], timers = new Map();
  const root = {dataset:{remoteAdmin:String(remote)}};
  const document = {documentElement:root, getElementById:id => { assert.ok(nodes.has(id), id); return nodes.get(id); }, querySelector:() => new Node(), querySelectorAll:() => [], addEventListener(){}, createElement:() => new Node()};
  let timer = 0;
  const window = {addEventListener(){},setTimeout:fn => { timers.set(++timer, fn); return timer; },clearTimeout:id => timers.delete(id),setInterval(){},clearInterval(){}};
  const fetch = (path, options) => new Promise((resolve, reject) => requests.push({path,options,resolve:(value, status = 200) => resolve({ok:status >= 200 && status < 300,status,json:async() => value}),reject}));
  const expose = `  globalThis.__test = {openTranscriptionSetup,refreshTranscriptionStatus,copyTranscriptionCommand,configureTranscriptionSetup,expireAuth,
    setSetup:value=>{setup=value;},setRemote:value=>{remoteMode=value;},setStatus:value=>{status=value;}};\n})();`;
  const context = vm.createContext({document,window,fetch,AbortController,Option:Node,Date,Intl,URLSearchParams,crypto:crypto.webcrypto,localStorage:{getItem(){return '';},setItem(){},removeItem(){}},navigator:{clipboard},location:{hash:'',pathname:'/'},history:{replaceState(){}},console});
  vm.runInContext(source.replace(ending, expose), context, {timeout:5000});
  const api = context.__test;
  api.setSetup({configured:false,demo:false,policy:{ChatMode:'selective'}});
  api.setStatus({Connection:'disconnected',Codes:[]});
  return {api,nodes,requests,timers,root};
}
const cases = [];
async function check(name, fn) { await fn(); cases.push(name); }
async function open(h, value = {configured:false,state:'not_configured',hebrew_ready:false}) {
  const pending = h.api.openTranscriptionSetup();
  assert.equal(h.requests.at(-1).path, '/api/transcription/status');
  h.requests.at(-1).resolve(value);
  await pending;
}

await check('first run offers Hebrew without claiming installation or blocking text import', async() => {
  const h = harness(); h.api.configureTranscriptionSetup();
  assert.equal(h.nodes.get('setup-transcription').hidden, false);
  assert.equal(h.nodes.get('models-transcription').hidden, false);
  assert.match(html, /Choosing Hebrew opens a guide/);
  assert.match(html, /Continue without voice setup/);
  h.api.setSetup({configured:true}); h.api.configureTranscriptionSetup();
  assert.equal(h.nodes.get('setup-transcription').hidden, true);
  assert.equal(h.nodes.get('models-transcription').hidden, false);
});
await check('the guide uses the Hebrew installer and shared env without exposing or sourcing it', async() => {
  const h = harness(); await open(h);
  const install = h.nodes.get('transcription-install-command').value;
  assert.match(install, /python3\.11 services\/transcription\/setup\.py/);
  assert.match(install, /--python python3\.11[\s\\]+--profile hebrew[\s\\]+--language he/);
  assert.match(install, /\$HOME\/\.local\/share\/my-whatsapp-mcp\/transcription-hebrew/);
  assert.match(h.nodes.get('transcription-service-command').value, /venv\/bin\/python.*services\/transcription\/server\.py[\s\\]+--env-file/);
  assert.match(h.nodes.get('transcription-launch-argument').value, /^--transcription-env-file /);
  assert.match(h.nodes.get('transcription-default-command').value, /\.\/bin\/wa-indexer serve[\s\\]+--transcription-env-file/);
  const commands = [...h.nodes].filter(([id]) => /transcription-.*(?:command|argument)$/.test(id)).map(([,node]) => node.value).join('\n');
  assert.doesNotMatch(commands, /TOKEN|cat |source |eval |sudo|bootstrap/);
  assert.doesNotMatch(h.nodes.get('transcription-launch-argument').value + h.nodes.get('transcription-default-command').value, /--data-dir/, 'launch guidance must not redirect an existing archive');
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].options.method, 'GET', 'opening setup never starts an installer or changes launch configuration');
});
await check('only a verified running Hebrew worker can display connected', async() => {
  for (const [state, ready, copy] of [['not_configured',false,/not configured/i],['connecting',false,/connecting/i],['ready',true,/Hebrew transcription connected/i],['busy',true,/previously verified.*busy/i],['busy',false,/busy.*not yet verified/i],['unavailable',false,/unavailable/i],['mismatch',false,/model, runtime or language/i],['stopped',false,/stopped/i]]) {
    const h = harness(); await open(h, {configured:state !== 'not_configured',state,hebrew_ready:ready});
    assert.match(h.nodes.get('transcription-status').textContent, copy);
    if (state !== 'ready') assert.doesNotMatch(h.nodes.get('transcription-status').textContent, /Hebrew transcription connected/i);
    assert.equal(h.nodes.get('transcription-check').disabled, false);
    assert.equal(h.timers.size, 0);
  }
});
await check('malformed and contradictory readiness cannot claim Hebrew is connected', async() => {
  for (const value of [{}, {configured:true,state:'future_state',hebrew_ready:true}, {configured:false,state:'ready',hebrew_ready:true}, {configured:true,state:'ready',hebrew_ready:false}, {configured:false,state:'busy',hebrew_ready:false}, {configured:false,state:'connecting',hebrew_ready:false}, {configured:false,state:'unavailable',hebrew_ready:false}, {configured:false,state:'mismatch',hebrew_ready:false}, {configured:true,state:'unavailable',hebrew_ready:true}, {configured:true,state:'ready',hebrew_ready:'true'}]) {
    const h = harness(); await open(h, value);
    assert.doesNotMatch(h.nodes.get('transcription-status').textContent, /connected/i);
    assert.match(h.nodes.get('transcription-status').textContent, /could not.*verified/i);
  }
});
await check('an unavailable status check removes a previous connected claim', async() => {
  const h = harness(); await open(h, {configured:true,state:'ready',hebrew_ready:true});
  const pending = h.api.refreshTranscriptionStatus();
  assert.doesNotMatch(h.nodes.get('transcription-status').textContent, /connected/i);
  h.requests.at(-1).resolve({code:'transcription_status_unavailable'},503); await pending;
  assert.doesNotMatch(h.nodes.get('transcription-status').textContent, /connected/i);
  assert.match(h.nodes.get('transcription-status').textContent, /could not.*verified/i);
});
await check('the shared status cooldown gives calm retry guidance without claiming readiness', async() => {
  const h = harness(); const pending = h.api.openTranscriptionSetup();
  h.requests[0].resolve({code:'transcription_status_busy'},429); await pending;
  assert.match(h.nodes.get('transcription-status').textContent, /wait a moment.*check again/i);
  assert.doesNotMatch(h.nodes.get('transcription-status').textContent, /connected/i);
  assert.equal(h.nodes.get('transcription-check').disabled, false);
});
await check('checking coalesces requests and bounds the wait', async() => {
  const h = harness(); const pending = h.api.openTranscriptionSetup();
  await h.api.refreshTranscriptionStatus(); assert.equal(h.requests.length, 1);
  assert.equal(h.nodes.get('transcription-check').disabled, true);
  assert.equal(h.timers.size, 1); for (const fn of h.timers.values()) fn();
  assert.equal(h.requests[0].options.signal.aborted, true);
  const error = new Error('Synthetic timeout'); error.name = 'AbortError'; h.requests[0].reject(error); await pending;
  assert.equal(h.nodes.get('transcription-check').disabled, false);
  assert.equal(h.timers.size, 0);
});
await check('closing the guide fences a late ready response and permits reopening', async() => {
  const h = harness(); const pending = h.api.openTranscriptionSetup(); const old = h.requests[0];
  h.nodes.get('transcription-setup-dialog').close();
  assert.equal(old.options.signal.aborted, true);
  const reopened = h.api.openTranscriptionSetup();
  h.requests[1].resolve({configured:false,state:'not_configured',hebrew_ready:false}); await reopened;
  old.resolve({configured:true,state:'ready',hebrew_ready:true}); await pending;
  assert.match(h.nodes.get('transcription-status').textContent, /not configured/i);
});
await check('a ready response delivered after the deadline cannot claim connection', async() => {
  const h = harness(); const pending = h.api.openTranscriptionSetup();
  for (const fn of h.timers.values()) fn();
  h.requests[0].resolve({configured:true,state:'ready',hebrew_ready:true}); await pending;
  assert.doesNotMatch(h.nodes.get('transcription-status').textContent, /connected/i);
  assert.match(h.nodes.get('transcription-status').textContent, /could not.*verified/i);
  assert.equal(h.nodes.get('transcription-check').disabled, false);
});
await check('logout removes readiness and prevents a late success restoring it', async() => {
  const h = harness(); const pending = h.api.openTranscriptionSetup(); const old = h.requests[0];
  h.api.expireAuth(); old.resolve({configured:true,state:'ready',hebrew_ready:true}); await pending;
  assert.equal(h.nodes.get('transcription-status').textContent, '');
  assert.equal(h.nodes.get('transcription-check').disabled, true);
  const before = h.requests.length; await h.api.openTranscriptionSetup(); assert.equal(h.requests.length, before);
});
await check('remote, demo and historical views cannot open native provisioning checks', async() => {
  for (const mode of ['remote-admin','remote-managed','demo','historical']) {
    const h = harness({remote:mode === 'remote-admin'});
    if (mode === 'remote-managed') h.api.setRemote(true);
    if (mode === 'demo') h.api.setSetup({configured:false,demo:true});
    if (mode === 'historical') h.api.setSetup({configured:false,historical_snapshot:{}});
    h.api.configureTranscriptionSetup(); await h.api.openTranscriptionSetup();
    assert.equal(h.nodes.get('setup-transcription').hidden, true);
    assert.equal(h.nodes.get('models-transcription').hidden, true);
    assert.equal(h.requests.length, 0);
  }
});
await check('copying a command never executes it and has a keyboard fallback', async() => {
  const copied = [], h = harness({clipboard:{writeText:async text => copied.push(text)}}); await open(h);
  await h.api.copyTranscriptionCommand('transcription-install-command');
  assert.equal(copied[0], h.nodes.get('transcription-install-command').value);
  assert.equal(h.requests.length, 1);
  const fallback = harness(); await open(fallback);
  await fallback.api.copyTranscriptionCommand('transcription-service-command');
  assert.equal(fallback.nodes.get('transcription-service-command').selected, true);
  assert.match(fallback.nodes.get('transcription-feedback').textContent, /keyboard/i);
});
await check('a clipboard result cannot restore feedback after the guide closes', async() => {
  let finish; const h = harness({clipboard:{writeText:() => new Promise(resolve => { finish = resolve; })}}); await open(h);
  const pending = h.api.copyTranscriptionCommand('transcription-install-command');
  h.nodes.get('transcription-setup-dialog').close(); finish(); await pending;
  assert.equal(h.nodes.get('transcription-feedback').textContent, '');
});

console.log(JSON.stringify({suite:'Hebrew transcription setup UI',passed:cases.length,cases}));
