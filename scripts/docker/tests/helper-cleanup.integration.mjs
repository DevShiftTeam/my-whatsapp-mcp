import {randomUUID} from 'node:crypto';
import {docker,helperWithOptions,check} from '../lib.mjs';
import {pins} from '../oci-tools.mjs';
// An actual container stays asleep after the Docker CLI is killed. finally must remove it.
const id=randomUUID(), volume='wa-task4-hung-'+id, image='wa-task5-hung:'+id;
process.env.WA_DOCKER_RUN_ID=id;
try {
  await docker(['build','--tag',image,'-'],{input:'FROM '+pins.bases.slim.reference+'\nENTRYPOINT ["sh", "-c", "sleep 300", "--"]\n',timeout:180000});
  await docker(['volume','create','--label','wa.task4.run='+id,volume]);
  process.env.WA_DOCKER_HELPER_IMAGE=image;
  let failed=false;
  try{await helperWithOptions({volume,name:volume},'inspect',[],{timeout:1500});}catch(e){failed=e.message==='subprocess_failed_docker_run';}
  check(failed,'hung_helper_did_not_timeout');
  const remaining=(await docker(['ps','-aq','--filter','label=wa.task4.run='+id])).out.trim();
  check(!remaining,'hung_helper_container_leaked');console.log('hung_helper_cleanup_PASS');
}finally {
  await docker(['volume','rm',volume],{allowFailure:true});
  await docker(['image','rm',image],{allowFailure:true});
}
