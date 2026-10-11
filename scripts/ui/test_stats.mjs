import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import crypto from 'node:crypto';

// Evaluate the shipped script with startup suppressed and synthetic browser
// interfaces. This checks request/authorization races, not native DOM or layout.
const source = fs.readFileSync(new URL('../../internal/localui/assets/app.js', import.meta.url), 'utf8');
const html = fs.readFileSync(new URL('../../internal/localui/assets/index.html', import.meta.url), 'utf8');
const sourceSHA = crypto.createHash('sha256').update(source).digest('hex');
const ending = '  start();\n})();';
assert.equal(source.split(ending).length, 2, 'exact boot seam must be present once');

class FakeNode {
  constructor(tag = 'div') {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.dataset = {};
    this.style = {};
    this.attributes = {};
    this.listeners = new Map();
    this.hidden = false;
    this.disabled = false;
    this.open = false;
    this.checked = false;
    this.clientWidth = 650;
    this._text = '';
    this._value = '';
    this.defaultValue = '';
    this.className = '';
    this.classList = {
      classes:new Set(),
      toggle:(key,force) => { const present = force ?? !this.classList.classes.has(key); if (present) this.classList.classes.add(key); else this.classList.classes.delete(key); return present; },
      contains:key => this.classList.classes.has(key),
      add:(...keys) => keys.forEach(key => this.classList.classes.add(key)),
      remove:(...keys) => keys.forEach(key => this.classList.classes.delete(key)),
    };
  }
  get textContent() { return this._text + this.children.map(child => child.textContent ?? '').join(''); }
  set textContent(value) { this._text = String(value); this.children = []; }
  get value() { return this.tagName === 'PROGRESS' ? Number(this._value) : this._value; }
  set value(value) { this._value = String(value); }
  get options() { return this.children.filter(child => child.tagName === 'OPTION'); }
  get selectedIndex() { return this.options.findIndex(option => option.value === this.value); }
  get parentElement() { return this.parent ?? new FakeNode(); }
  append(...nodes) { for (const node of nodes) { const child = typeof node === 'string' ? textNode(node) : node; child.parent = this; this.children.push(child); } }
  replaceChildren(...nodes) { this.children = []; this._text = ''; this.append(...nodes); }
  setAttribute(key,value) { this.attributes[key] = String(value); }
  removeAttribute(key) { delete this.attributes[key]; }
  getAttribute(key) { return this.attributes[key] ?? null; }
  addEventListener(name,callback) { if (!this.listeners.has(name)) this.listeners.set(name,[]); this.listeners.get(name).push(callback); }
  querySelector() { return new FakeNode(); }
  querySelectorAll() { return []; }
  focus() {}
  close() { this.open = false; }
  showModal() { this.open = true; }
  reset() { this.value = this.defaultValue; }
  click() { for (const callback of this.listeners.get('click') ?? []) callback({preventDefault(){}}); }
}
function textNode(text) { const node = new FakeNode('#text'); node.textContent = text; return node; }
function option(text,value) { const node = new FakeNode('option'); node.textContent = text; node.value = value; return node; }
const snapshot = (count = 6, name = 'Synthetic · צוות Atlas') => ({sampled_at:1791711600,messages:count,chats:2,sent:2,received:count-2,last_message_at:1791711300,activity_interval:'day',activity_step:1,daily:[{date:'2026-10-11',messages:count}],top_chats:[{id:'synthetic-chat',name,messages:count}],kinds:[{kind:'text',messages:count-2},{kind:'voice',messages:2}],media:{voice_messages:2,retained_voice:2,transcribed_voice:1}});
function harness() {
  const nodes = new Map([...html.matchAll(/<([a-z][a-z0-9]*)\b[^>]*\bid="([^"]+)"/gi)].map(match => [match[2],new FakeNode(match[1])]));
  const selectors = new Map([['.skip-link',new FakeNode('a')],['.stats-update',new FakeNode()]]);
  const docListeners = new Map();
  const intervals = new Map();
  const timers = new Map();
  const requests = [];
  let nextTimer = 1;
  const document = {
    hidden:false,documentElement:{dataset:{remoteAdmin:'false'}},activeElement:null,
    getElementById:id => { assert.ok(nodes.has(id),'real HTML must contain ID ' + id); return nodes.get(id); },
    createElement:tag => new FakeNode(tag),createElementNS:(_,tag) => new FakeNode(tag),createTextNode:textNode,
    querySelector:selector => { if (!selectors.has(selector)) selectors.set(selector,new FakeNode()); return selectors.get(selector); },
    querySelectorAll:() => [],
    addEventListener:(name,callback) => { if (!docListeners.has(name)) docListeners.set(name,[]); docListeners.get(name).push(callback); },
  };
  for (const id of ['stats-range','stats-kind','stats-chat']) nodes.get(id).tagName = 'SELECT';
  nodes.get('stats-range').append(option('All saved history','all'),option('Last 7 days','7'),option('Custom dates','custom'));
  nodes.get('stats-kind').append(option('All message types','all'),option('Voice notes','voice'));
  nodes.get('stats-chat').append(option('All archived chats',''));
  for (const [id,value] of [['stats-range','all'],['stats-kind','all'],['stats-chat','']]) { nodes.get(id).value=value; nodes.get(id).defaultValue=value; }
  nodes.get('stats-filters').reset = () => { for (const id of ['stats-range','stats-kind','stats-chat','stats-from','stats-until']) nodes.get(id).value=nodes.get(id).defaultValue; };
  const fakeWindow = {
    addEventListener(){},
    setTimeout:callback => { const id=nextTimer++; timers.set(id,callback); return id; },clearTimeout:id => timers.delete(id),
    setInterval:callback => { const id=nextTimer++; intervals.set(id,callback); return id; },clearInterval:id => intervals.delete(id),
  };
  const fetch = (path,options={}) => new Promise((resolve,reject) => { requests.push({path,options,resolve:(value,status=200) => resolve({ok:status>=200&&status<300,status,json:async()=>value}),resolveJSON:json => resolve({ok:true,status:200,json}),reject}); });
  const expose = `  globalThis.__test = {refreshStats,resetStats,selectWorkspaceView,changeStatsFilters,statsFilter,invalidateStats,renderStats,renderStatsChart,renderStatsStatus,renderStatsProgress,expireAuth,startPolling,stopPolling,
    state:()=>({activeView,statsSnapshot,statsVersion,statsController,statsReloadQueued,authExpired,authEpoch}),
    setStatus:value=>{status=value;},setSetup:value=>{setup=value;},setChats:value=>{chats=value;},setView:value=>{activeView=value;}};\n})();`;
  const context = vm.createContext({document,window:fakeWindow,fetch,AbortController,Option:option,Date,Intl,URLSearchParams,localStorage:{getItem(){return '';},removeItem(){},setItem(){}},navigator:{clipboard:{}},location:{hash:'',pathname:'/'},history:{replaceState(){}},console});
  vm.runInContext(source.replace(ending,expose),context,{timeout:5000});
  assert.equal(requests.length,0,'test loading must not start network requests');
  const api = context.__test;
  api.setSetup({configured:true,Policy:{ChatMode:'all'}});
  api.setStatus({Connection:'connected',Messages:6,Keyword:{State:'ready',Complete:6,Pending:0,Failed:0},Media:{State:'ready',Complete:1,Pending:1,Failed:0},Semantic:{State:'not_configured'},Codes:[]});
  return {api,nodes,document,requests,intervals,timers,fire:name => { for(const callback of docListeners.get(name) ?? []) callback(); }};
}
const flush = () => new Promise(resolve => setImmediate(resolve));
const cases = [];
async function check(name,body) { await body(); cases.push(name); }

await check('filter payload uses scoped IDs, type and inclusive dates',async()=>{
  const h=harness(); assert.deepEqual(JSON.parse(JSON.stringify(h.api.statsFilter())),{});
  h.nodes.get('stats-range').value='custom'; h.nodes.get('stats-from').value='2026-10-05'; h.nodes.get('stats-until').value='2026-10-11'; h.nodes.get('stats-chat').value='synthetic-chat'; h.nodes.get('stats-kind').value='voice';
  const actual=JSON.parse(JSON.stringify(h.api.statsFilter()));
  assert.deepEqual(actual,{from:new Date('2026-10-05T00:00:00').getTime()/1000,until:new Date('2026-10-12T00:00:00').getTime()/1000,chat_id:'synthetic-chat',kind:'voice'});
  h.nodes.get('stats-from').value='2026-10-12'; assert.throws(()=>h.api.statsFilter(),/end date/);
  h.nodes.get('stats-from').value=''; h.nodes.get('stats-until').value=''; assert.throws(()=>h.api.statsFilter(),/at least one/);
});
await check('actual rendering preserves Hebrew and treats source names as text',async()=>{
  const h=harness(); const name='Synthetic · שלום <img src=x onerror=alert(1)>'; h.api.renderStats(snapshot(6,name));
  assert.equal(h.nodes.get('stats-message-count').textContent,'6');
  const nameNode=h.nodes.get('stats-top-chats').children[0].children[0].children[0]; assert.equal(nameNode.tagName,'BDI'); assert.equal(nameNode.dir,'auto'); assert.equal(nameNode.textContent,name); assert.equal(nameNode.children.length,0);
  assert.equal(h.nodes.get('stats-transcribed').textContent,'1 / 2'); assert.equal(h.nodes.get('stats-voice-progress').value,1); assert.equal(h.nodes.get('stats-voice-progress').max,2);
  assert.match(h.nodes.get('stats-chart').children[0].attributes['aria-label'],/accessible table/); assert.equal(h.nodes.get('stats-activity-rows').children.length,1);
});
await check('rapid filter changes abort and discard the older response with one request in flight',async()=>{
  const h=harness(); const pending=h.api.refreshStats(); assert.equal(h.requests.length,1);
  h.api.refreshStats(); assert.equal(h.requests.length,1);
  h.nodes.get('stats-kind').value='voice'; h.api.changeStatsFilters(); assert.equal(h.requests[0].options.signal.aborted,true); assert.equal(h.requests.length,1);
  h.requests[0].resolve(snapshot(111,'STALE_NAME')); await pending; await flush(); assert.equal(h.requests.length,2); assert.equal(JSON.parse(h.requests[1].options.body).kind,'voice'); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.doesNotMatch(h.nodes.get('stats-top-chats').textContent,/STALE_NAME/);
  h.requests[1].resolve(snapshot(222,'CURRENT_NAME')); await flush(); assert.equal(h.nodes.get('stats-message-count').textContent,'222'); assert.match(h.nodes.get('stats-top-chats').textContent,/CURRENT_NAME/);
});
await check('owner expiry clears names, filters and progress and rejects a late response',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'PRIVATE_SYNTHETIC_NAME')); h.nodes.get('stats-chat').append(option('PRIVATE_SYNTHETIC_NAME','synthetic-chat')); h.nodes.get('stats-chat').value='synthetic-chat';
  const pending=h.api.refreshStats(); h.api.expireAuth(); assert.equal(h.requests[0].options.signal.aborted,true); assert.equal(h.api.state().authExpired,true); assert.equal(h.api.state().statsSnapshot,null); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-chat').value,''); assert.equal(h.nodes.get('stats-chat').options.length,1); assert.equal(h.nodes.get('stats-chat').textContent,'All archived chats');
  h.requests[0].resolve(snapshot(333,'LATE_PRIVATE_SYNTHETIC_NAME')); await pending; assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.doesNotMatch(h.nodes.get('stats-top-chats').textContent,/PRIVATE_SYNTHETIC_NAME/); assert.equal(h.nodes.get('stats-media-progress').value,0);
});
await check('401 from Stats expires the owner and clears rendered aggregates',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'PRIVATE_SYNTHETIC_NAME')); const pending=h.api.refreshStats(); h.requests[0].resolve({code:'unauthorized'},401); await pending; assert.equal(h.api.state().authExpired,true); assert.equal(h.nodes.get('workspace').hidden,true); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-top-chats').children.length,0);
});
await check('hidden page and Archive view abort and cannot publish stale Stats',async()=>{
  const h=harness(); const pending=h.api.refreshStats(); h.document.hidden=true; h.fire('visibilitychange'); assert.equal(h.requests[0].options.signal.aborted,true); await h.api.refreshStats(); assert.equal(h.requests.length,1); h.requests[0].resolve(snapshot(444,'HIDDEN_STALE_NAME')); await pending; assert.equal(h.api.state().statsSnapshot,null);
  h.document.hidden=false; const second=h.api.refreshStats(); h.api.selectWorkspaceView('archive'); assert.equal(h.requests[1].options.signal.aborted,true); h.requests[1].resolve(snapshot(555,'ARCHIVE_STALE_NAME')); await second; assert.equal(h.api.state().statsSnapshot,null); assert.equal(h.nodes.get('stats-view').hidden,true); assert.equal(h.nodes.get('archive-view').hidden,false);
});
await check('unsafe archive hides aggregates without fetching and clears source details',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'PRIVATE_SYNTHETIC_NAME')); h.api.setStatus({Connection:'reconciliation_required',ReconciliationRequired:true,Codes:[]}); await h.api.refreshStats(); assert.equal(h.requests.length,0); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-top-chats').children.length,0); assert.match(h.nodes.get('stats-feedback').textContent,/unavailable/);
});
await check('job progress is real completed/pending/failed count with explicit busy status',async()=>{
  const h=harness(); h.api.renderStatsProgress('media',{State:'ready',Code:'transcription_busy',Complete:9,Pending:2,Failed:1}); assert.equal(h.nodes.get('stats-media-progress').value,9); assert.equal(h.nodes.get('stats-media-progress').max,12); assert.equal(h.nodes.get('stats-media-state').textContent,'Transcribing another recording'); assert.equal(h.nodes.get('stats-media-detail').textContent,'9 completed · 2 waiting · 1 failed'); h.api.renderStatsProgress('semantic',{State:'not_configured'}); assert.equal(h.nodes.get('stats-semantic-progress').hidden,true); assert.match(h.nodes.get('stats-semantic-detail').textContent,/Optional/);
});
await check('malformed successful response clears old aggregates and exposes retry guidance',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'OLD_SYNTHETIC_NAME')); const pending=h.api.refreshStats(); h.requests[0].resolve({...snapshot(),messages:-1}); await pending; assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-top-chats').children.length,0); assert.equal(h.nodes.get('stats-updated').textContent,'Update unavailable'); assert.match(h.nodes.get('stats-feedback').textContent,/Refresh stats/); assert.equal(h.nodes.get('stats-refresh').disabled,false);
});
await check('empty filtered dataset presents zero totals and an explicit no-match state',async()=>{
  const h=harness(); h.api.renderStats({...snapshot(0),sent:0,received:0,chats:0,daily:[],top_chats:[],kinds:[],media:{voice_messages:0,retained_voice:0,transcribed_voice:0}}); assert.equal(h.nodes.get('stats-message-count').textContent,'0'); assert.equal(h.nodes.get('stats-chat-count').textContent,'0'); assert.match(h.nodes.get('stats-chart').textContent,/No readable messages match/); assert.equal(h.nodes.get('stats-activity-table').hidden,true); assert.equal(h.nodes.get('stats-transcribed').textContent,'0 / 0'); assert.equal(h.nodes.get('stats-voice-progress').max,1); assert.equal(h.nodes.get('stats-voice-progress').value,0);
});
await check('aborting a deferred response body retains timeout guidance and releases the request',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'OLD_SYNTHETIC_NAME')); const pending=h.api.refreshStats(); let rejectBody; const body=new Promise((_,reject)=>{rejectBody=reject;}); h.requests[0].resolveJSON(()=>body); await flush(); assert.equal(h.requests[0].options.signal.aborted,false); assert.equal(h.timers.size,1); [...h.timers.values()][0](); assert.equal(h.requests[0].options.signal.aborted,true); const abort=new Error('Synthetic body aborted'); abort.name='AbortError'; rejectBody(abort); await pending; assert.equal(h.api.state().statsController,null); assert.equal(h.nodes.get('stats-data').attributes['aria-busy'],'false'); assert.equal(h.nodes.get('stats-refresh').disabled,false); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-top-chats').children.length,0); assert.match(h.nodes.get('stats-feedback').textContent,/Stats took too long/);
});
console.log(JSON.stringify({ok:true,tests:cases.length,cases,app_js_sha256:sourceSHA,scope:'actual JavaScript with minimal synthetic DOM; no browser layout proof'},null,2));
