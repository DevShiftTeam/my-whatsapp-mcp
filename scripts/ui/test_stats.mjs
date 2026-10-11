import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import crypto from 'node:crypto';

// Evaluate the shipped script with startup suppressed and synthetic browser
// interfaces. This checks requests, authorization races and accessible controls,
// not native DOM, browser geometry or layout.
const source = fs.readFileSync(new URL('../../internal/localui/assets/app.js', import.meta.url), 'utf8');
const html = fs.readFileSync(new URL('../../internal/localui/assets/index.html', import.meta.url), 'utf8');
const sourceSHA = crypto.createHash('sha256').update(source).digest('hex');
const ending = '  start();\n})();';
assert.equal(source.split(ending).length, 2, 'exact boot seam must be present once');

class FakeNode {
  constructor(tag = 'div') {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.dataset = new Proxy({}, {set:(target,key,value) => { target[key]=String(value); this.attributes['data-'+String(key).replace(/[A-Z]/g,char=>'-'+char.toLowerCase())]=String(value); return true; }});
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
  get tabIndex() { return Number(this.attributes.tabindex ?? (['BUTTON','SELECT','INPUT','TEXTAREA','SUMMARY'].includes(this.tagName) ? 0 : -1)); }
  set tabIndex(value) { this.attributes.tabindex = String(value); }
  append(...nodes) { for (const node of nodes) { const child = typeof node === 'string' ? textNode(node) : node; child.parent = this; this.children.push(child); } }
  replaceChildren(...nodes) { this.children = []; this._text = ''; this.append(...nodes); }
  setAttribute(key,value) { this.attributes[key] = String(value); if (key.startsWith('data-')) this.dataset[key.slice(5).replace(/-([a-z])/g,(_,char)=>char.toUpperCase())]=value; }
  removeAttribute(key) { delete this.attributes[key]; }
  getAttribute(key) { return this.attributes[key] ?? null; }
  addEventListener(name,callback) { if (!this.listeners.has(name)) this.listeners.set(name,[]); this.listeners.get(name).push(callback); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] ?? null; }
  querySelectorAll(selector) { return descendants(this).filter(node => matches(node,selector)); }
  contains(node) { return this===node || descendants(this).includes(node); }
  focus() { if (this.ownerDocument) this.ownerDocument.activeElement = this; this.dispatch('focus'); }
  close() { this.open = false; }
  showModal() { this.open = true; }
  reset() { this.value = this.defaultValue; }
  dispatch(name,values={}) { const event={target:this,currentTarget:this,defaultPrevented:false,preventDefault(){this.defaultPrevented=true;},...values}; for (const callback of this.listeners.get(name) ?? []) callback(event); return event; }
  click() { if (!this.disabled) this.dispatch('click'); }
}
function descendants(node) { return node.children.flatMap(child => [child,...descendants(child)]); }
function matches(node,selector) {
  if (selector === '*') return true;
  if (selector.startsWith('.')) return node.className.split(/\s+/).includes(selector.slice(1));
  const attribute=selector.match(/^\[([^=\]]+)(?:=["']?([^"'\]]+)["']?)?\]$/);
  if (attribute) return node.getAttribute(attribute[1]) !== null && (attribute[2] === undefined || node.getAttribute(attribute[1]) === attribute[2]);
  return node.tagName === selector.toUpperCase();
}
function textNode(text) { const node = new FakeNode('#text'); node.textContent = text; return node; }
function option(text,value) { const node = new FakeNode('option'); node.textContent = text; node.value = value; return node; }
function snapshot(count=6,name='Synthetic · צוות Atlas') {
  const voice=Math.min(2,count),text=count-voice,sent=Math.min(2,count),firstSent=Math.min(sent,text);
  const rows=[];
  if (text) rows.push({id:'synthetic-chat',name,messages:text,sent:firstSent,received:text-firstSent,chats:1,last_message_at:1791708300});
  if (voice) rows.push({id:'synthetic-other',name:'Synthetic second chat',messages:voice,sent:sent-firstSent,received:voice-sent+firstSent,chats:1,last_message_at:1791711300});
  return {
    sampled_at:1791711600,messages:count,chats:rows.length,sent,received:count-sent,last_message_at:count ? 1791711300 : 0,
    activity_interval:'day',activity_step:1,daily:count ? [{date:'2026-10-11',messages:count}] : [],
    top_chats:rows.map(({id,name,messages})=>({id,name,messages})),
    kinds:count ? [{kind:'text',messages:text},{kind:'voice',messages:voice}] : [],
    media:{voice_messages:voice,retained_voice:voice,transcribed_voice:voice ? 1 : 0},
    insights:{active_days:count ? 1 : 0,longest_streak_days:count ? 1 : 0,busiest_date:count ? '2026-10-11' : '',busiest_date_messages:count},
    rhythm:[...(text ? [{weekday:0,hour:8,messages:text}] : []),...(voice ? [{weekday:0,hour:9,messages:voice}] : [])],
    breakdown:{dimension:'chats',page:1,page_size:10,total:rows.length,sort:'messages',rows},
  };
}
function pageSnapshot(page=1,total=23,pageSize=10,name='Synthetic page row') {
  const first=(page-1)*pageSize;
  return {...snapshot(total*10),chats:total,sent:total*2,received:total*8,top_chats:Array.from({length:Math.min(8,total)},(_,index)=>({id:'synthetic-row-'+index,name:name+' '+(index+1),messages:10})),breakdown:{dimension:'chats',page,page_size:pageSize,total,sort:'messages',rows:Array.from({length:Math.max(0,Math.min(pageSize,total-first))},(_,index)=>({id:'synthetic-row-'+(first+index),name:name+' '+(first+index+1),messages:10,sent:2,received:8,chats:1,last_message_at:1791711300-first-index}))}};
}
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
    createElement:tag => { const node=new FakeNode(tag); node.ownerDocument=document; return node; },createElementNS:(_,tag) => { const node=new FakeNode(tag); node.ownerDocument=document; return node; },createTextNode:textNode,
    querySelector:selector => { if (!selectors.has(selector)) selectors.set(selector,new FakeNode()); return selectors.get(selector); },
    querySelectorAll:() => [],
    addEventListener:(name,callback) => { if (!docListeners.has(name)) docListeners.set(name,[]); docListeners.get(name).push(callback); },
  };
  for (const node of nodes.values()) node.ownerDocument=document;
  for (const id of ['stats-range','stats-kind','stats-chat','stats-breakdown-sort','stats-breakdown-size']) nodes.get(id).tagName = 'SELECT';
  nodes.get('stats-range').append(option('All saved history','all'),option('Last 7 days','7'),option('Custom dates','custom'));
  nodes.get('stats-kind').append(option('All message types','all'),option('Voice notes','voice'));
  nodes.get('stats-chat').append(option('All archived chats',''));
  nodes.get('stats-breakdown-sort').append(option('Most messages','messages'),option('Name','name'),option('Most recent','recent'));
  nodes.get('stats-breakdown-size').append(option('10 rows','10'),option('25 rows','25'),option('50 rows','50'));
  nodes.get('stats-breakdown-sort').value='messages';
  nodes.get('stats-breakdown-size').value='10';
  for (const [id,value] of [['stats-range','all'],['stats-kind','all'],['stats-chat','']]) { nodes.get(id).value=value; nodes.get(id).defaultValue=value; }
  nodes.get('stats-filters').reset = () => { for (const id of ['stats-range','stats-kind','stats-chat','stats-from','stats-until']) nodes.get(id).value=nodes.get(id).defaultValue; };
  const fakeWindow = {
    addEventListener(){},
    setTimeout:callback => { const id=nextTimer++; timers.set(id,callback); return id; },clearTimeout:id => timers.delete(id),
    setInterval:callback => { const id=nextTimer++; intervals.set(id,callback); return id; },clearInterval:id => intervals.delete(id),
  };
  const fetch = (path,options={}) => new Promise((resolve,reject) => { requests.push({path,options,resolve:(value,status=200) => resolve({ok:status>=200&&status<300,status,json:async()=>value}),resolveJSON:json => resolve({ok:true,status:200,json}),reject}); });
  const expose = `  globalThis.__test = {refreshStats,resetStats,selectWorkspaceView,changeStatsFilters,statsFilter,invalidateStats,renderStats,renderStatsChart,renderStatsStatus,renderStatsProgress,renderStatsRhythm,renderStatsBreakdown,changeStatsBreakdown,setStatsChartMode,expireAuth,startPolling,stopPolling,
    state:()=>({activeView,statsSnapshot,statsVersion,statsController,statsReloadQueued,authExpired,authEpoch,statsDimension,statsPage,statsPageSize,statsSort,statsChartMode}),
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
  const h=harness(); const paging={breakdown:'chats',page:1,page_size:10,sort:'messages'}; assert.deepEqual(JSON.parse(JSON.stringify(h.api.statsFilter())),paging);
  h.nodes.get('stats-range').value='custom'; h.nodes.get('stats-from').value='2026-10-05'; h.nodes.get('stats-until').value='2026-10-11'; h.nodes.get('stats-chat').value='synthetic-chat'; h.nodes.get('stats-kind').value='voice';
  const actual=JSON.parse(JSON.stringify(h.api.statsFilter()));
  assert.deepEqual(actual,{...paging,from:new Date('2026-10-05T00:00:00').getTime()/1000,until:new Date('2026-10-12T00:00:00').getTime()/1000,chat_id:'synthetic-chat',kind:'voice'});
  h.nodes.get('stats-from').value='2026-10-12'; assert.throws(()=>h.api.statsFilter(),/end date/);
  h.nodes.get('stats-from').value=''; h.nodes.get('stats-until').value=''; assert.throws(()=>h.api.statsFilter(),/at least one/);
});
await check('actual rendering preserves Hebrew and treats source names as text',async()=>{
  const h=harness(); const name='Synthetic · שלום <img src=x onerror=alert(1)>'; h.api.renderStats(snapshot(6,name));
  assert.equal(h.nodes.get('stats-message-count').textContent,'6');
  const nameNode=descendants(h.nodes.get('stats-aggregate-rows')).find(node=>node.tagName==='BDI'); assert.ok(nameNode); assert.equal(nameNode.dir,'auto'); assert.equal(nameNode.textContent,name); assert.equal(nameNode.children.length,0);
  assert.equal(h.nodes.get('stats-transcribed').textContent,'1 / 2'); assert.equal(h.nodes.get('stats-voice-progress').value,1); assert.equal(h.nodes.get('stats-voice-progress').max,2);
  assert.equal(h.nodes.get('stats-chart').children[0].attributes.role,'group'); assert.match(h.nodes.get('stats-chart').children[0].attributes['aria-label'],/table/i); assert.equal(h.nodes.get('stats-activity-rows').children.length,1);
});
await check('rapid filter changes abort and discard the older response with one request in flight',async()=>{
  const h=harness(); const pending=h.api.refreshStats(); assert.equal(h.requests.length,1);
  h.api.refreshStats(); assert.equal(h.requests.length,1);
  h.nodes.get('stats-kind').value='voice'; h.api.changeStatsFilters(); assert.equal(h.requests[0].options.signal.aborted,true); assert.equal(h.requests.length,1);
  h.requests[0].resolve(snapshot(111,'STALE_NAME')); await pending; await flush(); assert.equal(h.requests.length,2); assert.equal(JSON.parse(h.requests[1].options.body).kind,'voice'); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.doesNotMatch(h.nodes.get('stats-aggregate-rows').textContent,/STALE_NAME/);
  h.requests[1].resolve(snapshot(222,'CURRENT_NAME')); await flush(); assert.equal(h.nodes.get('stats-message-count').textContent,'222'); assert.match(h.nodes.get('stats-aggregate-rows').textContent,/CURRENT_NAME/);
});
await check('owner expiry clears names, filters and progress and rejects a late response',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'PRIVATE_SYNTHETIC_NAME')); h.nodes.get('stats-chat').append(option('PRIVATE_SYNTHETIC_NAME','synthetic-chat')); h.nodes.get('stats-chat').value='synthetic-chat';
  const pending=h.api.refreshStats(); h.api.expireAuth(); assert.equal(h.requests[0].options.signal.aborted,true); assert.equal(h.api.state().authExpired,true); assert.equal(h.api.state().statsSnapshot,null); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-chat').value,''); assert.equal(h.nodes.get('stats-chat').options.length,1); assert.equal(h.nodes.get('stats-chat').textContent,'All archived chats');
  h.requests[0].resolve(snapshot(333,'LATE_PRIVATE_SYNTHETIC_NAME')); await pending; assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.doesNotMatch(h.nodes.get('stats-aggregate-rows').textContent,/PRIVATE_SYNTHETIC_NAME/); assert.equal(h.nodes.get('stats-media-progress').value,0); assert.equal(h.nodes.get('stats-heatmap').children.length,0); assert.equal(h.nodes.get('stats-active-days').textContent,'—'); assert.equal(h.nodes.get('stats-streak').textContent,'—'); assert.equal(h.nodes.get('stats-busiest-day').textContent,'—'); assert.equal(h.nodes.get('stats-daily-average').textContent,'—');
});
await check('401 from Stats expires the owner and clears rendered aggregates',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'PRIVATE_SYNTHETIC_NAME')); const pending=h.api.refreshStats(); h.requests[0].resolve({code:'unauthorized'},401); await pending; assert.equal(h.api.state().authExpired,true); assert.equal(h.nodes.get('workspace').hidden,true); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0);
});
await check('hidden page and Archive view abort and cannot publish stale Stats',async()=>{
  const h=harness(); const pending=h.api.refreshStats(); h.document.hidden=true; h.fire('visibilitychange'); assert.equal(h.requests[0].options.signal.aborted,true); await h.api.refreshStats(); assert.equal(h.requests.length,1); h.requests[0].resolve(snapshot(444,'HIDDEN_STALE_NAME')); await pending; assert.equal(h.api.state().statsSnapshot,null);
  h.document.hidden=false; const second=h.api.refreshStats(); h.api.selectWorkspaceView('archive'); assert.equal(h.requests[1].options.signal.aborted,true); h.requests[1].resolve(snapshot(555,'ARCHIVE_STALE_NAME')); await second; assert.equal(h.api.state().statsSnapshot,null); assert.equal(h.nodes.get('stats-view').hidden,true); assert.equal(h.nodes.get('archive-view').hidden,false);
});
await check('unsafe archive hides aggregates without fetching and clears source details',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'PRIVATE_SYNTHETIC_NAME')); h.api.setStatus({Connection:'reconciliation_required',ReconciliationRequired:true,Codes:[]}); await h.api.refreshStats(); assert.equal(h.requests.length,0); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0); assert.equal(h.nodes.get('stats-heatmap').children.length,0); assert.match(h.nodes.get('stats-feedback').textContent,/unavailable/);
});
await check('job progress is real completed/pending/failed count with explicit busy status',async()=>{
  const h=harness(); h.api.renderStatsProgress('media',{State:'ready',Code:'transcription_busy',Complete:9,Pending:2,Failed:1}); assert.equal(h.nodes.get('stats-media-progress').value,9); assert.equal(h.nodes.get('stats-media-progress').max,12); assert.equal(h.nodes.get('stats-media-state').textContent,'Transcribing another recording'); assert.equal(h.nodes.get('stats-media-detail').textContent,'9 completed · 2 waiting · 1 failed'); h.api.renderStatsProgress('semantic',{State:'not_configured'}); assert.equal(h.nodes.get('stats-semantic-progress').hidden,true); assert.match(h.nodes.get('stats-semantic-detail').textContent,/Optional/);
});
await check('malformed successful response clears old aggregates and exposes retry guidance',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'OLD_SYNTHETIC_NAME')); const pending=h.api.refreshStats(); h.requests[0].resolve({...snapshot(),messages:-1}); await pending; assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0); assert.equal(h.nodes.get('stats-updated').textContent,'Update unavailable'); assert.match(h.nodes.get('stats-feedback').textContent,/Refresh stats/); assert.equal(h.nodes.get('stats-refresh').disabled,false);
});
await check('empty filtered dataset presents zero totals and an explicit no-match state',async()=>{
  const h=harness(); h.api.renderStats({...snapshot(0),sent:0,received:0,chats:0,daily:[],top_chats:[],kinds:[],media:{voice_messages:0,retained_voice:0,transcribed_voice:0},insights:{active_days:0,longest_streak_days:0,busiest_date:'',busiest_date_messages:0},rhythm:[],breakdown:{dimension:'chats',page:1,page_size:10,total:0,sort:'messages',rows:[]}}); assert.equal(h.nodes.get('stats-message-count').textContent,'0'); assert.equal(h.nodes.get('stats-chat-count').textContent,'0'); assert.match(h.nodes.get('stats-chart').textContent,/No readable messages match/); assert.equal(h.nodes.get('stats-activity-table').hidden,true); assert.equal(h.nodes.get('stats-transcribed').textContent,'0 / 0'); assert.equal(h.nodes.get('stats-voice-progress').max,1); assert.equal(h.nodes.get('stats-voice-progress').value,0); assert.equal(h.nodes.get('stats-page-prev').disabled,true); assert.equal(h.nodes.get('stats-page-next').disabled,true); assert.equal(h.nodes.get('stats-active-days').textContent,'0'); assert.equal(h.nodes.get('stats-daily-average').textContent,'0');
});
await check('aborting a deferred response body retains timeout guidance and releases the request',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'OLD_SYNTHETIC_NAME')); const pending=h.api.refreshStats(); let rejectBody; const body=new Promise((_,reject)=>{rejectBody=reject;}); h.requests[0].resolveJSON(()=>body); await flush(); assert.equal(h.requests[0].options.signal.aborted,false); assert.equal(h.timers.size,1); [...h.timers.values()][0](); assert.equal(h.requests[0].options.signal.aborted,true); const abort=new Error('Synthetic body aborted'); abort.name='AbortError'; rejectBody(abort); await pending; assert.equal(h.api.state().statsController,null); assert.equal(h.nodes.get('stats-data').attributes['aria-busy'],'false'); assert.equal(h.nodes.get('stats-refresh').disabled,false); assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0); assert.match(h.nodes.get('stats-feedback').textContent,/Stats took too long/);
});
await check('aggregate paging controls send bounded pages and display a clamped final page',async()=>{
  const h=harness(); h.api.renderStats(pageSnapshot());
  assert.equal(h.nodes.get('stats-aggregate-rows').children.length,10);
  assert.equal(h.nodes.get('stats-page-prev').disabled,true); assert.equal(h.nodes.get('stats-page-next').disabled,false);
  assert.match(h.nodes.get('stats-page-description').textContent,/1[–-]10.*23/);
  h.nodes.get('stats-page-next').click(); assert.equal(h.requests.length,1);
  assert.deepEqual(JSON.parse(h.requests[0].options.body),{breakdown:'chats',page:2,page_size:10,sort:'messages'});
  h.requests[0].resolve(pageSnapshot(2)); await flush(); assert.equal(h.nodes.get('stats-page-prev').disabled,false);
  h.nodes.get('stats-page-next').click(); assert.equal(JSON.parse(h.requests[1].options.body).page,3);
  h.requests[1].resolve(pageSnapshot(2,12)); await flush();
  assert.equal(h.api.state().statsPage,2,'the server may clamp a page after data shrinks');
  assert.equal(h.nodes.get('stats-aggregate-rows').children.length,2); assert.equal(h.nodes.get('stats-page-next').disabled,true);
  assert.match(h.nodes.get('stats-page-description').textContent,/11[–-]12.*12/);
  h.nodes.get('stats-page-next').click(); assert.equal(h.requests.length,2,'last-page control cannot request another page');
  h.nodes.get('stats-page-prev').click(); assert.equal(JSON.parse(h.requests[2].options.body).page,1);
});
await check('dimension, sort and page-size controls reset to page one while polling preserves the chosen page',async()=>{
  const h=harness(); h.api.renderStats(pageSnapshot(3));
  h.nodes.get('stats-breakdown-people').click(); assert.equal(h.requests.length,1);
  assert.deepEqual(JSON.parse(h.requests[0].options.body),{breakdown:'people',page:1,page_size:10,sort:'messages'});
  h.requests[0].resolve({...pageSnapshot(),breakdown:{...pageSnapshot().breakdown,dimension:'people'}}); await flush();
  assert.equal(h.nodes.get('stats-breakdown-people').getAttribute('aria-pressed'),'true'); assert.equal(h.nodes.get('stats-breakdown-chats').getAttribute('aria-pressed'),'false');
  h.nodes.get('stats-breakdown-sort').value='name'; h.nodes.get('stats-breakdown-sort').dispatch('change');
  assert.deepEqual(JSON.parse(h.requests[1].options.body),{breakdown:'people',page:1,page_size:10,sort:'name'});
  h.requests[1].resolve({...pageSnapshot(),breakdown:{...pageSnapshot().breakdown,dimension:'people',sort:'name'}}); await flush();
  h.nodes.get('stats-breakdown-size').value='25'; h.nodes.get('stats-breakdown-size').dispatch('change');
  assert.deepEqual(JSON.parse(h.requests[2].options.body),{breakdown:'people',page:1,page_size:25,sort:'name'});
  h.requests[2].resolve({...pageSnapshot(1,53,25),breakdown:{...pageSnapshot(1,53,25).breakdown,dimension:'people',sort:'name'}}); await flush();
  h.nodes.get('stats-page-next').click(); h.requests[3].resolve({...pageSnapshot(2,53,25),breakdown:{...pageSnapshot(2,53,25).breakdown,dimension:'people',sort:'name'}}); await flush();
  const poll=h.api.refreshStats(); assert.deepEqual(JSON.parse(h.requests[4].options.body),{breakdown:'people',page:2,page_size:25,sort:'name'});
  h.requests[4].resolve({...pageSnapshot(2,53,25),breakdown:{...pageSnapshot(2,53,25).breakdown,dimension:'people',sort:'name'}}); await poll;
  h.nodes.get('stats-kind').value='voice'; h.api.changeStatsFilters(); assert.equal(JSON.parse(h.requests[5].options.body).page,1); assert.equal(JSON.parse(h.requests[5].options.body).breakdown,'people'); assert.equal(JSON.parse(h.requests[5].options.body).page_size,25);
});
await check('newer aggregate controls abort a page request and discard its names, page and totals',async()=>{
  const h=harness(); h.api.renderStats(pageSnapshot());
  h.nodes.get('stats-page-next').click(); assert.equal(JSON.parse(h.requests[0].options.body).page,2);
  h.api.changeStatsBreakdown({sort:'recent'}); assert.equal(h.requests[0].options.signal.aborted,true); assert.equal(h.requests.length,1);
  assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0);
  h.requests[0].resolve(pageSnapshot(2,23,10,'STALE_PAGE_NAME')); await flush();
  assert.equal(h.requests.length,2); assert.deepEqual(JSON.parse(h.requests[1].options.body),{breakdown:'chats',page:1,page_size:10,sort:'recent'});
  assert.doesNotMatch(h.nodes.get('stats-aggregate-rows').textContent,/STALE_PAGE_NAME/); assert.equal(h.api.state().statsPage,1);
  h.requests[1].resolve({...pageSnapshot(1,4,10,'CURRENT_SORT_NAME'),breakdown:{...pageSnapshot(1,4,10,'CURRENT_SORT_NAME').breakdown,sort:'recent'}}); await flush();
  assert.match(h.nodes.get('stats-aggregate-rows').textContent,/CURRENT_SORT_NAME/); assert.equal(h.nodes.get('stats-page-next').disabled,true);
});
await check('reset restores default aggregate and chart choices and removes selected details',async()=>{
  const h=harness(); h.api.renderStats(pageSnapshot(2)); h.api.changeStatsBreakdown({dimension:'people',sort:'recent',pageSize:50}); h.api.setStatsChartMode('line'); h.api.resetStats();
  assert.deepEqual(JSON.parse(JSON.stringify(h.api.statsFilter())),{breakdown:'chats',page:1,page_size:10,sort:'messages'});
  assert.equal(h.api.state().statsChartMode,'bars'); assert.equal(h.nodes.get('stats-breakdown-sort').value,'messages'); assert.equal(h.nodes.get('stats-breakdown-size').value,'10');
  assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0); assert.equal(h.nodes.get('stats-heatmap').children.length,0); assert.doesNotMatch(h.nodes.get('stats-rhythm-detail').textContent,/Synthetic/);
});
await check('rhythm renders all UTC hours accessibly from sparse data with a table alternative',async()=>{
  const h=harness(); h.api.renderStats(snapshot());
  const cells=descendants(h.nodes.get('stats-heatmap')).filter(node=>node.tagName==='BUTTON'); assert.equal(cells.length,168);
  assert.equal(cells.filter(node=>node.tabIndex===0).length,1);
  const selected=cells.find(node=>node.dataset.weekday==='0'&&node.dataset.hour==='8'); assert.ok(selected);
  assert.match(selected.getAttribute('aria-label'),/Sunday.*08|Sun.*08/); assert.match(selected.getAttribute('aria-label'),/4.*message/);
  selected.focus(); assert.match(h.nodes.get('stats-rhythm-detail').textContent,/4.*message/); assert.match(h.nodes.get('stats-rhythm-detail').textContent,/UTC/);
  const zero=cells.find(node=>node.dataset.weekday==='6'&&node.dataset.hour==='23'); assert.match(zero.getAttribute('aria-label'),/0.*message/);
  assert.equal(h.nodes.get('stats-rhythm-rows').children.length,7); assert.match(h.nodes.get('stats-rhythm-rows').children[0].textContent,/Sun.*6/);
});
await check('heatmap keyboard navigation has one tab stop and stays within its weekday and hour boundaries',async()=>{
  const h=harness(); h.api.renderStats(snapshot()); const cells=descendants(h.nodes.get('stats-heatmap')).filter(node=>node.tagName==='BUTTON');
  const at=(day,hour)=>cells.find(node=>node.dataset.weekday===String(day)&&node.dataset.hour===String(hour));
  at(0,8).focus(); at(0,8).dispatch('keydown',{key:'ArrowRight'}); assert.equal(h.document.activeElement,at(0,9));
  at(0,9).dispatch('keydown',{key:'ArrowDown'}); assert.equal(h.document.activeElement,at(1,9));
  at(1,9).dispatch('keydown',{key:'Home'}); assert.equal(h.document.activeElement,at(1,0));
  at(1,0).dispatch('keydown',{key:'ArrowLeft'}); assert.equal(h.document.activeElement,at(1,0));
  at(1,0).dispatch('keydown',{key:'End'}); assert.equal(h.document.activeElement,at(1,23));
  at(1,23).dispatch('keydown',{key:'ArrowRight'}); assert.equal(h.document.activeElement,at(1,23));
  assert.equal(cells.filter(node=>node.tabIndex===0).length,1); assert.equal(at(1,23).tabIndex,0);
});
await check('activity chart has keyboard period selection and changes style without a network request',async()=>{
  const h=harness(); const value={...snapshot(9),daily:[{date:'2026-10-09',messages:2},{date:'2026-10-10',messages:3},{date:'2026-10-11',messages:4}]}; h.api.renderStats(value);
  const periods=()=>descendants(h.nodes.get('stats-chart')).filter(node=>node.dataset.periodIndex!==undefined);
  assert.equal(periods().length,3); assert.equal(periods().filter(node=>node.tabIndex===0).length,1);
  const first=periods()[0]; first.focus(); first.dispatch('keydown',{key:'ArrowRight'}); assert.equal(h.document.activeElement,periods()[1]); assert.match(h.nodes.get('stats-chart-detail').textContent,/3.*message/);
  periods()[1].dispatch('keydown',{key:'End'}); assert.equal(h.document.activeElement,periods()[2]); assert.match(h.nodes.get('stats-chart-detail').textContent,/4.*message/);
  h.api.setStatsChartMode('line'); assert.equal(h.api.state().statsChartMode,'line'); assert.equal(h.requests.length,0); assert.equal(periods().length,3);
  assert.ok(descendants(h.nodes.get('stats-chart')).some(node=>node.tagName==='PATH' || node.tagName==='POLYLINE'),'line mode renders a continuous series');
  h.api.setStatsChartMode('bars'); assert.equal(h.api.state().statsChartMode,'bars');
});
await check('same-data poll preserves chart and rhythm focus even when sampling time changes',async()=>{
  const h=harness(); const value=snapshot(); h.api.renderStats(value); const selected=descendants(h.nodes.get('stats-heatmap')).find(node=>node.tagName==='BUTTON'&&node.dataset.hour==='9'); selected.focus();
  h.api.renderStats({...value,sampled_at:value.sampled_at+5,archive_revision:2}); assert.equal(h.document.activeElement.dataset.weekday,'0'); assert.equal(h.document.activeElement.dataset.hour,'9'); assert.ok(h.nodes.get('stats-heatmap').contains(h.document.activeElement));
  const period=descendants(h.nodes.get('stats-chart')).find(node=>node.dataset.periodIndex==='0'); period.focus(); h.api.renderStats({...value,sampled_at:value.sampled_at+10,archive_revision:3}); assert.equal(h.document.activeElement.dataset.periodIndex,'0'); assert.ok(h.nodes.get('stats-chart').contains(h.document.activeElement));
});
await check('changed live counts preserve the focused hour or activity period',async()=>{
  const h=harness(); h.api.renderStats(snapshot());
  descendants(h.nodes.get('stats-heatmap')).find(node=>node.tagName==='BUTTON'&&node.dataset.weekday==='0'&&node.dataset.hour==='8').focus();
  h.api.renderStats({...snapshot(7),archive_revision:4});
  assert.equal(h.document.activeElement.dataset.weekday,'0'); assert.equal(h.document.activeElement.dataset.hour,'8'); assert.ok(h.nodes.get('stats-heatmap').contains(h.document.activeElement)); assert.match(h.nodes.get('stats-rhythm-detail').textContent,/5.*message/);
  descendants(h.nodes.get('stats-chart')).find(node=>node.dataset.periodIndex==='0').focus();
  h.api.renderStats({...snapshot(8),archive_revision:5});
  assert.equal(h.document.activeElement.dataset.periodIndex,'0'); assert.ok(h.nodes.get('stats-chart').contains(h.document.activeElement)); assert.match(h.nodes.get('stats-chart-detail').textContent,/8.*message/);
});
await check('interesting highlights use observed active UTC dates rather than imply complete-history statistics',async()=>{
  const h=harness(); h.api.renderStats({...snapshot(18),daily:[{date:'2026-10-08',messages:4},{date:'2026-10-09',messages:10},{date:'2026-10-11',messages:4}],insights:{active_days:3,longest_streak_days:2,busiest_date:'2026-10-09',busiest_date_messages:10}});
  assert.equal(h.nodes.get('stats-active-days').textContent,'3'); assert.equal(h.nodes.get('stats-streak').textContent,'2 days'); assert.equal(h.nodes.get('stats-daily-average').textContent,'6'); assert.match(h.nodes.get('stats-busiest-day').textContent,/9/); assert.match(h.nodes.get('stats-busiest-detail').textContent,/10.*message/);
  assert.doesNotMatch(h.nodes.get('stats-daily-average').textContent,/NaN|Infinity/);
});
await check('new Stats response fields are validated before any private aggregates can render',async()=>{
  const malformed=[
    value=>({...value,insights:{...value.insights,active_days:-1}}),
    value=>({...value,rhythm:[{weekday:7,hour:8,messages:1}]}),
    value=>({...value,rhythm:[{weekday:0,hour:24,messages:1}]}),
    value=>({...value,rhythm:[{weekday:0,hour:8,messages:-1}]}),
    value=>({...value,rhythm:[{weekday:0,hour:8,messages:1},{weekday:0,hour:8,messages:2}]}),
    value=>({...value,rhythm:Array.from({length:169},()=>({weekday:0,hour:8,messages:1}))}),
    value=>({...value,breakdown:{...value.breakdown,page:0}}),
    value=>({...value,breakdown:{...value.breakdown,total:-1}}),
    value=>({...value,breakdown:{...value.breakdown,rows:Array.from({length:11},()=>value.breakdown.rows[0])}}),
    value=>({...value,breakdown:{...value.breakdown,rows:[{...value.breakdown.rows[0],messages:-1}]}}),
  ];
  for (const corrupt of malformed) {
    const h=harness(); h.api.renderStats(snapshot(6,'OLD_PRIVATE_SYNTHETIC_NAME')); const pending=h.api.refreshStats(); h.requests[0].resolve(corrupt(snapshot(9,'MALFORMED_PRIVATE_SYNTHETIC_NAME'))); await pending;
    assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0); assert.equal(h.nodes.get('stats-heatmap').children.length,0); assert.equal(h.nodes.get('stats-active-days').textContent,'—'); assert.match(h.nodes.get('stats-feedback').textContent,/Refresh stats/);
    assert.doesNotMatch(h.nodes.get('stats-rhythm-detail').textContent,/\d+ messages/); assert.doesNotMatch(h.nodes.get('stats-chart-detail').textContent,/\d+ messages/);
  }
});
await check('Stats rejects response metadata or count relationships that disagree with the requested view',async()=>{
  const malformed=[
    value=>({...value,breakdown:{...value.breakdown,dimension:'people'}}),
    value=>({...value,breakdown:{...value.breakdown,sort:'name'}}),
    value=>({...value,breakdown:{...value.breakdown,page_size:25}}),
    value=>({...value,sent:value.messages+1}),
    value=>({...value,breakdown:{...value.breakdown,rows:[{...value.breakdown.rows[0],sent:value.breakdown.rows[0].messages+1}]}}),
    value=>({...value,insights:{...value.insights,longest_streak_days:value.insights.active_days+1}}),
    value=>({...value,insights:{...value.insights,busiest_date_messages:value.messages+1}}),
    value=>({...value,rhythm:[{weekday:0,hour:8,messages:value.messages+1}]}),
  ];
  for (const corrupt of malformed) {
    const h=harness(); h.api.renderStats(snapshot(6,'PRIOR_SYNTHETIC_NAME')); const pending=h.api.refreshStats(); h.requests[0].resolve(corrupt(snapshot(9,'INCONSISTENT_SYNTHETIC_NAME'))); await pending;
    assert.equal(h.nodes.get('stats-message-count').textContent,'—'); assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0); assert.equal(h.nodes.get('stats-heatmap').children.length,0); assert.match(h.nodes.get('stats-feedback').textContent,/Refresh stats/);
  }
});
await check('readable pre-epoch and epoch dates stay visible in charts, totals and aggregate rows',async()=>{
  for (const [date,seconds] of [['1969-12-31',-1],['1800-01-01',Date.parse('1800-01-01T12:00:00Z')/1000],['1970-01-01',0]]) {
    const h=harness(),value=snapshot(); value.last_message_at=seconds; value.first_message_at=seconds; value.daily=[{date,messages:value.messages}]; value.insights.busiest_date=date; value.breakdown.rows=value.breakdown.rows.map(row=>({...row,last_message_at:seconds}));
    const pending=h.api.refreshStats(); h.requests[0].resolve(value); await pending;
    const localDate=new Intl.DateTimeFormat(undefined,{dateStyle:'medium',timeStyle:'short'}).format(new Date(seconds*1000));
    assert.equal(h.nodes.get('stats-message-count').textContent,'6'); assert.equal(h.nodes.get('stats-latest-message').textContent,localDate); assert.ok(h.nodes.get('stats-aggregate-rows').textContent.includes(localDate)); assert.equal(h.nodes.get('stats-activity-rows').children[0].children[0].textContent,date); assert.doesNotMatch(h.nodes.get('stats-feedback').textContent,/could not|unavailable/);
  }
});
await check('pagination restores the useful page control after loading without stealing a newer focus choice',async()=>{
  for (const [response,total,expected] of [[2,23,'stats-page-next'],[2,12,'stats-page-prev']]) {
    const h=harness(); h.api.renderStats(pageSnapshot()); const next=h.nodes.get('stats-page-next'); next.focus(); next.click(); h.document.activeElement=null;
    h.requests[0].resolve(pageSnapshot(response,total)); await flush();
    assert.equal(h.document.activeElement,h.nodes.get(expected));
  }
  const h=harness(); h.api.renderStats(pageSnapshot()); h.nodes.get('stats-page-next').focus(); h.nodes.get('stats-page-next').click(); const newerFocus=h.nodes.get('stats-chart-line'); newerFocus.focus(); h.requests[0].resolve(pageSnapshot(2)); await flush();
  assert.equal(h.document.activeElement,newerFocus);
});
await check('missing optional rich Stats fields clear old highlights, rhythm details and aggregate names',async()=>{
  const h=harness(); h.api.renderStats(snapshot(6,'OLD_OPTIONAL_SYNTHETIC_NAME')); const selected=descendants(h.nodes.get('stats-heatmap')).find(node=>node.tagName==='BUTTON'&&node.dataset.hour==='8'); selected.focus();
  const value=snapshot(9); delete value.insights; delete value.rhythm; delete value.breakdown; const pending=h.api.refreshStats(); h.requests[0].resolve(value); await pending;
  assert.equal(h.nodes.get('stats-message-count').textContent,'9'); assert.equal(h.nodes.get('stats-active-days').textContent,'—'); assert.equal(h.nodes.get('stats-streak').textContent,'—'); assert.equal(h.nodes.get('stats-busiest-day').textContent,'—'); assert.equal(h.nodes.get('stats-daily-average').textContent,'—'); assert.equal(descendants(h.nodes.get('stats-heatmap')).filter(node=>node.tagName==='BUTTON').length,0); assert.equal(h.nodes.get('stats-rhythm-rows').children.length,0); assert.doesNotMatch(h.nodes.get('stats-rhythm-detail').textContent,/4 messages/); assert.equal(h.nodes.get('stats-aggregate-rows').children.length,0); assert.equal(h.nodes.get('stats-page-prev').disabled,true); assert.equal(h.nodes.get('stats-page-next').disabled,true);
});
console.log(JSON.stringify({ok:true,tests:cases.length,cases,app_js_sha256:sourceSHA,scope:'actual JavaScript with minimal synthetic DOM; no browser layout proof'},null,2));
