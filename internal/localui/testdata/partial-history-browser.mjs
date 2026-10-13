// Synthetic owner API routes only. No live app, linked device or provider.
// Run after installing the optional documentation Playwright dependencies:
// node internal/localui/testdata/partial-history-browser.mjs /absolute/private/output
import assert from 'node:assert/strict';
import {readFile,writeFile,mkdir,chmod} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import {pathToFileURL} from 'node:url';
const moduleURL=process.env.WA_UI_PLAYWRIGHT_MODULE
  ? pathToFileURL(process.env.WA_UI_PLAYWRIGHT_MODULE).href
  : new URL('../../../docs/.vitepress/node_modules/playwright/index.mjs',import.meta.url).href;
const {chromium}=await import(moduleURL);
assert.ok(process.argv[2]?.startsWith('/'),'private output must be an absolute directory');
const output=process.argv[2];await mkdir(output,{recursive:true,mode:0o700});await chmod(output,0o700);
const assets=new URL('../assets/',import.meta.url);
const app=await readFile(new URL('app.js',assets),'utf8');
const html=(await readFile(new URL('index.html',assets),'utf8')).replace('<html','<html data-remote-admin="true"');
const csrf=Buffer.alloc(32,0x73).toString('base64url');
assert.equal(csrf.length,43);
const capture={source:'historical',snapshot_id:'0123456789abcdef',captured_at:'2026-10-01T12:00:00Z',read_only:true,coverage:'partial',current_phone_state:'unknown'};
const key=(source,kind,suffix)=>source==='historical'?'h1.'+capture.snapshot_id+'.'+kind+'.'+Buffer.from(suffix).toString('base64url'):'synthetic-'+suffix;
const snapshot=()=>({PolicyGeneration:4,ArchiveRevision:40,UnknownHistoryGap:true,DeliveredHistorySnapshot:true});
function progress(){return {operation_id:'synthetic-operation',generation:'synthetic-generation',revision:3,policy_generation:4,state:'running',strategy:'delivered-history',strategy_epoch:2,target_start:'2026-01-01T00:00:00Z',copy_limit:10000,copy_spent:20,discovery_limit:512,discovery_spent:46,discovered:8,queued:2,pending:14,accepted:6,stale:2,deferred:0,last_attempt_at:new Date(Date.now()-15000).toISOString(),oldest_observed:'2026-01-01T01:00:00Z',queue_full:false,delivered_accepted:6,delivered_limit:10000,readable_messages:6,delivered_oldest:'2026-10-13T08:00:00Z',page_attempts:2,page_state:'waiting_phone'};}
function ownerStatus(unsafe=false){return {Connection:unsafe?'reconciliation_required':'connected',Paused:false,ReconciliationRequired:unsafe,Messages:unsafe?0:6,Coverage:'partial',Keyword:{State:unsafe?'unavailable':'ready',Complete:6,Pending:0,Failed:0},Media:{State:'not_configured'},Semantic:{State:'not_configured'},Codes:unsafe?['history_corrupt']:[],Snapshot:snapshot(),live_rebuild:{mode:'live_rebuild',coverage:'partial',current_phone_state:'unknown'},recovery:progress()};}
function stats(source,filter){
  const count=source==='historical'?4:6,sent=2,voice=2,text=count-voice;
  const rows=[{id:key(source,'c','first-chat'),name:'Synthetic צוות Atlas',messages:text,sent,received:text-sent,chats:1,last_message_at:1791878400},{id:key(source,'c','second-chat'),name:'Synthetic second chat',messages:voice,sent:0,received:voice,chats:1,last_message_at:1791882000}];
  return {sampled_at:1791885600,messages:count,chats:2,sent,received:count-sent,last_message_at:1791882000,activity_interval:'day',activity_step:1,daily:[{date:'2026-10-13',messages:count}],top_chats:rows.map(({id,name,messages})=>({id,name,messages})),kinds:[{kind:'text',messages:text},{kind:'voice',messages:voice}],media:{voice_messages:voice,retained_voice:voice,transcribed_voice:1},insights:{active_days:1,longest_streak_days:1,busiest_date:'2026-10-13',busiest_date_messages:count},rhythm:[{weekday:2,hour:8,messages:text},{weekday:2,hour:9,messages:voice}],breakdown:{dimension:filter.breakdown_dimension||'chats',page:filter.breakdown_page||1,page_size:filter.breakdown_page_size||10,total:2,sort:filter.breakdown_sort||'messages',rows},unknown_history_gap:true,delivered_history_snapshot:true,...(source==='historical'?{archive_view:capture}:{})};
}
const connections={local_state:'ready',remote_state:'ready',recovery_state:'ready',local_recovery_state:'ready',remote_recovery_state:'ready',items:[{id:'synthetic-preserved-client',client:'codex_desktop',label:'Synthetic preserved AI client',mode:'remote',revision:3,state:'ready',expires_at:'2027-01-01T00:00:00Z',scope:{all_chats:true,chat_count:0,chats:[]},send_permission:{mode:'none',revision:1,chats:[]}}]};
const browser=await chromium.launch({headless:true}),results=[];
try {
  for(const width of [320,360,390]) for(const theme of ['light','dark']) {
    const context=await browser.newContext({viewport:{width,height:844},colorScheme:theme});
    const page=await context.newPage(),errors=[],requests=[],screenshots=[];
    page.setDefaultTimeout(5000);
    let unsafe=false,connectionsUnavailable=false;
    page.on('pageerror',error=>errors.push(error.message));
    await page.route('**/*',async route=>{
      const request=route.request(),url=new URL(request.url()),path=url.pathname;
      assert.equal(url.origin,'https://synthetic.example','all browser requests stay in the intercepted fixture origin');
      if(path==='/')return route.fulfill({status:200,contentType:'text/html',body:html});
      if(path.startsWith('/assets/'))return route.fulfill({status:200,contentType:path.endsWith('.css')?'text/css':path.endsWith('.png')?'image/png':'application/javascript',body:path==='/assets/app.js'?app:await readFile(new URL(path.slice(8),assets))});
      let body;try{body=request.postDataJSON();}catch{body=null;}
      requests.push({path:url.pathname+url.search,method:request.method(),body});
      if(path==='/api/auth/session'){assert.equal(request.method(),'POST');return route.fulfill({status:200,json:{csrf_token:csrf}});}
      assert.equal(request.headers()['x-csrf-token'],csrf,'actual owner requests retain the established session capability');
      const source=body?.source||url.searchParams.get('source')||'live';
      const json=value=>route.fulfill({status:200,json:value});
      if(path==='/api/setup')return json({configured:true,policy:{Generation:4,DateMode:'since',ImportStart:'2026-01-01T00:00:00Z',ChatMode:'all',IncludedChats:[],RollingRetention:0},archive_view:capture,recovery:progress(),live_rebuild:{mode:'live_rebuild',coverage:'partial',current_phone_state:'unknown'}});
      if(path==='/api/status')return json(source==='historical'?{Connection:'historical_snapshot',Messages:4,Coverage:'partial',Snapshot:snapshot(),archive_view:capture}:ownerStatus(unsafe));
      if(path==='/api/pairing')return json({state:unsafe?'reconciliation_required':'connected',has_session:true,qr_available:false});
      if(path==='/api/chats')return json({Chats:[{ID:key(source,'c','first-chat'),Name:'Synthetic צוות Atlas',Selected:true},{ID:key(source,'c','second-chat'),Name:'Synthetic second chat',Selected:true}],Snapshot:snapshot(),...(source==='historical'?{archive_view:capture}:{})});
      if(path==='/api/stats'){assert.equal(request.method(),'POST');return unsafe?route.fulfill({status:409,json:{code:'reconciliation_required'}}):json(stats(source,body));}
      if(path==='/api/search'){assert.equal(request.method(),'POST');return unsafe?route.fulfill({status:409,json:{code:'reconciliation_required'}}):json({Hits:[{MessageID:key(source,'m','message'),ChatName:'Synthetic צוות Atlas',SenderName:'Synthetic sender',SentAt:'2026-10-13T08:00:00Z',Snippet:'Synthetic '+source+' message שלום',Kind:'text'}],Snapshot:snapshot()});}
      if(path==='/api/context'){assert.equal(request.method(),'POST');assert.equal(body.message_id,key(source,'m','message'));return unsafe?route.fulfill({status:409,json:{code:'reconciliation_required'}}):json({Messages:[{ID:key(source,'m','message'),SenderName:'Synthetic sender',SentAt:'2026-10-13T08:00:00Z',Body:'Synthetic '+source+' message שלום',Kind:'text'}],Snapshot:snapshot()});}
      if(path==='/api/connections')return connectionsUnavailable?route.fulfill({status:503,json:{code:'client_connections_unavailable'}}):json(connections);
      if(path==='/api/recovery')return json(progress());
      throw new Error('Unexpected or mutating fixture route: '+path);
    });
    async function noOverflow(){assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);}
    async function shot(name){const file=name+'-'+width+'-'+theme+'.png';await page.screenshot({path:output+'/'+file,fullPage:true});screenshots.push(file);}
    async function selectSource(value){const picker=page.getByRole('combobox',{name:/View archive:/});await picker.scrollIntoViewIfNeeded();await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));await picker.click();await page.getByRole('option',{name:value==='historical'?'Historical':'Live',exact:true}).click();}
    async function search(source){await page.locator('#search-query').fill('Synthetic');await page.locator('#search-submit').click();await page.locator('#search-results button').waitFor({state:'visible'});assert.match(await page.locator('#search-results').textContent(),new RegExp('Synthetic '+source+' message'));await page.locator('#search-results button').click();await page.locator('#conversation-messages').getByText('Synthetic '+source+' message שלום',{exact:true}).waitFor({state:'visible'});assert.match(await page.locator('#conversation-messages').textContent(),new RegExp('Synthetic '+source+' message'));}
    await page.goto('https://synthetic.example/');await page.locator('#workspace').waitFor({state:'visible'});await page.waitForFunction(()=>document.getElementById('stats-message-count').textContent==='6');
    assert.equal(await page.locator('#archive-source-control').isVisible(),true);
    assert.match(await page.locator('#unknown-history-notice').textContent(),/Supported controls.*current permissions.*enforced/);
    assert.match(await page.locator('#history-import-summary').textContent(),/6 snapshot messages accepted.*6 readable messages in Live/);
    assert.equal(await page.locator('#history-import-spinner').isVisible(),false);await noOverflow();await shot('stats-live');
    await page.locator('#nav-archive').click();await search('live');await noOverflow();await shot('archive-live');
    await page.locator('#clients-open').click();await page.getByText('Synthetic preserved AI client',{exact:true}).waitFor({state:'visible'});assert.match(await page.locator('#client-list').textContent(),/Grant ready; client call not yet verified/);assert.equal(await page.locator('#clients-feedback').textContent(),'');await noOverflow();await shot('ai-ready');await page.locator('#clients-close').click();
    await selectSource('historical');await page.waitForFunction(()=>document.getElementById('archive-source-note').textContent.includes('Read-only'));await search('historical');await noOverflow();await shot('archive-historical');
    await page.locator('#nav-stats').click();await page.waitForFunction(()=>document.getElementById('stats-message-count').textContent==='4');assert.match(await page.locator('#history-import-help').textContent(),/historical view remains unchanged.*Live separately/);await noOverflow();await shot('stats-historical');
    await page.locator('#settings-open').click();await page.locator('#settings-dialog').waitFor({state:'visible'});assert.equal(await page.locator('#save-policy').isDisabled(),false);assert.equal(await page.locator('#import-start').isDisabled(),false);assert.equal(await page.locator('#recovery-panel').isVisible(),true);await noOverflow();await page.locator('#settings-close').click();
    await selectSource('live');await page.waitForFunction(()=>document.getElementById('stats-message-count').textContent==='6');
    unsafe=true;await page.waitForFunction(()=>document.getElementById('connection-status').textContent==='Import stopped',{timeout:10000});assert.match(await page.locator('#stats-connection-description').textContent(),/stats are paused.*requires checks/i);assert.equal(await page.locator('#stats-message-count').textContent(),'—');assert.equal(await page.locator('#messages-count').textContent(),'—');assert.equal(await page.locator('#history-import-spinner').isVisible(),false);
    await page.locator('#nav-archive').click();assert.equal(await page.locator('#search-content').isVisible(),false);assert.equal(await page.locator('#import-title').textContent(),'Import stopped');assert.match(await page.locator('#import-description').textContent(),/archive is paused|could not verify the archive/i);
    connectionsUnavailable=true;await page.locator('#clients-open').click();await page.getByText('AI connections are unavailable. Keep the app running and refresh their state.',{exact:true}).waitFor({state:'visible'});assert.equal(await page.locator('#client-list').textContent(),'');await page.locator('#clients-close').click();
    assert.ok(requests.some(r=>r.path==='/api/auth/session'));
    for(const route of ['/api/setup','/api/status','/api/pairing','/api/chats?limit=100','/api/stats','/api/search','/api/context','/api/connections'])assert.ok(requests.some(r=>r.path===route),'actual API chain includes '+route);
    for(const route of ['/api/status?source=historical','/api/chats?source=historical&limit=100'])assert.ok(requests.some(r=>r.path===route),'historical metadata source is explicit: '+route);
    for(const route of ['/api/stats','/api/search','/api/context'])assert.ok(requests.some(r=>r.path===route&&r.body?.source==='historical'),'historical read source is explicit: '+route);
    assert.ok(requests.filter(r=>r.path==='/api/setup').every(r=>r.method==='GET'),'viewing date choices never mutates them');
    assert.equal(requests.some(r=>r.path.includes('/control')||r.path.includes('/revoke')||r.path.includes('/permissions')||r.path.includes('/send')),false);
    assert.deepEqual(errors,[]);results.push({width,theme,passed:true,requests:requests.length,checks:['real authenticated startup API chain','readable Stats despite generic partial flag','live search/context','preserved ready AI card','interactive source dropdown','historical status/chats/Stats/search/context','editable Live date controls','Live history progress while viewing capture','whole-page overflow absent','real source fence remains visible','AI503 clears unverifiable card'],screenshots});await context.close();
  }
  const receipt={passed:true,scope:'Intercepted synthetic owner APIs and unmodified actual app startup; no production or provider requests',app_js_sha256:createHash('sha256').update(app).digest('hex'),results};
  await writeFile(output+'/result.json',JSON.stringify(receipt,null,2),{mode:0o600});console.log(JSON.stringify({passed:true,configurations:results.length,checks:results.length*11,app_js_sha256:receipt.app_js_sha256}));
} finally {await browser.close();}
