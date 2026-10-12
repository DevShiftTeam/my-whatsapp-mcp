import {test} from 'node:test';
import assert from 'node:assert/strict';
import {gzipSync} from 'node:zlib';
import {mkdtempSync,writeFileSync,readFileSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {digest, inspectLayout,verifyMergedPlatforms,readArchive} from '../oci.mjs';
import {releaseVersion, validateEnvironment, validateEvidence,checkReleaseEnvironment} from '../release-policy.mjs';
import {helperWithOptions} from '../lib.mjs';
import {releaseCompose} from '../release-compose.mjs';
import {record,verifyBundle} from '../ci.mjs';

const revision='a'.repeat(40), imageSource='https://github.com/DevShiftTeam/my-whatsapp-mcp';
function fixture({provenanceRevision=revision,subjectDigest,arch='arm64'}={}) {
  const files=new Map();
  const put=(value,mediaType,extra={})=>{const b=Buffer.isBuffer(value)?value:Buffer.from(JSON.stringify(value));const d=digest(b);files.set('blobs/sha256/'+d.slice(7),b);return {mediaType,digest:d,size:b.length,...extra};};
  const raw=Buffer.from('a synthetic layer');
  const layer=put(gzipSync(raw),'application/vnd.oci.image.layer.v1.tar+gzip');
  const config=put({os:'linux',architecture:arch,rootfs:{type:'layers',diff_ids:[digest(raw)]},config:{Labels:{'org.opencontainers.image.revision':revision,'org.opencontainers.image.source':imageSource,'org.opencontainers.image.licenses':'GPL-3.0-only'}}},'application/vnd.oci.image.config.v1+json');
  const runnable=put({schemaVersion:2,config,layers:[layer]},'application/vnd.oci.image.manifest.v1+json',{platform:{os:'linux',architecture:arch}});
  const predicates=['https://slsa.dev/provenance/v1','https://spdx.dev/Document'];
  const attest=put({schemaVersion:2,config:put({},'application/vnd.oci.image.config.v1+json'),layers:predicates.map(predicateType=>put({_type:'https://in-toto.io/Statement/v1',subject:[{name:'image',digest:{sha256:subjectDigest||runnable.digest.slice(7)}}],predicateType,predicate:predicateType.includes('slsa')?{buildDefinition:{externalParameters:{request:{args:{'build-arg:SOURCE_REVISION':provenanceRevision,target:'runtime'}}}}}:{spdxVersion:'SPDX-2.3',documentNamespace:'https://example.com/fixture',packages:[{name:'fixture'}]}},'application/vnd.in-toto+json'))},'application/vnd.oci.image.manifest.v1+json',{platform:{os:'unknown',architecture:'unknown'},annotations:{'vnd.docker.reference.type':'attestation-manifest','vnd.docker.reference.digest':runnable.digest}});
  const root=put({schemaVersion:2,mediaType:'application/vnd.oci.image.index.v1+json',manifests:[runnable,attest]},'application/vnd.oci.image.index.v1+json');
  files.set('oci-layout',Buffer.from('{"imageLayoutVersion":"1.0.0"}'));
  files.set('index.json',Buffer.from(JSON.stringify({schemaVersion:2,manifests:[root]})));
  return {files,config,runnable,attest,root,layer};
}
const expected={platform:'linux/arm64',sourceRevision:revision};
function tar(files,{type='0'}={}) {
  const chunks=[];
  for(const [name,value] of files){const bytes=Buffer.from(value),h=Buffer.alloc(512);h.write(name);h.write('0000600\0',100);h.write('0000000\0',108);h.write('0000000\0',116);h.write(bytes.length.toString(8).padStart(11,'0')+'\0',124);h.write('00000000000\0',136);h.fill(32,148,156);h.write(type,156);h.write('ustar\0',257);const sum=[...h].reduce((a,b)=>a+b,0);h.write(sum.toString(8).padStart(6,'0')+'\0 ',148);chunks.push(h,bytes,Buffer.alloc((512-bytes.length%512)%512));}
  return Buffer.concat([...chunks,Buffer.alloc(1024)]);
}
test('OCI identity binds tested config to runnable descriptor and both attestations',()=>{
  const f=fixture();const result=inspectLayout(f.files,{...expected,imageID:f.config.digest});
  assert.equal(result.rootDigest,f.root.digest);assert.equal(result.platforms[0].manifestDigest,f.runnable.digest);
  assert.equal(result.platforms[0].configDigest,f.config.digest);assert.equal(result.platforms[0].attestations.length,1);
  assert.ok(result.graph.length>=8);
});
test('rejects wrong tested config, source, platform and tampered layer',()=>{
  const f=fixture();
  for(const patch of [{imageID:'sha256:'+'b'.repeat(64)},{sourceRevision:'b'.repeat(40)},{platform:'linux/amd64'}]) assert.throws(()=>inspectLayout(f.files,{...expected,...patch}));
  f.files.set('blobs/sha256/'+f.layer.digest.slice(7),Buffer.from('tampered'));
  assert.throws(()=>inspectLayout(f.files,expected));
});
test('rejects dropped attestation and unreferenced archive payload',()=>{
  const f=fixture();f.files.delete('blobs/sha256/'+f.attest.digest.slice(7));assert.throws(()=>inspectLayout(f.files,expected));
  const g=fixture();g.files.set('secrets.txt',Buffer.from('unexpected'));assert.throws(()=>inspectLayout(g.files,expected));
});
test('attestation predicates must bind the actual build revision and runnable subject',()=>{
  assert.throws(()=>inspectLayout(fixture({provenanceRevision:'b'.repeat(40)}).files,expected));
  assert.throws(()=>inspectLayout(fixture({subjectDigest:'c'.repeat(64)}).files,expected));
});
test('merged index must retain both exact platform manifests and attestation sets',()=>{
  const arm=fixture(),amd=fixture({arch:'amd64'});
  const qualified=[inspectLayout(amd.files,{sourceRevision:revision}),inspectLayout(arm.files,expected)];
  const files=new Map([...arm.files,...amd.files]);
  files.delete('blobs/sha256/'+arm.root.digest.slice(7));files.delete('blobs/sha256/'+amd.root.digest.slice(7));
  files.set('index.json',Buffer.from(JSON.stringify({schemaVersion:2,mediaType:'application/vnd.oci.image.index.v1+json',manifests:[amd.runnable,amd.attest,arm.runnable,arm.attest]})));
  const graph=inspectLayout(files,{sourceRevision:revision});verifyMergedPlatforms(graph,qualified);
  assert.throws(()=>verifyMergedPlatforms({...graph,platforms:graph.platforms.slice(0,1)},qualified));
  const tampered=structuredClone(graph);tampered.platforms[0].attestations=[];assert.throws(()=>verifyMergedPlatforms(tampered,qualified));
  assert.throws(()=>verifyMergedPlatforms(graph,[qualified[0],qualified[0]]));
});
test('strict release tags and protected environment policy fail closed',()=>{
  assert.equal(releaseVersion('refs/tags/v1.2.3'),'v1.2.3');
  for(const ref of ['refs/heads/main','refs/tags/v01.2.3','refs/tags/v1.2.3;echo hi','refs/tags/latest','refs/tags/v1.2.3-rc.1'])assert.throws(()=>releaseVersion(ref));
  const env={id:42,name:'docker-release',protection_rules:[{type:'required_reviewers',prevent_self_review:true,reviewers:[{type:'Team',reviewer:{id:7}}]}],deployment_branch_policy:{protected_branches:false,custom_branch_policies:true}};
  const policies={total_count:1,branch_policies:[{name:'v*',type:'tag'}]};
  assert.equal(validateEnvironment(env,policies,'v1.2.3').environmentID,42);
  for(const invalid of [{}, {...env,protection_rules:[]},{...env,deployment_branch_policy:null},{...env,protection_rules:[{type:'required_reviewers',reviewers:[]}]}])assert.throws(()=>validateEnvironment(invalid,policies,'v1.2.3'));
  assert.throws(()=>validateEnvironment(env,{total_count:1,branch_policies:[{type:'branch',name:'*'}]},'v1.2.3'));
  assert.throws(()=>validateEnvironment(env,{total_count:2,branch_policies:policies.branch_policies},'v1.2.3'));
});
test('release evidence cannot be reused from another run/platform/source or missing benchmark',()=>{
  const e={schema:1,sourceRevision:revision,harnessRevision:revision,runID:'10',runAttempt:'2',platform:'linux/arm64',acceptance:'PASS',benchmark:'PASS',native:true};
  const ctx={sourceRevision:revision,runID:'10',runAttempt:'2',platform:'linux/arm64'};
  validateEvidence(e,ctx);
  for(const patch of [{runID:'9'},{runAttempt:'1'},{platform:'linux/amd64'},{sourceRevision:'b'.repeat(40)},{acceptance:'FAIL'},{benchmark:'NOT_RUN'},{native:false}])assert.throws(()=>validateEvidence({...e,...patch},ctx));
});
test('environment preflight rejects unavailable APIs before scheduling publisher',async()=>{
  for(const status of [403,404,500])await assert.rejects(checkReleaseEnvironment({token:'fixture',repository:'DevShiftTeam/my-whatsapp-mcp',ref:'refs/tags/v1.2.3',request:async()=>({status})}),new RegExp('release_policy_http_'+status));
  await assert.rejects(checkReleaseEnvironment({token:'fixture',repository:'other/repo',ref:'refs/tags/v1.2.3',request:async()=>{throw new Error('must not request')}}),/release_repository/);
});
test('a timed-out helper is named, labeled and explicitly removed in finally',async()=>{
  const calls=[];const execute=async(args,options)=>{calls.push({args,options});if(args[0]==='run')throw new Error('timeout');return {code:0};};
  await assert.rejects(helperWithOptions({volume:'wa-task4-fixture',name:'wa-task4-fixture'},'inspect',[],{execute,timeout:10}),/timeout/);
  const a=calls[0].args;const name=a[a.indexOf('--name')+1];assert.match(name,/^wa-task4-helper-[a-f0-9-]+$/);
  assert.ok(a.includes('--label'));assert.deepEqual(calls[1].args,['rm','-f',name]);assert.equal(calls[1].options.timeout,10000);
  assert.equal(calls[0].options.timeout,10);
});
test('downloadable Compose requires an actual stable version and immutable published digest',()=>{
  const source='# preview\nservices:\n  wa-indexer:\n    image: wa-indexer:development\n    ports: ["127.0.0.1:8123:8123"]\n';
  const result=releaseCompose(source,'v1.2.3','ghcr.io/devshiftteam/my-whatsapp-mcp@sha256:'+'a'.repeat(64));
  assert.match(result,/v1\.2\.3@sha256:/);assert.match(result,/127\.0\.0\.1:8123:8123/);
  for(const ref of ['ghcr.io/devshiftteam/my-whatsapp-mcp:latest','docker.io/other@sha256:'+'a'.repeat(64)])assert.throws(()=>releaseCompose(source,'v1.2.3',ref));
  assert.throws(()=>releaseCompose(source.replace('development','changed'),'v1.2.3','ghcr.io/devshiftteam/my-whatsapp-mcp@sha256:'+'a'.repeat(64)));
});
test('archive reader rejects traversal, links, duplicates, truncation and altered headers',()=>{
  const dir=mkdtempSync(join(tmpdir(),'wa-oci-tar-')),path=join(dir,'candidate.tar');
  try {
    const f=fixture();writeFileSync(path,tar(f.files));assert.equal(readArchive(path).files.size,f.files.size);
    const bad=[tar([['../outside','x']]),tar([['index.json','x']],{type:'2'}),tar([['index.json','x'],['index.json','y']]),tar(f.files).subarray(0,1000)];
    const header=tar(f.files);header[0]^=1;bad.push(header);
    for(const b of bad){writeFileSync(path,b);assert.throws(()=>readArchive(path));}
  }finally{rmSync(dir,{recursive:true,force:true});}
});
test('record and publisher verification bind exact archive, PASS summaries and current run',()=>{
  const dir=mkdtempSync(join(tmpdir(),'wa-oci-record-'));const f=fixture();
  const keys=['GITHUB_SHA','GITHUB_RUN_ID','GITHUB_RUN_ATTEMPT'];const old=keys.map(k=>process.env[k]);
  const write=(name,value)=>writeFileSync(join(dir,name),JSON.stringify(value)+'\n');
  try {
    const ctx={sourceRevision:revision,harnessRevision:revision,runID:'10',runAttempt:'2',platform:'linux/arm64',native:true};
    process.env.GITHUB_SHA=revision;process.env.GITHUB_RUN_ID='10';process.env.GITHUB_RUN_ATTEMPT='2';
    writeFileSync(join(dir,'candidate.tar'),tar(f.files));write('context.json',ctx);
    const identity={imageID:f.config.digest,sourceRevision:revision,harnessRevision:revision,platform:'linux/arm64'};
    write('acceptance.jsonl',{identity,acceptance:'PASS'});
    const measurement={identity,fresh:{httpMs:{samples:[1,2,3,4,5]}},reusedSynthetic:{httpMs:{samples:[1,2,3,4,5]}}};
    writeFileSync(join(dir,'benchmark.jsonl'),[measurement,measurement,{benchmark:'PASS'}].map(x=>JSON.stringify(x)).join('\n')+'\n');
    // Attested BuildKit OCI output reports the root digest, not a config digest.
    write('build.json',{'containerimage.digest':f.root.digest});
    writeFileSync(join(dir,'buildx.txt'),'github.com/docker/buildx v0.38.0 fixture');writeFileSync(join(dir,'builder.txt'),'fixture');
    record(dir);const e=verifyBundle(dir,'arm64');assert.equal(e.graph.platforms[0].configDigest,f.config.digest);
    process.env.GITHUB_RUN_ATTEMPT='3';assert.throws(()=>verifyBundle(dir,'arm64'),/runAttempt/);process.env.GITHUB_RUN_ATTEMPT='2';
    write('acceptance.jsonl',{identity,acceptance:'FAIL'});assert.throws(()=>record(dir),/acceptance_summary/);assert.throws(()=>verifyBundle(dir,'arm64'),/checksum/);
    write('acceptance.jsonl',{identity,acceptance:'PASS'});
    const original=readFileSync(join(dir,'candidate.tar'));original[600]^=1;writeFileSync(join(dir,'candidate.tar'),original);assert.throws(()=>verifyBundle(dir,'arm64'));
  }finally{keys.forEach((k,i)=>{if(old[i]===undefined)delete process.env[k];else process.env[k]=old[i];});rmSync(dir,{recursive:true,force:true});}
});
