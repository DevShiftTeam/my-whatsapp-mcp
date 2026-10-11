import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

// Run the shipped browser source with synthetic names and deferred HTTP replies.
// No app, linked session, credentials, or real WhatsApp messages are used here.
const source = fs.readFileSync(new URL('../../internal/localui/assets/app.js', import.meta.url), 'utf8');
const html = fs.readFileSync(new URL('../../internal/localui/assets/index.html', import.meta.url), 'utf8');
const ending = '  start();\n})();';
assert.equal(source.split(ending).length, 2);
class Node {
  constructor(tag = 'div') {
    this.tagName = tag.toUpperCase(); this.children = []; this.dataset = {}; this.attributes = {}; this.listeners = new Map(); this.style = {};
    this.hidden = false; this.disabled = false; this.open = false; this.checked = false; this.value = ''; this._text = ''; this.className = '';
    this.classList = {toggle(){},add(){},remove(){}};
  }
  get textContent() { return this._text + this.children.map(node => node.textContent).join(''); }
  set textContent(value) { this._text = String(value); this.children = []; }
  get parentElement() { return this.parent ?? new Node(); }
  get options() { return this.children.filter(node => node.tagName === 'OPTION'); }
  append(...nodes) { for (const value of nodes) { const node = typeof value === 'string' ? textNode(value) : value; node.parent = this; this.children.push(node); } }
  replaceChildren(...nodes) { this.children = []; this._text = ''; this.append(...nodes); }
  setAttribute(key, value) { this.attributes[key] = String(value); }
  removeAttribute(key) { delete this.attributes[key]; }
  getAttribute(key) { return this.attributes[key] ?? null; }
  addEventListener(name, callback) { if (!this.listeners.has(name)) this.listeners.set(name, []); this.listeners.get(name).push(callback); }
  querySelectorAll(selector) {
    const result = []; const [tag, constraint] = selector.split(':');
    for (const node of this.children) {
      if (node.tagName.toLowerCase() === tag && (constraint !== 'checked' || node.checked)) result.push(node);
      result.push(...node.querySelectorAll(selector));
    }
    return result;
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] ?? null; }
  focus() { this.focused = true; if (this.ownerDocument) this.ownerDocument.activeElement = this; }
  contains(node) { for (let current=node;current;current=current.parent) if (current===this) return true; return false; }
  reset() { this.value = ''; }
  close() { this.open = false; this.fire('close'); }
  showModal() { this.open = true; }
  fire(name) { return Promise.all((this.listeners.get(name) ?? []).map(callback => callback({preventDefault(){},target:this}))); }
}
function textNode(value) { const node = new Node('#text'); node.textContent = value; return node; }
function option(text, value) { const node = new Node('option'); node.textContent = text; node.value = value; return node; }
function client(overrides = {}) {
  return {id:'synthetic-client',label:'Synthetic assistant',client:'composio',mode:'remote',state:'ready',revision:4,expires_at:'2099-10-11T12:00:00Z',scope:{all_chats:true,chats:[],chat_count:2},send_permission:{mode:'none',chats:[],revision:0},...overrides};
}
function connections(item = client()) { return {local_state:'ready',remote_state:'ready',local_recovery_state:'ready',remote_recovery_state:'ready',items:[item]}; }
function harness() {
  const nodes = new Map([...html.matchAll(/<([a-z][a-z0-9]*)\b[^>]*\bid="([^"]+)"/gi)].map(match => [match[2],new Node(match[1])]));
  const selectors = new Map(); const requests = []; const intervals = new Map();
  const document = {
    hidden:false,documentElement:{dataset:{}},body:new Node('body'),activeElement:null,
    getElementById:id => { assert.ok(nodes.has(id), 'HTML ID ' + id); return nodes.get(id); },
    createElement:tag => { const node=new Node(tag); node.ownerDocument=document; return node; },createElementNS:(_,tag) => new Node(tag),createTextNode:textNode,
    querySelector:selector => { if (!selectors.has(selector)) selectors.set(selector,new Node('input')); return selectors.get(selector); },
    querySelectorAll:() => [],addEventListener(){},
  };
  document.activeElement = document.body;
  for (const node of [...nodes.values(),document.body]) node.ownerDocument=document;
  for (const id of ['client-send-title','client-send-search','client-send-save']) nodes.get(id).parent=nodes.get('client-send-editor');
  document.querySelector('input[name=client-scope]:checked').value = 'all';
  nodes.get('client-mode').value = 'remote'; nodes.get('client-choice').value = 'composio'; nodes.get('clients-dialog').open = true;
  nodes.get('client-send-editor').hidden = true;
  for (const mode of ['none','selected','all']) { const node = nodes.get('client-send-mode-' + mode); if (node) { node.value = mode; selectors.set('input[name=client-send-mode][value="' + mode + '"]', node); } }
  document.querySelectorAll = selector => selector === 'input[name=client-send-mode]' ? ['none','selected','all'].map(mode => nodes.get('client-send-mode-' + mode)) : [];
  const fetch = (path, options) => new Promise((resolve, reject) => requests.push({path,options,reject,resolve:(value,status=200)=>resolve({ok:status>=200&&status<300,status,json:async()=>value})}));
  const expose = `  globalThis.__test = {renderConnections,openSendPermissions,clearSendPermissions,renderSendPermissionChats,updateSendPermissionControls,saveSendPermissions,refreshConnections,expireAuth,
    state:()=>({clientBusy,clientVersion,authExpired,sendPermissionBinding,sendPermissionMode,sendPermissionSelection:Array.from(sendPermissionSelection)}),
    setChats:value=>{chats=value;},setSetup:value=>{setup=value;},setCSRF:value=>{csrf=value;}};\n})();`;
  const context = vm.createContext({document,fetch,window:{addEventListener(){},setInterval:fn=>{intervals.set(1,fn);return 1;},clearInterval:id=>intervals.delete(id),setTimeout(){},clearTimeout(){}},Date,Intl,URL,URLSearchParams,Option:option,AbortController,localStorage:{getItem(){return '';},removeItem(){}},navigator:{clipboard:{}},location:{hash:'',pathname:'/'},history:{replaceState(){}}});
  vm.runInContext(source.replace(ending, expose), context, {timeout:5000});
  const api = context.__test;
  api.setSetup({configured:true,Policy:{ChatMode:'all'}}); api.setCSRF('synthetic-csrf');
  api.setChats([{ID:'chat-a',Name:'Noa Levi',Selected:true},{ID:'chat-b',Name:'צוות Atlas',Selected:true}]);
  api.renderConnections(connections());
  return {api,nodes,requests,document};
}
const plain = value => JSON.parse(JSON.stringify(value));
const flush = () => new Promise(resolve => setImmediate(resolve));
const cases = [];
async function check(name, body) { await body(); cases.push(name); }
function changeMode(h, mode) { for (const value of ['none','selected','all']) h.nodes.get('client-send-mode-' + value).checked = value === mode; return h.nodes.get('client-send-mode-' + mode).fire('change'); }

await check('existing permissions remain unavailable when the server omits authoritative state', async()=>{
  const h=harness(); const item=client({send_permission:undefined}); h.api.renderConnections(connections(item));
  assert.match(h.nodes.get('client-list').textContent,/Read-only/);
  assert.equal(h.nodes.get('client-list').querySelectorAll('button').some(node=>node.dataset.action==='permissions'),false);
  h.api.openSendPermissions(item); assert.equal(h.nodes.get('client-send-editor').hidden,true); assert.equal(h.requests.length,0);
});
await check('selected chats are bounded by archive policy and the existing read scope', async()=>{
  const h=harness(); const item=client({scope:{all_chats:false,chats:['chat-a','chat-c'],chat_count:2},send_permission:{mode:'selected',chats:['chat-c'],revision:2}});
  h.api.setSetup({configured:true,Policy:{ChatMode:'selected'}});
  h.api.setChats([{ID:'chat-a',Name:'נועה לוי',Selected:true},{ID:'chat-b',Name:'Other scope',Selected:true},{ID:'chat-c',Name:'Not archived',Selected:false}]);
  h.api.renderConnections(connections(item)); h.api.openSendPermissions(item);
  assert.deepEqual(h.nodes.get('client-send-chats').querySelectorAll('input').map(node=>node.value),['chat-a']);
  assert.deepEqual(plain(h.api.state().sendPermissionSelection),['chat-c']);
  assert.match(h.nodes.get('client-send-selection-note').textContent,/1 selected chat.*not shown/);
});
await check('name filtering supports Hebrew, treats names as text, and preserves hidden choices', async()=>{
  const h=harness(); h.api.setChats([{ID:'chat-a',Name:'נוֹעָה לוי <img src=x>',Selected:true},{ID:'chat-b',Name:'Atlas',Selected:true}]);
  h.api.openSendPermissions(client()); await changeMode(h,'selected');
  const first=h.nodes.get('client-send-chats').querySelectorAll('input')[0]; first.checked=true; await first.fire('change');
  h.nodes.get('client-send-search').value='Atlas'; h.api.renderSendPermissionChats(); assert.deepEqual(h.nodes.get('client-send-chats').querySelectorAll('input').map(node=>node.value),['chat-b']);
  assert.deepEqual(plain(h.api.state().sendPermissionSelection),['chat-a']);
  h.nodes.get('client-send-search').value='נועה'; h.api.renderSendPermissionChats();
  const label=h.nodes.get('client-send-chats').children[0]; assert.match(label.textContent,/<img src=x>/); assert.equal(label.querySelector('bdi').children.length,0); assert.equal(label.querySelector('input').checked,true);
});
await check('the 101st selection is rejected while checked choices remain removable', async()=>{
  const h=harness(); h.api.setChats(Array.from({length:101},(_,index)=>({ID:'chat-'+index,Name:'Synthetic '+index,Selected:true})));
  h.api.openSendPermissions(client()); await changeMode(h,'selected');
  const inputs=h.nodes.get('client-send-chats').querySelectorAll('input');
  for (const input of inputs.slice(0,100)) { input.checked=true; await input.fire('change'); }
  assert.equal(h.api.state().sendPermissionSelection.length,100); assert.equal(inputs[100].disabled,true); assert.equal(inputs[0].disabled,false);
  inputs[100].checked=true; await inputs[100].fire('change'); assert.equal(inputs[100].checked,false); assert.equal(h.api.state().sendPermissionSelection.length,100);
  inputs[0].checked=false; await inputs[0].fire('change'); assert.equal(inputs[100].disabled,false);
});
await check('saving all uses owner CSRF and both current revisions without accessing a token', async()=>{
  const h=harness(); const item=client({send_permission:{mode:'selected',chats:['chat-a'],revision:7}}); h.api.renderConnections(connections(item)); h.api.openSendPermissions(item); await changeMode(h,'all');
  const pending=h.api.saveSendPermissions({preventDefault(){}}); assert.equal(h.requests.length,1);
  assert.equal(h.requests[0].path,'/api/connections/send-permission'); assert.equal(h.requests[0].options.headers['X-CSRF-Token'],'synthetic-csrf');
  assert.deepEqual(JSON.parse(h.requests[0].options.body),{id:item.id,revision:4,mode:'all',chats:[],expected_send_revision:7});
  h.requests[0].resolve({send_permission:{mode:'all',chats:[],revision:8}}); await flush();
  assert.equal(h.requests[1].path,'/api/connections'); h.requests[1].resolve(connections(client({send_permission:{mode:'all',chats:[],revision:8}}))); await pending;
  assert.match(h.nodes.get('client-list').textContent,/Send to all permitted chats/); assert.match(h.nodes.get('clients-feedback').textContent,/token is unchanged/);
  assert.equal(h.requests.some(request=>/reveal|\/api\/send\//.test(request.path)),false);
});
await check('a permission conflict refreshes current state without resubmitting the stale draft', async()=>{
  const h=harness(); h.api.openSendPermissions(client()); await changeMode(h,'all');
  const pending=h.api.saveSendPermissions({preventDefault(){}}); h.requests[0].resolve({code:'send_permission_changed'},409); await flush();
  assert.equal(h.nodes.get('client-send-editor').hidden,true); assert.equal(h.requests.length,2); assert.equal(h.requests[1].path,'/api/connections');
  h.requests[1].resolve(connections(client({send_permission:{mode:'selected',chats:['chat-b'],revision:1}}))); await pending;
  assert.match(h.nodes.get('clients-feedback').textContent,/changed.*review/i); assert.equal(h.requests.filter(request=>request.options.method==='POST').length,1);
  assert.equal(h.document.activeElement.dataset.action,'permissions');
});
await check('an in-flight permission save cannot be cancelled into an outdated authority badge', async()=>{
  const h=harness(); h.api.openSendPermissions(client()); await changeMode(h,'all');
  const pending=h.api.saveSendPermissions({preventDefault(){}}); assert.equal(h.nodes.get('client-send-cancel').disabled,true);
  await h.nodes.get('client-send-cancel').fire('click'); assert.notEqual(h.api.state().sendPermissionBinding,null);
  h.requests[0].resolve({send_permission:{mode:'all',chats:[],revision:1}}); await flush(); assert.equal(h.requests[1].path,'/api/connections');
  h.requests[1].resolve(connections(client({send_permission:{mode:'all',chats:[],revision:1}}))); await pending;
  assert.match(h.nodes.get('client-list').textContent,/Send to all permitted chats/); assert.equal(h.nodes.get('client-send-editor').hidden,true);
});
await check('refresh invalidates an edited permission and expiry prevents a new save', async()=>{
  const h=harness(); h.api.openSendPermissions(client()); await changeMode(h,'all');
  h.api.renderConnections(connections(client({send_permission:{mode:'none',chats:[],revision:1}}))); assert.equal(h.nodes.get('client-send-editor').hidden,true);
  h.api.renderConnections(connections(client({expires_at:'2000-01-01T00:00:00Z'}))); h.api.openSendPermissions(client({expires_at:'2000-01-01T00:00:00Z'}));
  await h.api.saveSendPermissions({preventDefault(){}}); assert.equal(h.requests.length,0); assert.equal(h.nodes.get('client-send-editor').hidden,true);
});
await check('invalidating a focused editor restores its visible connection action or a fallback', async()=>{
  const h=harness(); h.api.openSendPermissions(client()); h.nodes.get('client-send-search').focus();
  h.api.renderConnections(connections(client({send_permission:{mode:'none',chats:[],revision:1}})));
  assert.equal(h.document.activeElement.dataset.action,'permissions');
  h.api.openSendPermissions(client({send_permission:{mode:'none',chats:[],revision:1}}));
  h.api.renderConnections(connections(client({state:'expired',expires_at:'2000-01-01T00:00:00Z',send_permission:{mode:'none',chats:[],revision:1}})));
  assert.equal(h.document.activeElement.dataset.action,'heading');
  h.api.renderConnections(connections()); h.api.openSendPermissions(client());
  h.api.renderConnections({...connections(),items:[]}); assert.equal(h.document.activeElement,h.nodes.get('clients-refresh'));
});
await check('closing the connections window fences a late save response and clears selected names', async()=>{
  const h=harness(); h.api.openSendPermissions(client()); await changeMode(h,'all'); const pending=h.api.saveSendPermissions({preventDefault(){}});
  h.nodes.get('clients-dialog').close(); h.requests[0].resolve({send_permission:{mode:'all',chats:[],revision:1}}); await pending;
  assert.equal(h.api.state().sendPermissionBinding,null); assert.equal(h.nodes.get('client-send-chats').children.length,0); assert.equal(h.requests.length,1); assert.doesNotMatch(h.nodes.get('clients-feedback').textContent,/saved/);
});
await check('owner expiry clears the editor and rejects a late permission reply', async()=>{
  const h=harness(); h.api.openSendPermissions(client()); await changeMode(h,'all'); const pending=h.api.saveSendPermissions({preventDefault(){}}); h.api.expireAuth(); h.requests[0].resolve({send_permission:{mode:'all',chats:[],revision:1}}); await pending;
  assert.equal(h.api.state().authExpired,true); assert.equal(h.api.state().sendPermissionBinding,null); assert.equal(h.nodes.get('client-send-editor').hidden,true); assert.equal(h.nodes.get('client-send-chats').children.length,0); assert.equal(h.requests.length,1);
});
console.log(JSON.stringify({ok:true,tests:cases.length,cases,scope:'shipped JavaScript and synthetic DOM; no browser layout or live sends'},null,2));
