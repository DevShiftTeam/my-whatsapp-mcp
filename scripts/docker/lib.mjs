import {spawn} from 'node:child_process';
import {createServer} from 'node:net';
import {randomUUID} from 'node:crypto';
export const binary='/usr/local/bin/wa-indexer', root='/var/lib/wa-indexer/demo';
export const pause=ms=>new Promise(r=>setTimeout(r,ms));
export function check(ok,code){if(!ok)throw new Error(code);}
export function run(cmd,args,{allowFailure=false,timeout=60000,input}={}){
 return new Promise((resolve,reject)=>{const p=spawn(cmd,args,{stdio:['pipe','pipe','pipe']});let out='',err='';let over=false;
 const timer=setTimeout(()=>{over=true;p.kill('SIGKILL')},timeout);
 p.stdout.on('data',b=>{out+=b;if(out.length>4*1024*1024){over=true;p.kill('SIGKILL')}});p.stderr.on('data',b=>{err+=b;if(err.length>4*1024*1024){over=true;p.kill('SIGKILL')}});
 p.on('error',()=>{clearTimeout(timer);reject(new Error('subprocess_start_failed'))});p.on('close',code=>{clearTimeout(timer);if(over||code!==0&&!allowFailure)reject(new Error('subprocess_failed_'+cmd+'_'+String(args[0]).replace(/[^a-z]/g,'')));else resolve({out,err,code})});p.stdin.end(input);
 });
}
export const docker=(args,options)=>run('docker',args,options);
export async function freePort(){const s=createServer();await new Promise(r=>s.listen(0,'127.0.0.1',r));const p=s.address().port;await new Promise(r=>s.close(r));return p;}
export async function waitReady(name,origin,start=performance.now()){
 let httpMs,healthMs;const deadline=Date.now()+30000;
 while(Date.now()<deadline){if(httpMs===undefined){try{const r=await fetch(origin+'/healthz',{signal:AbortSignal.timeout(500)});if(r.ok&&await r.text()==='ready\n')httpMs=performance.now()-start;}catch{}}
 const s=JSON.parse((await docker(['inspect',name,'--format','{{json .State}}'])).out);if(!s.Running)throw new Error('container_exited_before_ready');if(s.Health?.Status==='healthy')healthMs??=performance.now()-start;
 if(httpMs!==undefined&&healthMs!==undefined)return {httpMs,healthMs};await pause(50);}
 throw new Error('readiness_timeout');
}
export function singleApplicationProcess(output) {
 const lines=output.trim().split('\n');
 return lines.length===2&&/^PID\s+COMMAND$/.test(lines[0].trim())&&/^\d+\s+wa-indexer$/.test(lines[1].trim());
}
export function fixtureMode(mode='demo') {
 check(mode==='demo'||mode==='serve','invalid_fixture_mode');
 return {mode,root:'/var/lib/wa-indexer/'+(mode==='demo'?'demo':'data'),healthArgs:mode==='demo'?['--demo']:[]};
}
export async function createFixture(image,{volume,name,port,mode='demo'}={}){
 const selected=fixtureMode(mode);
 name??='wa-task4-'+randomUUID().slice(0,12);volume??=name+'-data';port??=await freePort();const origin='http://127.0.0.1:'+port;
 check(name.startsWith('wa-task4-')&&volume.startsWith('wa-task4-'),'unsafe_fixture_name');
 await docker(['volume','create','--label','wa.task4.run='+(process.env.WA_DOCKER_RUN_ID||'local'),volume]);
 const config={name,services:{app:{image,labels:{'wa.task4.run':process.env.WA_DOCKER_RUN_ID||'local'},container_name:name,read_only:true,cap_drop:['ALL'],security_opt:['no-new-privileges:true'],tmpfs:['/tmp:rw,noexec,nosuid,size=16m,mode=1777'],stop_grace_period:'10s',ports:['127.0.0.1:'+port+':8123'],volumes:[volume+':/var/lib/wa-indexer'],healthcheck:{test:['CMD',binary,'health',...selected.healthArgs,'--data-dir',selected.root,'--connect','127.0.0.1:8123'],start_period:'30s',start_interval:'1s',interval:'30s',timeout:'3s',retries:3},command:[mode,...(mode==='demo'?['--demo-acceptance-fixtures','--mcp-listen','127.0.0.1:6968','--mcp-public-url','https://docker-synthetic.invalid/mcp']:[]),'--container','--browser-origin',origin,'--docker-container-name',name,'--data-dir',selected.root]}},networks:{default:{labels:{'wa.task4.run':process.env.WA_DOCKER_RUN_ID||'local'}}},volumes:{[volume]:{external:true,name:volume}}};
 await docker(['compose','-p',name,'-f','-','create'],{input:JSON.stringify(config)});return {name,volume,port,origin,image,config,...selected};
}
export async function start(f){const t=performance.now();await docker(['start',f.name]);return waitReady(f.name,f.origin,t);}
export async function stop(f){const t=performance.now();await docker(['stop','--time','10',f.name]);const state=JSON.parse((await docker(['inspect',f.name,'--format','{{json .State}}'])).out);check(state.ExitCode===0&&!state.OOMKilled,'shutdown_not_clean');return performance.now()-t;}
export async function cleanup(f,{volume=true}={}){if(!f?.name?.startsWith('wa-task4-'))return;await docker(['rm','-f',f.name],{allowFailure:true});await docker(['network','rm',f.name+'_default'],{allowFailure:true});if(volume)await docker(['volume','rm',f.volume],{allowFailure:true});}
export async function ownerLink(f){const s=(await docker(['exec',f.name,binary,'owner-link',...fixtureMode(f.mode).healthArgs,'--data-dir',f.root||root,'--connect','127.0.0.1:8123'])).out;const url=s.match(/http:\/\/[^\s]+/u)?.[0];check(url?.startsWith(f.origin+'/#bootstrap='),'owner_link_origin');return url;}
export async function api(page,path,method='GET',body){return page.evaluate(async({path,method,body})=>{
 const csrf=sessionStorage.getItem('wa_archive_owner_capability');const r=await fetch(path,{method,headers:{'Content-Type':'application/json','X-CSRF-Token':csrf||''},body:method==='GET'?undefined:JSON.stringify(body??{})});return {status:r.status,value:await r.json()};
 },{path,method,body});}
