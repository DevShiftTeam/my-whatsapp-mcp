import assert from 'node:assert/strict';
import fs from 'node:fs';
import {execFileSync} from 'node:child_process';
import {chromium} from '../docs/.vitepress/node_modules/playwright/index.mjs';

// Real browser/DOM, shipped assets, synthetic intercepted API only. No account,
// local app, provider, archive, credential, screenshot, or external request.
const baseline = process.argv.includes('--baseline');
const app = baseline
  ? execFileSync('git', ['show', 'HEAD:internal/localui/assets/app.js'], {encoding:'utf8'})
  : fs.readFileSync('internal/localui/assets/app.js', 'utf8');
const ending = '  start();\n})();';
assert.equal(app.split(ending).length, 2, 'exact startup seam');
const script = app.replace(ending, '  window.__refreshBrowser = {refreshStatus,refreshStats,runSearch};\n  start();\n})();');
const assets = new Map([
  ['/', ['text/html', fs.readFileSync('internal/localui/assets/index.html')]],
  ['/assets/app.js', ['text/javascript', script]],
  ...['app.css','theme.js','selects.js','logo.png'].map(name => ['/assets/'+name, [name.endsWith('.css') ? 'text/css' : name.endsWith('.png') ? 'image/png' : 'text/javascript', fs.readFileSync('internal/localui/assets/'+name)]]),
]);
const origin = 'http://127.0.0.1:39871';
const sentAt = '2026-10-11T08:00:00Z';
const hit = n => ({MessageID:'synthetic-'+n, ChatName:'Synthetic conversation', SenderName:'Synthetic sender', Snippet:'Synthetic message '+n+' שלום', SentAt:sentAt, Kind:'text'});
function stats(messages = 100) {
  return {
    sampled_at:1791709200,archive_revision:1,messages,chats:1,sent:40,received:messages-40,
    first_message_at:1791705600,last_message_at:1791709200,activity_interval:'day',activity_step:1,
    daily:[{date:'2026-10-11',messages}],top_chats:[{id:'synthetic-chat',name:'Synthetic conversation',messages}],
    kinds:[{kind:'text',messages}],media:{voice_messages:0,retained_voice:0,transcribed_voice:0},
    insights:{active_days:1,longest_streak_days:1,busiest_date:'2026-10-11',busiest_date_messages:messages},
    rhythm:[{weekday:0,hour:8,messages}],
    breakdown:{dimension:'chats',page:1,page_size:10,total:1,sort:'messages',rows:[{id:'synthetic-chat',name:'Synthetic conversation',messages,sent:40,received:messages-40,chats:1,last_message_at:1791709200}]},
  };
}

