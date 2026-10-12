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
export async function verifyPublisher() {
  // Pull is outside the network-isolated copy container; always by the committed digest.
  await docker(['pull',pins.ci.publisher],{timeout:300000});
  check((await skopeo(['--version'])).out.trim()===pins.ci.publisherVersion,'publisher_version_mismatch');
}
