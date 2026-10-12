import {summarize as summary} from './statistics.mjs';
// Developer qualification only. Uses prebuilt images; never rebuilds or pulls silently.
import {imageMetrics} from './image-metrics.mjs';
import {randomUUID} from 'node:crypto';
import {chromium} from '../../docs/.vitepress/node_modules/@playwright/test/index.mjs';
import {binary,api,fixtureMode,singleApplicationProcess,check,docker,createFixture,start,stop,cleanup,ownerLink,helper,pause,imageIdentity,option,waitReady,cleanupRun} from './lib.mjs';
async function main(){
process.env.WA_DOCKER_RUN_ID=randomUUID();
const count=Number(option('--samples','5'));check(Number.isInteger(count)&&count>=5&&count<=20,'samples_range');
const mode=option('--mode','demo');const selected=fixtureMode(mode);
const refs=[option('--image','wa-task4:development'),option('--comparison','wa-task4-slim:development')];
const browser=await chromium.launch({headless:true});const results=[];
try{for(const reference of refs){const identity=await imageIdentity(reference);check(/^[a-f0-9]{40}$/.test(identity.sourceRevision),'immutable_source_label_required');const fresh=[],reused=[],shutdown=[];let f;
 try{
 if(mode==='serve'){
  f=await createFixture(identity.imageID,{mode});
  await docker(['network','disconnect',f.name+'_default',f.name]);
  await docker(['start',f.name]);
  const deadline=Date.now()+30000;let ready=false;
  while(Date.now()<deadline){const result=await docker(['exec',f.name,binary,'health','--data-dir',selected.root,'--connect','127.0.0.1:8123'],{allowFailure:true});if(result.code===0){ready=true;break;}await pause(100);}
  check(ready,'offline_normal_ready');
  const processes=(await docker(['top',f.name,'-eo','pid,comm'])).out;
  check(singleApplicationProcess(processes),'offline_no_helper');
  await stop(f);await cleanup(f);f=null;
 }
 for(let i=0;i<count;i++){f=await createFixture(identity.imageID,{mode});fresh.push(await start(f));shutdown.push(await stop(f));await cleanup(f);f=null;}
 f=await createFixture(identity.imageID,{mode});await start(f);const ctx=await browser.newContext({timezoneId:'UTC'});const page=await ctx.newPage();await page.goto(await ownerLink(f));await page.locator('#settings-dialog[open]').waitFor();await page.locator('input[name=date-mode][value=all]').check();await page.locator('#save-policy').click();await page.locator('#settings-dialog').waitFor({state:'hidden'});if(mode==='demo'){await page.locator('#pairing-start').click();await page.locator('#connection-close').click();await page.getByRole('button',{name:'Archive',exact:true}).click();await page.locator('#search-query').fill('Project Atlas');await page.locator('#search-submit').click();await page.locator('#search-results .result-button').first().waitFor();}else{const status=await api(page,'/api/remote/status');check(status.status===200&&status.value.saved_origin===''&&status.value.browser_enabled===false,'default_remote_off');const processes=(await docker(['top',f.name,'-eo','pid,comm'])).out;check(singleApplicationProcess(processes),'disabled_no_helper_process');}
 const savedPolicy=(await api(page,'/api/setup')).value;await ctx.close();if(mode==='demo')await helper(f,'sentinel');await stop(f);
 for(let i=0;i<count;i++){reused.push(await start(f));if(mode==='demo')check(JSON.parse((await helper(f,'inspect')).out).sentinel,'populated_volume_sentinel');else{const c=await browser.newContext();const p=await c.newPage();await p.goto(await ownerLink(f));await p.locator('#settings-open').waitFor();const actual=(await api(p,'/api/setup')).value;check(JSON.stringify(actual.Policy||actual.policy)===JSON.stringify(savedPolicy.Policy||savedPolicy.policy),'normal_policy_persistence');await c.close();}shutdown.push(await stop(f));}
 if(mode==='serve'){await cleanup(f,{volume:false});f=await createFixture(identity.imageID,{mode,volume:f.volume,name:f.name,port:f.port});}
 await start(f);if(mode==='serve'){const c=await browser.newContext();const p=await c.newPage();await p.goto(await ownerLink(f));await p.locator('#settings-open').waitFor();const actual=(await api(p,'/api/setup')).value;check(JSON.stringify(actual.Policy||actual.policy)===JSON.stringify(savedPolicy.Policy||savedPolicy.policy),'recreated_normal_policy');await c.close();}const before=JSON.parse((await helper(f,'metrics')).out);const idleStart=performance.now();await pause(2000);const after=JSON.parse((await helper(f,'metrics')).out);const idleSeconds=(performance.now()-idleStart)/1000;
 const health=[];for(let i=0;i<5;i++){const t=performance.now();await docker(['exec',f.name,binary,'health',...selected.healthArgs,'--data-dir',selected.root,'--connect','127.0.0.1:8123']);health.push(performance.now()-t);}
 const restartStart=performance.now();await docker(['restart','--time','10',f.name]);const restart=await waitReady(f.name,f.origin,restartStart);
 const sizes=await imageMetrics(identity.imageID);const item={mode,normalServe:mode==='serve'?{offlineStartup:'PASS',offlineNoHelper:'PASS',defaultRemoteOff:'PASS',defaultNoHelper:'PASS',sameVolumeRecreation:'PASS'}:null,volumeCondition:mode==='serve'?'initialized unpaired archive with saved policy; no message import':'populated synthetic demo',network:'default bridge with literal loopback publication',offlineStartup:mode==='serve'?'PASS: separate disconnected-network normal serve health and no-helper probe':'NOT_RUN',sameVolumeRecreation:mode==='serve'?'PASS':'covered by separate acceptance',identity,sizes,fresh:{httpMs:summary(fresh.map(x=>x.httpMs)),dockerHealthMs:summary(fresh.map(x=>x.healthMs))},reusedSynthetic:{httpMs:summary(reused.map(x=>x.httpMs)),dockerHealthMs:summary(reused.map(x=>x.healthMs))},shutdownMs:summary(shutdown),restart,healthCommandMs:summary(health),idle:{rssKiB:after.rssKiB,cpuTicks:Number(after.cpuUserTicks)+Number(after.cpuSystemTicks)-Number(before.cpuUserTicks)-Number(before.cpuSystemTicks),seconds:idleSeconds,clockTicksPerSecond:100},acquisition:'NOT_MEASURED_IN_BOOT; prebuilt local image',build:'separate qualification record'};
 results.push(item);console.log(JSON.stringify(item));
 }finally{if(f)await cleanup(f);}}
 check(results[0].identity.sourceRevision===results[1].identity.sourceRevision,'comparison_source_mismatch');check(results[0].sizes.binarySHA256===results[1].sizes.binarySHA256,'comparison_binary_mismatch');console.log(JSON.stringify({benchmark:'PASS',mode,samplesPerImage:count*2,exactEndUserClients:'NOT_RUN',nativeAMD64:process.arch==='x64'?'requires native runner attestation':'NOT_RUN'}));
}finally{await browser.close();await cleanupRun(process.env.WA_DOCKER_RUN_ID);}
}
main().catch(()=>{console.log(JSON.stringify({benchmark:'FAIL',code:'bounded_benchmark_failed'}));process.exitCode=1;});