async function fixture(browser, options = {}) {
  const context = await browser.newContext({viewport:options.viewport || {width:1280,height:900}, colorScheme:options.theme || 'light', reducedMotion:options.motion || 'no-preference', serviceWorkers:'block', locale:'en-US'});
  const page = await context.newPage();
  const state = {revision:1,policy:1,holdBrowse:false,pending:[],statsFailure:false,unauthorized:false,missingContext:false,queries:[],navigations:0,errors:[]};
  page.on('pageerror', error=>state.errors.push(error.message));
  // history.replaceState strips capabilities at startup but is not a reload.
  // Count document loads, not same-document history notifications.
  page.on('domcontentloaded', () => {state.navigations++;});
  await context.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin !== origin) {await route.abort();return;}
    const asset = assets.get(url.pathname);
    if (asset) {await route.fulfill({status:200,contentType:asset[0],body:asset[1]});return;}
    const respond = (value, status=200) => route.fulfill({status,contentType:'application/json',body:JSON.stringify(value)});
    const request = route.request().postDataJSON() || {};
    switch (url.pathname) {
      case '/api/interface': return respond({realm:'local'});
      case '/api/session': return respond({csrf_token:'s'.repeat(43)});
      case '/api/setup': return respond({configured:true,demo:true,policy:{ChatMode:'all',DateMode:'all',IncludedChats:[]}});
      case '/api/status': return respond(state.unauthorized ? {code:'unauthorized'} : {Connection:'connected',Paused:false,Messages:100,Keyword:{State:'ready',Complete:100,Pending:0,Failed:0},Media:{State:'not_configured'},Semantic:{State:'not_configured'},Codes:[],Snapshot:{PolicyGeneration:state.policy,ArchiveRevision:state.revision,RecoveryEpoch:0}}, state.unauthorized ? 401 : 200);
      case '/api/pairing': return respond({state:'connected',has_session:true});
      case '/api/chats': return respond({Chats:[{ID:'synthetic-chat',Name:'Synthetic conversation',Selected:true}]});
      case '/api/stats':
        if (state.statsFailure) return route.fulfill({status:502,contentType:'text/html',body:'Synthetic gateway unavailable'});
        return respond(stats());
      case '/api/archive/browse':
      case '/api/search': {
        state.queries.push(request.query || '');
        const offset = request.cursor === 'synthetic-page-2' ? 50 : 0;
        const value = {Hits:Array.from({length:50},(_,i)=>hit(offset+i+1)),NextCursor:offset ? '' : 'synthetic-page-2',Snapshot:{PolicyGeneration:state.policy,ArchiveRevision:state.revision,RecoveryEpoch:0},saved_view:{source:'saved',coverage:'partial',current_phone_state:'unknown'}};
        if (state.holdBrowse) {state.pending.push({route,value});return;}
        return respond(value);
      }
      case '/api/context':
        if (state.missingContext) return respond({code:'source_unavailable'},404);
        return respond({Messages:[{ID:request.message_id,SenderName:'Synthetic sender',Body:('Synthetic conversation detail שלום\n').repeat(45),SentAt:sentAt,Kind:'text',Derived:[]}],saved_view:{source:'saved',coverage:'partial',current_phone_state:'unknown'}});
      default: return respond({},404);
    }
  });
  await page.goto(origin);
  try {
    await page.waitForFunction(() => document.querySelector('#workspace')?.hidden === false && document.querySelector('#stats-message-count')?.textContent === '100',null,{timeout:5000});
  } catch {
    const details=await page.evaluate(()=>({hidden:document.querySelector('#workspace')?.hidden,auth:document.querySelector('#auth-message')?.textContent,stats:document.querySelector('#stats-feedback')?.textContent}));
    await context.close();
    throw new Error('Synthetic startup failed: '+JSON.stringify({...details,errors:state.errors}));
  }
  return {context,page,state,release:async()=>{state.holdBrowse=false;for(const item of state.pending.splice(0)) await item.route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(item.value)});}};
}

