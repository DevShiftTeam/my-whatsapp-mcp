// Developer-only acceptance coordinator. A separately reviewed, exact-client
// executable owns provider/device operations; no account provisioning is built in.
import { readFile, lstat, realpath, open, mkdtemp, rm, rmdir, chmod } from 'node:fs/promises';
import { constants } from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import { createHash, randomUUID } from 'node:crypto';
import { spawn } from 'node:child_process';
import { isDeepStrictEqual } from 'node:util';
import { tmpdir } from 'node:os';
import { isIP } from 'node:net';
const schema = JSON.parse(await readFile(new URL('./report-schema.json', import.meta.url)));
const fixture = JSON.parse(await readFile(new URL('./fixture.json', import.meta.url)));
const providers = schema.properties.provider.enum;
const fail = () => { throw new Error('remote_acceptance_invalid_input'); };
const hash = value => 'sha256:' + createHash('sha256').update(value).digest('hex');
const exact = (value, keys) => { if (!value || typeof value !== 'object' || Array.isArray(value) || Object.keys(value).some(k => !keys.includes(k))) fail(); };

// Deliberately limited validator for the committed schema vocabulary. The JSON
// schema is also consumable by a standard draft-2020-12 validator.
function validate(value, rule) {
 if (rule.anyOf && !rule.anyOf.some(r => { try { validate(value,r);return true; } catch { return false; } })) fail();
 if (rule.const !== undefined && value !== rule.const || rule.enum && !rule.enum.includes(value)) fail();
 if (rule.type && !(rule.type === 'null' ? value === null : rule.type === 'array' ? Array.isArray(value) : rule.type === 'object' ? value && typeof value === 'object' && !Array.isArray(value) : typeof value === rule.type)) fail();
 if (typeof value === 'string' && (rule.minLength && value.length < rule.minLength || rule.maxLength && value.length > rule.maxLength || rule.pattern && !new RegExp(rule.pattern).test(value))) fail();
 if (Array.isArray(value)) { if (rule.minItems && value.length < rule.minItems || rule.maxItems !== undefined && value.length > rule.maxItems || rule.uniqueItems && new Set(value).size !== value.length) fail(); if(rule.items) for(const item of value) validate(item,rule.items); }
 if(value && typeof value === 'object' && !Array.isArray(value)) { for(const key of rule.required || []) if(!Object.hasOwn(value,key)) fail(); for(const [key,item] of Object.entries(value)) { if(rule.additionalProperties === false && !Object.hasOwn(rule.properties || {},key)) fail(); if(rule.properties?.[key]) validate(item,rule.properties[key]); } }
 for(const r of rule.allOf || []) validate(value,r);
 if(rule.if) { let matches=true;try { validate(value,rule.if); } catch { matches=false; } if(matches) validate(value,rule.then); }
}
export function validateReport(report) { validate(report,schema); const date=new Date(report.testedAt+'T00:00:00Z');if(!Number.isFinite(+date)||date.toISOString().slice(0,10)!==report.testedAt) fail();return report; }
export function requiredChecks(provider,browser) { return [...fixture.checks,...(provider==='tailscale_serve'?fixture.privateChecks:fixture.publicChecks),...(browser?fixture.phoneChecks:[]),...(browser&&provider==='ngrok'?['ngrok_html_interstitial_observed']:[])]; }
export function baseReport(provider,browser=false) { if(!providers.includes(provider)) fail();return {schemaVersion:1,fixture:fixture.id,testedAt:new Date().toISOString().slice(0,10),provider,browser,outcome:'NOT_RUN',identity:null,missing:['missing_build'],checks:{}}; }

async function privatePath(file,directory=false) {
 if(typeof file!=='string'||!path.isAbsolute(file)||path.normalize(file)!==file||await realpath(file)!==file) fail();
 let at=path.parse(file).root;for(const part of file.slice(at.length).split(path.sep).filter(Boolean)){at=path.join(at,part);if((await lstat(at)).isSymbolicLink())fail();}
 const stat=await lstat(file);if(stat.uid!==process.getuid?.() || (stat.mode&0o777)!==(directory?0o700:0o600) || (directory?!stat.isDirectory():!stat.isFile()) || !directory&&stat.nlink!==1) fail();
 return stat;
}
export async function readPrivateJSON(file) { await privatePath(path.dirname(file),true);const stat=await privatePath(file);if(stat.size>65536) fail();const fd=await open(file,'r');try {const after=await fd.stat();if(after.ino!==stat.ino||after.dev!==stat.dev||after.size>65536)fail();return JSON.parse(await fd.readFile('utf8'));}finally{await fd.close();} }
export async function writeReport(dir,report) { validateReport(report);await privatePath(dir,true);const fd=await open(path.join(dir,'report.json'),'wx',0o600);try {await fd.writeFile(JSON.stringify(report,null,2)+'\n');}finally{await fd.close();} }

