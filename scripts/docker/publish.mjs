// Invoked only by the environment-bound tag job. Never builds or edits source.
import {readFileSync,writeFileSync,mkdirSync,appendFileSync} from 'node:fs';
import {resolve,join} from 'node:path';
import {check,docker} from './lib.mjs';
import {verifyBundle} from './ci.mjs';
import {releaseVersion} from './release-policy.mjs';
import {inspectDirectory,verifyMergedPlatforms} from './oci.mjs';
import {pins,skopeo,verifyPublisher} from './oci-tools.mjs';

const incoming=resolve('.artifacts/incoming'), published=resolve('.artifacts/published');
const arches=['amd64','arm64'];
function precheck() {
  check(process.env.GITHUB_EVENT_NAME==='push'&&process.env.GITHUB_REPOSITORY==='DevShiftTeam/my-whatsapp-mcp','publisher_event_repository');
  const version=releaseVersion(process.env.GITHUB_REF);
  check(version===process.env.RELEASE_VERSION,'publisher_version_mismatch');
  for(const arch of arches){check(/^[0-9]+$/.test(process.env['ARTIFACT_'+arch.toUpperCase()+'_ID']||''),'artifact_id_missing');check(/^[a-f0-9]{64}$/.test(process.env['ARTIFACT_'+arch.toUpperCase()+'_DIGEST']||''),'artifact_digest_missing');}
  const evidence=arches.map(arch=>verifyBundle(join(incoming,arch),arch));
  return {version,evidence};
}
async function main() {
  const {version,evidence}=precheck();
  if(process.argv[2]==='precheck'){console.log('qualified_archives_verified');return;}
  check(process.argv[2]==='publish','publisher_command');
  const auth=resolve(process.env.DOCKER_CONFIG||'');check(auth&&auth!==resolve('.'),'isolated_registry_auth_required');
  mkdirSync(published,{recursive:true,mode:0o700});await verifyPublisher();
  const mounts=['type=bind,src='+incoming+',dst=/input,readonly','type=bind,src='+published+',dst=/output','type=bind,src='+auth+',dst=/auth,readonly'];
  const refs=[];
  for(const [i,arch] of arches.entries()) {
    const tag=version+'-run-'+process.env.GITHUB_RUN_ID+'-'+process.env.GITHUB_RUN_ATTEMPT+'-'+arch;
    await skopeo(['copy','--all','--preserve-digests','--dest-authfile','/auth/config.json','--digestfile','/output/'+arch+'.digest','oci-archive:/input/'+arch+'/candidate.tar','docker://'+pins.ci.image+':'+tag],{mounts,network:'bridge'});
    const d=readFileSync(join(published,arch+'.digest'),'utf8').trim();check(d===evidence[i].graph.rootDigest,'published_arch_digest_mismatch');refs.push(pins.ci.image+'@'+d);
    await skopeo(['copy','--all','--preserve-digests','--src-authfile','/auth/config.json','docker://'+refs[i],'oci:/output/'+arch],{mounts,network:'bridge'});
    const graph=inspectDirectory(join(published,arch),{platform:'linux/'+arch,sourceRevision:process.env.GITHUB_SHA});
    check(graph.rootDigest===d&&JSON.stringify(graph.platforms)===JSON.stringify(evidence[i].graph.platforms)&&JSON.stringify(graph.graph)===JSON.stringify(evidence[i].graph.graph),'registry_graph_roundtrip_mismatch');
  }
  // Only immutable digests enter the merged index. No moving intermediate lookup.
  await docker(['buildx','imagetools','create','--tag',pins.ci.image+':'+version,'--metadata-file',join(published,'manifest.json'),...refs],{timeout:180000});
  const metadata=JSON.parse(readFileSync(join(published,'manifest.json'),'utf8'));
  const d=metadata['containerimage.descriptor']?.digest;check(/^sha256:[a-f0-9]{64}$/.test(d),'published_index_digest_missing');
  const reference=pins.ci.image+'@'+d;
  await skopeo(['copy','--all','--preserve-digests','--src-authfile','/auth/config.json','docker://'+reference,'oci:/output/release'],{mounts,network:'bridge'});
  const graph=inspectDirectory(join(published,'release'),{sourceRevision:process.env.GITHUB_SHA});
  const platforms=evidence.flatMap(e=>e.graph.platforms).sort((a,b)=>a.platform.localeCompare(b.platform));
  check(graph.rootDigest===d,'merged_root_identity_mismatch');
  verifyMergedPlatforms(graph,evidence.map(e=>e.graph));
  const record={schema:1,version,reference,sourceRevision:process.env.GITHUB_SHA,runID:process.env.GITHUB_RUN_ID,runAttempt:process.env.GITHUB_RUN_ATTEMPT,graph,
    artifacts:arches.map(arch=>({platform:'linux/'+arch,id:process.env['ARTIFACT_'+arch.toUpperCase()+'_ID'],digest:process.env['ARTIFACT_'+arch.toUpperCase()+'_DIGEST']})),
    publication:'PASS',anonymousPull:'REQUIRES_FRESH_NATIVE_VERIFICATION',freshVolumeAcceptance:'NOT_RUN',quickstart:'SOURCE_PREVIEW_UNTIL_VERIFIED'};
  writeFileSync(join(published,'release.json'),JSON.stringify(record,null,2)+'\n',{mode:0o600});
  for(const [key,value] of [['reference',reference],...platforms.map(p=>[p.platform.split('/')[1]+'_config',p.configDigest])])appendFileSync(process.env.GITHUB_OUTPUT,key+'='+value+'\n');
}
main().catch(e=>{console.error(/^[a-zA-Z0-9_]+$/.test(e.message)?e.message:'docker_publication_failed');process.exitCode=1;});
