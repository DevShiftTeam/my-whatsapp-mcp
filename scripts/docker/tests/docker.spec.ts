import {createHash} from 'node:crypto';
import {request as httpRequest} from 'node:http';
import {test,expect} from '../../../docs/.vitepress/node_modules/@playwright/test/index.mjs';
import {binary,root,check,docker,createFixture,start,stop,cleanup,ownerLink,api,SDK,helper,pause,freePort,waitReady} from '../lib.mjs';
const reads=['search_messages','get_context','list_chats','get_index_status','search_contacts','search_groups','search_attachments','get_attachment','get_send_status'];
const mutations=['send_message','send_media','send_voice_note'];
const tools=[...reads,...mutations].sort();
const argsFor=(name,meeting)=>name==='search_messages'?{query:'Atlas'}:name==='get_context'?{message_id:meeting.MessageID,before:1,after:1}:['search_contacts','search_groups'].includes(name)?{query:'Atlas'}:name==='get_attachment'?{attachment_id:'unknown-synthetic'}:name==='get_send_status'?{request_id:'synthetic-denial'}:name==='send_message'?{chat_id:meeting.ChatID,request_id:'synthetic-denial',text:'Synthetic denial only'}:name==='send_media'?{chat_id:meeting.ChatID,request_id:'synthetic-media-denial',kind:'document',data_base64:'c3ludGhldGlj',mime_type:'text/plain',filename:'synthetic.txt'}:name==='send_voice_note'?{chat_id:meeting.ChatID,request_id:'synthetic-voice-denial',data_base64:'T2dnUw==',seconds:1}:{};
const unwrap=r=>{check(r.ok&&!r.value?.isError,'sdk_call_failed');return r.value.structuredContent};
const denied=r=>!r.ok||r.value?.isError===true;
test('private Compose preview browser and native SDK',async({browser})=>{
 let f,ctx,page,sdk,expirySDK,wrongSDK,documentSDK;let phase='create';
 const stage=v=>{phase=v;console.log(JSON.stringify({phase:v}));};
 try{
 f=await createFixture(process.env.WA_DOCKER_IMAGE||'wa-task4:development');await start(f);
 ctx=await browser.newContext({timezoneId:'UTC'});page=await ctx.newPage();
 stage('bootstrap_policy_demo');const firstLink=await ownerLink(f);await page.goto(firstLink);await page.locator('#settings-dialog[open]').waitFor();
 check(!page.url().includes('bootstrap'),'bootstrap_fragment_removed');
 await page.locator('input[name=date-mode][value=all]').check();await page.locator('#save-policy').click();
 const loaded=page.waitForResponse(response=>response.url().endsWith('/api/pairing/start')&&response.request().method()==='POST');
 await page.locator('#pairing-start').click();check((await loaded).status()===200,'synthetic_load_response');
 await expect(page.locator('#pairing-feedback')).toContainText('Synthetic messages loaded.');await page.locator('#connection-close').click();
 for(const query of ['Project Atlas','פגישה']){
  await page.getByRole('button',{name:'Archive',exact:true}).click();
  await page.locator('#search-query').fill(query);
  const searched=page.waitForResponse(response=>response.url().endsWith('/api/search')&&response.request().method()==='POST'&&response.request().postDataJSON()?.query===query);
  await page.locator('#search-submit').click();check((await searched).status()===200,'browser_search_response');
  await expect(page.locator('#search-results')).toContainText(query);check((await page.locator('#search-results').innerText()).includes(query),'browser_search');
 }
 const ownerSearch=async query=>{const r=await api(page,'/api/search','POST',{query,limit:50});check(r.status===200,'owner_search');return r.value.Hits||[]};
 const meeting=(await ownerSearch('פגישה'))[0];const travel=(await ownerSearch('Travel'))[0];check(meeting&&travel,'demo_opaque_ids');
 check((await ownerSearch('FILTERED_DATE_CANARY')).length===1,'old_fixture_present');
 for(const q of ['OLD_EDIT_CANARY','DELETED_CANARY'])check((await ownerSearch(q)).length===0,'edit_delete_filter');
 await page.reload();await page.getByRole('button',{name:'Archive',exact:true}).click();await page.locator('#search-query').waitFor();stage('ui_private_grant');await page.locator('#clients-open').click();stage('grant_panel');await page.locator('#client-create-panel summary').click();await page.locator('#client-label').fill('Docker synthetic acceptance');
 stage('grant_chat');await page.locator('#client-chat-choices input[value="'+meeting.ChatID+'"]').check();stage('grant_dates');await page.locator('#client-from').fill('2026-10-05');await page.locator('#client-through').fill('2026-10-05');
 await page.locator('#client-expiry').fill(new Date(Date.now()+86400000).toISOString().slice(0,16));stage('grant_submit');let availability;for(let i=0;i<100;i++){availability=await api(page,'/api/connections');if(availability.value.local_state==='ready')break;await pause(100);}check(availability.value.local_state==='ready','native_transport_unavailable');await page.locator('#client-create').click();
 let grant;for(let i=0;i<50;i++){const r=await api(page,'/api/connections');grant=r.value.items?.find(x=>x.label==='Docker synthetic acceptance');if(grant)break;await pause(100);}check(grant,'grant_created');
 const configFor=async id=>{const r=await api(page,'/api/connections/guide','POST',{id,client:'codex_cli',mode:'local'});check(r.status===200,'guide_ready');check(r.value.configuration.includes('docker')&&r.value.configuration.includes(f.name),'docker_guide');const p=r.value.configuration.match(/\/var\/lib\/wa-indexer\/[^"'\s]+\.json/u)?.[0];check(p,'private_config_path');return p;};
 const config=await configFor(grant.id);await page.locator('#clients-close').click();
 sdk=new SDK(process.env.WA_DOCKER_SDK_HELPER);check((await sdk.request({op:'connect',container:f.name,config})).ok,'sdk_connect');
 stage('twelve_tools_scope');const listed=await sdk.request({op:'list'});check(listed.ok,'sdk_list');check(JSON.stringify(listed.value.tools.map(t=>t.name).sort())===JSON.stringify(tools),'exact_twelve_tools');for(const t of listed.value.tools){const mutation=mutations.includes(t.name);check(t.annotations.readOnlyHint===!mutation&&(mutation?t.annotations.destructiveHint===true:t.name==='get_send_status'?t.annotations.destructiveHint===false:t.annotations.destructiveHint===undefined||t.annotations.destructiveHint===false)&&t.annotations.openWorldHint===mutation,'exact_tool_annotations');check(t.inputSchema.type==='object'&&t.inputSchema.additionalProperties===false,'strict_tool_schema');}for(const name of ['send_media','send_voice_note'])check(listed.value.tools.find(t=>t.name===name).inputSchema.properties.data_base64.maxLength===11184812,'large_media_schema_bound');for(const name of ['search_contacts','search_groups']){const properties=listed.value.tools.find(t=>t.name===name).inputSchema.properties;check(properties.query.maxLength===256&&properties.limit.maximum===50,'name_schema_bound');}check(listed.value.tools.find(t=>t.name==='get_context').inputSchema.properties.before.maximum===10,'context_schema_bound');check(listed.value.tools.find(t=>t.name==='search_attachments').inputSchema.properties.limit.maximum===20,'attachment_schema_bound');
 const call=(name,args={})=>sdk.request({op:'call',name,args});
 for(const name of mutations)check(denied(await call(name,argsFor(name,meeting))),'send_default_deny');
 for(const name of ['search_contacts','search_groups']){const result=unwrap(await call(name,{query:'Travel'}));check((result.chats||[]).length===0,'excluded_name_scope');}
 const attachmentSearch=unwrap(await call('search_attachments',{query:'SYNTHETIC_DOCUMENT_ACCEPTANCE'}));check((attachmentSearch.attachments||[]).length===0,'excluded_document_scope');
 const documentHit=(await ownerSearch('SYNTHETIC_DOCUMENT_ACCEPTANCE'))[0];check(documentHit,'retained_document_fixture');
 const documentExpiry=Date.now()+10000;const documentGrant=await api(page,'/api/connections/create','POST',{client:'codex_cli',label:'Docker document expiry',mode:'local',all_chats:false,chats:[documentHit.ChatID],expires_at:new Date(documentExpiry).toISOString()});check(documentGrant.status===200,'document_grant');
 documentSDK=new SDK(process.env.WA_DOCKER_SDK_HELPER);check((await documentSDK.request({op:'connect',container:f.name,config:await configFor(documentGrant.value.id)})).ok,'document_sdk');
 const docs=unwrap(await documentSDK.request({op:'call',name:'search_attachments',args:{query:'SYNTHETIC_DOCUMENT_ACCEPTANCE'}}));check(docs.attachments?.length===1,'retained_document_search');const documentID=docs.attachments[0].attachment_id;
 check(denied(await call('get_attachment',{attachment_id:documentID})),'retained_document_out_of_scope');
 const original=unwrap(await documentSDK.request({op:'call',name:'get_attachment',args:{attachment_id:documentID,inline:true}}));check(original.status==='ready'&&original.download_url,'retained_document_ready');
 const expectedDocument='%PDF-1.7\n% SYNTHETIC_DOCUMENT_ACCEPTANCE\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n';const download=JSON.parse((await helper(f,'attachment-download',original.download_url)).out);check(download.status===200&&download.sha256===createHash('sha256').update(expectedDocument).digest('hex'),'retained_original_download');
 await pause(Math.max(0,documentExpiry-Date.now()+150));check(JSON.parse((await helper(f,'attachment-download',original.download_url)).out).status!==200,'expired_original_denied');await documentSDK.close();documentSDK=null;
 const revocableDocument=await api(page,'/api/connections/create','POST',{client:'codex_cli',label:'Docker document revocation',mode:'local',all_chats:false,chats:[documentHit.ChatID],expires_at:new Date(Date.now()+86400000).toISOString()});check(revocableDocument.status===200,'revocable_document_grant');documentSDK=new SDK(process.env.WA_DOCKER_SDK_HELPER);check((await documentSDK.request({op:'connect',container:f.name,config:await configFor(revocableDocument.value.id)})).ok,'revocable_document_sdk');const revocableOriginal=unwrap(await documentSDK.request({op:'call',name:'get_attachment',args:{attachment_id:documentID}}));const documentConnection=(await api(page,'/api/connections')).value.items.find(item=>item.id===revocableDocument.value.id);check((await api(page,'/api/connections/revoke','POST',{id:documentConnection.id,revision:documentConnection.revision})).status===200,'document_revoke');check(JSON.parse((await helper(f,'attachment-download',revocableOriginal.download_url)).out).status!==200,'revoked_original_denied');await documentSDK.close();documentSDK=null;
 check(denied(await call('get_attachment',{attachment_id:'guessed-unknown-id'})),'unknown_attachment_denied');
 for(const query of ['Project Atlas','פגישה']){const r=unwrap(await call('search_messages',{query}));check(r.untrusted===true&&r.hits?.length>0&&r.hits.every(h=>h.chat_id===meeting.ChatID),'sdk_bilingual_scope');}
 for(const query of ['Travel','FILTERED_DATE_CANARY','DELETED_CANARY','OLD_EDIT_CANARY'])check((unwrap(await call('search_messages',{query})).hits||[]).length===0,'excluded_query');
 check(denied(await call('get_context',{message_id:travel.MessageID,before:1,after:1})),'excluded_context');
 check(denied(await call('get_context',{message_id:'guessed-unknown-id',before:1,after:1})),'guessed_context');
 check(unwrap(await call('get_context',{message_id:meeting.MessageID,before:1,after:1})).messages.length>0,'allowed_context');
 const chatList=unwrap(await call('list_chats'));check(chatList.chats.length===1&&chatList.chats[0].chat_id===meeting.ChatID,'scoped_chat_list');const status=unwrap(await call('get_index_status'));check(status.chats.length===1&&status.chats[0].chat_id===meeting.ChatID&&status.message_count>0,'scoped_status');
 stage('wrong_credential');const wrong=(await helper(f,'wrong',config)).out.trim();wrongSDK=new SDK(process.env.WA_DOCKER_SDK_HELPER);const wrongConnect=await wrongSDK.request({op:'connect',container:f.name,config:wrong});if(wrongConnect.ok)check(denied(await wrongSDK.request({op:'call',name:'get_index_status',args:{}})),'wrong_token_denied');await wrongSDK.close();wrongSDK=null;await helper(f,'remove-wrong',wrong);
 stage('expiry_existing_session');const expires=Date.now()+7000;const exp=await api(page,'/api/connections/create','POST',{client:'codex_cli',label:'Docker expiry',mode:'local',all_chats:false,chats:[meeting.ChatID],expires_at:new Date(expires).toISOString()});check(exp.status===200,'expiring_grant_created');expirySDK=new SDK(process.env.WA_DOCKER_SDK_HELPER);check((await expirySDK.request({op:'connect',container:f.name,config:await configFor(exp.value.id)})).ok,'expiry_connect');unwrap(await expirySDK.request({op:'call',name:'get_index_status',args:{}}));await pause(Math.max(0,expires-Date.now()+150));for(const name of tools)check(denied(await expirySDK.request({op:'call',name,args:argsFor(name,meeting)})),'expired_all_tools');await expirySDK.close();expirySDK=null;
 stage('narrow_policy_existing_session');const setup=await api(page,'/api/setup');const policy=setup.value.Policy||setup.value.policy;check((await api(page,'/api/policy','PUT',{expected_generation:policy.Generation,date_mode:'since',import_start:'2026-10-05T12:02:00Z',chat_mode:'selective',included_chats:[meeting.ChatID],rolling_retention_days:0})).status===200,'policy_narrowed');
 check(denied(await call('get_context',{message_id:meeting.MessageID,before:1,after:1})),'old_context_denied');check((unwrap(await call('search_messages',{query:'פגישה'})).hits||[]).length===0,'narrowed_search');
 stage('revocation_existing_session');check((await api(page,'/api/connections/revoke','POST',{id:grant.id,revision:grant.revision})).status===200,'revoke');for(const name of tools)check(denied(await call(name,argsFor(name,meeting))),'revoked_all_tools');await sdk.close();sdk=null;
 const top=(await docker(['top',f.name])).out;check(!top.includes('mcp stdio'),'stdio_disconnect_cleanup');
 stage('persistence_private_roots');const persistent=await api(page,'/api/connections/create','POST',{client:'codex_cli',label:'Docker persistent',mode:'local',all_chats:false,chats:[meeting.ChatID],expires_at:new Date(Date.now()+86400000).toISOString()});check(persistent.status===200,'persistent_grant');const persistentConfig=await configFor(persistent.value.id);await helper(f,'sentinel');const privateBefore=JSON.parse((await helper(f,'inspect')).out);check(privateBefore.private&&privateBefore.sentinel,'private_modes_before');
 const stale=await ownerLink(f);const before=await api(page,'/api/setup');const clients=await api(page,'/api/connections');const messages=await ownerSearch('Project Atlas');
 check(JSON.parse((await helper(f,'lock')).out).locked,'instance_lock');
 await stop(f);await cleanup(f,{volume:false});f=await createFixture(f.image,{name:f.name,volume:f.volume,port:f.port});await start(f);
 const staleCtx=await browser.newContext();const stalePage=await staleCtx.newPage();await stalePage.goto(stale);await pause(200);check((await api(stalePage,'/api/setup')).status===401,'stale_link_denied');await staleCtx.close();
 await ctx.close();ctx=await browser.newContext();page=await ctx.newPage();await page.goto(await ownerLink(f));await page.getByRole('button',{name:'Archive',exact:true}).click();await page.locator('#search-query').waitFor();
 const after=await api(page,'/api/setup');check(JSON.stringify(after.value.Policy||after.value.policy)===JSON.stringify(before.value.Policy||before.value.policy),'policy_persisted');check(JSON.stringify((await api(page,'/api/connections')).value.items)===JSON.stringify(clients.value.items),'clients_persisted');check(JSON.stringify(await ownerSearch('Project Atlas'))===JSON.stringify(messages),'messages_persisted');const privateAfter=JSON.parse((await helper(f,'inspect')).out);check(privateAfter.private&&privateAfter.sentinel,'models_private_persisted');
 sdk=new SDK(process.env.WA_DOCKER_SDK_HELPER);check((await sdk.request({op:'connect',container:f.name,config:persistentConfig})).ok,'persistent_config_reconnect');unwrap(await sdk.request({op:'call',name:'get_index_status',args:{}}));for(const name of mutations)check(denied(await sdk.request({op:'call',name,args:argsFor(name,meeting)})),'send_default_deny_after_recreation');await sdk.close();sdk=null;await stop(f);await start(f);check(JSON.parse((await helper(f,'inspect')).out).private,'restart_private');
 stage('routine_logs_capability_free');const logs=(await docker(['logs',f.name])).out;check(!logs.includes('bootstrap=')&&!logs.includes('owner_session')&&!logs.includes('FILTERED_DATE_CANARY'),'routine_logs_private');
 stage('passed');
 }catch(error){console.log(JSON.stringify({phase:'failure_'+phase+'_'+(/^[a-z_]+$/.test(error.message)?error.message:'ui_action')}));throw new Error('docker_check_failed_'+phase,{cause:undefined});}
 finally{for(const s of [sdk,expirySDK,wrongSDK,documentSDK]){if(s)try{await s.close()}catch{s.process.kill('SIGKILL')}}if(ctx)await ctx.close();if(f)await cleanup(f);}
});

test('foreign bind mounts are refused without repair',async()=>{
 const {randomUUID}=await import('node:crypto');
 for(const kind of ['bad-owner','bad-nonempty','bad-symlink']){
  console.log(JSON.stringify({phase:kind.replaceAll('-','_')+'_setup'}));
  const name='wa-task4-bind-'+randomUUID().slice(0,12),volume=name+'-data';
  const helperImage=process.env.WA_DOCKER_HELPER_IMAGE||'wa-task4-helper:development';
  try{
   // Bind a task-owned daemon-native directory. Desktop host shares synthesize
   // ownership across users and cannot prove Linux wrong-owner refusal.
   await docker(['volume','create','--label','wa.task4.run='+(process.env.WA_DOCKER_RUN_ID||'local'),volume]);
   const dir=(await docker(['volume','inspect',volume,'--format','{{.Mountpoint}}'])).out.trim();
   check(dir.startsWith('/var/lib/docker/volumes/'),'daemon_volume_mountpoint');
   const mount=['--mount','type=bind,src='+dir+',dst=/var/lib/wa-indexer'];
   await docker(['run','--rm','--network=none','--user','0:0',...mount,helperImage,kind]);
   const hash=async()=>(await docker(['run','--rm','--network=none','--user','0:0',...mount,helperImage,'hash'])).out;
   const before=await hash();const result=await docker(['run','--name',name,'--label','wa.task4.run='+(process.env.WA_DOCKER_RUN_ID||'local'),'--network=none','--read-only','--cap-drop=ALL','--security-opt=no-new-privileges:true','--tmpfs','/tmp:rw,noexec,nosuid,size=16m,mode=1777',...mount,process.env.WA_DOCKER_IMAGE||'wa-task4:development','demo','--container','--browser-origin','http://127.0.0.1:8123','--data-dir',root],{allowFailure:true,timeout:5000});check(result.code!==0,'foreign_mount_accepted');check(await hash()===before,'foreign_mount_changed');
  }finally{await docker(['rm','-f',name],{allowFailure:true});await docker(['volume','rm',volume],{allowFailure:true});}
 }
});

test('normal source-preview Compose is ready before pairing',async()=>{
 const {randomUUID}=await import('node:crypto');
 const name='wa-task4-normal-'+randomUUID().slice(0,12),port=await freePort();
 const overlay={services:{'wa-indexer':{image:process.env.WA_DOCKER_IMAGE,container_name:name,command:['serve','--container','--browser-origin','http://127.0.0.1:'+port,'--docker-container-name',name,'--data-dir','/var/lib/wa-indexer/data'],ports:[],labels:{'wa.task4.run':process.env.WA_DOCKER_RUN_ID}}},volumes:{wa_data:{labels:{'wa.task4.run':process.env.WA_DOCKER_RUN_ID}}},networks:{default:{labels:{'wa.task4.run':process.env.WA_DOCKER_RUN_ID}}}};
 // Environment interpolation supplies the main Compose's loopback port and exact command.
 const oldPort=process.env.WA_INDEXER_PORT,oldName=process.env.WA_INDEXER_CONTAINER_NAME;
 process.env.WA_INDEXER_PORT=String(port);process.env.WA_INDEXER_CONTAINER_NAME=name;
 try{
  await docker(['compose','-p',name,'-f','compose.yaml','-f','-','create'],{input:JSON.stringify(overlay)});
  await docker(['start',name]);await waitReady(name,'http://127.0.0.1:'+port);
  check((await fetch('http://127.0.0.1:'+port+'/api/setup')).status===401,'normal_owner_auth_required');
  const wrongHost=await new Promise<number>((resolve,reject)=>{const r=httpRequest('http://127.0.0.1:'+port+'/healthz',{headers:{host:'unexpected.invalid'}},response=>{response.resume();resolve(response.statusCode||0)});r.on('error',reject);r.setTimeout(3000,()=>r.destroy());r.end();});check(wrongHost===403,'normal_host_authority');
  const logs=await docker(['logs',name]);check(!(logs.out+logs.err).includes('bootstrap='),'normal_capability_free_logs');
 }finally{
  await docker(['rm','-f',name],{allowFailure:true});await docker(['network','rm',name+'_default'],{allowFailure:true});await docker(['volume','rm',name+'_wa_data'],{allowFailure:true});
  if(oldPort===undefined)delete process.env.WA_INDEXER_PORT;else process.env.WA_INDEXER_PORT=oldPort;if(oldName===undefined)delete process.env.WA_INDEXER_CONTAINER_NAME;else process.env.WA_INDEXER_CONTAINER_NAME=oldName;
 }
});
