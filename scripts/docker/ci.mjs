import {appendFileSync,readFileSync,writeFileSync,mkdirSync,mkdtempSync,realpathSync} from 'node:fs';
import {resolve,join} from 'node:path';
import {cpus,totalmem,release as kernelRelease} from 'node:os';
import {pathToFileURL} from 'node:url';
import {docker,run,check,imageIdentity} from './lib.mjs';
import {inspectArchive,digest} from './oci.mjs';
import {pins,skopeo,verifyPublisher} from './oci-tools.mjs';
import {validateEvidence,validateNormalServeBenchmark,checkReleaseEnvironment,checkCurrentRunApproval} from './release-policy.mjs';

const read=p=>JSON.parse(readFileSync(p,'utf8'));
const save=(p,value)=>writeFileSync(p,JSON.stringify(value,null,2)+'\n',{mode:0o600});
const output=(key,value)=>{check(process.env.GITHUB_OUTPUT,'github_output_missing');appendFileSync(process.env.GITHUB_OUTPUT,key+'='+value+'\n');};
const expected=arch=>({sourceRevision:process.env.GITHUB_SHA,runID:process.env.GITHUB_RUN_ID,runAttempt:process.env.GITHUB_RUN_ATTEMPT,platform:'linux/'+arch});
export async function prepare(arch) {
  check(['amd64','arm64'].includes(arch),'invalid_arch');
  check(process.env.RUNNER_ENVIRONMENT==='github-hosted'&&process.platform==='linux'&&process.arch===(arch==='amd64'?'x64':'arm64'),'native_hosted_runner_required');
  check((await run('uname',['-m'])).out.trim()===(arch==='amd64'?'x86_64':'aarch64'),'native_kernel_mismatch');
  check((await run('git',['status','--porcelain'])).out.trim()==='','dirty_checkout');
  const sha=(await run('git',['rev-parse','HEAD'])).out.trim();check(sha===process.env.GITHUB_SHA&&/^[a-f0-9]{40}$/.test(sha),'checkout_sha_mismatch');
  check(process.versions.node===pins.ci.node,'node_pin_mismatch');
  const go=(await run('go',['env','GOVERSION'])).out.trim();check(go===pins.toolchain,'go_pin_mismatch');
  const info=JSON.parse((await docker(['info','--format','{{json .}}'])).out);
  const machine=info.Architecture==='x86_64'?'amd64':info.Architecture==='aarch64'?'arm64':info.Architecture;
  check(info.OSType==='linux'&&machine===arch,'native_engine_platform_mismatch');
  const engine=(await docker(['version','--format','{{.Server.Version}}'])).out.trim();check(Number(engine.split('.')[0])>=28,'engine28_required');
  const compose=(await docker(['compose','version','--short'])).out.trim();const [major,minor,patch]=compose.replace(/^v/,'').split('.').map(Number);
  check(major>2||major===2&&(minor>20||minor===20&&patch>=2),'compose_2_20_2_required');
  const dir=resolve('.artifacts/docker-'+arch);mkdirSync(dir,{recursive:true,mode:0o700});
  const tmp=realpathSync(mkdtempSync(join(process.env.RUNNER_TEMP,'wa-docker-')));
  const context={...expected(arch),harnessRevision:sha,native:true,runner:{name:process.env.RUNNER_NAME,arch:process.env.RUNNER_ARCH,imageOS:process.env.ImageOS,imageVersion:process.env.ImageVersion,environment:process.env.RUNNER_ENVIRONMENT,cpus:cpus().length,cpuModel:cpus()[0]?.model,totalMemory:totalmem(),kernel:kernelRelease(),engine,compose,node:process.versions.node,go}};
  save(join(dir,'context.json'),context);
  for(const [k,v] of Object.entries({dir,source:sha,platform:'linux/'+arch,buildkit:pins.ci.buildkit,buildx:pins.ci.buildx,sbom:pins.ci.sbomGenerator}))output(k,v);
  appendFileSync(process.env.GITHUB_ENV,'TMPDIR='+tmp+'\nWA_DOCKER_SDK_HELPER='+join(tmp,'sdk-helper')+'\n');
}

export async function loadCandidate(dir,arch) {
  const context=read(join(dir,'context.json'));
  const graph=inspectArchive(join(dir,'candidate.tar'),context);
  check(graph.platforms.length===1,'single_platform_candidate_required');
  await verifyPublisher();
  await skopeo(['copy','--override-os','linux','--override-arch',arch,'oci-archive:/input/candidate.tar','docker-archive:/output/runtime.tar:wa-ci-runtime:qualified'],{mounts:['type=bind,src='+dir+',dst=/input,readonly','type=bind,src='+dir+',dst=/output']});
  await docker(['load','--input',join(dir,'runtime.tar')],{timeout:180000});
  const identity=await imageIdentity('wa-ci-runtime:qualified');
  check(identity.sourceRevision===context.sourceRevision&&identity.imageID===graph.platforms[0].configDigest&&identity.platform===context.platform,'loaded_config_binding_failed');
  save(join(dir,'graph.json'),graph);output('image',identity.imageID);
}