// Every directory entry must be protected against replacement by other users.
// Root-owned sticky temporary directories are safe ancestors of an owned entry;
// ordinary shared writable directories are not, even above a private directory.
async function trustedAncestors(directory) {
 if(typeof directory!=='string'||!path.isAbsolute(directory)||path.normalize(directory)!==directory||await realpath(directory)!==directory)fail();
 const uid=process.getuid?.();let current=path.parse(directory).root;
 for(const part of ['',...directory.slice(current.length).split(path.sep).filter(Boolean)]){
  if(part)current=path.join(current,part);
  const stat=await lstat(current);
  if(!stat.isDirectory()||stat.isSymbolicLink()||![0,uid].includes(stat.uid))fail();
  if(stat.mode&0o022 && !(stat.uid===0 && stat.mode&0o1000))fail();
 }
}
async function reviewedHarnessBytes(harness) {
 if(typeof harness.file!=='string'||!path.isAbsolute(harness.file)||path.normalize(harness.file)!==harness.file)fail();
 await trustedAncestors(path.dirname(harness.file));
 const fd=await open(harness.file,constants.O_RDONLY|constants.O_NOFOLLOW|constants.O_NONBLOCK);
 try{
  const stat=await fd.stat(),maximum=16*1024*1024;
  if(!stat.isFile()||stat.uid!==process.getuid?.()||stat.nlink!==1||stat.mode&0o022||!(stat.mode&0o100)||stat.size>maximum)fail();
  // A growing file cannot turn a prior stat size check into an unbounded read.
  const bytes=Buffer.alloc(maximum+1);let length=0;
  while(length<bytes.length){const {bytesRead}=await fd.read(bytes,length,bytes.length-length,null);if(!bytesRead)break;length+=bytesRead;}
  const approved=bytes.subarray(0,length);
  if(length>maximum||hash(approved)!==harness.digest)fail();
  return approved;
 }finally{await fd.close();}
}
export async function withHarnessSnapshot(harness,directory,operation) {
 await trustedAncestors(directory);await privatePath(directory,true);
 // Read and hash from one validated descriptor, then execute only those bytes.
 // Later source-path replacement cannot change this owner-only snapshot.
 const bytes=await reviewedHarnessBytes(harness);let dir;
 try{
  dir=await mkdtemp(path.join(directory,'.remote-harness-'));
  const executable=path.join(dir,'harness');
  const fd=await open(executable,'wx',0o500);
  try{await fd.writeFile(bytes);}finally{await fd.close();}
  return await operation(executable);
 }finally{
  bytes.fill(0);
  // Only two owned entries: no recursive traversal of harness-created content.
  if(dir){try{await rm(path.join(dir,'harness'),{force:true});}finally{await rmdir(dir);}}
 }
}
function origin(value){let url;try{url=new URL(value);}catch{fail();}if(url.protocol!=='https:'||url.origin!==value||url.username||url.password||url.port||isIP(url.hostname)||url.hostname==='localhost'||!url.hostname.includes('.')||!/^([a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z][a-z0-9-]*$/.test(url.hostname))fail();return url;}
export const supportsProcessGroups=platform=>platform==='darwin'||platform==='linux';
export function runBounded(executable,args,{input,timeout=30000,limit=65536,env=process.env}={}) {
 if(!supportsProcessGroups(process.platform))return Promise.reject(new Error('remote_acceptance_harness_failed'));
 return new Promise((resolve,reject)=>{
  // POSIX process groups include ordinary client children that inherit pipes.
  // The deadline also settles independently of close, even if a reviewed client
  // deliberately detaches a descendant. No raw child diagnostics are emitted.
  const grouped=process.platform!=='win32';
  const child=spawn(executable,args,{stdio:['pipe','pipe','pipe'],env,detached:grouped});
  let output='',size=0,settled=false;
  const terminate=()=>{
   if(settled)return;settled=true;clearTimeout(timer);
   try{if(grouped&&child.pid)process.kill(-child.pid,'SIGKILL');else child.kill('SIGKILL');}catch{}
   child.stdin.destroy();child.stdout.destroy();child.stderr.destroy();child.unref();
   reject(new Error('remote_acceptance_harness_failed'));
  };
  const timer=setTimeout(terminate,timeout);
  child.stdin.on('error',()=>{});
  child.stdout.on('data',b=>{size+=b.length;if(size>limit)terminate();else output+=b;});
  child.stderr.on('data',b=>{size+=b.length;if(size>limit)terminate();});
  child.on('error',terminate);
  child.on('close',code=>{if(settled)return;if(code!==0){terminate();return;}settled=true;clearTimeout(timer);resolve(output);});
  child.stdin.end(input);
 });
}
const command=runBounded;
// No secrets or credential values are accepted in this configuration. Paths go
// to the reviewed executable on stdin. Its stdout/stderr are never forwarded.
export async function preflight(config) {
 exact(config,['provider','browser','origin','accountReady','permissionReady','build','harness','client','devices','runner','providerCredentialFile','mcpCredentialFile','ownerCredentialFile']);
 if(!providers.includes(config.provider)||config.browser!==undefined&&typeof config.browser!=='boolean')fail();
 if(config.origin)origin(config.origin);
 const report=baseReport(config.provider,config.browser||false);report.missing=[];
 const need=(ok,code)=>{if(!ok)report.missing.push(code);};
 need(config.build,'missing_build');need(config.accountReady===true,'missing_account');need(config.origin,'missing_domain');need(config.permissionReady===true,'missing_permission');need(config.harness,'missing_harness');need(config.client,'missing_client');need(config.runner&&supportsProcessGroups(process.platform),'missing_runner');
 exact(config.devices||{},['enrolled','unenrolled','external','phone']);
 for(const device of Object.values(config.devices||{}))validate(device,schema.properties.identity.anyOf[1].properties.clientVersion);
 if(config.provider==='tailscale_serve'){need(config.devices?.enrolled,'missing_enrolled_device');need(config.devices?.unenrolled,'missing_unenrolled_device');}else need(config.devices?.external,'missing_external_device');
 if(config.browser)need(config.devices?.phone,'missing_phone');
 if(!config.provider.startsWith('tailscale'))need(config.providerCredentialFile,'missing_provider_credential');need(config.mcpCredentialFile,'missing_mcp_credential');if(config.browser)need(config.ownerCredentialFile,'missing_owner_credential');
 for(const key of ['providerCredentialFile','mcpCredentialFile','ownerCredentialFile'])if(config[key]){await privatePath(path.dirname(config[key]),true);const stat=await privatePath(config[key]);if(stat.size<1||stat.size>16384)fail();}
 if(config.build){exact(config.build,['buildSha','imageID','platform','architecture','tailscale','ngrok','cloudflared','helperDigest']);validate(config.build.buildSha,schema.properties.identity.anyOf[1].properties.buildSha);if(!/^sha256:[a-f0-9]{64}$/.test(config.build.imageID))fail();for(const k of ['platform','architecture','tailscale','ngrok','cloudflared','helperDigest'])validate(config.build[k],schema.properties.identity.anyOf[1].properties[k]);}
 if(config.harness){exact(config.harness,['file','digest','version','reviewFile']);if(!config.harness.reviewFile){need(false,'missing_harness');}else{const review=await readPrivateJSON(config.harness.reviewFile);exact(review,['digest','version','reviewedAt','client','clientVersion']);if(review.digest!==config.harness.digest||review.version!==config.harness.version||review.client!==config.client?.name||review.clientVersion!==config.client?.version||!/^\d{4}-\d{2}-\d{2}$/.test(review.reviewedAt))fail();(await reviewedHarnessBytes(config.harness)).fill(0);}}
 if(config.harness){validate(config.harness.digest,schema.properties.identity.anyOf[1].properties.harnessDigest);validate(config.harness.version,schema.properties.identity.anyOf[1].properties.harnessVersion);}
 if(config.client){exact(config.client,['name','version']);validate(config.client.name,schema.properties.identity.anyOf[1].properties.client);validate(config.client.version,schema.properties.identity.anyOf[1].properties.clientVersion);}
 if(config.runner){exact(config.runner,['platform','architecture','native']);if(config.runner.native!==true||!config.build||config.runner.platform!==config.build.platform||config.runner.architecture!==config.build.architecture)fail();}
 if(report.missing.length){report.missing=[...new Set(report.missing)];return validateReport(report);}
 const inspected=JSON.parse(await command('docker',['image','inspect',config.build.imageID]))[0];
 if(inspected.Id!==config.build.imageID||inspected.Os!==config.build.platform||inspected.Architecture!==config.build.architecture||inspected.Config.Labels?.['org.opencontainers.image.revision']!==config.build.buildSha)fail();
 const engine=JSON.parse(await command('docker',['info','--format','{{json .}}']));const arch=engine.Architecture==='aarch64'?'arm64':engine.Architecture==='x86_64'?'amd64':engine.Architecture;if(engine.OSType!==config.runner.platform||arch!==config.runner.architecture||(process.arch==='x64'?'amd64':process.arch)!==arch)fail();
 // Read only immutable image files from a never-started, never-mounted container.
 // Verify both the retained module inventory and the actual bundled helper.
 const helperManifest=JSON.parse(await readFile(new URL('../../../packaging/cloudflared-manifest.json',import.meta.url)));
 if('sha256:'+helperManifest.assets.find(a=>a.platform===config.build.platform+'/'+config.build.architecture)?.executable_sha256!==config.build.helperDigest)fail();
 const dir=await realpath(await mkdtemp(path.join(tmpdir(),'wa-remote-image-')));await chmod(dir,0o700);const name='wa-remote-image-'+randomUUID();
 try{await command('docker',['create','--name',name,'--network=none',config.build.imageID,'--help']);
 await command('docker',['cp',name+':/usr/share/doc/wa-indexer/go-modules.txt',path.join(dir,'modules')]);
 await command('docker',['cp',name+':/usr/local/bin/cloudflared',path.join(dir,'helper')]);
 const modules=await readFile(path.join(dir,'modules'),'utf8');if(!modules.split('\n').includes('tailscale.com v1.104.1')||!modules.split('\n').includes('golang.ngrok.com/ngrok/v2 v2.2.0')||hash(await readFile(path.join(dir,'helper')))!==config.build.helperDigest)fail();
 }finally{try{await command('docker',['rm','-f',name]);}finally{await rm(dir,{recursive:true,force:true});}}
 report.identity={buildSha:config.build.buildSha,artifactDigest:config.build.imageID,platform:config.build.platform,architecture:config.build.architecture,tailscale:config.build.tailscale,ngrok:config.build.ngrok,cloudflared:config.build.cloudflared,helperDigest:config.build.helperDigest,harnessVersion:config.harness.version,harnessDigest:config.harness.digest,client:config.client.name,clientVersion:config.client.version,endpointDigest:hash(config.origin+'/mcp')};
 report.outcome='FAIL';return validateReport(report);
}
async function main(){
 const args=process.argv.slice(2);if(args.length!==4||args[0]!=='--config'||args[2]!=='--output')fail();
 const config=await readPrivateJSON(args[1]);await privatePath(args[3],true);let report=await preflight(config);
 if(!report.missing.length){const nonce=randomUUID();const output=JSON.parse(await withHarnessSnapshot(config.harness,args[3],executable=>command(executable,[],{input:JSON.stringify({protocol:1,nonce,config,fixture,expectedIdentity:report.identity,requiredChecks:requiredChecks(config.provider,config.browser)})+'\n',timeout:600000,env:{PATH:process.env.PATH,HOME:process.env.HOME,TMPDIR:process.env.TMPDIR}})));exact(output,['nonce','report']);if(output.nonce!==nonce)fail();validateReport(output.report);if(!isDeepStrictEqual(output.report.identity,report.identity)||output.report.provider!==report.provider||output.report.browser!==report.browser||output.report.testedAt!==report.testedAt)fail();report=output.report;}
 await writeReport(args[3],report);console.log(JSON.stringify({outcome:report.outcome,missing:report.missing}));process.exitCode=report.outcome==='PASS'?0:report.outcome==='NOT_RUN'?2:1;
}
if(process.argv[1]&&import.meta.url===pathToFileURL(path.resolve(process.argv[1])).href)main().catch(()=>{console.error('remote_acceptance_failed');process.exitCode=1;});
