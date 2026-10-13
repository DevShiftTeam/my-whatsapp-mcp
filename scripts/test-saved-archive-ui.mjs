import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import crypto from 'node:crypto';

// Evaluate the shipped script with startup suppressed and synthetic browser
// interfaces. This checks requests, authorization races and accessible controls,
// not native DOM, browser geometry or layout.
const source = fs.readFileSync(new URL('../internal/localui/assets/app.js', import.meta.url), 'utf8');
const html = fs.readFileSync(new URL('../internal/localui/assets/index.html', import.meta.url), 'utf8');
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
    this.scrollTop = 0;
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
  get id() { return this.attributes.id ?? ''; }
  set id(value) { this.attributes.id = String(value); }
  get options() { return this.children.filter(child => child.tagName === 'OPTION'); }
  get selectedIndex() { return this.options.findIndex(option => option.value === this.value); }
  get parentElement() { return this.parent ?? new FakeNode(); }
  get tabIndex() { return Number(this.attributes.tabindex ?? (['BUTTON','SELECT','INPUT','TEXTAREA','SUMMARY'].includes(this.tagName) ? 0 : -1)); }
  set tabIndex(value) { this.attributes.tabindex = String(value); }
  get nextElementSibling() { return this.parent?.children[this.parent.children.indexOf(this)+1] ?? null; }
  append(...nodes) { for (const node of nodes) { const child = typeof node === 'string' ? textNode(node) : node; if(child.parent) child.parent.children.splice(child.parent.children.indexOf(child),1); child.parent = this; this.children.push(child); } }
  insertBefore(node,before) { if(node===before)return node; if(node.parent){if(node.contains(node.ownerDocument?.activeElement))node.ownerDocument.activeElement=null;node.parent.children.splice(node.parent.children.indexOf(node),1);} const index=this.children.indexOf(before); node.parent=this; this.children.splice(index<0 ? this.children.length : index,0,node); return node; }
  remove() { if(this.parent){this.parent.children.splice(this.parent.children.indexOf(this),1);this.parent=null;} if(this.contains(this.ownerDocument?.activeElement))this.ownerDocument.activeElement=null; }
  replaceChildren(...nodes) { for(const child of [...this.children])child.remove(); this._text = ''; this.append(...nodes); }
  getBoundingClientRect() { const top=this.parent?.tagName==='OL' ? this.parent.children.indexOf(this)*72-this.parent.scrollTop : 0; return {top,bottom:top+72}; }
  setAttribute(key,value) { this.attributes[key] = String(value); if (key.startsWith('data-')) this.dataset[key.slice(5).replace(/-([a-z])/g,(_,char)=>char.toUpperCase())]=value; }
  removeAttribute(key) { delete this.attributes[key]; }
  getAttribute(key) { return this.attributes[key] ?? null; }
  addEventListener(name,callback) { if (!this.listeners.has(name)) this.listeners.set(name,[]); this.listeners.get(name).push(callback); }
  querySelector(selector) { return this.querySelectorAll(selector)[0] ?? null; }
  querySelectorAll(selector) { return descendants(this).filter(node => matches(node,selector)); }
  contains(node) { return this===node || descendants(this).includes(node); }
  focus() { if (this.ownerDocument) this.ownerDocument.activeElement = this; this.dispatch('focus'); }
  scrollIntoView() {}
  closest() { return this.parentElement; }
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
function harness(clock = null) {
  const nodes = new Map([...html.matchAll(/<([a-z][a-z0-9]*)\b[^>]*\bid="([^"]+)"/gi)].map(match => [match[2],new FakeNode(match[1])]));
  // Parent-owned markup contract; retain the seam while the markup lands in parallel.
  for(const id of ['archive-update','archive-update-text'])if(!nodes.has(id))nodes.set(id,new FakeNode());
  nodes.get('archive-update').hidden=true;
  const selectors = new Map([['.skip-link',new FakeNode('a')],['.stats-update',new FakeNode()]]);
  const policyRadios = [...html.matchAll(/<input\b[^>]*name="(date-mode|chat-mode)"[^>]*value="([^"]+)"[^>]*>/g)].map(match => {
    const node=new FakeNode('input'); node.name=match[1]; node.value=match[2]; node.checked=match[0].includes(' checked'); return node;
  });
  const docListeners = new Map();
  const intervals = new Map();
  const timers = new Map();
  const requests = [];
  let nextTimer = 1;
  const document = {
    hidden:false,documentElement:{dataset:{remoteAdmin:'false'}},activeElement:null,
    getElementById:id => { const node=nodes.get(id) ?? descendants(nodes.get('workspace')).find(node=>node.id===id); assert.ok(node || id==='history-import-progress','real or dynamically created node must contain ID '+id); return node ?? null; },
    createElement:tag => { const node=new FakeNode(tag); node.ownerDocument=document; return node; },createElementNS:(_,tag) => { const node=new FakeNode(tag); node.ownerDocument=document; return node; },createTextNode:textNode,
    querySelector:selector => { const radio=selector.match(/^input\[name=(date-mode|chat-mode)\](?::checked|\[value=['"]([^'"]+)['"]\])$/); if (radio) return policyRadios.find(node=>node.name===radio[1]&&(radio[2] ? node.value===radio[2] : node.checked)); if (!selectors.has(selector)) selectors.set(selector,new FakeNode()); return selectors.get(selector); },
    querySelectorAll:selector => selector === 'input[name=date-mode], input[name=chat-mode]' ? policyRadios : [],
    addEventListener:(name,callback) => { if (!docListeners.has(name)) docListeners.set(name,[]); docListeners.get(name).push(callback); },
  };
  for (const node of nodes.values()) node.ownerDocument=document;
  for (const id of ['progress-link','progress-choose','progress-import']) { const marker=new FakeNode('span'); marker.className='step-marker'; nodes.get(id).append(marker); }
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
    setTimeout:(callback,delay) => { callback.timerDelay=delay; const id=nextTimer++; timers.set(id,callback); return id; },clearTimeout:id => timers.delete(id),
    setInterval:callback => { const id=nextTimer++; intervals.set(id,callback); return id; },clearInterval:id => intervals.delete(id),
  };
  const fetch = (path,options={}) => new Promise((resolve,reject) => { requests.push({path,options,resolve:(value,status=200) => resolve({ok:status>=200&&status<300,status,json:async()=>value}),resolveJSON:json => resolve({ok:true,status:200,json}),reject}); });
  const expose = `  globalThis.__test = {requestAPI:api,refreshVoiceRecovery,retryVoiceDownloads,stopVoiceRecovery,runSearch,showContext,addMessage,initializeWorkspace,clearResults,renderArchiveSource,invalidateSavedChats,loadSavedChats,openSettings,savePolicy,refreshStatus,refreshConnections,refreshStats,resetStats,selectWorkspaceView,changeStatsFilters,statsFilter,invalidateStats,renderStats,renderStatsChart,renderStatsStatus,renderStatsProgress,renderStatsRhythm,renderStatsBreakdown,changeStatsBreakdown,setStatsChartMode,expireAuth,startPolling,stopPolling,
     state:()=>({activeView,statsSnapshot,statsVersion,statsController,statsReloadQueued,authExpired,authEpoch,statsDimension,statsPage,statsPageSize,statsSort,statsChartMode,searching,searchVersion,searchRequest,nextCursor,archiveRefreshNeeded,savedChats,archiveSourceVersion,savedChatRefreshNeeded,selectedMessage,visibleMessages}),
    setStatus:value=>{status=value;},setSetup:value=>{setup=value;},setChats:value=>{chats=value;},setView:value=>{activeView=value;},setCSRF:value=>{csrf=value;}};\n})();`;
  const ClockDate = clock ? class extends Date { static now() { return clock.now; } } : Date;
  const context = vm.createContext({document,window:fakeWindow,fetch,AbortController,Option:option,Date:ClockDate,Intl,URLSearchParams,crypto:crypto.webcrypto,localStorage:{getItem(){return '';},removeItem(){},setItem(){}},navigator:{clipboard:{}},location:{hash:'',pathname:'/'},matchMedia:()=>({matches:false}),history:{replaceState(){}},console});
  vm.runInContext(source.replace(ending,expose),context,{timeout:5000});
  assert.equal(requests.length,0,'test loading must not start network requests');
  const api = context.__test;
  api.setSetup({configured:true,Policy:{ChatMode:'all'}});
  api.setStatus({Connection:'connected',Messages:6,Keyword:{State:'ready',Complete:6,Pending:0,Failed:0},Media:{State:'ready',Complete:1,Pending:1,Failed:0},Semantic:{State:'not_configured'},Codes:[]});
  return {api,nodes,document,requests,intervals,timers,policyRadios,fire:name => { for(const callback of docListeners.get(name) ?? []) callback(); }};
}
const flush = () => new Promise(resolve => setImmediate(resolve));
const cases=[];
const hit=(id='synthetic-live',text='Hello שלום')=>({MessageID:id,ChatName:'Synthetic conversation',SenderName:'Synthetic sender',Snippet:text,SentAt:'2026-10-13T12:00:00Z',Kind:'text'});
const savedPage=(hits=[hit()],cursor='')=>({Hits:hits,NextCursor:cursor,Snapshot:{},saved_view:{source:'saved',historical:{captured_at:'2026-09-01T12:00:00Z'},coverage:'partial',current_phone_state:'unknown'}});
const body=request=>JSON.parse(request.options.body);
async function check(name,fn){await fn();cases.push(name);}
function fireRefreshTimers(h){for(const [id,callback] of [...h.timers]) if(callback.timerDelay===1000){h.timers.delete(id);callback();}}
async function browse(h,page=savedPage()){h.api.setView('archive');const pending=h.api.runSearch();h.requests.at(-1).resolve(page);await pending;}
const pageHits=(page)=>Array.from({length:50},(_,index)=>hit('synthetic-'+((page-1)*50+index),'Synthetic message '+((page-1)*50+index)));
async function loadPages(h,count){await browse(h,savedPage(pageHits(1),'s1.page-2'));for(let page=2;page<=count;page++){const pending=h.api.runSearch(true);h.requests.at(-1).resolve(savedPage(pageHits(page),'s1.page-'+(page+1)));await pending;}}
async function openConversation(h,id='synthetic-60'){const button=h.nodes.get('search-results').querySelectorAll('button').find(node=>node.dataset.messageId===id);const pending=h.api.showContext(hit(id),button);h.requests.at(-1).resolve({Messages:[{ID:id,SenderName:'Synthetic sender',Body:'Kept conversation',Kind:'text'}]});await pending;return button;}
await check('ordinary refresh retains selection, context, focused nodes, filters and every loaded page atomically',async()=>{
 const h=harness();h.nodes.get('search-chat').value='synthetic-chat';await loadPages(h,2);const button=await openConversation(h);button.focus();h.nodes.get('search-results').scrollTop=4320;h.nodes.get('conversation-messages').scrollTop=30;const contextNode=h.nodes.get('conversation-messages').children[0];
 const pending=h.api.runSearch(false,true);assert.equal(h.nodes.get('search-results').children.length,100);assert.equal(h.nodes.get('context-copy').hidden,false);
 h.requests.at(-1).resolve(savedPage(pageHits(1),'s1.fresh-2'));await flush();assert.equal(h.nodes.get('search-results').children.length,100,'first rebuilt page must not publish');assert.equal(h.nodes.get('archive-update').hidden,false);assert.equal(h.nodes.get('archive-update-text').textContent,'Updating…');assert.equal(body(h.requests.at(-1)).cursor,'s1.fresh-2');h.requests.at(-1).resolve(savedPage(pageHits(2),'s1.fresh-3'));await flush();
 assert.equal(h.requests.at(-1).path,'/api/context');h.requests.at(-1).resolve({Messages:[{ID:'synthetic-60',SenderName:'Synthetic sender',Body:'Kept conversation',Kind:'text'}]});await pending;
 assert.equal(h.nodes.get('search-results').children.length,100);assert.equal(h.api.state().nextCursor,'s1.fresh-3');assert.equal(h.api.state().selectedMessage,'synthetic-60');assert.equal(button.getAttribute('aria-pressed'),'true');assert.equal(h.document.activeElement,button);assert.ok(h.nodes.get('search-results').contains(button));assert.equal(h.nodes.get('search-results').scrollTop,4320);assert.equal(h.nodes.get('conversation-messages').scrollTop,30);assert.equal(h.nodes.get('conversation-messages').children[0],contextNode);assert.equal(h.nodes.get('search-chat').value,'synthetic-chat');assert.equal(h.nodes.get('archive-update').hidden,true);
});
await check('initial loader hides on success and transient refresh failure retains the reading state',async()=>{
 const h=harness();h.api.setView('archive');const initial=h.api.runSearch();assert.equal(h.nodes.get('archive-update-text').textContent,'Loading messages…');h.requests.at(-1).resolve(savedPage());await initial;const button=await openConversation(h,'synthetic-live');button.focus();const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve({code:'proxy_unavailable'},502);await pending;
 assert.equal(h.nodes.get('archive-update').hidden,true);assert.equal(h.nodes.get('archive-update-text').textContent,'');assert.equal(h.nodes.get('search-results').children.length,1);assert.equal(h.api.state().selectedMessage,'synthetic-live');assert.match(h.nodes.get('conversation-messages').textContent,/Kept conversation/);assert.equal(h.document.activeElement,button);assert.match(h.nodes.get('search-feedback').textContent,/kept|last.*update/i);
});
await check('stale continuation rebuilds the accumulated range before publishing any replacement',async()=>{
 const h=harness();await loadPages(h,2);const button=await openConversation(h);button.focus();const pending=h.api.runSearch(true);h.requests.at(-1).resolve({code:'saved_cursor_stale'},409);await flush();assert.equal(body(h.requests.at(-1)).cursor,'');h.requests.at(-1).resolve(savedPage(pageHits(1),'s1.retry-2'));await flush();assert.equal(h.nodes.get('search-results').children.length,100);h.requests.at(-1).resolve(savedPage(pageHits(2),'s1.retry-3'));await flush();assert.equal(h.nodes.get('search-results').children.length,100);h.requests.at(-1).resolve(savedPage(pageHits(3),'s1.retry-4'));await flush();h.requests.at(-1).resolve({Messages:[{ID:'synthetic-60',SenderName:'Synthetic sender',Body:'Kept conversation',Kind:'text'}]});await pending;
 assert.equal(h.nodes.get('search-results').children.length,150);assert.equal(h.api.state().selectedMessage,'synthetic-60');assert.equal(h.document.activeElement,button);assert.equal(h.api.state().nextCursor,'s1.retry-4');
});
await check('failed rebuilt suffix and repeated stale cursors never publish a partial range or clear context',async()=>{
 for(const failure of ['archive_busy','saved_cursor_stale']){const h=harness();await loadPages(h,2);await openConversation(h);const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage([hit('new','Unpublished new row')],'s1.fresh-2'));await flush();h.requests.at(-1).resolve({code:failure},failure==='archive_busy'?503:409);await flush();if(failure==='archive_busy'){fireRefreshTimers(h);await flush();}h.requests.at(-1).resolve({code:failure},failure==='archive_busy'?503:409);await pending;await flush();assert.equal(h.nodes.get('search-results').children.length,100);assert.doesNotMatch(h.nodes.get('search-results').textContent,/Unpublished/);assert.match(h.nodes.get('conversation-messages').textContent,/Kept conversation/);assert.equal(h.nodes.get('archive-update').hidden,true);const count=h.requests.length;fireRefreshTimers(h);assert.equal(h.requests.length,count);}
});
await check('deep background views show an honest stale hint without silently truncating or unbounded reads',async()=>{
 const h=harness();await loadPages(h,5);const button=await openConversation(h);button.focus();const count=h.requests.length;await h.api.runSearch(false,true);assert.equal(h.requests.length,count);assert.equal(h.nodes.get('search-results').children.length,250);assert.equal(h.document.activeElement,button);assert.match(h.nodes.get('search-feedback').textContent,/kept.*(?:search|submit)|(?:search|submit).*kept/i);assert.match(h.nodes.get('search-feedback').textContent,/changed|not.*updated|older/i);assert.equal(h.nodes.get('archive-update').hidden,true);
});
await check('explicit successful query transition resets context but not before replacement succeeds',async()=>{
 const h=harness();await browse(h);await openConversation(h,'synthetic-live');h.nodes.get('search-query').value='new query';const pending=h.api.runSearch();assert.match(h.nodes.get('conversation-messages').textContent,/Kept conversation/);h.requests.at(-1).resolve(savedPage([hit('new')]));await pending;assert.equal(h.api.state().selectedMessage,'');assert.equal(h.nodes.get('conversation-messages').children.length,0);
});
await check('malformed successful archive pages cannot silently replace the accumulated range',async()=>{
 const h=harness();await loadPages(h,2);const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve({Hits:'not a page',NextCursor:''});await pending;assert.equal(h.nodes.get('search-results').children.length,100);assert.match(h.nodes.get('search-feedback').textContent,/could not be read|invalid/i);assert.equal(h.nodes.get('archive-update').hidden,true);
});
await check('changed rows keep their keyed button, fresh click data and scroll anchor when reordered',async()=>{
 const h=harness();await browse(h,savedPage([hit('a','First'),hit('b','Second'),hit('c','Third')]));const button=await openConversation(h,'b');button.focus();const list=h.nodes.get('search-results');list.scrollTop=72;const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage([hit('b','Updated second'),hit('new','New arrival'),hit('a','First'),hit('c','Third')]));await flush();h.requests.at(-1).resolve({Messages:[{ID:'b',SenderName:'Synthetic sender',Body:'Kept conversation',Kind:'text'}]});await pending;
 assert.ok(h.document.activeElement===button,'keyed reordering must retain focus');assert.ok(list.children[0].querySelector('button')===button);assert.match(button.textContent,/Updated second/);assert.equal(list.scrollTop,0,'the previously visible row remains at the same reading position');button.click();assert.equal(body(h.requests.at(-1)).message_id,'b');h.requests.at(-1).resolve({Messages:[]});await flush();
});
await check('refresh source failure during context revalidation clears all staged and displayed content',async()=>{
 const h=harness();await loadPages(h,2);await openConversation(h);const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage(pageHits(1),'s1.guarded-2'));await flush();h.requests.at(-1).resolve(savedPage(pageHits(2),'s1.guarded-3'));await flush();const context=h.requests.at(-1);assert.equal(context.path,'/api/context');const failed=h.api.requestAPI('/api/stats','POST',{source:'saved'});h.requests.at(-1).resolve({code:'historical_source_unavailable'},503);await assert.rejects(failed);assert.equal(h.nodes.get('search-results').children.length,0);assert.equal(h.nodes.get('conversation-messages').children.length,0);assert.equal(h.nodes.get('archive-update').hidden,true);context.resolve({Messages:[{ID:'synthetic-60',Body:'LATE_SYNTHETIC_BODY'}]});await pending;assert.equal(h.nodes.get('search-results').children.length,0);assert.doesNotMatch(h.nodes.get('conversation-messages').textContent,/LATE_SYNTHETIC_BODY/);
});
await check('newer query and selection cannot be overwritten by a pending rebuilt range or context',async()=>{
 const h=harness();await loadPages(h,2);await openConversation(h);const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage(pageHits(1),'s1.pending-2'));await flush();const suffix=h.requests.at(-1);h.nodes.get('search-query').value='new query';const newer=h.api.runSearch();const current=h.requests.at(-1);assert.equal(suffix.options.signal.aborted,true);suffix.resolve(savedPage(pageHits(2),'s1.old-3'));await pending;assert.equal(h.nodes.get('search-results').children.length,100);assert.equal(h.nodes.get('archive-update').hidden,false);current.resolve(savedPage([hit('new-query'),hit('other')]));await newer;assert.equal(h.nodes.get('search-results').children.length,2);assert.equal(h.api.state().selectedMessage,'');
 await openConversation(h,'new-query');const refresh=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage([hit('new-query'),hit('other')]));await flush();const oldContext=h.requests.at(-1);assert.equal(oldContext.path,'/api/context');await openConversation(h,'other');oldContext.resolve({Messages:[{ID:'new-query',Body:'STALE_SYNTHETIC_SELECTION'}]});await refresh;assert.equal(h.api.state().selectedMessage,'other');assert.match(h.nodes.get('conversation-messages').textContent,/Kept conversation/);assert.doesNotMatch(h.nodes.get('conversation-messages').textContent,/STALE_SYNTHETIC_SELECTION/);
});
await check('a range batch has one total deadline and never publishes a body delivered after timeout',async()=>{
 const h=harness();await loadPages(h,2);const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage(pageHits(1),'s1.slow-2'));await flush();const suffix=h.requests.at(-1);const deadlines=[...h.timers.values()].filter(fn=>fn.timerDelay===15000);assert.equal(deadlines.length,1);deadlines[0]();assert.equal(suffix.options.signal.aborted,true);suffix.resolve(savedPage([hit('late','LATE_SYNTHETIC_ROW')]));await pending;assert.equal(h.nodes.get('search-results').children.length,100);assert.doesNotMatch(h.nodes.get('search-results').textContent,/LATE_SYNTHETIC_ROW/);assert.equal(h.nodes.get('archive-update').hidden,true);assert.match(h.nodes.get('search-feedback').textContent,/too long/);
});
await check('refresh preserves the selected excerpt when several search rows share a message handle',async()=>{
 const h=harness();const hits=[{...hit('shared'),DocumentID:'synthetic-document-a'},{...hit('shared'),DocumentID:'synthetic-document-b'}];await browse(h,savedPage(hits));const buttons=h.nodes.get('search-results').querySelectorAll('button');const opening=h.api.showContext(hits[1],buttons[1]);h.requests.at(-1).resolve({Messages:[{ID:'shared',Body:'Synthetic shared message',Kind:'text'}]});await opening;const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage(hits));await flush();h.requests.at(-1).resolve({Messages:[{ID:'shared',Body:'Synthetic shared message',Kind:'text'}]});await pending;assert.equal(buttons[0].getAttribute('aria-pressed'),'false');assert.equal(buttons[1].getAttribute('aria-pressed'),'true');
});
await check('four-page revalidation retries one stale context atomically within a bounded read budget',async()=>{
 const h=harness();await loadPages(h,4);await openConversation(h);const before=h.requests.length,pending=h.api.runSearch(false,true);for(let page=1;page<=4;page++){h.requests.at(-1).resolve({...savedPage(pageHits(page),'s1.first-'+(page+1)),Snapshot:{ArchiveRevision:2}});await flush();}assert.equal(h.requests.at(-1).path,'/api/context');h.requests.at(-1).resolve({Messages:[{ID:'synthetic-60',Body:'Unpublished context'}],Snapshot:{ArchiveRevision:3}});await flush();assert.equal(h.nodes.get('search-results').children.length,200);for(let page=1;page<=4;page++){h.requests.at(-1).resolve({...savedPage(pageHits(page),'s1.final-'+(page+1)),Snapshot:{ArchiveRevision:3}});await flush();}assert.equal(h.requests.at(-1).path,'/api/context');h.requests.at(-1).resolve({Messages:[{ID:'synthetic-60',Body:'Fresh guarded context',Kind:'text'}],Snapshot:{ArchiveRevision:3}});await pending;assert.equal(h.requests.length-before,10);assert.equal(h.nodes.get('search-results').children.length,200);assert.match(h.nodes.get('conversation-messages').textContent,/Fresh guarded context/);assert.doesNotMatch(h.nodes.get('conversation-messages').textContent,/Unpublished/);
});
await check('known scope generations in successful pages fence old content before a range can rebuild',async()=>{
 const h=harness();await browse(h,{...savedPage(pageHits(1),'s1.scope-2'),Snapshot:{PolicyGeneration:1,RecoveryEpoch:0}});const more=h.api.runSearch(true);h.requests.at(-1).resolve({...savedPage(pageHits(2),'s1.scope-3'),Snapshot:{PolicyGeneration:1,RecoveryEpoch:0}});await more;await openConversation(h);h.api.renderStats(snapshot());const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve({...savedPage(pageHits(1),'s1.new-scope'),Snapshot:{PolicyGeneration:2,RecoveryEpoch:0}});await flush();assert.equal(h.nodes.get('search-results').children.length,0);await pending;assert.equal(h.nodes.get('conversation-messages').children.length,0);assert.equal(h.nodes.get('stats-message-count').textContent,'—');assert.equal(h.nodes.get('archive-update').hidden,true);assert.equal(h.api.state().authExpired,false);
});
await check('empty query browses the actual saved reader and renders captured provenance',async()=>{
 const h=harness();h.api.setView('archive');const pending=h.api.runSearch();
 assert.equal(h.requests.at(-1).path,'/api/archive/browse');assert.equal(body(h.requests.at(-1)).source,'saved');assert.equal(body(h.requests.at(-1)).query,undefined);assert.equal(body(h.requests.at(-1)).kind,undefined);
 h.requests.at(-1).resolve(savedPage([hit(),hit('h1.synthetic','Saved voice'),hit('synthetic-third','Third')]));await pending;
 assert.equal(h.nodes.get('search-results').children.length,3);assert.match(h.nodes.get('search-results').textContent,/Captured.*Read-only/);
});
await check('initial workspace loads recent messages without submitting a search',async()=>{
 const h=harness();const opening=h.api.initializeWorkspace();
 h.requests[0].resolve({configured:true,policy:{ChatMode:'all'}});await flush();
 const stats=h.requests.find(r=>r.path==='/api/stats');assert.equal(body(stats).source,'saved');stats.resolve(snapshot());
 h.requests.find(r=>r.path==='/api/status').resolve({Connection:'connected',Messages:6,Codes:[]});await flush();
 h.requests.find(r=>r.path==='/api/pairing').resolve({state:'connected',has_session:true});await flush();
 h.requests.find(r=>r.path==='/api/chats?limit=100').resolve({Chats:[]});await flush();
 h.requests.find(r=>r.path==='/api/chats?source=saved&limit=100').resolve({Chats:[{ID:'synthetic-current-chat',Name:'Synthetic saved'}]});await flush();
 const recent=h.requests.find(r=>r.path==='/api/archive/browse');assert.ok(recent);recent.resolve(savedPage());await opening;
 assert.equal(h.nodes.get('search-results').children.length,1);assert.equal(h.nodes.get('chat-choices').children.length,0);
});
await check('initial saved chat busy or limit keeps the owner workspace and polling alive',async()=>{
 for(const [code,httpStatus] of [['archive_busy',503],['saved_read_limit',409]]){
  const h=harness();const opening=h.api.initializeWorkspace();h.requests[0].resolve({configured:true,policy:{ChatMode:'all'}});await flush();
  h.requests.find(r=>r.path==='/api/stats').resolve(snapshot());h.requests.find(r=>r.path==='/api/status').resolve({Connection:'connected',Messages:6,Codes:[]});await flush();
  h.requests.find(r=>r.path==='/api/pairing').resolve({state:'connected',has_session:true});await flush();h.requests.find(r=>r.path==='/api/chats?limit=100').resolve({Chats:[]});await flush();
  h.requests.find(r=>r.path==='/api/chats?source=saved&limit=100').resolve({code},httpStatus);await flush();h.requests.find(r=>r.path==='/api/archive/browse').resolve(savedPage());await opening;
  assert.equal(h.nodes.get('workspace').hidden,false);assert.equal(h.api.state().authExpired,false);assert.equal(h.intervals.size,1);assert.equal(h.nodes.get('stats-message-count').textContent,'6');assert.match(h.nodes.get('archive-source-note').textContent,/Saved chat choices are unavailable/);
 }
});
await check('initial saved chat genuine 401 expires the owner session',async()=>{
 const h=harness();const opening=h.api.initializeWorkspace();h.requests[0].resolve({configured:true,policy:{ChatMode:'all'}});await flush();
 h.requests.find(r=>r.path==='/api/stats').resolve(snapshot());h.requests.find(r=>r.path==='/api/status').resolve({Connection:'connected',Messages:6,Codes:[]});await flush();
 h.requests.find(r=>r.path==='/api/pairing').resolve({state:'connected',has_session:true});await flush();h.requests.find(r=>r.path==='/api/chats?limit=100').resolve({Chats:[]});await flush();
 h.requests.find(r=>r.path==='/api/chats?source=saved&limit=100').resolve({code:'unauthorized'},401);await opening;
 assert.equal(h.nodes.get('workspace').hidden,true);assert.equal(h.api.state().authExpired,true);assert.equal(h.intervals.size,0);assert.equal(h.nodes.get('stats-message-count').textContent,'—');
});
await check('clearing query browses and filters keep RFC3339 while kind remains Stats only',async()=>{
 const h=harness();await browse(h);h.nodes.get('search-query').value='שלום';h.nodes.get('search-chat').value='synthetic-chat';h.nodes.get('search-from').value='2026-10-01';h.nodes.get('search-until').value='2026-10-13';
 const searching=h.api.runSearch();assert.equal(h.requests.at(-1).path,'/api/search');assert.match(body(h.requests.at(-1)).from,/T.*Z$/);assert.equal(body(h.requests.at(-1)).source,'saved');assert.equal(body(h.requests.at(-1)).chats[0],'synthetic-chat');h.requests.at(-1).resolve(savedPage());await searching;
 h.nodes.get('search-query').value='';h.nodes.get('search-query').dispatch('input');assert.equal(h.requests.at(-1).path,'/api/archive/browse');assert.equal(body(h.requests.at(-1)).query,undefined);assert.equal(body(h.requests.at(-1)).chats[0],'synthetic-chat');h.requests.at(-1).resolve(savedPage());await flush();
 h.nodes.get('stats-range').value='custom';h.nodes.get('stats-from').value='2026-10-01';h.nodes.get('stats-kind').value='voice';const filter=h.api.statsFilter();assert.equal(filter.source,'saved');assert.equal(filter.kind,'voice');assert.equal(typeof filter.from,'number');
});
await check('filter edits replace only after success and ignore a late superseded response',async()=>{
 const h=harness();await browse(h);h.nodes.get('search-query').value='old';const old=h.api.runSearch();const oldRequest=h.requests.at(-1);
 h.nodes.get('search-chat').value='new-chat';h.nodes.get('search-chat').dispatch('change');const newer=h.requests.at(-1);assert.equal(oldRequest.options.signal.aborted,true);
 assert.equal(h.nodes.get('search-results').children.length,1);newer.resolve(savedPage([hit('new','New selection')]));await flush();oldRequest.resolve(savedPage([hit('old','Stale selection')]));await old;
 assert.match(h.nodes.get('search-results').textContent,/New selection/);assert.doesNotMatch(h.nodes.get('search-results').textContent,/Stale selection/);assert.equal(h.api.state().searching,false);
});
await check('ten import polls preserve an in-flight search and coalesce its follow-up',async()=>{
 const h=harness();await browse(h);h.api.setStatus({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:1,ArchiveRevision:1,RecoveryEpoch:0},Codes:[]});
 h.nodes.get('search-query').value='slow';const slow=h.api.runSearch();const request=h.requests.at(-1);
 for(let revision=2;revision<=11;revision++){const poll=h.api.refreshStatus();h.requests.at(-1).resolve({Connection:'connected',Messages:6+revision,Snapshot:{PolicyGeneration:1,ArchiveRevision:revision,RecoveryEpoch:0},Codes:[]});await poll;}
 assert.equal(request.options.signal.aborted,false);assert.equal(h.nodes.get('search-results').children.length,1);assert.equal(h.requests.filter(r=>r.path==='/api/search').length,1);
 request.resolve(savedPage([hit('slow','Guarded response')]));await slow;fireRefreshTimers(h);assert.equal(h.requests.filter(r=>r.path==='/api/search').length,2);h.requests.at(-1).resolve(savedPage());await flush();
});
await check('ten import polls do not abort or blank Stats and queue one follow-up',async()=>{
 const h=harness();h.api.renderStats(snapshot());h.api.setStatus({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:1,ArchiveRevision:1,RecoveryEpoch:0},Codes:[]});const slow=h.api.refreshStats();const request=h.requests.at(-1);
 for(let revision=2;revision<=11;revision++){const poll=h.api.refreshStatus();h.requests.at(-1).resolve({Connection:'connected',Messages:6+revision,Snapshot:{PolicyGeneration:1,ArchiveRevision:revision,RecoveryEpoch:0},Codes:[]});await poll;}
 assert.equal(request.options.signal.aborted,false);assert.equal(h.nodes.get('stats-message-count').textContent,'6');assert.equal(h.requests.filter(r=>r.path==='/api/stats').length,1);
 request.resolve(snapshot(9));await slow;assert.equal(h.requests.filter(r=>r.path==='/api/stats').length,2);assert.equal(h.nodes.get('stats-message-count').textContent,'9');h.requests.at(-1).resolve(snapshot(10));await flush();
});
await check('stale continuation restarts the same saved query once',async()=>{
 const h=harness();h.nodes.get('search-query').value='same';await browse(h,savedPage([hit()],'s1.synthetic'));
 const more=h.api.runSearch(true);assert.equal(body(h.requests.at(-1)).cursor,'s1.synthetic');h.requests.at(-1).resolve({code:'saved_cursor_stale'},409);await flush();
 const restarted=h.requests.at(-1);assert.equal(body(restarted).cursor,'');assert.equal(body(restarted).query,'same');restarted.resolve(savedPage());await more;assert.equal(h.api.state().authExpired,false);
});
await check('busy response retries once and keeps already delivered rows',async()=>{
 const h=harness();await browse(h);const pending=h.api.runSearch();h.requests.at(-1).resolve({code:'archive_busy'},503);await pending;
 assert.equal(h.api.state().authExpired,false);assert.equal(h.nodes.get('search-results').children.length,1);fireRefreshTimers(h);h.requests.at(-1).resolve({code:'archive_busy'},503);await flush();const count=h.requests.length;fireRefreshTimers(h);assert.equal(h.requests.length,count);
});
await check('Stats busy retries once without clearing the last guarded totals',async()=>{
 const h=harness();h.api.renderStats(snapshot());const pending=h.api.refreshStats();h.requests.at(-1).resolve({code:'archive_busy'},503);await pending;assert.equal(h.nodes.get('stats-message-count').textContent,'6');fireRefreshTimers(h);h.requests.at(-1).resolve(snapshot(7));await flush();assert.equal(h.nodes.get('stats-message-count').textContent,'7');
});
await check('owner revocation clears content immediately and late responses cannot restore it',async()=>{
 const h=harness();await browse(h);const pending=h.api.runSearch();const late=h.requests.at(-1);const auth=h.api.requestAPI('/api/status');h.requests.at(-1).resolve({code:'unauthorized'},401);await assert.rejects(auth);
 assert.equal(h.api.state().authExpired,true);assert.equal(h.nodes.get('search-results').children.length,0);late.resolve(savedPage());await pending;assert.equal(h.nodes.get('search-results').children.length,0);
});
await check('policy changes and failed source guards clear displayed content',async()=>{
 const h=harness();await browse(h);h.api.renderStats(snapshot());h.api.setStatus({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:1,RecoveryEpoch:0},Codes:[]});const poll=h.api.refreshStatus();h.requests.at(-1).resolve({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:2,RecoveryEpoch:0},Codes:[]});await poll;assert.equal(h.nodes.get('search-results').children.length,0);assert.equal(h.nodes.get('stats-message-count').textContent,'—');
 await browse(h);const read=h.api.runSearch();h.requests.at(-1).resolve({code:'archive_unavailable'},503);await read;assert.equal(h.nodes.get('search-results').children.length,0);
});
await check('source policy or scoped unauthorized failures clear reads without converting them to owner logout',async()=>{
 for(const [code,httpStatus] of [['policy_changed',409],['unauthorized',403]]){
  const h=harness();await browse(h);h.api.renderStats(snapshot());const read=h.api.runSearch();h.requests.at(-1).resolve({code},httpStatus);await read;
  assert.equal(h.nodes.get('search-results').children.length,0);assert.equal(h.nodes.get('stats-message-count').textContent,'—');assert.equal(h.api.state().authExpired,false);assert.ok(h.nodes.get('search-feedback').classList.contains('error'));assert.ok(h.nodes.get('stats-feedback').classList.contains('error'));if(httpStatus===403) assert.doesNotMatch(h.nodes.get('search-feedback').textContent,/session has ended/);
 }
});
await check('policy narrowing clears chat names and rejects a pending old-scope page',async()=>{
 const h=harness();const loaded=h.api.loadSavedChats();h.requests.at(-1).resolve({Chats:[{ID:'old-chat',Name:'REMOVED_SYNTHETIC_NAME'}]});await loaded;
 h.nodes.get('search-chat').value='old-chat';h.nodes.get('stats-chat').value='old-chat';const pending=h.api.loadSavedChats();const late=h.requests.at(-1);
 h.api.setStatus({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:1,RecoveryEpoch:0},Codes:[]});h.api.setView('archive');const poll=h.api.refreshStatus();h.requests.at(-1).resolve({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:2,RecoveryEpoch:0},Codes:[]});await poll;
 assert.equal(late.options.signal.aborted,true);assert.doesNotMatch(h.nodes.get('search-chat').textContent,/REMOVED_SYNTHETIC_NAME/);assert.doesNotMatch(h.nodes.get('stats-chat').textContent,/REMOVED_SYNTHETIC_NAME/);assert.equal(h.nodes.get('search-chat').value,'');assert.equal(h.nodes.get('stats-chat').value,'');
 late.resolve({Chats:[{ID:'old-chat',Name:'REMOVED_SYNTHETIC_NAME'}]});await pending;assert.doesNotMatch(h.nodes.get('search-chat').textContent,/REMOVED_SYNTHETIC_NAME/);
 const refreshed=h.api.loadSavedChats();h.requests.at(-1).resolve({Chats:[{ID:'allowed-chat',Name:'Allowed current scope'}]});await refreshed;assert.match(h.nodes.get('search-chat').textContent,/Allowed current scope/);
});
await check('ordinary imports keep allowed chat selections until a guarded list replacement',async()=>{
 const h=harness();const initial=h.api.loadSavedChats();h.requests.at(-1).resolve({Chats:[{ID:'kept-chat',Name:'Kept synthetic chat'}]});await initial;h.nodes.get('search-chat').value='kept-chat';h.nodes.get('stats-chat').value='kept-chat';h.api.setView('archive');
 h.api.setStatus({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:1,RecoveryEpoch:0,ArchiveRevision:1},Codes:[]});const poll=h.api.refreshStatus();h.requests.at(-1).resolve({Connection:'connected',Messages:7,Snapshot:{PolicyGeneration:1,RecoveryEpoch:0,ArchiveRevision:2},Codes:[]});await poll;
 assert.equal(h.nodes.get('search-chat').value,'kept-chat');assert.match(h.nodes.get('search-chat').textContent,/Kept synthetic chat/);assert.equal(h.api.state().savedChatRefreshNeeded,true);
 const refreshed=h.api.loadSavedChats();h.requests.at(-1).resolve({Chats:[{ID:'kept-chat',Name:'Kept synthetic chat'},{ID:'new-chat',Name:'New imported synthetic chat'}]});await refreshed;
 assert.equal(h.nodes.get('search-chat').value,'kept-chat');assert.equal(h.nodes.get('stats-chat').value,'kept-chat');assert.match(h.nodes.get('search-chat').textContent,/New imported synthetic chat/);
 const pending=h.api.loadSavedChats();const late=h.requests.at(-1);const source=h.api.requestAPI('/api/context','POST',{source:'saved',message_id:'h1.synthetic'});h.requests.at(-1).resolve({code:'policy_changed'},409);await assert.rejects(source);late.resolve({Chats:[{ID:'kept-chat',Name:'Kept synthetic chat'}]});await pending;assert.doesNotMatch(h.nodes.get('search-chat').textContent,/Kept synthetic chat/);
});
await check('historical context uses typed handles and no current download or send action',async()=>{
 const h=harness();await browse(h);const context=h.api.showContext(hit('h1.synthetic'),h.nodes.get('search-results').querySelector('button'));assert.equal(body(h.requests.at(-1)).source,'saved');assert.equal(body(h.requests.at(-1)).message_id,'h1.synthetic');h.requests.at(-1).resolve({Messages:[{ID:'h1.synthetic',SenderName:'Synthetic sender',Body:'Captured message',Kind:'voice',MediaAvailability:'retained',Derived:[]}],saved_view:savedPage().saved_view});await context;
 assert.match(h.nodes.get('conversation-messages').textContent,/Captured.*Read-only/);assert.match(h.nodes.get('conversation-messages').textContent,/captured original unavailable here/);assert.equal(h.nodes.get('conversation-messages').querySelectorAll('button').length,0);
 assert.ok(h.requests.every(r=>!r.path.includes('download')&&!r.path.includes('send')));
});
await check('current-only saved responses need no configured snapshot',async()=>{
 const h=harness();await browse(h,{Hits:[hit()],NextCursor:'',saved_view:{source:'saved',coverage:'partial',current_phone_state:'unknown'}});assert.equal(h.nodes.get('search-results').children.length,1);assert.doesNotMatch(h.nodes.get('search-results').textContent,/Captured/);assert.match(h.nodes.get('archive-source-note').textContent,/history may be incomplete/);
});
await check('voice recovery polling stops on completion, dialog close and auth changes',async()=>{
 const empty={awaiting_phone:0,waiting_cleanup:0,queued:0,downloading:0,completed:0,failed:0};
 const h=harness();h.nodes.get('models-dialog').open=true;
 let pending=h.api.refreshVoiceRecovery();assert.equal(h.requests.at(-1).path,'/api/voice/recovery');h.requests.at(-1).resolve({...empty,awaiting_phone:1});await pending;
 assert.match(h.nodes.get('voice-retry-feedback').textContent,/waiting for your phone/);assert.equal([...h.timers.values()].filter(fn=>fn.timerDelay===2500).length,1);
 h.api.stopVoiceRecovery();assert.equal(h.timers.size,0);
 pending=h.api.refreshVoiceRecovery();h.requests.at(-1).resolve({...empty,completed:1});await pending;
 assert.match(h.nodes.get('voice-retry-feedback').textContent,/original recording.*Transcription may still be running/);assert.equal(h.timers.size,0);
 pending=h.api.refreshVoiceRecovery();const late=h.requests.at(-1);h.nodes.get('models-dialog').close();h.nodes.get('models-dialog').dispatch('close');late.resolve({...empty,awaiting_phone:7});await pending;
 assert.doesNotMatch(h.nodes.get('voice-retry-feedback').textContent,/7 waiting/);assert.equal(h.timers.size,0);
 h.nodes.get('models-dialog').open=true;pending=h.api.refreshVoiceRecovery();const old=h.requests.at(-1);h.api.expireAuth();old.resolve({...empty,downloading:5});await pending;
 assert.doesNotMatch(h.nodes.get('voice-retry-feedback').textContent,/5 downloading/);assert.equal(h.timers.size,0);
});
await check('voice recovery renders cleanup separately and never treats request acknowledgement as transcript',async()=>{
 const h=harness();h.nodes.get('models-dialog').open=true;
 const pending=h.api.retryVoiceDownloads();assert.equal(h.requests.at(-1).path,'/api/voice/retry');
 h.requests.at(-1).resolve({queued:0,skipped:0,requested:1,recovery:{awaiting_phone:0,waiting_cleanup:1,queued:0,downloading:0,completed:0,failed:0}});await pending;
 assert.match(h.nodes.get('voice-retry-feedback').textContent,/waiting for earlier files to finish cleanup/);assert.doesNotMatch(h.nodes.get('voice-retry-feedback').textContent,/transcript.*complete/i);
 h.nodes.get('models-dialog').close();h.nodes.get('models-dialog').dispatch('close');assert.equal(h.timers.size,0);
});
await check('gone selected context removes every staged row for that message and keeps unrelated rows, chat scope and Stats',async()=>{
 const h=harness();h.api.renderStats(snapshot());h.nodes.get('search-chat').value='synthetic-chat';
 const gone=[{...hit('synthetic-gone','Gone selected row A'),DocumentID:'synthetic-document-a'},{...hit('synthetic-gone','Gone selected row B'),DocumentID:'synthetic-document-b'}];
 const rows=[...gone,hit('synthetic-kept','Kept other row')];
 await browse(h,savedPage(rows));await openConversation(h,'synthetic-gone');
 const pending=h.api.runSearch(false,true);assert.equal(body(h.requests.at(-1)).chats[0],'synthetic-chat');h.requests.at(-1).resolve(savedPage(rows));await flush();
 assert.equal(h.requests.at(-1).path,'/api/context');assert.equal(body(h.requests.at(-1)).message_id,'synthetic-gone');
 h.requests.at(-1).resolve({code:'source_unavailable'},404);await pending;
 assert.equal(h.nodes.get('search-results').children.length,1);assert.doesNotMatch(h.nodes.get('search-results').textContent,/Gone selected row/);assert.match(h.nodes.get('search-results').textContent,/Kept other row/);
 assert.equal(h.api.state().selectedMessage,'');assert.equal(h.nodes.get('conversation-messages').children.length,0);assert.match(h.nodes.get('context-feedback').textContent,/no longer available/);
 assert.equal(h.nodes.get('stats-message-count').textContent,'6');assert.equal(h.nodes.get('archive-update').hidden,true);assert.equal(h.api.state().authExpired,false);
});
await check('gone selection on a deep page keeps the rebuilt range instead of truncating it',async()=>{
 const h=harness();await loadPages(h,2);await openConversation(h,'synthetic-60');
 const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage(pageHits(1),'s1.gone-2'));await flush();h.requests.at(-1).resolve(savedPage(pageHits(2),''));await flush();
 assert.equal(h.requests.at(-1).path,'/api/context');h.requests.at(-1).resolve({code:'source_unavailable'},404);await pending;
 const list=h.nodes.get('search-results');assert.equal(list.children.length,99);assert.doesNotMatch(list.textContent,/Synthetic message 60(?!\d)/);assert.match(list.textContent,/Synthetic message 59(?!\d)/);assert.match(list.textContent,/Synthetic message 61(?!\d)/);
 assert.equal(h.api.state().selectedMessage,'');assert.equal(h.api.state().nextCursor,'');assert.match(h.nodes.get('search-feedback').textContent,/99 saved messages shown/);
});
await check('an explicit result click keeps global clearing for an exact 404 source invalidation',async()=>{
 const h=harness();h.api.renderStats(snapshot());await browse(h,savedPage([hit('synthetic-live','Clicked row'),hit('synthetic-other','Other row')]));
 const button=h.nodes.get('search-results').querySelectorAll('button')[0];const pending=h.api.showContext(hit('synthetic-live','Clicked row'),button);h.requests.at(-1).resolve({code:'source_unavailable'},404);await pending;
 assert.equal(h.nodes.get('search-results').children.length,0);assert.equal(h.nodes.get('stats-message-count').textContent,'—');assert.ok(h.nodes.get('search-feedback').classList.contains('error'));
});
await check('known source invalidations clear reads and fence late responses from the background context and other owner reads',async()=>{
 for(const [code,httpStatus] of [['source_unavailable',503],['historical_source_unavailable',404],['policy_changed',409],['reconciliation_required',409],['unauthorized',403]]){
  const rows=[hit('synthetic-live','Invalidated row'),hit('synthetic-other','Other row')];
  {const h=harness();h.api.renderStats(snapshot());await browse(h,savedPage(rows));await openConversation(h,'synthetic-live');
   const pending=h.api.runSearch(false,true);const late=h.requests.at(-1);const failed=h.api.requestAPI('/api/search','POST',{source:'saved'});h.requests.at(-1).resolve({code},httpStatus);await assert.rejects(failed);
   assert.equal(h.nodes.get('search-results').children.length,0,code);assert.equal(h.nodes.get('conversation-messages').children.length,0,code);assert.equal(h.nodes.get('stats-message-count').textContent,'—',code);assert.ok(h.nodes.get('stats-feedback').classList.contains('error'),code);assert.equal(h.api.state().authExpired,false,code);
   late.resolve(savedPage(rows));await pending;assert.equal(h.nodes.get('search-results').children.length,0,code);assert.doesNotMatch(h.nodes.get('search-results').textContent,/Invalidated row/,code);}
  {const h=harness();h.api.renderStats(snapshot());await browse(h,savedPage(rows));await openConversation(h,'synthetic-live');
   const pending=h.api.runSearch(false,true);h.requests.at(-1).resolve(savedPage(rows));await flush();h.requests.at(-1).resolve({code},httpStatus);await pending;
   assert.equal(h.nodes.get('search-results').children.length,0,code);assert.equal(h.nodes.get('conversation-messages').children.length,0,code);assert.equal(h.nodes.get('stats-message-count').textContent,'—',code);assert.ok(h.nodes.get('search-feedback').classList.contains('error'),code);assert.equal(h.api.state().authExpired,false,code);}
 }
});
await check('ordinary background refresh keeps the submitted chat scope until an invalidation',async()=>{
 const h=harness();h.nodes.get('search-chat').value='synthetic-chat';await browse(h,savedPage([hit('synthetic-live','Scoped row')]));
 const pending=h.api.runSearch(false,true);assert.equal(h.requests.at(-1).path,'/api/archive/browse');assert.deepEqual(body(h.requests.at(-1)).chats,['synthetic-chat']);h.requests.at(-1).resolve(savedPage([hit('synthetic-live','Scoped row')]));await pending;
});
await check('policy invalidation replays only the last submitted query with its chat scope narrowed',async()=>{
 const h=harness();h.api.setView('archive');h.nodes.get('search-chat').value='old-chat';h.nodes.get('search-query').value='submitted';const submitted=h.api.runSearch();
 assert.equal(body(h.requests.at(-1)).query,'submitted');assert.deepEqual(body(h.requests.at(-1)).chats,['old-chat']);h.requests.at(-1).resolve(savedPage([hit('synthetic-live','Submitted row')]));await submitted;
 h.nodes.get('search-query').value='typed but not submitted';
 h.api.setStatus({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:1,RecoveryEpoch:0},Codes:[]});const poll=h.api.refreshStatus();h.requests.at(-1).resolve({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:2,RecoveryEpoch:0},Codes:[]});await poll;
 assert.equal(h.nodes.get('search-results').children.length,0);
 const before=h.requests.length;const refresh=h.api.runSearch(false,true);const replay=h.requests.at(-1);
 assert.equal(h.requests.length,before+1,'the refresh issues exactly one request');assert.equal(replay.path,'/api/search');assert.equal(body(replay).query,'submitted');assert.deepEqual(body(replay).chats,[]);assert.doesNotMatch(JSON.stringify(body(replay)),/typed but not submitted/);
 replay.resolve(savedPage([hit('synthetic-live','Replayed row')]));await refresh;
 assert.match(h.nodes.get('search-results').textContent,/Replayed row/);assert.equal(h.nodes.get('search-query').value,'typed but not submitted');
});
await check('returning to Archive refreshes a cleared list with the submitted query after its replay fails',async()=>{
 const h=harness();h.api.setView('archive');h.nodes.get('search-query').value='submitted';const submitted=h.api.runSearch();h.requests.at(-1).resolve(savedPage([hit('synthetic-live','Submitted row')]));await submitted;
 h.nodes.get('search-query').value='typed but not submitted';
 h.api.setStatus({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:1,RecoveryEpoch:0},Codes:[]});const poll=h.api.refreshStatus();h.requests.at(-1).resolve({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:2,RecoveryEpoch:0},Codes:[]});await poll;
 const failed=h.api.runSearch(false,true);h.requests.at(-1).resolve({code:'archive_busy'},503);await flush();fireRefreshTimers(h);h.requests.at(-1).resolve({code:'archive_busy'},503);await failed;await flush();
 assert.equal(h.nodes.get('search-results').children.length,0);assert.equal(h.api.state().archiveRefreshNeeded,false);
 h.api.selectWorkspaceView('stats');const before=h.requests.length;h.api.selectWorkspaceView('archive');
 assert.equal(h.requests.length,before+1,'returning to Archive issues exactly one refresh');assert.equal(h.requests.at(-1).path,'/api/search');assert.equal(body(h.requests.at(-1)).query,'submitted');
});
await check('a background refresh before any submitted search browses defaults and never reads typed text',async()=>{
 const h=harness();h.nodes.get('search-query').value='typed before submit';
 h.api.selectWorkspaceView('archive');
 const first=h.requests.at(-1);assert.equal(h.requests.length,1,'one default browse request');assert.equal(first.path,'/api/archive/browse');assert.equal(body(first).query,undefined);assert.doesNotMatch(JSON.stringify(body(first)),/typed before submit/);
 first.resolve(savedPage([hit('synthetic-live','Default row')]));await flush();assert.match(h.nodes.get('search-results').textContent,/Default row/);
});
await check('owner expiry forgets the submitted query and its chat scope',async()=>{
 const h=harness();h.nodes.get('search-query').value='secret submitted';const pending=h.api.runSearch();h.requests.at(-1).resolve(savedPage([hit('synthetic-live')]));await pending;
 assert.equal(h.api.state().searchRequest.query,'secret submitted');h.api.expireAuth();assert.equal(h.api.state().searchRequest,null);
});
await check('ordinary deletion of the selected message keeps the refreshed list, Stats and chat filter',async()=>{
 const h=harness();h.api.renderStats(snapshot());h.nodes.get('search-chat').value='synthetic-chat';
 await browse(h,savedPage([hit('synthetic-live','Deleted row'),hit('synthetic-other','Other row')]));await openConversation(h,'synthetic-live');
 const before=h.requests.length;const pending=h.api.runSearch(false,true);assert.equal(h.requests.length,before+1);assert.deepEqual(body(h.requests.at(-1)).chats,['synthetic-chat']);
 h.requests.at(-1).resolve(savedPage([hit('synthetic-other','Other row')]));await flush();
 assert.equal(h.requests.at(-1).path,'/api/context');assert.equal(body(h.requests.at(-1)).message_id,'synthetic-live');h.requests.at(-1).resolve({code:'source_unavailable'},404);await pending;
 assert.equal(h.nodes.get('search-results').children.length,1);assert.doesNotMatch(h.nodes.get('search-results').textContent,/Deleted row/);assert.match(h.nodes.get('search-results').textContent,/Other row/);
 assert.equal(h.api.state().selectedMessage,'');assert.equal(h.nodes.get('conversation-messages').children.length,0);assert.match(h.nodes.get('context-feedback').textContent,/no longer available/);assert.equal(h.nodes.get('stats-message-count').textContent,'6');
});
await check('automatic refresh after an invalidation never submits unsent query text',async()=>{
 const h=harness();await browse(h,savedPage([hit('synthetic-a','Row A')]));
 h.nodes.get('search-query').value='typed but not submitted';
 h.api.setStatus({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:1,RecoveryEpoch:0},Codes:[]});const poll=h.api.refreshStatus();h.requests.at(-1).resolve({Connection:'connected',Messages:6,Snapshot:{PolicyGeneration:2,RecoveryEpoch:0},Codes:[]});await poll;
 const before=h.requests.length;const refresh=h.api.runSearch(false,true);assert.equal(h.requests.length,before+1);
 assert.equal(h.requests.at(-1).path,'/api/archive/browse');assert.equal(body(h.requests.at(-1)).query,undefined);assert.doesNotMatch(JSON.stringify(body(h.requests.at(-1))),/typed but not submitted/);
 h.requests.at(-1).resolve(savedPage([hit('synthetic-new','Row new')]));await refresh;assert.match(h.nodes.get('search-results').textContent,/Row new/);
});
console.log(JSON.stringify({suite:'saved archive UI',source_sha256:sourceSHA,cases,passed:cases.length}));