const browser = await chromium.launch();
const failures = [], passed = [];
async function check(name, fn) {
  try {await fn();passed.push(name);}
  catch (error) {failures.push({case:name,error:error.message});}
}
try {
  await check('background import keeps loaded pages, selected conversation, focus and scroll without navigation', async()=>{
    const f = await fixture(browser);
    try {
      await f.page.locator('#nav-archive').click();
      await f.page.waitForFunction(()=>document.querySelectorAll('#search-results > li').length===50);
      await f.page.locator('#search-more').click();
      await f.page.waitForFunction(()=>document.querySelectorAll('#search-results > li').length===100);
      const selected = '.result-button[data-message-id="synthetic-70"]';
      await f.page.locator(selected).click();
      await f.page.waitForFunction(()=>document.querySelector('#conversation-messages').children.length>0);
      await f.page.locator(selected).focus();
      await f.page.evaluate(()=>{document.querySelector('#search-results').scrollTop=240;document.querySelector('#conversation-messages').scrollTop=120;});
      const before = await f.page.evaluate(()=>({results:document.querySelector('#search-results').scrollTop,context:document.querySelector('#conversation-messages').scrollTop}));
      f.state.holdBrowse=true;f.state.revision++;
      await f.page.evaluate(()=>window.__refreshBrowser.refreshStatus());
      await f.page.evaluate(()=>{window.__pendingRefresh=window.__refreshBrowser.runSearch(false,true);});
      await f.page.waitForFunction(()=>document.querySelector('#search-submit').disabled);
      assert.equal(await f.page.locator('#search-results > li').count(),100);
      assert.equal(await f.page.locator('#conversation-messages > *').count(),1);
      await f.release();
      await f.page.evaluate(()=>window.__pendingRefresh);
      assert.equal(await f.page.locator('#search-results > li').count(),100);
      assert.equal(await f.page.locator('#conversation-messages > *').count(),1);
      assert.equal(await f.page.locator(selected).getAttribute('aria-pressed'),'true');
      const after = await f.page.evaluate(()=>({focus:document.activeElement.dataset.messageId,results:document.querySelector('#search-results').scrollTop,context:document.querySelector('#conversation-messages').scrollTop}));
      assert.equal(after.focus,'synthetic-70');assert.equal(after.results,before.results);assert.equal(after.context,before.context);
      assert.equal(f.state.navigations,1);
    } finally {await f.context.close();}
  });
  await check('transient gateway failure preserves Stats and ends the busy state', async()=>{
    const f = await fixture(browser);
    try {
      f.state.statsFailure=true;
      await f.page.evaluate(()=>window.__refreshBrowser.refreshStats());
      assert.equal(await f.page.locator('#stats-message-count').textContent(),'100');
      assert.equal(await f.page.locator('#stats-data').getAttribute('aria-busy'),'false');
      assert.equal(await f.page.locator('#stats-updated').textContent(),'Update unavailable');
      assert.equal(f.state.navigations,1);
    } finally {await f.context.close();}
  });
  await check('an unavailable selected message clears only its content and retains unrelated refreshed data',async()=>{
    const f=await fixture(browser);
    try {
      await f.page.locator('#nav-archive').click();
      await f.page.waitForFunction(()=>document.querySelectorAll('#search-results > li').length===50);
      await f.page.locator('.result-button[data-message-id="synthetic-20"]').click();
      await f.page.waitForFunction(()=>document.querySelector('#conversation-messages').children.length>0);
      f.state.missingContext=true;
      await f.page.evaluate(()=>window.__refreshBrowser.runSearch(false,true));
      assert.equal(await f.page.locator('#search-results > li').count(),49);
      assert.equal(await f.page.locator('.result-button[data-message-id="synthetic-20"]').count(),0);
      assert.equal(await f.page.locator('#conversation-messages > *').count(),0);
      assert.equal(await f.page.locator('#stats-message-count').textContent(),'100');
      assert.match(await f.page.locator('#context-feedback').textContent(),/no longer available/i);
    } finally {await f.context.close();}
  });
  await check('a policy refresh never submits the unsubmitted search draft',async()=>{
    const f=await fixture(browser);
    try {
      await f.page.locator('#nav-archive').click();
      await f.page.locator('#search-query').fill('submitted query');
      await f.page.locator('#search-submit').click();
      await f.page.waitForFunction(()=>document.querySelector('#search-submit').disabled===false);
      await f.page.locator('#search-query').fill('unsubmitted draft');
      f.state.policy++;
      await f.page.evaluate(()=>window.__refreshBrowser.refreshStatus());
      await f.page.evaluate(()=>window.__refreshBrowser.runSearch(false,true));
      assert.equal(f.state.queries.at(-1),'submitted query');
      assert.equal(await f.page.locator('#search-query').inputValue(),'unsubmitted draft');
    } finally {await f.context.close();}
  });
  for (const [theme,motion,viewport] of [['light','no-preference',{width:1280,height:900}],['dark','reduce',{width:390,height:844}]]) {
    await check('inline refresh indicator preserves data in '+theme+' and '+motion, async()=>{
      const f = await fixture(browser,{theme,motion,viewport});
      try {
        await f.page.locator('#nav-archive').click();
        await f.page.waitForFunction(()=>document.querySelectorAll('#search-results > li').length===50);
        f.state.holdBrowse=true;
        await f.page.evaluate(()=>{window.__pendingRefresh=window.__refreshBrowser.runSearch(false,true);});
        await f.page.waitForFunction(()=>document.querySelector('#archive-update')?.hidden===false);
        assert.equal(await f.page.locator('#search-results > li').count(),50);
        assert.equal(await f.page.locator('#archive-update-text').textContent(),'Updating…');
        const animation=await f.page.locator('.refresh-spinner').evaluate(node=>getComputedStyle(node).animationName);
        assert.equal(animation==='none',motion==='reduce');
        assert.equal(await f.page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth),true);
        await f.release();await f.page.evaluate(()=>window.__pendingRefresh);
        assert.equal(await f.page.locator('#archive-update').isVisible(),false);
      } finally {await f.context.close();}
    });
  }
  await check('owner expiry clears content and prevents a late refreshed page from restoring it', async()=>{
    const f = await fixture(browser);
    try {
      await f.page.locator('#nav-archive').click();
      await f.page.waitForFunction(()=>document.querySelectorAll('#search-results > li').length===50);
      f.state.holdBrowse=true;
      await f.page.evaluate(()=>{window.__pendingRefresh=window.__refreshBrowser.runSearch(false,true);});
      f.state.unauthorized=true;
      await f.page.evaluate(()=>window.__refreshBrowser.refreshStatus());
      assert.equal(await f.page.locator('#workspace').isVisible(),false);
      assert.equal(await f.page.locator('#search-results > li').count(),0);
      await f.release();await f.page.evaluate(()=>window.__pendingRefresh);
      assert.equal(await f.page.locator('#search-results > li').count(),0);
    } finally {await f.context.close();}
  });
} finally {await browser.close();}
console.log(JSON.stringify({suite:'synthetic seamless refresh browser',baseline,passed,failures},null,2));
if (failures.length) process.exitCode=1;
