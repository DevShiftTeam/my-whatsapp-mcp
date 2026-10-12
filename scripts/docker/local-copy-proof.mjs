// Explicit developer integration test: no external writes, credentials or user resources.
import {randomUUID} from 'node:crypto';
import {mkdirSync,mkdtempSync,copyFileSync,readFileSync,writeFileSync} from 'node:fs';
import {resolve,join} from 'node:path';
import {docker,check,imageIdentity} from './lib.mjs';
import {inspectArchive,inspectDirectory} from './oci.mjs';
import {skopeo,verifyPublisher,pins} from './oci-tools.mjs';

const archive=resolve(process.argv[2]);
const name='wa-task5-reg-'+randomUUID(), network=name+'-net', volume=name+'-data', carrier=name+'-carrier';
mkdirSync('.artifacts',{recursive:true,mode:0o700});
const dir=mkdtempSync(join(process.env.TMPDIR||resolve('.artifacts'),'task5-copy-'));
copyFileSync(archive,join(dir,'candidate.tar'));
const before=inspectArchive(join(dir,'candidate.tar'));
check(before.platforms.length===1,'proof_single_platform');
const arch=before.platforms[0].platform.split('/')[1];
// Docker Desktop host-share writes can stall; this proof uses a daemon-native,
// task-owned staging volume. The workflow on native Linux uses private bind dirs.
const mounts=['type=volume,src='+volume+',dst=/proof'];
const copyOptions={mounts,user:'0:0'};
try {
  await verifyPublisher();
  console.log('publisher_version_PASS');
  await docker(['volume','create','--label','wa.task5.run='+name,volume]);
  await docker(['create','--name',carrier,'--label','wa.task5.run='+name,'--mount',mounts[0],'--entrypoint','/usr/bin/true',pins.ci.publisher]);
  await docker(['cp',archive,carrier+':/proof/candidate.tar']);
  await skopeo(['copy','--override-os','linux','--override-arch',arch,'oci-archive:/proof/candidate.tar','docker-archive:/proof/runtime.tar:wa-task5-runtime:proof'],copyOptions);
  await docker(['cp',carrier+':/proof/runtime.tar',join(dir,'runtime.tar')]);
  await docker(['load','--input',join(dir,'runtime.tar')],{timeout:180000});
  console.log('runnable_import_PASS');
  const identity=await imageIdentity('wa-task5-runtime:proof');
  inspectArchive(archive,{platform:identity.platform,sourceRevision:identity.sourceRevision,imageID:identity.imageID});
  await docker(['pull',pins.ci.localProofRegistry],{timeout:180000});
  await docker(['network','create','--internal','--label','wa.task5.run='+name,network]);
  await docker(['run','--detach','--name',name,'--label','wa.task5.run='+name,'--network',network,'--read-only','--cap-drop=ALL','--security-opt=no-new-privileges:true','--tmpfs','/var/lib/registry:rw,nosuid,nodev,size=256m',pins.ci.localProofRegistry]);
  const dest=name+':5000/proof:owned';
  await skopeo(['copy','--all','--preserve-digests','--dest-tls-verify=false','--retry-times','3','--digestfile','/proof/pushed.digest','oci-archive:/proof/candidate.tar','docker://'+dest],{...copyOptions,network});
  await docker(['cp',carrier+':/proof/pushed.digest',join(dir,'pushed.digest')]);
  check(readFileSync(join(dir,'pushed.digest'),'utf8').trim()===before.rootDigest,'proof_push_digest_mismatch');
  await skopeo(['copy','--all','--preserve-digests','--src-tls-verify=false','docker://'+name+':5000/proof@'+before.rootDigest,'oci:/proof/roundtrip'],{...copyOptions,network});
  await docker(['cp',carrier+':/proof/roundtrip',join(dir,'roundtrip')]);
  const after=inspectDirectory(join(dir,'roundtrip'));
  check(after.rootDigest===before.rootDigest&&JSON.stringify(after.platforms)===JSON.stringify(before.platforms)&&JSON.stringify(after.graph)===JSON.stringify(before.graph),'proof_graph_changed');
  const result={status:'PASS',scope:'native local OCI copy, not hosted/GHCR/release proof',identity,before,after,archive};
  writeFileSync(join(dir,'proof.json'),JSON.stringify(result,null,2)+'\n');
  console.log(JSON.stringify({status:'PASS',imageID:identity.imageID,rootDigest:before.rootDigest,archiveSHA256:before.archiveSHA256,evidence:join(dir,'proof.json')}));
}finally {
  await docker(['rm','-f',name],{allowFailure:true,timeout:10000});
  await docker(['network','rm',network],{allowFailure:true,timeout:10000});
  await docker(['rm','-f',carrier],{allowFailure:true,timeout:10000});
  await docker(['volume','rm',volume],{allowFailure:true,timeout:10000});
}