export class SDK {
 constructor(path){this.process=spawn(path,[],{stdio:['pipe','pipe','ignore']});this.pending=[];this.buf='';this.process.stdout.on('data',b=>{this.buf+=b;for(;;){const i=this.buf.indexOf('\n');if(i<0)break;const line=this.buf.slice(0,i);this.buf=this.buf.slice(i+1);const next=this.pending.shift();if(next){clearTimeout(next.timer);next.resolve(JSON.parse(line));}}});this.process.on('exit',()=>{for(const p of this.pending){clearTimeout(p.timer);p.reject(new Error('sdk_driver_exited'));}});}
 request(value){return new Promise((resolve,reject)=>{const timer=setTimeout(()=>{this.process.kill('SIGKILL');reject(new Error('sdk_timeout'))},value.op==='call'&&['send_message','send_media','send_voice_note'].includes(value.name)?70000:20000);this.pending.push({resolve,reject,timer});this.process.stdin.write(JSON.stringify(value)+'\n')});}
 async close(){await this.request({op:'close'});this.process.stdin.end();}
}
export async function helperWithOptions(f,op,args,{execute=docker,timeout=60000}={}){
 const name='wa-task4-helper-'+randomUUID();
 try{return await execute(['run','--name',name,'--label','wa.task4.run='+(process.env.WA_DOCKER_RUN_ID||name),'--rm','--read-only',...(op==='attachment-download'?['--network=container:'+f.name]:['--network=none']),'--cap-drop=ALL','--security-opt=no-new-privileges:true',...(op==='metrics'?['--pid=container:'+f.name]:[]),'-v',f.volume+':/var/lib/wa-indexer',process.env.WA_DOCKER_HELPER_IMAGE||'wa-task4-helper:development',op,...args],{timeout});}
 finally{await execute(['rm','-f',name],{allowFailure:true,timeout:10000});}
}
export async function helper(f,op,...args){return helperWithOptions(f,op,args);}

export async function imageIdentity(reference){
 check(typeof reference==='string'&&/^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,255}$/.test(reference),'invalid_image_reference');
 const image=JSON.parse((await docker(['image','inspect',reference])).out)[0];
 const source=image.Config.Labels?.['org.opencontainers.image.revision'];
 const harnessRevision=(await run('git',['rev-parse','HEAD'])).out.trim();
 return {imageID:image.Id,repoDigests:image.RepoDigests||[],sourceRevision:source,platform:image.Os+'/'+image.Architecture,harnessRevision,unpackedBytes:image.Size};
}
export function option(name,fallback){const i=process.argv.indexOf(name);return i<0?fallback:process.argv[i+1];}

export async function cleanupRun(id){
 check(/^[a-f0-9-]{36}$/.test(id),'invalid_cleanup_id');
 for(const [type,listing] of [['container',['ps','-aq']],['network',['network','ls','-q']],['volume',['volume','ls','-q']]]){
 const ids=(await docker([...listing,'--filter','label=wa.task4.run='+id])).out.trim().split(/\s+/).filter(Boolean);
 for(const resource of ids)await docker(type==='container'?['rm','-f',resource]:[type,'rm',resource],{allowFailure:true});
 }
}
