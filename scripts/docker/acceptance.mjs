import {randomUUID} from 'node:crypto';
import {run,check,docker,imageIdentity,option,cleanupRun} from './lib.mjs';
const supplied=option('--image',process.env.WA_DOCKER_IMAGE||'wa-task4:development');
const identity=await imageIdentity(supplied);if(process.argv.includes('--image'))check(/^[a-f0-9]{40}$/.test(identity.sourceRevision),'immutable_source_label_required');
process.env.WA_DOCKER_IMAGE=identity.imageID;console.log(JSON.stringify({identity}));
const major=Number(process.versions.node.split('.')[0]);check(major===24,'node24_required');
check(JSON.parse((await run(process.execPath,['-e',"process.stdout.write(JSON.stringify(require('./docs/.vitepress/node_modules/@playwright/test/package.json').version))"])).out)==='1.64.0','locked_playwright_required');
await docker(['compose','-f','compose.yaml','-f','compose.build.yaml','config','--quiet']);
const engine=(await docker(['version','--format','{{.Server.Version}}'])).out.trim();check(Number(engine.split('.')[0])>=28,'engine28_required');
const compose=(await docker(['compose','version','--short'])).out.trim().replace(/^v/,'').split('.').map(Number);check(compose[0]>2||compose[0]===2&&(compose[1]>20||compose[1]===20&&compose[2]>=2),'compose_2_20_2_required');
process.env.WA_DOCKER_RUN_ID=randomUUID();
let result;try{result=await run(process.execPath,['docs/.vitepress/node_modules/@playwright/test/cli.js','test','--config','scripts/docker/playwright.config.ts'],{allowFailure:true,timeout:240000});}finally{await cleanupRun(process.env.WA_DOCKER_RUN_ID);}
// The custom reporter emits fixed summaries only.
process.stdout.write(result.out);console.log(JSON.stringify({identity,acceptance:result.code===0?'PASS':'FAIL',exactEndUserClients:'NOT_RUN'}));if(result.code)process.exitCode=1;
