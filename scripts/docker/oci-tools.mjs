import {randomUUID} from 'node:crypto';
import {readFileSync} from 'node:fs';
import {docker,check} from './lib.mjs';
export const pins=JSON.parse(readFileSync(new URL('../../packaging/docker-images.json',import.meta.url)));

// No Docker socket mount or host credentials by default. Each invocation owns one container.
export async function skopeo(args,{mounts=[],network='none',timeout=300000,env=[],user=String(process.getuid?.()??1001)+':'+String(process.getgid?.()??1001)}={}) {
  const name='wa-task5-skopeo-'+randomUUID();
  try{return await docker(['run','--rm','--name',name,'--label','wa.task5.run='+name,
    '--read-only','--cap-drop=ALL','--security-opt=no-new-privileges:true','--network',network,
    '--user',user,
    '--tmpfs','/tmp:rw,nosuid,nodev,mode=1777,size=128m',
    '--tmpfs','/var/tmp:rw,nosuid,nodev,mode=1777,size=512m','--env','TMPDIR=/tmp',
    ...env.flatMap(e=>['--env',e]),...mounts.flatMap(m=>['--mount',m]),pins.ci.publisher,...args],{timeout});
  }finally{await docker(['rm','-f',name],{allowFailure:true,timeout:10000});}
}
const pullFailures=[
  ['publisher_access_denied',/unauthorized|access denied|requested access to the resource is denied|forbidden|\b40[13]\b/i],
  ['publisher_rate_limited',/toomanyrequests|too many requests|\b429\b/i],
  ['publisher_manifest_unavailable',/manifest unknown|manifest_unknown|not found|\b404\b/i],
  ['publisher_network_failed',/\btls\b|x509|certificate|handshake|i\/o timeout|connection refused|connection reset|no such host|temporary failure in name resolution|network is unreachable|dial tcp|context deadline exceeded|\beof\b/i],
];
// Only the stderr tail is matched, and the result is always one of these fixed codes.
export function classifyPullFailure(stderr) {
  const tail=String(stderr??'').slice(-4096);
  return pullFailures.find(([,pattern])=>pattern.test(tail))?.[0]??'publisher_pull_failed';
}
export async function verifyPublisher({execute=docker,runSkopeo=skopeo}={}) {
  // Pull is outside the network-isolated copy container; always by the committed digest.
  const pulled=await execute(['pull',pins.ci.publisher],{timeout:300000,allowFailure:true}).catch(()=>null);
  check(pulled?.code===0,pulled?classifyPullFailure(pulled.err):'publisher_pull_failed');
  check((await runSkopeo(['--version'])).out.trim()===pins.ci.publisherVersion,'publisher_version_mismatch');
}