function jsonLines(path){return readFileSync(path,'utf8').trim().split('\n').filter(Boolean).map(s=>JSON.parse(s));}
export function record(dir,{published=false}={}) {
  const ctx=read(join(dir,'context.json')), rows=jsonLines(join(dir,'acceptance.jsonl'));
  const accepted=rows.at(-1);check(accepted?.acceptance==='PASS','acceptance_summary_missing');
  check(accepted.identity.sourceRevision===ctx.sourceRevision&&accepted.identity.harnessRevision===ctx.sourceRevision&&accepted.identity.platform===ctx.platform,'acceptance_identity_mismatch');
  if(published){check(accepted.identity.imageID===process.env.EXPECTED_CONFIG,'published_config_mismatch');save(join(dir,'verification.json'),{...ctx,acceptance:'PASS',imageID:accepted.identity.imageID,releaseReference:process.env.RELEASE_REFERENCE,anonymousPull:'PASS'});return;}
  const graph=inspectArchive(join(dir,'candidate.tar'),{...ctx,imageID:accepted.identity.imageID});
  const benchmark=jsonLines(join(dir,'benchmark.jsonl'));const normalServe=validateNormalServeBenchmark(benchmark);
  const measurements=benchmark.filter(r=>r.identity);
  check(measurements.length===2&&measurements[0].identity.imageID===accepted.identity.imageID&&measurements.every(r=>r.identity.sourceRevision===ctx.sourceRevision&&r.identity.harnessRevision===ctx.sourceRevision&&r.identity.platform===ctx.platform&&r.fresh.httpMs.samples.length>=5&&r.reusedSynthetic.httpMs.samples.length>=5),'benchmark_identity_mismatch');
  const build=read(join(dir,'build.json'));
  check(build['containerimage.digest']===graph.rootDigest,'build_export_identity_mismatch');
  // The attested OCI exporter reports an index digest. The config binding above
  // comes from its verified runnable descriptor and the actually tested image.
  const buildxVersion=readFileSync(join(dir,'buildx.txt'),'utf8').trim();
  check(buildxVersion.includes(' '+pins.ci.buildx+' '),'buildx_pin_mismatch');
  const evidence={schema:1,...ctx,acceptance:'PASS',benchmark:'PASS',...normalServe,performanceBaseline:pins.ci.performanceBaseline,graph,
    acceptanceSHA256:digest(readFileSync(join(dir,'acceptance.jsonl'))),benchmarkSHA256:digest(readFileSync(join(dir,'benchmark.jsonl'))),
    build:{metadata:build,buildxVersion,builder:readFileSync(join(dir,'builder.txt'),'utf8').trim(),helperImage:process.env.WA_DOCKER_HELPER_IMAGE,pins}};
  validateEvidence(evidence,ctx);save(join(dir,'evidence.json'),evidence);
}

export function verifyBundle(dir,arch) {
  const e=read(join(dir,'evidence.json'));validateEvidence(e,expected(arch));
  const graph=inspectArchive(join(dir,'candidate.tar'),{...expected(arch),imageID:e.graph?.platforms?.[0]?.configDigest});
  check(JSON.stringify(graph)===JSON.stringify(e.graph),'transferred_graph_mismatch');
  check(digest(readFileSync(join(dir,'acceptance.jsonl')))===e.acceptanceSHA256&&digest(readFileSync(join(dir,'benchmark.jsonl')))===e.benchmarkSHA256,'transferred_evidence_checksum');
  const acceptance=jsonLines(join(dir,'acceptance.jsonl')).at(-1);
  check(acceptance?.acceptance==='PASS'&&acceptance.identity.imageID===graph.platforms[0].configDigest&&acceptance.identity.harnessRevision===e.harnessRevision,'transferred_acceptance_mismatch');
  validateNormalServeBenchmark(jsonLines(join(dir,'benchmark.jsonl')));
  return e;
}

export async function main(args) {
  const [command,arg,extra]=args;
  if(command==='prepare')return prepare(arg);
  if(command==='load')return loadCandidate(resolve(arg),extra);
  if(command==='record')return record(resolve(arg),{published:extra==='published'});
  if(command==='verify')return verifyBundle(resolve(arg),extra);
  if(command==='approval'){
    const result=await checkCurrentRunApproval({token:process.env.GH_TOKEN,repository:process.env.GITHUB_REPOSITORY,ref:process.env.GITHUB_REF,runID:process.env.GITHUB_RUN_ID,runAttempt:process.env.GITHUB_RUN_ATTEMPT,sourceRevision:process.env.GITHUB_SHA});
    const dir=resolve('.artifacts/published');mkdirSync(dir,{recursive:true,mode:0o700});save(join(dir,'approval.json'),result);console.log(JSON.stringify(result));return;
  }
  if(command==='preflight'){
    const result=await checkReleaseEnvironment({token:process.env.GH_TOKEN,repository:process.env.GITHUB_REPOSITORY,ref:process.env.GITHUB_REF});
    output('version',result.version);console.log(JSON.stringify(result));return;
  }
  throw new Error('unknown_ci_command');
}
if(import.meta.url===pathToFileURL(process.argv[1]||'').href)main(process.argv.slice(2)).catch(e=>{console.error(/^[a-zA-Z0-9_]+$/.test(e.message)?e.message:'docker_ci_failed');process.exitCode=1;});
