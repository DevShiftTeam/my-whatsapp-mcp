import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as policy from '../release-policy.mjs';
import {main} from '../ci.mjs';
import {mkdtempSync,existsSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';

const repository='DevShiftTeam/my-whatsapp-mcp', revision='a'.repeat(40);
const context={repository,ref:'refs/tags/v1.2.3',runID:'123',runAttempt:'1',sourceRevision:revision};
function fixture() {
  const env={id:42,name:'docker-release',protection_rules:[{type:'required_reviewers',prevent_self_review:true,reviewers:[{type:'User',reviewer:{id:7}}]}],deployment_branch_policy:{protected_branches:false,custom_branch_policies:true}};
  const policies={total_count:1,branch_policies:[{type:'tag',name:'v*'}]};
  const run={id:123,run_attempt:1,head_sha:revision,event:'push',path:'.github/workflows/docker.yml',repository:{full_name:repository},actor:{id:8,type:'User'},triggering_actor:{id:9,type:'User'}};
  const reviews=[{state:'approved',user:{id:7,type:'User'},comment:'',environments:[{id:42,name:'docker-release'}]}];
  return {env,policies,run,reviews};
}
async function check(f=fixture(),ctx=context,options={}) {
  const base='https://api.github.com/repos/'+repository;
  const routes=new Map([[base+'/environments/docker-release',f.env],[base+'/environments/docker-release/deployment-branch-policies?per_page=100',f.policies],[base+'/actions/runs/123',f.run],[base+'/actions/runs/123/approvals',f.reviews]]);
  return policy.checkCurrentRunApproval({...ctx,token:'fixture-token',request:async(url,init)=>{
    assert.equal(init.method??'GET','GET');assert.equal(init.redirect,'error');
    assert.equal(init.headers['X-GitHub-Api-Version'],'2026-03-10');
    const status=options.failureURL===url?options.status:(options.failureURL?200:options.status??(routes.has(url)?200:404));
    return new Response(JSON.stringify(routes.get(url)??{}),{status,headers:options.headers});
  }});
}
test('configuration alone never claims current-run approval',()=>{
  const f=fixture();assert.equal(policy.validateEnvironment(f.env,f.policies,'v1.2.3').actualRunApproval,'NOT_VERIFIED');
});
test('accepts one configured independent User decision bound to this first run and environment',async()=>{
  const result=await check();
  assert.equal(result.actualRunApproval,'INDEPENDENT_REQUIRED_REVIEWER_APPROVAL');
  assert.equal(result.reviewerID,7);assert.equal(result.environmentID,42);
  assert.equal(result.runID,'123');assert.equal(result.runAttempt,'1');assert.equal(result.sourceRevision,revision);
});
test('rejects self-review, triggering actor, unconfigured administrator and Team-only reviewers',async()=>{
  for(const id of [8,9,10]){const f=fixture();f.reviews[0].user.id=id;await assert.rejects(check(f),/release_approval_reviewer/);}
  for(const id of [8,9]){const f=fixture();f.env.protection_rules[0].reviewers[0].reviewer.id=id;f.reviews[0].user.id=id;await assert.rejects(check(f),/release_approval_reviewer/);}
  const f=fixture();f.env.protection_rules[0].reviewers[0].type='Team';await assert.rejects(check(f),/release_approval_reviewer/);
});
test('rejects absent, ambiguous, non-approved, malformed and unrelated approval records',async()=>{
  const baseline=fixture().reviews[0];
  for(const reviews of [[],null,[baseline,baseline],[{...baseline,state:'rejected'}],[{...baseline,state:'pending'}],[{...baseline,state:'bypassed'}],[{...baseline,user:null}],[{...baseline,environments:[{id:41,name:'docker-release'}]}],[{...baseline,environments:[{id:42,name:'other'}]}],[{...baseline,environments:[{id:42,name:'docker-release'},{id:43,name:'other'}]}]]){
    await assert.rejects(check({...fixture(),reviews}),/release_approval/);
  }
});
test('rejects mismatched run, source, repository, workflow, event, actors and reruns',async()=>{
  for(const patch of [{id:124},{head_sha:'b'.repeat(40)},{repository:{full_name:'other/repo'}},{path:'.github/workflows/other.yml'},{event:'workflow_dispatch'},{run_attempt:2},{actor:null},{triggering_actor:null}])await assert.rejects(check({...fixture(),run:{...fixture().run,...patch}}),/release_approval_run/);
  for(const patch of [{runAttempt:'2'},{runID:'0123'},{sourceRevision:'bad'},{repository:'other/repo'},{ref:'refs/heads/main'}])await assert.rejects(check(fixture(),{...context,...patch}));
});
test('fails closed on API errors or incomplete paginated approval history',async()=>{
  for(const path of ['/environments/docker-release','/environments/docker-release/deployment-branch-policies?per_page=100','/actions/runs/123','/actions/runs/123/approvals'])
    for(const status of [403,404,500])await assert.rejects(check(fixture(),context,{status,failureURL:'https://api.github.com/repos/'+repository+path}),new RegExp('release_policy_http_'+status));
  await assert.rejects(check(fixture(),context,{headers:{link:'<https://api.github.com/next>; rel="next"'}}),/release_policy_incomplete/);
});
test('approval CLI fails without writing evidence when current-run context is absent',async()=>{
  const cwd=process.cwd(),old=process.env.GITHUB_RUN_ATTEMPT,dir=mkdtempSync(join(tmpdir(),'wa-approval-denied-'));
  try {
    process.chdir(dir);process.env.GITHUB_RUN_ATTEMPT='2';
    await assert.rejects(main(['approval']));
    assert.equal(existsSync(join(dir,'.artifacts/published/approval.json')),false);
  }finally{process.chdir(cwd);if(old===undefined)delete process.env.GITHUB_RUN_ATTEMPT;else process.env.GITHUB_RUN_ATTEMPT=old;rmSync(dir,{recursive:true,force:true});}
});
